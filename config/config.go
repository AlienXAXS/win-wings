package config

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/creasty/defaults"
	"github.com/gbrlsnchs/jwt/v3"
	"golang.org/x/sys/windows/registry"
	"gopkg.in/yaml.v2"
)

const DefaultLocation = `C:\ProgramData\WinWings\config.yml`

// DefaultTLSConfig sets sane defaults to use when configuring the internal
// webserver to listen for public connections.
//
// @see https://blog.cloudflare.com/exposing-go-on-the-internet
var DefaultTLSConfig = &tls.Config{
	NextProtos: []string{"h2", "http/1.1"},
	CipherSuites: []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	},
	PreferServerCipherSuites: true,
	MinVersion:               tls.VersionTLS12,
	MaxVersion:               tls.VersionTLS13,
	CurvePreferences:         []tls.CurveID{tls.X25519, tls.CurveP256},
}

var (
	mu            sync.RWMutex
	_config       *Configuration
	_jwtAlgo      *jwt.HMACSHA
	_debugViaFlag bool
)

// Locker specific to writing the configuration to the disk, this happens
// in areas that might already be locked, so we don't want to crash the process.
var _writeLock sync.Mutex

// SftpConfiguration defines the configuration of the internal SFTP server.
type SftpConfiguration struct {
	// The bind address of the SFTP server.
	Address string `default:"0.0.0.0" json:"bind_address" yaml:"bind_address"`
	// The bind port of the SFTP server.
	Port int `default:"2022" json:"bind_port" yaml:"bind_port"`
	// If set to true, no write actions will be allowed on the SFTP server.
	ReadOnly bool `default:"false" yaml:"read_only"`
}

// ApiConfiguration defines the configuration for the internal API that is
// exposed by the Wings webserver.
type ApiConfiguration struct {
	// The interface that the internal webserver should bind to.
	Host string `default:"0.0.0.0" yaml:"host"`

	// The port that the internal webserver should bind to.
	Port int `default:"8080" yaml:"port"`

	// SSL configuration for the daemon.
	Ssl struct {
		Enabled         bool   `json:"enabled" yaml:"enabled"`
		CertificateFile string `json:"cert" yaml:"cert"`
		KeyFile         string `json:"key" yaml:"key"`
	}

	// Determines if functionality for allowing remote download of files into server directories
	// is enabled on this instance. If set to "true" remote downloads will not be possible for
	// servers.
	DisableRemoteDownload bool `json:"-" yaml:"disable_remote_download"`

	// The maximum size for files uploaded through the Panel in MB.
	UploadLimit int64 `default:"100" json:"upload_limit" yaml:"upload_limit"`

	// A list of IP address of proxies that may send a X-Forwarded-For header to set the true clients IP
	TrustedProxies []string `json:"trusted_proxies" yaml:"trusted_proxies"`
}

// RemoteQueryConfiguration defines the configuration settings for remote requests
// from Wings to the Panel.
type RemoteQueryConfiguration struct {
	// The amount of time in seconds that Wings should allow for a request to the Panel API
	// to complete. If this time passes the request will be marked as failed. If your requests
	// are taking longer than 30 seconds to complete it is likely a performance issue that
	// should be resolved on the Panel, and not something that should be resolved by upping this
	// number.
	Timeout int `default:"30" yaml:"timeout"`

	// The number of servers to load in a single request to the Panel API when booting the
	// Wings instance. A single request is initially made to the Panel to get this number
	// of servers, and then the pagination status is checked and additional requests are
	// fired off in parallel to request the remaining pages.
	//
	// It is not recommended to change this from the default as you will likely encounter
	// memory limits on your Panel instance. In the grand scheme of things 4 requests for
	// 50 servers is likely just as quick as two for 100 or one for 400, and will certainly
	// be less likely to cause performance issues on the Panel.
	BootServersPerPage int `default:"50" yaml:"boot_servers_per_page"`
}

