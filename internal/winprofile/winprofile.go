//go:build windows

// Package winprofile loads and removes the Windows user profiles belonging to
// server accounts.
//
// A logon token on its own does not give a process a usable HKEY_CURRENT_USER.
// The registry hive behind HKCU lives in the account's profile directory, and
// nothing maps it until someone calls LoadUserProfile — which is normally done
// by winlogon during an interactive logon, and by nobody at all when a service
// calls LogonUser and CreateProcessAsUser.
//
// Without it, HKCU either fails to open or resolves to the default user's hive,
// and anything that reads per-user settings from the registry breaks in ways
// that never mention the registry. Two seen on this port:
//
//   - .NET, and therefore PowerShell, resolves its default web proxy from the
//     WinINET settings under HKCU. When that read fails the failure surfaces as
//     "Error creating the Web Proxy specified in the 'system.net/defaultProxy'
//     configuration section" on the first Invoke-WebRequest, which reads like a
//     misconfigured proxy and is nothing of the sort.
//   - Steam resolves steamclient64.dll through HKCU\Software\Valve\Steam.
//
// Profiles are loaded once per account and left loaded for the lifetime of the
// daemon. Unloading one while a server is still running would pull the hive out
// from under it, and the accounting needed to know when the last process using a
// profile has exited buys nothing: a loaded hive costs a mapped file.
package winprofile

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	moduserenv = windows.NewLazySystemDLL("userenv.dll")

	procLoadUserProfileW = moduserenv.NewProc("LoadUserProfileW")
	procDeleteProfileW   = moduserenv.NewProc("DeleteProfileW")
)

// piNoUI suppresses the profile-error dialog. A service has no desktop to show
// it on, so without this a failure here would hang rather than return.
const piNoUI = 0x00000001

// profileInfo is PROFILEINFOW.
type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	Profile     windows.Handle
}

var (
	mu     sync.Mutex
	loaded = map[string]bool{}
)

// Load maps the account's registry hive into HKEY_USERS so that processes
// running under the given token get a working HKCU.
//
// The profile directory is created on first call, under C:\Users like any other.
// It is deliberately not where a server's files go — see internal/winenv, which
// points USERPROFILE and APPDATA back into the server's own directory so that
// what a server writes stays inside its quota and its backups. This profile
// exists for the hive, not for the storage.
//
// Requires SeRestorePrivilege and SeBackupPrivilege, which the daemon holds as
// an administrator. Idempotent: the result is cached per account.
func Load(token windows.Token, username string) error {
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("winprofile: could not read the account from the logon token: %w", err)
	}
	key := user.User.Sid.String()

	mu.Lock()
	defer mu.Unlock()
	if loaded[key] {
		return nil
	}

	name, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return fmt.Errorf("winprofile: username: %w", err)
	}

	pi := profileInfo{
		Flags:    piNoUI,
		UserName: name,
	}
	pi.Size = uint32(unsafe.Sizeof(pi))

	r, _, e := procLoadUserProfileW.Call(uintptr(token), uintptr(unsafe.Pointer(&pi)))
	if r == 0 {
		return fmt.Errorf("winprofile: could not load the user profile for %q: %w", username, e)
	}

	// pi.Profile is the HKCU key handle. It is intentionally not closed: closing
	// it is what unloads the hive, and the hive has to stay mapped for as long as
	// processes are running under this account.
	loaded[key] = true
	return nil
}

// Delete removes an account's profile directory and hive.
//
// Called when the daemon removes the account itself. NetUserDel leaves the
// profile behind, so without this a host that has created and destroyed a few
// hundred servers accumulates a few hundred abandoned directories under C:\Users.
//
// A profile that was never created is not an error.
func Delete(sid *windows.SID) error {
	s, err := windows.UTF16PtrFromString(sid.String())
	if err != nil {
		return fmt.Errorf("winprofile: sid: %w", err)
	}

	mu.Lock()
	delete(loaded, sid.String())
	mu.Unlock()

	r, _, e := procDeleteProfileW.Call(uintptr(unsafe.Pointer(s)), 0, 0)
	if r == 0 {
		// ERROR_FILE_NOT_FOUND / ERROR_PATH_NOT_FOUND mean there was nothing to
		// remove, which is the expected outcome for an account whose servers
		// never started.
		if errno, ok := e.(windows.Errno); ok &&
			(errno == windows.ERROR_FILE_NOT_FOUND || errno == windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("winprofile: could not delete the profile for %s: %w", sid, e)
	}
	return nil
}
