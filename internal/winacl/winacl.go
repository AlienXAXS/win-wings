//go:build windows

// Package winacl applies the NTFS permissions that separate servers from one
// another.
//
// Under Docker this was free: a container could only see its own bind mount.
// Here every server's files sit on the same volume as every other server's, and
// an ACL is the only thing keeping them apart. The layout in config/paths.go
// depends on this being applied — without it the nesting is documentation
// rather than enforcement.
package winacl

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// GrantExclusiveWrite gives one account full control of a directory tree and
// removes everyone else except SYSTEM and Administrators.
//
// Inheritance is disabled on the target so the parent's more permissive rules do
// not leak in. Applied to a server's data directory, the result is that the
// server's own account can write its files and no other server's account can
// read them.
//
// Passing an empty account is not an error: with shared isolation there is no
// per-server account to grant, and the caller has already been warned that
// servers are not isolated.
func GrantExclusiveWrite(path, account string) error {
	if account == "" {
		return nil
	}

	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return fmt.Errorf("winacl: could not resolve account %q: %w", account, err)
	}

	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("winacl: could not resolve SYSTEM: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("winacl: could not resolve Administrators: %w", err)
	}

	// Inheritable so that everything the server creates beneath this directory
	// carries the same restriction.
	const inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT

	entries := []windows.EXPLICIT_ACCESS{
		explicitAccess(sid, windows.GENERIC_ALL, inherit),
		explicitAccess(system, windows.GENERIC_ALL, inherit),
		explicitAccess(admins, windows.GENERIC_ALL, inherit),
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("winacl: could not build an ACL for %q: %w", path, err)
	}

	// PROTECTED_DACL_SECURITY_INFORMATION severs inheritance from the parent,
	// which is the point: the servers root denies write to server accounts, and
	// this directory must not inherit that denial while also not inheriting any
	// broader grant.
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("winacl: could not apply permissions to %q: %w", path, err)
	}

	return nil
}

// DenyAll restricts a path to SYSTEM and Administrators only.
//
// Used on the files that authorise what a worker executes. A server that could
// write its own worker.json would be able to rewrite its startup command, which
// is arbitrary code execution as whatever account it runs under.
func DenyAll(path string) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("winacl: could not resolve SYSTEM: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("winacl: could not resolve Administrators: %w", err)
	}

	// Also grant the account the daemon runs as, which is not necessarily
	// SYSTEM — a dedicated service account still has to read these files.
	self, err := currentUserSID()
	if err != nil {
		return err
	}

	const inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	entries := []windows.EXPLICIT_ACCESS{
		explicitAccess(system, windows.GENERIC_ALL, inherit),
		explicitAccess(admins, windows.GENERIC_ALL, inherit),
		explicitAccess(self, windows.GENERIC_ALL, inherit),
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("winacl: could not build an ACL for %q: %w", path, err)
	}

	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("winacl: could not apply permissions to %q: %w", path, err)
	}
	return nil
}

func currentUserSID() (*windows.SID, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("winacl: could not read this process's account: %w", err)
	}
	return u.User.Sid, nil
}

func explicitAccess(sid *windows.SID, permissions windows.ACCESS_MASK, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
