// Package claudecli installs and upgrades the claude CLI for the
// desktop app. claude is the agent the gateway spawns; we keep it at
// ~/.octobuddy/bin/claude (writable, so the tray's "Update claude" can
// replace it without admin / npm).
//
// Unlike octocli, we do NOT ship claude inside the .app — Anthropic's
// CLI has no declared open-source license so redistribution is unclear.
// EnsureInstalled fetches the latest release on first launch (sha-
// verified against the release's SHASUMS256.txt). The daemon's
// claude-bin resolver falls back to PATH while the download runs.
package claudecli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/desktop/internal/safehttp"
)

const repo = "anthropics/claude-code"

// userAgent identifies traffic so GitHub anti-abuse returns clearer
// errors than "blank UA".
const userAgent = "octobuddy-desktop/claudecli (+https://github.com/lml2468/octobuddy)"

// Injectable seams for tests. Production uses the real GitHub API over
// the hardened HTTP client; tests point these at an httptest server.
var (
	httpClient = safehttp.NewClient(safehttp.Options{Tag: "claudecli"})
	apiBase    = "https://api.github.com"
)

// skipInstallEnv suppresses the background fetch. Operators who manage
// claude separately (Homebrew, npm, corporate-managed PATH install) can
// opt out so we don't burn 200 MB of bandwidth they didn't ask for.
const skipInstallEnv = "OCTOBUDDY_SKIP_CLAUDE_INSTALL"

// installing is set while a fetch is in flight so the tray can render
// "downloading…" without racing the goroutine.
var installing atomic.Bool

// installListeners are invoked (under installListenersMu) whenever the
// install state transitions. The tray subscribes so its label refreshes
// when a background fetch completes — without polling.
var (
	installListenersMu sync.Mutex
	installListeners   []func()
)

// OnInstallStateChange registers a callback fired on every transition
// of Installing(). Used by the tray to refresh its label.
func OnInstallStateChange(fn func()) {
	installListenersMu.Lock()
	installListeners = append(installListeners, fn)
	installListenersMu.Unlock()
}

func notifyInstallState() {
	installListenersMu.Lock()
	fns := append([]func(){}, installListeners...)
	installListenersMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "claude.exe"
	}
	return "claude"
}

// Dir is ~/.octobuddy/bin — writable install dir; the daemon prefers
// this path over PATH for claude lookup.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octobuddy", "bin")
}

// BinPath is ~/.octobuddy/bin/claude.
func BinPath() string { return filepath.Join(Dir(), binName()) }

// ResolvedBinPath mirrors the daemon's resolveClaudeBin: the desktop-managed
// binary at ~/.octobuddy/bin/claude when it is a usable regular file, else
// "claude" on PATH. Callers that spawn claude (e.g. the toolset probe) MUST use
// this rather than BinPath() so an operator who manages claude on PATH
// (OCTOBUDDY_SKIP_CLAUDE_INSTALL) is probed against the same binary the daemon
// actually runs — otherwise the probe always fails and the GUI tool picker is
// unusable while turns work fine.
func ResolvedBinPath() string {
	if isFile(BinPath()) {
		return BinPath()
	}
	return binName()
}

// InstalledVersion returns the recorded version of the installed binary,
// or "" if claude isn't installed here. SafeRead refuses a symlinked
// version file so an agent-planted symlink can't surface arbitrary
// content in the tray as a "version" string.
func InstalledVersion() string {
	if !isFile(BinPath()) {
		return ""
	}
	b, err := safepath.SafeRead(Dir(), "claude.version", 256)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Installing reports whether a background install/upgrade is running.
func Installing() bool { return installing.Load() }

// writeVersion records the install tag. Returns the error so the caller
// can surface a failed sidecar write (which would otherwise leave the
// tray label saying "not installed" with a usable binary on disk).
func writeVersion(v string) error {
	return safepath.SafeWrite(Dir(), "claude.version", []byte(strings.TrimSpace(v)+"\n"), 0o644)
}

// EnsureInstalled kicks off a background fetch when claude isn't already
// at BinPath(). Non-blocking: returns immediately so the daemon can come
// up on PATH-installed claude (if any) while the ~200 MB download runs.
// A second call while a fetch is in flight is a no-op.
//
// Set OCTOBUDDY_SKIP_CLAUDE_INSTALL=1 to suppress the fetch entirely
// for operators who manage claude separately.
func EnsureInstalled() {
	if os.Getenv(skipInstallEnv) != "" {
		return
	}
	if isFile(BinPath()) {
		return
	}
	if !installing.CompareAndSwap(false, true) {
		return // already running
	}
	notifyInstallState()
	go func() {
		defer func() {
			installing.Store(false)
			notifyInstallState()
		}()
		// recover() so a panic in archive/tar / archive/zip / gzip /
		// json.Decoder (all run over network-supplied bytes here) can't
		// take down the whole desktop process. Log + clear the flag.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[claudecli] background install panicked: %v\n", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ver, err := install(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[claudecli] background install failed: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "[claudecli] installed claude %s at %s\n", ver, BinPath())
	}()
}

// Upgrade runs the same fetch as EnsureInstalled but synchronously and
// overwrites the existing binary. Returns the version installed.
func Upgrade(ctx context.Context) (string, error) {
	if !installing.CompareAndSwap(false, true) {
		return "", fmt.Errorf("an install/upgrade is already in flight")
	}
	notifyInstallState()
	defer func() {
		installing.Store(false)
		notifyInstallState()
	}()
	return install(ctx)
}

// install fetches the latest release for this OS+arch, verifies the
// sha256 against the release's SHASUMS256.txt, extracts the binary, and
// atomically replaces ~/.octobuddy/bin/claude. Returns the tag installed.
func install(ctx context.Context) (string, error) {
	rel, err := latestRelease(ctx)
	if err != nil {
		return "", err
	}
	want := assetName()
	var assetURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			assetURL = a.URL
		case "SHASUMS256.txt":
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		return "", fmt.Errorf("no claude asset %q in release %s", want, rel.TagName)
	}
	if sumsURL == "" {
		return "", fmt.Errorf("release %s has no SHASUMS256.txt; refusing to install unverified binary", rel.TagName)
	}

	archive, err := download(ctx, assetURL)
	if err != nil {
		return "", err
	}
	sums, err := download(ctx, sumsURL)
	if err != nil {
		return "", fmt.Errorf("claudecli: download SHASUMS256.txt: %w", err)
	}
	if err := verifyChecksum(sums, archive, want); err != nil {
		return "", err
	}
	bin, err := extractBinary(archive, want)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	if err := safepath.SafeMkdirAll(home, ".octobuddy/bin", 0o755); err != nil {
		return "", err
	}
	// Snapshot the current binary by RENAME — atomic, zero RAM, real
	// rollback target. (The previous read-into-buffer-then-write pattern
	// peaked at ~400 MiB heap per Upgrade and silently failed on a
	// future binary >256 MiB.) os.Rename errors are non-fatal: a
	// missing prior binary (first install) returns ENOENT; we just
	// proceed.
	_ = os.Rename(BinPath(), BinPath()+".prev")
	if err := safepath.SafeWriteAbs(BinPath(), bin, 0o700); err != nil {
		return "", err
	}
	if err := writeVersion(rel.TagName); err != nil {
		// Binary IS installed but the sidecar didn't land — the tray
		// label would lie ("not installed") and EnsureInstalled would
		// skip on the next launch. Surface the failure so the operator
		// can free disk and retry.
		return rel.TagName, fmt.Errorf("claudecli: binary installed but version sidecar write failed: %w", err)
	}
	return rel.TagName, nil
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func latestRelease(ctx context.Context) (ghRelease, error) {
	var r ghRelease
	url := apiBase + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return r, fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}
	// Cap the response so a hostile or buggy upstream can't allocate
	// unbounded memory parsing a giant assets array.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return r, err
	}
	if r.TagName == "" {
		return r, fmt.Errorf("github releases: no tag in latest release")
	}
	return r, nil
}

