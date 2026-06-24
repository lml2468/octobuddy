// Package router routes inbound messages to per-session handlers with three
// guarantees ported from cc-channel's session-router:
//
// 1. SessionKey derivation — DM is per-peer, group is per-channel (a group is
// one shared session, not N private ones).
// 2. Per-session serial execution — messages within a session run FIFO; across
// sessions they run concurrently (a mutex-per-key, the Go analogue of
// cc-channel's promise-chain lock).
// 3. Rate limiting — three token buckets (global / per-user / per-session),
// all must pass.
//
// It is agent- and IM-agnostic: the IM layer produces InboundMessage values.
package router

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ChannelType mirrors the IM channel kind. Only the DM/Group distinction matters
// for session-key derivation; concrete IMs map their own enums onto these.
type ChannelType int

const (
	ChannelDM    ChannelType = 1
	ChannelGroup ChannelType = 2
)

// AttachmentKind classifies an inbound attachment for materialization (gateway
// media-download helper). Mirrors the MessageType branches cc-channel-octo's
// inbound.ts feeds into downloadInboundImage / tryResolveFile.
type AttachmentKind int

const (
	// AttachmentImage is a still/animated image (PNG/JPEG/GIF/WebP). The gateway
	// downloads it into the session cwd so the agent's Read tool can open it.
	AttachmentImage AttachmentKind = iota
	// AttachmentFile is a generic file: inlined (small text) or downloaded to a
	// temp path (large/binary), per tryResolveFile semantics.
	AttachmentFile
)

// Attachment is one inbound media/file reference. The IM connector populates it
// from the payload (URL + type) WITHOUT downloading — it has no session cwd. The
// gateway materializes it after the cwd is resolved (see gateway/media.go),
// mirroring cc-channel-octo/src/inbound.ts + media-inbound.ts.
type Attachment struct {
	Kind AttachmentKind
	// URL is the fully-resolved, host-validated absolute download URL.
	URL string
	// Name is the (already-sanitized) display filename, used for the Read hint
	// and the <file_content name="…"> attribute. Empty for images.
	Name string
	// Size is the server-reported byte size when known (0 = unknown). Lets the
	// gateway skip downloading files known to exceed the cap.
	Size int64
}

// InboundMessage is the agent/IM-agnostic unit the router operates on.
type InboundMessage struct {
	FromUID     string
	FromName    string
	ChannelID   string
	ChannelType ChannelType
	SpaceID     string // optional isolation prefix (one bot = one space, usually "")
	Text        string
	// Attachments are inbound media/file references the gateway materializes into
	// the session cwd before driver.Query (downloaded images, inlined text files).
	// The connector fills these from the payload; it does not download.
	Attachments []Attachment
	// MessageSeq is the IM platform's per-channel monotonic message sequence (0
	// for synthetic/cron fires). Used by group-context answered/new segmentation
	// to advance the bot's last-reply cursor; never used for session-key derivation.
	MessageSeq int64
	// Mentioned reports whether the bot was addressed (group gate). DMs ignore it.
	Mentioned bool
	// CronFire marks an operator-scheduled synthetic message that bypasses the
	// mention gate and rate limiting (authenticity is the caller's concern).
	CronFire bool
}

// SessionKey derives the logical session identity. Returns an error for
// unroutable messages (missing identity) — callers treat that as a drop, never
// falling back to "" (which would collapse unrelated peers/channels into one
// shared session and leak history/memory).
func (m InboundMessage) SessionKey() (string, error) {
	switch m.ChannelType {
	case ChannelDM:
		if m.FromUID == "" {
			return "", fmt.Errorf("DM message has no from_uid")
		}
		if m.SpaceID != "" {
			return m.SpaceID + ":" + m.FromUID, nil
		}
		return m.FromUID, nil
	case ChannelGroup:
		if m.ChannelID == "" {
			return "", fmt.Errorf("group message has no channel_id")
		}
		return m.ChannelID, nil
	default:
		return "", fmt.Errorf("unknown channel type %d", m.ChannelType)
	}
}

