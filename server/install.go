//go:build windows

package server

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/internal/accounts"
	"github.com/pterodactyl/wings/internal/jobobject"
	"github.com/pterodactyl/wings/internal/winacl"
	"github.com/pterodactyl/wings/internal/winenv"
	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"
)

// Install executes the installation stack for a server process. Bubbles any
// errors up to the calling function which should handle contacting the panel to
// notify it of the server state.
func (s *Server) Install() error {
	return s.install(false)
}

func (s *Server) install(reinstall bool) error {
	var err error
	if !s.Config().SkipEggScripts {
		// Send the start event so the Panel can automatically update. We don't
		// send this unless the process is actually going to run, otherwise all
		// sorts of weird rapid UI behavior happens since there isn't an actual
		// install process being executed.
		s.Events().Publish(InstallStartedEvent, "")

		err = s.internalInstall()
	} else {
		s.Log().Info("server configured to skip running installation scripts for this egg, not executing process")
	}

	s.Log().WithField("was_successful", err == nil).Debug("notifying panel of server install state")
	if serr := s.SyncInstallState(err == nil, reinstall); serr != nil {
		l := s.Log().WithField("was_successful", err == nil)
		if err == nil {
			l.WithField("error", err)
		}
		l.Warn("failed to notify panel of server install state")
	}

	// Ensure that the server is marked as offline at this point, otherwise you
	// end up with a blank value which is a bit confusing.
	s.Environment.SetState(environment.ProcessOfflineState)

	// Push an event to the websocket, so we can auto-refresh the information in
	// the panel once the installation is completed.
	s.Events().Publish(InstallCompletedEvent, "")

	return errors.WithStackIf(err)
}

// Reinstall reinstalls a server's software by utilizing the installation script
// for the server egg. This does not touch any existing files for the server,
// other than what the script modifies.
func (s *Server) Reinstall() error {
	if s.Environment.State() != environment.ProcessOfflineState {
		s.Log().Debug("waiting for server instance to enter a stopped state")
		if err := s.Environment.WaitForStop(s.Context(), time.Second*10, true); err != nil {
			return errors.WrapIf(err, "install: failed to stop running environment")
		}
	}

	s.Log().Info("syncing server state with remote source before executing re-installation process")
	if err := s.Sync(); err != nil {
		return errors.WrapIf(err, "install: failed to sync server state with Panel")
	}

	return s.install(true)
}

// Internal installation function used to simplify reporting back to the Panel.
func (s *Server) internalInstall() error {
	script, err := s.client.GetInstallationScript(s.Context(), s.ID())
	if err != nil {
		return err
	}
	p, err := NewInstallationProcess(s, &script)
	if err != nil {
		return err
	}

	s.Log().Info("beginning installation process for server")
	if err := p.Run(); err != nil {
		return err
	}

	s.Log().Info("completed installation process for server")
	return nil
}

// InstallationProcess runs an egg's installation script for a server.
//
// Upstream executed the script inside a throwaway Docker container built from
// the egg's container_image, with the server's files bind-mounted at
// /mnt/server. There is no container here: the script is PowerShell, run
// directly on the host inside a Job Object that bounds what it can consume.
//
// That difference is why every egg needs a Windows-specific install script. The
// Panel-side Blueprint plugin supplies it; see the project documentation for the
// API contract.
type InstallationProcess struct {
	Server *Server
	Script *remote.InstallationScript
}

// NewInstallationProcess returns a new installation process for a server.
func NewInstallationProcess(s *Server, script *remote.InstallationScript) (*InstallationProcess, error) {
	return &InstallationProcess{Server: s, Script: script}, nil
}

// IsInstalling returns if the server is actively running the installation
// process by checking the status of the installer lock.
func (s *Server) IsInstalling() bool {
	return s.installing.Load()
}

func (s *Server) SetInstalling(state bool) {
	s.installing.Store(state)
	if state {
		s.Sftp().CancelAll()
	}
}

func (s *Server) IsTransferring() bool {
	return s.transferring.Load()
}

func (s *Server) SetTransferring(state bool) {
	s.transferring.Store(state)
	if state {
		s.Sftp().CancelAll()
	}
}

