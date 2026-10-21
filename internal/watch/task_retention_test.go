package watch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedCompleted(t *testing.T, m *TaskManager, c Config, count int) []TaskRequest {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	requests := []TaskRequest{}
	start := time.Now().Add(-48 * time.Hour)
	for i := 0; i < count; i++ {
		r := TaskRequest{ID: fmt.Sprintf("old-%03d", i), MachineID: c.Machines[0].ID, Shell: "printf " + strings.Repeat("x", 1800)}
		finished, code := start.Add(time.Duration(i)*time.Minute), 0
		job := &TaskJob{TaskRequest: r, SelectedMachineID: r.MachineID, Host: c.Machines[0].Host, Port: c.Machines[0].Port, Username: c.Profiles[0].Username, Status: "succeeded", CreatedAt: start.Add(-time.Minute), UpdatedAt: finished, FinishedAt: &finished, ExitCode: &code, ArchiveMode: "remote", Events: []TaskEvent{{At: finished, Type: "succeeded", Text: "exit 0"}}}
		if err := os.Mkdir(m.path(r.ID, ""), 0700); err != nil {
			t.Fatal(err)
		}
		if err := m.save(job); err != nil {
			t.Fatal(err)
		}
		m.jobs[r.ID] = job
		requests = append(requests, r)
	}
	return requests
}

func TestCompletedRetentionPreservesDownloadsAndDeduplicationAfterRestart(t *testing.T) {
	remote := &fakeTaskRemote{code: "done:0"}
	m, monitor, config, store := setupTasks(t, remote)
	requests := seedCompleted(t, m, config, 130)
	cache := []byte("existing scientific result")
	if err := os.WriteFile(m.path(requests[0].ID, "result.tar.gz"), cache, 0600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.jobs[requests[0].ID].ArchiveReady = true
	if err := m.save(m.jobs[requests[0].ID]); err != nil {
		t.Fatal(err)
	}
	if err := m.compactCompletedLocked(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	old, _ := m.Get(requests[0].ID)
	if !old.HistoryCompacted || old.Shell != "" || len(old.Events) != 0 || old.RequestSHA256 != continuationTaskHash(requests[0]) {
		t.Fatal("old details were not compacted with their request identity")
	}
	if _, err := os.Stat(m.path(old.ID, "job.json")); !os.IsNotExist(err) {
		t.Fatal("old job file retained")
	}
	if raw, _ := os.ReadFile(m.path(old.ID, "result.tar.gz")); !bytes.Equal(raw, cache) {
		t.Fatal("existing scientific result changed")
	}
	var stream bytes.Buffer
	if err := m.StreamArchive(context.Background(), requests[1].ID, &stream); err != nil || stream.String() != "archive" {
		t.Fatalf("compacted remote result unavailable: %v", err)
	}
	m.Close()
	restarted, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.List()) != len(requests) {
		t.Fatal("status index lost task identities")
	}
	if job, status, err := restarted.Submit(requests[0]); err != nil || status != 200 || !job.HistoryCompacted {
		t.Fatalf("old ID did not query its completed record: %d %v", status, err)
	}
	changed := requests[0]
	changed.Shell = "printf different"
	if _, status, err := restarted.Submit(changed); err == nil || status != 409 {
		t.Fatal("old ID accepted a new command")
	}
	remote.mu.Lock()
	launched := remote.launch
	remote.mu.Unlock()
	if launched != 0 {
		t.Fatal("retention replayed an old task")
	}
	app := NewServer(store, monitor, nil, nil, "example.test")
	app.tasks = restarted
	defer app.Close()
	req := httptest.NewRequest("GET", "http://example.test/api/tasks/archive?id="+old.ID, nil)
	req.Header.Set("X-Watch-Token", app.token)
	req.Header.Set("Range", "bytes=0-7")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), cache[:8]) {
		t.Fatal("cached result download broke after compact/restart")
	}
}

func TestRetentionProtectsUnknownActiveReservationAndUnreportedExperiment(t *testing.T) {
	m, _, config, _ := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	r := seedCompleted(t, m, config, 130)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[r[0].ID].Status, m.jobs[r[0].ID].FinishedAt = "unknown", nil
	m.jobs[r[1].ID].Status, m.jobs[r[1].ID].FinishedAt = "running", nil
	m.jobs[r[2].ID].ReservationID = "still-held"
	m.reservations["still-held"] = Reservation{ID: "still-held"}
	m.continuations["unreported"] = Continuation{State: "executing", ContinuationRequest: ContinuationRequest{ExistingTaskIDs: []string{r[3].ID}, Tasks: []ContinuationTask{{ID: r[4].ID}}}}
	if err := m.compactCompletedLocked(); err != nil {
		t.Fatal(err)
	}
	for _, request := range r[:5] {
		if m.jobs[request.ID].HistoryCompacted {
			t.Fatalf("live evidence compacted: %s", request.ID)
		}
	}
	if !m.jobs[r[5].ID].HistoryCompacted {
		t.Fatal("unprotected old completion did not compact")
	}
}

