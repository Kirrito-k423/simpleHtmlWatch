package watch

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func idleNPUTable() string {
	return "| NPU Name | Health | Power(W) Temp(C) |\n" +
		"| Chip Device | Bus-Id | AICore(%) Memory-Usage(MB) HBM-Usage(MB) |\n" +
		"| 0 910B | OK | 100 40 |\n| 0 0 | 0000:01:00.0 | 0 0 / 0 1000 / 65536 |\n" +
		"| 1 910B | OK | 100 40 |\n| 0 1 | 0000:02:00.0 | 0 0 / 0 1000 / 65536 |\n" +
		"| NPU ID | Chip ID | Process id | Process name | Process memory(MB) |\n" +
		"| No running processes found |\n+--------------------------+\n"
}

func continuationFixture(t *testing.T) (*TaskManager, *Monitor, Config, *Store, *time.Time, ContinuationRequest, *fakeTaskRemote) {
	t.Helper()
	remote := &fakeTaskRemote{}
	m, monitor, c, store := setupTasks(t, remote)
	t.Cleanup(m.Close)
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, chinaTime)
	m.now = func() time.Time { return now }
	other := c.Machines[0]
	other.ID, other.Name, other.Host = "second", "second", "192.0.2.42"
	c.Machines = append(c.Machines, other)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, now)
	r := ContinuationRequest{ID: "wait-one", ExperimentID: "exp-one", StepID: "step-one",
		Target:     ContinuationTarget{Provider: "codex", HostID: "local", ThreadID: "original-thread", Workspace: t.TempDir()},
		Resources:  ResourceRequirements{Group: "gpu", MachineCount: 2, Model: "910B", DevicesPerMachine: 2, MaxUtilization: 5, MinFreeHBMMB: 60000},
		Checkpoint: ContinuationCheckpoint{Path: filepath.Join(t.TempDir(), "checkpoint.json"), Digest: strings.Repeat("a", 64)},
		Authorized: true, AuthorizationRef: "user-authorized-experiment",
		Tasks: []ContinuationTask{{ID: "exp-task-1", Shell: "printf one"}, {ID: "exp-task-2", Shell: "printf two"}}}
	return m, monitor, c, store, &now, r, remote
}

func refreshContinuationSamples(monitor *Monitor, config Config, at time.Time) {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	code := 0
	for _, machine := range config.Machines {
		monitor.states[machine.ID] = State{MachineID: machine.ID, Status: "online", UpdatedAt: at, Results: []Result{{CommandID: "npu", Stdout: idleNPUTable(), ExitCode: &code, EndedAt: at}}}
	}
}

