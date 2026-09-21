package watch

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/ssh"
)

func testConfig() Config {
	return Config{Interval: 3, Profiles: []Profile{{ID: "root", Name: "Shared", Username: "root", Password: "only-a-test-password"}}, Machines: []Machine{{ID: "m1", Name: "test", Host: "127.0.0.1", Port: 22, ProfileID: "root", Commands: []string{"npu", "python", "usage"}, Enabled: true}}}
}
func TestEncryptedConfigAndReuse(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	if err = s.Save(c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.enc"))
	if bytes.Contains(b, []byte(c.Profiles[0].Password)) || bytes.Contains(b, []byte("127.0.0.1")) {
		t.Fatal("configuration saved in plaintext")
	}
	p := s.Public()
	if p.Profiles[0].Password != "" || !p.Profiles[0].HasPassword {
		t.Fatal("public config exposes password or loses status")
	}
	p.Machines[0].Name = "edited"
	if err = s.Save(p); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Profiles[0].Password != c.Profiles[0].Password {
		t.Fatal("blank edit erased shared password")
	}
	p.Profiles[0].Password = "replacement"
	if err = s.Save(p); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Profiles[0].Password != "replacement" {
		t.Fatal("replacement failed")
	}
	b, _ = os.ReadFile(filepath.Join(dir, "config.enc"))
	b[len(b)-1] ^= 1
	_ = os.WriteFile(filepath.Join(dir, "config.enc"), b, 0600)
	if _, err = NewStore(dir); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	_ = os.Remove(filepath.Join(dir, "vault.key"))
	if _, err = NewStore(dir); err == nil {
		t.Fatal("missing key silently regenerated")
	}
}
func TestValidate(t *testing.T) {
	for _, mutate := range []func(*Config){func(c *Config) { c.Interval = 0 }, func(c *Config) { c.Machines[0].Host = "x; touch /tmp/no" }, func(c *Config) { c.Machines[0].ProfileID = "missing" }, func(c *Config) { c.Machines[0].Commands = []string{"rm -rf /"} }, func(c *Config) { c.Machines[0].Port = 70000 }, func(c *Config) { c.Profiles = append(c.Profiles, c.Profiles[0]) }} {
		c := testConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
	c := testConfig()
	c.Machines[0].Host = "::1"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestHTTPBoundary(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_ = s.Save(testConfig())
	trust, _ := NewTrustStore(t.TempDir())
	m := NewMonitor(trust)
	defer m.Close()
	app := NewServer(s, m, trust, fstest.MapFS{"index.html": {Data: []byte("__WATCH_TOKEN__")}}, "127.0.0.1:9999")
	for _, tc := range []struct {
		path, host, origin, token string
		want                      int
	}{
		{"/api/config", "127.0.0.1:9999", "", app.token, 200},
		{"/api/config", "127.0.0.1:9999", "", "bad", 403},
		{"/api/config", "evil.example", "", app.token, 403},
		{"/api/config", "127.0.0.1:9999", "https://evil.example", app.token, 403},
		{"/", "evil.example", "", "", 403},
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+tc.path, nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Watch-Token", tc.token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v got %d", tc, w.Code)
		}
		if strings.Contains(w.Body.String(), "only-a-test-password") {
			t.Fatal("HTTP leaked password")
		}
	}
	b, _ := json.Marshal(s.Public())
	r := httptest.NewRequest("PUT", "http://127.0.0.1:9999/api/config", bytes.NewReader(append(b, []byte(` {}`)...)))
	r.Header.Set("X-Watch-Token", app.token)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
}

type fixture struct {
	listener        net.Listener
	signer          ssh.Signer
	authenticated   atomic.Int32
	firstAuthAt     atomic.Int64
	connections     atomic.Int32
	hanging         atomic.Bool
	omitExitStatus  atomic.Bool
	dropNPUOnce     atomic.Bool
	dropAllNPU      atomic.Bool
	failNPU         atomic.Bool
	emptyNPU        atomic.Bool
	ignoreKeepalive atomic.Bool
	customRuns      atomic.Int32
	dropCustom      atomic.Bool
	npuRuns         atomic.Int32
	activeConns     sync.Map
	stop            chan struct{}
	wg              sync.WaitGroup
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{listener: l, signer: signer, stop: make(chan struct{})}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() != "root" || string(p) != "only-a-test-password" {
			return nil, fmt.Errorf("bad credentials")
		}
		f.authenticated.Add(1)
		f.firstAuthAt.CompareAndSwap(0, time.Now().UnixNano())
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			f.connections.Add(1)
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer conn.Close()
				f.activeConns.Store(conn, true)
				defer f.activeConns.Delete(conn)
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				c, ch, req, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer c.Close()
				go func() {
					for request := range req {
						if request.WantReply && !f.ignoreKeepalive.Load() {
							_ = request.Reply(false, nil)
						}
					}
				}()
				for channel := range ch {
					if channel.ChannelType() != "session" {
						_ = channel.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					stream, requests, err := channel.Accept()
					if err != nil {
						continue
					}
					f.wg.Add(1)
					go func() {
						defer f.wg.Done()
						defer stream.Close()
						for r := range requests {
							if r.Type != "exec" {
								_ = r.Reply(false, nil)
								continue
							}
							var v struct{ Command string }
							_ = ssh.Unmarshal(r.Payload, &v)
							_ = r.Reply(true, nil)
							if f.hanging.Load() {
								select {
								case <-f.stop:
								case <-time.After(2 * time.Second):
								}
								return
							}
							code := uint32(0)
							if strings.Contains(v.Command, "tilexr") {
								f.customRuns.Add(1)
								_, _ = io.WriteString(stream, v.Command)
								if f.dropCustom.Load() {
									conn.Close()
									return
								}
							} else if strings.Contains(v.Command, "npu-smi") {
								f.npuRuns.Add(1)
								if f.failNPU.Load() {
									_, _ = io.WriteString(stream.Stderr(), "npu-smi: command not found\n")
									code = 127
								} else if !f.emptyNPU.Load() {
									_, _ = io.WriteString(stream, "NPU 0 OK\n")
								}
								if f.dropAllNPU.Load() || f.dropNPUOnce.CompareAndSwap(true, false) {
									conn.Close()
									return
								}
								if f.omitExitStatus.Load() {
									return
								}
							} else if strings.Contains(v.Command, "/proc/sys/kernel/random/boot_id") {
								_, _ = io.WriteString(stream, "BOOT\tdemo-boot\n42\t12345\tL2hvbWUvdGVhbS90aWxleHI=\n")
							} else if strings.Contains(v.Command, "ps -ef") {
								_, _ = io.WriteString(stream, "root 42 1 python train.py\n")
							} else {
								_, _ = io.WriteString(stream.Stderr(), "ps: unsupported sort\n")
								code = 1
							}
							_, _ = stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
							return
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() { close(f.stop); l.Close(); f.wg.Wait() })
	return f
}

func (f *fixture) closeConnections() {
	f.activeConns.Range(func(conn, _ any) bool { _ = conn.(net.Conn).Close(); return true })
}
func waitState(t *testing.T, m *Monitor, predicate func(map[string]State) bool) map[string]State {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		s := m.Snapshot()
		if predicate(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state timeout: %+v", m.Snapshot())
	return nil
}
func TestSixteenMachinesTrustReuseAndCommandErrors(t *testing.T) {
	f := newFixture(t)
	trust, _ := NewTrustStore(t.TempDir())
	m := NewMonitor(trust)
	defer m.Close()
	c := testConfig()
	c.Interval = 3600
	c.Machines = nil
	addr := f.listener.Addr().(*net.TCPAddr)
	for i := 0; i < 16; i++ {
		host := testConfig().Machines[0]
		host.ID = fmt.Sprintf("m%d", i)
		host.Port = addr.Port
		c.Machines = append(c.Machines, host)
	}
	m.Replace(c)
	states := waitState(t, m, func(s map[string]State) bool {
		for _, v := range s {
			if v.Status != "untrusted" {
				return false
			}
		}
		return len(s) == 16
	})
	if f.authenticated.Load() != 0 {
		t.Fatal("password sent before host trust")
	}
	s := states["m0"]
	if err := trust.Accept(s.Address, s.Fingerprint); err != nil {
		t.Fatal(err)
	}
	m.Refresh()
	states = waitState(t, m, func(s map[string]State) bool {
		for _, v := range s {
			if v.Status != "partial" {
				return false
			}
		}
		return len(s) == 16
	})
	if states["m0"].Results[0].Output != "NPU 0 OK\n" || states["m0"].Results[2].Error == "" {
		t.Fatal("results or command failure lost")
	}
	auths := f.authenticated.Load()
	if auths != 16 {
		t.Fatalf("want 16 authenticated sessions got %d", auths)
	}
	last := states["m0"].UpdatedAt
	m.Refresh()
	waitState(t, m, func(s map[string]State) bool { return s["m0"].UpdatedAt.After(last) })
	if f.authenticated.Load() != auths {
		t.Fatal("refresh did not reuse connection")
	}
	_, newKey, _ := ed25519.GenerateKey(rand.Reader)
	changed, _ := ssh.NewSignerFromKey(newKey)
	err := trust.Callback(s.Address, false)("", addr, changed.PublicKey())
	te, ok := err.(*TrustError)
	if !ok || !te.Changed {
		t.Fatal("changed host key not blocked")
	}
}
func TestTimeoutAndCancellation(t *testing.T) {
	f := newFixture(t)
	f.hanging.Store(true)
	trust, _ := NewTrustStore(t.TempDir())
	addr := f.listener.Addr().String()
	_ = trust.Callback(addr, false)("", f.listener.Addr(), f.signer.PublicKey())
	_ = trust.Accept(addr, ssh.FingerprintSHA256(f.signer.PublicKey()))
	m := NewMonitor(trust)
	m.commandTimeout = 100 * time.Millisecond
	defer m.Close()
	c := testConfig()
	c.Interval = 3600
	c.Machines[0].Port = f.listener.Addr().(*net.TCPAddr).Port
	m.Replace(c)
	waitState(t, m, func(s map[string]State) bool { return s["m1"].Status == "offline" })
	start := time.Now()
	m.Refresh()
	time.Sleep(20 * time.Millisecond)
	m.Close()
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not interrupt command")
	}
}
func TestOutputLimitAndLock(t *testing.T) {
	var b boundedOutput
	payload := bytes.Repeat([]byte("x"), maxOutput+100)
	n, err := b.Write(payload)
	if err != nil || n != len(payload) || !b.truncated || b.b.Len() != maxOutput {
		t.Fatal("output not bounded")
	}
	dir := t.TempDir()
	unlock, err := LockDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := LockDirectory(dir); err == nil {
		u()
		t.Fatal("second process lock succeeded")
	}
	unlock()
	u, err := LockDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	u()
}
func TestCancelledDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, _, err := dial(ctx, "127.0.0.1:1", Profile{}, nil); err == nil {
		c.Close()
		t.Fatal("cancelled dial succeeded")
	}
}
