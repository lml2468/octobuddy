package octoapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAddBotInputValidation covers the client-side guards that fire BEFORE
// any network call — each is a security check (uk_ prefix, URL scheme,
// non-empty name) and a clearer-error-than-server-401 quality-of-life win.
func TestAddBotInputValidation(t *testing.T) {
	cases := []struct {
		name, apiURL, apiKey, botName, errContains string
	}{
		{"empty url", "", "uk_abc", "Bot", "API URL"},
		{"http non-loopback", "http://example.com", "uk_abc", "Bot", "API URL"},
		{"ftp scheme", "ftp://example.com", "uk_abc", "Bot", "API URL"},
		{"empty key", "https://im.example.com", "", "Bot", "API Key"},
		{"wrong key prefix", "https://im.example.com", "bf_pasted_wrong_kind", "Bot", "uk_"},
		{"empty name", "http://127.0.0.1:1", "uk_abc", "  ", "Bot 名称"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AddBot(context.Background(), tc.apiURL, tc.apiKey, tc.botName)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("error should mention %q, got %v", tc.errContains, err)
			}
		})
	}
}

// TestAddBotHappyPath runs the full POST against an httptest server. The
// server URL is http://127.0.0.1:PORT — IsAllowedURL accepts loopback http
// (dev gateway path), so AssertPublicURL is skipped and the dial guard
// allows loopback. End-to-end without touching real octo-server.
func TestAddBotHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Endpoint shape + auth header presence are the contract with octo-server.
		if r.URL.Path != "/v1/user/bots" {
			t.Errorf("expected POST /v1/user/bots, got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer uk_test_key" {
			t.Errorf("Authorization = %q, want bearer uk_test_key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		if body["name"] != "Buddy" {
			t.Errorf("body.name = %v, want Buddy", body["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"robot_id":  "1234567890ab",
			"bot_token": "bf_realtoken",
		})
	}))
	defer srv.Close()

	got, err := AddBot(context.Background(), srv.URL, "uk_test_key", "Buddy")
	if err != nil {
		t.Fatalf("AddBot: %v", err)
	}
	if got.RobotID != "1234567890ab" || got.BotToken != "bf_realtoken" {
		t.Fatalf("got %+v", got)
	}
}

// TestAddBotRejectsHostileRobotID is the argv-injection regression: octo-
// cli later receives `--bot-id <robotID>` as argv, so a server that
// returns a flag-shaped id ("-config=…") must be rejected client-side. A
// leading dash slips through go-flag's parser as a flag for the previous
// arg or the command.
func TestAddBotRejectsHostileRobotID(t *testing.T) {
	for _, bad := range []string{
		"-config=/tmp/x",         // leading dash → flag-shaped
		".hidden",                // leading dot → addresses a hidden file in any future profile-dir
		"with space",             // whitespace breaks tokenization
		"with/slash",             // path separator
		"with;semi",              // shell metachar
		strings.Repeat("a", 129), // over the 128-char cap
		"",                       // empty
	} {
		t.Run(bad, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{
					"robot_id":  bad,
					"bot_token": "bf_ok",
				})
			}))
			defer srv.Close()
			_, err := AddBot(context.Background(), srv.URL, "uk_x", "Bot")
			if err == nil {
				t.Fatalf("must reject hostile robot_id %q", bad)
			}
		})
	}
}

// TestAddBotRejectsBotTokenWithoutPrefix: the server's bot_token feeds
// straight into the secret backend + later as a bearer. A token lacking
// the bf_ prefix is structurally wrong; we'd persist a useless string.
func TestAddBotRejectsBotTokenWithoutPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"robot_id":  "1234567890ab",
			"bot_token": "not_prefixed_token",
		})
	}))
	defer srv.Close()
	_, err := AddBot(context.Background(), srv.URL, "uk_x", "Bot")
	if err == nil || !strings.Contains(err.Error(), "bf_") {
		t.Fatalf("must reject bot_token without bf_ prefix, got %v", err)
	}
}

// TestAddBotRefusesRedirect: this POST carries the operator's uk_ bearer.
// Go strips Authorization only on cross-host redirects, so a same-host /
// sibling-subdomain 302 keeps it. The client's CheckRedirect returns
// ErrUseLastResponse so a 3xx is itself the failure signal — never
// silently followed with the bearer attached.
func TestAddBotRefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/v1/redirected")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	_, err := AddBot(context.Background(), srv.URL, "uk_x", "Bot")
	if err == nil || !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("must refuse 3xx, got %v", err)
	}
}

