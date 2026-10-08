package watch

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type authFixture struct {
	addr        string
	hostKey     ssh.Signer
	passwords   atomic.Int32
	keys        atomic.Int32
	connections atomic.Int32
	rejectKeys  atomic.Bool
	mu          sync.Mutex
	trace       []string
}

func (f *authFixture) attempted(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trace = append(f.trace, method)
}

func (f *authFixture) takeTrace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	trace := f.trace
	f.trace = nil
	return trace
}

func newAuthFixture(t *testing.T, publicKey ssh.PublicKey, keyOnly ...bool) *authFixture {
	t.Helper()
	_, hostPrivate, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, _ := ssh.NewSignerFromKey(hostPrivate)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &authFixture{addr: listener.Addr().String(), hostKey: hostKey}
	config := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			f.passwords.Add(1)
			f.attempted("password")
			if string(password) != "correct-password" {
				return nil, errors.New("password rejected")
			}
			return nil, nil
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.keys.Add(1)
			f.attempted("key")
			if f.rejectKeys.Load() || !bytes.Equal(key.Marshal(), publicKey.Marshal()) {
				return nil, errors.New("key rejected")
			}
			return nil, nil
		},
	}
	if len(keyOnly) > 0 && keyOnly[0] {
		config.PasswordCallback = nil
	}
	config.AddHostKey(hostKey)
	var wg sync.WaitGroup
	var active sync.Map
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.connections.Add(1)
			active.Store(conn, true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer active.Delete(conn)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for request := range channels {
					channel, requests, err := request.Accept()
					if err != nil {
						continue
					}
					for request := range requests {
						if request.Type != "exec" {
							_ = request.Reply(false, nil)
							continue
						}
						_ = request.Reply(true, nil)
						_, _ = channel.Write([]byte("key-ok\n"))
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
						_ = channel.Close()
						break
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		active.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
		wg.Wait()
	})
	return f
}

func testSSHKey(t *testing.T, path, passphrase string) (ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(private, "test-only identity")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(private, "test-only identity", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(private)
	return private, signer.PublicKey()
}

func isolatedSSHHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	return home
}

