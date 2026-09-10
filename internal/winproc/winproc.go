//go:build windows

// Package winproc launches and controls server processes.
//
// It exists because os/exec cannot do what is needed here: syscall.SysProcAttr
// exposes no way to attach a proc-thread attribute list, which is required to
// hand a process a pseudo console, and no way to start suspended and place the
// process in a Job Object before its first instruction runs.
//
// That last point is a correctness requirement rather than a nicety. A process
// assigned to a job only after it starts may already have spawned children, and
// those children are outside the job — beyond its resource limits and beyond the
// reach of TerminateJobObject. Every process here is created suspended, assigned,
// and only then resumed.
package winproc

import (
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/internal/jobobject"
	"github.com/pterodactyl/wings/internal/winsta"
)

// procThreadAttributePseudoConsole is PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
// which golang.org/x/sys/windows does not export.
const procThreadAttributePseudoConsole = 0x00020016

// Config describes a process to launch.
type Config struct {
	// Argv is the command and its arguments, already split. Argv[0] is the
	// executable.
	//
	// Splitting is the caller's responsibility precisely so that no shell is
	// involved: see ParseCommandLine.
	Argv []string

	// Dir is the working directory. Required.
	Dir string

	// Env is the environment, as "KEY=VALUE" strings.
	Env []string

	// Token, when non-zero, runs the process as that account. Zero runs it as
	// the account the worker itself runs as.
	Token windows.Token

	// Desktop names the window station and desktop to launch on, as
	// "station\desktop". Empty means Start works it out, which is what every
	// caller wants: see winsta.
	Desktop string

	// PseudoConsole allocates a ConPTY rather than plain pipes.
	//
	// Needed by processes that check whether stdout is a character device and
	// change behaviour when it is not — steamcmd being the common example, which
	// mangles or drops progress output when handed a pipe. The cost is that
	// output arrives as a VT stream with escape sequences and cursor movement
	// rather than clean lines, so enable it per-egg rather than globally.
	PseudoConsole bool

	// Cols and Rows size the pseudo console. Ignored without PseudoConsole.
	Cols, Rows uint16
}

// Process is a running server process.
type Process struct {
	Pid int

	proc   windows.Handle
	thread windows.Handle

	// ptyMu guards pty, which Wait and Close both release.
	ptyMu sync.Mutex
	pty   windows.Handle

	stdin  *os.File
	output *os.File

	// ptyClosers holds the handles that must outlive process creation and be
	// released afterwards.
	ptyClosers []windows.Handle
}

