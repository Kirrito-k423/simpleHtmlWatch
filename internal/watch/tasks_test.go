package watch

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeTaskRemote struct {
	mu      sync.Mutex
	launch  int
	probe   int
	collect int
	code    string
}

func (f *fakeTaskRemote) Launch(_ context.Context, _ TaskJob, _ Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launch++
	return nil
}
func (f *fakeTaskRemote) Probe(_ context.Context, _ TaskJob, _ Profile) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probe++
	if f.code != "" {
		return f.code, nil
	}
	return "running", nil
}
func (f *fakeTaskRemote) Logs(_ context.Context, _ TaskJob, _ Profile) (TaskLogs, error) {
	return TaskLogs{Stdout: "progress", Stderr: ""}, nil
}
func (f *fakeTaskRemote) Collect(_ context.Context, _ TaskJob, _ Profile, path string) error {
	f.mu.Lock()
	f.collect++
	f.mu.Unlock()
	return os.WriteFile(path, []byte("archive"), 0600)
}

func setupTasks(t *testing.T, remote taskRemote) (*TaskManager, *Monitor, Config, *Store) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	c.Machines[0].Group = "gpu"
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor := NewMonitor(nil)
	monitor.states[c.Machines[0].ID] = State{MachineID: c.Machines[0].ID, Status: "online", UpdatedAt: time.Now()}
	m, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	m.poll = 10 * time.Millisecond
	return m, monitor, c, store
}

func waitTask(t *testing.T, m *TaskManager, id string) TaskJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := m.Get(id)
		if ok && job.FinishedAt != nil && job.ArchiveReady {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("task did not finish and collect")
	return TaskJob{}
}

func TestTaskReadyDispatchIdempotencyAndRecovery(t *testing.T) {
	remote := &fakeTaskRemote{}
	m, monitor, c, store := setupTasks(t, remote)
	if len(m.Ready("", "gpu")) != 1 || len(m.Ready("", "other")) != 0 {
		t.Fatal("ready selection mismatch")
	}
	request := TaskRequest{ID: "job-one", Shell: "printf done", Group: "gpu"}
	job, status, err := m.Submit(request)
	if err != nil || status != 202 || job.SelectedMachineID != c.Machines[0].ID {
		t.Fatalf("submit: %d %v %+v", status, err, job)
	}
	if _, status, err := m.Submit(TaskRequest{ID: "job-two", Shell: "printf other", MachineID: c.Machines[0].ID}); status != 409 || err == nil {
		t.Fatal("busy machine accepted second job")
	}
	if _, status, err := m.Submit(request); status != 200 || err != nil {
		t.Fatal("idempotent replay rejected")
	}
	changed := request
	changed.Shell = "printf changed"
	if _, status, err := m.Submit(changed); status != 409 || err == nil {
		t.Fatal("changed command reused task ID")
	}
	remote.mu.Lock()
	remote.code = "done:0"
	remote.mu.Unlock()
	job = waitTask(t, m, request.ID)
	if job.Status != "succeeded" || job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("finished job: %+v", job)
	}
	if logs, err := m.Logs(request.ID); err != nil || logs.Stdout != "progress" {
		t.Fatalf("logs: %+v %v", logs, err)
	}
	if _, err := os.Stat(m.path(request.ID, "result.tar.gz")); err != nil {
		t.Fatal(err)
	}
	m.Close()
	restarted, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if got, ok := restarted.Get(request.ID); !ok || !got.ArchiveReady {
		t.Fatal("durable task missing after restart")
	}
	remote.mu.Lock()
	launches := remote.launch
	remote.mu.Unlock()
	if launches != 1 {
		t.Fatalf("task launched %d times", launches)
	}
}