func dialAuthFixture(t *testing.T, f *authFixture, profile Profile) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, conn, err := dial(ctx, f.addr, profile, func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if !bytes.Equal(key.Marshal(), f.hostKey.PublicKey().Marshal()) {
			return errors.New("test host key changed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer conn.Close()
	result, err := run(client, Command{ID: "test", Shell: "printf key-ok"})
	if err != nil || !strings.Contains(result.Output, "key-ok") {
		t.Fatalf("authenticated session command: %v / %s", err, result.Output)
	}
	return connectionAuthMethod(conn)
}

func TestSSHPasswordKeyFallbackAndPreference(t *testing.T) {
	home := isolatedSSHHome(t)
	keyPath := filepath.Join(home, ".ssh", "identity")
	_, publicKey := testSSHKey(t, keyPath, "")
	f := newAuthFixture(t, publicKey)
	profile := Profile{ID: "test", Username: "root", Password: "incorrect-password", PrivateKeyPath: keyPath}
	if method := dialAuthFixture(t, f, profile); method != "key" {
		t.Fatalf("fallback authenticated with %q", method)
	}
	trace := f.takeTrace()
	if len(trace) < 2 || trace[0] != "password" || trace[1] != "key" || f.connections.Load() != 1 {
		t.Fatalf("fallback must stay in one handshake: %v, connections=%d", trace, f.connections.Load())
	}
	passwords := f.passwords.Load()
	dialAuthFixture(t, f, profile)
	if trace := f.takeTrace(); len(trace) == 0 || trace[0] != "key" || f.passwords.Load() != passwords {
		t.Fatalf("subsequent connection did not prefer key: %v", trace)
	}
	// Separate hosts/users must still start with password.
	otherHost := newAuthFixture(t, publicKey)
	dialAuthFixture(t, otherHost, profile)
	if trace := otherHost.takeTrace(); trace[0] != "password" {
		t.Fatalf("preference crossed host boundary: %v", trace)
	}
	otherUser := profile
	otherUser.Username = "another-user"
	dialAuthFixture(t, f, otherUser)
	if trace := f.takeTrace(); trace[0] != "password" {
		t.Fatalf("preference crossed user boundary: %v", trace)
	}
	// Credential edits reset the preference, so a corrected password works first.
	profile.Password = "correct-password"
	if method := dialAuthFixture(t, f, profile); method != "password" {
		t.Fatalf("corrected password was not used: %s", method)
	}
	if trace := f.takeTrace(); len(trace) != 1 || trace[0] != "password" {
		t.Fatalf("valid password unnecessarily loaded a key: %v", trace)
	}
	// If a previously preferred key is revoked, password remains a fallback.
	profile.Password = "incorrect-password"
	dialAuthFixture(t, f, profile)
	f.takeTrace()
	f.rejectKeys.Store(true)
	// Keep the same credential digest while changing server acceptance.
	profile.Password = "correct-password"
	id, digest, _ := sshPreference(profile, f.addr)
	rememberSSHAuth(id, digest, "key")
	if method := dialAuthFixture(t, f, profile); method != "password" {
		t.Fatal("revoked key did not fall back to password")
	}
	if trace := f.takeTrace(); len(trace) < 2 || trace[0] != "key" || trace[len(trace)-1] != "password" {
		t.Fatalf("revoked-key fallback order: %v", trace)
	}
	_, _, preferred := sshPreference(profile, f.addr)
	if preferred {
		t.Fatal("password success retained rejected key preference")
	}
}

func TestSSHKeyOnlyEncryptedAndDefaultFiles(t *testing.T) {
	home := isolatedSSHHome(t)
	path := filepath.Join(home, ".ssh", "id_ed25519")
	_, publicKey := testSSHKey(t, path, "private-key-passphrase")
	f := newAuthFixture(t, publicKey)
	profile := Profile{Username: "root", PrivateKeyPath: "~/.ssh/id_ed25519", KeyPassphrase: "private-key-passphrase"}
	if method := dialAuthFixture(t, f, profile); method != "key" || f.passwords.Load() != 0 {
		t.Fatal("key-only authentication attempted password")
	}
	profile.PrivateKeyPath = ""
	dialAuthFixture(t, f, profile)
	for _, passphrase := range []string{"", "wrong-private-key-passphrase"} {
		profile.PrivateKeyPath = path
		profile.KeyPassphrase = passphrase
		_, _, err := dial(context.Background(), f.addr, profile, ssh.FixedHostKey(f.hostKey.PublicKey()))
		if err == nil || strings.Contains(err.Error(), "wrong-private-key-passphrase") || strings.Contains(err.Error(), "PRIVATE KEY-----") {
			t.Fatalf("encrypted-key rejection leaked a secret or succeeded: %v", err)
		}
	}
	profile.KeyPassphrase = ""
	profile.PrivateKeyPath = filepath.Join(home, "missing")
	if _, _, err := dial(context.Background(), f.addr, profile, ssh.FixedHostKey(f.hostKey.PublicKey())); err == nil {
		t.Fatal("missing explicit key was ignored")
	}
	// A valid password does not read an unavailable key file.
	profile.Password = "correct-password"
	if method := dialAuthFixture(t, f, profile); method != "password" {
		t.Fatal("password login unnecessarily required a private key")
	}
}

func TestSSHKeyOnlyServerAndCanceledHandshake(t *testing.T) {
	home := isolatedSSHHome(t)
	path := filepath.Join(home, ".ssh", "id_ed25519")
	_, publicKey := testSSHKey(t, path, "")
	f := newAuthFixture(t, publicKey, true)
	profile := Profile{Username: "root", Password: "incorrect-password"}
	if method := dialAuthFixture(t, f, profile); method != "key" || f.passwords.Load() != 0 {
		t.Fatal("publickey-only server could not authenticate a password-configured profile")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			buffer := make([]byte, 4096)
			for {
				if _, err := conn.Read(buffer); err != nil {
					return
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = dial(ctx, listener.Addr().String(), profile, ssh.FixedHostKey(f.hostKey.PublicKey()))
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("canceled handshake was not bounded: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled handshake leaked its TCP connection")
	}
}

func TestSSHKeyFallbackPreservesHostTrust(t *testing.T) {
	home := isolatedSSHHome(t)
	_, publicKey := testSSHKey(t, filepath.Join(home, "identity"), "")
	f := newAuthFixture(t, publicKey)
	trust, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{Username: "root", Password: "incorrect-password", PrivateKeyPath: filepath.Join(home, "unreadable-key")}
	_, _, err = dial(context.Background(), f.addr, profile, trust.Callback(f.addr, false))
	var untrusted *TrustError
	if !errors.As(err, &untrusted) || f.passwords.Load() != 0 || f.keys.Load() != 0 || f.connections.Load() != 1 {
		t.Fatalf("credentials or reconnect before host trust: %v", err)
	}
	_ = trust.Callback(f.addr, false)("", nil, publicKey)
	if err := trust.Accept(f.addr, ssh.FingerprintSHA256(publicKey)); err != nil {
		t.Fatal(err)
	}
	_, _, err = dial(context.Background(), f.addr, profile, trust.Callback(f.addr, false))
	if !errors.As(err, &untrusted) || !untrusted.Changed || f.passwords.Load() != 0 || f.keys.Load() != 0 {
		t.Fatalf("host key change bypassed verification: %v", err)
	}
}

func TestSSHAgentFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH_AUTH_SOCK agent is supported on macOS/Linux; Windows uses private-key files")
	}
	home := isolatedSSHHome(t)
	private, publicKey := testSSHKey(t, filepath.Join(home, "agent-only-identity"), "")
	dir, err := os.MkdirTemp("", "shw-agent-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "a.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var active sync.Map
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			active.Store(conn, true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer active.Delete(conn)
				defer conn.Close()
				_ = agent.ServeAgent(keyring, conn)
			}()
		}
	}()
	defer func() {
		listener.Close()
		active.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
		wg.Wait()
	}()
	t.Setenv("SSH_AUTH_SOCK", socket)
	f := newAuthFixture(t, publicKey)
	if method := dialAuthFixture(t, f, Profile{Username: "root", Password: "incorrect-password"}); method != "key" {
		t.Fatal("agent key was not used after password failure")
	}
}

func TestSSHKeyCredentialStorage(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig()
	config.Profiles[0].PrivateKeyPath = "~/.ssh/id_ed25519"
	config.Profiles[0].KeyPassphrase = "unique-key-passphrase"
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	public := store.Public()
	encoded, _ := json.Marshal(public)
	if public.Profiles[0].KeyPassphrase != "" || !public.Profiles[0].HasKeyPassphrase || bytes.Contains(encoded, []byte("unique-key-passphrase")) {
		t.Fatal("public config exposed private-key passphrase")
	}
	ciphertext, _ := os.ReadFile(filepath.Join(store.dir, "config.enc"))
	if bytes.Contains(ciphertext, []byte("unique-key-passphrase")) {
		t.Fatal("private-key passphrase was not encrypted")
	}
	public.Machines[0].Name = "changed"
	if err := store.Save(public); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(store.dir)
	if err != nil || reopened.Snapshot().Profiles[0].KeyPassphrase != "unique-key-passphrase" {
		t.Fatal("blank edit or restart lost private-key passphrase")
	}
	public.Profiles[0].ClearPassword = true
	if err := store.Save(public); err != nil {
		t.Fatal(err)
	}
	if profile := store.Snapshot().Profiles[0]; profile.Password != "" || profile.ClearPassword {
		t.Fatal("key-only edit retained password or edit flag")
	}
	public = store.Public()
	public.Profiles[0].PrivateKeyPath = "~/.ssh/other_identity"
	if err := store.Save(public); err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Profiles[0].KeyPassphrase != "" {
		t.Fatal("changed key reused another key's passphrase")
	}
	for _, path := range []string{"relative/key", "~/key\x00bad"} {
		config.Profiles[0].PrivateKeyPath = path
		if err := store.Save(config); err == nil {
			t.Fatal("invalid key path accepted")
		}
	}
}

func TestSSHKeySharedByMonitorExecutionAndTaskRemote(t *testing.T) {
	home := isolatedSSHHome(t)
	path := filepath.Join(home, "identity")
	_, publicKey := testSSHKey(t, path, "")
	f := newAuthFixture(t, publicKey)
	trust, _ := NewTrustStore(t.TempDir())
	_ = trust.Callback(f.addr, false)("", nil, f.hostKey.PublicKey())
	if err := trust.Accept(f.addr, ssh.FingerprintSHA256(f.hostKey.PublicKey())); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(f.addr)
	var portNumber int
	_, _ = fmt.Sscanf(port, "%d", &portNumber)
	config := testConfig()
	config.Profiles[0].Password, config.Profiles[0].PrivateKeyPath = "incorrect-password", path
	config.Machines[0].Host, config.Machines[0].Port = host, portNumber
	monitor := NewMonitor(trust)
	defer monitor.Close()
	monitor.Replace(config)
	for i := 0; i < 100 && monitor.Snapshot()["m1"].Status != "online"; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if state := monitor.Snapshot()["m1"]; state.Status != "online" || state.AuthMethod != "key" {
		t.Fatalf("monitor did not use key fallback: %+v", state)
	}
	passwords := f.passwords.Load()
	executor := NewExecutor(trust)
	defer executor.Close()
	request := ExecutionRequest{ID: "ssh-key-execution", Shell: "printf key-ok", Targets: []ExecutionTarget{{ID: "m1", Host: host, Port: portNumber, Username: "root"}}}
	if _, _, err := executor.Start(request, config); err != nil {
		t.Fatal(err)
	}
	completed := false
	for i := 0; i < 100; i++ {
		job, _ := executor.Get(request.ID)
		if job.FinishedAt != nil {
			if len(job.Results) != 1 || job.Results[0].Result.Error != "" || !strings.Contains(job.Results[0].Result.Output, "key-ok") {
				t.Fatalf("one-shot execution failed: %+v", job)
			}
			completed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !completed {
		t.Fatal("one-shot execution did not finish")
	}
	remote := &sshTaskRemote{trust: trust}
	result, err := remote.command(context.Background(), TaskJob{Host: host, Port: portNumber}, config.Profiles[0], "printf key-ok")
	if err != nil || !strings.Contains(result.Stdout, "key-ok") || f.passwords.Load() != passwords {
		t.Fatalf("shared preference not used by execution/task: %v, password attempts %d -> %d", err, passwords, f.passwords.Load())
	}
}