// ParseCommandLine splits a command line string into argv using the same rules
// as CommandLineToArgvW.
//
// This is how an egg's startup string becomes an argv. Upstream wings never had
// to do this: it set STARTUP as an environment variable and the Docker image's
// entrypoint ran it through a shell. There is no entrypoint here, and routing a
// partly user-controlled string through cmd.exe would execute it with the
// server account's full privileges on the host.
//
// A consequence worth knowing: shell operators (&&, |, >, subshells) are not
// interpreted. A startup line that relies on them will not work and needs
// rewriting in the egg's Windows profile.
func ParseCommandLine(cmdline string) ([]string, error) {
	argv, err := windows.DecomposeCommandLine(cmdline)
	if err != nil {
		return nil, fmt.Errorf("winproc: parse command line: %w", err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("winproc: command line is empty")
	}
	return argv, nil
}

// Start launches the process and places it in job before it runs.
func Start(cfg Config, job *jobobject.Job) (_ *Process, err error) {
	if len(cfg.Argv) == 0 {
		return nil, fmt.Errorf("winproc: no command specified")
	}
	if cfg.Dir == "" {
		return nil, fmt.Errorf("winproc: no working directory specified")
	}

	// proc is a local, not the returned value. Every failure path below returns
	// nil for the process, and when this was a named return that nil was assigned
	// before the deferred cleanup ran -- so the cleanup dereferenced a nil pointer
	// and panicked, replacing the actual failure with a crash in the worker. The
	// first result is blank so that it cannot be assigned again by accident.
	proc := &Process{}
	defer func() {
		if err != nil {
			proc.closeAll()
		}
	}()

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))

	// A process launched under a different account has to be given access to a
	// window station and desktop, and told which. Without it user32 fails to
	// connect during process startup and the process dies in the loader with
	// STATUS_DLL_INIT_FAILED, having run none of its own code -- which is not a
	// diagnosable error message, it is an exit code.
	//
	// Best effort: a host where this cannot be done is a host where the launch
	// was going to fail anyway, and reporting the underlying failure is more use
	// than replacing it with this one. The warning says what happened.
	desktop := cfg.Desktop
	if desktop == "" && cfg.Token != 0 {
		if d, gerr := winsta.GrantToToken(cfg.Token); gerr != nil {
			Warn(fmt.Sprintf("could not grant %q access to a desktop; if it fails to start "+
				"with 0xC0000142 this is why: %v", cfg.Argv[0], gerr))
		} else {
			desktop = d
		}
	}
	if desktop != "" {
		var d *uint16
		if d, err = windows.UTF16PtrFromString(desktop); err != nil {
			return nil, fmt.Errorf("winproc: desktop %q: %w", desktop, err)
		}
		si.Desktop = d
	}

	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)

	var attrList *windows.ProcThreadAttributeListContainer

	if cfg.PseudoConsole {
		if err = proc.setupPseudoConsole(cfg, &si); err != nil {
			return nil, err
		}
		attrList, err = windows.NewProcThreadAttributeList(1)
		if err != nil {
			return nil, fmt.Errorf("winproc: attribute list: %w", err)
		}
		defer attrList.Delete()

		if err = attrList.Update(
			procThreadAttributePseudoConsole,
			hpconValue(proc.pty),
			unsafe.Sizeof(proc.pty),
		); err != nil {
			return nil, fmt.Errorf("winproc: attach pseudo console: %w", err)
		}
		si.ProcThreadAttributeList = attrList.List()
		flags |= windows.EXTENDED_STARTUPINFO_PRESENT
	} else {
		if err = proc.setupPipes(&si); err != nil {
			return nil, err
		}
		// Deliberately NOT CREATE_NEW_PROCESS_GROUP. It would let CTRL_BREAK be
		// aimed at this process specifically, but its documentation is explicit
		// that Ctrl+C is disabled for every process in the new group -- and a
		// real interrupt is worth more than a targeted break. Addressing the
		// whole console is the right thing anyway: see console.go.
		//
	}

	cmdline := windows.ComposeCommandLine(cfg.Argv)

	// Resolved rather than passed through: a relative lpApplicationName is
	// resolved against this process's current directory, not the child's. See
	// ResolveExecutable. The command line keeps argv[0] as the egg wrote it, so
	// a process that inspects its own arguments sees what it was configured with.
	exePath, err := ResolveExecutable(cfg.Argv[0], cfg.Dir, cfg.Env)
	if err != nil {
		return nil, err
	}
	argv0, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return nil, fmt.Errorf("winproc: executable path: %w", err)
	}
	cmdlinePtr, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, fmt.Errorf("winproc: command line: %w", err)
	}
	dirPtr, err := windows.UTF16PtrFromString(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("winproc: working directory: %w", err)
	}
	envBlock, err := buildEnvBlock(cfg.Env)
	if err != nil {
		return nil, err
	}

	var pi windows.ProcessInformation

	// A pseudo console supplies the child's handles through the attribute list,
	// not through inheritance. Passing bInheritHandles=TRUE alongside it causes
	// the console to never attach and the child to produce no output at all.
	inheritHandles := !cfg.PseudoConsole

	if cfg.Token != 0 {
		err = windows.CreateProcessAsUser(
			cfg.Token, argv0, cmdlinePtr,
			nil, nil, inheritHandles, flags,
			envBlock, dirPtr, &si.StartupInfo, &pi,
		)
	} else {
		err = windows.CreateProcess(
			argv0, cmdlinePtr,
			nil, nil, inheritHandles, flags,
			envBlock, dirPtr, &si.StartupInfo, &pi,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("winproc: create process %s: %w", exePath, err)
	}

	proc.Pid = int(pi.ProcessId)
	proc.proc = pi.Process
	proc.thread = pi.Thread

	// Release the ends of the pipes now owned by the child, so that reads see
	// EOF when the child exits rather than hanging on our own dangling handle.
	proc.releaseChildHandles()

	// Assign before resuming: this is the window in which a process could
	// otherwise spawn children outside the job.
	if job != nil {
		if err = job.Assign(proc.proc); err != nil {
			_ = windows.TerminateProcess(proc.proc, 1)
			return nil, err
		}
	}

	if _, err = windows.ResumeThread(proc.thread); err != nil {
		_ = windows.TerminateProcess(proc.proc, 1)
		return nil, fmt.Errorf("winproc: resume: %w", err)
	}

	return proc, nil
}

