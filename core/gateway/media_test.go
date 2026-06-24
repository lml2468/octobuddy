package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lml2468/octobuddy/core/router"
)

// pngBytes is a tiny valid-enough PNG header blob (content is never decoded by
// the downloader — only the content-type header gates acceptance).
var pngBytes = []byte("\x89PNG\r\n\x1a\nfake-image-data")

// loopbackMediaClient is a media HTTP client without the production dial guard,
// so httptest servers on 127.0.0.1 are reachable. It keeps the manual-redirect
// CheckRedirect contract fetchGuarded depends on. The dial guard itself is
// exercised by the SSRF tests, which use the real client.
func loopbackMediaClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// testGW builds a Gateway whose SSRF guard is permissive, so httptest servers
// (which bind to 127.0.0.1 and would otherwise be rejected as loopback) can be
// reached. The real config.AssertPublicURL is exercised separately in the SSRF
// tests below, which deliberately do NOT use this helper.
func testGW() *Gateway {
	return &Gateway{
		assertPublic: func(context.Context, string) error { return nil },
		mediaClient:  loopbackMediaClient(),
	}
}

func TestMaterializeImage_LandsInCwd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	g := testGW()
	atts := []router.Attachment{{Kind: router.AttachmentImage, URL: srv.URL + "/img"}}
	hint := g.materializeAttachments(context.Background(), cwd, atts)

	if !strings.Contains(hint, "已下载图片到本地") || !strings.Contains(hint, "请用 Read 工具") {
		t.Fatalf("hint missing Read marker: %q", hint)
	}
	if !strings.Contains(hint, InboundMediaDir) {
		t.Fatalf("hint rel path not under media dir: %q", hint)
	}
	// The file must exist on disk under <cwd>/.octobuddy-media.
	entries, err := os.ReadDir(filepath.Join(cwd, InboundMediaDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 file in media dir, got %v err=%v", entries, err)
	}
	if !strings.HasSuffix(entries[0].Name(), ".png") {
		t.Fatalf("expected .png extension, got %q", entries[0].Name())
	}
	got, _ := os.ReadFile(filepath.Join(cwd, InboundMediaDir, entries[0].Name()))
	if string(got) != string(pngBytes) {
		t.Fatalf("downloaded bytes mismatch")
	}
}

func TestMaterializeImage_PluralHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpegdata"))
	}))
	defer srv.Close()

	cwd := t.TempDir()
	g := testGW()
	atts := []router.Attachment{
		{Kind: router.AttachmentImage, URL: srv.URL + "/a"},
		{Kind: router.AttachmentImage, URL: srv.URL + "/b"},
	}
	hint := g.materializeAttachments(context.Background(), cwd, atts)
	if !strings.Contains(hint, "已下载 2 张图片到本地") {
		t.Fatalf("expected plural hint, got %q", hint)
	}
}

func TestMaterializeImage_CapEnforced(t *testing.T) {
	big := make([]byte, maxImageBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	g := testGW()
	atts := []router.Attachment{{Kind: router.AttachmentImage, URL: srv.URL}}
	hint := g.materializeAttachments(context.Background(), cwd, atts)
	if hint != "" {
		t.Fatalf("oversized image should produce no hint, got %q", hint)
	}
	// The partial file must have been removed.
	entries, _ := os.ReadDir(filepath.Join(cwd, InboundMediaDir))
	if len(entries) != 0 {
		t.Fatalf("oversized download left a file: %v", entries)
	}
}

func TestMaterializeImage_RejectsNonImageType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	g := testGW()
	atts := []router.Attachment{{Kind: router.AttachmentImage, URL: srv.URL}}
	if hint := g.materializeAttachments(context.Background(), t.TempDir(), atts); hint != "" {
		t.Fatalf("non-image type should be rejected, got %q", hint)
	}
}

func TestMaterializeImage_MaxImagesCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	cwd := t.TempDir()
	g := testGW()
	var atts []router.Attachment
	for i := 0; i < maxImagesPerMessage+3; i++ {
		atts = append(atts, router.Attachment{Kind: router.AttachmentImage, URL: fmt.Sprintf("%s/%d", srv.URL, i)})
	}
	g.materializeAttachments(context.Background(), cwd, atts)
	entries, _ := os.ReadDir(filepath.Join(cwd, InboundMediaDir))
	if len(entries) != maxImagesPerMessage {
		t.Fatalf("expected %d images materialized, got %d", maxImagesPerMessage, len(entries))
	}
}

func TestMaterializeImage_NoCwdSkips(t *testing.T) {
	g := &Gateway{}
	atts := []router.Attachment{{Kind: router.AttachmentImage, URL: "https://example.com/x.png"}}
	if hint := g.materializeAttachments(context.Background(), "", atts); hint != "" {
		t.Fatalf("no-cwd should skip images, got %q", hint)
	}
}

