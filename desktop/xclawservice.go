package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/lml2468/xclaw/desktop/internal/configstore"
	"github.com/lml2468/xclaw/desktop/internal/control"
	"github.com/lml2468/xclaw/desktop/internal/core"
	"github.com/lml2468/xclaw/desktop/internal/octoapi"
	"github.com/lml2468/xclaw/desktop/internal/octocli"
	"github.com/lml2468/xclaw/desktop/internal/secrets"
	"github.com/lml2468/xclaw/desktop/internal/skills"
	"github.com/lml2468/xclaw/desktop/internal/workflows"
	"github.com/lml2468/xclaw/desktop/internal/workspace"
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
	shuttingDown bool
	// daemonOut is where the daemon's stdout+stderr land. nil means inherit
	// os.Stderr (the legacy default). main() supplies the rotating xclaw.log
	// tee so daemon banner / gateway errors / selfcheck lines survive past the
	// app's stderr.
	daemonOut io.Writer
	// epoch is a generation counter for daemon (re)connect cycles. RestartCore
	// and each reconnect run bump it; an in-flight reconnect loop bails as soon
	// as it sees a newer epoch, so a manual restart can't be fought (and undone)
	// by a stale crash-reconnect loop still backing off.
	epoch uint64
	// oversizedRetries counts consecutive ErrTooLong re-dials so a daemon emitting
	// a legitimately oversized frame can't trap the bridge in a 300ms re-dial busy
	// loop — after the cap we fall back to a full reconnect. Reset on a clean read.
	oversizedRetries int
}

// maxOversizedRedials bounds consecutive ErrTooLong re-dials before escalating to
// a full reconnect. The most likely cause of an over-cap frame is the daemon
// itself producing a large event, which a re-dial won't fix — so don't loop on it.
const maxOversizedRedials = 3

// NewXClawService constructs the bridge (ServiceStartup wires it). daemonOut
// receives the daemon's stdout+stderr; pass nil to inherit os.Stderr.
func NewXClawService(daemonOut io.Writer) *XClawService {
	return &XClawService{daemonOut: daemonOut}
}

// ServiceStartup spawns xclawd and connects the control bus.
func (x *XClawService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	bin, err := core.ResolveBinary()
	if err != nil {
		return err
	}
	// Always run the daemon in multi-bot config mode. On first launch
	// ~/.xclaw/config.json may not exist yet — the daemon tolerates that and
	// serves an empty bots.list; the GUI then drives the user to the Add Bot
	// wizard. We MUST pin the supervisor's ConfigPath now (never to "") so a
	// later RestartCore after the first SaveConfig actually re-spawns with
	// -config and picks up the freshly-written roster, instead of remaining
	// stuck in the synthetic single-bot REPL fallback.
	cfg := core.ConfigPath()
	x.sup = &core.Supervisor{BinPath: bin, SocketPath: core.SocketPath(), ConfigPath: cfg, Output: x.daemonOut}
	if err := x.sup.Start(); err != nil {
		// Start may have spawned the daemon process before the socket-wait
		// timed out — Supervisor returns the error but leaves s.cmd set, so
		// the reaper goroutine is alive and the daemon is running. Without
		// this Stop, the orphaned daemon survives until -exit-with-parent
		// kicks in (Linux only); on macOS it lingers indefinitely.
		x.sup.Stop()
		return err
	}
	if err := x.connect(); err != nil {
		x.sup.Stop()
		return err
	}
	log.Printf("xclaw: bridge up (bin=%s socket=%s)", bin, x.sup.SocketPath)
	return nil
}

