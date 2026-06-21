// Package octocli installs and upgrades the octo-cli companion binary
// (github.com/Mininglamp-OSS/octo-cli) for the desktop app. octo-cli is a
// metadata-driven CLI with structured JSON I/O and no interactive prompts —
// the spawned Claude agent calls it via Bash, so we keep it on the agent's PATH
// at ~/.xclaw/bin/octo-cli (writable, so one-click upgrade can replace it).
//
// On first run the baseline bundled inside the app (Contents/Helpers/octo-cli)
// is copied into place; Upgrade fetches the latest GitHub release, verifies its
// sha256 against the release checksums.txt, and atomically swaps the binary.
package octocli

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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lml2468/xclaw/core/atomicfile"
	"github.com/lml2468/xclaw/core/config"
)

const repo = "Mininglamp-OSS/octo-cli"

// userAgent is sent on every HTTP request so the server can attribute traffic
// (also lets GitHub's anti-abuse return clearer errors than "blank UA").
const userAgent = "xclaw-desktop/octocli (+https://github.com/lml2468/xclaw)"

// Injectable seams for tests. Production uses the real GitHub API over the
// hardened HTTP client (dial-time SSRF guard); tests point these at an
// httptest server.
var (
	httpClient = newSafeClient()
	apiBase    = "https://api.github.com"
)

// newSafeClient returns an HTTP client whose dialer rejects connections to
// private/loopback/link-local/CGN addresses (mirrors core/gateway's media
// downloader). The download URLs (GitHub API + asset CDN) are public, but
// they redirect to S3 / Fastly hosts — a poisoned DNS or compromised mirror
// could redirect to 169.254.169.254 (cloud metadata) or a private internal
// address; this dial guard turns that into a connect-refused.
func newSafeClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   dialControlGuard,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			MaxConnsPerHost:       4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func dialControlGuard(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("octocli dial: bad address %q: %w", address, err)
	}
	if config.IsPrivateOrLocalAddress(host) {
		return fmt.Errorf("octocli dial: refusing private/local address %s", host)
	}
	return nil
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "octo-cli.exe"
	}
	return "octo-cli"
}

// Dir is ~/.xclaw/bin — the writable install dir, added to the agent's PATH.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", "bin")
}

// BinPath is ~/.xclaw/bin/octo-cli.
func BinPath() string { return filepath.Join(Dir(), binName()) }

func versionFile() string { return filepath.Join(Dir(), "octo-cli.version") }

// InstalledVersion returns the recorded version of the installed binary, or ""
// if octo-cli isn't installed.
func InstalledVersion() string {
	if !isFile(BinPath()) {
		return ""
	}
	b, err := os.ReadFile(versionFile())
	if err != nil {
		return "" // installed but version unknown
	}
	return strings.TrimSpace(string(b))
}

func writeVersion(v string) {
	_ = os.WriteFile(versionFile(), []byte(strings.TrimSpace(v)+"\n"), 0o644)
}

// bundledBinary returns the octo-cli shipped inside the app bundle
// (Contents/Helpers/octo-cli) and its version (Contents/Resources/octo-cli.version,
// kept out of Helpers so it isn't treated as an unsignable code subcomponent),
// or ("", "") in a dev build.
func bundledBinary() (path, version string) {
	exe, err := os.Executable()
	if err != nil {
		return "", ""
	}
	contents := filepath.Dir(filepath.Dir(exe)) // …/Contents (exe is …/Contents/MacOS/<app>)
	p := filepath.Join(contents, "Helpers", binName())
	if !isFile(p) {
		return "", ""
	}
	v := ""
	if b, err := os.ReadFile(filepath.Join(contents, "Resources", "octo-cli.version")); err == nil {
		v = strings.TrimSpace(string(b))
	}
	return filepath.Clean(p), v
}

