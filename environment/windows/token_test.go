//go:build windows

package windows

import (
	"testing"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/worker"
)

// A daemon restart must not lock the daemon out of workers that are still
// running.
//
// The worker reads its token from worker.json once, at startup, and holds it for
// its lifetime -- which by design outlives the daemon's. A token minted per
// daemon run therefore failed the handshake against every surviving worker, and
// the daemon concluded there was no worker there at all.
func TestWorkerTokenIsStableAcrossDaemonRuns(t *testing.T) {
	const uuid = "token-stability-test"

	cfg := config.Configuration{AuthenticationToken: "test-token"}
	cfg.System.Data = t.TempDir()
	config.Set(&cfg)

	first, err := workerToken(uuid)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if first == "" {
		t.Fatal("first token is empty")
	}

	// Nothing is on disk yet, so a second call before the worker configuration is
	// written is free to differ. What matters is what happens afterwards.
	if err := worker.WriteConfig(cfg.System.ServerRoot(uuid), worker.Config{
		UUID:       uuid,
		Token:      first,
		WorkingDir: cfg.System.ServerData(uuid),
	}); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	second, err := workerToken(uuid)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if second != first {
		t.Fatalf("token changed across daemon runs: %q then %q", first, second)
	}
}

// A server that has never been created gets a fresh token rather than an error
// or an empty one.
func TestWorkerTokenIsGeneratedWhenThereIsNoConfig(t *testing.T) {
	cfg := config.Configuration{AuthenticationToken: "test-token"}
	cfg.System.Data = t.TempDir()
	config.Set(&cfg)

	a, err := workerToken("brand-new-server")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	b, err := workerToken("another-brand-new-server")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("generated an empty token")
	}
	if a == b {
		t.Fatal("two servers with no configuration on disk got the same token")
	}
}
