//go:build windows

package winproc

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/internal/winprofile"
)

// golang.org/x/sys/windows does not bind LogonUserW, so it is bound here.
var (
	modadvapi32    = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW = modadvapi32.NewProc("LogonUserW")
)

// Logon types. Only the two that make sense for a service are exposed.
const (
	// logon32LogonBatch is the right type for a non-interactive workload started
	// by a service. The target account needs the "Log on as a batch job" right,
	// which is granted at install time.
	logon32LogonBatch = 4

	logon32ProviderDefault = 0
)

// LogonUser obtains a primary token for a local account.
//
// Two privileges are required by the account this process runs as before the
// resulting token can be used with CreateProcessAsUser:
// SeAssignPrimaryTokenPrivilege and SeIncreaseQuotaPrivilege. Neither can be
// granted at runtime — they are install-time configuration, applied to the
// daemon's service account.
//
// The domain is fixed to "." so only local accounts can be used. Running game
// servers as domain accounts would put domain credentials on a host whose whole
// purpose is executing untrusted third-party binaries.
func LogonUser(username, password string) (windows.Token, error) {
	u, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return 0, fmt.Errorf("winproc: username: %w", err)
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return 0, fmt.Errorf("winproc: password: %w", err)
	}
	d, err := windows.UTF16PtrFromString(".")
	if err != nil {
		return 0, fmt.Errorf("winproc: domain: %w", err)
	}

	var token windows.Token
	r, _, e := procLogonUserW.Call(
		uintptr(unsafe.Pointer(u)),
		uintptr(unsafe.Pointer(d)),
		uintptr(unsafe.Pointer(p)),
		uintptr(logon32LogonBatch),
		uintptr(logon32ProviderDefault),
		uintptr(unsafe.Pointer(&token)),
	)
	if r == 0 {
		return 0, fmt.Errorf("winproc: logon %q: %w", username, e)
	}

	// A token is not a logon session as far as the registry is concerned. Nothing
	// maps the account's hive to HKEY_CURRENT_USER until the profile is loaded,
	// and a service calling LogonUser is not something Windows does that for.
	//
	// Best effort, because a process that never touches HKCU runs perfectly well
	// without one and refusing the logon would be a worse outcome than the
	// failures this prevents. The warning names it if something later goes wrong.
	if err := winprofile.Load(token, username); err != nil {
		Warn(fmt.Sprintf("%v; processes for this account will have no HKEY_CURRENT_USER, "+
			"which breaks anything reading per-user registry settings -- PowerShell's web "+
			"proxy resolution and Steam's client library lookup among them", err))
	}

	return token, nil
}