// EnsureInstalled copies the bundled baseline into ~/.xclaw/bin when nothing is
// installed yet, or when the bundle ships a newer version than what's installed
// (an app update) — but never downgrades a binary the user upgraded via Upgrade.
// Best-effort: a dev build with no bundle is a no-op (the user can still
// Upgrade to download octo-cli).
//
// Verifies the bundled binary's SHA-256 against a sha256 sidecar shipped
// alongside the version file (Contents/Resources/octo-cli.sha256), so a
// post-build / post-install tamper of the helper — anyone with write access
// to XClaw.app/Contents/Helpers/ on a non-Developer-signed bundle, e.g. an
// admin user, a tampered .zip downloaded over HTTP, a malicious package
// extractor — fails closed instead of getting silently installed and
// executed (round 11 Sec). A production bundle is detected by the presence
// of the version file (Resources/octo-cli.version, shipped by
// package-desktop.sh alongside the sidecar); a production bundle MUST carry
// the sidecar (round 12 Sec S2: missing-sidecar previously fell open). A
// dev build (no version file, no sidecar) keeps the install path open with
// a stderr warning so local builds can iterate.
//
// Read-once-then-install (round 12 Sec S1): the bundled bytes are read into
// a buffer, hashed, then handed to installBinary which writes the SAME
// buffer — closes the TOCTOU window between hash-from-disk and copy-from-disk
// that an attacker with write access to Contents/Helpers/ could race.
func EnsureInstalled() error {
	src, bundledVer := bundledBinary()
	if src == "" {
		return nil // no bundle (dev build)
	}
	installed := InstalledVersion()
	if isFile(BinPath()) && !(bundledVer != "" && compareVersions(bundledVer, installed) > 0) {
		return nil // already installed and not older than the bundle
	}
	buf, err := verifyBundledBytes(src, bundledVer != "")
	if err != nil {
		return fmt.Errorf("EnsureInstalled: bundled octo-cli integrity check failed: %w", err)
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	if err := installBinary("", buf); err != nil {
		return err
	}
	if bundledVer != "" {
		writeVersion(bundledVer)
	}
	return nil
}

// verifyBundledBytes reads src into a buffer, hashes the SAME buffer (no
// re-open between hash and install), and compares to the expected sha256
// written in the app bundle's Resources/octo-cli.sha256 sidecar (one hex
// digest per line; the first token wins so the file can carry an optional
// filename suffix in shasum -a 256 format). Returns the buffer for the
// caller to install verbatim — never re-read from disk after this point.
//
// When isProduction is true the sidecar MUST exist and verify; a missing
// sidecar is an error (round 12 Sec S2). When isProduction is false (no
// version file shipped, i.e. dev build), a missing sidecar logs a warning
// and returns the buffer unverified — local iteration stays unblocked.
func verifyBundledBytes(src string, isProduction bool) ([]byte, error) {
	buf, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("read bundled binary: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		// We located src via Executable a moment ago; if it now errors the
		// process is in an unusual state. Fail closed in production rather
		// than ship an unverified buffer (round 13 F3).
		if isProduction {
			return nil, fmt.Errorf("cannot locate bundle for sidecar lookup: %w", err)
		}
		return buf, nil
	}
	contents := filepath.Dir(filepath.Dir(exe))
	shaPath := filepath.Join(contents, "Resources", "octo-cli.sha256")
	raw, err := os.ReadFile(shaPath)
	if err != nil {
		if os.IsNotExist(err) {
			if isProduction {
				return nil, fmt.Errorf("production bundle is missing %s — refusing to install unverified binary", shaPath)
			}
			fmt.Fprintf(os.Stderr, "[octocli] WARNING: %s missing — installing bundled binary without integrity check (dev build)\n", shaPath)
			return buf, nil
		}
		return nil, fmt.Errorf("read sha256 sidecar: %w", err)
	}
	// Round 13 F4: an empty / whitespace-only sidecar previously panicked on
	// `strings.Fields(...)[0]`. Treat empty as malformed.
	parts := strings.Fields(string(raw))
	if len(parts) == 0 {
		return nil, fmt.Errorf("sha256 sidecar empty or whitespace-only")
	}
	want := strings.ToLower(parts[0])
	if len(want) != 64 {
		return nil, fmt.Errorf("sha256 sidecar malformed: expected 64 hex chars, got %d", len(want))
	}
	if got := sha256hex(buf); got != want {
		return nil, fmt.Errorf("bundled octo-cli sha256 mismatch: have %s, want %s", got, want)
	}
	return buf, nil
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
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return r, err
	}
	if r.TagName == "" {
		return r, fmt.Errorf("github releases: no tag in latest release")
	}
	return r, nil
}

// assetName is the GoReleaser archive name for this platform, e.g.
// "octo-cli_0.6.0_darwin_arm64.tar.gz".
func assetName(tag string) string {
	v := strings.TrimPrefix(tag, "v")
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("octo-cli_%s_%s_%s.%s", v, runtime.GOOS, runtime.GOARCH, ext)
}