// SystemConfiguration defines basic system configuration settings.
type SystemConfiguration struct {
	// The root directory where all of the win-wings data is stored at.
	RootDirectory string `default:"C:\\ProgramData\\WinWings" json:"-" yaml:"root_directory"`

	// Directory where logs for server installations and other wings events are logged.
	LogDirectory string `default:"C:\\ProgramData\\WinWings\\logs" json:"-" yaml:"log_directory"`

	// Directory where the server data is stored at. This tree is writable by the
	// account server processes run under, and is the root exposed over SFTP.
	//
	// Nothing the daemon relies on may live here — see InstanceDirectory.
	Data string `default:"C:\\ProgramData\\WinWings\\volumes" json:"-" yaml:"data"`

	// InstanceDirectory holds per-server worker state: the worker's configuration,
	// the resolved startup command, and its console log.
	//
	// This is deliberately a sibling of Data rather than a subdirectory of each
	// server's files. The contents authorise what the worker executes, so a
	// server able to write its own instance directory could rewrite its startup
	// command and achieve arbitrary code execution as the account it runs under.
	// The account running server processes must be denied write access here.
	InstanceDirectory string `default:"C:\\ProgramData\\WinWings\\instances" json:"-" yaml:"instance_directory"`

	// Directory where server archives for transferring will be stored.
	ArchiveDirectory string `default:"C:\\ProgramData\\WinWings\\archives" json:"-" yaml:"archive_directory"`

	// Directory where local backups will be stored on the machine.
	BackupDirectory string `default:"C:\\ProgramData\\WinWings\\backups" json:"-" yaml:"backup_directory"`

	// TmpDirectory specifies where temporary files for installation processes
	// should be created.
	TmpDirectory string `default:"C:\\ProgramData\\WinWings\\tmp" json:"-" yaml:"tmp_directory"`

	// The timezone for this Wings instance. This is detected by Wings automatically if possible,
	// and falls back to UTC if not able to be detected. If you need to set this manually, that
	// can also be done.
	//
	// This timezone value is passed into all containers created by Wings.
	Timezone string `yaml:"timezone"`

	// Account configures the Windows account(s) that server processes run under.
	Account AccountConfiguration `yaml:"account"`

	// The amount of time in seconds that can elapse before a server's disk space calculation is
	// considered stale and a re-check should occur. DANGER: setting this value too low can seriously
	// impact system performance and cause massive I/O bottlenecks and high CPU usage for the Wings
	// process.
	//
	// Set to 0 to disable disk checking entirely. This will always return 0 for the disk space used
	// by a server and should only be set in extreme scenarios where performance is critical and
	// disk usage is not a concern.
	DiskCheckInterval int64 `default:"150" yaml:"disk_check_interval"`

	// ActivitySendInterval is the amount of time that should ellapse between aggregated server activity
	// being sent to the Panel. By default this will send activity collected over the last minute. Keep
	// in mind that only a fixed number of activity log entries, defined by ActivitySendCount, will be sent
	// in each run.
	ActivitySendInterval int `default:"60" yaml:"activity_send_interval"`

	// ActivitySendCount is the number of activity events to send per batch.
	ActivitySendCount int `default:"100" yaml:"activity_send_count"`

	// If set to true, file permissions for a server will be checked when the process is
	// booted. This can cause boot delays if the server has a large amount of files. In most
	// cases disabling this should not have any major impact unless external processes are
	// frequently modifying a servers' files.
	CheckPermissionsOnBoot bool `default:"true" yaml:"check_permissions_on_boot"`

	// The number of lines to send when a server connects to the websocket.
	WebsocketLogCount int `default:"150" yaml:"websocket_log_count"`

	Sftp SftpConfiguration `yaml:"sftp"`

	CrashDetection CrashDetection `yaml:"crash_detection"`

	Backups Backups `yaml:"backups"`

	Transfers Transfers `yaml:"transfers"`
}