func TestRetentionFailureDoesNotBlockNewTaskAtFullTarget(t *testing.T) {
	remote := &fakeTaskRemote{code: "done:0"}
	m, _, c, _ := setupTasks(t, remote)
	defer m.Close()
	seedCompleted(t, m, c, 110)
	// Force only the optional history index write to fail. Task identity writes
	// remain possible and must still precede remote dispatch.
	if err := os.Mkdir(filepath.Join(m.dir, "completed.json.gz"), 0700); err != nil {
		t.Fatal(err)
	}
	m.budget.limit = m.budget.used
	request := TaskRequest{ID: "new-at-full", Shell: "printf ok", MachineID: c.Machines[0].ID}
	if _, status, err := m.Submit(request); err != nil || status != 202 {
		t.Fatalf("optional retention blocked new task: %d %v", status, err)
	}
	waitTask(t, m, request.ID)
	if m.StorageStatus()["retentionError"] == "" {
		t.Fatal("retention failure was hidden")
	}
}

func TestRetentionRecoversIndexWrittenBeforeJobUnlink(t *testing.T) {
	remote := &fakeTaskRemote{}
	m, monitor, c, store := setupTasks(t, remote)
	requests := seedCompleted(t, m, c, 110)
	full, _ := os.ReadFile(m.path(requests[0].ID, "job.json"))
	m.mu.Lock()
	if err := m.compactCompletedLocked(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	// Recreate the exact duplicate that a crash after index fsync but before
	// job unlink can leave. A duplicate with a different request is rejected.
	_ = os.MkdirAll(m.path(requests[0].ID, ""), 0700)
	if err := os.WriteFile(m.path(requests[0].ID, "job.json"), full, 0600); err != nil {
		t.Fatal(err)
	}
	m.Close()
	reopened, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	var job TaskJob
	_ = json.Unmarshal(full, &job)
	job.Shell = "changed while crashed"
	raw, _ := json.Marshal(job)
	_ = os.MkdirAll(m.path(job.ID, ""), 0700)
	_ = os.WriteFile(m.path(job.ID, "job.json"), raw, 0600)
	if bad, err := newTaskManager(store, monitor, remote); err == nil {
		bad.Close()
		t.Fatal("conflicting old request identity was accepted")
	}
}

func TestSoftStorageReadsPreviouslyAuthorizedVersionTwoBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "storage-budget.json"), []byte(`{"version":2,"baselineBytes":4096,"limitBytes":8004096,"growthLimitBytes":8000000}`), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := newStorageBudget(dir)
	if err != nil || b.limit != 8004096 || b.baseline != 4096 {
		t.Fatalf("authorized v2 target not preserved: %v", err)
	}
	if b.Status()["admissionBlocked"] != false || b.Status()["policy"] != "prune-completed" {
		t.Fatal("budget is still advertised as admission control")
	}
}

func TestCompactedGroupTaskKeepsGrantIdentityAcrossRestart(t *testing.T) {
	m, monitor, c, store := setupGroup(t)
	ids := []string{}
	for _, machine := range c.Machines {
		ids = append(ids, machine.ID)
	}
	shell := "printf " + strings.Repeat("x", 1800)
	digest := sha256.Sum256([]byte(shell))
	r, _, err := m.ReserveGroup(GroupReservationRequest{ID: "old-group", MachineID: ids[0], MachineIDs: ids, TaskID: "old-000", PlanSHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	requests := seedCompleted(t, m, c, 110)
	request := requests[0]
	request.ReservationID, request.ReservationGrantID, request.ReservationGeneration = r.ID, r.GrantID, r.Generation
	m.mu.Lock()
	m.jobs[request.ID].TaskRequest = request
	if err := m.reservationIntentLocked(request); err != nil {
		t.Fatal(err)
	}
	if err := m.save(m.jobs[request.ID]); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	if _, _, err := m.ReleaseReservation(r.ID); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	if err := m.compactCompletedLocked(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	m.Close()
	reopened, err := newTaskManager(store, monitor, &fakeTaskRemote{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if job, status, err := reopened.Submit(request); status != 200 || err != nil || !job.HistoryCompacted {
		t.Fatalf("completed group lost its immutable request/grant identity: %d %v", status, err)
	}
}

func TestCompactedContinuationValidatesOriginalPlanWithoutRetainingShell(t *testing.T) {
	m, _, _, _ := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	r := TaskRequest{ID: "old-experiment", Shell: "printf evidence", ContinuationID: "closed-experiment", GrantID: "old-grant", Generation: 1}
	m.continuations[r.ContinuationID] = Continuation{State: "completed", Intents: map[string]string{r.ID: continuationTaskHash(r)}, ContinuationRequest: ContinuationRequest{ID: r.ContinuationID, Tasks: []ContinuationTask{{ID: r.ID, Shell: r.Shell}}}}
	job := compactTask(TaskJob{TaskRequest: r})
	if err := m.validateContinuationTask(job); err != nil {
		t.Fatal(err)
	}
	c := m.continuations[r.ContinuationID]
	c.Tasks[0].Shell = "changed plan"
	m.continuations[r.ContinuationID] = c
	if err := m.validateContinuationTask(job); err == nil {
		t.Fatal("compacted status bypassed the immutable experiment plan")
	}
}
