package watch

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTaskMachineInventoryIncludesOccupiedOfflineAndAliases(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	first := c.Machines[0]
	c.Machines = nil
	for i := 0; i < 16; i++ {
		machine := first
		machine.ID, machine.Host = fmt.Sprintf("m%d", i), fmt.Sprintf("192.0.2.%d", i+1)
		c.Machines = append(c.Machines, machine)
		monitor.states[machine.ID] = State{Status: "online", UpdatedAt: time.Now()}
	}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		machine := c.Machines[i]
		id := fmt.Sprintf("unknown-%d", i)
		m.jobs[id] = &TaskJob{TaskRequest: TaskRequest{ID: id}, SelectedMachineID: machine.ID, Host: machine.Host, Port: machine.Port, Status: "unknown"}
	}
	machines := m.Machines()
	ready := 0
	for _, machine := range machines {
		if machine.Ready {
			ready++
		}
	}
	if len(machines) != 16 || ready != 13 {
		t.Fatalf("inventory=%d ready=%d", len(machines), ready)
	}
	alias := c.Machines[0]
	alias.ID = "alias"
	alias.Port++
	c.Machines = append(c.Machines, alias)
	c.Machines[3].Enabled = false
	monitor.states[c.Machines[4].ID] = State{Status: "offline", UpdatedAt: time.Now()}
	monitor.states[c.Machines[5].ID] = State{Status: "online", UpdatedAt: time.Now().Add(-time.Hour)}
	c.Machines[6].ResourceID, c.Machines[7].ResourceID = "shared", "shared"
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	m.reservations["owner"] = Reservation{ID: "owner", MachineID: c.Machines[6].ID, Host: c.Machines[6].Host, ResourceKey: "physical:shared"}
	got := map[string]TaskMachine{}
	for _, machine := range m.Machines() {
		got[machine.ID] = machine
	}
	for id, reason := range map[string]string{"alias": "任务占用", "m3": "已停用", "m4": "监控未在线", "m5": "监控状态已过期", "m6": "已被预约", "m7": "已被预约"} {
		if got[id].Ready || got[id].Reason != reason {
			t.Fatalf("%s: %+v", id, got[id])
		}
	}
	if len(got["alias"].TaskIDs) != 1 || got["alias"].TaskIDs[0] != "unknown-0" {
		t.Fatal("alias must explain its blocking task")
	}
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	r := httptest.NewRequest("GET", "http://"+app.host+"/api/tasks/machines", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("inventory requires token")
	}
	r.Header.Set("X-Watch-Token", app.token)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("inventory: %s", w.Body)
	}
	var rows []TaskMachine
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 17 {
		t.Fatalf("rows: %v %d", err, len(rows))
	}
}

func TestTaskManualProbeNeverRelaunchesOrReleasesUncertainTask(t *testing.T) {
	remote := &fakeTaskRemote{code: "lost"}
	m, monitor, c, store := setupTasks(t, remote)
	machine := c.Machines[0]
	id := "killed-task"
	m.jobs[id] = &TaskJob{TaskRequest: TaskRequest{ID: id}, SelectedMachineID: machine.ID, Host: machine.Host, Port: machine.Port, Username: c.Profiles[0].Username, Status: "unknown", CreatedAt: time.Now()}
	if err := os.MkdirAll(m.path(id, ""), 0700); err != nil {
		t.Fatal(err)
	}
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	call := func(method, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+app.host+"/api/tasks/probe?id="+id, nil)
		r.Header.Set("X-Watch-Token", token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	if call("POST", "bad").Code != 403 || call("GET", app.token).Code != 400 {
		t.Fatal("probe boundary failed")
	}
	for _, state := range []string{"lost", "missing", "incomplete", "done:bad", "done:256"} {
		remote.code = state
		if w := call("POST", app.token); w.Code != 200 {
			t.Fatalf("probe: %s", w.Body)
		}
		job, _ := m.Get(id)
		if job.Status != "unknown" || job.FinishedAt != nil || job.ExitCode != nil || len(m.Ready("", "")) != 0 {
			t.Fatalf("unsafe %s: %+v", state, job)
		}
	}
	remote.code = "running"
	call("POST", app.token)
	job, _ := m.Get(id)
	if job.Status != "running" {
		t.Fatal("running not recovered")
	}
	remote.code = "done:137"
	call("POST", app.token)
	job, _ = m.Get(id)
	if job.Status != "failed" || job.ExitCode == nil || *job.ExitCode != 137 || len(m.Ready("", "")) != 1 {
		t.Fatalf("confirmed exit: %+v", job)
	}
	remote.code = "running"
	call("POST", app.token)
	job, _ = m.Get(id)
	if job.Status != "failed" || remote.launch != 0 {
		t.Fatal("relaunch or terminal regression")
	}
}
