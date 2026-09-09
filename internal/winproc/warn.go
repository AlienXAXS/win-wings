//go:build windows

package winproc

// Warn reports a non-fatal problem encountered while launching a process.
//
// This package is used by both the daemon and the worker, which log through
// different facilities and neither of which this package should import. Rather
// than thread a logger through every call, each binary replaces this at startup.
//
// The default discards, so that tests and short-lived tools are silent.
var Warn = func(string) {}
