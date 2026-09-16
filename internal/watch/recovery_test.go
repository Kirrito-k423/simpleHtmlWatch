package watch

import (
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func startTrustedMonitor(t *testing.T, f *fixture, commands []string, timeout ...time.Duration) *Monitor {
	t.Helper()
	trust, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	addr := f.listener.Addr().String()
	_ = trust.Callback(addr, false)("", f.listener.Addr(), f.signer.PublicKey())
	if err = trust.Accept(addr, ssh.FingerprintSHA256(f.signer.PublicKey())); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(trust)
	if len(timeout) != 0 {
		m.commandTimeout = timeout[0]
	}
	t.Cleanup(m.Close)
	c := testConfig()
	c.Interval = 3600
	c.Machines[0].Port = f.listener.Addr().(*net.TCPAddr).Port
	c.Machines[0].Commands = commands
	m.Replace(c)
	return m
}

func TestIssue1MissingExitStatusKeepsCollectedOutput(t *testing.T) {
	f := newFixture(t)
	f.omitExitStatus.Store(true)
	m := startTrustedMonitor(t, f, []string{"npu", "python"})
	s := waitState(t, m, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "partial" || s.Error != "" || len(s.Results) != 2 || s.Results[0].Output != "NPU 0 OK\n" || s.Results[1].Output != "root 42 1 python train.py\n" {
		t.Fatalf("issue #1: a missing exit status hid collected output / stopped later commands: %+v", s)
	}
	if s.Results[0].Warning == "" || s.Results[0].Error != "" || s.Reconnects != 0 {
		t.Fatal("missing status was not marked as an uncertainty warning")
	}
	first := s.UpdatedAt
	m.Refresh()
	s = waitState(t, m, func(s map[string]State) bool { return s["m1"].UpdatedAt.After(first) })["m1"]
	if s.Status != "partial" || f.authenticated.Load() != 1 {
		t.Fatal("a live connection was unnecessarily replaced")
	}
}

func TestIssue1MissingExitStatusWithoutOutputIsNotSuccess(t *testing.T) {
	f := newFixture(t)
	f.omitExitStatus.Store(true)
	f.emptyNPU.Store(true)
	m := startTrustedMonitor(t, f, []string{"npu"})
	s := waitState(t, m, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "partial" || len(s.Results) != 1 || s.Results[0].Output != "" || s.Results[0].Warning == "" {
		t.Fatalf("empty unconfirmed output presented as success: %+v", s)
	}
}

func TestIssue1ReconnectsStaleSessionWithinSamePoll(t *testing.T) {
	f := newFixture(t)
	m := startTrustedMonitor(t, f, []string{"npu", "python"})
	first := waitState(t, m, func(s map[string]State) bool { return s["m1"].Status == "online" })["m1"]
	f.closeConnections()
	m.Refresh()
	s := waitState(t, m, func(s map[string]State) bool { return s["m1"].UpdatedAt.After(first.UpdatedAt) })["m1"]
	if s.Status != "online" || len(s.Results) != 2 || f.authenticated.Load() != 2 {
		t.Fatalf("dead cached connection was not recovered in the same poll: %+v, authentications=%d", s, f.authenticated.Load())
	}
}

func TestIssue1RetriesInterruptedCommandOnce(t *testing.T) {
	f := newFixture(t)
	f.dropNPUOnce.Store(true)
	m := startTrustedMonitor(t, f, []string{"npu", "python"})
	s := waitState(t, m, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "online" || len(s.Results) != 2 || f.npuRuns.Load() != 2 {
		t.Fatalf("interrupted read-only command was not retried once: %+v, runs=%d", s, f.npuRuns.Load())
	}
}

func TestIssue1PersistentDisconnectRemainsOfflineAndBounded(t *testing.T) {
	f := newFixture(t)
	f.dropAllNPU.Store(true)
	m := startTrustedMonitor(t, f, []string{"npu", "python"})
	s := waitState(t, m, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "offline" || s.Error == "" || f.npuRuns.Load() != 2 {
		t.Fatalf("persistent disconnection was hidden or retry count wrong: %+v, runs=%d", s, f.npuRuns.Load())
	}
}

func TestIssue1RealCommandFailureIsNotRetried(t *testing.T) {
	f := newFixture(t)
	f.failNPU.Store(true)
	m := startTrustedMonitor(t, f, []string{"npu", "python"})
	s := waitState(t, m, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "partial" || len(s.Results) != 2 || s.Results[0].Error != "Process exited with status 127" || f.npuRuns.Load() != 1 {
		t.Fatalf("real command failure was swallowed or retried: %+v, runs=%d", s, f.npuRuns.Load())
	}
}

func TestIssue1SilentConnectionProbeIsBounded(t *testing.T) {
	f := newFixture(t)
	f.omitExitStatus.Store(true)
	f.ignoreKeepalive.Store(true)
	start := time.Now()
	m := startTrustedMonitor(t, f, []string{"npu"}, 80*time.Millisecond)
	s := waitState(t, m, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "offline" || s.Error == "" || s.Reconnects != 1 || f.npuRuns.Load() != 2 || time.Since(start) > 2*time.Second {
		t.Fatalf("unresponsive probe bypassed timeout/retry limits: %+v, executions=%d", s, f.npuRuns.Load())
	}
}
