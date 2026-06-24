package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/router"
)

// fireCronTask wakes the gateway as if a real inbound had arrived. For IM
// targets (DM/Group) it enqueues a synthetic CronFire message onto the octo
// connector's per-session worker so it serializes with any concurrent real
// inbound on the same sessionKey (direct gw.Handle here used to race
// onInbound's target write, mis-delivering one reply and dropping the other).
// For Console targets (ChannelConsole) the connector path is bypassed entirely
// — Console fires belong to the desktop GUI's CONSOLE_UID session, the IM
// connector has no business with them, and the reply naturally surfaces in
// the chat window via the existing session.user_message + session.reply event
// path. The Console call is wrapped in target.turnsWG.Add(1)/Done() so the
// runBot shutdown chain drains in-flight Console fires before st.Close.
//
// Best-effort: a failed enqueue or routing error is logged, never propagated,
// so the scheduler loop survives.
func fireCronTask(ctx context.Context, connector *octo.Connector, gw *gateway.Gateway, target *botTarget, t cron.Task) {
	if t.ChannelType == cron.ChannelConsole {
		// Console-target fire — bypass IM connector. The synthetic inbound
		// is shaped like a CONSOLE_UID DM so router.SessionKey derives the
		// same key the GUI's Composer-typed messages use, and the resulting
		// session.user_message / session.reply broadcasts land in the
		// Console session the user is watching.
		inbound := router.InboundMessage{
			FromUID:     t.FromUID,
			FromName:    t.FromName,
			ChannelType: router.ChannelDM,
			Text:        t.Prompt,
			CronFire:    true,
		}
		if _, err := inbound.SessionKey(); err != nil {
			fmt.Fprintf(os.Stderr, "cron: task %s console fire has unroutable coords: %v\n", t.ID, err)
			return
		}
		target.turnsWG.Add(1)
		go func() {
			defer target.turnsWG.Done()
			if _, err := gw.Handle(ctx, inbound); err != nil {
				fmt.Fprintf(os.Stderr, "cron: task %s console fire dispatch failed: %v\n", t.ID, err)
			}
		}()
		return
	}

	// IM targets — the original path through the per-session worker queue.
	chType := router.ChannelDM
	octoType := octo.ChannelDM
	if t.ChannelType == cron.ChannelKind(router.ChannelGroup) {
		chType = router.ChannelGroup
		octoType = octo.ChannelGroup
	}
	inbound := router.InboundMessage{
		FromUID:     t.FromUID,
		FromName:    t.FromName,
		ChannelID:   t.ChannelID,
		ChannelType: chType,
		Text:        t.Prompt,
		CronFire:    true,
	}
	key, err := inbound.SessionKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cron: task %s has unroutable coords: %v\n", t.ID, err)
		return
	}
	connector.EnqueueCron(key, t.ChannelID, octoType, inbound)
}
