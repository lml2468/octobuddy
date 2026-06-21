package octocli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyChecksum(t *testing.T) {
	archive := []byte("octo-cli archive bytes")
	name := "octo-cli_0.6.0_darwin_arm64.tar.gz"
	good := sha256hex(archive)
	sums := []byte(fmt.Sprintf("%s  %s\nfeed00  other.tar.gz\n", good, name))

	t.Run("match passes", func(t *testing.T) {
		if err := verifyChecksum(sums, archive, name); err != nil {
			t.Fatalf("verifyChecksum: %v", err)
		}
	})
	t.Run("case-insensitive hex passes", func(t *testing.T) {
		up := []byte(fmt.Sprintf("%s  %s\n", strings.ToUpper(good), name))
		if err := verifyChecksum(up, archive, name); err != nil {
			t.Fatalf("verifyChecksum (uppercase): %v", err)
		}
	})
	t.Run("missing entry fails closed", func(t *testing.T) {
		if err := verifyChecksum([]byte("feed00  other.tar.gz\n"), archive, name); err == nil {
			t.Fatal("expected error when checksums.txt has no entry for the asset")
		}
	})
	t.Run("empty sums fails closed", func(t *testing.T) {
		if err := verifyChecksum(nil, archive, name); err == nil {
			t.Fatal("expected error when checksums.txt is empty")
		}
	})
	t.Run("mismatch fails", func(t *testing.T) {
		if err := verifyChecksum(sums, []byte("tampered"), name); err == nil {
			t.Fatal("expected error on checksum mismatch")
		}
	})
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.6.0", "v0.6.0", 0},
		{"v0.7.0", "v0.6.0", 1},
		{"0.6.0", "0.6.1", -1},
		{"v1.0.0", "v0.9.9", 1},
		{"v0.6.0", "", 1}, // installed unknown → bundle is "newer"
		{"v0.6", "v0.6.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetNameStripsV(t *testing.T) {
	// The asset name uses the bare version (no leading v) per GoReleaser.
	got := assetName("v0.6.0")
	if len(got) == 0 || got[:9] != "octo-cli_" {
		t.Fatalf("unexpected asset name: %q", got)
	}
	if bytes.Contains([]byte(got), []byte("_v0.6.0_")) {
		t.Errorf("asset name should strip the leading v: %q", got)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "abc123  octo-cli_0.6.0_darwin_arm64.tar.gz\ndef456  octo-cli_0.6.0_linux_amd64.tar.gz\n"
	if got := checksumFor([]byte(sums), "octo-cli_0.6.0_linux_amd64.tar.gz"); got != "def456" {
		t.Errorf("checksumFor = %q want def456", got)
	}
	if got := checksumFor([]byte(sums), "missing.tar.gz"); got != "" {
		t.Errorf("checksumFor(missing) = %q want empty", got)
	}
}

func TestExtractTarGz(t *testing.T) {
	want := []byte("#!/bin/sh\necho octo\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// a decoy file + the real binary at archive root
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"LICENSE", []byte("Apache-2.0")},
		{binName(), want}, // per-OS name (…/octo-cli or octo-cli.exe) so the lookup matches on Windows too
	} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data))})
		_, _ = tw.Write(f.data)
	}
	tw.Close()
	gz.Close()

	got, err := extractBinary(buf.Bytes(), "octo-cli_0.6.0_darwin_arm64.tar.gz")
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extracted %q want %q", got, want)
	}
}

// runUpgrade points the package's HTTP seams at an httptest server and HOME at a
// temp dir (so Dir()/BinPath() never touch the real ~/.xclaw), then runs Upgrade.
func runUpgrade(t *testing.T, h http.HandlerFunc) (string, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	oldBase, oldClient := apiBase, httpClient
	apiBase, httpClient = srv.URL, srv.Client()
	t.Cleanup(func() { apiBase, httpClient = oldBase, oldClient })
	t.Setenv("HOME", t.TempDir())
	return Upgrade(context.Background())
}

// A release that ships no checksums.txt must abort BEFORE any download — the
// binary lands on the agent's PATH, so an unverifiable release is never installed.
func TestUpgradeFailsClosedWithoutChecksums(t *testing.T) {
	want := assetName("v9.9.9")
	_, err := runUpgrade(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			base := "http://" + r.Host
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{
					{"name": want, "browser_download_url": base + "/dl/asset"},
					// deliberately no checksums.txt
				},
			})
			return
		}
		t.Errorf("unexpected request to %s — Upgrade must abort before downloading", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	if err == nil {
		t.Fatal("Upgrade must fail closed when the release ships no checksums.txt")
	}
	if !strings.Contains(err.Error(), "checksums.txt") {
		t.Errorf("error should mention checksums.txt, got: %v", err)
	}
	if isFile(BinPath()) {
		t.Error("no binary must be installed when verification is impossible")
	}
}

// If checksums.txt exists but can't be fetched, Upgrade must abort (not fall
// back to installing the unverified, already-downloaded archive).
func TestUpgradeAbortsWhenChecksumsDownloadFails(t *testing.T) {
	want := assetName("v9.9.9")
	_, err := runUpgrade(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			base := "http://" + r.Host
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{
					{"name": want, "browser_download_url": base + "/dl/asset"},
					{"name": "checksums.txt", "browser_download_url": base + "/dl/checksums"},
				},
			})
		case r.URL.Path == "/dl/asset":
			_, _ = w.Write([]byte("archive-bytes")) // asset download succeeds…
		case r.URL.Path == "/dl/checksums":
			http.Error(w, "boom", http.StatusInternalServerError) // …but checksums fails
		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	})
	if err == nil {
		t.Fatal("Upgrade must abort when checksums.txt cannot be downloaded")
	}
	if isFile(BinPath()) {
		t.Error("no binary must be installed when the checksums download fails")
	}
}

func TestLoginRequiresRobotIDAndToken(t *testing.T) {
	if err := Login(context.Background(), "", "bf_x", "https://x/api"); err == nil {
		t.Error("Login with empty robotID must error")
	}
	if err := Login(context.Background(), "bot_x", "", "https://x/api"); err == nil {
		t.Error("Login with empty token must error")
	}
}

func TestLoginMissingBinaryErrors(t *testing.T) {
	// Point HOME at an empty temp dir so BinPath() resolves to a path that
	// doesn't exist; Login must refuse with a clear error rather than spawn.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	err := Login(context.Background(), "bot_x", "bf_x", "https://x/api")
	if err == nil {
		t.Fatal("Login with no installed binary must error")
	}
}

func TestLogoutNoOpOnAbsentBinary(t *testing.T) {
	// Without a binary OR a profile, Logout must be a clean no-op so callers
	// (e.g. bot-delete path) can run it unconditionally.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := Logout(context.Background(), "bot_x"); err != nil {
		t.Errorf("Logout with no binary should be no-op, got %v", err)
	}
	if err := Logout(context.Background(), ""); err != nil {
		t.Errorf("Logout with empty robotID should be no-op, got %v", err)
	}
}
