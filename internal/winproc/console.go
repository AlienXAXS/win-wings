//go:build windows

package winproc

import (
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// Console ownership.
//
// A worker is spawned detached and therefore has no console at all. That single
// fact is why GenerateConsoleCtrlEvent used to fail with "the handle is invalid"
// and why a server whose egg stops with a signal had no graceful path: there was
// no console for an interrupt to travel through.
//
// Allocating one fixes it, and it does not need a pseudo console. A server
// launched without CREATE_NEW_CONSOLE inherits the worker's console, and an
// interrupt raised on that console reaches it. Three conditions have to hold at
// once, and each was independently verified to be load-bearing:
//
//  1. The worker must own a console. AllocConsole, once, at startup.
//  2. The inherited "ignore Ctrl+C" attribute must be cleared BEFORE the server
//     is launched. SetConsoleCtrlHandler(NULL, TRUE) sets an attribute that
//     child processes inherit, so a server started under it cannot be
//     interrupted by anything. Whatever launched the worker may have set it.
//  3. The server must NOT be created with CREATE_NEW_PROCESS_GROUP. Its
//     documentation says plainly that Ctrl+C is disabled for every process in
//     the new group, so the flag and the mechanism are mutually exclusive.
//
// The cost of (3) is that CTRL_BREAK can no longer be aimed at one process
// group. It does not matter: a worker supervises exactly one server, so its
// console holds only itself and that server's tree, and addressing the whole
// console is addressing the right thing.
//
// The worker excludes itself by REGISTERING A HANDLER rather than by setting the
// ignore attribute. The distinction matters twice over. A handler is not
// inherited, so it needs no clearing around each spawn -- and an ignore
// attribute that has to be cleared around each spawn leaves a window in which an
// interrupt takes the worker down with the server. That is not theoretical: a
// CTRL_C_EVENT raised while ignoring is delivered later, the moment the
// attribute drops, which killed the test binary at the next unrelated spawn.

var (
	consoleOnce sync.Once
	consoleErr  error

	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procAllocConsole     = kernel32.NewProc("AllocConsole")
	procFreeConsole      = kernel32.NewProc("FreeConsole")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procSetConsoleCtrlFn = kernel32.NewProc("SetConsoleCtrlHandler")
)

// EnsureConsole gives this process a console if it does not already have one,
// and makes it deaf to the interrupts it raises on that console.
//
// Safe to call more than once, and safe in a process that already has a console:
// AllocConsole fails with ERROR_ACCESS_DENIED there, which is success as far as
// this is concerned.
func EnsureConsole() error {
	consoleOnce.Do(func() {
		if !hasConsole() {
			r, _, err := procAllocConsole.Call()
			if r == 0 && !hasConsole() {
				consoleErr = fmt.Errorf("winproc: allocate a console: %w", err)
				return
			}
		}
		protectFromCtrlEvents()
	})
	return consoleErr
}

// protectFromCtrlEvents makes this process survive the console events it raises,
// without making its children survive them too.
func protectFromCtrlEvents() {
	// Clear any inherited ignore attribute first. It is inherited from whatever
	// started this process and it would be inherited onward by every server,
	// which could then not be interrupted by anything at all.
	_, _, _ = procSetConsoleCtrlFn.Call(0, 0)

	handler := windows.NewCallback(func(event uint32) uintptr {
		switch event {
		case windows.CTRL_C_EVENT, windows.CTRL_BREAK_EVENT:
			// Handled: this is an interrupt aimed at the server, and the worker
			// must outlive it to report the exit and clean up the job object.
			return 1
		}
		// Close, logoff and shutdown are left to the default handler.
		return 0
	})
	_, _, _ = procSetConsoleCtrlFn.Call(handler, 1)
}

// UseOwnConsole detaches from any inherited console and allocates a private one,
// returning whether it succeeded.
//
// The worker does not need this -- it is spawned detached and has no console to
// inherit -- but anything that raises an interrupt while attached to a console it
// shares will take down whatever else is on it, which for a test runner is the
// shell that started it. Exported so tests can put themselves in the worker's
// position rather than signalling into their parent's console.
func UseOwnConsole() bool {
	_, _, _ = procFreeConsole.Call()
	r, _, _ := procAllocConsole.Call()
	if r == 0 && !hasConsole() {
		return false
	}
	protectFromCtrlEvents()
	return true
}

// hasConsole reports whether this process is attached to one.
func hasConsole() bool {
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}
