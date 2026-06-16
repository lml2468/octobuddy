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
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const repo = "Mininglamp-OSS/octo-cli"

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
func EnsureInstalled() error {
	src, bundledVer := bundledBinary()
	if src == "" {
		return nil // no bundle (dev build)
	}
	installed := InstalledVersion()
	if isFile(BinPath()) && !(bundledVer != "" && compareVersions(bundledVer, installed) > 0) {
		return nil // already installed and not older than the bundle
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	if err := installBinary(src, nil); err != nil {
		return err
	}
	if bundledVer != "" {
		writeVersion(bundledVer)
	}
	return nil
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
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "xclaw-desktop")
	resp, err := http.DefaultClient.Do(req)
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

// LatestVersion returns the latest published release tag (for update checks).
func LatestVersion(ctx context.Context) (string, error) {
	r, err := latestRelease(ctx)
	return r.TagName, err
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
	if err := installBinary("", bin); err != nil {
		return "", err
	}
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
	tmp := BinPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, BinPath())
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "xclaw-desktop")
	resp, err := http.DefaultClient.Do(req)
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
