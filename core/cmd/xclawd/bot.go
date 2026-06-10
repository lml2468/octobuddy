package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/lml2468/xclaw/core/config"
	"github.com/lml2468/xclaw/core/gateway"
	"github.com/lml2468/xclaw/core/groupctx"
	"github.com/lml2468/xclaw/core/im/octo"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/store"
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

// runConfigMode loads the bot-first config and runs every configured bot in its
// own isolated goroutine until SIGINT/SIGTERM.
func runConfigMode(path string) {
	bots, err := config.Load(path)
	if err != nil {
		fatal("config: %v", err)
	}
	if len(bots) == 0 {
		fatal("config: no bots configured")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("xclawd — config mode: %d bot(s)\n", len(bots))

	var wg sync.WaitGroup
	for _, cfg := range bots {
		wg.Add(1)
		go func(cfg config.Resolved) {
			defer wg.Done()
			if err := runBot(ctx, cfg); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "[%s] exited: %v\n", cfg.BotID, err)
			}
		}(cfg)
	}
	wg.Wait()
}

// runBot assembles and runs one bot's complete, isolated stack from a resolved
// config: its own SQLite store (under the bot's derived dataDir), router,
// gateway (with group-context + SOUL system prompt), agent driver, and Octo
// connector. Blocks until ctx is cancelled. Each bot is fully isolated — no
// shared store, gateway, or connector — mirroring cc-channel's per-bot subtree.
func runBot(ctx context.Context, cfg config.Resolved) error {
	// Per-bot SQLite under the derived data dir.
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("bot %s: mkdir data: %w", cfg.BotID, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "xclaw.db"))
	if err != nil {
		return fmt.Errorf("bot %s: store: %w", cfg.BotID, err)
	}
	defer st.Close()
	if n, _ := st.CleanupExpired(store.DefaultTTL); n > 0 {
		fmt.Fprintf(os.Stderr, "[%s] swept %d expired session(s)\n", cfg.BotID, n)
	}

	drv, err := makeDriver(cfg.SDK.Driver, "")
	if err != nil {
		return fmt.Errorf("bot %s: %w", cfg.BotID, err)
	}

	rt := router.New(router.Config{MaxPerMinute: cfg.RateLimit.MaxPerMinute})

	// Octo connector requires an apiUrl + octo token.
	if cfg.APIURL == "" || cfg.OctoToken == "" {
		return fmt.Errorf("bot %s: config mode requires apiUrl + octoToken", cfg.BotID)
	}
	connector := octo.NewConnector(octo.NewRESTClient(cfg.APIURL, cfg.OctoToken))

	gw := gateway.New(drv, st, rt, connector).
		WithGroupContext(groupctx.New(cfg.Context.MaxContextChars)).
		WithSystemPrompt(cfg.SDK.SystemPrompt)
	connector.SetGateway(gw)

	fmt.Printf("[%s] started — driver=%s api=%s data=%s\n",
		cfg.BotID, drv.Name(), cfg.APIURL, cfg.DataDir)

	return connector.Run(ctx)
}
