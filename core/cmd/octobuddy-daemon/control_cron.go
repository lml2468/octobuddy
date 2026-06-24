package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/cron"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
)

func (d controlCommandDispatcher) cronCreate(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronCreateBody](body)
	if err != nil {
		return nil, err
	}
	t, owner, err := d.cronTarget(b.BotID, "create")
	if err != nil {
		return nil, err
	}
	coords, err := cronCreateCoords(b, owner)
	if err != nil {
		return nil, err
	}
	task, err := t.cron.Create(cron.CreateParams{
		Schedule: b.Schedule, Prompt: b.Prompt, Recurring: b.Recurring, Coords: coords, RequestUID: owner,
	})
	if err != nil {
		return nil, err
	}
	return cronTaskInfo(task), nil
}

func (d controlCommandDispatcher) cronTarget(botID, action string) (*botTarget, string, error) {
	t, err := d.deps.resolve(botID)
	if err != nil {
		return nil, "", err
	}
	if t.cron == nil {
		return nil, "", fmt.Errorf("cron is not enabled for this bot")
	}
	owner := t.cron.OwnerUID()
	if owner == "" {
		return nil, "", fmt.Errorf("bot owner not resolved yet; cannot %s scheduled tasks", action)
	}
	return t, owner, nil
}

func cronCreateCoords(b control.CronCreateBody, owner string) (cron.SessionCoords, error) {
	chType := channelTypeFor(b.ChannelType, b.ChannelID)
	fromUID, err := resolveFromUID(chType, b.FromUID, owner)
	if err != nil {
		return cron.SessionCoords{}, err
	}
	return cron.SessionCoords{
		ChannelID:   b.ChannelID,
		ChannelType: cron.ChannelKind(chType),
		FromUID:     fromUID,
		FromName:    safety.SanitizeDisplayName(b.FromName, owner),
	}, nil
}

func (d controlCommandDispatcher) cronList(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronListBody](body)
	if err != nil {
		return nil, err
	}
	t, err := d.cronListTarget(b.BotID)
	if err != nil {
		return nil, err
	}
	tasks, err := t.cron.List()
	if err != nil {
		return nil, err
	}
	out := make([]control.CronTaskInfo, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, cronTaskInfo(task))
	}
	return control.CronListResponse{BotID: b.BotID, Tasks: out}, nil
}

func (d controlCommandDispatcher) cronListTarget(botID string) (*botTarget, error) {
	t, err := d.deps.resolve(botID)
	if err != nil {
		return nil, err
	}
	if t.cron == nil {
		return nil, fmt.Errorf("cron is not enabled for this bot")
	}
	return t, nil
}

func (d controlCommandDispatcher) cronDelete(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronDeleteBody](body)
	if err != nil {
		return nil, err
	}
	t, owner, err := d.cronTarget(b.BotID, "delete")
	if err != nil {
		return nil, err
	}
	if err := t.cron.Delete(b.ID, owner); err != nil {
		return nil, err
	}
	return control.OKBody{OK: true}, nil
}

func (d controlCommandDispatcher) cronUpdate(body json.RawMessage) (any, error) {
	b, err := decodeControlBody[control.CronUpdateBody](body)
	if err != nil {
		return nil, err
	}
	t, owner, err := d.cronTarget(b.BotID, "update")
	if err != nil {
		return nil, err
	}
	task, err := t.cron.Update(cron.UpdateParams{
		ID: b.ID, Schedule: b.Schedule, Prompt: b.Prompt, Recurring: b.Recurring,
		Coords: cronUpdateCoords(b, owner), Enabled: b.Enabled, RequestUID: owner,
	})
	if err != nil {
		return nil, err
	}
	return cronTaskInfo(task), nil
}

