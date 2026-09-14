//go:build windows

package statsagent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pterodactyl/wings/config"
)

func newTestAgent(t *testing.T, token string) *Agent {
	t.Helper()
	c := config.Configuration{}
	c.StatsAgent.Enabled = true
	c.StatsAgent.Host = "127.0.0.1"
	c.StatsAgent.Token = token
	if _, err := c.ResolveStatsAgentToken(); err != nil {
		t.Fatal(err)
	}
	a, err := New(c.StatsAgent, NewCollector(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func do(t *testing.T, h http.Handler, method, path, auth string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s: non-JSON body %q", method, path, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s %s: content-type %q", method, path, ct)
	}
	return rec.Code, body
}

func TestPingNeedsNoAuth(t *testing.T) {
	h := newTestAgent(t, "secret").Handler()
	code, body := do(t, h, http.MethodGet, "/ping", "")
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestMetricsRequiresExactBearer(t *testing.T) {
	h := newTestAgent(t, "secret").Handler()

	for _, auth := range []string{"", "Bearer", "Bearer wrong", "Bearer secre", "Bearer secrets", "Basic secret", "secret"} {
		code, body := do(t, h, http.MethodGet, "/metrics", auth)
		if code != http.StatusUnauthorized || body["error"] != "unauthorized" {
			t.Fatalf("auth %q: got %d %v", auth, code, body)
		}
	}

	code, body := do(t, h, http.MethodGet, "/metrics", "Bearer secret")
	if code != http.StatusOK {
		t.Fatalf("got %d %v", code, body)
	}
	if body["version"] != float64(2) {
		t.Fatalf("version %v", body["version"])
	}
	for _, k := range []string{"cpu", "memory", "disk", "collected_at"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("missing %q in %v", k, body)
		}
	}
	// The scheme is case-insensitive per RFC 7235.
	if code, _ := do(t, h, http.MethodGet, "/metrics", "bearer secret"); code != http.StatusOK {
		t.Fatalf("lowercase scheme rejected: %d", code)
	}
}

func TestTokenFromFile(t *testing.T) {
	p := t.TempDir() + `\token`
	if err := writeFile(p, "from-file\r\n"); err != nil {
		t.Fatal(err)
	}
	h := newTestAgent(t, "file://"+p).Handler()
	if code, _ := do(t, h, http.MethodGet, "/metrics", "Bearer from-file"); code != http.StatusOK {
		t.Fatalf("file token rejected: %d", code)
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	h := newTestAgent(t, "secret").Handler()
	if code, body := do(t, h, http.MethodGet, "/nope", "Bearer secret"); code != http.StatusNotFound || body["error"] != "not found" {
		t.Fatalf("got %d %v", code, body)
	}
	for _, p := range []string{"/ping", "/metrics", "/"} {
		if code, _ := do(t, h, http.MethodPost, p, "Bearer secret"); code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s: got %d", p, code)
		}
	}
}

func TestNewRefusesUnresolvedToken(t *testing.T) {
	var c config.StatsAgentConfiguration
	if _, err := New(c, NewCollector("")); err == nil {
		t.Fatal("expected an error with no resolved token")
	}
}

func TestGeneratedTokenIsPersistedInStruct(t *testing.T) {
	c := config.Configuration{}
	c.StatsAgent.Enabled = true
	gen, err := c.ResolveStatsAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	if !gen || len(c.StatsAgent.Token) != 64 || c.StatsAgent.ResolvedToken() != c.StatsAgent.Token {
		t.Fatalf("generated=%v token=%q", gen, c.StatsAgent.Token)
	}
	// Disabled: nothing generated, nothing resolved.
	d := config.Configuration{}
	if gen, err := d.ResolveStatsAgentToken(); err != nil || gen || d.StatsAgent.ResolvedToken() != "" {
		t.Fatalf("disabled agent resolved something: gen=%v err=%v", gen, err)
	}
}

// Run binds a real socket and shuts down on cancel; this is what the daemon
// drives, so it is exercised end to end once.
func TestRunServesAndStops(t *testing.T) {
	// Find a free port, then hand it to the agent.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	a := newTestAgent(t, "secret")
	a.cfg.Port = port

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	url := "http://127.0.0.1:" + strconv.Itoa(port)
	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = http.Get(url + "/ping")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("agent never came up: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "{\"ok\":true}\n" {
		t.Fatalf("ping: %d %q", resp.StatusCode, b)
	}

	req, _ := http.NewRequest(http.MethodGet, url+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}

	// A second agent on the same port must fail at Run, not silently.
	ln2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	if err := newTestAgentOnPort(t, port).Run(context.Background()); err == nil {
		t.Fatal("expected a bind error on an occupied port")
	}
}

func newTestAgentOnPort(t *testing.T, port int) *Agent {
	a := newTestAgent(t, "secret")
	a.cfg.Port = port
	return a
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }
