//go:build windows

// Package winsta grants a server's account access to the window station and
// desktop its process will be launched on.
//
// Every Windows process belongs to a window station and a desktop, whether or
// not it draws anything. user32.dll connects to them during process startup, and
// if the process's token has no access to either, that connection fails and the
// process dies in the loader with STATUS_DLL_INIT_FAILED (0xC0000142) before a
// single line of its own code runs.
//
// This is unavoidable for the daemon. A service runs on the window station
// Service-0x0-3e7$, whose DACL grants access to the service account and nobody
// else. CreateProcessAsUser with no lpDesktop makes the child inherit that
// station, and the child's token is a *different* account by design — that being
// the entire point of the isolation. So the child is refused, and the failure
// arrives as an exit code with no message attached to it.
//
// The fix is to add the server's account to the DACL of the station and desktop,
// and to name them explicitly in STARTUPINFO. Which is what this does.
//
// It grants less than the classic MSDN sample does. The access rights that allow
// reading the screen, reading the clipboard, installing hooks, recording input
// and switching desktops are deliberately withheld: a game server has no use for
// any of them, and on a host where the daemon runs in the foreground rather than
// as a service, the station in question is the operator's own.
package winsta

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Window station access rights.
const (
	winstaEnumDesktops      = 0x0001
	winstaReadAttributes    = 0x0002
	winstaAccessClipboard   = 0x0004
	winstaCreateDesktop     = 0x0008
	winstaWriteAttributes   = 0x0010
	winstaAccessGlobalAtoms = 0x0020
	winstaExitWindows       = 0x0040
	winstaEnumerate         = 0x0100
	winstaReadScreen        = 0x0200
)

// Desktop access rights.
const (
	desktopReadObjects     = 0x0001
	desktopCreateWindow    = 0x0002
	desktopCreateMenu      = 0x0004
	desktopHookControl     = 0x0008
	desktopJournalRecord   = 0x0010
	desktopJournalPlayback = 0x0020
	desktopEnumerate       = 0x0040
	desktopWriteObjects    = 0x0080
	desktopSwitchDesktop   = 0x0100
)

// stationRights is what a server's account is given on the window station.
//
// AccessGlobalAtoms and ExitWindows are the two that user32 actually requires
// during initialisation; the rest are what a process needs to behave normally
// once running.
//
// Withheld: AccessClipboard and ReadScreen. Neither is needed by a headless game
// server, and both would let one read what an operator is doing if the daemon is
// running in the foreground on their own station rather than as a service.
const stationRights = winstaEnumDesktops | winstaReadAttributes | winstaCreateDesktop |
	winstaWriteAttributes | winstaAccessGlobalAtoms | winstaExitWindows | winstaEnumerate |
	windows.READ_CONTROL

// desktopRights is what a server's account is given on the desktop.
//
// Withheld: HookControl, JournalRecord, JournalPlayback and SwitchDesktop. Those
// are the rights that let a process observe or inject input across every other
// window on the desktop, and handing them to something running third-party game
// code would undo much of what the per-server account is for.
const desktopRights = desktopReadObjects | desktopCreateWindow | desktopCreateMenu |
	desktopEnumerate | desktopWriteObjects | windows.READ_CONTROL

// ACE inheritance flags, which x/sys does not name for this purpose.
const (
	objectInheritAce      = 0x1
	containerInheritAce   = 0x2
	noPropagateInheritAce = 0x4
	inheritOnlyAce        = 0x8
)

// uoiName selects the object's name in GetUserObjectInformation.
const uoiName = 2

var (
	moduser32 = windows.NewLazySystemDLL("user32.dll")

	procGetProcessWindowStation  = moduser32.NewProc("GetProcessWindowStation")
	procGetThreadDesktop         = moduser32.NewProc("GetThreadDesktop")
	procGetUserObjectInformation = moduser32.NewProc("GetUserObjectInformationW")
)

var (
	// granted remembers which accounts have already been given access, so that
	// starting fifty servers does not rewrite the same two DACLs a hundred
	// times. The grant is idempotent, but each one is a read-modify-write of a
	// shared object and doing it concurrently is a good way to lose an entry.
	mu      sync.Mutex
	granted = map[string]string{}
)

// GrantToToken gives the account behind a logon token access to this process's
// window station and desktop, and returns the value for STARTUPINFO.lpDesktop.
//
// Safe to call for every process launch: the result is cached per account.
func GrantToToken(token windows.Token) (string, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("winsta: could not read the account from the logon token: %w", err)
	}
	return Grant(user.User.Sid)
}

