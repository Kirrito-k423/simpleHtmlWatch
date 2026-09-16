package watch

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
)

type TrustError struct {
	Address     string
	Fingerprint string
	Changed     bool
}

func (e *TrustError) Error() string {
	if e.Changed {
		return "SSH 主机密钥发生变化，请先核对指纹"
	}
	return "首次连接，请确认 SSH 主机指纹"
}

type TrustStore struct {
	mu      sync.Mutex
	path    string
	keys    map[string]string
	pending map[string]string
}

func NewTrustStore(dir string) (*TrustStore, error) {
	t := &TrustStore{path: filepath.Join(dir, "known_hosts.json"), keys: map[string]string{}, pending: map[string]string{}}
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
func (t *TrustStore) Callback(addr string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		t.mu.Lock()
		defer t.mu.Unlock()
		old := t.keys[addr]
		if old == fp {
			return nil
		}
		t.pending[addr] = fp
		return &TrustError{Address: addr, Fingerprint: fp, Changed: old != ""}
	}
}
func (t *TrustStore) Accept(addr, fp string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fp == "" || t.pending[addr] != fp {
		return errors.New("指纹已失效，请刷新后重新核对")
	}
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
