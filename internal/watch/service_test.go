package watch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyLockOwnerIsNeverTakenOver(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	lease, existing, err := AcquireService(dir, "default", "test", 0, false, true)
	if lease != nil || existing != nil || !errors.Is(err, ErrDirectoryInUse) {
		t.Fatalf("legacy owner must block takeover: lease=%v existing=%v err=%v", lease, existing, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "service.json")); !os.IsNotExist(err) {
		t.Fatal("failed launch must not publish discovery metadata")
	}
	if u, err := LockDirectory(dir); err == nil {
		u()
		t.Fatal("existing owner lock was removed")
	}
}

func TestInstanceDirectoryResolution(t *testing.T) {
	base, err := ResolveServiceDir("", "default")
	if err != nil {
		t.Fatal(err)
	}
	canary, err := ResolveServiceDir("", "canary")
	if err != nil || canary != filepath.Join(base, "instances", "canary") {
		t.Fatalf("named instance directory: %s %v", canary, err)
	}
	for _, name := range []string{"../default", "", "a/b"} {
		if _, err := ResolveServiceDir(t.TempDir(), name); err == nil {
			t.Fatalf("accepted invalid instance %q", name)
		}
	}
}

func TestDiscoveryRejectsNonlocalURLs(t *testing.T) {
	for _, raw := range []string{"https://127.0.0.1:1234", "http://example.com:1234", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user@127.0.0.1:1234", "http://127.0.0.1:1234/healthz", "http://127.0.0.1:1234?x=1"} {
		if _, err := servicePort(raw); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", raw)
		}
	}
}
