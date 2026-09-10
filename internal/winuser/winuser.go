//go:build windows

// Package winuser creates and manages the local Windows accounts that server
// processes run under.
//
// The alternative was to make the operator create an account per server by hand,
// keep its password in the daemon's configuration file, and maintain the two in
// step forever. That is a poor trade: the passwords then exist at rest, the pool
// caps how many servers a node can run, and adding a server becomes a manual
// step on the host rather than a click in the Panel.
//
// So the daemon owns them. It creates one account per server, names it after the
// server's UUID, gives it a random password it never writes down, grants it the
// single logon right it needs and explicitly denies the rest, and deletes it
// when the server is deleted.
//
// Passwords are deliberately not persisted. Rather than store a secret the
// daemon would then have to protect, an account's password is reset to a fresh
// random value the first time the daemon needs it after a restart, and kept only
// in memory. Resetting a password does not disturb processes already running
// under that account, because their tokens were issued at logon and are not
// revalidated afterwards.
//
// All of this requires administrator rights, which is why the daemon runs as an
// administrative service account. See config/validate_windows.go for what that
// changes about the security posture and what is done to contain it.
package winuser

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modadvapi32 = windows.NewLazySystemDLL("advapi32.dll")
	modnetapi32 = windows.NewLazySystemDLL("netapi32.dll")

	procNetUserAdd             = modnetapi32.NewProc("NetUserAdd")
	procNetUserDel             = modnetapi32.NewProc("NetUserDel")
	procNetUserSetInfo         = modnetapi32.NewProc("NetUserSetInfo")
	procNetUserGetLocalGroups  = modnetapi32.NewProc("NetUserGetLocalGroups")
	procNetLocalGroupAddMembrs = modnetapi32.NewProc("NetLocalGroupAddMembers")
)

// Network management status codes worth distinguishing from the rest.
const (
	nerrUserNotFound   = 2221
	nerrPasswordPolicy = 2245
	errorMemberInAlias = 1378
)

// USER_INFO_1, the level used to create an account.
type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

// USER_INFO_1003, which carries nothing but a password.
type userInfo1003 struct {
	Password *uint16
}

// LOCALGROUP_MEMBERS_INFO_0, which carries nothing but a SID.
type localgroupMembersInfo0 struct {
	SID *windows.SID
}

// GROUP_USERS_INFO_0, which carries nothing but a group name.
type groupUsersInfo0 struct {
	Name *uint16
}

const (
	userPrivUser = 1

	ufScript           = 0x0001
	ufPasswdCantChange = 0x0040
	ufNormalAccount    = 0x0200
	ufDontExpirePasswd = 0x10000

	// LG_INCLUDE_INDIRECT walks nested group membership rather than just direct
	// members, which is what a membership check has to do to be meaningful.
	lgIncludeIndirect = 0x0001
)

// accountFlags is what every account this package creates is given.
//
// UF_SCRIPT is required on NT and is meaningless beyond that.
// UF_DONT_EXPIRE_PASSWD stops a password policy from expiring a password nobody
// can type, and UF_PASSWD_CANT_CHANGE stops anything running as the account from
// changing the password out from under the daemon.
const accountFlags = ufScript | ufNormalAccount | ufDontExpirePasswd | ufPasswdCantChange

// markerPrefix identifies an account as belonging to this daemon.
//
// It is written to the account's comment field and checked before the daemon
// touches an account it did not create. Without it a name collision with a
// pre-existing account would silently reset that account's password.
const markerPrefix = "win-wings server "

// netError converts a NET_API_STATUS return value into an error.
func netError(r uintptr) error {
	if r == 0 {
		return nil
	}
	return windows.Errno(r)
}