// Config controls rate limiting and the bot-loop / mention-free gating ported
// from cc-channel-octo's session-router.ts (G12 mention-free groups, G14
// multi-bot loop guard, DM blocklist).
type Config struct {
	MaxPerMinute   int // per-session and per-user bucket size; default 30
	MaxContentByte int // reject longer text; default 32 KiB

	// SelfUID is this bot's own uid. It is treated as a known bot (so a relayed
	// self-message in a mention-free group can't trigger a self-loop) — mirrors
	// session-router.ts seeding knownBotUids with robotId. May be "".
	SelfUID string

	// MentionFreeGroups (G12) lists channel ids where the bot replies WITHOUT an
	// @mention. In those channels, unmentioned group messages are Accepted (still
	// size-gated and rate-limited) instead of DroppedNotMentioned.
	MentionFreeGroups []string

	// KnownBotUids (G14) are uids known to be bots, in addition to the `_bot`
	// suffix heuristic. Messages from bot-looking uids are silently dropped in DMs
	// and in mention-free groups to prevent bot↔bot reply loops.
	KnownBotUids []string

	// AllowedBotUids (G14) whitelists bot-looking uids that should be allowed
	// through the loop guard anyway (trusted bots).
	AllowedBotUids []string

	// BotBlocklist lists uids whose DMs are silently dropped (DM only), mirroring
	// session-router.ts's isBlockedBot DM filter.
	BotBlocklist []string
}

func (c Config) withDefaults() Config {
	if c.MaxPerMinute <= 0 {
		c.MaxPerMinute = 30
	}
	if c.MaxContentByte <= 0 {
		c.MaxContentByte = 32 * 1024
	}
	return c
}

// globalRateMultiplier: the global bucket is N× the per-session size.
const globalRateMultiplier = 10

// rateLimitWindow is the refill window for every bucket. Reap relies on
// idle > rateLimitWindow so a reaped bucket is necessarily full again (evicting
// it can't reset a flooder's partially-drained budget). It is the single source
// of truth — both newBucket calls and Reap's clamp reference it.
const rateLimitWindow = time.Minute

// Router serializes per-session work and enforces rate limits.
type Router struct {
	cfg Config
	now func() time.Time

	// Precomputed sets from cfg (built once in New) for O(1) gating lookups.
	mentionFree map[string]bool // channel ids that don't require an @mention (G12)
	knownBots   map[string]bool // uids known to be bots (incl. SelfUID) (G14)
	allowedBots map[string]bool // bot-looking uids exempt from the loop guard (G14)
	blocklisted map[string]bool // uids whose DMs are dropped

	locksMu sync.Mutex
	locks   map[string]*lockEntry // one lock per session key (refcounted for eviction)

	rlMu    sync.Mutex
	global  *bucket
	perUser map[string]*bucket
	perSess map[string]*bucket
}

// lockEntry is a per-session serialization lock plus the bookkeeping Reap needs
// to evict it safely: refs counts in-flight holders, lastUse marks when the last
// holder released. An entry is only evictable when refs == 0 (no goroutine is
// blocked on or holding mu) and it has been idle past the reap threshold.
type lockEntry struct {
	mu      sync.Mutex
	refs    int       // in-flight acquirers; guarded by Router.locksMu
	lastUse time.Time // set on release; guarded by Router.locksMu
}

// New constructs a Router.
func New(cfg Config) *Router {
	cfg = cfg.withDefaults()
	knownBots := toSet(cfg.KnownBotUids)
	// session-router.ts seeds knownBotUids with the bot's own robotId.
	if cfg.SelfUID != "" {
		knownBots[cfg.SelfUID] = true
	}
	return &Router{
		cfg:         cfg,
		now:         time.Now,
		mentionFree: toSet(cfg.MentionFreeGroups),
		knownBots:   knownBots,
		allowedBots: toSet(cfg.AllowedBotUids),
		blocklisted: toSet(cfg.BotBlocklist),
		locks:       make(map[string]*lockEntry),
		perUser:     make(map[string]*bucket),
		perSess:     make(map[string]*bucket),
	}
}

// toSet builds a lookup set from a slice, skipping empty entries. Returns a
// non-nil map so reads need no nil check.
func toSet(vals []string) map[string]bool {
	s := make(map[string]bool, len(vals))
	for _, v := range vals {
		if v != "" {
			s[v] = true
		}
	}
	return s
}

// looksLikeBot mirrors session-router.ts looksLikeBot: a uid is bot-looking if
// it's a known bot uid (incl. SelfUID) or ends in the conventional `_bot`
// suffix. Heuristic, not authoritative — but catches the common bot↔bot loop.
func (r *Router) looksLikeBot(uid string) bool {
	if r.knownBots[uid] {
		return true
	}
	return strings.HasSuffix(uid, "_bot")
}

// SetClock overrides the time source (tests).
func (r *Router) SetClock(now func() time.Time) { r.now = now }
