package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteAppends(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "x.log", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("world\n")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "x.log"))
	if string(b) != "hello\nworld\n" {
		t.Fatalf("got %q", b)
	}
}

func TestReopenPreservesPriorContent(t *testing.T) {
	dir := t.TempDir()
	w1, _ := New(dir, "x.log", 0)
	w1.Write([]byte("first run\n"))
	w1.Close()

	w2, _ := New(dir, "x.log", 0)
	defer w2.Close()
	w2.Write([]byte("second run\n"))

	b, _ := os.ReadFile(filepath.Join(dir, "x.log"))
	if string(b) != "first run\nsecond run\n" {
		t.Fatalf("got %q", b)
	}
}

func TestRotateAtCap(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "x.log", 32) // tiny cap so a few writes trigger rotation
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// 1: 20 bytes — under cap, written to live file
	w.Write(bytes.Repeat([]byte("a"), 20))
	// 2: 20 bytes — 20+20=40 > 32, rotates BEFORE writing; live now holds just this 20
	w.Write(bytes.Repeat([]byte("b"), 20))

	live, _ := os.ReadFile(filepath.Join(dir, "x.log"))
	if l := len(live); l != 20 || !bytes.Equal(live, bytes.Repeat([]byte("b"), 20)) {
		t.Fatalf("live: len=%d content=%q (want 20 b's)", l, live)
	}
	rot, err := os.ReadFile(filepath.Join(dir, "x.log.1"))
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if !bytes.Equal(rot, bytes.Repeat([]byte("a"), 20)) {
		t.Fatalf("rotated content = %q", rot)
	}
}

func TestRotateReplacesPriorBackup(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir, "x.log", 16)
	defer w.Close()
	// fill + rotate twice — the second rotation must overwrite the first .1
	w.Write(bytes.Repeat([]byte("a"), 12))
	w.Write(bytes.Repeat([]byte("b"), 12)) // rotates: .1 holds "a"*12, live holds "b"*12
	w.Write(bytes.Repeat([]byte("c"), 12)) // rotates: .1 holds "b"*12, live holds "c"*12

	rot, _ := os.ReadFile(filepath.Join(dir, "x.log.1"))
	if !bytes.Equal(rot, bytes.Repeat([]byte("b"), 12)) {
		t.Fatalf("rotated content = %q (want 12 b's; the prior 'a' backup was supposed to be replaced)", rot)
	}
}

func TestPathStableAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir, "x.log", 8)
	defer w.Close()
	want := w.Path()
	w.Write(bytes.Repeat([]byte("a"), 10)) // forces a rotation on next write
	w.Write([]byte("b"))
	if w.Path() != want {
		t.Fatalf("path changed across rotation: was %q now %q", want, w.Path())
	}
}

func TestConcurrentWritesDoNotInterleaveLines(t *testing.T) {
	// Lines from different goroutines may appear in any order, but each Write
	// must land atomically — no half-written lines mid-rotation.
	dir := t.TempDir()
	w, _ := New(dir, "x.log", 200) // small enough to provoke rotation
	defer w.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			line := []byte(strings.Repeat("x", 20) + "\n")
			for i := 0; i < 50; i++ {
				w.Write(line)
			}
		}(g)
	}
	wg.Wait()

	check := func(path string) {
		b, _ := os.ReadFile(path)
		for _, line := range bytes.Split(bytes.TrimRight(b, "\n"), []byte{'\n'}) {
			if len(line) != 20 {
				t.Fatalf("%s: torn line len=%d (%q)", filepath.Base(path), len(line), line)
			}
		}
	}
	check(filepath.Join(dir, "x.log"))
	if _, err := os.Stat(filepath.Join(dir, "x.log.1")); err == nil {
		check(filepath.Join(dir, "x.log.1"))
	}
}

func TestTeeFanout(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir, "x.log", 0)
	defer w.Close()
	var buf bytes.Buffer
	tee := w.Tee(&buf)
	tee.Write([]byte("xyz"))
	if buf.String() != "xyz" {
		t.Fatalf("other writer didn't receive: %q", buf.String())
	}
	disk, _ := os.ReadFile(filepath.Join(dir, "x.log"))
	if string(disk) != "xyz" {
		t.Fatalf("file didn't receive: %q", disk)
	}
}
