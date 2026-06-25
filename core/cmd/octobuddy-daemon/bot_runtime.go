package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lml2468/octobuddy/core/clog"
	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/store"
)

// configFlagSet reports whether -config was passed on the command line (so an
// empty value still selects config mode with the default path).
func configFlagSet() bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			set = true
		}
	})
	return set
}

// botRuntime is one running bot's externally-reachable handles: its gateway (for
// routing control-bus commands) and live status.
type botRuntime struct {
	cfg     config.Resolved
	gateway *gateway.Gateway
	store   *store.Store
	secrets *secretStore  // in-memory tokens (seeded from cfg, updated by secret.inject)
	cron    *cron.Manager // nil when agent.cron is disabled
	// connector is the IM-edge for this bot. The control plane reaches into it
	// for name resolution (sidebar channel titles, bubble sender labels — see
	// summariesFromSessions in control.go); the gateway holds its own ref via
	// Sink/MediaAuth/BackfillFetch wiring.
	connector *octo.Connector

	// target is the per-bot control-handler target, owned for the runtime's
	// lifetime so the embedded turnsWG is shared across all session.send
	// goroutines for this bot (a fresh botTarget per resolve would give
	// each goroutine its own WaitGroup and defeat the shutdown barrier).
	target *botTarget

	mu        sync.Mutex
	connected bool
	lastErr   string
}

func (b *botRuntime) info() control.BotInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return control.BotInfo{ID: b.cfg.BotID, Connected: b.connected, LastError: b.lastErr}
}

func (b *botRuntime) setStatus(connected bool, lastErr string) {
	b.mu.Lock()
	b.connected, b.lastErr = connected, lastErr
	b.mu.Unlock()
}

// botRegistry tracks all running bots for control-bus routing and bots.list.
type botRegistry struct {
	mu   sync.RWMutex
	bots map[string]*botRuntime
	srv  *control.Server // for broadcasting bot.status changes
}

func newBotRegistry(srv *control.Server) *botRegistry {
	return &botRegistry{bots: map[string]*botRuntime{}, srv: srv}
}

func (r *botRegistry) add(b *botRuntime) {
	r.mu.Lock()
	r.bots[b.cfg.BotID] = b
	r.mu.Unlock()
}

func (r *botRegistry) get(id string) *botRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id == "" && len(r.bots) == 1 {
		for _, b := range r.bots { // single-bot convenience
			return b
		}
	}
	return r.bots[id]
}

func (r *botRegistry) list() []control.BotInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]control.BotInfo, 0, len(r.bots))
	for _, b := range r.bots {
		out = append(out, b.info())
	}
	return out
}

// runConfigMode loads the single-file config, serves the control bus, and runs
// every configured bot in its own isolated goroutine until SIGINT/SIGTERM (or,
// when exitWithParent is set, until the launching process dies).
func runConfigMode(path, controlSock string, exitWithParent bool, authStdin bool) {
	bots, err := config.Load(path)
	if err != nil {
		fatal("config: %v", err)
	}
	// An empty roster is a valid first-run state — the GUI writes config.json
	// before the user adds any bots, and adds them later via the control bus
	// (SaveConfig → RestartCore). The daemon stays up and serves an empty
	// bots.list rather than dying and leaving the GUI without a peer.

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// When launched by the GUI, shut down if the app dies (even on a crash that
	// skips graceful teardown) so the daemon never lingers holding the socket.
	if exitWithParent {
		watchParentExit(stop)
	}

	started := time.Now()

	reg := newBotRegistry(nil)
	srv, stopControl := startConfigControl(ctx, reg, started, controlSock, authStdin)
	if stopControl != nil {
		defer stopControl()
	}

	fmt.Printf("octobuddy-daemon — config mode: %d bot(s)\n", len(bots))
	runConfiguredBots(ctx, path, bots, reg, srv)
}

func startConfigControl(ctx context.Context, reg *botRegistry, started time.Time, controlSock string, authStdin bool) (*control.Server, func()) {
	if controlSock == "" {
		return nil, nil
	}
	srv := control.NewServer(nil)
	reg.srv = srv
	srv.SetHandler(makeMultiBotHandler(ctx, reg, started))
	configureBusAuth(srv, authStdin)
	cleanup := serveControlBus(srv, controlSock)
	var once sync.Once
	stopControl := func() { once.Do(cleanup) }
	go func() {
		<-ctx.Done()
		stopControl()
	}()
	return srv, stopControl
}

func runConfiguredBots(ctx context.Context, configPath string, bots []config.Resolved, reg *botRegistry, srv *control.Server) {
	var wg sync.WaitGroup
	for _, cfg := range bots {
		wg.Add(1)
		go func(cfg config.Resolved) {
			defer wg.Done()
			// Panic isolation: a panic in one bot's driver/connector must not crash
			// the whole daemon and take down every other bot — the promise is a
			// fully-isolated per-bot stack. Recover, mark the bot failed (so it shows
			// up in bots.list with the error instead of silently vanishing), and let
			// the others keep running.
			defer func() {
				if r := recover(); r != nil {
					clog.For("bot").Error("panic", "bot", cfg.BotID, "panic", r)
					registerFailedBot(reg, cfg, fmt.Sprintf("panic: %v", r))
				}
			}()
			if err := runBot(ctx, configPath, cfg, reg, srv); err != nil && ctx.Err() == nil {
				clog.For("bot").Warn("exited", "bot", cfg.BotID, "err", err)
				// A startup failure (store open, mkdir, …) returns before the bot
				// registers itself. Register a failed-status stub so the GUI/bots.list
				// shows it as down-with-error rather than missing entirely.
				registerFailedBot(reg, cfg, err.Error())
			}
		}(cfg)
	}
	// First-run state (no bots yet): wg.Wait would return immediately and the
	// daemon would exit before the GUI ever connects. Block on ctx so the
	// control bus stays up and SaveConfig → RestartCore can populate the
	// roster. The signal-driven ctx.Done path (SIGINT/SIGTERM/exit-with-parent)
	// is the same one that ends a populated run after wg.Wait, so shutdown
	// semantics match either branch.
	if len(bots) == 0 {
		<-ctx.Done()
		return
	}
	wg.Wait()
}

// registerFailedBot records a bot that failed to start (or panicked) so it
// appears in bots.list with a down status + error, instead of silently missing.
// If the bot already registered itself, its existing runtime is marked failed.
func registerFailedBot(reg *botRegistry, cfg config.Resolved, errMsg string) {
	if b := reg.get(cfg.BotID); b != nil {
		b.setStatus(false, errMsg)
		return
	}
	b := &botRuntime{cfg: cfg, secrets: &secretStore{}}
	b.setStatus(false, errMsg)
	reg.add(b)
}
