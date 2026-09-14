package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStatsAgentDefaults(t *testing.T) {
	c, err := NewAtPath("x")
	if err != nil {
		t.Fatal(err)
	}
	sa := c.StatsAgent
	if sa.Enabled || sa.Host != "0.0.0.0" || sa.Port != 8081 || sa.Token != "" || !sa.OpenFirewall {
		t.Fatalf("unexpected defaults: %+v", sa)
	}
	if sa.Address() != "0.0.0.0:8081" {
		t.Fatalf("address %q", sa.Address())
	}
}

func TestStatsAgentRoundTripsThroughYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yml")
	src := "token_id: abc\ntoken: def\nstats_agent:\n  enabled: true\n  port: 9090\n  token: sekrit\n"
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := NewAtPath(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(src), c); err != nil {
		t.Fatal(err)
	}
	if !c.StatsAgent.Enabled || c.StatsAgent.Port != 9090 || c.StatsAgent.Token != "sekrit" || c.StatsAgent.Host != "0.0.0.0" {
		t.Fatalf("yaml not applied over defaults: %+v", c.StatsAgent)
	}

	gen, err := c.ResolveStatsAgentToken()
	if err != nil || gen {
		t.Fatalf("gen=%v err=%v", gen, err)
	}
	if c.StatsAgent.ResolvedToken() != "sekrit" {
		t.Fatalf("resolved %q", c.StatsAgent.ResolvedToken())
	}

	// Written back, the block and its token survive; the resolved copy does not
	// appear as a second key.
	if err := WriteToDisk(c); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	if !strings.Contains(s, "stats_agent:") || !strings.Contains(s, "token: sekrit") || !strings.Contains(s, "port: 9090") {
		t.Fatalf("block missing from written config:\n%s", s)
	}
	if strings.Contains(s, "resolved") {
		t.Fatalf("private field leaked into config:\n%s", s)
	}
}

// The Panel pushes configuration as JSON into a copy of the live struct. The
// stats agent block must be invisible to that, or a Panel that knows nothing of
// it would clear it on every update.
func TestStatsAgentIsHiddenFromJSON(t *testing.T) {
	c, _ := NewAtPath("x")
	c.StatsAgent.Enabled = true
	c.StatsAgent.Token = "sekrit"
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "stats_agent") || strings.Contains(string(b), "sekrit") {
		t.Fatalf("stats agent leaked into JSON: %s", b)
	}

	// And a JSON update that mentions it anyway changes nothing.
	if err := json.Unmarshal([]byte(`{"stats_agent":{"enabled":false,"token":"evil"}}`), c); err != nil {
		t.Fatal(err)
	}
	if !c.StatsAgent.Enabled || c.StatsAgent.Token != "sekrit" {
		t.Fatalf("JSON update altered the stats agent block: %+v", c.StatsAgent)
	}
}

func TestStatsAgentGeneratesTokenWhenEnabledWithout(t *testing.T) {
	c, _ := NewAtPath("x")
	c.StatsAgent.Enabled = true
	gen, err := c.ResolveStatsAgentToken()
	if err != nil || !gen {
		t.Fatalf("gen=%v err=%v", gen, err)
	}
	if len(c.StatsAgent.Token) != 64 || c.StatsAgent.ResolvedToken() != c.StatsAgent.Token {
		t.Fatalf("token %q resolved %q", c.StatsAgent.Token, c.StatsAgent.ResolvedToken())
	}
	// Idempotent: a second resolve keeps the token it has.
	tok := c.StatsAgent.Token
	if gen, _ := c.ResolveStatsAgentToken(); gen || c.StatsAgent.Token != tok {
		t.Fatal("second resolve regenerated the token")
	}
}

func TestStatsAgentTokenFromFileAndEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	os.WriteFile(p, []byte("  spaced \n"), 0o600)

	c, _ := NewAtPath("x")
	c.StatsAgent.Enabled = true
	c.StatsAgent.Token = "file://" + p
	if _, err := c.ResolveStatsAgentToken(); err != nil {
		t.Fatal(err)
	}
	if c.StatsAgent.ResolvedToken() != "spaced" {
		t.Fatalf("resolved %q", c.StatsAgent.ResolvedToken())
	}

	os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := c.ResolveStatsAgentToken(); err == nil {
		t.Fatal("an empty token file must be an error, not an agent that accepts an empty bearer")
	}
	c.StatsAgent.Token = "file://" + filepath.Join(dir, "missing")
	if _, err := c.ResolveStatsAgentToken(); err == nil {
		t.Fatal("a missing token file must be an error")
	}
}