// hpconValue prepares an HPCON to be handed to UpdateProcThreadAttribute.
//
// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE is the odd one out among the proc-thread
// attributes: lpValue is the HPCON *itself*, not the address of a variable
// holding one. The ones alongside it — PARENT_PROCESS, HANDLE_LIST,
// MITIGATION_POLICY — all take a pointer to their value, and x/sys/windows types
// the parameter as unsafe.Pointer, so the wrong call is the one that reads
// naturally. Microsoft's EchoCon sample is where the difference is visible: a
// single line that passes hPC rather than &hPC.
//
// Getting this wrong does not fail: UpdateProcThreadAttribute and CreateProcess
// both succeed, a console host is spawned, and the child is then handed a
// PseudoConsole struct read from the wrong address. It dies in the loader with
// STATUS_DLL_INIT_FAILED (0xC0000142) having run none of its own code, which is
// an exit code and not an error message. That cost a great deal of time here;
// see the note at the top of conpty_diag_test.go.
//
// The double indirection launders the uintptr past both `go vet`'s unsafeptr
// check and checkptr under -race. Neither has anything to complain about — the
// value is a kernel32 heap pointer that the Go collector knows nothing about and
// must not try to track — but a direct unsafe.Pointer(uintptr) conversion is
// indistinguishable to them from the mistake they exist to catch.
func hpconValue(pty windows.Handle) unsafe.Pointer {
	p := uintptr(pty)
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

// setupPseudoConsole creates a ConPTY and the pipes feeding it.
func (p *Process) setupPseudoConsole(cfg Config, si *windows.StartupInfoEx) error {
	var inRead, inWrite, outRead, outWrite windows.Handle

	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return fmt.Errorf("winproc: pty input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return fmt.Errorf("winproc: pty output pipe: %w", err)
	}

	cols, rows := cfg.Cols, cfg.Rows
	if cols == 0 {
		cols = 200
	}
	if rows == 0 {
		rows = 50
	}

	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &p.pty); err != nil {
		for _, h := range []windows.Handle{inRead, inWrite, outRead, outWrite} {
			_ = windows.CloseHandle(h)
		}
		return fmt.Errorf("winproc: create pseudo console: %w", err)
	}

	// The pseudo console duplicated these; our copies must go or the child will
	// never see EOF.
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)

	p.stdin = os.NewFile(uintptr(inWrite), "conpty-stdin")
	p.output = os.NewFile(uintptr(outRead), "conpty-output")

	// Hand the child no standard handles at all, so that it takes them from the
	// console it is being attached to.
	//
	// Leaving STARTF_USESTDHANDLES clear looks right -- the pseudo console is
	// supposed to supply the handles -- and is what Microsoft's EchoCon sample
	// does. It only works there because EchoCon is a console program whose own
	// standard handles already belong to a console, and console handles are
	// remapped onto whichever console the child ends up attached to. The worker's
	// standard output is a pipe, and a pipe handle is copied down literally: the
	// child inherited the worker's stdout, wrote everything into the daemon's own
	// log, and the pseudo console carried nothing but its startup escape
	// sequences. That is the "a conhost is spawned but never services the
	// console" symptom recorded in conpty_diag_test.go.
	//
	// Setting the flag with three NULL handles is how to say "inherit nothing":
	// given no handles and a console, the child opens its standard handles onto
	// that console.
	si.Flags |= windows.STARTF_USESTDHANDLES
	si.StdInput = 0
	si.StdOutput = 0
	si.StdErr = 0
	return nil
}

