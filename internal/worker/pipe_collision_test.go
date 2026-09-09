//go:build windows

package worker

import (
	"errors"
	"testing"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/internal/wire"
)

// A second worker for a server that already has one cannot take the pipe, and
// Windows reports that as access denied rather than as a collision.
//
// This is the whole reason connectOrSpawn checks PipeExists before spawning: the
// duplicate dies with an error that reads like a permissions problem, while the
// original carries on, so the daemon sees a healthy pipe and no evidence that
// anything went wrong.
func TestSecondListenOnTheSamePipeIsDeniedNotCollided(t *testing.T) {
	const uuid = "pipe-collision-test"

	l, err := winio.ListenPipe(wire.PipeName(uuid), &winio.PipeConfig{})
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer l.Close()

	_, err = winio.ListenPipe(wire.PipeName(uuid), &winio.PipeConfig{})
	if err == nil {
		t.Fatal("second listen on the same pipe name succeeded; expected it to fail")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("second listen failed with %v, expected ERROR_ACCESS_DENIED", err)
	}
}

func TestPipeExists(t *testing.T) {
	const uuid = "pipe-exists-test"

	if PipeExists(uuid) {
		t.Fatal("reported a pipe that was never created")
	}

	l, err := winio.ListenPipe(wire.PipeName(uuid), &winio.PipeConfig{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if !PipeExists(uuid) {
		t.Fatal("did not report a pipe that is listening")
	}

	_ = l.Close()
	if PipeExists(uuid) {
		t.Fatal("still reported the pipe after the listener closed")
	}
}
