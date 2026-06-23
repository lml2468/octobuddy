package octo

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRESTGetUserInfo asserts the happy path: a 200 with {"name":"…"} returns
// the name; the request goes to the right URL with the uid query-escaped.
func TestRESTGetUserInfo(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"uid":"u1","name":"Alice"}`))
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, func() string { return "tk" })
	if got := c.GetUserInfo(t.Context(), "u1"); got != "Alice" {
		t.Fatalf("GetUserInfo = %q, want %q", got, "Alice")
	}
	if want := "/v1/bot/user/info?uid=u1"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
}

// TestRESTGetUserInfo404 proves the documented 404→"" soft-degrade — a missing
// uid is a normal not-found, not an error to log.
func TestRESTGetUserInfo404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, func() string { return "tk" })
	if got := c.GetUserInfo(t.Context(), "u1"); got != "" {
		t.Fatalf("GetUserInfo on 404 = %q, want empty", got)
	}
}

// TestRESTGetGroupInfo asserts the happy path for groups.
func TestRESTGetGroupInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"group_no":"g1","name":"Engineering"}`))
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, func() string { return "tk" })
	if got := c.GetGroupInfo(t.Context(), "g1"); got != "Engineering" {
		t.Fatalf("GetGroupInfo = %q, want %q", got, "Engineering")
	}
}

// TestNameCacheLearnUserNoRESTCall asserts the free-feed contract: LearnUser
// seeds the cache directly so ResolveUser never issues a request. A test
// server that fails the test on any hit proves the inbound message stream
// short-circuits the lookup for the common case (sender names already arrive
// on every BotMessage).
func TestNameCacheLearnUserNoRESTCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected REST call: %s", r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	nc := newNameCache(NewRESTClient(srv.URL, func() string { return "tk" }))
	nc.LearnUser("u1", "Alice")
	if got := nc.ResolveUser("u1"); got != "Alice" {
		t.Fatalf("ResolveUser after LearnUser = %q, want %q", got, "Alice")
	}
}

// TestNameCacheResolveChannelLazyFetch proves the miss-then-cache shape: the
// first ResolveChannel returns "" but kicks a REST fetch; once that completes,
// the cache holds the resolved name and the second call returns it without
// another request.
func TestNameCacheResolveChannelLazyFetch(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"group_no":"g1","name":"Eng"}`))
	}))
	defer srv.Close()
	nc := newNameCache(NewRESTClient(srv.URL, func() string { return "tk" }))

	if got := nc.ResolveChannel("g1"); got != "" {
		t.Fatalf("first ResolveChannel = %q, want empty (lazy fetch)", got)
	}
	// Wait for the background fetch to populate the cache. The fetch runs in a
	// goroutine with its own ctx; poll with a tight cap rather than sleep.
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = nc.ResolveChannel("g1"); got == "Eng" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != "Eng" {
		t.Fatalf("ResolveChannel never resolved within deadline: %q", got)
	}
	// One more call should NOT re-hit the server — the entry is positive-cached
	// indefinitely. We may have raced a few extra calls during the poll loop;
	// the strict assertion is "≥1 hit", and the prior assertions prove caching
	// occurred (otherwise we'd loop until deadline with hits>>1).
	if atomic.LoadInt32(&hits) < 1 {
		t.Fatalf("expected ≥1 REST hit, got %d", hits)
	}
	before := atomic.LoadInt32(&hits)
	for i := 0; i < 5; i++ {
		nc.ResolveChannel("g1")
	}
	if got := atomic.LoadInt32(&hits); got != before {
		t.Fatalf("post-cache ResolveChannel issued extra REST calls: before=%d after=%d", before, got)
	}
}

// TestNameCacheResolveChannelThreadCompound proves a thread channel id
// "<groupNo>____<shortId>" fetches the THREAD's own name via the threads
// endpoint, AND simultaneously warms the parent group so a downstream
// composition (sidebar / chat-header) has both halves.
func TestNameCacheResolveChannelThreadCompound(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/bot/groups/g1/threads/topic9":
			_, _ = w.Write([]byte(`{"name":"Bug Triage"}`))
		case "/v1/bot/groups/g1":
			_, _ = w.Write([]byte(`{"name":"Engineering"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	nc := newNameCache(NewRESTClient(srv.URL, func() string { return "tk" }))
	nc.ResolveChannel("g1" + ThreadIDSeparator + "topic9")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		thread := hits["/v1/bot/groups/g1/threads/topic9"]
		parent := hits["/v1/bot/groups/g1"]
		mu.Unlock()
		if thread > 0 && parent > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/v1/bot/groups/g1/threads/topic9"] != 1 {
		t.Fatalf("thread endpoint hit count = %d, want 1", hits["/v1/bot/groups/g1/threads/topic9"])
	}
	if hits["/v1/bot/groups/g1"] != 1 {
		t.Fatalf("parent group endpoint hit count = %d, want 1 (parent warmed in parallel)", hits["/v1/bot/groups/g1"])
	}
}

// TestNameCacheNegativeTTLReFetch proves a 404 (cached as empty name) re-fetches
// after the negative TTL elapses — a group that gets a name later eventually
// shows up without a daemon restart. We shrink negativeTTL for the test.
func TestNameCacheNegativeTTLReFetch(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	nc := newNameCache(NewRESTClient(srv.URL, func() string { return "tk" }))

	// Force the first fetch (and wait for it).
	nc.ResolveChannel("g1")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&hits) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if hits := atomic.LoadInt32(&hits); hits == 0 {
		t.Fatal("first fetch never occurred")
	}
	// Backdate the cached negative entry past the TTL so the next call re-fetches.
	nc.mu.Lock()
	e := nc.channels["g1"]
	e.fetchedAt = time.Now().Add(-2 * negativeTTL)
	nc.channels["g1"] = e
	nc.mu.Unlock()

	before := atomic.LoadInt32(&hits)
	nc.ResolveChannel("g1") // should kick a re-fetch
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&hits) == before {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hits); got <= before {
		t.Fatalf("expired negative entry did not re-fetch: hits stayed at %d", got)
	}
}