// setupPipes wires plain anonymous pipes for stdin and a merged stdout/stderr.
//
// Output is merged deliberately. Game servers interleave the two and the Panel
// presents a single console, so keeping them separate would only create an
// ordering problem to solve later.
func (p *Process) setupPipes(si *windows.StartupInfoEx) error {
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))

	var inRead, inWrite, outRead, outWrite windows.Handle

	if err := windows.CreatePipe(&inRead, &inWrite, sa, 0); err != nil {
		return fmt.Errorf("winproc: stdin pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, sa, 0); err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return fmt.Errorf("winproc: stdout pipe: %w", err)
	}

	// Our ends must not be inherited, or the child holds a copy and the pipe
	// never reports EOF.
	for _, h := range []windows.Handle{inWrite, outRead} {
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			return fmt.Errorf("winproc: clear inherit flag: %w", err)
		}
	}

	si.Flags |= windows.STARTF_USESTDHANDLES
	si.StdInput = inRead
	si.StdOutput = outWrite
	si.StdErr = outWrite

	p.stdin = os.NewFile(uintptr(inWrite), "stdin")
	p.output = os.NewFile(uintptr(outRead), "output")
	p.ptyClosers = append(p.ptyClosers, inRead, outWrite)

	return nil
}

// releaseChildHandles closes the pipe ends the child now owns.
func (p *Process) releaseChildHandles() {
	if p == nil {
		return
	}
	for _, h := range p.ptyClosers {
		_ = windows.CloseHandle(h)
	}
	p.ptyClosers = nil
}

// Stdin returns the writer feeding the process's standard input.
func (p *Process) Stdin() io.WriteCloser { return p.stdin }

// Output returns the reader carrying the process's combined output.
//
// In pseudo console mode this is a VT stream: it carries escape sequences,
// cursor positioning and carriage-return redraws, not clean lines.
func (p *Process) Output() io.ReadCloser { return p.output }

