package octocli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

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