func (s *Server) IsRestoring() bool {
	return s.restoring.Load()
}

func (s *Server) SetRestoring(state bool) {
	s.restoring.Store(state)
	if state {
		s.Sftp().CancelAll()
	}
}

func (s *Server) IsInProtectedState() bool {
	return s.IsInstalling() || s.IsTransferring() || s.IsRestoring()
}

// Run executes the installation process.
func (ip *InstallationProcess) Run() error {
	ip.Server.Log().Debug("acquiring installation process lock")
	if !ip.Server.installing.SwapIf(true) {
		return errors.New("install: cannot obtain installation lock")
	}
	ip.Server.Sftp().CancelAll()

	defer func() {
		ip.Server.Log().Debug("releasing installation process lock")
		ip.Server.installing.Store(false)
	}()

	if err := ip.BeforeExecute(); err != nil {
		return err
	}

	output, execErr := ip.Execute()

	// Write the log regardless of the outcome — a failed install is exactly when
	// the operator needs to read it.
	if err := ip.AfterExecute(output); err != nil {
		ip.Server.Log().WithField("error", err).Warn("failed to write installation log")
	}

	return execErr
}

// scriptPath is where the installation script is staged.
//
// Beside worker.json in the server's own directory rather than in a shared
// temporary directory, for two reasons. It is the first thing anyone wants to
// look at when an install fails, and a path under the server's UUID is where
// they will look. And it is removed with the server, rather than depending on a
// cleanup step that does not run when the daemon is killed mid-install.
//
// Deliberately NOT inside the server's data directory. The script is authored by
// an administrator and is what the installer executes; a server able to rewrite
// it before the next install would gain arbitrary code execution as its own
// account. The server root is daemon-owned, and the installer is granted read
// access to this one file.
func (ip *InstallationProcess) scriptPath() string {
	return filepath.Join(config.Get().System.ServerRoot(ip.Server.ID()), "install.ps1")
}