type CrashDetection struct {
	// CrashDetectionEnabled sets if crash detection is enabled globally for all servers on this node.
	CrashDetectionEnabled bool `default:"true" yaml:"enabled"`

	// Determines if Wings should detect a server that stops with a normal exit code of
	// "0" as being crashed if the process stopped without any Wings interaction. E.g.
	// the user did not press the stop button, but the process stopped cleanly.
	DetectCleanExitAsCrash bool `default:"true" yaml:"detect_clean_exit_as_crash"`

	// Timeout specifies the timeout between crashes that will not cause the server
	// to be automatically restarted, this value is used to prevent servers from
	// becoming stuck in a boot-loop after multiple consecutive crashes.
	Timeout int `default:"60" json:"timeout"`
}

type Backups struct {
	// WriteLimit imposes a Disk I/O write limit on backups to the disk, this affects all
	// backup drivers as the archiver must first write the file to the disk in order to
	// upload it to any external storage provider.
	//
	// If the value is less than 1, the write speed is unlimited,
	// if the value is greater than 0, the write speed is the value in MiB/s.
	//
	// Defaults to 0 (unlimited)
	WriteLimit int `default:"0" yaml:"write_limit"`

	// CompressionLevel determines how much backups created by wings should be compressed.
	//
	// "none" -> no compression will be applied
	// "best_speed" -> uses gzip level 1 for fast speed
	// "best_compression" -> uses gzip level 9 for minimal disk space useage
	//
	// Defaults to "best_speed" (level 1)
	CompressionLevel string `default:"best_speed" yaml:"compression_level"`

	// RestoreHostAllowlist allows backup restore downloads to connect to otherwise blocked
	// private/internal destinations. Entries may be hostnames, IP addresses, or CIDR ranges.
	RestoreHostAllowlist []string `yaml:"restore_host_allowlist"`
}

type Transfers struct {
	// DownloadLimit imposes a Network I/O read limit when downloading a transfer archive.
	//
	// If the value is less than 1, the write speed is unlimited,
	// if the value is greater than 0, the write speed is the value in MiB/s.
	//
	// Defaults to 0 (unlimited)
	DownloadLimit int `default:"0" yaml:"download_limit"`
}

type ConsoleThrottles struct {
	// Whether or not the throttler is enabled for this instance.
	Enabled bool `json:"enabled" yaml:"enabled" default:"true"`

	// The total number of lines that can be output in a given Period period before
	// a warning is triggered and counted against the server.
	Lines uint64 `json:"lines" yaml:"lines" default:"2000"`

	// The amount of time after which the number of lines processed is reset to 0. This runs in
	// a constant loop and is not affected by the current console output volumes. By default, this
	// will reset the processed line count back to 0 every 100ms.
	Period uint64 `json:"line_reset_interval" yaml:"line_reset_interval" default:"100"`
}

type Token struct {
	ID    string
	Token string
}

