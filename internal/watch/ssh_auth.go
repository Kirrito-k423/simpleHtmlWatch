package watch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Shared by monitoring, one-shot commands and task control. Preferences contain
// only a credential digest and are scoped to an address/user in this process.
// A credential edit resets the preference; different users/hosts stay separate.
var sshKeyPreferences = struct {
	sync.Mutex
	entries map[string][32]byte
}{entries: make(map[string][32]byte)}

func sshPreference(p Profile, addr string) (string, [32]byte, bool) {
	id := addr + "\x00" + p.Username
	digest := sha256.Sum256([]byte(p.ID + "\x00" + p.Password + "\x00" + p.PrivateKeyPath + "\x00" + p.KeyPassphrase + "\x00" + os.Getenv("SSH_AUTH_SOCK")))
	sshKeyPreferences.Lock()
	defer sshKeyPreferences.Unlock()
	previous, exists := sshKeyPreferences.entries[id]
	return id, digest, exists && previous == digest
}

func rememberSSHAuth(id string, digest [32]byte, method string) {
	sshKeyPreferences.Lock()
	defer sshKeyPreferences.Unlock()
	if method != "key" {
		delete(sshKeyPreferences.entries, id)
		return
	}
	if len(sshKeyPreferences.entries) >= 512 {
		clear(sshKeyPreferences.entries)
	}
	sshKeyPreferences.entries[id] = digest
}

type authenticatedConn struct {
	net.Conn
	method string
}

func connectionAuthMethod(conn net.Conn) string {
	if authenticated, ok := conn.(*authenticatedConn); ok {
		return authenticated.method
	}
	return ""
}

func resolvePrivateKeyPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("无法定位本机用户目录，请填写 SSH 私钥绝对路径")
		}
		return filepath.Join(home, path[2:]), nil
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("SSH 私钥路径需为本机绝对路径或以 ~/ 开头")
	}
	return path, nil
}

func privateKeySigner(path, passphrase string) (ssh.Signer, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取 SSH 私钥：%w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("SSH 私钥需为不超过 1 MiB 的普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取 SSH 私钥：%w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	var encrypted *ssh.PassphraseMissingError
	if errors.As(err, &encrypted) {
		if passphrase == "" {
			return nil, errors.New("SSH 私钥已加密，请填写私钥口令或将密钥加载到 SSH agent")
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
		if err != nil {
			return nil, errors.New("无法解锁 SSH 私钥，请检查私钥口令")
		}
	}
	if err != nil {
		return nil, errors.New("SSH 私钥格式无效，请选择 OpenSSH 或 PEM 私钥文件")
	}
	return signer, nil
}

// Called lazily inside SSH authentication, after host-key verification. An
// explicit file selects only that identity; auto mode tries the local agent
// (SSH_AUTH_SOCK on macOS/Linux), then the standard files on every platform.
func sshKeySigners(ctx context.Context, p Profile, deadline time.Time) ([]ssh.Signer, func(), error) {
	if p.PrivateKeyPath != "" {
		path, err := resolvePrivateKeyPath(p.PrivateKeyPath)
		if err != nil {
			return nil, func() {}, err
		}
		signer, err := privateKeySigner(path, p.KeyPassphrase)
		if err != nil {
			return nil, func() {}, err
		}
		return []ssh.Signer{signer}, func() {}, nil
	}
	var signers []ssh.Signer
	cleanup := func() {}
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
		if err == nil {
			stop := context.AfterFunc(ctx, func() { connection.Close() })
			cleanup = func() { stop(); connection.Close() }
			// A stalled agent must leave time to try file identities.
			_ = connection.SetDeadline(minTime(deadline, time.Now().Add(2*time.Second)))
			signers, _ = agent.NewClient(connection).Signers()
			_ = connection.SetDeadline(deadline)
		}
	}
	var keyErrors []error
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			path := filepath.Join(home, ".ssh", name)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				continue
			}
			signer, err := privateKeySigner(path, p.KeyPassphrase)
			if err == nil {
				signers = append(signers, signer)
			} else {
				keyErrors = append(keyErrors, err)
			}
		}
	}
	// Avoid offering a file identity a second time when the agent already has it.
	seen := make(map[string]bool)
	unique := make([]ssh.Signer, 0, len(signers))
	for _, signer := range signers {
		key := string(signer.PublicKey().Marshal())
		if !seen[key] {
			unique = append(unique, signer)
			seen[key] = true
		}
	}
	if len(unique) == 0 {
		if len(keyErrors) > 0 {
			return nil, cleanup, errors.Join(keyErrors...)
		}
		return nil, cleanup, errors.New("未找到可用的 SSH key，请在共享凭据中指定本机私钥路径，或将密钥加载到 SSH agent")
	}
	return unique, cleanup, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func dial(ctx context.Context, addr string, p Profile, cb ssh.HostKeyCallback) (*ssh.Client, net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH 连接失败：%w", err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	if limit, ok := ctx.Deadline(); ok {
		deadline = minTime(deadline, limit)
	}
	_ = conn.SetDeadline(deadline)
	id, digest, preferKey := sshPreference(p, addr)
	method := ""
	cleanupKeys := func() {}
	defer func() { cleanupKeys() }()
	keyAuth := ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		method = "key"
		var signers []ssh.Signer
		var err error
		signers, cleanupKeys, err = sshKeySigners(ctx, p, deadline)
		return signers, err
	})
	var auth []ssh.AuthMethod
	if preferKey || p.Password == "" {
		auth = append(auth, keyAuth)
	}
	if p.Password != "" {
		auth = append(auth, ssh.PasswordCallback(func() (string, error) {
			method = "password"
			return p.Password, nil
		}), ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			method = "password"
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = p.Password
			}
			return answers, nil
		}))
	}
	if !preferKey && p.Password != "" {
		auth = append(auth, keyAuth)
	}
	// Methods are tried within one handshake. Failed host trust or transport
	// never triggers a new connection or a replay of an application command.
	c, ch, requests, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{User: p.Username, Auth: auth, HostKeyCallback: cb})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	rememberSSHAuth(id, digest, method)
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, ch, requests), &authenticatedConn{Conn: conn, method: method}, nil
}
