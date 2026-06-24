package gateway

import (
	"log/slog"

	"github.com/lml2468/octobuddy/core/clog"
)

// glog returns the gateway-component logger. Lazy (not a var) so it
// always picks up the live slog default — main installs clog.Setup
// AFTER package init, and a var binding would capture the pre-Setup
// default and silently bypass --debug/--log-json.
func glog() *slog.Logger { return clog.For("gateway") }