type Configuration struct {
	Token Token `json:"-" yaml:"-"`

	// The location from which this configuration instance was instantiated.
	path string

	// Determines if wings should be running in debug mode. This value is ignored
	// if the debug flag is passed through the command line arguments.
	Debug bool

	AppName string `default:"Pterodactyl" json:"app_name" yaml:"app_name"`

	// A unique identifier for this node in the Panel.
	Uuid string

	// An identifier for the token which must be included in any requests to the panel
	// so that the token can be looked up correctly.
	AuthenticationTokenId string `json:"token_id" yaml:"token_id"`

	// The token used when performing operations. Requests to this instance must
	// validate against it.
	AuthenticationToken string `json:"token" yaml:"token"`

	Api    ApiConfiguration    `json:"api" yaml:"api"`
	System SystemConfiguration `json:"system" yaml:"system"`
	Runtime RuntimeConfiguration `json:"runtime" yaml:"runtime"`

	// Defines internal throttling configurations for server processes to prevent
	// someone from running an endless loop that spams data to logs.
	Throttles ConsoleThrottles

	// The location where the panel is running that this daemon should connect to
	// to collect data and send events.
	PanelLocation string                   `json:"-" yaml:"remote"`
	RemoteQuery   RemoteQueryConfiguration `json:"remote_query" yaml:"remote_query"`

	// AllowedMounts is a list of allowed host-system mount points.
	// This is required to have the "Server Mounts" feature work properly.
	AllowedMounts []string `json:"-" yaml:"allowed_mounts"`

	// AllowedOrigins is a list of allowed request origins.
	// The Panel URL is automatically allowed, this is only needed for adding
	// additional origins.
	AllowedOrigins []string `json:"allowed_origins" yaml:"allowed_origins"`

	// AllowCORSPrivateNetwork sets the `Access-Control-Request-Private-Network` header which
	// allows client browsers to make requests to internal IP addresses over HTTP.  This setting
	// is only required by users running Wings without SSL certificates and using internal IP
	// addresses in order to connect. Most users should NOT enable this setting.
	AllowCORSPrivateNetwork bool `json:"allow_cors_private_network" yaml:"allow_cors_private_network"`

	// IgnorePanelConfigUpdates causes confiuration updates that are sent by the panel to be ignored.
	IgnorePanelConfigUpdates bool `json:"ignore_panel_config_updates" yaml:"ignore_panel_config_updates"`
}

// NewAtPath creates a new struct and set the path where it should be stored.
// This function does not modify the currently stored global configuration.
func NewAtPath(path string) (*Configuration, error) {
	var c Configuration
	// Configures the default values for many of the configuration options present
	// in the structs. Values set in the configuration file take priority over the
	// default values.
	if err := defaults.Set(&c); err != nil {
		return nil, err
	}
	// Track the location where we created this configuration.
	c.path = path
	return &c, nil
}

// Set the global configuration instance. This is a blocking operation such that
// anything trying to set a different configuration value, or read the configuration
// will be paused until it is complete.
func Set(c *Configuration) {
	mu.Lock()
	defer mu.Unlock()
	token := c.Token.Token
	if token == "" {
		c.Token.Token = c.AuthenticationToken
		token = c.Token.Token
	}
	if _config == nil || _config.Token.Token != token {
		_jwtAlgo = jwt.NewHS256([]byte(token))
	}
	_config = c
}

// ResolveToken populates the derived Token field, preferring values pinned
// through the environment over those in the configuration itself.
//
// Set remote when the values came from the Panel. Local values may use
// "file://" or "$VAR" indirection; expanding one sent over the network would
// leak files and environment variables back out through the token we attach to
// every request. Environment overrides must already match remote values so a
// configuration update cannot leave Wings and the Panel using different keys.
func (c *Configuration) ResolveToken(remote bool) error {
	resolve := func(name, env, local string) (string, error) {
		if remote && (strings.Contains(local, "$") || strings.HasPrefix(local, "file://")) {
			return "", fmt.Errorf("config: remote %s cannot use token indirection", name)
		}
		if env != "" {
			value, err := Expand(env)
			if err != nil {
				return "", err
			}
			if remote && value != local {
				return "", fmt.Errorf("config: remote %s does not match environment override", name)
			}
			return value, nil
		}
		if remote {
			return local, nil
		}
		return Expand(local)
	}

	var err error
	if c.Token.ID, err = resolve("token ID", os.Getenv("WINGS_TOKEN_ID"), c.AuthenticationTokenId); err != nil {
		return err
	}
	if c.Token.Token, err = resolve("token", os.Getenv("WINGS_TOKEN"), c.AuthenticationToken); err != nil {
		return err
	}
	return nil
}

