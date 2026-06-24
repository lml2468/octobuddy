package octo

import (
	"context"
	"sync"
	"time"
)

// PrewarmChannels synchronously resolves channel names for the given channel
// ids, capped by timeout. Used by sessions.list to dodge the cold-start
// "every row shows its bare id" first paint: ChannelName by itself returns
// "" on miss and kicks a background fetch the NEXT call sees populated —
// but the GUI only re-issues sessions.list when the bot is switched, so
// without prewarm names never appear on the first listing.
//
// Each id is fetched via the right endpoint per shape (GetGroupInfo for bare
// groups, GetThreadInfo for thread compounds). Thread ids ALSO warm their
// parent group_no so the "<ThreadName>(<GroupName>)" composition that
// projection layers do has both halves available at the same time.
//
// DM peer names are warmed by the sister PrewarmUsers — they need it too
// for any session that's had no inbound this restart (LearnUser only
// seeds from live inbound messages).
func (c *nameCache) PrewarmChannels(channelIDs []string, timeout time.Duration) {
	var groups, threads []string
	parents := map[string]struct{}{}
	for _, ch := range channelIDs {
		if ch == "" {
			continue
		}
		if IsThreadChannelID(ch) {
			threads = append(threads, ch)
			parents[ExtractParentGroupNo(ch)] = struct{}{}
		} else {
			groups = append(groups, ch)
		}
	}
	for p := range parents {
		groups = append(groups, p)
	}
	// One shared timeout — both phases run in parallel since they hit
	// independent endpoints and share the prewarmConcurrency pool.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.prewarm(groups, timeout, "c:", NameKindChannel, identity, c.channels, c.rest.GetGroupInfo)
	}()
	go func() {
		defer wg.Done()
		c.prewarm(threads, timeout, "c:", NameKindChannel, identity, c.channels, func(ctx context.Context, id string) string {
			return c.rest.GetThreadInfo(ctx, ExtractParentGroupNo(id), extractThreadShortID(id))
		})
	}()
	wg.Wait()
}

func identity(s string) string { return s }

// PrewarmUsers is the user-side sister of PrewarmChannels — synchronously
// fetches display names for any uids not already in cache. Sessions.list
// uses it so DM sidebar rows show the peer's name on first paint even when
// the session has had no inbound this restart to free-feed the cache.
func (c *nameCache) PrewarmUsers(uids []string, timeout time.Duration) {
	c.prewarm(uids, timeout, "u:", NameKindUser, identity, c.users, c.rest.GetUserInfo)
}

// prewarm is the shared body for PrewarmChannels/PrewarmUsers: dedup against
// the cache and the inflight set, then fire parallel fetches and wait up to
// `timeout` for them. The wait is decoupled from each fetch's deadline: the
// fetches use their OWN longer ctx (prewarmFetchTimeout) so a slow API that
// would have succeeded in 3 s isn't punished by the caller's 1.5 s wait
// budget — under the old shared-ctx shape every still-running goroutine saw
// the cancelled ctx, returned "", and poisoned the negative-cache for the
// full negativeTTL, sticking the sidebar at bare ids for minutes.
//
// Fetches run with at most prewarmConcurrency in flight at once so a session
// list with N rows doesn't fanout N parallel REST calls — a 50-group bot was
// otherwise firing 50 concurrent requests at the name service. Excess fetches
// queue on a buffered channel and run as slots free up.
func (c *nameCache) prewarm(
	keys []string,
	timeout time.Duration,
	prefix string,
	kind NameKind,
	normalize func(string) string,
	bucket map[string]nameEntry,
	fetch func(context.Context, string) string,
) {
	if len(keys) == 0 || c.rest == nil {
		return
	}
	want := make(map[string]struct{}, len(keys))
	c.mu.Lock()
	for _, k := range keys {
		if k == "" {
			continue
		}
		nk := normalize(k)
		cacheKey := prefix + nk
		if !c.shouldPrewarmKey(cacheKey, nk, bucket) {
			continue
		}
		want[nk] = struct{}{}
		c.inflight[cacheKey] = struct{}{}
	}
	c.mu.Unlock()
	if len(want) == 0 {
		return
	}

	done := make(chan struct{})
	sem := make(chan struct{}, prewarmConcurrency)
	var wg sync.WaitGroup
	for nk := range want {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c.prewarmOne(kind, k, bucket, prefix+k, fetch)
		}(nk)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (c *nameCache) prewarmOne(kind NameKind, key string, bucket map[string]nameEntry, cacheKey string, fetch func(context.Context, string) string) {
	// Fetch ctx is independent of the caller's wait budget; the caller may walk
	// away, but the result still lands in cache for the next caller.
	ctx, cancel := context.WithTimeout(context.Background(), prewarmFetchTimeout)
	defer cancel()
	c.storeName(kind, key, bucket, cacheKey, fetch(ctx, key))
}

func (c *nameCache) shouldPrewarmKey(cacheKey, key string, bucket map[string]nameEntry) bool {
	if _, busy := c.inflight[cacheKey]; busy {
		return false
	}
	if e, ok := bucket[key]; ok {
		return e.name == "" && time.Since(e.fetchedAt) >= negativeTTL
	}
	return true
}

// prewarmConcurrency caps the in-flight prewarm fetches per nameCache. Picked
// to be enough to populate a typical session-list quickly without fanning out
// hundreds of REST calls at once on a bot with many groups.
const prewarmConcurrency = 8

// prewarmFetchTimeout bounds each individual REST call kicked by prewarm.
// Larger than the caller's wait budget by design (see prewarm doc): the
// caller walks away on timeout, the fetch keeps running and seeds the cache.
const prewarmFetchTimeout = 5 * time.Second
