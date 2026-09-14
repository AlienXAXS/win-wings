//go:build windows

// Package statsagent serves host load figures to the Panel's Free Servers
// extension.
//
// It is the Windows counterpart of ax-freeservers/node-agent, a Node.js
// container that Linux nodes run beside wings. The Panel polls it every ten
// minutes over plain HTTP with a per-node bearer token and does not know or
// care what is answering, so this speaks exactly the same protocol:
//
//	GET /ping      no auth       {"ok":true}
//	GET /metrics   Bearer token  CPU, memory and disk (see hoststats.Report)
//
// Anything else is 404, any other method 405, and a bad or missing token 401.
// Errors carry the same {"error": "..."} body the Linux agent sends.
package statsagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/hoststats"
	"github.com/pterodactyl/wings/system"
)

// Agent is one listener with its collector.
type Agent struct {
	cfg       config.StatsAgentConfiguration
	collector *hoststats.Collector
}

// New builds an agent from the configuration, serving reports from collector.
// The token must already have been resolved; a caller that forgot gets an error
// rather than a listener that rejects everything.
func New(cfg config.StatsAgentConfiguration, collector *hoststats.Collector) (*Agent, error) {
	if cfg.ResolvedToken() == "" {
		return nil, errors.New("statsagent: no token resolved; call config.ResolveStatsAgentToken first")
	}
	if collector == nil {
		return nil, errors.New("statsagent: no collector")
	}
	return &Agent{cfg: cfg, collector: collector}, nil
}

// NewCollector is the sampler the daemon runs for the lifetime of the process,
// whether or not the agent's listener is enabled: the main API serves the same
// report under the node token, and utilisation needs history to be meaningful.
func NewCollector(dataPath string) *hoststats.Collector {
	return hoststats.New(dataPath, "win-wings "+system.Version)
}

// Handler returns the HTTP handler, separate from Run so it can be tested
// without binding a port.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", a.ping)
	mux.HandleFunc("/metrics", a.metrics)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
}

// Run binds the listener, starts sampling, and serves until ctx is cancelled.
//
// Binding is done before the goroutine so that a port already in use is
// reported to the caller at boot rather than lost in a log line.
func (a *Agent) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Address())
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.WithFields(log.Fields{
		"subsystem": "stats-agent",
		"address":   ln.Addr().String(),
	}).Info("stats agent is listening; the Panel's Free Servers extension polls this over plain HTTP")

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *Agent) ping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Agent) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !a.authorised(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, a.collector.Report())
}

// authorised checks the bearer token in constant time.
func (a *Agent) authorised(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got, want := []byte(strings.TrimSpace(h[len(prefix):])), []byte(a.cfg.ResolvedToken())
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