// Upgrade downloads the latest release's binary for this platform, verifies its
// sha256 against the release checksums.txt, and atomically replaces the
// installed binary. Verification is mandatory and fails closed: if the release
// ships no checksums.txt, or it lacks an entry for our asset, or the hash
// mismatches, Upgrade aborts without installing. Returns the version installed.
func Upgrade(ctx context.Context) (string, error) {
	rel, err := latestRelease(ctx)
	if err != nil {
		return "", err
	}
	want := assetName(rel.TagName)
	var assetURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			assetURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		return "", fmt.Errorf("no octo-cli asset %q in release %s", want, rel.TagName)
	}

	// Fail closed: the release MUST ship a checksums.txt and it MUST contain a
	// matching sha256 for our asset. octo-cli lands on the agent's PATH and is
	// executed by the spawned agent, so we never install an unverified binary.
	if sumsURL == "" {
		return "", fmt.Errorf("octo-cli release %s has no checksums.txt; refusing to install unverified binary", rel.TagName)
	}

	archive, err := download(ctx, assetURL)
	if err != nil {
		return "", err
	}
	sums, err := download(ctx, sumsURL)
	if err != nil {
		return "", fmt.Errorf("octo-cli: download checksums.txt: %w", err)
	}
	if err := verifyChecksum(sums, archive, want); err != nil {
		return "", err
	}

	bin, err := extractBinary(archive, want)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return "", err
	}
	// Snapshot the current binary as .prev before replacing it, so a bad upgrade
	// (verified checksum but non-functional binary) has a known-good rollback
	// point. Best-effort: a missing current binary (first install) just skips it.
	if cur, rerr := os.ReadFile(BinPath()); rerr == nil {
		_ = os.WriteFile(BinPath()+".prev", cur, 0o700)
	}
	if err := installBinary("", bin); err != nil {
		return "", err
	}
	// Only stamp the version AFTER the binary is in place, so a crash between the
	// two never records a version we didn't actually install.
	writeVersion(rel.TagName)
	return rel.TagName, nil
}

// installBinary writes the octo-cli binary atomically (temp file + rename) and
// makes it executable. Provide srcPath to copy from a file, or data for bytes.
func installBinary(srcPath string, data []byte) error {
	if data == nil {
		b, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		data = b
	}
	// 0o700 on the temp + atomic rename via the shared helper (same write
	// path as configstore / cron — was previously a hand-rolled
	// temp+chmod+rename without fsync, a fourth copy of this pattern that
	// drifted from the others; the round-6 atomicfile pkg was created
	// exactly to retire this kind of duplication).
	return atomicfile.Write(BinPath(), data, 0o700)
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
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64 MiB cap
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verifyChecksum enforces that the GoReleaser checksums.txt contains an entry
// for filename and that the archive's sha256 matches it. It fails closed: a
// missing entry is an error, never a silent skip.
func verifyChecksum(sums, archive []byte, filename string) error {
	exp := checksumFor(sums, filename)
	if exp == "" {
		return fmt.Errorf("octo-cli: no checksum entry for %s in checksums.txt", filename)
	}
	if got := sha256hex(archive); !strings.EqualFold(exp, got) {
		return fmt.Errorf("octo-cli checksum mismatch for %s: want %s got %s", filename, exp, got)
	}
	return nil
}

// checksumFor finds the sha256 for filename in a GoReleaser checksums.txt
// ("<sha256>  <filename>" per line).
func checksumFor(sums []byte, filename string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == filename {
			return f[0]
		}
	}
	return ""
}

// extractBinary pulls the octo-cli executable out of a .tar.gz or .zip archive.
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
		// Round 12 Sec S3: only regular files, never symlinks / hardlinks /
		// directories with the binName as basename (a malicious archive can
		// ship a symlink or empty hardlink to silently install a 0-byte
		// binary or a body the attacker controls).
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// Reject any traversal segment — the archive should only contain
		// flat or single-level paths under a well-known directory, and a
		// `../../../` name is never something we want to honor.
		if strings.Contains(h.Name, "..") {
			continue
		}
		if filepath.Base(h.Name) == binName() {
			return io.ReadAll(io.LimitReader(tr, 64<<20))
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
		// Round 12 Sec S3: skip non-regular entries (directory, symlink) +
		// any traversal segment in the entry name.
		if f.FileInfo().IsDir() || f.FileInfo().Mode()&os.ModeSymlink != 0 {
			continue
		}
		if strings.Contains(f.Name, "..") {
			continue
		}
		if filepath.Base(f.Name) == binName() {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 64<<20))
		}
	}
	return nil, fmt.Errorf("%s not found in archive", binName())
}