// TestMaterialize_DownloadsConcurrently proves the async fix: N slow downloads
// complete in ~one delay (parallel) rather than N×delay (the old sequential path
// that could block the turn for downloadTimeout × N before driver.Query).
func TestMaterialize_DownloadsConcurrently(t *testing.T) {
	const delay = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	g := testGW()
	var atts []router.Attachment
	for i := 0; i < mediaConcurrency; i++ { // all run at once under the bound
		atts = append(atts, router.Attachment{Kind: router.AttachmentImage, URL: fmt.Sprintf("%s/%d", srv.URL, i)})
	}
	start := time.Now()
	g.materializeAttachments(context.Background(), cwd, atts)
	elapsed := time.Since(start)
	// Sequential would be ~mediaConcurrency*delay; concurrent should be ~1*delay.
	if elapsed > 3*delay {
		t.Fatalf("downloads not concurrent: %v (sequential would be ~%v)", elapsed, time.Duration(mediaConcurrency)*delay)
	}
	entries, _ := os.ReadDir(filepath.Join(cwd, InboundMediaDir))
	if len(entries) != mediaConcurrency {
		t.Fatalf("expected %d images, got %d", mediaConcurrency, len(entries))
	}
}

func TestResolveFile_InlinesSmallText(t *testing.T) {
	content := []byte("package main\nfunc main() {}\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	g := testGW()
	att := router.Attachment{Kind: router.AttachmentFile, URL: srv.URL + "/main.go", Name: "main.go"}
	hint := g.materializeAttachments(context.Background(), t.TempDir(), []router.Attachment{att})

	if !strings.Contains(hint, `<file_content name="main.go" encoding="base64"`) {
		t.Fatalf("expected base64 file_content wrapper, got %q", hint)
	}
	b64 := base64.StdEncoding.EncodeToString(content)
	if !strings.Contains(hint, b64) {
		t.Fatalf("expected base64 content %q in %q", b64, hint)
	}
	if !strings.Contains(hint, fmt.Sprintf(`bytes="%d"`, len(content))) {
		t.Fatalf("expected bytes attribute, got %q", hint)
	}
}

func TestResolveFile_LargeTextDownloadsToCwd(t *testing.T) {
	content := make([]byte, inlineFileMaxBytes+100)
	for i := range content {
		content[i] = 'a'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	g := testGW()
	att := router.Attachment{Kind: router.AttachmentFile, URL: srv.URL + "/big.txt", Name: "big.txt"}
	hint := g.materializeAttachments(context.Background(), cwd, []router.Attachment{att})

	if !strings.Contains(hint, "本地路径:") {
		t.Fatalf("expected local-path hint, got %q", hint)
	}
	entries, _ := os.ReadDir(filepath.Join(cwd, InboundMediaDir))
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "big.txt") {
		t.Fatalf("expected big.txt downloaded, got %v", entries)
	}
}

func TestResolveFile_BinaryDescribedNotDownloaded(t *testing.T) {
	g := &Gateway{}
	att := router.Attachment{Kind: router.AttachmentFile, URL: "https://example.com/x.bin", Name: "x.bin", Size: 1234}
	hint := g.materializeAttachments(context.Background(), t.TempDir(), []router.Attachment{att})
	if !strings.Contains(hint, "[文件: x.bin (1.2KB)]") {
		t.Fatalf("expected size description for binary, got %q", hint)
	}
}

func TestResolveFile_KnownOversizeSkipsDownload(t *testing.T) {
	g := &Gateway{}
	att := router.Attachment{Kind: router.AttachmentFile, URL: "https://example.com/x.txt", Name: "x.txt", Size: maxFileDownloadBytes + 1}
	hint := g.materializeAttachments(context.Background(), t.TempDir(), []router.Attachment{att})
	if !strings.Contains(hint, "超过下载上限") {
		t.Fatalf("expected over-cap description, got %q", hint)
	}
}

func TestFetchGuarded_SSRFRejected(t *testing.T) {
	g := &Gateway{}
	// A loopback URL must be refused before any network I/O.
	_, err := g.fetchGuarded(context.Background(), "http://127.0.0.1:1/secret")
	if err == nil || !strings.Contains(err.Error(), "private/local") {
		t.Fatalf("expected SSRF rejection, got %v", err)
	}
}

// TestDialGuard_RejectsRebindToPrivate proves the H1 DNS-rebinding fix: even when
// the policy check (assertPublic) is bypassed, the production transport's dial
// guard refuses a connection whose resolved address is private/local. This is the
// authoritative check that runs on the exact IP being dialed, defeating a DNS
// answer that passed the earlier lookup but resolves to a private IP at dial time.
func TestDialGuard_RejectsRebindToPrivate(t *testing.T) {
	g := &Gateway{
		// Policy check bypassed (simulating a hostname that resolved public once)…
		assertPublic: func(context.Context, string) error { return nil },
		// …but the real hardened client is used, so the dial guard still fires.
	}
	_, err := g.fetchGuarded(context.Background(), "http://127.0.0.1:9/x")
	if err == nil || !strings.Contains(err.Error(), "private/local") {
		t.Fatalf("dial guard should reject private target even when policy bypassed, got %v", err)
	}
}

func TestMaterializeFile_SSRFRejectionDegrades(t *testing.T) {
	g := &Gateway{}
	att := router.Attachment{Kind: router.AttachmentFile, URL: "http://169.254.169.254/latest/meta-data/", Name: "meta.txt"}
	hint := g.materializeAttachments(context.Background(), t.TempDir(), []router.Attachment{att})
	if !strings.Contains(hint, "拒绝下载") {
		t.Fatalf("expected metadata-IP rejection description, got %q", hint)
	}
}