// TestAddBotSurfaces401AsKeyInvalid: a 401 from octo-server means the
// operator's uk_ is wrong/expired — message must be unambiguous so the
// wizard doesn't show a raw "HTTP 401".
func TestAddBotSurfaces401AsKeyInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"msg":"bad key"}`))
	}))
	defer srv.Close()
	_, err := AddBot(context.Background(), srv.URL, "uk_x", "Bot")
	if err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("401 should surface as friendly Key error, got %v", err)
	}
}

// TestAddBotSurfacesServerMsg: non-2xx responses with {"msg":"…"} should
// bubble the server's human-readable message into the wizard toast.
func TestAddBotSurfacesServerMsg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"msg":"username already taken"}`))
	}))
	defer srv.Close()
	_, err := AddBot(context.Background(), srv.URL, "uk_x", "Bot")
	if err == nil || !strings.Contains(err.Error(), "username already taken") {
		t.Fatalf("expected server msg surfaced, got %v", err)
	}
}

// TestAddBotRejectsIncompleteResponse: empty robot_id or empty bot_token
// from the server is wrong; better fail loudly than persist garbage.
func TestAddBotRejectsIncompleteResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"robot_id":"","bot_token":""}`))
	}))
	defer srv.Close()
	_, err := AddBot(context.Background(), srv.URL, "uk_x", "Bot")
	if err == nil || !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("expected incomplete-response error, got %v", err)
	}
}

// TestValidRobotIDRegex pins the exact slug grammar so a relax that lets
// dot-leading or dash-leading IDs through is loud (cf. the comment block
// on ValidRobotID).
func TestValidRobotIDRegex(t *testing.T) {
	ok := []string{"a", "abc123", "A_B-C.d", "1234567890ab", "underscore_start"}
	bad := []string{
		"",                       // empty
		".hidden",                // leading dot
		"-flag",                  // leading dash
		"with space",             // whitespace
		"with/slash",             // path sep
		"with;semi",              // shell metachar
		strings.Repeat("a", 129), // over cap
	}
	for _, s := range ok {
		if !ValidRobotID.MatchString(s) {
			t.Errorf("expected %q to be a valid robot id", s)
		}
	}
	for _, s := range bad {
		if ValidRobotID.MatchString(s) {
			t.Errorf("expected %q to be REJECTED as a robot id", s)
		}
	}
}

// TestSanitizeServerTextStripsControlChars is the anti-ANSI-escape guard:
// untrusted server text can't smuggle SGR / cursor-move sequences or NULs
// into the wizard toast. Tab and newline survive — they're plain
// whitespace, no rendering risk.
func TestSanitizeServerTextStripsControlChars(t *testing.T) {
	in := "hello\x1b[31m red \x1b[0m\x00\x07world\ttab\nnl"
	got := sanitizeServerText(in)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\x00") || strings.Contains(got, "\x07") {
		t.Fatalf("control chars survived: %q", got)
	}
	if !strings.Contains(got, "\t") || !strings.Contains(got, "\n") {
		t.Fatalf("TAB/LF should survive: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("body content lost: %q", got)
	}
}

// TestSanitizeServerTextClamps caps the result at 256 chars + ellipsis so
// a misbehaving server can't push a multi-megabyte string into the toast.
func TestSanitizeServerTextClamps(t *testing.T) {
	in := strings.Repeat("x", 1000)
	got := sanitizeServerText(in)
	if len(got) > 256+len("…") {
		t.Fatalf("len = %d, expected ≤ 256+…", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected trailing ellipsis, got %q", got[len(got)-10:])
	}
}

// TestServerMsgIgnoresGarbage: an unmarshal failure must return "" so
// downstream falls back to the generic "HTTP %d" message instead of
// crashing or panicking.
func TestServerMsgIgnoresGarbage(t *testing.T) {
	if got := serverMsg([]byte("not json at all")); got != "" {
		t.Fatalf("expected empty on garbage, got %q", got)
	}
	if got := serverMsg(nil); got != "" {
		t.Fatalf("expected empty on nil, got %q", got)
	}
}