func TestTaskAutoSelectionAndDurableTimeline(t *testing.T) {
	remote := &fakeTaskRemote{code: "done:0"}
	m, monitor, c, store := setupTasks(t, remote)
	other := c.Machines[0]
	other.ID, other.Name, other.Host, other.Group = "a0", "另一组机器", "192.0.2.23", "other"
	c.Machines = append(c.Machines, other)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor.states[other.ID] = State{MachineID: other.ID, Status: "online", UpdatedAt: time.Now()}
	request := TaskRequest{ID: "auto-job", Shell: "printf result"}
	job, status, err := m.Submit(request)
	if err != nil || status != 202 || job.SelectedMachineID != other.ID {
		t.Fatalf("auto submit: %d %v %+v", status, err, job)
	}
	job = waitTask(t, m, request.ID)
	want := []string{"dispatching", "running", "succeeded", "archive_ready"}
	if len(job.Events) != len(want) {
		t.Fatalf("timeline events: %+v", job.Events)
	}
	for i, kind := range want {
		if job.Events[i].Type != kind || job.Events[i].At.IsZero() || i > 0 && job.Events[i].At.Before(job.Events[i-1].At) {
			t.Fatalf("event %d: %+v", i, job.Events)
		}
	}
	m.Close()
	restarted, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	got, ok := restarted.Get(request.ID)
	if !ok || len(got.Events) != len(want) || got.Events[3].Type != "archive_ready" {
		t.Fatalf("timeline lost on restart: %+v", got.Events)
	}
}

func TestTaskTimelineKeepsFirstAndRecentEvents(t *testing.T) {
	job := &TaskJob{}
	base := time.Now()
	for i := 0; i < 520; i++ {
		taskEvent(job, base.Add(time.Duration(i)*time.Second), "running", fmt.Sprint(i))
	}
	if len(job.Events) != 512 || job.EventsDropped != 8 || job.Events[0].Text != "0" || job.Events[1].Text != "9" || job.Events[511].Text != "519" {
		t.Fatalf("bounded timeline: first=%+v second=%+v last=%+v dropped=%d", job.Events[0], job.Events[1], job.Events[511], job.EventsDropped)
	}
}

func TestTaskNoStaleReadyOrUnsafeSelectors(t *testing.T) {
	m, monitor, c, _ := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	monitor.states[c.Machines[0].ID] = State{MachineID: c.Machines[0].ID, Status: "online", UpdatedAt: time.Now().Add(-time.Hour)}
	if len(m.Ready("", "gpu")) != 0 {
		t.Fatal("stale machine shown ready")
	}
	for _, request := range []TaskRequest{
		{ID: "bad/id", Shell: "echo hi", Group: "gpu"},
		{ID: "both", Shell: "echo hi", Group: "gpu", MachineID: c.Machines[0].ID},
		{ID: "watch", Shell: "watch echo hi", Group: "gpu"},
		{ID: "null", Shell: "echo\x00hi", Group: "gpu"},
		{ID: "long", Shell: strings.Repeat("x", 4097), Group: "gpu"},
	} {
		if _, _, err := m.Submit(request); err == nil {
			t.Fatalf("accepted invalid request: %+v", request)
		}
	}
}

func TestTaskAliasesShareOneSSHAddressSlot(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	alias := c.Machines[0]
	alias.ID = "m2"
	c.Machines = append(c.Machines, alias)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor.states[alias.ID] = State{MachineID: alias.ID, Status: "online", UpdatedAt: time.Now()}
	if len(m.Ready("", "gpu")) != 2 {
		t.Fatal("aliases not listed before reservation")
	}
	if _, _, err := m.Submit(TaskRequest{ID: "alias-job", Shell: "echo task", MachineID: c.Machines[0].ID}); err != nil {
		t.Fatal(err)
	}
	if len(m.Ready("", "gpu")) != 0 {
		t.Fatal("same SSH address remained ready through alias")
	}
}