func offerAndClaim(t *testing.T, m *TaskManager, r ContinuationRequest) Continuation {
	t.Helper()
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.GetContinuation(r.ID)
	if c.State != "offered" {
		t.Fatalf("not offered: %+v", c)
	}
	claimed, _, err := m.ClaimContinuation(claimFor(c))
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func claimFor(c Continuation) ContinuationClaim {
	return ContinuationClaim{ID: c.ID, Generation: c.Generation, WakeEventID: c.Wake.ID, ThreadID: c.Target.ThreadID, CheckpointDigest: c.Checkpoint.Digest}
}

func taskFor(c Continuation, index int) TaskRequest {
	a := c.Allocations[index]
	return TaskRequest{ID: a.TaskID, Shell: c.Tasks[index].Shell, MachineID: a.MachineID, ContinuationID: c.ID, GrantID: c.GrantID, Generation: c.Generation}
}

func TestContinuationCurrentNPUFactsVetoAllocation(t *testing.T) {
	for name, alter := range map[string]func(*State){
		"one_busy_chip": func(s *State) {
			s.Results[0].Stdout = strings.Replace(idleNPUTable(), "0 0 / 0 1000", "80 0 / 0 1000", 1)
		},
		"process": func(s *State) {
			s.Results[0].Stdout = strings.Replace(idleNPUTable(), "| No running processes found |", "| 0 | 0 | 99 | train | 100 |", 1)
		},
		"process_table_unknown": func(s *State) { s.Results[0].Stdout = strings.Split(idleNPUTable(), "| NPU ID")[0] },
		"stale_npu_fresh_ssh":   func(s *State) { s.Results[0].EndedAt = s.UpdatedAt.Add(-2 * time.Minute) },
		"truncated":             func(s *State) { s.Results[0].Truncated = true },
		"unknown_exit":          func(s *State) { s.Results[0].ExitCode = nil },
		"hbm": func(s *State) {
			s.Results[0].Stdout = strings.ReplaceAll(idleNPUTable(), "1000 / 65536", "65000 / 65536")
		},
		"missing_chip": func(s *State) {
			s.Results[0].Stdout = strings.Replace(idleNPUTable(), "| 0 1 | 0000:02:00.0 | 0 0 / 0 1000 / 65536 |\n", "", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, monitor, config, _, _, r, _ := continuationFixture(t)
			monitor.mu.Lock()
			s := monitor.states[config.Machines[0].ID]
			alter(&s)
			monitor.states[s.MachineID] = s
			monitor.mu.Unlock()
			if _, _, err := m.RegisterContinuation(r); err != nil {
				t.Fatal(err)
			}
			if err := m.CheckContinuations(); err != nil {
				t.Fatal(err)
			}
			job, _ := m.GetContinuation(r.ID)
			if job.State != "waiting_resources" || job.EligibleCount != 1 || len(job.Allocations) != 0 || job.Excluded[config.Machines[0].ID] == "" {
				t.Fatalf("unsafe eligibility: %+v", job)
			}
		})
	}
	if devices, err := parseNPUResources(idleNPUTable()); err != nil || len(devices) != 2 {
		t.Fatalf("idle A2/A3 parse: %+v %v", devices, err)
	}
	if _, err := parseNPUResources(strings.Replace(npuTable("0", "0"), "| 0 | 42 | python | 100 |", "| No running processes found |", 1)); err != nil {
		t.Fatalf("A5 layout parser: %v", err)
	}
}

func TestContinuationGangAllocationFencesAllEntrypoints(t *testing.T) {
	m, _, config, _, _, r, _ := continuationFixture(t)
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	other := r
	other.ID, other.ExperimentID = "wait-two", "exp-two"
	other.Tasks = []ContinuationTask{{ID: "other-1", Shell: "echo one"}, {ID: "other-2", Shell: "echo two"}}
	if _, _, err := m.RegisterContinuation(other); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.CheckContinuations(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	a, _ := m.GetContinuation(r.ID)
	b, _ := m.GetContinuation(other.ID)
	if a.State != "offered" || b.State != "waiting_resources" || len(a.Allocations) != 2 || len(b.Allocations) != 0 {
		t.Fatalf("gang conflict: %+v %+v", a, b)
	}
	if len(m.Ready("", "")) != 0 {
		t.Fatal("legacy ready ignored offer")
	}
	if _, status, _ := m.Reserve("intruder", config.Machines[0].ID); status != 409 {
		t.Fatal("legacy reservation stole offer")
	}
	if _, status, _ := m.Submit(TaskRequest{ID: "intruder", Shell: "echo steal", MachineID: config.Machines[0].ID}); status != 409 {
		t.Fatal("legacy task stole offer")
	}
	if _, status, _ := m.Submit(TaskRequest{ID: r.Tasks[0].ID, Shell: r.Tasks[0].Shell}); status != 409 {
		t.Fatal("planned ID bypassed claim")
	}
	for _, machine := range m.Machines() {
		if len(machine.ContinuationIDs) != 1 || machine.Ready {
			t.Fatalf("inventory missed occupancy: %+v", machine)
		}
	}
	wrong := claimFor(a)
	wrong.ThreadID = "another-thread"
	if _, status, _ := m.ClaimContinuation(wrong); status != 409 {
		t.Fatal("wrong original thread claimed")
	}
}

func TestContinuationAliasCountingAndContinuousIdle(t *testing.T) {
	m, monitor, config, store, now, r, _ := continuationFixture(t)
	alias := config.Machines[0]
	alias.ID = "alias"
	alias.ResourceID = "contradictory-alias-identity"
	config.Machines = append(config.Machines, alias)
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, config, *now)
	r.Resources.MachineCount = 3
	r.Tasks = append(r.Tasks, ContinuationTask{ID: "task-three", Shell: "echo three"})
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	job, _ := m.GetContinuation(r.ID)
	if job.EligibleCount != 2 || job.State != "waiting_resources" {
		t.Fatalf("aliases counted: %+v", job)
	}
	m2, mon2, config2, _, now2, r2, _ := continuationFixture(t)
	r2.Policy.StableFor = 600
	if _, _, err := m2.RegisterContinuation(r2); err != nil {
		t.Fatal(err)
	}
	if err := m2.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	*now2 = now2.Add(600 * time.Second)
	refreshContinuationSamples(mon2, config2, *now2)
	if err := m2.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	job, _ = m2.GetContinuation(r2.ID)
	if job.State != "waiting_resources" {
		t.Fatal("two endpoints treated as continuous idle")
	}
	for range 150 {
		*now2 = now2.Add(4 * time.Second)
		refreshContinuationSamples(mon2, config2, *now2)
		if err := m2.CheckContinuations(); err != nil {
			t.Fatal(err)
		}
	}
	job, _ = m2.GetContinuation(r2.ID)
	if job.State != "offered" {
		t.Fatalf("continuous samples did not offer: %+v", job)
	}
}

func TestContinuationWindowsAndUnusedExpiry(t *testing.T) {
	for _, test := range []struct {
		h, m int
		want bool
	}{{12, 29, false}, {12, 30, true}, {13, 59, true}, {14, 0, false}, {2, 29, false}, {2, 30, true}, {5, 59, true}, {6, 0, false}} {
		at := time.Date(2026, 10, 10, test.h, test.m, 0, 0, chinaTime)
		if continuationLowPeak(at.UTC()) != test.want {
			t.Fatalf("wrong Shanghai boundary %v", at)
		}
	}
	p := ContinuationPolicy{CheckEvery: 600, WindowCheckEvery: 60}
	at := time.Date(2026, 10, 10, 12, 29, 0, 0, chinaTime)
	if nextContinuationCheck(at, p).Sub(at) != time.Minute {
		t.Fatal("missed window entry")
	}
	m, monitor, config, _, now, r, _ := continuationFixture(t)
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	old, _ := m.GetContinuation(r.ID)
	*now = now.Add(301 * time.Second)
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := m.ClaimContinuation(claimFor(old)); status != 410 {
		t.Fatal("expired offer claimed")
	}
	refreshContinuationSamples(monitor, config, *now)
	if _, _, err := m.ControlContinuation(ContinuationControl{ID: r.ID, Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ControlContinuation(ContinuationControl{ID: r.ID, Action: "resume"}); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.GetContinuation(r.ID)
	if c.Generation != old.Generation+1 {
		t.Fatal("generation did not fence late wake")
	}
	if _, _, err := m.ClaimContinuation(claimFor(c)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(24 * time.Hour)
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	if len(m.Ready("", "")) != 0 {
		t.Fatal("claimed grant expired")
	}
}

func TestContinuationTaskHandoffRecoveryAndReview(t *testing.T) {
	m, monitor, config, store, now, r, remote := continuationFixture(t)
	c := offerAndClaim(t, m, r)
	for i := range c.Tasks {
		request := taskFor(c, i)
		if _, status, err := m.Submit(request); err != nil || status != 202 {
			t.Fatalf("submit: %d %v", status, err)
		}
		if _, status, err := m.Submit(request); err != nil || status != 200 {
			t.Fatalf("replay: %d %v", status, err)
		}
		request.Shell += " changed"
		if _, status, _ := m.Submit(request); status != 409 {
			t.Fatal("changed task accepted")
		}
	}
	m.Close()
	remote.mu.Lock()
	remote.code = "done:0"
	remote.mu.Unlock()
	restarted, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.now = func() time.Time { return *now }
	refreshContinuationSamples(monitor, config, *now)
	if len(restarted.Ready("", "")) != 0 {
		t.Fatal("restart lost grant")
	}
	if _, status, _ := restarted.ReportContinuation(ContinuationReport{ContinuationClaim: claimFor(c), Action: "release-unused"}); status != 409 {
		t.Fatal("released launch intents")
	}
	remote.mu.Lock()
	remote.code = "done:0"
	remote.mu.Unlock()
	for _, planned := range c.Tasks {
		if !restarted.probe(planned.ID) {
			t.Fatal("recovery query did not confirm original task completion")
		}
		waitTask(t, restarted, planned.ID)
	}
	if err := restarted.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	review, _ := restarted.GetContinuation(r.ID)
	if review.State != "awaiting_review" || review.Wake.Kind != "tasks_finished" || review.Wake.ID == c.Wake.ID {
		t.Fatalf("missing review wake: %+v", review)
	}
	if _, _, err := restarted.ReportContinuation(ContinuationReport{ContinuationClaim: claimFor(review), Action: "complete", Outcome: "succeeded", Evidence: "results checked against original criterion"}); err != nil {
		t.Fatal(err)
	}
	if len(restarted.Ready("", "")) != 2 {
		t.Fatal("verified completed grant retained")
	}
	remote.mu.Lock()
	launches := remote.launch
	remote.mu.Unlock()
	if launches != 2 {
		t.Fatalf("remote tasks launched %d times", launches)
	}
}

func TestContinuationUncertainDeliveryAndPersistFailure(t *testing.T) {
	m, _, _, store, now, r, _ := continuationFixture(t)
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.GetContinuation(r.ID)
	d := ContinuationDelivery{ID: c.ID, Generation: c.Generation, WakeEventID: c.Wake.ID, SenderID: "bridge-one", Action: "begin"}
	if _, _, err := m.DeliveryContinuation(d); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := m.DeliveryContinuation(d); status != 409 {
		t.Fatal("event delivered twice")
	}
	d.Action, d.Reason = "uncertain", "response lost"
	if _, _, err := m.DeliveryContinuation(d); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(601 * time.Second)
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	c, _ = m.GetContinuation(r.ID)
	if c.State != "waiting_resources" || c.Wake.ID != d.WakeEventID || c.Wake.Status != "delivery_uncertain" {
		t.Fatalf("uncertain send replaced: %+v", c)
	}
	d.Action, d.TurnID, d.SenderID = "delivered", "original-turn", "new-bridge"
	if _, _, err := m.DeliveryContinuation(d); err != nil {
		t.Fatal(err)
	}
	// Filesystem failure must not mutate the live outbox through pointer aliasing.
	path := filepath.Join(store.dir, "tasks", "continuations.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	d.TurnID = "different-turn"
	if _, _, err := m.DeliveryContinuation(d); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	after, _ := m.GetContinuation(r.ID)
	if after.Wake.TurnID != "original-turn" {
		t.Fatal("failed save leaked mutation")
	}
}

func TestContinuationAPIIdentityAndHeartbeat(t *testing.T) {
	m, monitor, _, store, _, r, _ := continuationFixture(t)
	r.Policy.WakeMode = "heartbeat"
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	app.SetServiceInfo(ServiceInfo{RunID: "run-one"})
	body, _ := json.Marshal(r)
	call := func(method, path, token, run string, body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://127.0.0.1:9999"+path, bytes.NewReader(body))
		request.Header.Set("X-Watch-Token", token)
		request.Header.Set("X-Watch-Instance", run)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, request)
		return w
	}
	if w := call("POST", "/api/continuations", "wrong", "run-one", body); w.Code != 403 {
		t.Fatal("missing authentication")
	}
	if w := call("POST", "/api/continuations", app.token, "old-run", body); w.Code != 409 {
		t.Fatal("stale process accepted")
	}
	if w := call("POST", "/api/continuations", app.token, "run-one", body); w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.GetContinuation(r.ID)
	if c.State != "waiting_resources" {
		t.Fatal("heartbeat preallocated before waking")
	}
	if _, _, err := m.EvaluateContinuation(ContinuationControl{ID: r.ID}); err != nil {
		t.Fatal(err)
	}
	c, _ = m.GetContinuation(r.ID)
	if c.State != "offered" {
		t.Fatal("heartbeat did not allocate")
	}
	if _, _, err := m.ClaimContinuation(claimFor(c)); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationIdempotentEmptyCollectionsAndClockZones(t *testing.T) {
	m, _, _, _, now, r, _ := continuationFixture(t)
	r.Resources.Labels, r.ExistingTaskIDs = map[string]string{}, []string{}
	deadline := now.Add(time.Hour)
	r.ExpiresAt = &deadline
	if _, status, err := m.RegisterContinuation(r); err != nil || status != 201 {
		t.Fatalf("initial: %d %v", status, err)
	}
	if _, status, err := m.RegisterContinuation(r); err != nil || status != 200 {
		t.Fatalf("identical retry: %d %v", status, err)
	}
	r.Tasks[0].Shell = "changed"
	if _, status, _ := m.RegisterContinuation(r); status != 409 {
		t.Fatal("changed request reused ID")
	}
	c, _ := m.GetContinuation(r.ID)
	if c.Tasks[0].Shell != "printf one" {
		t.Fatal("caller mutated persisted plan")
	}
}

func TestContinuationSamplingGapWithinFreshnessResetsStability(t *testing.T) {
	m, _, config, _, now, r, _ := continuationFixture(t)
	r.Policy.WakeMode = "heartbeat"
	r.Policy.StableFor = config.Interval*2 + 1
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.EvaluateContinuation(ContinuationControl{ID: r.ID}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Duration(r.Policy.StableFor) * time.Second)
	c, _, err := m.EvaluateContinuation(ContinuationControl{ID: r.ID})
	if err != nil || c.State != "waiting_resources" || c.EligibleCount != 0 {
		t.Fatalf("missing samples counted as continuous: %+v %v", c, err)
	}
}

func TestContinuationIntentWithoutJobNeverRelaunchesAndModeSwitchFences(t *testing.T) {
	m, monitor, _, store, _, r, remote := continuationFixture(t)
	c := offerAndClaim(t, m, r)
	request := taskFor(c, 0)
	m.mu.Lock()
	err := m.continuationIntentLocked(request)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	restarted, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, status, _ := restarted.Submit(request); status != 409 {
		t.Fatal("recovered uncertain intent relaunched")
	}
	if _, status, _ := restarted.ControlContinuation(ContinuationControl{ID: r.ID, Action: "set-mode", WakeMode: "heartbeat"}); status != 409 {
		t.Fatal("active intent changed recovery host mode")
	}
	remote.mu.Lock()
	launches := remote.launch
	remote.mu.Unlock()
	if launches != 0 {
		t.Fatal("intent recovery sent remote command")
	}
	m2, _, _, _, _, r2, _ := continuationFixture(t)
	if _, _, err := m2.RegisterContinuation(r2); err != nil {
		t.Fatal(err)
	}
	if err := m2.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	old, _ := m2.GetContinuation(r2.ID)
	updated, _, err := m2.ControlContinuation(ContinuationControl{ID: r2.ID, Action: "set-mode", WakeMode: "heartbeat"})
	if err != nil || updated.State != "waiting_resources" || updated.Generation <= old.Generation || len(m2.Ready("", "")) != 2 {
		t.Fatalf("mode change did not fence old offer: %+v %v", updated, err)
	}
	if _, status, _ := m2.ClaimContinuation(claimFor(old)); status != 409 {
		t.Fatal("old mode event claimed")
	}
}

func TestContinuationScopeAndSSHIdentityChangesFenceHandoff(t *testing.T) {
	m, _, config, store, _, r, remote := continuationFixture(t)
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.GetContinuation(r.ID)
	for i := range config.Machines {
		if config.Machines[i].ID == c.Allocations[0].MachineID {
			config.Machines[i].Group = "outside-authorized-scope"
		}
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := m.ClaimContinuation(claimFor(c)); status != 409 {
		t.Fatal("machine moved outside scope still claimed")
	}
	m2, _, config2, store2, _, r2, _ := continuationFixture(t)
	c2 := offerAndClaim(t, m2, r2)
	config2.Profiles[0].Username = "different-user"
	if err := store2.Save(config2); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := m2.Submit(taskFor(c2, 0)); status != 409 {
		t.Fatal("changed SSH principal launched old plan")
	}
	remote.mu.Lock()
	launches := remote.launch
	remote.mu.Unlock()
	if launches != 0 {
		t.Fatal("changed scope started remote task")
	}
}

func TestContinuationSampleCompletingAfterTickEntryUsesObservationTime(t *testing.T) {
	m, monitor, config, _, now, r, _ := continuationFixture(t)
	if _, _, err := m.RegisterContinuation(r); err != nil {
		t.Fatal(err)
	}
	observed := now.Add(2 * time.Millisecond)
	refreshContinuationSamples(monitor, config, observed)
	calls := 0
	m.now = func() time.Time {
		calls++
		if calls == 1 {
			return now.Add(time.Millisecond)
		}
		return observed
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.GetContinuation(r.ID)
	if c.State != "offered" {
		t.Fatalf("fresh concurrent sample rejected as future: %+v", c)
	}
}
