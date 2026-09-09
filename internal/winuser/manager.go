//go:build windows

package winuser

import (
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// maxAccountName is the SAM account name limit. Exceeding it is not truncated
// silently — NetUserAdd rejects the call — so names are built to fit.
const maxAccountName = 20

// DefaultPrefix is prepended to every per-server account name.
//
// Short on purpose. The account name has to carry enough of the server's UUID to
// be unique on the host and still fit in 20 characters, which leaves very little
// room for anything decorative.
const DefaultPrefix = "ww-"

// Manager owns the local accounts that servers run under.
//
// It holds each account's password in memory only. On a daemon restart the
// passwords are simply regenerated, which is why nothing here reads or writes a
// credential store.
type Manager struct {
	prefix string

	mu    sync.Mutex
	creds map[string]string // server UUID -> password
}

// New returns a Manager naming accounts with the given prefix.
func New(prefix string) *Manager {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return &Manager{prefix: prefix, creds: make(map[string]string)}
}

// NameFor derives a server's account name from its UUID.
//
// Windows caps a local account name at 20 characters, so the UUID is stripped of
// its dashes and truncated to whatever fits. 16 hex characters remain after the
// default prefix, which is 64 bits of the UUID — enough that a collision between
// two servers on one node is not a thing that happens.
//
// A collision would not be silently damaging in any case: the account carries a
// marker naming the server it belongs to, and the daemon refuses to reuse an
// account whose marker names a different one.
func (m *Manager) NameFor(uuid string) string {
	compact := strings.ReplaceAll(strings.ToLower(uuid), "-", "")
	room := maxAccountName - len(m.prefix)
	if room < 0 {
		room = 0
	}
	if len(compact) > room {
		compact = compact[:room]
	}
	return m.prefix + compact
}

// Ensure returns the account a server runs under, creating it if necessary.
//
// Safe to call repeatedly: after the first call in a daemon's lifetime it
// returns the cached credentials without touching the account database.
func (m *Manager) Ensure(uuid string) (string, string, error) {
	name := m.NameFor(uuid)

	m.mu.Lock()
	if pw, ok := m.creds[uuid]; ok {
		m.mu.Unlock()
		return name, pw, nil
	}
	m.mu.Unlock()

	password, err := GeneratePassword()
	if err != nil {
		return "", "", err
	}

	existing, err := Lookup(name)
	if err != nil {
		return "", "", err
	}
	switch {
	case existing == nil:
		if err := Create(name, password, markerPrefix+uuid); err != nil {
			return "", "", err
		}

	case !existing.ManagedFor(uuid):
		// Either an account of the operator's that happens to collide, or one
		// left behind by a server with a different UUID. Neither is ours to
		// reset the password on.
		return "", "", fmt.Errorf(
			"winuser: the account %q already exists but was not created by this daemon for "+
				"server %s (its description reads %q). Rename or remove that account, or set "+
				"system.account.prefix to something that does not collide",
			name, uuid, existing.Comment)

	default:
		// Ours, from a previous run of the daemon. The password it was given
		// then is gone, so issue a new one.
		if err := SetPassword(name, password); err != nil {
			return "", "", err
		}
	}

	sid, err := SID(name)
	if err != nil {
		return "", "", err
	}

	// A server account that is an administrator could take ownership of any
	// other server's files, which would make the whole layout decorative. This
	// is not something the daemon does, so it only happens if someone did it by
	// hand — but it is cheap to check and expensive to miss.
	admin, err := IsAdministrator(name)
	if err != nil {
		return "", "", err
	}
	if admin {
		return "", "", fmt.Errorf(
			"winuser: the account %q is a member of the local Administrators group, so it "+
				"cannot be used to isolate a server. Remove it from that group", name)
	}

	rights := append(append([]string{}, serverAccountRights...), serverAccountDenials...)
	if err := GrantRights(sid, rights...); err != nil {
		return "", "", err
	}

	m.mu.Lock()
	m.creds[uuid] = password
	m.mu.Unlock()

	return name, password, nil
}

// Invalidate forgets a cached password so the next Ensure reissues one.
//
// Used when a logon fails with a credential error, which means the account's
// password was changed behind the daemon's back.
func (m *Manager) Invalidate(uuid string) {
	m.mu.Lock()
	delete(m.creds, uuid)
	m.mu.Unlock()
}

// Remove deletes a server's account.
//
// Called when a server is deleted. The account's rights are stripped first: LSA
// keys its policy database by SID and does not clean up after a deleted account,
// so leaving them would accumulate orphaned entries for the lifetime of the
// host.
func (m *Manager) Remove(uuid string) error {
	name := m.NameFor(uuid)

	existing, err := Lookup(name)
	if err != nil {
		return err
	}
	if existing == nil {
		m.Invalidate(uuid)
		return nil
	}
	if !existing.ManagedFor(uuid) {
		// Refusing to delete is the right failure here. Deleting an account the
		// daemon does not own, because a UUID happened to hash into its name,
		// would be considerably worse than leaving one behind.
		return fmt.Errorf(
			"winuser: refusing to delete the account %q, which was not created by this daemon "+
				"for server %s", name, uuid)
	}

	if sid, err := SID(name); err == nil {
		// Failure here is not fatal: the account still goes, and a stale policy
		// entry keyed to a SID that no longer resolves grants nothing.
		_ = RemoveAllRights(sid)
	}

	if err := Delete(name); err != nil {
		return err
	}
	m.Invalidate(uuid)
	return nil
}

// EnsureServiceAccount creates or updates the account the daemon itself runs as,
// and returns its password.
//
// This is the one account that needs real authority: it creates the per-server
// accounts and rewrites NTFS permissions, so it is an administrator. It is still
// worth being a distinct account rather than LocalSystem — it can be audited,
// denied interactive logon, and revoked — but the honest description is that a
// compromise of the daemon is a compromise of the host either way. What the
// separation buys is that a compromise of a *server* is not.
//
// Called from `wings service install`, at a point where the operator is already
// at an elevated prompt.
func EnsureServiceAccount(name, comment string) (string, error) {
	password, err := GeneratePassword()
	if err != nil {
		return "", err
	}

	existing, err := Lookup(name)
	if err != nil {
		return "", err
	}
	if existing == nil {
		if err := Create(name, password, comment); err != nil {
			return "", err
		}
	} else if err := SetPassword(name, password); err != nil {
		return "", err
	}

	sid, err := SID(name)
	if err != nil {
		return "", err
	}
	if err := AddToAdministrators(sid); err != nil {
		return "", err
	}

	// SeServiceLogonRight so the service control manager can start it at all.
	//
	// The other two are what CreateProcessAsUser needs. Administrators hold
	// SeIncreaseQuotaPrivilege by default but not SeAssignPrimaryTokenPrivilege,
	// so granting both explicitly is what removes the trip to secpol.msc that
	// this daemon used to require.
	//
	// Interactive and remote-interactive logon are denied: the service account
	// has no reason to sign in anywhere, and a password nobody knows is not by
	// itself a control.
	if err := GrantRights(sid,
		RightServiceLogon,
		RightAssignPrimaryToken,
		RightIncreaseQuota,
		DenyInteractiveLogon,
		DenyRemoteInteractiveLogon,
	); err != nil {
		return "", err
	}

	return password, nil
}

// GrantSelfRights grants the account this process is running as the privileges
// needed to launch processes under other accounts.
//
// LSA account rights are evaluated at logon, so this does not affect the running
// process — the caller has to tell the operator to restart the service. It
// exists so that recovery is a service restart rather than a documented manual
// procedure.
func GrantSelfRights() error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("winuser: could not read this process's account: %w", err)
	}
	return GrantRights(user.User.Sid, RightAssignPrimaryToken, RightIncreaseQuota)
}
