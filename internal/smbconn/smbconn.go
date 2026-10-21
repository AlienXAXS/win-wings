//go:build windows

// Package smbconn authenticates the daemon to an SMB share.
//
// On Linux a NAS is mounted once by the host and the daemon just sees a
// directory. The Windows equivalent is a connection held by the redirector for
// one logon session, and the daemon runs as its own service account in its own
// session: a share mapped by an administrator at the desktop is invisible to
// it. So the daemon makes the connection itself.
//
// The connection is "deviceless" — no drive letter — and not remembered in any
// profile. It lasts until the session ends, and the redirector reconnects with
// the same credentials on its own if the server goes away and comes back.
package smbconn

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// golang.org/x/sys/windows does not bind the WNet functions, so they are bound
// here.
var (
	modmpr                  = windows.NewLazySystemDLL("mpr.dll")
	procWNetAddConnection2W = modmpr.NewProc("WNetAddConnection2W")
	procWNetCancelConnect2W = modmpr.NewProc("WNetCancelConnection2W")
)

const resourcetypeDisk = 1

// Error codes WNetAddConnection2 returns directly rather than through
// GetLastError, which is why they are compared as numbers.
const (
	errorAccessDenied              = 5
	errorBadNetpath                = 53
	errorBadNetName                = 67
	errorLogonFailure              = 1326
	errorSessionCredentialConflict = 1219
)

// netResource is NETRESOURCEW.
type netResource struct {
	Scope       uint32
	Type        uint32
	DisplayType uint32
	Usage       uint32
	LocalName   *uint16
	RemoteName  *uint16
	Comment     *uint16
	Provider    *uint16
}

// ErrCredentialConflict means this logon session already holds a connection
// to the same server under different credentials. SMB allows one set per
// server per session, so the new credentials were not applied — the existing
// connection is still there and may well have access, which is why callers
// should treat this as a warning and let the file operation decide.
var ErrCredentialConflict = errors.New("smbconn: this session is already connected to the " +
	"server with different credentials (error 1219); the existing connection is used instead")

// ShareRoot returns \\server\share for a UNC path, and false for anything
// else: drive-letter paths, relative paths, and the \\?\ and \\.\ device
// namespaces, which name local objects despite the leading slashes. The
// \\?\UNC\ long-path form is recognised as the UNC path it is.
//
// Forward slashes are accepted because they turn up in hand-written YAML and
// Windows accepts them.
func ShareRoot(p string) (string, bool) {
	p = strings.ReplaceAll(p, "/", `\`)

	switch {
	case hasPrefixFold(p, `\\?\UNC\`):
		p = p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`), strings.HasPrefix(p, `\\.\`):
		return "", false
	case strings.HasPrefix(p, `\\`):
		p = p[2:]
	default:
		return "", false
	}

	parts := strings.SplitN(p, `\`, 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return `\\` + parts[0] + `\` + parts[1], true
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// Connect authenticates this logon session to share, which must be in the
// \\server\share form ShareRoot returns.
//
// An empty username connects as the calling account, which picks up anything
// stored for it with cmdkey. The username is passed through as given, so
// "nasuser" and "NAS\nasuser" both work; which one a given NAS wants is up to
// the NAS.
func Connect(share, username, password string) error {
	remote, err := windows.UTF16PtrFromString(share)
	if err != nil {
		return fmt.Errorf("smbconn: share: %w", err)
	}
	var user, pass *uint16
	if username != "" {
		if user, err = windows.UTF16PtrFromString(username); err != nil {
			return fmt.Errorf("smbconn: username: %w", err)
		}
		if pass, err = windows.UTF16PtrFromString(password); err != nil {
			return fmt.Errorf("smbconn: password: %w", err)
		}
	}

	nr := netResource{Type: resourcetypeDisk, RemoteName: remote}
	r, _, _ := procWNetAddConnection2W.Call(
		uintptr(unsafe.Pointer(&nr)),
		uintptr(unsafe.Pointer(pass)),
		uintptr(unsafe.Pointer(user)),
		0, // not remembered: the daemon reconnects on every boot
	)
	return describe(share, username, windows.Errno(r))
}

// Disconnect drops this session's connection to share. Open files on it are
// closed forcibly.
func Disconnect(share string) error {
	remote, err := windows.UTF16PtrFromString(share)
	if err != nil {
		return fmt.Errorf("smbconn: share: %w", err)
	}
	r, _, _ := procWNetCancelConnect2W.Call(uintptr(unsafe.Pointer(remote)), 0, 1)
	if r != 0 {
		return fmt.Errorf("smbconn: disconnect %s: %w", share, windows.Errno(r))
	}
	return nil
}

// describe turns the codes people actually hit into something that says what
// to fix.
func describe(share, username string, e windows.Errno) error {
	who := username
	if who == "" {
		who = "the daemon's own account"
	}
	switch e {
	case 0:
		return nil
	case errorSessionCredentialConflict:
		return ErrCredentialConflict
	case errorLogonFailure:
		return fmt.Errorf("smbconn: %s rejected the username or password for %s: %w", share, who, e)
	case errorAccessDenied:
		return fmt.Errorf("smbconn: %s denied access to %s: %w", share, who, e)
	case errorBadNetpath:
		return fmt.Errorf("smbconn: the server in %s could not be reached: %w", share, e)
	case errorBadNetName:
		return fmt.Errorf("smbconn: the server is reachable but has no share named in %s: %w", share, e)
	default:
		return fmt.Errorf("smbconn: connect %s as %s: %w", share, who, e)
	}
}
