package watch

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func TestAutoTrustDeadlinePersistenceAndManualMode(t *testing.T) {
	dir := t.TempDir()
	trust, err := NewTrustStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	trust.now = func() time.Time { return now }
	key := newHostKey(t)
	cb := trust.Callback("test:22", true)
	err = cb("", nil, key)
	p, ok := err.(*TrustError)
	if !ok || p.Changed || !p.AutoTrustAt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("missing 5s deadline: %+v", err)
	}
	deadline := p.AutoTrustAt
	now = deadline.Add(-time.Nanosecond)
	if err = cb("", nil, key); err == nil {
		t.Fatal("trusted before the full five seconds")
	}
	if _, err = os.Stat(trust.path); !os.IsNotExist(err) {
		t.Fatal("key persisted before deadline")
	}
	now = deadline
	if err = cb("", nil, key); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewTrustStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = loaded.Callback("test:22", false)("", nil, key); err != nil {
		t.Fatal("trusted key not persisted")
	}
	if err = trust.Accept("test:22", ssh.FingerprintSHA256(key)); err != nil {
		t.Fatal("manual/auto race should be idempotent", err)
	}

	// Disabling auto-trust during a countdown must cancel that deadline.
	_ = trust.Callback("manual:22", true)("", nil, key)
	now = now.Add(time.Second)
	err = trust.Callback("manual:22", false)("", nil, key)
	if p, ok = err.(*TrustError); !ok || !p.AutoTrustAt.IsZero() {
		t.Fatal("manual mode retained automatic deadline")
	}
	now = now.Add(time.Hour)
	if err = trust.Callback("manual:22", false)("", nil, key); err == nil {
		t.Fatal("manual mode accepted without user action")
	}
	err = trust.Callback("manual:22", true)("", nil, key)
	if p, ok = err.(*TrustError); !ok || !p.AutoTrustAt.Equal(now.Add(5*time.Second)) {
		t.Fatal("re-enabling did not start a fresh countdown")
	}
	trust.ResetCountdowns()
	now = now.Add(time.Hour)
	err = trust.Callback("manual:22", true)("", nil, key)
	if p, ok = err.(*TrustError); !ok || !p.AutoTrustAt.Equal(now.Add(5*time.Second)) {
		t.Fatal("resuming a disabled machine reused an expired countdown")
	}
}

func TestAutoTrustRejectsChangedKeysAndWriteFailure(t *testing.T) {
	for _, alreadyTrusted := range []bool{false, true} {
		t.Run(fmt.Sprint("previously_trusted=", alreadyTrusted), func(t *testing.T) {
			trust, _ := NewTrustStore(t.TempDir())
			now := time.Now()
			trust.now = func() time.Time { return now }
			key1, key2 := newHostKey(t), newHostKey(t)
			cb := trust.Callback("test:22", true)
			_ = cb("", nil, key1)
			if alreadyTrusted {
				if err := trust.Accept("test:22", ssh.FingerprintSHA256(key1)); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(time.Second)
			_ = cb("", nil, key2)
			now = now.Add(time.Hour)
			err := cb("", nil, key2)
			p, ok := err.(*TrustError)
			if !ok || !p.Changed || !p.AutoTrustAt.IsZero() {
				t.Fatalf("changed key auto-accepted: %+v", err)
			}
			if err = trust.Accept("test:22", ssh.FingerprintSHA256(key2)); err != nil {
				t.Fatal(err)
			}
		})
	}
	trust, _ := NewTrustStore(t.TempDir())
	now := time.Now()
	trust.now = func() time.Time { return now }
	key := newHostKey(t)
	cb := trust.Callback("test:22", true)
	_ = cb("", nil, key)
	now = now.Add(5 * time.Second)
	// A directory as the destination reliably fails atomic replacement on all platforms.
	if err := os.Mkdir(trust.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := cb("", nil, key); err == nil {
		t.Fatal("trusted a key that could not be saved")
	}
	if trust.keys["test:22"] != "" {
		t.Fatal("failed save modified in-memory trust")
	}
}

func TestSixteenMachinesAutoConnectAfterFiveSeconds(t *testing.T) {
	f := newFixture(t)
	trust, _ := NewTrustStore(t.TempDir())
	m := NewMonitor(trust)
	defer m.Close()
	c := testConfig()
	c.Interval = 3600
	c.AutoTrustNewKeys = true
	c.Machines = nil
	for i := 0; i < 16; i++ {
		h := testConfig().Machines[0]
		h.ID = fmt.Sprintf("m%d", i)
		h.Port = f.listener.Addr().(*net.TCPAddr).Port
		c.Machines = append(c.Machines, h)
	}
	m.Replace(c)
	states := waitState(t, m, func(s map[string]State) bool {
		for _, v := range s {
			if v.Status != "untrusted" || v.AutoTrustAt == nil {
				return false
			}
		}
		return len(s) == 16
	})
	deadline := *states["m0"].AutoTrustAt
	if f.authenticated.Load() != 0 {
		t.Fatal("password sent while waiting")
	}
	// No refresh button, browser polling, or manual Accept calls: the worker wakes itself.
	waitState(t, m, func(s map[string]State) bool {
		for _, v := range s {
			if v.Status != "partial" {
				return false
			}
		}
		return len(s) == 16
	})
	first := time.Unix(0, f.firstAuthAt.Load())
	if first.Before(deadline) {
		t.Fatalf("password sent early: %v < %v", first, deadline)
	}
	if first.Sub(deadline) > 2*time.Second {
		t.Fatal("auto trust incorrectly waited for sampling interval")
	}
	if f.authenticated.Load() != 16 {
		t.Fatalf("authenticated %d of 16", f.authenticated.Load())
	}
	t.Logf("16 machines connected automatically; first authentication %v after the 5s deadline", first.Sub(deadline))
}

func TestAutoTrustConfigMigrationAndOptOut(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Public().AutoTrustNewKeys {
		t.Fatal("new configuration must enable automatic trust")
	}
	legacy := testConfig()
	raw, _ := json.Marshal(legacy)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "autoTrustNewKeys")
	raw, _ = json.Marshal(fields)
	enc, _ := s.encrypt(raw)
	if err = os.WriteFile(filepath.Join(dir, "config.enc"), enc, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Public().AutoTrustNewKeys {
		t.Fatal("legacy configuration did not inherit new default")
	}
	c := s.Snapshot()
	c.AutoTrustNewKeys = false
	if err = s.Save(c); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Public().AutoTrustNewKeys {
		t.Fatal("explicit opt-out was lost on restart")
	}
}
