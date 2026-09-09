//go:build windows

package winproc

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Console input injection.
//
// Some servers do not read their commands from stdin at all. A process that
// opens a console of its own -- a modloader started with -console is the case
// this was written for -- typically reads with ReadConsole or ReadConsoleInput,
// which draw from the console's INPUT BUFFER, the queue a keyboard feeds. A
// redirected stdin pipe is a different thing entirely and nothing written to it
// will ever arrive.
//
// Because the server now shares the worker's console, that buffer is reachable:
// synthesising key events into it is indistinguishable, from the server's side,
// from somebody typing. This is the same mechanism that makes the -console flag
// usable by hand.
//
// It is not a replacement for stdin. A server that does read stdin will not see
// any of this, so the two are tried in turn rather than one being chosen.

const keyEvent = 0x0001

// inputRecord is INPUT_RECORD specialised to KEY_EVENT_RECORD. The layout is
// load-bearing: a two-byte event type, two bytes of padding before the union,
// then the key event's fields. Twenty bytes on amd64.
type inputRecord struct {
	eventType uint16
	_         uint16
	keyDown   int32
	repeat    uint16
	vk        uint16
	scan      uint16
	char      uint16
	ctrlState uint32
}

var procWriteConsoleInput = kernel32.NewProc("WriteConsoleInputW")

// WriteConsoleLine types a line into this process's console input buffer,
// followed by Return.
//
// Every character is sent as a key down and a key up, because a process reading
// with ReadConsoleInput sees both and one without the other looks like a stuck
// key. Only the unicode character is set: the virtual key and scan codes are
// what a keyboard driver fills in, and nothing that reads text needs them.
func WriteConsoleLine(text string) error {
	if !hasConsole() {
		return fmt.Errorf("winproc: console input: this process has no console")
	}

	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return fmt.Errorf("winproc: console input: %w", err)
	}
	// CONIN$ names the console's input buffer regardless of what stdin is, which
	// matters here: the worker's own standard handles are not the console.
	h, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("winproc: open console input: %w", err)
	}
	defer windows.CloseHandle(h)

	runes := append([]rune(text), '\r')
	records := make([]inputRecord, 0, len(runes)*2)
	for _, r := range runes {
		// Anything outside the BMP cannot be expressed as a single UTF-16 code
		// unit in a key event. Dropping it is better than writing half of a
		// surrogate pair into the buffer.
		if r > 0xFFFF {
			continue
		}
		var vk uint16
		if r == '\r' {
			vk = 0x0D // VK_RETURN, which some readers check rather than the char
		}
		down := inputRecord{eventType: keyEvent, keyDown: 1, repeat: 1, vk: vk, char: uint16(r)}
		up := down
		up.keyDown = 0
		records = append(records, down, up)
	}
	if len(records) == 0 {
		return nil
	}

	var written uint32
	r1, _, err := procWriteConsoleInput.Call(uintptr(h),
		uintptr(unsafe.Pointer(&records[0])), uintptr(len(records)),
		uintptr(unsafe.Pointer(&written)))
	if r1 == 0 {
		return fmt.Errorf("winproc: write console input: %w", err)
	}
	if int(written) != len(records) {
		return fmt.Errorf("winproc: write console input: wrote %d of %d events", written, len(records))
	}
	return nil
}

// TypeLine delivers a line to the process the way a keyboard would.
//
// With a pseudo console the process's stdin IS the console input, so the text
// goes there. Otherwise the process is sharing the worker's console and the
// line is synthesised into that console's input buffer.
func (p *Process) TypeLine(text string) error {
	if p.console() != 0 {
		if p.stdin == nil {
			return fmt.Errorf("winproc: console input: the process has no console input")
		}
		if _, err := p.stdin.Write([]byte(text + "\r\n")); err != nil {
			return fmt.Errorf("winproc: console input: %w", err)
		}
		return nil
	}
	return WriteConsoleLine(text)
}
