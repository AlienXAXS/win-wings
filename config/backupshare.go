package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/apex/log"
	"gopkg.in/yaml.v2"

	"github.com/pterodactyl/wings/internal/smbconn"
)

// BackupShareConfiguration is how the daemon authenticates to an SMB share
// used as system.backup_directory. It is consulted only when that directory
// is a UNC path; a local path ignores it entirely.
//
// Credentials go either inline or in a separate file, not both. The file is
// the better choice when config.yml is copied around or pasted from the Panel,
// and is re-read on every connection so a password can be rotated without a
// restart. It holds the same two keys:
//
//	username: nasuser
//	password: hunter2
//
// With neither set the daemon connects as its own service account, which uses
// anything stored for that account with cmdkey.
type BackupShareConfiguration struct {
	// Username as the NAS expects it: "nasuser", or "NAS\nasuser" if it wants
	// the server name as a domain.
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// CredentialsFile is a YAML file holding username and password. Lock it
	// down to Administrators and the daemon's account.
	CredentialsFile string `yaml:"credentials_file"`
}

type shareCredentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Credentials resolves the username and password to connect with.
func (c BackupShareConfiguration) Credentials() (username, password string, err error) {
	if c.CredentialsFile == "" {
		return c.Username, c.Password, nil
	}
	if c.Username != "" || c.Password != "" {
		return "", "", errors.New("config: system.backup_share sets both credentials_file and an " +
			"inline username or password; use one or the other")
	}

	b, err := os.ReadFile(c.CredentialsFile)
	if err != nil {
		return "", "", fmt.Errorf("config: backup share credentials: %w", err)
	}
	var creds shareCredentials
	if err := yaml.UnmarshalStrict(b, &creds); err != nil {
		return "", "", fmt.Errorf("config: backup share credentials %s: %w", c.CredentialsFile, err)
	}
	if creds.Username == "" {
		return "", "", fmt.Errorf("config: backup share credentials %s has no username", c.CredentialsFile)
	}
	return creds.Username, creds.Password, nil
}

// BackupDirectoryIsShare reports whether backups go to an SMB share.
func (sc *SystemConfiguration) BackupDirectoryIsShare() bool {
	_, ok := smbconn.ShareRoot(sc.BackupDirectory)
	return ok
}

// EnsureBackupDirectory makes the backup directory usable, connecting to its
// share first when it is a UNC path.
//
// Cheap to call before every backup operation: when the directory is already
// reachable it is a single stat. When it is not — the daemon booted while the
// NAS was down, or the NAS rebooted under a connection the redirector could
// not restore — it reconnects.
func (sc *SystemConfiguration) EnsureBackupDirectory() error {
	dir := sc.BackupDirectory
	share, ok := smbconn.ShareRoot(dir)
	if !ok {
		return os.MkdirAll(dir, 0o700)
	}

	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return nil
	}

	user, pass, err := sc.BackupShare.Credentials()
	if err != nil {
		return err
	}
	if err := smbconn.Connect(share, user, pass); err != nil {
		if !errors.Is(err, smbconn.ErrCredentialConflict) {
			return err
		}
		// The connection already there may have the access needed. The mkdir
		// below decides; this explains it if it does not.
		log.WithField("share", share).Warn(err.Error())
	} else {
		fields := log.Fields{"share": share, "username": user}
		if user == "" {
			fields["username"] = "(daemon account)"
		}
		log.WithFields(fields).Info("connected to backup share")
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: backup directory %s: %w", dir, err)
	}
	return nil
}
