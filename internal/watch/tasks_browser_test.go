package watch

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in local browser fixture: synthetic records and fake SSH only. It cannot
// contact configured user machines and is excluded from the production binary.
func TestTaskDashboardBrowserFixture(t *testing.T) {
	if os.Getenv("SHW_BROWSER_FIXTURE") != "1" {
		t.Skip("only started by Playwright")
	}
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{code: "lost"})
	c.Interval = 3600
	monitor.states = map[string]State{}
	first := c.Machines[0]
	c.Machines = nil
	for i := 0; i < 16; i++ {
		machine := first
		machine.ID, machine.Name, machine.Host = fmt.Sprintf("machine-%02d", i), fmt.Sprintf("测试机器 %02d", i), fmt.Sprintf("192.0.2.%d", i+1)
		c.Machines = append(c.Machines, machine)
		monitor.states[machine.ID] = State{MachineID: machine.ID, Status: "online", UpdatedAt: time.Now()}
	}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for i := 0; i < 30; i++ {
		machine := c.Machines[i%3]
		start := now.Add(-time.Duration(72-i) * time.Hour)
		end := start.Add(90 * time.Minute)
		id := fmt.Sprintf("history-%02d", i)
		code := 0
		job := &TaskJob{TaskRequest: TaskRequest{ID: id, Shell: "printf browser-fixture"}, SelectedMachineID: machine.ID, MachineName: machine.Name, Host: machine.Host, Port: machine.Port, Username: c.Profiles[0].Username, Status: "succeeded", CreatedAt: start, UpdatedAt: end, FinishedAt: &end, ExitCode: &code, ArchiveReady: true}
		taskEvent(job, start, "dispatching", "本地测试：受理")
		taskEvent(job, start.Add(time.Minute), "running", "本地测试：启动")
		taskEvent(job, end, "succeeded", "退出码 0")
		taskEvent(job, end.Add(time.Minute), "archive_ready", "本地结果包已回收")
		m.jobs[id] = job
		if err := os.MkdirAll(m.path(id, ""), 0700); err != nil {
			t.Fatal(err)
		}
		if err := m.save(job); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(m.path(id, "result.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		archive := tar.NewWriter(gz)
		content := []byte("local browser fixture only\n")
		if err := archive.WriteHeader(&tar.Header{Name: "stdout.log", Mode: 0600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		machine := c.Machines[i]
		id := fmt.Sprintf("unknown-%d", i)
		start := now.Add(-time.Duration(8-i) * time.Hour)
		job := &TaskJob{TaskRequest: TaskRequest{ID: id, Shell: "printf unknown-fixture"}, SelectedMachineID: machine.ID, MachineName: machine.Name, Host: machine.Host, Port: machine.Port, Username: c.Profiles[0].Username, Status: "unknown", CreatedAt: start, UpdatedAt: now, Error: "远端任务未见退出标记"}
		taskEvent(job, start, "dispatching", "任务已受理")
		taskEvent(job, start.Add(time.Minute), "unknown", job.Error)
		m.jobs[id] = job
		if err := os.MkdirAll(m.path(id, ""), 0700); err != nil {
			t.Fatal(err)
		}
		if err := m.save(job); err != nil {
			t.Fatal(err)
		}
	}
	assets := os.DirFS(filepath.Join("..", "..", "web"))
	listener, err := net.Listen("tcp4", "127.0.0.1:18767")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	app := NewServer(store, monitor, nil, assets, listener.Addr().String())
	app.tasks = m
	defer app.Close()
	t.Log("synthetic dashboard fixture: http://127.0.0.1:18767/?tasks=1")
	server := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