func cronUpdateCoords(b control.CronUpdateBody, owner string) cron.SessionCoords {
	if cronUpdateEnabledOnly(b) {
		return cron.SessionCoords{}
	}
	chType := channelTypeFor(b.ChannelType, b.ChannelID)
	fromUID := b.FromUID
	if chType == int(cron.ChannelConsole) {
		fromUID = cron.ConsoleUID
	} else if chType == int(router.ChannelGroup) {
		fromUID = owner
	}
	fromName := ""
	if b.FromName != "" {
		fromName = safety.SanitizeDisplayName(b.FromName, owner)
	}
	return cron.SessionCoords{
		ChannelID: b.ChannelID, ChannelType: cron.ChannelKind(chType), FromUID: fromUID, FromName: fromName,
	}
}

func cronUpdateEnabledOnly(b control.CronUpdateBody) bool {
	return b.Schedule == "" && b.Prompt == "" && b.Recurring == nil &&
		b.ChannelID == "" && b.ChannelType == 0 && b.FromUID == "" && b.FromName == "" &&
		b.Enabled != nil
}

// channelTypeFor resolves the router/octo channel type for a cron task: an
// explicit non-zero type wins; otherwise a present channelId implies a group and
// its absence a DM. Mirrors the create-time coords binding in cron-tool.ts.
// ChannelConsole (= 3) is honored explicitly so a Console-target task isn't
// silently demoted to DM (the default branch); the IM connector ignores it
// and bot.go's fireCronTask routes it past EnqueueCron straight to the
// gateway. Without this branch a Console body would fall through to "DM
// with empty channelId" which the connector would then try to deliver to.
// resolveFromUID picks the stored FromUID for a NEW cron task based on the
// channel type, falling back to the server-resolved owner for Group targets
// and stamping the canonical ConsoleUID for Console. DM tasks require an
// explicit body FromUID (the peer the task should DM to) — empty is a
// validation error because storing the owner uid for a "DM to alice" task
// would silently rewrite the target to "DM to self" on first fire.
// Used only by cron.create; cron.update's "blank = preserve" semantics
// live in the update handler + Manager.Update mutator.
func resolveFromUID(chType int, bodyFromUID, owner string) (string, error) {
	switch chType {
	case int(cron.ChannelConsole):
		return cron.ConsoleUID, nil
	case int(router.ChannelGroup):
		return owner, nil
	default: // DM
		if bodyFromUID == "" {
			return "", fmt.Errorf("DM target requires fromUid (peer's uid)")
		}
		return bodyFromUID, nil
	}
}

func channelTypeFor(explicit int, channelID string) int {
	if explicit == int(router.ChannelDM) || explicit == int(router.ChannelGroup) || explicit == int(cron.ChannelConsole) {
		return explicit
	}
	if channelID != "" {
		return int(router.ChannelGroup)
	}
	return int(router.ChannelDM)
}

// cronTaskInfo projects a stored cron task onto the wire type (nextRun rendered
// as RFC3339, mirroring cron-tool.ts summarize). LastRun follows the same
// formatter and is omitted entirely when zero (the task has never fired). The
// channel coords are exposed so the GUI can render "into 群 X" / "into DM @ y"
// / "into 控制台" without needing a side-channel lookup, but CreatedBy and
// FromUID are deliberately NOT included — operator-internal auth state, of no
// use to the renderer and a needless leakage surface.
func cronTaskInfo(t cron.Task) control.CronTaskInfo {
	next := ""
	if t.NextRun != 0 {
		next = time.UnixMilli(t.NextRun).UTC().Format(time.RFC3339)
	}
	last := ""
	if t.LastRun != 0 {
		last = time.UnixMilli(t.LastRun).UTC().Format(time.RFC3339)
	}
	return control.CronTaskInfo{
		ID:          t.ID,
		Schedule:    t.Schedule,
		Recurring:   t.Recurring,
		Prompt:      t.Prompt,
		NextRun:     next,
		LastRun:     last,
		ChannelID:   t.ChannelID,
		ChannelType: int(t.ChannelType),
		FromName:    t.FromName,
		Enabled:     t.Enabled,
	}
}
