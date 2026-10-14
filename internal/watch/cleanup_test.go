package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCleanupRemote struct {
	fakeTaskRemote
	snapshot          CleanupSnapshot
	inspects, signals int
	signalError       error
	hook              func(cleanupRequest)
}

func (f *fakeCleanupRemote) Cleanup(_ context.Context, _ Machine, _ Profile, q cleanupRequest) (CleanupSnapshot, string, error) {
	if f.hook != nil {
		f.hook(q)
	}
	if q.Mode == "terminate" {
		f.signals++
		return CleanupSnapshot{}, `{"state":"terminated","exited":true}`, f.signalError
	}
	f.inspects++
	s := f.snapshot
	s.Logs = append([]CleanupLog(nil), f.snapshot.Logs...)
	s.PID = q.PID
	return s, "", nil
}
func busyCleanupTable(pid string) string {
	return strings.Replace(idleNPUTable(), "| No running processes found |", "| 0 | 0 | "+pid+" | python | 302 |", 1)
}
func cleanupFixture(t *testing.T) (*TaskManager, *Monitor, *time.Time, *fakeCleanupRemote, ContinuationRequest) {
	t.Helper()
	remote := &fakeCleanupRemote{}
	m, mon, c, store := setupTasks(t, remote)
	t.Cleanup(m.Close)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, chinaTime)
	m.now = func() time.Time { return now }
	for i := 1; i < 4; i++ {
		h := c.Machines[0]
		h.ID = string(rune('a' + i))
		h.Host = "192.0.2." + string(rune('1'+i))
		c.Machines = append(c.Machines, h)
	}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(mon, c, now)
	mon.mu.Lock()
	s := mon.states[c.Machines[0].ID]
	s.Results[0].Stdout = busyCleanupTable("42")
	mon.states[c.Machines[0].ID] = s
	mon.mu.Unlock()
	r := ContinuationRequest{ID: "large-wait", ExperimentID: "large-exp", StepID: "step", Target: ContinuationTarget{Provider: "codex", HostID: "local", ThreadID: "original-thread", Workspace: t.TempDir()}, Resources: ResourceRequirements{Group: "gpu", MachineCount: 4, Model: "910B", DevicesPerMachine: 2, MinFreeHBMMB: 60000, MaxUtilization: 5, MaxSampleAge: 60}, Checkpoint: ContinuationCheckpoint{Path: filepath.Join(t.TempDir(), "checkpoint"), Digest: strings.Repeat("a", 64)}, Authorized: true, AuthorizationRef: "explicit-user-authorization", Tasks: []ContinuationTask{{ID: "t1", Shell: "true"}, {ID: "t2", Shell: "true"}, {ID: "t3", Shell: "true"}, {ID: "t4", Shell: "true"}}}
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	remote.snapshot = CleanupSnapshot{Schema: "simplehtmlwatch.cleanup.remote.v1", BootID: "boot", PID: 42, StartTicks: 100, ParentPID: 40, ParentStartTicks: 80, CommandHash: strings.Repeat("b", 64), ParentCommandHash: strings.Repeat("c", 64), Cwd: "/test/moe", State: "S", Uptime: 1000, AgeSeconds: 900, CPUSeconds: 1, OrphanForkserver: true, Failure: true, NPU: busyCleanupTable("42"), Logs: []CleanupLog{{FD: "1", Size: 100, MtimeNS: 100, Hash: strings.Repeat("d", 64), Tail: "[ERROR] Task failed!"}}}
	if err := m.SetCleanupPolicy(CleanupPolicy{Enabled: true, AuthorizationRef: "user-authorized-cleanup"}); err != nil {
		t.Fatal(err)
	}
	return m, mon, &now, remote, r
}
func cleanupAdvance(m *TaskManager, mon *Monitor, now *time.Time, remote *fakeCleanupRemote) {
	*now = now.Add(time.Minute)
	remote.snapshot.Uptime += 60
	remote.snapshot.CPUSeconds += 0.01
	mon.mu.Lock()
	for k, s := range mon.states {
		s.UpdatedAt = *now
		for i := range s.Results {
			s.Results[i].EndedAt = *now
		}
		mon.states[k] = s
	}
	mon.mu.Unlock()
}
func runCleanupFiveMinutes(t *testing.T, m *TaskManager, mon *Monitor, now *time.Time, remote *fakeCleanupRemote) {
	t.Helper()
	for i := 0; i <= 5; i++ {
		if i > 0 {
			cleanupAdvance(m, mon, now, remote)
		}
		if err := m.CheckCleanup(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCleanupConfirmedResidueAndIndependentVerification(t *testing.T) {
	m, mon, now, remote, _ := cleanupFixture(t)
	runCleanupFiveMinutes(t, m, mon, now, remote)
	if remote.signals != 1 {
		t.Fatalf("signals=%d", remote.signals)
	}
	for _, a := range m.cleanup.Audits {
		if a.SignalIntentAt == nil {
			t.Fatal("missing durable intent")
		}
		b, e := os.ReadFile(filepath.Join(m.cleanupDir(), a.ID+".json"))
		if e != nil || !strings.Contains(string(b), "signalIntentAt") {
			t.Fatal("intent not persisted")
		}
	}
	remote.snapshot = CleanupSnapshot{Schema: "simplehtmlwatch.cleanup.remote.v1", BootID: "boot", PID: 42, Missing: true, NPU: idleNPUTable()}
	cleanupAdvance(m, mon, now, remote)
	if err := m.CheckCleanup(); err != nil {
		t.Fatal(err)
	}
	for _, a := range m.cleanup.Audits {
		if a.Phase != "verified" {
			t.Fatalf("phase=%s", a.Phase)
		}
	}
	if remote.signals != 1 {
		t.Fatal("signal replay")
	}
}
func TestCleanupDebugUnknownAndProgressNeverSignal(t *testing.T) {
	cases := map[string]func(*fakeCleanupRemote){"debug": func(f *fakeCleanupRemote) { f.snapshot.Debug = true }, "interactive": func(f *fakeCleanupRemote) { f.snapshot.Interactive = true }, "stopped": func(f *fakeCleanupRemote) { f.snapshot.State = "T" }, "parent_alive": func(f *fakeCleanupRemote) { f.snapshot.OrphanForkserver = false }, "no_failure": func(f *fakeCleanupRemote) { f.snapshot.Failure = false }, "young": func(f *fakeCleanupRemote) { f.snapshot.AgeSeconds = 299 }, "children": func(f *fakeCleanupRemote) { f.snapshot.Children = true }, "siblings": func(f *fakeCleanupRemote) { f.snapshot.Siblings = true }, "missing_logs": func(f *fakeCleanupRemote) { f.snapshot.Logs = nil }, "normal_cpu_progress": func(f *fakeCleanupRemote) {
		f.hook = func(q cleanupRequest) {
			if q.Mode == "inspect" {
				f.snapshot.CPUSeconds += 1
			}
		}
	}, "log_progress": func(f *fakeCleanupRemote) { f.hook = func(q cleanupRequest) { f.snapshot.Logs[0].Size++ } }, "identity_changes": func(f *fakeCleanupRemote) { f.hook = func(q cleanupRequest) { f.snapshot.StartTicks++ } }, "device_busy": func(f *fakeCleanupRemote) {
		f.snapshot.NPU = strings.Replace(busyCleanupTable("42"), "0 0 / 0 1000", "80 0 / 0 1000", 1)
	}}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m, mon, now, f, _ := cleanupFixture(t)
			change(f)
			runCleanupFiveMinutes(t, m, mon, now, f)
			if f.signals != 0 {
				t.Fatal("unsafe signal")
			}
			if len(m.cleanup.Audits) == 0 {
				t.Fatal("missing diagnostic log")
			}
		})
	}
}
func TestCleanupQueueScopeAndOwnedMachines(t *testing.T) {
	for _, name := range []string{"disabled", "three_nodes", "paused", "expired", "prior_unknown", "reservation", "managed_unknown", "wrong_group", "enough_idle"} {
		t.Run(name, func(t *testing.T) {
			m, _, now, f, r := cleanupFixture(t)
			m.mu.Lock()
			c := m.continuations[r.ID]
			h := m.store.Snapshot().Machines[0]
			switch name {
			case "disabled":
				m.cleanup.Policy.Enabled = false
			case "three_nodes":
				c.Resources.MachineCount = 3
			case "paused":
				c.State = "paused"
			case "expired":
				c.ExpiresAt = now
			case "prior_unknown":
				c.ExistingTaskIDs = []string{"missing"}
			case "reservation":
				m.reservations["hold"] = Reservation{ID: "hold", MachineID: h.ID, ResourceKey: machineResource(h), Host: h.Host}
			case "managed_unknown":
				m.jobs["active"] = &TaskJob{TaskRequest: TaskRequest{ID: "active"}, SelectedMachineID: h.ID, Host: h.Host, Status: "unknown"}
			case "wrong_group":
				c.Resources.Group = "other"
			case "enough_idle":
				cfg := m.store.Snapshot()
				extra := cfg.Machines[1]
				extra.ID = "extra"
				extra.Host = "192.0.2.99"
				cfg.Machines = append(cfg.Machines, extra)
				if err := m.store.Save(cfg); err != nil {
					t.Fatal(err)
				}
				refreshContinuationSamples(m.monitor, cfg, *now)
			}
			m.continuations[r.ID] = c
			m.mu.Unlock()
			if err := m.CheckCleanup(); err != nil {
				t.Fatal(err)
			}
			if f.inspects != 0 || f.signals != 0 {
				t.Fatal("out-of-scope diagnostic")
			}
		})
	}
}
func TestCleanupPauseDuringDiagnosticAndStorageFailure(t *testing.T) {
	for _, name := range []string{"pause", "storage"} {
		t.Run(name, func(t *testing.T) {
			m, mon, now, f, r := cleanupFixture(t)
			for i := 0; i < 5; i++ {
				if i > 0 {
					cleanupAdvance(m, mon, now, f)
				}
				if err := m.CheckCleanup(); err != nil {
					t.Fatal(err)
				}
			}
			cleanupAdvance(m, mon, now, f)
			f.hook = func(q cleanupRequest) {
				if q.Mode != "inspect" {
					return
				}
				if name == "pause" {
					m.mu.Lock()
					c := m.continuations[r.ID]
					c.State = "paused"
					m.continuations[r.ID] = c
					m.mu.Unlock()
				} else {
					if err := os.RemoveAll(m.cleanupDir()); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := m.CheckCleanup()
			if name == "storage" && err == nil {
				t.Fatal("expected storage failure")
			}
			if f.signals != 0 {
				t.Fatal("signal despite pause/storage failure")
			}
		})
	}
}
func TestCleanupUncertainAndRestartNeverReplay(t *testing.T) {
	m, mon, now, f, _ := cleanupFixture(t)
	f.signalError = errors.New("SSH accepted, reply lost")
	runCleanupFiveMinutes(t, m, mon, now, f)
	if f.signals != 1 {
		t.Fatal(f.signals)
	}
	if err := m.loadCleanup(); err != nil {
		t.Fatal(err)
	}
	f.signalError = nil
	runCleanupFiveMinutes(t, m, mon, now, f)
	if f.signals != 1 {
		t.Fatal("replayed after restart")
	}
}
func TestCleanupBusyGateAndParserConservatism(t *testing.T) {
	m, mon, now, f, _ := cleanupFixture(t)
	f.hook = func(q cleanupRequest) {
		if q.Mode == "inspect" && len(m.Ready("", "gpu")) != 3 {
			t.Fatal("diagnostic machine not protected")
		}
	}
	if err := m.CheckCleanup(); err != nil {
		t.Fatal(err)
	}
	req := ResourceRequirements{Model: "910B", DevicesPerMachine: 2, MinFreeHBMMB: 60000, MaxSampleAge: 60}
	zero := 0
	for _, output := range []string{strings.Replace(busyCleanupTable("42"), "| 0 | 0 | 42", "| | 0 | 42", 1), busyCleanupTable("42") + "\n| 1 | 0 | 99 | python | 302 |", strings.Replace(busyCleanupTable("42"), "Process id", "PID", 1)} {
		s := State{Results: []Result{{CommandID: "npu", Stdout: output, ExitCode: &zero, EndedAt: *now}}}
		if _, e := cleanupCandidate(s, req, *now); e == nil {
			t.Fatal("accepted incomplete/multi-PID table")
		}
	}
	if err := m.SetCleanupPolicy(CleanupPolicy{Enabled: true}); err == nil {
		t.Fatal("enable without authorization")
	}
	if _, e := m.CleanupStatus("missing"); e == nil {
		t.Fatal("missing log found")
	}
	_ = mon
}

func TestCleanupRestartRestartsObservationAndLostDiagnosticIsLogged(t *testing.T) {
	m, mon, now, f, _ := cleanupFixture(t)
	for i := 0; i < 5; i++ {
		if i > 0 {
			cleanupAdvance(m, mon, now, f)
		}
		if err := m.CheckCleanup(); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.loadCleanup(); err != nil {
		t.Fatal(err)
	}
	cleanupAdvance(m, mon, now, f)
	if err := m.CheckCleanup(); err != nil {
		t.Fatal(err)
	}
	if f.signals != 0 {
		t.Fatal("reused pre-restart observation")
	}
}

func TestCleanupAuditReadDoesNotShareMutableEvents(t *testing.T) {
	m, _, _, _, _ := cleanupFixture(t)
	if err := m.CheckCleanup(); err != nil {
		t.Fatal(err)
	}
	for id := range m.cleanup.Audits {
		value, err := m.CleanupStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		a := value["audit"].(CleanupAudit)
		a.Events[0].Reason = "caller mutation"
		again, _ := m.CleanupStatus(id)
		if again["audit"].(CleanupAudit).Events[0].Reason == "caller mutation" {
			t.Fatal("shared mutable event array")
		}
	}
}

func TestCleanupResidueCanHoldTheRequiredHBM(t *testing.T) {
	m, mon, now, f, r := cleanupFixture(t)
	output := strings.Replace(busyCleanupTable("42"), "1000 / 65536", "62000 / 65536", 1)
	mon.mu.Lock()
	for id, state := range mon.states {
		if strings.Contains(state.Results[0].Stdout, "42") {
			state.Results[0].Stdout = output
			mon.states[id] = state
		}
	}
	mon.mu.Unlock()
	f.snapshot.NPU = output
	f.hook = func(q cleanupRequest) {
		if q.Mode == "terminate" && q.Requirements.MinFreeHBMMB != 0 {
			t.Fatal("requiring free HBM before reclaiming it")
		}
	}
	runCleanupFiveMinutes(t, m, mon, now, f)
	if f.signals != 1 {
		t.Fatal("failed residue excluded by its own HBM allocation")
	}
	zero := 0
	idle := strings.Replace(output, "| 0 | 0 | 42 | python | 302 |", "| No running processes found |", 1)
	state := State{Results: []Result{{CommandID: "npu", Stdout: idle, ExitCode: &zero, EndedAt: *now}}}
	if _, _, err := resourceSample(Machine{}, state, r.Resources, *now); err == nil {
		t.Fatal("original allocation HBM budget was relaxed")
	}
}

func TestCleanupSlowDiagnosticsDoNotStarveOtherMachines(t *testing.T) {
	m, mon, now, f, _ := cleanupFixture(t)
	mon.mu.Lock()
	for id, state := range mon.states {
		state.Results[0].Stdout = busyCleanupTable("42")
		mon.states[id] = state
	}
	mon.mu.Unlock()
	for i := 0; i < 4; i++ {
		if err := m.CheckCleanup(); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(20 * time.Second)
		f.snapshot.Uptime += 20
		mon.mu.Lock()
		for id, state := range mon.states {
			state.UpdatedAt = *now
			state.Results[0].EndedAt = *now
			mon.states[id] = state
		}
		mon.mu.Unlock()
	}
	if len(m.cleanup.Audits) != 4 {
		t.Fatalf("slow first nodes starved another candidate: audits=%d", len(m.cleanup.Audits))
	}
}
