package watch

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResourceIdentityConflictBoundaries(t *testing.T) {
	base := ReservationAllocation{"one", "physical:node-one", "127.0.0.1", 32201}
	for _, tc := range []struct {
		name string
		a, b ReservationAllocation
		busy bool
	}{
		{"distinct_forwards", base, ReservationAllocation{"two", "physical:node-two", "127.0.0.1", 32202}, false},
		{"same_physical_node", base, ReservationAllocation{"alias", "physical:node-one", "192.0.2.42", 22}, true},
		{"same_machine_id", base, ReservationAllocation{"one", "physical:changed", "192.0.2.42", 22}, true},
		{"same_endpoint", base, ReservationAllocation{"alias", "physical:contradiction", "127.0.0.1", 32201}, true},
		{"localhost_same_endpoint", base, ReservationAllocation{"alias", "physical:contradiction", "LOCALHOST", 32201}, true},
		{"ipv6_forward", base, ReservationAllocation{"two", "physical:node-two", "::1", 32202}, false},
		{"missing_physical_id", base, ReservationAllocation{"two", "host:127.0.0.1", "127.0.0.1", 32202}, true},
		{"legacy_unknown_task", base, ReservationAllocation{"old", "", "127.0.0.1", 32202}, true},
		{"invalid_physical_id", base, ReservationAllocation{"two", "physical:", "127.0.0.1", 32202}, true},
		{"invalid_port", base, ReservationAllocation{"two", "physical:node-two", "127.0.0.1", 0}, true},
		{"direct_host_still_shared", ReservationAllocation{"one", "physical:one", "server.example", 22}, ReservationAllocation{"two", "physical:two", "SERVER.EXAMPLE", 2222}, true},
		{"distinct_direct_hosts", ReservationAllocation{"one", "host:192.0.2.1", "192.0.2.1", 22}, ReservationAllocation{"two", "host:192.0.2.2", "192.0.2.2", 22}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resourcesConflict(tc.a, tc.b) != tc.busy || resourcesConflict(tc.b, tc.a) != tc.busy {
				t.Fatalf("conflict should be symmetric and equal %v", tc.busy)
			}
		})
	}
}

func forwardedMachines(c Config) Config {
	first := c.Machines[0]
	first.Host, first.Port, first.ResourceID = "127.0.0.1", 32201, "node-one"
	second := first
	second.ID, second.Port, second.ResourceID = "forward-two", 32202, "node-two"
	c.Machines = []Machine{first, second}
	return c
}

func TestForwardedIndependentReservations(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	c = forwardedMachines(c)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, time.Now())
	if _, status, err := m.Reserve("forward-one", c.Machines[0].ID); status != 201 || err != nil {
		t.Fatalf("first reserve: %d %v", status, err)
	}
	for _, row := range m.Machines() {
		if row.ID == c.Machines[1].ID && (!row.Ready || len(row.ReservationIDs) != 0 || row.Port != c.Machines[1].Port) {
			t.Fatalf("independent forwarded inventory reports occupancy: %+v", row)
		}
	}
	if _, status, err := m.Reserve("forward-two", c.Machines[1].ID); status != 201 || err != nil {
		t.Fatalf("second distinct forwarded node: %d %v", status, err)
	}
}

func TestForwardedAtomicGroupReservation(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	c = forwardedMachines(c)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, time.Now())
	req := GroupReservationRequest{ID: "forward-group", MachineID: c.Machines[0].ID,
		MachineIDs: []string{c.Machines[0].ID, c.Machines[1].ID}, TaskID: "forward-group-task", PlanSHA256: strings.Repeat("a", 64)}
	if _, status, err := m.ReserveGroup(req); status != 201 || err != nil {
		t.Fatalf("distinct forwarded group: %d %v", status, err)
	}
	m.Close()
	m, err := newTaskManager(store, monitor, &fakeTaskRemote{})
	if err != nil {
		t.Fatalf("forwarded group recovery: %v", err)
	}
	defer m.Close()
	if len(m.Ready("", "")) != 0 {
		t.Fatal("restart lost group occupancy")
	}
	if _, status, err := m.ReserveGroup(req); status != 200 || err != nil {
		t.Fatalf("recovered idempotent group: %d %v", status, err)
	}
}

func TestForwardedContinuationCountsPhysicalNodes(t *testing.T) {
	m, monitor, c, store, now, req, _ := continuationFixture(t)
	c = forwardedMachines(c)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, *now)
	if _, _, err := m.RegisterContinuation(req); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContinuations(); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetContinuation(req.ID)
	if got.State != "offered" || got.EligibleCount != 2 || len(got.Allocations) != 2 {
		t.Fatalf("two distinct forwarded nodes did not form an offer: state=%s eligible=%d allocations=%d excluded=%v",
			got.State, got.EligibleCount, len(got.Allocations), got.Excluded)
	}
	m.Close()
	recovered, err := newTaskManager(store, monitor, &fakeTaskRemote{})
	if err != nil {
		t.Fatalf("forwarded offer recovery: %v", err)
	}
	defer recovered.Close()
	if len(recovered.Ready("", "")) != 0 {
		t.Fatal("restart lost forwarded offer occupancy")
	}
}

