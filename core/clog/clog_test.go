package clog

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestForCapturesPostSetupHandler is the regression for the lazy-binding
// rationale: a package using clog.For() must pick up the handler
// installed by Setup, NOT whatever was default at package-init time.
func TestForCapturesPostSetupHandler(t *testing.T) {
	// Capture pre-Setup logger by binding For() before changing default.
	preSetupLogger := For("gateway")

	var buf bytes.Buffer
	Setup(false, false, &buf)
	t.Cleanup(func() { Setup(false, false, nil) })

	preSetupLogger.Info("from pre-setup binding", "k", "v")
	if !strings.Contains(buf.String(), "from pre-setup binding") {
		t.Fatalf("pre-Setup For() bound the wrong handler — buf is %q", buf.String())
	}
}

// TestSetupJSONHandler covers the --log-json path.
func TestSetupJSONHandler(t *testing.T) {
	var buf bytes.Buffer
	Setup(false, true, &buf)
	t.Cleanup(func() { Setup(false, false, nil) })

	For("router").Info("dropped", "reason", "rate_limit")
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("expected JSON line, got %q: %v", buf.String(), err)
	}
	if got["component"] != "router" || got["reason"] != "rate_limit" || got["msg"] != "dropped" {
		t.Fatalf("missing expected fields in %+v", got)
	}
}

// TestSetupDebugFiltersInfoVsDebug: at the default level, Debug lines
// are dropped; --debug surfaces them.
func TestSetupDebugFiltersInfoVsDebug(t *testing.T) {
	var buf bytes.Buffer
	Setup(false, false, &buf)
	t.Cleanup(func() { Setup(false, false, nil) })

	For("test").DebugContext(context.Background(), "noisy diagnostic")
	if buf.Len() != 0 {
		t.Fatalf("Debug should be filtered at INFO level, got %q", buf.String())
	}

	buf.Reset()
	Setup(true, false, &buf)
	For("test").DebugContext(context.Background(), "noisy diagnostic")
	if !strings.Contains(buf.String(), "noisy diagnostic") {
		t.Fatalf("Debug should surface with --debug, got %q", buf.String())
	}
}
