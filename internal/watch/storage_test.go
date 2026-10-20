package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageBudgetSurvivesRestartAndPreservesEvidence(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "reservations.json")
	if err := os.WriteFile(legacy, []byte(`{"old":"evidence"}`), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := newStorageBudget(dir)
	if err != nil {
		t.Fatal(err)
	}
	ceiling := b.limit
	for i := 0; ; i++ {
		path := filepath.Join(dir, fmt.Sprintf("audit-%d.json", i))
		err = b.Write(path, bytes.Repeat([]byte("x"), 16000), 0)
		if err != nil {
			break
		}
	}
	if !strings.Contains(err.Error(), "STORAGE_BUDGET_FULL") || b.used-b.baseline > metadataGrowthLimit {
		t.Fatalf("budget: %v %+v", err, b)
	}
	reopened, err := newStorageBudget(dir)
	if err != nil || reopened.limit != ceiling || reopened.used != b.used {
		t.Fatalf("restart reset budget: %v", err)
	}
	if raw, _ := os.ReadFile(legacy); string(raw) != `{"old":"evidence"}` {
		t.Fatal("old identity/evidence evicted")
	}
	if err := reopened.Write(legacy, []byte(`{}`), 0); err != nil {
		t.Fatal("shrinking existing records must still work", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "storage-budget.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorageBudget(dir); err == nil {
		t.Fatal("corrupt budget was silently reset")
	}
}

func TestStorageBudgetReservesTaskCompletion(t *testing.T) {
	dir := t.TempDir()
	b, err := newStorageBudget(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "job.json")
	if err := b.Write(path, []byte(`{}`), taskCompletionReserve); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if err := b.Write(filepath.Join(dir, fmt.Sprintf("other-%d.json", i)), make([]byte, 4096), 0); err != nil {
			break
		}
	}
	if err := b.Write(path, make([]byte, taskCompletionReserve), 0); err != nil {
		t.Fatal("reserved completion space unavailable", err)
	}
}