func TestTaskUnknownRequiresExplicitResolution(t *testing.T) {
	remote := &fakeTaskRemote{code: "missing"}
	m, _, c, _ := setupTasks(t, remote)
	defer m.Close()
	request := TaskRequest{ID: "unknown-task", Shell: "echo once", MachineID: c.Machines[0].ID}
	if _, _, err := m.Submit(request); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, _ := m.Get(request.ID)
		if job.Status == "unknown" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job, _ := m.Get(request.ID); job.Status != "unknown" {
		t.Fatal("missing remote task not marked unknown")
	}
	if len(m.Ready(c.Machines[0].ID, "")) != 0 {
		t.Fatal("unknown task did not reserve machine")
	}
	job, err := m.Abandon(request.ID)
	if err != nil || job.Status != "abandoned" || job.ExitCode != nil {
		t.Fatalf("abandon: %+v %v", job, err)
	}
	if len(job.Events) < 4 || job.Events[len(job.Events)-2].Type != "unknown" || job.Events[len(job.Events)-1].Type != "abandoned" {
		t.Fatalf("unknown resolution timeline: %+v", job.Events)
	}
	if len(m.Ready(c.Machines[0].ID, "")) != 1 {
		t.Fatal("manual resolution did not release machine")
	}
	time.Sleep(30 * time.Millisecond)
	if job, _ := m.Get(request.ID); job.Status != "abandoned" {
		t.Fatal("polling overwrote manual resolution")
	}
	if _, err := m.Collect(request.ID); err == nil {
		t.Fatal("unknown task collected after resolution")
	}
	if _, err := m.Abandon(request.ID); err == nil {
		t.Fatal("manual resolution accepted twice")
	}
}

func TestTaskInterruptedLaunchNeverReplayed(t *testing.T) {
	remote := &fakeTaskRemote{}
	m, monitor, c, store := setupTasks(t, remote)
	request := TaskRequest{ID: "interrupted", Shell: "echo once", MachineID: c.Machines[0].ID}
	job := &TaskJob{TaskRequest: request, SelectedMachineID: request.MachineID, MachineName: c.Machines[0].Name, Host: c.Machines[0].Host, Port: c.Machines[0].Port, Username: c.Profiles[0].Username, Status: "dispatching", CreatedAt: time.Now()}
	if err := os.Mkdir(m.path(job.ID, ""), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.save(job); err != nil {
		t.Fatal(err)
	}
	m.Close()
	recovered, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if got, _ := recovered.Get(job.ID); got.Status != "unknown" {
		t.Fatalf("interrupted task was not marked unknown: %+v", got)
	}
	remote.mu.Lock()
	launches := remote.launch
	remote.mu.Unlock()
	if launches != 0 {
		t.Fatal("recovered task relaunched")
	}
	if _, err := recovered.Collect(job.ID); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("unfinished task was collected")
	}
}

func TestSSHRemoteTaskLaunchProbeLogsAndCollect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("本测试使用本机 Bash 模拟 Linux SSH 目标")
	}
	home := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() != "test" || string(password) != "secret" {
			return nil, errors.New("bad credentials")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					stream, reqs, err := channel.Accept()
					if err != nil {
						continue
					}
					for req := range reqs {
						if req.Type != "exec" {
							_ = req.Reply(false, nil)
							continue
						}
						var payload struct{ Command string }
						_ = ssh.Unmarshal(req.Payload, &payload)
						_ = req.Reply(true, nil)
						cmd := exec.Command("bash", "-c", payload.Command)
						cmd.Env = append(os.Environ(), "HOME="+home)
						cmd.Stdout = stream
						cmd.Stderr = stream.Stderr()
						code := uint32(0)
						if err := cmd.Run(); err != nil {
							code = 1
							if exit, ok := err.(*exec.ExitError); ok {
								code = uint32(exit.ExitCode())
							}
						}
						_, _ = stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
						break
					}
					_ = stream.Close()
				}
			}()
		}
	}()
	defer wg.Wait()
	defer listener.Close()
	trust, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = trust.Callback(addr, false)("", listener.Addr(), signer.PublicKey())
	if err := trust.Accept(addr, ssh.FingerprintSHA256(signer.PublicKey())); err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	job := TaskJob{TaskRequest: TaskRequest{ID: "ssh-task-test", Shell: "printf 'hello\\n'; printf artifact >\"$SHW_RESULTS_DIR/file.txt\""}, Host: "127.0.0.1", Port: port, Username: "test"}
	profile := Profile{Username: "test", Password: "secret"}
	remote := &sshTaskRemote{trust: trust}
	if err := remote.Launch(context.Background(), job, profile); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		state, err = remote.Probe(context.Background(), job, profile)
		if err != nil {
			t.Fatal(err)
		}
		if state == "done:0" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state != "done:0" {
		t.Fatalf("remote task state %q", state)
	}
	logs, err := remote.Logs(context.Background(), job, profile)
	if err != nil || !strings.Contains(logs.Stdout, "hello") {
		t.Fatalf("logs %+v %v", logs, err)
	}
	archive := fmt.Sprintf("%s/result.tar.gz", t.TempDir())
	if err := remote.Collect(context.Background(), job, profile, archive); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := io.ReadFull(f, magic); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		t.Fatalf("archive magic %x %v", magic, err)
	}
	if err := remote.Launch(context.Background(), job, profile); err == nil {
		t.Fatal("remote ID reused")
	}
	large := job
	large.ID = "ssh-task-large"
	large.Shell = "head -c 6000000 /dev/urandom >\"$SHW_RESULTS_DIR/big.bin\""
	if err := remote.Launch(context.Background(), large, profile); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		state, err = remote.Probe(context.Background(), large, profile)
		if err == nil && state == "done:0" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state != "done:0" {
		t.Fatalf("large task state %q %v", state, err)
	}
	largeArchive := fmt.Sprintf("%s/large.tar.gz", t.TempDir())
	if err := remote.Collect(context.Background(), large, profile, largeArchive); err == nil {
		t.Fatal("oversized archive accepted")
	}
	if _, err := os.Stat(largeArchive); !os.IsNotExist(err) {
		t.Fatal("partial oversized archive retained")
	}
}

