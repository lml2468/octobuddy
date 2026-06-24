package main

import (
	"context"
	"fmt"
	"time"

	"github.com/lml2468/octobuddy/core/control"
)

// makeMultiBotHandler routes control-bus commands by botId across the registry.
// All command logic lives in the shared makeHandler; this only supplies the
// multi-bot resolution + roster + event broadcast.
func makeMultiBotHandler(ctx context.Context, reg *botRegistry, started time.Time) control.CommandHandler {
	return makeHandler(ctx, handlerDeps{
		started:  started,
		botCount: func() int { return len(reg.list()) },
		list:     reg.list,
		resolve: func(botID string) (*botTarget, error) {
			bot := reg.get(botID)
			if bot == nil {
				return nil, fmt.Errorf("unknown bot %q", botID)
			}
			// A bot whose startup failed (registerFailedBot stub) carries a
			// nil gateway/store + a populated LastError. Without this check
			// the resolve-fallback would wire nil into a botTarget, and the
			// first handler dereferencing `t.gateway` / `t.store` would nil-
			// deref the whole daemon. Test fixtures also
			// carry nil gateway/store but no LastError, so they keep using
			// the test-friendly lazy fallback below.
			if bot.gateway == nil && bot.info().LastError != "" {
				return nil, fmt.Errorf("bot %q failed to start: %s", botID, bot.info().LastError)
			}
			// runBot eager-initializes target AND cron; production never
			// sees nil here. Tests that build a botRuntime by hand must
			// set target explicitly or go through runBot — surfacing a
			// clear error beats a lock-free lazy write that would race
			// concurrent control commands.
			if bot.target == nil {
				return nil, fmt.Errorf("bot %q not initialised", botID)
			}
			return bot.target, nil
		},
		broadcast: func(eventType string, body any) {
			if reg.srv != nil {
				reg.srv.Broadcast(eventType, body)
			}
		},
	})
}
