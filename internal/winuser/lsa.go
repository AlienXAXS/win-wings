//go:build windows

package winuser

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Account rights, which are the strings behind the entries in secpol.msc under
// Local Policies -> User Rights Assignment.
//
// These are what the daemon grants so that an operator never has to open that
// tool. Granting them is an LSA policy change and takes effect at the account's
// next logon, not immediately for sessions already established.
const (
	// RightBatchLogon is required for LogonUser(LOGON32_LOGON_BATCH), which is
	// how a server process is started under its own account.
	RightBatchLogon = "SeBatchLogonRight"

	// RightServiceLogon is required for an account to run a Windows service.
	RightServiceLogon = "SeServiceLogonRight"

	// RightAssignPrimaryToken and RightIncreaseQuota are what CreateProcessAsUser
	// needs. Administrators hold the latter by default but not the former, so it
	// is granted explicitly even for an administrative service account.
	RightAssignPrimaryToken = "SeAssignPrimaryTokenPrivilege"
	RightIncreaseQuota      = "SeIncreaseQuotaPrivilege"

	// The deny rights below exist to make a server account useful for nothing
	// except running its own server. A deny right always beats a grant, so these
	// hold even if the account later lands in a group that is allowed to log on.
	DenyInteractiveLogon       = "SeDenyInteractiveLogonRight"
	DenyRemoteInteractiveLogon = "SeDenyRemoteInteractiveLogonRight"
	DenyNetworkLogon           = "SeDenyNetworkLogonRight"
	DenyServiceLogon           = "SeDenyServiceLogonRight"
)

// serverAccountRights is granted to every managed per-server account.
var serverAccountRights = []string{RightBatchLogon}

// serverAccountDenials is the rest of the logon surface, closed off.
//
// Network logon is denied so that a server account cannot be used to reach this
// host over SMB or WinRM; interactive and remote-interactive so it cannot be
// used to sign in at the console or over RDP; service so it cannot be attached
// to a service that would then start at boot outside the daemon's control.
var serverAccountDenials = []string{
	DenyInteractiveLogon,
	DenyRemoteInteractiveLogon,
	DenyNetworkLogon,
	DenyServiceLogon,
}

type lsaUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               *lsaUnicodeString
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

// Policy access rights needed to add rights to an account.
const (
	policyCreateAccount = 0x0010
	policyLookupNames   = 0x0800
)

var (
	procLsaOpenPolicy          = modadvapi32.NewProc("LsaOpenPolicy")
	procLsaClose               = modadvapi32.NewProc("LsaClose")
	procLsaAddAccountRights    = modadvapi32.NewProc("LsaAddAccountRights")
	procLsaRemoveAccountRights = modadvapi32.NewProc("LsaRemoveAccountRights")
	procLsaNtStatusToWinError  = modadvapi32.NewProc("LsaNtStatusToWinError")
)

// ntstatus converts an LSA NTSTATUS into a Go error, or nil on success.
func ntstatus(status uintptr, op string) error {
	if status == 0 {
		return nil
	}
	code, _, _ := procLsaNtStatusToWinError.Call(status)
	return fmt.Errorf("winuser: %s: %w", op, windows.Errno(code))
}

// newUnicodeString builds an LSA_UNICODE_STRING over s.
//
// Length excludes the terminating null and is counted in bytes, not characters;
// MaximumLength includes it. Getting this wrong is silently accepted by some LSA
// calls and rejected by others, so it is done in exactly one place.
//
// The returned value points into buf, which the caller must keep alive.
func newUnicodeString(s string) (lsaUnicodeString, []uint16, error) {
	buf, err := windows.UTF16FromString(s)
	if err != nil {
		return lsaUnicodeString{}, nil, err
	}
	n := (len(buf) - 1) * 2
	return lsaUnicodeString{
		Length:        uint16(n),
		MaximumLength: uint16(n + 2),
		Buffer:        &buf[0],
	}, buf, nil
}

// openPolicy connects to the local security authority.
func openPolicy(access uint32) (windows.Handle, error) {
	var attrs lsaObjectAttributes
	attrs.Length = uint32(unsafe.Sizeof(attrs))

	var handle windows.Handle
	status, _, _ := procLsaOpenPolicy.Call(
		0, // the local system
		uintptr(unsafe.Pointer(&attrs)),
		uintptr(access),
		uintptr(unsafe.Pointer(&handle)),
	)
	if err := ntstatus(status, "could not open the local security policy (this needs administrator rights)"); err != nil {
		return 0, err
	}
	return handle, nil
}

// GrantRights adds the named account rights to an account.
//
// Rights already held are not an error; LSA treats the call as a set union.
func GrantRights(sid *windows.SID, rights ...string) error {
	if len(rights) == 0 {
		return nil
	}

	policy, err := openPolicy(policyCreateAccount | policyLookupNames)
	if err != nil {
		return err
	}
	defer procLsaClose.Call(uintptr(policy))

	list := make([]lsaUnicodeString, 0, len(rights))
	keep := make([][]uint16, 0, len(rights))
	for _, r := range rights {
		u, buf, err := newUnicodeString(r)
		if err != nil {
			return err
		}
		list = append(list, u)
		keep = append(keep, buf)
	}

	status, _, _ := procLsaAddAccountRights.Call(
		uintptr(policy),
		uintptr(unsafe.Pointer(sid)),
		uintptr(unsafe.Pointer(&list[0])),
		uintptr(len(list)),
	)
	// keep is referenced here purely so the buffers the LSA_UNICODE_STRINGs
	// point at cannot be collected before the call returns.
	runtime.KeepAlive(keep)

	return ntstatus(status, fmt.Sprintf("could not grant %v", rights))
}

// RemoveAllRights strips every account right from an account.
//
// Called before deleting an account so that a SID left in the policy database —
// LSA keeps entries keyed by SID, and SIDs are not reused — cannot come back
// attached to something else.
func RemoveAllRights(sid *windows.SID) error {
	policy, err := openPolicy(policyCreateAccount | policyLookupNames)
	if err != nil {
		return err
	}
	defer procLsaClose.Call(uintptr(policy))

	status, _, _ := procLsaRemoveAccountRights.Call(
		uintptr(policy),
		uintptr(unsafe.Pointer(sid)),
		1, // AllRights
		0, 0,
	)
	// The account may hold no rights at all, which is reported as an object
	// not being found rather than as success.
	if err := ntstatus(status, "could not remove account rights"); err != nil {
		var errno windows.Errno
		if errors.As(err, &errno) && errno == windows.ERROR_FILE_NOT_FOUND {
			return nil
		}
		return err
	}
	return nil
}