// connect dials the control socket, starts forwarding the envelope stream to the
// frontend, primes health + roster, and injects per-bot secrets from the secret
// backend so the daemon can authenticate without tokens in config.json.
func (x *XClawService) connect() error {
	client, err := control.Dial(x.sup.SocketPath)
	if err != nil {
		return err
	}
	x.mu.Lock()
	x.client = client
	x.mu.Unlock()

	go func() {
		firstEnvelope := true
		err := client.Read(func(env control.Envelope) {
			if firstEnvelope {
				firstEnvelope = false
				// Only NOW do we know the wire is healthy enough to send
				// at least one frame. Reset the over-cap re-dial counter
				// here rather than at connect entry — resetting on every
				// re-dial turned the maxOversizedRedials cap into dead
				// code (the redial→connect→reset→redial cycle accumulated
				// no count, so the fallback full-reconnect never fired).
				x.mu.Lock()
				x.oversizedRetries = 0
				x.mu.Unlock()
			}
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
		// Tell the frontend the bus dropped so the UI can show "reconnecting"
		// instead of silently freezing on the last state.
		x.emitConnState(false, "control stream ended")
		// An oversized frame (bufio.ErrTooLong) desyncs the stream but does NOT
		// mean the daemon died. Re-dialing the LIVE daemon can clear a transient
		// desync — but the likeliest cause is the daemon emitting a legitimately
		// over-cap event, which a re-dial won't fix and would busy-loop on. So
		// bound the re-dials and route them through the epoch guard so a
		// concurrent RestartCore can't be raced; after the cap, fall back to a full
		// reconnect.
		if errors.Is(err, bufio.ErrTooLong) {
			x.mu.Lock()
			x.oversizedRetries++
			n := x.oversizedRetries
			x.epoch++
			myEpoch := x.epoch
			x.mu.Unlock()
			if n <= maxOversizedRedials {
				log.Printf("xclaw: oversized control frame (%v); re-dial %d/%d", err, n, maxOversizedRedials)
				time.Sleep(300 * time.Millisecond)
				if !x.epochCurrent(myEpoch) {
					return // a manual restart / newer reconnect superseded us
				}
				if derr := x.connect(); derr == nil {
					return
				}
			} else {
				log.Printf("xclaw: oversized control frame persists after %d re-dials; full reconnect", n)
			}
		}
		// Clean EOF / closed socket → the daemon exited; respawn + reconnect.
		x.reconnect()
	}()

	_, _ = client.Send(control.CmdAuth, control.AuthBody{Token: x.sup.Token()})
	_, _ = client.Send("health", nil)
	_, _ = client.Send("bots.list", nil)
	x.emitConnState(true, "") // bus is up — clear any "reconnecting" UI state
	// Inject secrets off the startup path: reading the secret backend can block
	// (for example on an OS credential prompt), and that must never freeze the
	// bridge boot or the UI.
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
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond) // linear: 0.5s, 1s, 1.5s…
		if attempt > 6 {
			time.Sleep(2 * time.Second) // add a flat 2s tail on later attempts
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

// emitConnState pushes a synthetic bridge.status event to the frontend so the UI
// can reflect the bus connection state (connected / reconnecting) instead of
// silently freezing on its last state when the daemon drops.
func (x *XClawService) emitConnState(connected bool, detail string) {
	app := application.Get()
	if app == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"connected": connected, "detail": detail})
	app.Event.Emit(EventStream, control.Envelope{
		V:    1,
		Kind: control.KindEvent,
		Type: "bridge.status",
		Body: body,
	})
}

// injectSecrets reads each configured bot's secrets from the configured secret
// backend and pushes them to the daemon over the bus (secret.inject). Secret
// values never touch config.json. Best-effort: a missing secret simply leaves
// that runtime value unset.
func (x *XClawService) injectSecrets(client *control.Client) {
	ids, err := configstore.BotIDs()
	if err != nil || len(ids) == 0 {
		return
	}
	refs, _ := configstore.BotSecretRefs()
	for _, id := range ids {
		if t := secrets.Get(id, secrets.OctoToken); t != "" {
			_, _ = client.Send("secret.inject", control.SecretInjectBody{BotID: id, Kind: secrets.OctoToken, Value: t})
		}
		if t := secrets.Get(id, secrets.GatewayToken); t != "" {
			_, _ = client.Send("secret.inject", control.SecretInjectBody{BotID: id, Kind: secrets.GatewayToken, Value: t})
		}
		for _, ref := range refs[id] {
			if t := secrets.Get(id, secrets.Kind(ref)); t != "" {
				_, _ = client.Send("secret.inject", control.SecretInjectBody{BotID: id, Kind: secrets.Kind(ref), Value: t})
			}
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

// SessionsList requests all persisted sessions for a bot, newest first (response
// arrives via EventStream as a sessions.list envelope).
func (x *XClawService) SessionsList(botID string) error {
	return x.send("sessions.list", control.SessionsListBody{BotID: botID})
}

// UsageStats requests a bot's token usage over a range (since = Unix seconds at a
// local-midnight bound; 0 = all time). The response arrives via EventStream as a
// usage.stats envelope echoing `since`.
func (x *XClawService) UsageStats(botID string, since int64) error {
	return x.send("usage.stats", control.UsageStatsBody{BotID: botID, Since: since})
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

// CronUpdate mutates a scheduled task. The body's Enabled is a pointer so the
// renderer's per-row enable/disable toggle can send an enabled-only update
// (other fields zero) without re-validating the unchanged schedule on the
// daemon — see core/cron Update's enabled-only fast path.
func (x *XClawService) CronUpdate(body control.CronUpdateBody) error {
	return x.send("cron.update", body)
}

// --- config (synchronous; touches config.json + secret backend directly) ---

// LoadConfig returns the editor view of every configured bot.
func (x *XClawService) LoadConfig() ([]configstore.BotConfig, error) {
	return configstore.Load()
}

// SaveConfig writes the bots back (config.json + SOUL/AGENTS + secret backend).
// removedIDs is the explicit list of bot ids the editor deleted this session;
// only those are pruned from disk (never an inferred set-difference). The caller
// follows with RestartCore to apply.
func (x *XClawService) SaveConfig(bots []configstore.BotConfig, removedIDs []string) error {
	return configstore.Save(bots, removedIDs)
}

// --- octo-cli profile management (per-bot disk profiles in ~/.octo-cli/) ---

// OctoCliStatus is the per-bot octo-cli registration state surfaced to the
// Octo-integration pane: registered iff ~/.octo-cli/config.json has an entry
// for the bot's OCTO_BOT_ID. RobotID is included so the UI can show what we
// looked up (and reveal mismatches between config and what's actually in env).
type OctoCliStatus struct {
	Registered bool   `json:"registered"`
	RobotID    string `json:"robotId"`
}

// OctoCliStatus reports whether the bot's octo-cli profile is registered.
// Reads config.json directly; no octo-cli spawn needed (cheap for a UI poll).
func (x *XClawService) OctoCliStatus(botID string) (OctoCliStatus, error) {
	robotID, _, _, err := loadOctoBinding(botID)
	if err != nil {
		return OctoCliStatus{}, err
	}
	return OctoCliStatus{Registered: octocli.HasProfile(robotID), RobotID: robotID}, nil
}

// GroupsList enumerates the IM groups the bot is a member of, populated for
// the scheduled-task target picker so the operator picks "this group" from
// a dropdown instead of pasting a channelId. Synchronous (not fire-and-
// forget over the control bus) because the renderer awaits it before
// opening the create-task modal, same shape as OctoCliStatus.
//
// Returns an empty list (and no error) when the bot has no groups; returns
// an error when octo-cli is not installed / not authenticated for this bot
// so the GUI can surface a meaningful "log in first" message rather than
// silently showing "no groups available".
func (x *XClawService) GroupsList(botID string) ([]octocli.Group, error) {
	robotID, _, _, err := loadOctoBinding(botID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return octocli.Groups(ctx, robotID)
}

// OctoCliRelogin re-writes the disk profile for the bot from the stored
// bf_ token. Used to repair a missing/stale profile from the Octo-integration
// pane without forcing the operator to re-save the whole config.
func (x *XClawService) OctoCliRelogin(botID string) error {
	robotID, token, apiURL, err := loadOctoBinding(botID)
	if err != nil {
		return err
	}
	if robotID == "" {
		return fmt.Errorf("bot %q has no OCTO_BOT_ID in env", botID)
	}
	if token == "" {
		return fmt.Errorf("bot %q has no bf_ token in the secret backend — set it via the Octo 集成 tab and re-save", botID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return octocli.Login(ctx, robotID, token, apiURL)
}

// OctoCliLogout clears the bot's disk profile. The stored bf_ token is
// left alone — re-login can restore the profile from it.
func (x *XClawService) OctoCliLogout(botID string) error {
	robotID, _, _, err := loadOctoBinding(botID)
	if err != nil {
		return err
	}
	if robotID == "" {
		return nil // nothing to log out of
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return octocli.Logout(ctx, robotID)
}

// loadOctoBinding returns the bot's (robotID, bf_token, apiURL) by reading
// configstore. Uses LoadOne so we do one config.json parse + two secret
// reads — not N of each for a single-bot lookup.
func loadOctoBinding(botID string) (robotID, token, apiURL string, err error) {
	bot, ok, lerr := configstore.LoadOne(botID)
	if lerr != nil {
		return "", "", "", lerr
	}
	if !ok {
		return "", "", "", fmt.Errorf("bot %q not found", botID)
	}
	return bot.Env["OCTO_BOT_ID"], bot.OctoToken, bot.APIURL, nil
}

// OctoAddBot provisions a new bot on octo-server using the operator's User API
// Key (uk_…), returning the bot's robot id + bf_ token. The wizard then folds
// these into a BotConfig and calls SaveConfig — so the token reaches the
// secret backend (never config.json) by the existing path. Self-service replacement
// for the manual BotFather /newbot flow.
func (x *XClawService) OctoAddBot(apiURL, apiKey, name string) (octoapi.BotResult, error) {
	// Bound the call so the wizard UI can't strand a request forever — the
	// octoapi httpClient has a 30 s timeout but no caller-side ceiling
	// (arch #7, matching the OctoCliRelogin / Logout pattern).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return octoapi.AddBot(ctx, apiURL, apiKey, name)
}

// --- skills: per-bot (~/.xclaw/<id>/.claude/skills) ---

// BotSkillsList returns a bot's skill bundles.
func (x *XClawService) BotSkillsList(botID string) ([]skills.SkillInfo, error) {
	return skills.BotList(botID)
}

// BotSkillFiles lists files in one of the bot's skill bundles.
func (x *XClawService) BotSkillFiles(botID, name string) ([]string, error) {
	return skills.BotFiles(botID, name)
}

// BotSkillRead reads a file within one of the bot's skill bundles.
func (x *XClawService) BotSkillRead(botID, name, rel string) (string, error) {
	return skills.BotRead(botID, name, rel)
}

// BotSkillWrite writes a file within one of the bot's skill bundles.
func (x *XClawService) BotSkillWrite(botID, name, rel, content string) error {
	return skills.BotWrite(botID, name, rel, content)
}

// BotSkillDeleteFile removes a file within one of the bot's skill bundles.
func (x *XClawService) BotSkillDeleteFile(botID, name, rel string) error {
	return skills.BotDeleteFile(botID, name, rel)
}

// BotSkillCreate scaffolds a new per-bot skill bundle.
func (x *XClawService) BotSkillCreate(botID, name string) error { return skills.BotCreate(botID, name) }

// BotSkillDelete removes one of the bot's skill bundles.
func (x *XClawService) BotSkillDelete(botID, name string) error { return skills.BotDelete(botID, name) }

// --- workflows: per-bot (~/.xclaw/<id>/.claude/workflows) ---

// BotWorkflowsList returns a bot's workflow scripts.
func (x *XClawService) BotWorkflowsList(botID string) ([]workflows.Info, error) {
	return workflows.BotList(botID)
}

// BotWorkflowRead reads one of the bot's workflow scripts.
func (x *XClawService) BotWorkflowRead(botID, name string) (string, error) {
	return workflows.BotRead(botID, name)
}

// BotWorkflowWrite writes one of the bot's workflow scripts.
func (x *XClawService) BotWorkflowWrite(botID, name, content string) error {
	return workflows.BotWrite(botID, name, content)
}

// BotWorkflowCreate scaffolds a new per-bot workflow script.
func (x *XClawService) BotWorkflowCreate(botID, name string) error {
	return workflows.BotCreate(botID, name)
}

// BotWorkflowDelete removes one of the bot's workflow scripts.
func (x *XClawService) BotWorkflowDelete(botID, name string) error {
	return workflows.BotDelete(botID, name)
}

// WorkspaceTree returns the file tree of a session's sandbox workspace
// (read-only). Returns an empty tree when no turn has created the sandbox yet.
func (x *XClawService) WorkspaceTree(botID string, channelType int, sessionKey string) (*workspace.Node, error) {
	return workspace.Tree(botID, channelType, sessionKey)
}

// WorkspaceFile returns one workspace file's contents for inline preview
// (utf8 text or base64 for images/binaries), bounded and traversal-safe.
func (x *XClawService) WorkspaceFile(botID string, channelType int, sessionKey, relPath string) (workspace.FileContent, error) {
	return workspace.File(botID, channelType, sessionKey, relPath)
}

// MemoryTree returns the file tree of a session's auto-memory directory
// (read-only). Returns an empty tree when no memory has been written yet.
func (x *XClawService) MemoryTree(botID string, channelType int, sessionKey string) (*workspace.Node, error) {
	return workspace.MemoryTree(botID, channelType, sessionKey)
}

// MemoryFile returns one session memory file's contents for inline preview,
// bounded and traversal-safe.
func (x *XClawService) MemoryFile(botID string, channelType int, sessionKey, relPath string) (workspace.FileContent, error) {
	return workspace.MemoryFile(botID, channelType, sessionKey, relPath)
}

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