// SetDebugViaFlag tracks if the application is running in debug mode because of
// a command line flag argument. If so we do not want to store that configuration
// change to the disk.
func SetDebugViaFlag(d bool) {
	mu.Lock()
	defer mu.Unlock()
	_config.Debug = d
	_debugViaFlag = d
}

// Get returns the global configuration instance. This is a thread-safe operation
// that will block if the configuration is presently being modified.
//
// Be aware that you CANNOT make modifications to the currently stored configuration
// by modifying the struct returned by this function. The only way to make
// modifications is by using the Update() function and passing data through in
// the callback.
func Get() *Configuration {
	mu.RLock()
	// Create a copy of the struct so that all modifications made beyond this
	// point are immutable.
	//goland:noinspection GoVetCopyLock
	c := *_config
	mu.RUnlock()
	return &c
}

// Update performs an in-situ update of the global configuration object using
// a thread-safe mutex lock. This is the correct way to make modifications to
// the global configuration.
func Update(callback func(c *Configuration)) {
	mu.Lock()
	defer mu.Unlock()
	callback(_config)
}

// GetJwtAlgorithm returns the in-memory JWT algorithm.
func GetJwtAlgorithm() *jwt.HMACSHA {
	mu.RLock()
	defer mu.RUnlock()
	return _jwtAlgo
}

// WriteToDisk writes the configuration to the disk. This is a thread safe operation
// and will only allow one write at a time. Additional calls while writing are
// queued up.
func WriteToDisk(c *Configuration) error {
	_writeLock.Lock()
	defer _writeLock.Unlock()

	//goland:noinspection GoVetCopyLock
	ccopy := *c
	// If debugging is set with the flag, don't save that to the configuration file,
	// otherwise you'll always end up in debug mode.
	if _debugViaFlag {
		ccopy.Debug = false
	}
	if c.path == "" {
		return errors.New("cannot write configuration, no path defined in struct")
	}
	b, err := yaml.Marshal(&ccopy)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path, b, 0o600); err != nil {
		return err
	}
	return nil
}



// FromFile reads the configuration from the provided file and stores it in the
// global singleton for this instance.
func FromFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c, err := NewAtPath(path)
	if err != nil {
		return err
	}

	if err := yaml.Unmarshal(b, c); err != nil {
		return err
	}

	if err := c.ResolveToken(false); err != nil {
		return err
	}

	// Store this configuration in the global state.
	Set(c)
	return nil
}

// ConfigureDirectories ensures that all the system directories exist on the
// system. These directories are created so that only the owner can read the data,
// and no other users.
//
// This function IS NOT thread-safe.
func ConfigureDirectories() error {
	root := _config.System.RootDirectory
	log.WithField("path", root).Debug("ensuring root data directory exists")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}

	// There are a non-trivial number of users out there whose data directories are actually a
	// symlink to another location on the disk. If we do not resolve that final destination at this
	// point things will appear to work, but endless errors will be encountered when we try to
	// verify accessed paths since they will all end up resolving outside the expected data directory.
	//
	// For the sake of automating away as much of this as possible, see if the data directory is a
	// symlink, and if so resolve to its final real path, and then update the configuration to use
	// that.
	if d, err := filepath.EvalSymlinks(_config.System.Data); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if d != _config.System.Data {
		_config.System.Data = d
	}

	log.WithField("path", _config.System.Data).Debug("ensuring server data directory exists")
	if err := os.MkdirAll(_config.System.Data, 0o700); err != nil {
		return err
	}

	log.WithField("path", _config.System.TmpDirectory).Debug("ensuring temporary data directory exists")
	if err := os.MkdirAll(_config.System.TmpDirectory, 0o700); err != nil {
		return err
	}

	log.WithField("path", _config.System.ArchiveDirectory).Debug("ensuring archive data directory exists")
	if err := os.MkdirAll(_config.System.ArchiveDirectory, 0o700); err != nil {
		return err
	}

	log.WithField("path", _config.System.BackupDirectory).Debug("ensuring backup data directory exists")
	if err := os.MkdirAll(_config.System.BackupDirectory, 0o700); err != nil {
		return err
	}

	log.WithField("path", _config.System.InstanceDirectory).Debug("ensuring instance directory exists")
	if err := os.MkdirAll(_config.System.InstanceDirectory, 0o700); err != nil {
		return err
	}

	return nil
}


