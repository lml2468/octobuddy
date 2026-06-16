package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/lml2468/xclaw/desktop/internal/configstore"
	"github.com/lml2468/xclaw/desktop/internal/control"
	"github.com/lml2468/xclaw/desktop/internal/core"
	"github.com/lml2468/xclaw/desktop/internal/secrets"
	"github.com/lml2468/xclaw/desktop/internal/skills"
	"github.com/lml2468/xclaw/desktop/internal/workflows"
)

// EventStream is the single Wails event name the backend uses to push every
// control-bus envelope (responses + events) to the frontend. The Svelte store
// folds them into the view model.
const EventStream = "xclaw:event"

// XClawService is the Wails-bound bridge: it owns the xclawd supervisor and the
// control-bus client, exposes command + config methods to the frontend, and
// forwards the daemon's envelope stream as Wails events.
type XClawService struct {
	mu           sync.Mutex
	sup          *core.Supervisor
	client       *control.Client
	configMode   bool
	shuttingDown bool
	// epoch is a generation counter for daemon (re)connect cycles. RestartCore
	// and each reconnect run bump it; an in-flight reconnect loop bails as soon
	// as it sees a newer epoch, so a manual restart can't be fought (and undone)
	// by a stale crash-reconnect loop still backing off.
	epoch uint64
}

// NewXClawService constructs the bridge (ServiceStartup wires it).
func NewXClawService() *XClawService { return &XClawService{} }

// ServiceStartup spawns xclawd and connects the control bus.
func (x *XClawService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	bin, err := core.ResolveBinary()
	if err != nil {
		return err
	}
	cfg := core.ConfigPath()
	if !fileExists(cfg) {
		cfg = "" // single-bot mode when no multi-bot config present
	}
	x.configMode = cfg != ""
	x.sup = &core.Supervisor{BinPath: bin, SocketPath: core.SocketPath(), ConfigPath: cfg}
	if err := x.sup.Start(); err != nil {
		return err
	}
	if err := x.connect(); err != nil {
		x.sup.Stop()
		return err
	}
	log.Printf("xclaw: bridge up (bin=%s socket=%s configMode=%t)", bin, x.sup.SocketPath, x.configMode)
	return nil
}

// connect dials the control socket, starts forwarding the envelope stream to the
// frontend, primes health + roster, and injects per-bot secrets from the OS
// credential store so the daemon can authenticate without tokens on disk.
func (x *XClawService) connect() error {
	client, err := control.Dial(x.sup.SocketPath)
	if err != nil {
		return err
	}
	x.mu.Lock()
	x.client = client
	x.mu.Unlock()

	go func() {
		err := client.Read(func(env control.Envelope) {
			if app := application.Get(); app != nil {
				app.Event.Emit(EventStream, env)
			}
		})
		// Read returned: the stream ended. Recover unless we're shutting down or
		// another (re)connect already moved on.
		x.mu.Lock()
		stale := x.shuttingDown || x.client != client
		x.mu.Unlock()
		if stale {
			return
		}
		// An oversized frame (bufio.ErrTooLong) desyncs the stream but does NOT
		// mean the daemon died — over a local UDS the daemon is still up, so
		// restarting it wouldn't help and could loop on the same frame. Re-dial
		// the live daemon instead; only fall back to a full respawn if that fails.
		if errors.Is(err, bufio.ErrTooLong) {
			log.Printf("xclaw: oversized control frame (%v); re-dialing without restart", err)
			time.Sleep(300 * time.Millisecond)
			if derr := x.connect(); derr == nil {
				return
			}
		}
		// Clean EOF / closed socket → the daemon exited; respawn + reconnect.
		x.reconnect()
	}()

	_, _ = client.Send("health", nil)
	_, _ = client.Send("bots.list", nil)
	// Inject secrets off the startup path: reading the OS credential store can
	// block on a SecurityAgent prompt (a differently-signed binary isn't yet in
	// the item ACL), and that must never freeze the bridge boot or the UI.
	go x.injectSecrets(client)
	return nil
}

// reconnect respawns the daemon and re-establishes the bus after a crash,
// backing off between attempts. Bounded so a hard-broken daemon doesn't spin.
// It claims a fresh epoch on entry and bails the moment a newer one appears
// (a manual RestartCore, or a later reconnect) so it can't tear down a daemon
// someone else just started.
func (x *XClawService) reconnect() {
	x.mu.Lock()
	x.epoch++
	myEpoch := x.epoch
	x.mu.Unlock()

	for attempt := 0; attempt < 20; attempt++ {
		if !x.epochCurrent(myEpoch) {
			return
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond) // 0.5s → capped below
		if attempt > 6 {
			time.Sleep(2 * time.Second)
		}
		if !x.epochCurrent(myEpoch) {
			return // superseded while we were backing off — don't restart
		}
		if err := x.sup.Restart(); err != nil {
			continue
		}
		if err := x.connect(); err == nil {
			log.Printf("xclaw: reconnected to daemon after crash")
			return
		}
	}
	log.Printf("xclaw: gave up reconnecting to daemon")
}

// epochCurrent reports whether e is still the live generation and we're not
// shutting down.
func (x *XClawService) epochCurrent(e uint64) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return !x.shuttingDown && x.epoch == e
}

// injectSecrets reads each configured bot's tokens from the credential store and
// pushes them to the daemon over the bus (secret.inject). Tokens never touch
// config.json. Best-effort: a bot with no stored token simply stays unauthed.
func (x *XClawService) injectSecrets(client *control.Client) {
	ids, err := configstore.BotIDs()
	if err != nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if t := secrets.Get(id, secrets.OctoToken); t != "" {
			_, _ = client.Send("secret.inject", control.SecretInjectBody{BotID: id, Kind: string(secrets.OctoToken), Value: t})
		}
		if t := secrets.Get(id, secrets.GatewayToken); t != "" {
			_, _ = client.Send("secret.inject", control.SecretInjectBody{BotID: id, Kind: string(secrets.GatewayToken), Value: t})
		}
	}
}

