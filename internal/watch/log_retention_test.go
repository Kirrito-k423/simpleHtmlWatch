package watch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBoundedLogsPreserveInodeAndOpenAppender(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "diagnostic.log")
	old := bytes.Repeat([]byte("old diagnostic line\n"), 6000)
	if err := os.WriteFile(path, append(old, []byte("recent diagnostic\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	writer, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	b, err := NewBoundedLogs(dir, []string{"diagnostic.log", "missing.log", "diagnostic.log"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := writer.Write([]byte("still connected\n")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	content, _ := os.ReadFile(path)
	if !os.SameFile(before, after) || len(content) > logTailBytes+32 || !bytes.HasSuffix(content, []byte("recent diagnostic\nstill connected\n")) {
		t.Fatal("log retention broke the existing writer or lost the recent tail")
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBoundedLogs(dir, []string{"diagnostic.log", "job.json"}); err == nil {
		t.Fatal("accepted task metadata as a diagnostic log")
	}
	if _, err := NewBoundedLogs(dir, []string{"../outside.log"}); err == nil {
		t.Fatal("accepted an outside target")
	}
}

func TestBoundedLogsRejectSymlinkBeforeTrimmingAnyFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	content := bytes.Repeat([]byte("evidence\n"), 10000)
	_ = os.WriteFile(outside, content, 0600)
	_ = os.WriteFile(filepath.Join(dir, "ordinary.log"), content, 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "link.log")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewBoundedLogs(dir, []string{"ordinary.log", "link.log"}); err == nil {
		t.Fatal("accepted a symlink log")
	}
	for _, path := range []string{outside, filepath.Join(dir, "ordinary.log")} {
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, content) {
			t.Fatal("invalid configuration modified a file")
		}
	}
}