func TestForwardedTaskPinsIdentityAndPreservesLegacyHold(t *testing.T) {
	remote := &fakeTaskRemote{}
	m, monitor, c, store := setupTasks(t, remote)
	c = forwardedMachines(c)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, time.Now())
	job, code, err := m.Submit(TaskRequest{ID: "forward-task", MachineID: c.Machines[0].ID, Shell: "echo fixture"})
	if code != 202 || err != nil || job.ResourceKey != "physical:node-one" {
		t.Fatalf("submit did not pin identity: %d %v %+v", code, err, job)
	}
	m.Close()
	m, err = newTaskManager(store, monitor, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(m.Ready(c.Machines[1].ID, "")) != 1 {
		t.Fatal("recovered task blocked independent forwarded node")
	}
	// Reassigning the selected machine must not forget the original physical node.
	alias := c.Machines[0]
	alias.ID, alias.Host, alias.Port = "original-node-alias", "192.0.2.42", 22
	c.Machines[0].ResourceID = "changed-node"
	c.Machines = append(c.Machines, alias)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, time.Now())
	if len(m.Ready(alias.ID, "")) != 0 {
		t.Fatal("config edit freed original physical identity")
	}
	c.Machines = c.Machines[:2]
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	// Simulate an older record whose physical identity was never persisted.
	m.mu.Lock()
	m.jobs[job.ID].ResourceKey = ""
	m.mu.Unlock()
	if len(m.Ready(c.Machines[1].ID, "")) != 0 {
		t.Fatal("legacy unidentified task was silently narrowed to one port")
	}
	for _, row := range m.Machines() {
		if row.Ready || len(row.TaskIDs) != 1 {
			t.Fatalf("legacy inventory lost conservative hold: %+v", row)
		}
	}
}

func TestForwardedCleanupAndLegacyReservationsUseSameExclusion(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	c = forwardedMachines(c)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, time.Now())
	m.cleanup.Busy["physical:node-one"] = true
	if len(m.Ready(c.Machines[0].ID, "")) != 0 || len(m.Ready(c.Machines[1].ID, "")) != 1 {
		t.Fatal("cleanup did not isolate the physical forwarded resource")
	}
	delete(m.cleanup.Busy, "physical:node-one")
	m.reservations["legacy"] = Reservation{ID: "legacy", MachineID: c.Machines[0].ID,
		ResourceKey: "host:127.0.0.1", Host: "127.0.0.1", Port: c.Machines[0].Port, CreatedAt: time.Now()}
	if len(m.Ready("", "")) != 0 {
		t.Fatal("upgrade silently narrowed a legacy host-only reservation")
	}
	for _, row := range m.Machines() {
		if row.Ready || len(row.ReservationIDs) != 1 {
			t.Fatalf("legacy reservation inventory lost hold: %+v", row)
		}
	}
}

func TestForwardedReservationAPIIsAtomicAndFenced(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	defer m.Close()
	c = forwardedMachines(c)
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, c, time.Now())
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	call := func(body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "http://"+app.host+"/api/tasks/reservations", bytes.NewReader(data))
		r.Header.Set("X-Watch-Token", app.token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	req := GroupReservationRequest{ID: "api-forward-group", MachineID: c.Machines[0].ID,
		MachineIDs: []string{c.Machines[0].ID, c.Machines[1].ID}, TaskID: "api-forward-task", PlanSHA256: strings.Repeat("a", 64)}
	if _, _, err := m.Reserve("occupied-member", c.Machines[1].ID); err != nil {
		t.Fatal(err)
	}
	if w := call(req); w.Code != 409 || len(m.Reservations()) != 1 {
		t.Fatalf("partial group was held: %d %s", w.Code, w.Body)
	}
	if _, _, err := m.ReleaseReservation("occupied-member"); err != nil {
		t.Fatal(err)
	}
	if w := call(req); w.Code != 201 {
		t.Fatalf("forwarded group API: %d %s", w.Code, w.Body)
	}
	if w := call(req); w.Code != 200 {
		t.Fatalf("idempotent group API: %d %s", w.Code, w.Body)
	}
	if w := call(map[string]string{"id": "intruder", "machineId": c.Machines[1].ID}); w.Code != 409 {
		t.Fatalf("group member stolen: %d %s", w.Code, w.Body)
	}
}