// writeScriptToDisk stages the installation script.
func (ip *InstallationProcess) writeScriptToDisk() error {
	if err := os.MkdirAll(filepath.Dir(ip.scriptPath()), 0o700); err != nil {
		return errors.WithMessage(err, "could not create the server directory for install process")
	}

	// PowerShell is content with either line ending, but normalising to CRLF
	// avoids surprises in here-strings within scripts authored on Windows.
	body := strings.ReplaceAll(ip.Script.Script, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")

	f, err := os.OpenFile(ip.scriptPath(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return errors.WithMessage(err, "failed to write server installation script to disk")
	}

	if _, err := io.Copy(f, strings.NewReader(body)); err != nil {
		_ = f.Close()
		return err
	}
	// Closed before the ACL is rewritten: SetNamedSecurityInfo on a file this
	// process still holds open works, but leaving it open past the point the
	// installer needs to read it invites a sharing violation.
	if err := f.Close(); err != nil {
		return err
	}

	// The server root is daemon-owned, so the account the installer runs as
	// cannot read anything in it. Open up this one file, read and execute only.
	username, _, err := accounts.For(ip.Server.ID())
	if err != nil {
		return errors.WithMessage(err, "could not resolve the server's install account")
	}
	if err := winacl.GrantReadFile(ip.scriptPath(), username); err != nil {
		return errors.WithMessage(err, "could not grant the install account access to the script")
	}
	return nil
}

// BeforeExecute prepares the installation environment.
func (ip *InstallationProcess) BeforeExecute() error {
	if err := ip.writeScriptToDisk(); err != nil {
		return errors.WithMessage(err, "failed to write installation script to disk")
	}
	// The server's data directory must exist; the script writes into it.
	if err := os.MkdirAll(ip.Server.Filesystem().Path(), 0o700); err != nil {
		return errors.WithMessage(err, "failed to create server data directory for install process")
	}
	// So must the scratch directory the environment points TEMP at. A script
	// that unpacks an archive is the first thing to notice its absence.
	if err := os.MkdirAll(config.Get().System.ServerTemp(ip.Server.ID()), 0o700); err != nil {
		return errors.WithMessage(err, "failed to create server temp directory for install process")
	}
	if err := os.MkdirAll(filepath.Dir(ip.GetLogPath()), 0o700); err != nil {
		return errors.WithMessage(err, "failed to create install log directory")
	}
	return nil
}

// GetLogPath returns the log path for the installation process.
func (ip *InstallationProcess) GetLogPath() string {
	return filepath.Join(config.Get().System.LogDirectory, "install", ip.Server.ID()+".log")
}

// AfterExecute writes the installation log.
func (ip *InstallationProcess) AfterExecute(output string) error {
	f, err := os.OpenFile(ip.GetLogPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	ip.Server.Log().WithField("path", ip.GetLogPath()).Debug("writing installation log to disk")

	tmpl, err := template.New("header").Parse(`win-wings Server Installation Log

|
| Details
| ------------------------------
  Server UUID:   {{.Server.ID}}
  Runtime:       {{.Script.ContainerImage}}
  Interpreter:   powershell.exe

|
| Environment Variables
| ------------------------------
{{ range $key, $value := .Server.GetEnvironmentVariables }}  {{ $value }}
{{ end }}

|
| Script Output
| ------------------------------
`)
	if err != nil {
		return err
	}
	if err := tmpl.Execute(f, ip); err != nil {
		return err
	}

	_, err = io.WriteString(f, output)
	return err
}

// installEnvironment returns the environment handed to the install script.
func (ip *InstallationProcess) installEnvironment() []string {
	env := ip.Server.GetEnvironmentVariables()

	// SERVER_DIR replaces the /mnt/server bind mount that Linux eggs write into.
	// It is also the script's working directory, so a script can use either.
	env = append(env, "SERVER_DIR="+ip.Server.Filesystem().Path())
	env = append(env, "INSTALL_RUNTIME="+ip.Script.ContainerImage)

	// Layer those over a working Windows environment. An install script is the
	// place this matters most: it downloads, unpacks and runs vendor tooling,
	// none of which survives a missing SystemRoot or TEMP.
	env = winenv.Merge(winenv.Base(winenv.Paths{
		Data: ip.Server.Filesystem().Path(),
		Temp: config.Get().System.ServerTemp(ip.Server.ID()),
	}), env)

	// Put the requested runtime ahead of the host PATH, and export RUNTIME_PATH,
	// so an install script can invoke the right java without hardcoding a path.
	return config.Get().Runtime.ApplyRuntime(ip.Script.ContainerImage, env)
}

// resourceLimits bounds the install process.
//
// Uses the higher of the node's configured installer limits and the server's own
// build limits, so a small server can still run an intensive install while a
// large one cannot consume several times its allocation during setup.
func (ip *InstallationProcess) resourceLimits() jobobject.Limits {
	limits := config.Get().Runtime.InstallerLimits

	c := *ip.Server.Config()
	cfg := c.Build
	if cfg.MemoryLimit < limits.Memory {
		cfg.MemoryLimit = limits.Memory
	}
	if limits.Cpu == 0 {
		cfg.CpuLimit = 0
	} else if cfg.CpuLimit != 0 && cfg.CpuLimit < limits.Cpu {
		cfg.CpuLimit = limits.Cpu
	}

	l := cfg.AsJobLimits()

	// No process cap during installation. These scripts are administrator
	// authored, frequently invoke build tooling that fans out widely, and a user
	// cannot execute arbitrary code here.
	return jobobject.Limits{
		MemoryBytes:  l.MemoryBytes,
		CpuRate:      l.CpuRate,
		CpuHardCap:   l.CpuHardCap,
		AffinityMask: l.AffinityMask,
		ProcessLimit: 0,
	}
}

// Execute runs the installation script and returns its combined output.
func (ip *InstallationProcess) Execute() (string, error) {
	ctx, cancel := context.WithCancel(ip.Server.Context())
	defer cancel()

	job, err := jobobject.Create()
	if err != nil {
		return "", errors.WrapIf(err, "install: failed to create job object")
	}
	defer job.Close()

	if err := job.SetLimits(ip.resourceLimits()); err != nil {
		return "", errors.WrapIf(err, "install: failed to apply installer resource limits")
	}

	powershell, err := powershellPath()
	if err != nil {
		return "", err
	}

	// -ExecutionPolicy Bypass is scoped to this process only and is required
	// because the script is generated rather than signed. -NonInteractive and
	// -NoProfile keep a script from stalling on a prompt or inheriting operator
	// profile state.
	argv := []string{
		powershell,
		"-NoProfile",
		"-NonInteractive",
		"-NoLogo",
		"-ExecutionPolicy", "Bypass",
		"-File", ip.scriptPath(),
	}

	username, password, err := accounts.For(ip.Server.ID())
	if err != nil {
		return "", errors.WrapIf(err, "install: could not obtain the server's install account")
	}

	console := config.Get().Runtime.Console
	cfg := winproc.Config{
		Argv:          argv,
		Dir:           ip.Server.Filesystem().Path(),
		Env:           ip.installEnvironment(),
		PseudoConsole: console.InstallPseudoConsole,
		Cols:          console.Columns,
		Rows:          console.Rows,
	}
	if username != "" {
		token, err := winproc.LogonUser(username, password)
		if err != nil {
			return "", errors.WrapIf(err, "install: failed to log on the server's install account")
		}
		defer token.Close()
		cfg.Token = token
	}

	ip.Server.Events().Publish(DaemonMessageEvent, "Running installation script...")

	proc, err := winproc.Start(cfg, job)
	if err != nil && cfg.PseudoConsole {
		// A ConPTY is a convenience, not a requirement. Rather than fail an
		// install on a host where one cannot be allocated -- an unusual console
		// host, a locked-down build -- fall back to pipes and say so, since the
		// symptom afterwards is output that only arrives at the end.
		ip.Server.Log().WithField("error", err).
			Warn("could not allocate a pseudo console for the install; falling back to " +
				"pipes, which means output will arrive in blocks rather than live")
		cfg.PseudoConsole = false
		proc, err = winproc.Start(cfg, job)
	}
	if err != nil {
		return "", errors.WrapIf(err, "install: failed to start installation script")
	}
	defer proc.Close()

	// Stream output to the install sink so administrators can watch it in the
	// Panel, while accumulating it for the log file.
	var sb strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink := ip.Server.Sink(system.InstallSink)
		_ = system.ScanReader(proc.Output(), func(line []byte) {
			sb.Write(line)
			sb.WriteByte('\n')
			sink.Push(line)
		})
	}()

	// Kill the installer if the server is deleted mid-install.
	go func() {
		<-ctx.Done()
		if ctx.Err() != nil {
			_ = job.Terminate(1)
		}
	}()

	code, waitErr := proc.Wait()
	<-done

	if waitErr != nil {
		return sb.String(), errors.WrapIf(waitErr, "install: failed waiting on installation script")
	}
	if code != 0 {
		// Rendered rather than printed raw: a loader failure reports a decimal
		// NTSTATUS that means nothing in that form, and points at the daemon's
		// environment rather than at anything the egg author wrote.
		explained := winproc.ExplainExitCode(code)
		ip.Server.Events().Publish(DaemonMessageEvent,
			fmt.Sprintf("Installation script exited with code %s.", explained))
		return sb.String(), errors.New(
			"install: installation script exited with a non-zero status: " + explained)
	}

	ip.Server.Events().Publish(DaemonMessageEvent, "Installation process completed.")
	return sb.String(), nil
}

// powershellPath locates a PowerShell interpreter.
//
// Windows PowerShell 5.1 is present on every supported Windows install and is
// used by default. PowerShell 7 is preferred when present, since egg authors are
// more likely to target it and it handles UTF-8 far better.
func powershellPath() (string, error) {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"),
		filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("install: could not locate a PowerShell interpreter")
}

// SyncInstallState makes an HTTP request to the Panel instance notifying it that
// the server has completed the installation process, and what the state of the
// server is.
func (s *Server) SyncInstallState(successful, reinstall bool) error {
	return s.client.SetInstallationStatus(s.Context(), s.ID(), remote.InstallStatusRequest{
		Successful: successful,
		Reinstall:  reinstall,
	})
}

var _ = log.Debug
