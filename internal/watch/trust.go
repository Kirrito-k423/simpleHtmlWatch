package watch

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type TrustError struct {
	Address     string
	Fingerprint string
	Changed     bool
	AutoTrustAt time.Time
}

func (e *TrustError) Error() string {
	if e.Changed {
		return "SSH 主机密钥发生变化，请先核对指纹"
	}
	if !e.AutoTrustAt.IsZero() {
		return "首次连接，等待 5 秒后自动信任主机指纹"
	}
	return "首次连接，请确认 SSH 主机指纹"
}

type TrustStore struct {
	mu      sync.Mutex
	path    string
	keys    map[string]string
	pending map[string]pendingKey
	now     func() time.Time
}

const autoTrustDelay = 5 * time.Second

type pendingKey struct {
	fingerprint string
	autoTrustAt time.Time
	changed     bool
}

func NewTrustStore(dir string) (*TrustStore, error) {
	t := &TrustStore{path: filepath.Join(dir, "known_hosts.json"), keys: map[string]string{}, pending: map[string]pendingKey{}, now: time.Now}
	b, err := os.ReadFile(t.path)
	if os.IsNotExist(err) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &t.keys); err != nil {
		return nil, err
	}
	if t.keys == nil {
		t.keys = map[string]string{}
	}
	return t, nil
}
func (t *TrustStore) Callback(addr string, autoTrust bool) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		t.mu.Lock()
		defer t.mu.Unlock()
		old := t.keys[addr]
		if old == fp {
			delete(t.pending, addr)
			return nil
		}
		p := t.pending[addr]
		// A different key during the waiting window also needs manual review.
		if p.fingerprint != "" && p.fingerprint != fp {
			p.changed = true
		}
		p.fingerprint = fp
		p.changed = p.changed || old != ""
		if autoTrust && !p.changed {
			if p.autoTrustAt.IsZero() {
				p.autoTrustAt = t.now().Add(autoTrustDelay)
			}
		} else {
			p.autoTrustAt = time.Time{}
		}
		t.pending[addr] = p
		if !p.autoTrustAt.IsZero() && !t.now().Before(p.autoTrustAt) {
			return t.acceptLocked(addr, fp)
		}
		return &TrustError{Address: addr, Fingerprint: fp, Changed: p.changed, AutoTrustAt: p.autoTrustAt}
	}
}
func (t *TrustStore) Accept(addr, fp string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fp != "" && t.keys[addr] == fp {
		return nil
	}
	if fp == "" || t.pending[addr].fingerprint != fp {
		return errors.New("指纹已失效，请刷新后重新核对")
	}
	return t.acceptLocked(addr, fp)
}

// Configuration changes cancel waiting workers. A resumed connection gets a fresh
// countdown, while evidence of a changed fingerprint remains blocked.
func (t *TrustStore) ResetCountdowns() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for addr, p := range t.pending {
		p.autoTrustAt = time.Time{}
		t.pending[addr] = p
	}
}

func (t *TrustStore) acceptLocked(addr, fp string) error {
	next := map[string]string{}
	for k, v := range t.keys {
		next[k] = v
	}
	next[addr] = fp
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err = writeAtomic(t.path, b); err != nil {
		return err
	}
	t.keys = next
	delete(t.pending, addr)
	return nil
}
