package watch

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReservationAPIAndAuthentication(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	defer app.Close()
	app.tasks = m
	call := func(method, route, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:9999"+route, bytes.NewBufferString(body))
		r.Header.Set("X-Watch-Token", token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	body := `{"id":"api-reservation","machineId":"` + c.Machines[0].ID + `"}`
	if w := call("POST", "/api/tasks/reservations", "wrong", body); w.Code != 403 {
		t.Fatalf("auth %d", w.Code)
	}
	if w := call("POST", "/api/tasks/reservations", app.token, body); w.Code != 201 {
		t.Fatalf("reserve %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/api/tasks/reservations", app.token, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "simplehtmlwatch.reservations.v1") {
		t.Fatalf("list %d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/tasks/reservations/release", app.token, `{"id":"api-reservation"}`); w.Code != 200 {
		t.Fatalf("release %d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/tasks/reservations", app.token, body); w.Code != 410 {
		t.Fatalf("reuse %d", w.Code)
	}
}

func TestReservationAtomicRaceRestartAndFencing(t *testing.T) {
	remote := &fakeTaskRemote{}
	m, monitor, c, store := setupTasks(t, remote)
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := []Reservation{}
	for _, id := range []string{"owner-a", "owner-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			r, status, err := m.Reserve(id, c.Machines[0].ID)
			if err == nil {
				mu.Lock()
				winners = append(winners, r)
				mu.Unlock()
			} else if status != 409 {
				t.Errorf("reserve %d %v", status, err)
			}
		}(id)
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("winners: %v", winners)
	}
	winner := winners[0]
	m.Close()
	m, err := newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(m.Ready("", "")) != 0 {
		t.Fatal("restart lost occupancy")
	}
	if _, status, err := m.Submit(TaskRequest{ID: "intruder", MachineID: winner.MachineID, Shell: "echo conflict"}); status != 409 || err == nil {
		t.Fatal("unreserved task bypassed owner")
	}
	req := TaskRequest{ID: "owned-job", MachineID: winner.MachineID, Shell: "echo done", ReservationID: winner.ID}
	if _, status, err := m.Submit(req); status != 202 || err != nil {
		t.Fatalf("owned submit: %d %v", status, err)
	}
	if _, status, err := m.ReleaseReservation(winner.ID); status != 409 || err == nil {
		t.Fatal("released live remote task")
	}
	m.mu.Lock()
	m.jobs[req.ID].Status = "unknown"
	m.mu.Unlock()
	if _, status, err := m.ReleaseReservation(winner.ID); status != 409 || err == nil {
		t.Fatal("released unknown task")
	}
	m.complete(req.ID, 0)
	if _, status, err := m.ReleaseReservation(winner.ID); status != 200 || err != nil {
		t.Fatalf("release: %d %v", status, err)
	}
	if _, status, _ := m.Reserve(winner.ID, winner.MachineID); status != 410 {
		t.Fatal("released ID reused")
	}
	req.ID = "stale-worker"
	if _, status, _ := m.Submit(req); status != 409 {
		t.Fatal("stale owner executed")
	}
	if _, status, err := m.Reserve("new-owner", winner.MachineID); status != 201 || err != nil {
		t.Fatalf("new owner %d %v", status, err)
	}
	if _, _, err := m.ReleaseReservation(winner.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.Ready("", "")) != 0 {
		t.Fatal("stale release freed new owner")
	}
}

func TestReservationPhysicalAliasesAndIndependentMachines(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	c.Machines[0].ResourceID = "physical-a"
	alias := c.Machines[0]
	alias.ID = "alias-a"
	alias.Host = "192.0.2.2"
	alias.Port = 2222
	other := c.Machines[0]
	other.ID = "machine-b"
	other.Host = "192.0.2.3"
	other.ResourceID = "physical-b"
	c.Machines = append(c.Machines, alias, other)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor.mu.Lock()
	for _, host := range c.Machines {
		monitor.states[host.ID] = State{MachineID: host.ID, Status: "online", UpdatedAt: time.Now()}
	}
	monitor.mu.Unlock()
	if _, _, err := m.Reserve("one", c.Machines[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := m.Reserve("two", alias.ID); status != 409 {
		t.Fatal("physical alias escaped lock")
	}
	if _, status, err := m.Reserve("three", other.ID); status != 201 || err != nil {
		t.Fatalf("independent machine blocked: %d %v", status, err)
	}
}