// compareVersions compares dotted versions (leading "v" ignored). Returns
// >0 if a>b, <0 if a<b, 0 if equal. Non-numeric/empty parts sort as 0.
func compareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(strings.TrimSpace(a), "v"), ".")
	pb := strings.Split(strings.TrimPrefix(strings.TrimSpace(b), "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// Login records a bot's bf_ token under a per-robot-id profile in octo-cli's
// credential store (~/.octo-cli/credentials.enc). Required for any new bot:
// when the agent is spawned with OCTO_BOT_ID=<robotID>, octo-cli does a
// profile lookup (NOT env fallback) and errors with "no profile found" if the
// profile is missing — so without this call, every octo-cli invocation from
// the agent for a newly-added bot fails authoritatively even though the bf_
// token is in our keychain + injected as OCTO_BOT_TOKEN.
//
// Idempotent: re-running with the same (robotID, token, apiURL) replaces the
// profile in place. Best-effort by design: a missing octo-cli binary or a
// failure inside auth login is returned so the caller can surface it, but a
// caller that wants to keep "saving config" robust can choose to log + ignore.
//
// The token is fed on stdin (`--with-token`) — never as an argv element, so it
// never appears in ps / process listings.
func Login(ctx context.Context, robotID, token, apiURL string) error {
	if robotID == "" {
		return fmt.Errorf("octocli.Login: robotID is required")
	}
	if token == "" {
		return fmt.Errorf("octocli.Login: token is required")
	}
	bin := BinPath()
	if !isFile(bin) {
		return fmt.Errorf("octocli.Login: octo-cli not installed at %s", bin)
	}
	args := []string{"auth", "login", "--bot-id", robotID, "--with-token"}
	if apiURL != "" {
		args = append(args, "--api-base-url", apiURL)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("octo-cli auth login (%s): %w (output: %s)", robotID, err, redactChildOutput(out))
	}
	return nil
}

// redactChildOutput truncates and redacts a child process's CombinedOutput
// before it is bubbled into an error returned to the desktop process. The
// concrete risk: octo-cli (or any helper invoked here in future) could echo a
// bearer token in a verbose-mode error path — currently it does not, but a
// regression would otherwise plumb the token straight into our logs and the
// SaveConfig UI error toast. The redactor masks any token-shaped substring
// (greedy — see tokenShapedRE) and clamps the result to 256 chars.
// tokenShapedRE matches any token-prefix substring anywhere in the input —
// NOT word-boundary-anchored — so glued forms (`Authorization=bf_xxx`,
// `token:bf_x`, `"bf_x"`, `[bf_x]`, ANSI-wrapped `\x1b[31mbf_x`) all redact.
// The greedy posture is deliberate: defense-in-depth against an octo-cli
// stderr regression that echoes a bearer. The trade-off is over-redaction
// of paths like `/api/bf_lookup/...` — acceptable in an error-toast / log
// context since over-redaction never leaks; the only cost is debuggability.
var tokenShapedRE = regexp.MustCompile(`(?i)(bf_|uk_|sk_|sk-|ANTHROPIC_)[A-Za-z0-9_\-]+`)

func redactChildOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	s = tokenShapedRE.ReplaceAllString(s, "<redacted>")
	const maxLen = 256
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// Logout removes the per-robot-id profile from octo-cli's credential store.
// Best-effort: absent profile + absent binary both return nil so callers can
// run this on bot deletion without worrying about preconditions.
func Logout(ctx context.Context, robotID string) error {
	if robotID == "" {
		return nil
	}
	bin := BinPath()
	if !isFile(bin) {
		return nil
	}
	cmd := exec.CommandContext(ctx, bin, "auth", "logout", "--bot-id", robotID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// octo-cli returns non-zero when the profile doesn't exist — treat as
		// already-clean rather than an error.
		if strings.Contains(string(out), "no profile found") {
			return nil
		}
		return fmt.Errorf("octo-cli auth logout (%s): %w (output: %s)", robotID, err, redactChildOutput(out))
	}
	return nil
}

// HasProfile reports whether ~/.octo-cli/config.json has an entry for robotID.
// We read the JSON directly instead of shelling out to `octo-cli auth list` —
// 10x faster for a UI poll, and a missing binary is just "no" not an error.
// The encrypted credentials file (credentials.enc) is separate; config.json
// is the index that lists which profiles exist.
func HasProfile(robotID string) bool {
	if robotID == "" {
		return false
	}
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".octo-cli", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false // missing file → no profiles registered
	}
	var cfg struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}
	_, ok := cfg.Profiles[robotID]
	return ok
}