// Wait blocks until the process exits and returns its exit code.
//
// Releasing the pseudo console is part of waiting, not part of closing. A plain
// pipe reports EOF when the child exits because the only remaining write handle
// went with it; a pseudo console does not, because the console itself holds that
// handle and keeps it until ClosePseudoConsole. A caller that waits for the
// process and then drains its output to EOF -- which is what both the installer
// and the worker do -- would otherwise block forever, with the deferred Close
// that would have released it sitting behind the drain it is waiting on.
//
// Closed here rather than in Close for that reason, and while a reader is still
// attached: ClosePseudoConsole flushes what the console has buffered before it
// closes the pipe, so the output arrives and then EOF does.
func (p *Process) Wait() (uint32, error) {
	defer p.closeConsole()

	if _, err := windows.WaitForSingleObject(p.proc, windows.INFINITE); err != nil {
		return 0, fmt.Errorf("winproc: wait: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
		return 0, fmt.Errorf("winproc: exit code: %w", err)
	}
	return code, nil
}

// console returns the pseudo console handle, or zero once it has been released.
func (p *Process) console() windows.Handle {
	p.ptyMu.Lock()
	defer p.ptyMu.Unlock()
	return p.pty
}

// closeConsole releases the pseudo console, if there is one, exactly once.
//
// Wait and Close can race -- a caller terminating a server calls Close while
// another goroutine sits in Wait -- and ClosePseudoConsole must not be handed a
// handle that has already gone.
func (p *Process) closeConsole() {
	p.ptyMu.Lock()
	pty := p.pty
	p.pty = 0
	p.ptyMu.Unlock()

	if pty != 0 {
		windows.ClosePseudoConsole(pty)
	}
}

// CtrlBreak sends CTRL_BREAK_EVENT to the process group.
//
// This is the nearest thing Windows has to SIGTERM and it is not a close match:
// many servers install no handler for it and die uncleanly, which is no better
// than a kill. Prefer CtrlC, which more servers actually handle.
//
// Addressed to the whole console rather than to this process's group, because
// the server is deliberately not given a group of its own -- that flag disables
// Ctrl+C. The worker's console holds only the worker and this server, and the
// worker ignores the event.
func (p *Process) CtrlBreak() error {
	if p.console() != 0 {
		return fmt.Errorf("winproc: ctrl-break unavailable in pseudo console mode")
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0); err != nil {
		return fmt.Errorf("winproc: ctrl-break: %w", err)
	}
	return nil
}

// CtrlC delivers a Ctrl+C to the process.
//
// There is no signal to send. This is done the way a keyboard does it: the byte
// 0x03 is written to the console input, and the console driver raises a
// CTRL_C_EVENT in every process attached to that console. GenerateConsoleCtrlEvent
// is no help here -- it cannot target CTRL_C_EVENT at a process group -- so the
// pseudo console's input side is the only route to a single server.
//
// A server given plain pipes shares the worker's console instead, and the event
// is raised on that console directly. Either way an interrupt is deliverable;
// what is required is that a console exists at all, which is what
// EnsureConsole guarantees.
func (p *Process) CtrlC() error {
	// With a pseudo console the interrupt goes in the way a keyboard sends it:
	// the byte 0x03 written to the console input, which the console driver
	// turns into a CTRL_C_EVENT.
	if p.console() != 0 {
		if p.stdin == nil {
			return fmt.Errorf("winproc: ctrl-c: the process has no console input")
		}
		if _, err := p.stdin.Write([]byte{0x03}); err != nil {
			return fmt.Errorf("winproc: ctrl-c: %w", err)
		}
		return nil
	}

	// Otherwise the server is sharing the worker's own console, and the event
	// can simply be raised on it. Group 0 means every process attached to this
	// console -- the worker included, which is why it holds the ignore
	// attribute. See console.go for the conditions this depends on.
	if !hasConsole() {
		return fmt.Errorf("winproc: ctrl-c: the worker has no console to raise an interrupt on")
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
		return fmt.Errorf("winproc: ctrl-c: %w", err)
	}
	return nil
}

// Resize changes the pseudo console dimensions. No-op without one.
func (p *Process) Resize(cols, rows uint16) error {
	pty := p.console()
	if pty == 0 {
		return nil
	}
	if err := windows.ResizePseudoConsole(pty, windows.Coord{X: int16(cols), Y: int16(rows)}); err != nil {
		return fmt.Errorf("winproc: resize pseudo console: %w", err)
	}
	return nil
}

// Kill terminates just this process. Prefer terminating the Job Object, which
// takes down the whole tree.
func (p *Process) Kill() error {
	if p.proc == 0 {
		return nil
	}
	return windows.TerminateProcess(p.proc, 1)
}

// Close releases every handle held for the process.
func (p *Process) Close() error {
	p.closeAll()
	return nil
}

func (p *Process) closeAll() {
	// Nil-tolerant on purpose. This runs from deferred cleanup on failure paths,
	// where it is the last thing standing between a start error and a panic that
	// takes the worker -- and the whole server -- down with it.
	if p == nil {
		return
	}
	p.releaseChildHandles()

	if p.stdin != nil {
		_ = p.stdin.Close()
		p.stdin = nil
	}
	if p.output != nil {
		_ = p.output.Close()
		p.output = nil
	}
	// Ordinarily already released by Wait. This covers the process that is
	// closed without ever being waited on -- a failed start, mainly. After the
	// pipes, because with no reader left ClosePseudoConsole would otherwise
	// block trying to flush into one.
	p.closeConsole()
	if p.thread != 0 {
		_ = windows.CloseHandle(p.thread)
		p.thread = 0
	}
	if p.proc != 0 {
		_ = windows.CloseHandle(p.proc)
		p.proc = 0
	}
}

// buildEnvBlock converts "KEY=VALUE" strings into the doubly-NUL-terminated
// UTF-16 block CreateProcess expects.
func buildEnvBlock(env []string) (*uint16, error) {
	if len(env) == 0 {
		return nil, nil
	}

	var block []uint16
	for _, e := range env {
		u, err := windows.UTF16FromString(e)
		if err != nil {
			return nil, fmt.Errorf("winproc: environment entry %q: %w", e, err)
		}
		block = append(block, u...)
	}
	block = append(block, 0)
	return &block[0], nil
}
