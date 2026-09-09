//go:build windows

// Package accounts resolves which Windows account a server runs under.
//
// It exists to keep one decision in one place. Two of the isolation modes answer
// the question from configuration and one creates accounts on demand, and the
// callers — starting a server, running an install script, applying NTFS
// permissions, deleting a server — should not each have to know which mode is in
// force.
package accounts

import (
	"sync"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/winuser"
)

var (
	once    sync.Once
	manager *winuser.Manager
)

// Manager returns the process-wide account manager used by "managed" isolation.
//
// The name prefix is read once. Changing it needs a restart, which is correct:
// the accounts already created carry the old prefix, and a running daemon
// switching prefixes would orphan every one of them.
func Manager() *winuser.Manager {
	once.Do(func() {
		manager = winuser.New(config.Get().System.Account.Prefix)
	})
	return manager
}

// For returns the account credentials a server should run under, creating the
// account if the configured isolation mode calls for it.
//
// An empty username means the server runs as the account the daemon itself uses,
// which only happens under "shared" isolation with no account named. The
// operator has already been warned about that at boot.
func For(uuid string) (username, password string, err error) {
	c := config.Get().System.Account
	if c.Isolation == "managed" {
		return Manager().Ensure(uuid)
	}
	username, password = c.For(uuid)
	return username, password, nil
}

// Release gives up the account belonging to a server that is being deleted.
//
// A no-op under the modes whose accounts the operator owns: a pool account is
// shared across restarts and reassigned to whatever server hashes onto it next,
// so deleting one server must not disturb it.
func Release(uuid string) error {
	if config.Get().System.Account.Isolation != "managed" {
		return nil
	}
	return Manager().Remove(uuid)
}

// Invalidate discards a cached password so the next For reissues one.
//
// Called after a logon failure, which under "managed" isolation means the
// account's password was changed outside the daemon.
func Invalidate(uuid string) {
	if config.Get().System.Account.Isolation != "managed" {
		return
	}
	Manager().Invalidate(uuid)
}
