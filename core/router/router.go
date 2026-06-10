// Package router routes inbound messages to per-session handlers with three
// guarantees ported from cc-channel's session-router:
//
//  1. SessionKey derivation — DM is per-peer, group is per-channel (a group is
//     one shared session, not N private ones).
//  2. Per-session serial execution — messages within a session run FIFO; across
//     sessions they run concurrently (a mutex-per-key, the Go analogue of
//     cc-channel's promise-chain lock).
//  3. Rate limiting — three token buckets (global / per-user / per-session),
//     all must pass.
//
// It is agent- and IM-agnostic: the IM layer produces InboundMessage values.
package router

import (
	"context"
	"fmt"
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

// InboundMessage is the agent/IM-agnostic unit the router operates on.
type InboundMessage struct {
	FromUID     string
	FromName    string
	ChannelID   string
	ChannelType ChannelType
	SpaceID     string // optional isolation prefix (one bot = one space, usually "")
	Text        string
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

// Config controls rate limiting.
type Config struct {
	MaxPerMinute   int // per-session and per-user bucket size; default 5
	MaxContentByte int // reject longer text; default 32 KiB
}

func (c Config) withDefaults() Config {
	if c.MaxPerMinute <= 0 {
		c.MaxPerMinute = 5
	}
	if c.MaxContentByte <= 0 {
		c.MaxContentByte = 32 * 1024
	}
	return c
}

// globalRateMultiplier: the global bucket is N× the per-session size.
const globalRateMultiplier = 10

// Router serializes per-session work and enforces rate limits.
type Router struct {
	cfg Config
	now func() time.Time

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // one mutex per session key

	rlMu    sync.Mutex
	global  *bucket
	perUser map[string]*bucket
	perSess map[string]*bucket
}

// New constructs a Router.
func New(cfg Config) *Router {
	return &Router{
		cfg:     cfg.withDefaults(),
		now:     time.Now,
		locks:   make(map[string]*sync.Mutex),
		perUser: make(map[string]*bucket),
		perSess: make(map[string]*bucket),
	}
}

// SetClock overrides the time source (tests).
func (r *Router) SetClock(now func() time.Time) { r.now = now }

// Decision is the outcome of routing a message.
type Decision int

const (
	Accepted            Decision = iota
	DroppedUnroutable            // no session key
	DroppedNotMentioned          // group message without a mention
	DroppedTooLong               // exceeds MaxContentByte
	RateLimited
)

func (d Decision) String() string {
	switch d {
	case Accepted:
		return "accepted"
	case DroppedUnroutable:
		return "unroutable"
	case DroppedNotMentioned:
		return "not_mentioned"
	case DroppedTooLong:
		return "too_long"
	case RateLimited:
		return "rate_limited"
	default:
		return "unknown"
	}
}

// Handler processes an accepted message under the session lock.
type Handler func(ctx context.Context, sessionKey string, msg InboundMessage) error

// RouteAndHandle applies all gates, then — if accepted — runs handler while
// holding the per-session lock (so same-session messages are strictly
// serialized). Returns the gate decision; handler errors are returned too.
func (r *Router) RouteAndHandle(ctx context.Context, msg InboundMessage, handler Handler) (Decision, error) {
	// Group mention gate (cron fires bypass it).
	if msg.ChannelType == ChannelGroup && !msg.Mentioned && !msg.CronFire {
		return DroppedNotMentioned, nil
	}

	key, err := msg.SessionKey()
	if err != nil {
		return DroppedUnroutable, nil
	}

	// Content size gate.
	if len(msg.Text) > r.cfg.MaxContentByte {
		return DroppedTooLong, nil
	}

	// Rate limiting (cron fires bypass it — operator-scheduled).
	if !msg.CronFire && !r.allow(key, msg.FromUID) {
		return RateLimited, nil
	}

	// Serialize within the session.
	lock := r.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	if err := handler(ctx, key, msg); err != nil {
		return Accepted, err
	}
	return Accepted, nil
}

func (r *Router) lockFor(key string) *sync.Mutex {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	m, ok := r.locks[key]
	if !ok {
		m = &sync.Mutex{}
		r.locks[key] = m
	}
	return m
}

// allow checks all three buckets; only consumes a token from each if ALL pass
// (peek-then-commit), mirroring cc-channel's checkAllRateLimits.
func (r *Router) allow(sessionKey, uid string) bool {
	r.rlMu.Lock()
	defer r.rlMu.Unlock()
	now := r.now()

	maxPer := r.cfg.MaxPerMinute
	if r.global == nil {
		r.global = newBucket(maxPer*globalRateMultiplier, time.Minute)
	}
	ub := r.perUser[uid]
	if ub == nil {
		ub = newBucket(maxPer, time.Minute)
		r.perUser[uid] = ub
	}
	sb := r.perSess[sessionKey]
	if sb == nil {
		sb = newBucket(maxPer, time.Minute)
		r.perSess[sessionKey] = sb
	}

	if !r.global.peek(now) || !ub.peek(now) || !sb.peek(now) {
		return false
	}
	r.global.take(now)
	ub.take(now)
	sb.take(now)
	return true
}
