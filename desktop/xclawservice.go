package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/lml2468/xclaw/desktop/internal/configstore"
	"github.com/lml2468/xclaw/desktop/internal/control"
	"github.com/lml2468/xclaw/desktop/internal/core"
	"github.com/lml2468/xclaw/desktop/internal/secrets"
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
		_ = client.Read(func(env control.Envelope) {
			if app := application.Get(); app != nil {
				app.Event.Emit(EventStream, env)
			}
		})
		// Read returned: the socket closed. Over a local UDS this means the
		// daemon exited — recover unless we're shutting down or already moved on.
		x.mu.Lock()
		stale := x.shuttingDown || x.client != client
		x.mu.Unlock()
		if !stale {
			x.reconnect()
		}
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
func (x *XClawService) reconnect() {
	for attempt := 0; attempt < 20; attempt++ {
		x.mu.Lock()
		done := x.shuttingDown
		x.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond) // 0.5s → capped below
		if attempt > 6 {
			time.Sleep(2 * time.Second)
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
// The caller follows with RestartCore to apply.
func (x *XClawService) SaveConfig(bots []configstore.BotConfig) error {
	return configstore.Save(bots)
}

// RestartCore restarts the daemon and reconnects (applies config changes).
func (x *XClawService) RestartCore() error {
	x.mu.Lock()
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