// assetName follows anthropics/claude-code's convention:
// claude-<os>-<arch>.tar.gz (or .zip on windows). Linux defaults to
// glibc (the musl variant is for Alpine).
func assetName() string {
	osTag := runtime.GOOS
	if osTag == "windows" {
		osTag = "win32"
	}
	archTag := runtime.GOARCH
	if archTag == "amd64" {
		archTag = "x64"
	}
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("claude-%s-%s.zip", osTag, archTag)
	}
	return fmt.Sprintf("claude-%s-%s.tar.gz", osTag, archTag)
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	// 256 MiB cap covers a ~75 MB compressed asset with headroom for growth.
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verifyChecksum enforces that SHASUMS256.txt contains an entry for
// filename and that the archive's sha256 matches it. Fails closed: a
// missing entry is an error, never a silent skip.
func verifyChecksum(sums, archive []byte, filename string) error {
	exp := checksumFor(sums, filename)
	if exp == "" {
		return fmt.Errorf("claudecli: no checksum entry for %s in SHASUMS256.txt", filename)
	}
	if got := sha256hex(archive); !strings.EqualFold(exp, got) {
		return fmt.Errorf("claudecli checksum mismatch for %s: want %s got %s", filename, exp, got)
	}
	return nil
}

// checksumFor finds the sha256 for filename in a SHASUMS256.txt
// ("<sha256>  <filename>" per line — note the double-space, which
// strings.Fields collapses anyway).
func checksumFor(sums []byte, filename string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == filename {
			return f[0]
		}
	}
	return ""
}

func extractBinary(archive []byte, name string) ([]byte, error) {
	if strings.HasSuffix(name, ".zip") {
		return extractZip(archive)
	}
	return extractTarGz(archive)
}

func extractTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// Regular files only — no symlink / hardlink / directory entries.
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if hasTraversalSegment(h.Name) {
			continue
		}
		if filepath.Base(h.Name) == binName() {
			return readArchiveEntry(tr, h.Name)
		}
	}
	return nil, fmt.Errorf("%s not found in archive", binName())
}

func extractZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || f.FileInfo().Mode()&os.ModeSymlink != 0 {
			continue
		}
		if hasTraversalSegment(f.Name) {
			continue
		}
		if filepath.Base(f.Name) == binName() {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return readArchiveEntry(rc, f.Name)
		}
	}
	return nil, fmt.Errorf("%s not found in archive", binName())
}

func hasTraversalSegment(name string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(name), "/"), "..")
}

// readArchiveEntry caps the read so a tampered archive can't grow the
// extracted binary unbounded. 256 MiB headroom; current claude is ~206 MiB.
func readArchiveEntry(r io.Reader, name string) ([]byte, error) {
	const cap = 256 << 20
	buf, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > cap {
		return nil, fmt.Errorf("archive entry %q exceeds %d byte cap", name, cap)
	}
	return buf, nil
}

// isFile reports whether path holds a real executable file. Routes the stat
// through safepath.SafeLstat (not a raw os.Lstat) so the whole parent chain —
// not just the leaf — is checked for symlinks, then applies the shared
// safepath.UsableExecutable predicate so the desktop's installed-status check and
// the daemon's resolveClaudeBin can't drift on what counts as a usable binary
// (non-symlink regular file, size>0, +x on unix).
func isFile(path string) bool {
	fi, err := safepath.SafeLstat(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return false
	}
	return safepath.UsableExecutable(fi)
}
