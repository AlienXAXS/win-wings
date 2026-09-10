//go:build windows

package netstat

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Two rights are needed to collect, and they live in different places.
//
// Starting the session is governed by group membership: administrators and
// Performance Log Users may. Enabling the kernel-network provider into that
// session is governed by a security descriptor on the provider itself, which
// by default admits only administrators. Performance Log Users membership on
// its own therefore gets as far as StartTrace and fails at EnableTraceEx2 with
// access denied, which is exactly what happened the first time this was run.
//
// The provider's descriptor can be amended with EventAccessControl. That is
// what lets a daemon running without administrator rights collect, and it is
// done once, elevated, at install time.

var procEventAccessControl = modadvapi32.NewProc("EventAccessControl")

const (
	// eventSecurityAddDACL appends an ACE to the provider's access list.
	//
	// EVENTSECURITYOPERATION numbers SetDACL 0, SetSACL 1, AddDACL 2 and
	// AddSACL 3. Passing 1 here by mistake asks for the audit list to be
	// replaced, which opens the provider object for ACCESS_SYSTEM_SECURITY and
	// fails with ERROR_PRIVILEGE_NOT_HELD even from an elevated prompt.
	eventSecurityAddDACL = 2
	// tracelogGUIDEnable is the right to enable the provider into a session.
	tracelogGUIDEnable = 0x0080
)

// GrantEnable allows an account to enable the kernel-network provider. Must be
// called elevated; the change persists in the registry across reboots.
//
// No privilege handling is needed here: EventAccessControl enables
// SeSecurityPrivilege on its own token for the duration of the call, and a
// DACL change only needs WRITE_DAC on the provider, which the default
// descriptor grants to administrators.
func GrantEnable(sid *windows.SID) error {
	guid := kernelNetworkProvider
	r, _, _ := procEventAccessControl.Call(
		uintptr(unsafe.Pointer(&guid)),
		eventSecurityAddDACL,
		uintptr(unsafe.Pointer(sid)),
		tracelogGUIDEnable,
		1, // allow
	)
	if r != 0 {
		return fmt.Errorf("netstat: grant provider enable right: %w", windows.Errno(r))
	}
	return nil
}
