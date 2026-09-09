//go:build windows

// Package winpriv inspects the privileges the daemon is running with.
//
// This exists because of a tension that is easy to get wrong. The daemon
// executes untrusted third-party install scripts and supervises untrusted game
// servers, so it should hold as little authority as possible. But launching a
// process as a *different* local account — which is what separates one server
// from another — requires two privileges an ordinary user does not have.
//
// Running as LocalSystem satisfies that requirement and is the wrong answer: a
// compromised daemon would own the host. The right answer is a dedicated
// unprivileged account granted exactly those two privileges and nothing else.
//
// These checks let the daemon say which of those situations it is in, at boot,
// rather than failing at the first server start.
package winpriv

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Privileges required to launch a process as another local account via
// CreateProcessAsUser.
const (
	// SeAssignPrimaryToken allows assigning a primary token to a new process.
	SeAssignPrimaryToken = "SeAssignPrimaryTokenPrivilege"
	// SeIncreaseQuota allows adjusting the memory quota of that process.
	SeIncreaseQuota = "SeIncreaseQuotaPrivilege"
)

// State describes the security context the daemon is running in.
type State struct {
	// Account is the fully qualified name of the account, e.g. NT AUTHORITY\SYSTEM.
	Account string

	// IsSystem reports whether this is LocalSystem.
	IsSystem bool

	// IsAdmin reports membership of the local Administrators group.
	IsAdmin bool

	// IsElevated reports whether the process holds an elevated token.
	IsElevated bool

	// CanLaunchAsUser reports whether both privileges needed to start a process
	// as another account are present.
	CanLaunchAsUser bool

	// Missing lists whichever of those privileges are absent.
	Missing []string
}

// Describe renders the state for a log line.
func (s State) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "account=%s", s.Account)
	if s.IsSystem {
		b.WriteString(" system=true")
	}
	if s.IsAdmin {
		b.WriteString(" admin=true")
	}
	if s.IsElevated {
		b.WriteString(" elevated=true")
	}
	fmt.Fprintf(&b, " can_launch_as_user=%t", s.CanLaunchAsUser)
	if len(s.Missing) > 0 {
		fmt.Fprintf(&b, " missing=%s", strings.Join(s.Missing, ","))
	}
	return b.String()
}

// Current inspects the process token.
func Current() (State, error) {
	var s State
	token := windows.GetCurrentProcessToken()

	user, err := token.GetTokenUser()
	if err != nil {
		return s, fmt.Errorf("winpriv: could not read the process account: %w", err)
	}
	account, domain, _, err := user.User.Sid.LookupAccount("")
	if err != nil {
		s.Account = user.User.Sid.String()
	} else if domain != "" {
		s.Account = domain + `\` + account
	} else {
		s.Account = account
	}

	if sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid); err == nil {
		s.IsSystem = windows.EqualSid(user.User.Sid, sid)
	}
	if sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err == nil {
		// IsMember accounts for the group being present but deny-only on a
		// non-elevated token, which is what we want: a filtered token is not
		// meaningfully an administrator.
		if member, err := token.IsMember(sid); err == nil {
			s.IsAdmin = member
		}
	}
	s.IsElevated = token.IsElevated()

	held, err := heldPrivileges(token)
	if err != nil {
		return s, err
	}
	for _, name := range []string{SeAssignPrimaryToken, SeIncreaseQuota} {
		if !held[name] {
			s.Missing = append(s.Missing, name)
		}
	}
	s.CanLaunchAsUser = len(s.Missing) == 0

	return s, nil
}

// heldPrivileges returns the set of privilege names present on the token.
//
// Presence rather than enabled state is what matters: a privilege the token
// holds but has not enabled can be enabled on demand, whereas one it does not
// hold at all cannot be acquired.
func heldPrivileges(token windows.Token) (map[string]bool, error) {
	var size uint32
	err := windows.GetTokenInformation(token, windows.TokenPrivileges, nil, 0, &size)
	if err != nil && err != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, fmt.Errorf("winpriv: could not size the privilege list: %w", err)
	}

	buf := make([]byte, size)
	if err := windows.GetTokenInformation(
		token, windows.TokenPrivileges, &buf[0], size, &size,
	); err != nil {
		return nil, fmt.Errorf("winpriv: could not read the privilege list: %w", err)
	}

	tp := (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0]))
	// Privileges is declared as a one-element array standing in for a
	// variable-length one, so it has to be re-sliced to its real length.
	all := unsafe.Slice(&tp.Privileges[0], tp.PrivilegeCount)

	held := make(map[string]bool, len(all))
	for i := range all {
		name, err := privilegeName(all[i].Luid)
		if err != nil {
			continue
		}
		held[name] = true
	}
	return held, nil
}

func privilegeName(luid windows.LUID) (string, error) {
	var size uint32
	err := lookupPrivilegeName(nil, &luid, nil, &size)
	if err != nil && err != windows.ERROR_INSUFFICIENT_BUFFER {
		return "", err
	}

	buf := make([]uint16, size)
	if err := lookupPrivilegeName(nil, &luid, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}

var (
	modadvapi32              = windows.NewLazySystemDLL("advapi32.dll")
	procLookupPrivilegeNameW = modadvapi32.NewProc("LookupPrivilegeNameW")
)

// lookupPrivilegeName wraps LookupPrivilegeNameW, which x/sys/windows binds only
// in the value direction.
func lookupPrivilegeName(system *uint16, luid *windows.LUID, name *uint16, size *uint32) error {
	r, _, e := procLookupPrivilegeNameW.Call(
		uintptr(unsafe.Pointer(system)),
		uintptr(unsafe.Pointer(luid)),
		uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(size)),
	)
	if r == 0 {
		return e
	}
	return nil
}

// currentToken is a seam for tests.
func currentToken() windows.Token { return windows.GetCurrentProcessToken() }
