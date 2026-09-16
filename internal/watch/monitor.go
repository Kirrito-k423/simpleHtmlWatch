package watch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const maxOutput = 256 * 1024

type Result struct {
	CommandID string `json:"commandId"`
	Output    string `json:"output"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated"`
}
type State struct {
	MachineID   string     `json:"machineId"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	LastSuccess time.Time  `json:"lastSuccess"`
	DurationMs  int64      `json:"durationMs"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	Address     string     `json:"address,omitempty"`
	KeyChanged  bool       `json:"keyChanged,omitempty"`
	AutoTrustAt *time.Time `json:"autoTrustAt,omitempty"`
	Results     []Result   `json:"results"`
}
type Monitor struct {
	mu             sync.RWMutex
	states         map[string]State
	triggers       map[string]chan struct{}
	cancel         context.CancelFunc
	trust          *TrustStore
	slots          chan struct{}
	wg             sync.WaitGroup
	commandTimeout time.Duration
}

func NewMonitor(t *TrustStore) *Monitor {
	return &Monitor{trust: t, states: map[string]State{}, triggers: map[string]chan struct{}{}, slots: make(chan struct{}, 8), commandTimeout: 12 * time.Second}
}

// Replace is serialized by the HTTP configuration handler. Workers belong to one generation.
func (m *Monitor) Replace(c Config) {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	m.trust.ResetCountdowns()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Lock()
	m.states = map[string]State{}
	m.triggers = map[string]chan struct{}{}
	profiles := map[string]Profile{}
	for _, p := range c.Profiles {
		profiles[p.ID] = p
	}
	for _, host := range c.Machines {
		status := "connecting"
		if !host.Enabled {
			status = "disabled"
		}
		m.states[host.ID] = State{MachineID: host.ID, Status: status, Results: []Result{}}
		if !host.Enabled {
			continue
		}
		trigger := make(chan struct{}, 1)
		m.triggers[host.ID] = trigger
		m.wg.Add(1)
		go m.worker(ctx, host, profiles[host.ProfileID], time.Duration(c.Interval)*time.Second, c.AutoTrustNewKeys, trigger)
	}
	m.mu.Unlock()
}
func (m *Monitor) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}
func (m *Monitor) Snapshot() map[string]State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]State{}
	for k, v := range m.states {
		v.Results = append([]Result{}, v.Results...)
		out[k] = v
	}
	return out
}
func (m *Monitor) Refresh() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ch := range m.triggers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (m *Monitor) put(ctx context.Context, s State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() == nil {
		m.states[s.MachineID] = s
	}
}
func (m *Monitor) worker(ctx context.Context, host Machine, profile Profile, interval time.Duration, autoTrust bool, trigger <-chan struct{}) {
	defer m.wg.Done()
	var client *ssh.Client
	var conn net.Conn
	last := time.Time{}
	defer func() {
		if client != nil {
			client.Close()
		}
		if conn != nil {
			conn.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case m.slots <- struct{}{}:
		}
		start := time.Now()
		s := State{MachineID: host.ID, Status: "online", Results: []Result{}, LastSuccess: last}
		addr := net.JoinHostPort(host.Host, strconv.Itoa(host.Port))
		var err error
		if client == nil {
			client, conn, err = dial(ctx, addr, profile, m.trust.Callback(addr, autoTrust))
		}
		if err == nil {
			activeConn := conn
			stop := context.AfterFunc(ctx, func() { activeConn.Close() })
			for _, id := range host.Commands {
				cmd, _ := commandByID(id)
				_ = conn.SetDeadline(time.Now().Add(m.commandTimeout))
				r, transportErr := run(client, cmd)
				s.Results = append(s.Results, r)
				if transportErr != nil {
					err = transportErr
					break
				}
				if r.Error != "" {
					s.Status = "partial"
				}
			}
			stop()
			_ = conn.SetDeadline(time.Time{})
		}
		if err != nil {
			s.Status = "offline"
			s.Error = err.Error()
			var trustErr *TrustError
			if errors.As(err, &trustErr) {
				s.Status = "untrusted"
				s.Error = trustErr.Error()
				s.Fingerprint = trustErr.Fingerprint
				s.Address = trustErr.Address
				s.KeyChanged = trustErr.Changed
				if !trustErr.AutoTrustAt.IsZero() {
					deadline := trustErr.AutoTrustAt
					s.AutoTrustAt = &deadline
				}
			}
			if client != nil {
				client.Close()
				client = nil
			}
			if conn != nil {
				conn.Close()
				conn = nil
			}
		} else {
			last = time.Now()
			s.LastSuccess = last
		}
		s.UpdatedAt = time.Now()
		s.DurationMs = time.Since(start).Milliseconds()
		m.put(ctx, s)
		<-m.slots
		// Retry at the trust deadline even when the sampling interval is much longer.
		// Waiting does not occupy a connection slot and does not depend on an open browser.
		wait := interval
		if s.AutoTrustAt != nil {
			wait = time.Until(*s.AutoTrustAt)
			if wait < 0 {
				wait = 0
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-trigger:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func dial(ctx context.Context, addr string, p Profile, cb ssh.HostKeyCallback) (*ssh.Client, net.Conn, error) {
	d := net.Dialer{Timeout: 8 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH 连接失败：%w", err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	cfg := &ssh.ClientConfig{User: p.Username, Auth: []ssh.AuthMethod{ssh.Password(p.Password), ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range answers {
			answers[i] = p.Password
		}
		return answers, nil
	})}, HostKeyCallback: cb}
	c, ch, req, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, ch, req), conn, nil
}

type boundedOutput struct {
	mu        sync.Mutex
	b         strings.Builder
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := maxOutput - b.b.Len()
	if n > left {
		b.truncated = true
		p = p[:left]
	}
	b.b.Write(p)
	return n, nil
}
func run(client *ssh.Client, cmd Command) (Result, error) {
	r := Result{CommandID: cmd.ID}
	s, err := client.NewSession()
	if err != nil {
		return r, err
	}
	defer s.Close()
	var out boundedOutput
	s.Stdout = &out
	s.Stderr = &out
	// A login shell loads PATH on common bare-metal installations; no PTY or watch process is created.
	err = s.Run("bash -o pipefail -lc '" + strings.ReplaceAll(cmd.Shell, "'", "'\"'\"'") + "'")
	r.Output = out.b.String()
	r.Truncated = out.truncated
	if err != nil {
		r.Error = err.Error()
		var exitErr *ssh.ExitError
		if !errors.As(err, &exitErr) {
			return r, err
		}
	}
	return r, nil
}
