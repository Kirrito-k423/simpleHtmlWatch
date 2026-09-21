package watch

import (
	"bytes"
	"encoding/json"
	"golang.org/x/crypto/ssh"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func executeRequest(c Config, id, shell string) ExecutionRequest {
	m := c.Machines[0]
	return ExecutionRequest{ID: id, Shell: shell, Targets: []ExecutionTarget{{ID: m.ID, Host: m.Host, Port: m.Port, Username: c.Profiles[0].Username}}}
}
func waitExecution(t *testing.T, e *Executor, id string) ExecutionJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := e.Get(id)
		if ok && job.FinishedAt != nil {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("batch did not finish")
	return ExecutionJob{}
}
func TestExecutionOnceIdempotencyAndIsolation(t *testing.T) {
	fixture := newFixture(t)
	app, store, c := customApp(t, fixture)
	defer app.Close()
	request := executeRequest(c, "test-once", "printf tilexr")
	job, status, err := app.executor.Start(request, store.Snapshot())
	if err != nil || status != 202 {
		t.Fatalf("start %d %v", status, err)
	}
	duplicate, _, err := app.executor.Start(request, store.Snapshot())
	if err != nil || duplicate.ID != job.ID {
		t.Fatal("idempotency failed")
	}
	finished := waitExecution(t, app.executor, job.ID)
	if finished.Results[0].Status != "succeeded" || !strings.Contains(finished.Results[0].Result.Stdout, "printf tilexr") {
		t.Fatalf("result %+v", finished)
	}
	if fixture.customRuns.Load() != 1 {
		t.Fatal("executed more than once")
	}
	if len(store.Snapshot().CustomCommands) != 0 || len(app.monitor.Snapshot()) != 0 {
		t.Fatal("batch mutated scheduled watch")
	}
	request.Shell = "printf different"
	if _, status, err = app.executor.Start(request, c); err == nil || status != 409 {
		t.Fatal("same key changed command accepted")
	}
	request.ID = "test-disabled"
	request.Targets[0].ID = "disabled"
	if _, _, err = app.executor.Start(request, c); err == nil {
		t.Fatal("disabled target accepted")
	}
	request = executeRequest(c, "test-changed", "printf tilexr")
	request.Targets[0].Host = "192.0.2.99"
	if _, _, err = app.executor.Start(request, c); err == nil {
		t.Fatal("changed target accepted")
	}
	for _, shell := range []string{"", "watch pkill -f tilexr", strings.Repeat("x", 4097)} {
		if _, _, err = app.executor.Start(executeRequest(c, "invalid", shell), c); err == nil {
			t.Fatal("invalid shell accepted")
		}
	}
}
func TestExecutionTransportUnknownNeverRetries(t *testing.T) {
	f := newFixture(t)
	f.dropCustom.Store(true)
	app, _, c := customApp(t, f)
	defer app.Close()
	request := executeRequest(c, "test-unknown", "printf tilexr")
	job, _, err := app.executor.Start(request, c)
	if err != nil {
		t.Fatal(err)
	}
	job = waitExecution(t, app.executor, job.ID)
	if job.Results[0].Status != "unknown" || job.Results[0].Result.Stdout == "" || f.customRuns.Load() != 1 {
		t.Fatalf("transport replay/result %+v runs %d", job, f.customRuns.Load())
	}
}
func TestExecutionRejectsUntrustedAndConcurrentBatch(t *testing.T) {
	f := newFixture(t)
	app, _, c := customApp(t, f)
	defer app.Close()
	untrusted, _ := NewTrustStore(t.TempDir())
	app.executor.trust = untrusted
	job, _, err := app.executor.Start(executeRequest(c, "test-untrusted", "printf tilexr"), c)
	if err != nil {
		t.Fatal(err)
	}
	job = waitExecution(t, app.executor, job.ID)
	if job.Results[0].Status != "failed" || f.customRuns.Load() != 0 {
		t.Fatal("executed without host trust")
	}
	app.executor.trust = app.trust
	f.hanging.Store(true)
	app.executor.timeout = 100 * time.Millisecond
	job, _, err = app.executor.Start(executeRequest(c, "test-timeout", "printf tilexr"), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, status, e := app.executor.Start(executeRequest(c, "test-other", "printf tilexr"), c); e == nil || status != 409 {
		t.Fatal("overlapping batch accepted")
	}
	job = waitExecution(t, app.executor, job.ID)
	if job.Results[0].Status != "unknown" {
		t.Fatal("timeout claimed known result")
	}
}
func TestExecutionAPIScopeAndResultRetention(t *testing.T) {
	f := newFixture(t)
	app, _, c := customApp(t, f)
	defer app.Close()
	request := executeRequest(c, "http-once", "printf tilexr")
	body, _ := json.Marshal(request)
	for _, token := range []string{"bad", app.token} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9999/api/executions", bytes.NewReader(body))
		r.Header.Set("X-Watch-Token", token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		want := 202
		if token == "bad" {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "only-a-test-password") {
			t.Fatal("credentials leaked")
		}
	}
	waitExecution(t, app.executor, request.ID)
	// Removing old results must not turn an old request ID into a fresh execution.
	app.executor.mu.Lock()
	delete(app.executor.jobs, request.ID)
	app.executor.order = nil
	app.executor.mu.Unlock()
	if _, status, err := app.executor.Start(request, c); err == nil || status != 410 {
		t.Fatal("expired job was replayed")
	}
}
func TestExecutionDuplicateSelectionIsAtomic(t *testing.T) {
	f := newFixture(t)
	app, _, c := customApp(t, f)
	defer app.Close()
	request := executeRequest(c, "duplicate-target", "printf tilexr")
	request.Targets = append(request.Targets, request.Targets[0])
	if _, _, err := app.executor.Start(request, c); err == nil {
		t.Fatal("duplicate targets accepted")
	}
	if f.customRuns.Load() != 0 {
		t.Fatal("partial execution before validation")
	}
}

func TestExecutionMultipleSelectedTargets(t *testing.T) {
	f1 := newFixture(t)
	f2 := newFixture(t)
	app, _, c := customApp(t, f1)
	defer app.Close()
	addr := f2.listener.Addr().String()
	_ = app.trust.Callback(addr, false)("", f2.listener.Addr(), f2.signer.PublicKey())
	if err := app.trust.Accept(addr, ssh.FingerprintSHA256(f2.signer.PublicKey())); err != nil {
		t.Fatal(err)
	}
	other := c.Machines[0]
	other.ID = "m2"
	other.Port = f2.listener.Addr().(*net.TCPAddr).Port
	c.Machines = append(c.Machines, other)
	request := executeRequest(c, "two-targets", "printf tilexr")
	request.Targets = append(request.Targets, ExecutionTarget{ID: other.ID, Host: other.Host, Port: other.Port, Username: c.Profiles[0].Username})
	job, _, err := app.executor.Start(request, c)
	if err != nil {
		t.Fatal(err)
	}
	job = waitExecution(t, app.executor, job.ID)
	if len(job.Results) != 2 || f1.customRuns.Load() != 1 || f2.customRuns.Load() != 1 {
		t.Fatal("selected target count mismatch")
	}
	for _, result := range job.Results {
		if result.Status != "succeeded" {
			t.Fatal(result)
		}
	}
}