// GetStatesPath returns the location of the JSON file that tracks server states.
func (sc *SystemConfiguration) GetStatesPath() string {
	return path.Join(sc.RootDirectory, "/states.json")
}

// ConfigureTimezone sets the timezone data for the configuration if it is
// currently missing. If a value has been set, this functionality will only run
// to validate that the timezone being used is valid.
//
// This function IS NOT thread-safe.
func ConfigureTimezone() error {
	if _config.System.Timezone == "" {
		if tz := os.Getenv("TZ"); tz != "" {
			_config.System.Timezone = tz
		}
	}

	// Windows identifies time zones by its own key names ("GMT Standard Time")
	// rather than IANA names ("Europe/London"), and Go ships no mapping between
	// the two. Rather than guess, read the system zone only to report it, and
	// require the operator to set system.timezone explicitly.
	//
	// This value is handed to server processes as TZ, where runtimes such as the
	// JVM and Node expect an IANA name. Defaulting to a silently wrong zone would
	// skew every timestamp a server writes, so prefer a loud fallback to UTC.
	//
	// TODO: embed the CLDR windowsZones mapping and resolve this automatically.
	if _config.System.Timezone == "" {
		_config.System.Timezone = "UTC"
		if name, err := systemTimezoneKeyName(); err != nil {
			log.WithField("error", err).Warn("failed to determine the system time zone, falling back to UTC")
		} else {
			log.WithField("windows_timezone", name).
				Warn("no system.timezone configured and Windows zone names cannot be mapped to IANA automatically; " +
					"falling back to UTC — set system.timezone in the config to the correct IANA name")
		}
		return nil
	}

	_config.System.Timezone = regexp.MustCompile(`(?i)[^a-z_/+\-0-9]+`).ReplaceAllString(_config.System.Timezone, "")
	if _, err := time.LoadLocation(_config.System.Timezone); err != nil {
		return errors.WithMessage(err, fmt.Sprintf("the supplied timezone %s is invalid", _config.System.Timezone))
	}
	return nil
}

// systemTimezoneKeyName returns the Windows time zone key name configured on the
// host, e.g. "GMT Standard Time". This is not an IANA identifier.
func systemTimezoneKeyName() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\TimeZoneInformation`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()

	name, _, err := k.GetStringValue("TimeZoneKeyName")
	if err != nil {
		return "", err
	}
	return name, nil
}



// Expand expands an input string by calling [os.ExpandEnv] to expand all
// environment variables, then checks if the value is prefixed with `file://`
// to support reading the value from a file.
//
// NOTE: the order of expanding environment variables first then checking if
// the value references a file is important. This behaviour allows a user to
// pass a value like `file://${CREDENTIALS_DIRECTORY}/token` to allow us to
// work with credentials loaded by systemd's `LoadCredential` (or `LoadCredentialEncrypted`)
// options without the user needing to assume the path of `CREDENTIALS_DIRECTORY`
// or use a preStart script to read the files for us.
func Expand(v string) (string, error) {
	// Expand environment variables within the string.
	//
	// NOTE: this may cause issues if the string contains `$` and doesn't intend
	// on getting expanded, however we are using this for our tokens which are
	// all alphanumeric characters only.
	v = os.ExpandEnv(v)

	// Handle files.
	const filePrefix = "file://"
	if strings.HasPrefix(v, filePrefix) {
		p := v[len(filePrefix):]

		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		v = string(bytes.TrimRight(bytes.TrimRight(b, "\r"), "\n"))
	}

	return v, nil
}