// ServiceShutdown tears the bridge down: close the socket, stop the daemon.
func (x *XClawService) ServiceShutdown() error {
	x.mu.Lock()
	x.shuttingDown = true
	c := x.client
	x.mu.Unlock()
	if c != nil {
		c.Close()
	}
	if x.sup != nil {
		x.sup.Stop()
	}
	return nil
}

// --- session commands (fire-and-forget; replies arrive via EventStream) ---

// Health requests a daemon health snapshot.
func (x *XClawService) Health() error { return x.send("health", nil) }

// BotsList requests the bot roster.
func (x *XClawService) BotsList() error { return x.send("bots.list", nil) }

// Send routes a DM message to a bot (botID may be empty for the default bot).
func (x *XClawService) Send(botID, uid, text string) error {
	return x.send("session.send", control.SessionSendBody{BotID: botID, UID: uid, Text: text})
}

// Reset clears a session's resume mapping (start fresh).
func (x *XClawService) Reset(botID, uid string) error {
	return x.send("session.reset", control.SessionSendBody{BotID: botID, UID: uid})
}

// History requests recent messages for a session (response arrives via EventStream).
func (x *XClawService) History(botID, sessionKey string, limit int) error {
	if limit <= 0 {
		limit = 40
	}
	return x.send("session.history", control.SessionHistoryBody{BotID: botID, SessionKey: sessionKey, Limit: limit})
}

// CronCreate schedules a task (owner-gated by the daemon).
func (x *XClawService) CronCreate(body control.CronCreateBody) error {
	return x.send("cron.create", body)
}

// CronList lists a bot's scheduled tasks.
func (x *XClawService) CronList(botID string) error {
	return x.send("cron.list", control.CronListBody{BotID: botID})
}

// CronDelete removes a scheduled task.
func (x *XClawService) CronDelete(botID, uid, id string) error {
	return x.send("cron.delete", control.CronDeleteBody{BotID: botID, UID: uid, ID: id})
}

// --- config (synchronous; touches config.json + credential store directly) ---

// LoadConfig returns the editor view of every configured bot.
func (x *XClawService) LoadConfig() ([]configstore.BotConfig, error) {
	return configstore.Load()
}

// SaveConfig writes the bots back (config.json + SOUL/AGENTS + credential store).
// removedIDs is the explicit list of bot ids the editor deleted this session;
// only those are pruned from disk (never an inferred set-difference). The caller
// follows with RestartCore to apply.
func (x *XClawService) SaveConfig(bots []configstore.BotConfig, removedIDs []string) error {
	return configstore.Save(bots, removedIDs)
}

// --- skills library (~/.xclaw/skills) for the Skills window ---

// SkillsList returns every skill in the global catalog.
func (x *XClawService) SkillsList() ([]skills.SkillInfo, error) { return skills.List() }

// SkillFiles lists the relative file paths in a skill bundle.
func (x *XClawService) SkillFiles(name string) ([]string, error) { return skills.Files(name) }

// SkillRead returns one file's contents from a skill bundle.
func (x *XClawService) SkillRead(name, rel string) (string, error) { return skills.ReadFile(name, rel) }

// SkillWrite creates/overwrites a file in a skill bundle.
func (x *XClawService) SkillWrite(name, rel, content string) error {
	return skills.WriteFile(name, rel, content)
}

// SkillDeleteFile removes a file from a skill bundle.
func (x *XClawService) SkillDeleteFile(name, rel string) error { return skills.DeleteFile(name, rel) }

// SkillCreate scaffolds a new skill (starter SKILL.md).
func (x *XClawService) SkillCreate(name string) error { return skills.Create(name) }

// SkillDelete removes a skill bundle entirely.
func (x *XClawService) SkillDelete(name string) error { return skills.Delete(name) }

// --- workflows library (~/.xclaw/workflows) for the Workflows window ---

// WorkflowsList returns every workflow in the global catalog.
func (x *XClawService) WorkflowsList() ([]workflows.Info, error) { return workflows.List() }

// WorkflowRead returns a workflow's script source.
func (x *XClawService) WorkflowRead(name string) (string, error) { return workflows.Read(name) }

// WorkflowWrite creates/overwrites a workflow's script.
func (x *XClawService) WorkflowWrite(name, content string) error {
	return workflows.Write(name, content)
}

// WorkflowCreate scaffolds a new workflow.
func (x *XClawService) WorkflowCreate(name string) error { return workflows.Create(name) }

// WorkflowDelete removes a workflow.
func (x *XClawService) WorkflowDelete(name string) error { return workflows.Delete(name) }

// RestartCore restarts the daemon and reconnects (applies config changes). It
// bumps the epoch first so any in-flight crash-reconnect loop bails instead of
// racing this restart.
func (x *XClawService) RestartCore() error {
	x.mu.Lock()
	x.epoch++ // supersede any in-flight reconnect
	if x.client != nil {
		x.client.Close()
		x.client = nil
	}
	x.mu.Unlock()
	if err := x.sup.Restart(); err != nil {
		return err
	}
	return x.connect()
}

func (x *XClawService) send(cmdType string, body any) error {
	x.mu.Lock()
	c := x.client
	x.mu.Unlock()
	if c == nil {
		return fmt.Errorf("control bus not connected")
	}
	_, err := c.Send(cmdType, body)
	return err
}