// Grant gives an account access to this process's window station and desktop.
func Grant(sid *windows.SID) (string, error) {
	key := sid.String()

	mu.Lock()
	defer mu.Unlock()
	if name, ok := granted[key]; ok {
		return name, nil
	}

	station, err := processWindowStation()
	if err != nil {
		return "", err
	}
	desktop, err := threadDesktop()
	if err != nil {
		return "", err
	}

	stationName, err := objectName(station)
	if err != nil {
		return "", fmt.Errorf("winsta: could not read the window station's name: %w", err)
	}
	desktopName, err := objectName(desktop)
	if err != nil {
		return "", fmt.Errorf("winsta: could not read the desktop's name: %w", err)
	}

	// Two entries on the window station. The inherit-only one is what any
	// desktop created under it later picks up; the no-propagate one applies to
	// the station itself and must not be inherited, or desktops would receive
	// window station rights that mean nothing to them.
	if err := addToDACL(station, "window station "+stationName, []windows.EXPLICIT_ACCESS{
		access(sid, desktopRights, objectInheritAce|containerInheritAce|inheritOnlyAce),
		access(sid, stationRights, noPropagateInheritAce),
	}); err != nil {
		return "", err
	}

	if err := addToDACL(desktop, "desktop "+desktopName, []windows.EXPLICIT_ACCESS{
		access(sid, desktopRights, noPropagateInheritAce),
	}); err != nil {
		return "", err
	}

	full := stationName + `\` + desktopName
	granted[key] = full
	return full, nil
}

// addToDACL merges entries into an object's existing DACL.
//
// Merging rather than replacing matters more here than anywhere else in this
// daemon: the window station's DACL is what lets the daemon itself, the service
// control manager and every other server reach it. Replacing it would lock out
// everything already running.
func addToDACL(handle windows.Handle, what string, entries []windows.EXPLICIT_ACCESS) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_WINDOW_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("winsta: could not read the security descriptor of %s: %w", what, err)
	}

	existing, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("winsta: could not read the DACL of %s: %w", what, err)
	}

	acl, err := windows.ACLFromEntries(entries, existing)
	if err != nil {
		return fmt.Errorf("winsta: could not build a DACL for %s: %w", what, err)
	}

	if err := windows.SetSecurityInfo(handle, windows.SE_WINDOW_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("winsta: could not grant access to %s: %w", what, err)
	}
	return nil
}

func access(sid *windows.SID, mask uint32, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(mask),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

// processWindowStation returns the station this process is attached to.
//
// Not a handle to close: it belongs to the process, and closing it would detach
// the daemon itself from its own window station.
func processWindowStation() (windows.Handle, error) {
	r, _, e := procGetProcessWindowStation.Call()
	if r == 0 {
		return 0, fmt.Errorf("winsta: could not open this process's window station: %w", e)
	}
	return windows.Handle(r), nil
}

// threadDesktop returns the desktop this thread is attached to.
//
// Also not a handle to close, for the same reason.
func threadDesktop() (windows.Handle, error) {
	r, _, e := procGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	if r == 0 {
		return 0, fmt.Errorf("winsta: could not open this thread's desktop: %w", e)
	}
	return windows.Handle(r), nil
}

// objectName reads a window station's or desktop's name.
func objectName(handle windows.Handle) (string, error) {
	var needed uint32
	// Sized first: the name is short, but guessing a buffer size and having it
	// silently truncated would produce a desktop path that does not resolve.
	r, _, e := procGetUserObjectInformation.Call(
		uintptr(handle), uoiName, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if r == 0 && needed == 0 {
		return "", e
	}

	buf := make([]uint16, needed/2+1)
	r, _, e = procGetUserObjectInformation.Call(
		uintptr(handle), uoiName,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2),
		uintptr(unsafe.Pointer(&needed)),
	)
	if r == 0 {
		return "", e
	}
	return windows.UTF16ToString(buf), nil
}

// Current returns this process's "station\desktop" without changing anything.
//
// Reported at boot and by the self test, because it is the single most useful
// fact when a process dies with 0xC0000142: a daemon on Service-0x0-3e7$ is
// running as a service and its children need an explicit grant, while one on
// WinSta0 is running in a console and mostly does not.
func Current() (string, error) {
	station, err := processWindowStation()
	if err != nil {
		return "", err
	}
	desktop, err := threadDesktop()
	if err != nil {
		return "", err
	}
	s, err := objectName(station)
	if err != nil {
		return "", err
	}
	d, err := objectName(desktop)
	if err != nil {
		return "", err
	}
	return s + `\` + d, nil
}