// Create makes a local account with the given password and comment.
//
// Fails with ERROR_ACCESS_DENIED if the daemon is not an administrator.
func Create(name, password, comment string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	passPtr, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	commentPtr, err := windows.UTF16PtrFromString(comment)
	if err != nil {
		return err
	}

	info := userInfo1{
		Name:     namePtr,
		Password: passPtr,
		Priv:     userPrivUser,
		Comment:  commentPtr,
		Flags:    accountFlags,
	}

	var parmErr uint32
	r, _, _ := procNetUserAdd.Call(
		0, // this host
		1, // USER_INFO_1
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&parmErr)),
	)
	if err := netError(r); err != nil {
		if r == nerrPasswordPolicy {
			return fmt.Errorf(
				"winuser: the host's password policy rejected the generated password for %q. "+
					"Generated passwords are %d characters with mixed case, digits and symbols; "+
					"a policy stricter than that needs relaxing: %w",
				name, passwordLength, err)
		}
		return fmt.Errorf("winuser: could not create the account %q: %w", name, err)
	}
	return nil
}

// SetPassword resets an account's password.
func SetPassword(name, password string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	passPtr, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}

	info := userInfo1003{Password: passPtr}

	var parmErr uint32
	r, _, _ := procNetUserSetInfo.Call(
		0,
		uintptr(unsafe.Pointer(namePtr)),
		1003, // USER_INFO_1003
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&parmErr)),
	)
	if err := netError(r); err != nil {
		return fmt.Errorf("winuser: could not set the password for %q: %w", name, err)
	}
	return nil
}

// Delete removes a local account.
//
// An account that is already gone is not an error, so that cleaning up after a
// partial failure is idempotent.
func Delete(name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(namePtr)))
	if r == nerrUserNotFound {
		return nil
	}
	if err := netError(r); err != nil {
		return fmt.Errorf("winuser: could not delete the account %q: %w", name, err)
	}
	return nil
}

// Info describes an existing local account.
type Info struct {
	Name    string
	Comment string
	Flags   uint32
}

// ManagedFor reports whether this daemon created the account for a given server.
func (i Info) ManagedFor(uuid string) bool {
	return i.Comment == markerPrefix+uuid
}

// Managed reports whether this daemon created the account at all.
func (i Info) Managed() bool {
	return strings.HasPrefix(i.Comment, markerPrefix)
}

// Lookup returns information about a local account, or nil if there is no such
// account.
func Lookup(name string) (*Info, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}

	var buf *byte
	if err := windows.NetUserGetInfo(nil, namePtr, 1, &buf); err != nil {
		var errno windows.Errno
		if e, ok := err.(windows.Errno); ok {
			errno = e
		}
		if uintptr(errno) == nerrUserNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("winuser: could not read the account %q: %w", name, err)
	}
	defer windows.NetApiBufferFree(buf)

	info := (*userInfo1)(unsafe.Pointer(buf))
	out := &Info{
		Name:  windows.UTF16PtrToString(info.Name),
		Flags: info.Flags,
	}
	if info.Comment != nil {
		out.Comment = windows.UTF16PtrToString(info.Comment)
	}
	return out, nil
}

// SID resolves a local account name to its security identifier.
func SID(name string) (*windows.SID, error) {
	sid, _, _, err := windows.LookupSID("", name)
	if err != nil {
		return nil, fmt.Errorf("winuser: could not resolve the account %q: %w", name, err)
	}
	return sid, nil
}

// LocalGroups returns the names of the local groups an account belongs to,
// including membership inherited through nested groups.
func LocalGroups(name string) ([]string, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}

	var buf *byte
	var read, total uint32
	r, _, _ := procNetUserGetLocalGroups.Call(
		0,
		uintptr(unsafe.Pointer(namePtr)),
		0, // GROUP_USERS_INFO_0
		lgIncludeIndirect,
		uintptr(unsafe.Pointer(&buf)),
		0xFFFFFFFF, // MAX_PREFERRED_LENGTH
		uintptr(unsafe.Pointer(&read)),
		uintptr(unsafe.Pointer(&total)),
	)
	if err := netError(r); err != nil {
		return nil, fmt.Errorf("winuser: could not read group membership for %q: %w", name, err)
	}
	defer windows.NetApiBufferFree(buf)

	if read == 0 {
		return nil, nil
	}
	entries := unsafe.Slice((*groupUsersInfo0)(unsafe.Pointer(buf)), read)
	out := make([]string, 0, read)
	for i := range entries {
		out = append(out, windows.UTF16PtrToString(entries[i].Name))
	}
	return out, nil
}