func TestTaskAPIRequiresTokenAndReturnsArchive(t *testing.T) {
	remote := &fakeTaskRemote{code: "done:0"}
	m, monitor, c, store := setupTasks(t, remote)
	defer m.Close()
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	defer app.Close()
	app.tasks = m
	request := TaskRequest{ID: "api-task", Shell: "printf ok", MachineID: c.Machines[0].ID}
	body, _ := json.Marshal(request)
	call := func(method, path, token string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:9999"+path, bytes.NewReader(body))
		r.Header.Set("X-Watch-Token", token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/api/tasks", "bad", body); w.Code != 403 {
		t.Fatalf("invalid token: %d", w.Code)
	}
	if w := call("GET", "/api/tasks/ready?machineId=m1", app.token, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "m1") {
		t.Fatalf("ready: %d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/tasks", app.token, body); w.Code != 202 {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	waitTask(t, m, request.ID)
	if w := call("GET", "/api/tasks/logs?id=api-task", app.token, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "progress") {
		t.Fatalf("logs: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/api/tasks/archive?id=api-task", app.token, nil); w.Code != 200 || w.Body.String() != "archive" {
		t.Fatalf("archive: %d %s", w.Code, w.Body)
	}
	autoBody, _ := json.Marshal(TaskRequest{ID: "api-auto", Shell: "printf auto"})
	if w := call("POST", "/api/tasks", app.token, autoBody); w.Code != 202 || !strings.Contains(w.Body.String(), `"selectedMachineId":"m1"`) {
		t.Fatalf("auto submit: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/api/tasks?id=api-auto", app.token, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"events"`) {
		t.Fatalf("timeline response: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/api/tasks/archive?id=../config.enc", app.token, nil); w.Code != 400 {
		t.Fatalf("path traversal: %d", w.Code)
	}
}

func TestTaskResolveAPIRequiresExplicitConfirmation(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{code: "missing"})
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	request := TaskRequest{ID: "resolve-api", Shell: "echo once", MachineID: c.Machines[0].ID}
	if _, _, err := m.Submit(request); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, _ := m.Get(request.ID)
		if job.Status == "unknown" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	call := func(body string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9999/api/tasks/resolve", strings.NewReader(body))
		r.Header.Set("X-Watch-Token", app.token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w.Code
	}
	if code := call(`{"id":"resolve-api"}`); code != 400 {
		t.Fatalf("missing confirmation: %d", code)
	}
	if code := call(`{"id":"resolve-api","confirm":"remote-stopped"}`); code != 200 {
		t.Fatalf("confirmed resolution: %d", code)
	}
}