func TestTaskBudgetRejectsBeforeLaunchAndOriginalIDStillQueries(t *testing.T) {
	r := &fakeTaskRemote{code: "done:0"}
	m, _, c, _ := setupTasks(t, r)
	defer m.Close()
	req := TaskRequest{ID: "once", Shell: "printf ok", MachineID: c.Machines[0].ID}
	if _, _, err := m.Submit(req); err != nil {
		t.Fatal(err)
	}
	waitTask(t, m, req.ID)
	m.budget.limit = m.budget.used
	if _, status, err := m.Submit(req); err != nil || status != 200 {
		t.Fatalf("same ID blocked at full budget: %d %v", status, err)
	}
	req.ID = "budget-rejected"
	if _, status, err := m.Submit(req); status != 507 || err == nil {
		t.Fatalf("new work admitted: %d %v", status, err)
	}
	if _, err := os.Stat(m.path(req.ID, "")); !os.IsNotExist(err) {
		t.Fatal("rejected request created task directory")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.launch != 1 || r.collect != 0 {
		t.Fatalf("unexpected remote effects: launches=%d streams=%d", r.launch, r.collect)
	}
}

func TestActiveTaskCanRecordChangesAndFinishAtFullBudget(t *testing.T) {
	m, _, c, _ := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	job, _, err := m.Submit(TaskRequest{ID: "reserved-status", Shell: "printf ok", MachineID: c.Machines[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if err := m.budget.Write(filepath.Join(m.dir, fmt.Sprintf("fill-%d.json", i)), make([]byte, 4096), 0); err != nil {
			break
		}
	}
	for i := 0; i < 70; i++ {
		m.observe(job.ID, []string{"unknown", "running"}[i%2], fmt.Sprint(i)+strings.Repeat("<", 1000))
	}
	if !m.complete(job.ID, 0) {
		t.Fatal("completion failed")
	}
	raw, err := os.ReadFile(m.path(job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"status":"succeeded"`)) || bytes.Contains(raw, []byte("STORAGE_BUDGET_FULL")) {
		t.Fatal("final status was not persisted within reservation")
	}
}

type largeStreamingRemote struct{ fakeTaskRemote }

func (r *largeStreamingRemote) Stream(ctx context.Context, _ TaskJob, _ Profile, out io.Writer) error {
	chunk := bytes.Repeat([]byte("a"), 64*1024)
	for i := 0; i < 128; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := out.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func TestRemoteArchiveDownloadNeverCreatesLocalCache(t *testing.T) {
	r := &largeStreamingRemote{fakeTaskRemote: fakeTaskRemote{code: "done:0"}}
	m, monitor, c, store := setupTasks(t, r)
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	job, _, err := m.Submit(TaskRequest{ID: "remote-only", Shell: "printf ok", MachineID: c.Machines[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	job = waitTask(t, m, job.ID)
	if job.ArchiveMode != "remote" || job.ArchiveReady {
		t.Fatalf("cache advertised: %+v", job)
	}
	before, _ := os.ReadFile(m.path(job.ID, "job.json"))
	server := httptest.NewServer(app)
	defer server.Close()
	app.host = strings.TrimPrefix(server.URL, "http://")
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", server.URL+"/api/tasks/archive?id="+job.ID, nil)
		req.Header.Set("X-Watch-Token", app.token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || n != 8*1024*1024 {
			t.Fatalf("stream: %d bytes, %v, status %d", n, err, resp.StatusCode)
		}
	}
	after, _ := os.ReadFile(m.path(job.ID, "job.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("reading results rewrote metadata")
	}
	entries, _ := os.ReadDir(m.path(job.ID, ""))
	if len(entries) != 1 || entries[0].Name() != "job.json" {
		t.Fatal("download left local payload or scratch file")
	}
}

func TestRemovedRawHistoryAPI(t *testing.T) {
	s := NewServer(nil, nil, nil, nil, "127.0.0.1:9999")
	defer s.Close()
	req := httptest.NewRequest("POST", "http://127.0.0.1:9999/api/history/status", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("X-Watch-Token", s.token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 410 {
		t.Fatalf("raw recording is still available: %d %s", w.Code, w.Body)
	}
}

type failingStreamingRemote struct {
	fakeTaskRemote
	partial bool
}

func (r *failingStreamingRemote) Stream(_ context.Context, _ TaskJob, _ Profile, out io.Writer) error {
	if r.partial {
		_, _ = out.Write(bytes.Repeat([]byte("partial"), 1000))
	}
	return errors.New("remote disconnected")
}

func TestArchiveStreamFailureCannotLookLikeSuccessfulDownload(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			r := &failingStreamingRemote{fakeTaskRemote: fakeTaskRemote{code: "done:0"}, partial: partial}
			m, monitor, c, store := setupTasks(t, r)
			app := NewServer(store, monitor, nil, nil, "")
			app.tasks = m
			defer app.Close()
			job, _, err := m.Submit(TaskRequest{ID: "disconnect", Shell: "true", MachineID: c.Machines[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			waitTask(t, m, job.ID)
			server := httptest.NewUnstartedServer(app)
			app.host = server.Listener.Addr().String()
			server.Start()
			defer server.Close()
			req, _ := http.NewRequest("GET", server.URL+"/api/tasks/archive?id="+job.ID, nil)
			req.Header.Set("X-Watch-Token", app.token)
			resp, err := server.Client().Do(req)
			if err == nil {
				_, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if partial && readErr == nil {
					t.Fatal("partial result completed successfully")
				}
				if !partial && resp.StatusCode != 502 {
					t.Fatalf("pre-stream error status %d", resp.StatusCode)
				}
			} else if !partial {
				t.Fatal(err)
			}
			if _, err := os.Stat(m.path(job.ID, "result.tar.gz")); !os.IsNotExist(err) {
				t.Fatal("failed download left a cache")
			}
		})
	}
}