// IsAdministrator reports whether an account is in the local Administrators
// group.
//
// This is checked before the daemon will run a server as an account. An
// administrative account makes the NTFS separation between servers meaningless,
// because it can take ownership of any file regardless of the ACL.
func IsAdministrator(name string) (bool, error) {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	// The group is named differently in each display language, so it has to be
	// resolved from the well-known SID rather than spelled out.
	group, _, _, err := admins.LookupAccount("")
	if err != nil {
		return false, fmt.Errorf("winuser: could not resolve the Administrators group: %w", err)
	}

	groups, err := LocalGroups(name)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if strings.EqualFold(g, group) {
			return true, nil
		}
	}
	return false, nil
}

// AddToAdministrators puts an account in the local Administrators group.
//
// Used only for the daemon's own service account, which needs to be able to
// create the per-server accounts and rewrite NTFS permissions.
func AddToAdministrators(sid *windows.SID) error {
	return addToWellKnownGroup(sid, windows.WinBuiltinAdministratorsSid, "Administrators")
}

// AddToPerformanceLogUsers puts an account in the local Performance Log Users
// group, which is what lets a non-administrator start an Event Tracing for
// Windows session. The daemon needs one for per-server network statistics.
func AddToPerformanceLogUsers(sid *windows.SID) error {
	return addToWellKnownGroup(sid, windows.WinBuiltinPerfLoggingUsersSid, "Performance Log Users")
}

// addToWellKnownGroup adds an account to a built-in local group identified by
// its well-known SID. The English name is only for error messages.
func addToWellKnownGroup(sid *windows.SID, kind windows.WELL_KNOWN_SID_TYPE, english string) error {
	groupSID, err := windows.CreateWellKnownSid(kind)
	if err != nil {
		return err
	}
	// The group is named differently in each display language, so it has to be
	// resolved from the well-known SID rather than spelled out.
	group, _, _, err := groupSID.LookupAccount("")
	if err != nil {
		return fmt.Errorf("winuser: could not resolve the %s group: %w", english, err)
	}
	groupPtr, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}

	member := localgroupMembersInfo0{SID: sid}
	r, _, _ := procNetLocalGroupAddMembrs.Call(
		0,
		uintptr(unsafe.Pointer(groupPtr)),
		0, // LOCALGROUP_MEMBERS_INFO_0
		uintptr(unsafe.Pointer(&member)),
		1,
	)
	if r == errorMemberInAlias {
		return nil // already a member
	}
	if err := netError(r); err != nil {
		return fmt.Errorf("winuser: could not add the account to %q: %w", group, err)
	}
	return nil
}

// Password character classes, chosen so a generated password satisfies the
// default Windows complexity policy while avoiding characters that a command
// line, an INI file or a YAML document would treat specially. Visually
// ambiguous characters are left out so a password can be read aloud if one ever
// has to be.
const (
	pwUpper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	pwLower  = "abcdefghijkmnopqrstuvwxyz"
	pwDigit  = "23456789"
	pwSymbol = "!@#$%^&*()-_=+?"
)

// passwordLength is far beyond anything that would be guessed and well under the
// 256-character limit the network management API imposes.
const passwordLength = 24

// GeneratePassword returns a random password satisfying the default Windows
// complexity requirements.
//
// One character is drawn from each class first so the result cannot fail the
// policy by chance, then the whole thing is shuffled so those positions are not
// predictable.
func GeneratePassword() (string, error) {
	classes := []string{pwUpper, pwLower, pwDigit, pwSymbol}
	all := strings.Join(classes, "")

	out := make([]byte, 0, passwordLength)
	for _, class := range classes {
		c, err := pick(class)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < passwordLength {
		c, err := pick(all)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}

	// Fisher-Yates, with a cryptographic source.
	for i := len(out) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", fmt.Errorf("winuser: could not generate a password: %w", err)
		}
		j := n.Int64()
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

func pick(set string) (byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
	if err != nil {
		return 0, fmt.Errorf("winuser: could not generate a password: %w", err)
	}
	return set[n.Int64()], nil
}
