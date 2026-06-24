// Package clog is the thin slog wrapper the daemon uses. It exists for
// two reasons:
//
//   1. Lazy logger binding. A package-level `var log = slog.Default().With(...)`
//      captures the default logger AT INIT TIME, before main.go has
//      called slog.SetDefault with the operator's --debug/--log-json
//      choice. Callers using `clog.For("gateway").Error(...)` instead
//      pick up the live default and get the configured handler.
//
//   2. Consistent component tagging. Every log line carries a
//      `component=<name>` attribute so operators can grep one subsystem
//      out of the unified stream.
//
// The package adds no behavior beyond slog itself — no levels, no
// destinations, no formatters. The handler is set up exactly once in
// daemon main() via clog.Setup() before any package starts logging.
package clog

import (
	"io"
	"log/slog"
	"os"
)

// Setup installs the daemon's default slog handler. Call once from main()
// BEFORE any subsystem starts emitting logs. debug=true bumps level from
// INFO to DEBUG; json=true switches the text handler for JSON (machine
// parsing). out=nil defaults to os.Stderr (matches the pre-slog
// fmt.Fprintf convention).
func Setup(debug, json bool, out io.Writer) {
	if out == nil {
		out = os.Stderr
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if json {
		h = slog.NewJSONHandler(out, opts)
	} else {
		h = slog.NewTextHandler(out, opts)
	}
	slog.SetDefault(slog.New(h))
}

// For returns a logger tagged with the given component name. Each call
// constructs a fresh sub-logger from the live default, so it always uses
// the handler Setup installed.
func For(component string) *slog.Logger {
	return slog.Default().With("component", component)
}
