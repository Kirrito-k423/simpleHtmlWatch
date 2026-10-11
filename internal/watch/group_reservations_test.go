package watch

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"
)

func TestGroupLaunchIntentCannotReplayOrReleaseAfterRestart(t *testing.T) {
	m, monitor, c, store := setupGroup(t)
	ids := []string{}
	for _, h := range c.Machines {
		ids = append(ids, h.ID)
	}
	shell := "echo frozen"
	digest := sha256.Sum256([]byte(shell))
	r, _, err := m.ReserveGroup(GroupReservationRequest{ID: "intent-group", MachineID: ids[0], MachineIDs: ids, TaskID: "intent-task", PlanSHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(m.path(r.TaskID, ""), 0700); err != nil {
		t.Fatal(err)
	} // Inject job-save gap.
	req := TaskRequest{ID: r.TaskID, Shell: shell, MachineID: r.MachineID, ReservationID: r.ID, ReservationGrantID: r.GrantID, ReservationGeneration: r.Generation}
	if _, code, _ := m.Submit(req); code != 500 {
		t.Fatal("expected save gap", code)
	}
	if _, code, _ := m.Submit(req); code != 409 {
		t.Fatal("uncertain intent replayed", code)
	}
	m.Close()
	m, err = newTaskManager(store, monitor, &fakeTaskRemote{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, code, _ := m.ReleaseReservation(r.ID); code != 409 {
		t.Fatal("uncertain group released")
	}
	if _, code, _ := m.Submit(req); code != 409 {
		t.Fatal("restart replayed uncertain intent")
	}
	if len(m.Ready("", "")) != 0 {
		t.Fatal("restart lost group locks")
	}
}

func TestGroupOwnedCoordinatorUsesFrozenPlanAndCannotReleaseWhileRunning(t *testing.T) {
	m, _, c, _ := setupGroup(t)
	defer m.Close()
	ids := []string{}
	for _, h := range c.Machines {
		ids = append(ids, h.ID)
	}
	shell := "echo frozen"
	digest := sha256.Sum256([]byte(shell))
	r, _, err := m.ReserveGroup(GroupReservationRequest{ID: "coordinator-group", MachineID: ids[0], MachineIDs: ids, TaskID: "coordinator-task", PlanSHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	req := TaskRequest{ID: r.TaskID, Shell: shell, MachineID: r.MachineID, ReservationID: r.ID, ReservationGrantID: r.GrantID, ReservationGeneration: r.Generation}
	changed := req
	changed.Shell = "echo changed"
	if _, code, _ := m.Submit(changed); code != 409 {
		t.Fatal("unfrozen command executed")
	}
	if _, code, err := m.Submit(req); err != nil || code != 202 {
		t.Fatalf("submit %d %v", code, err)
	}
	if _, code, _ := m.ReleaseReservation(r.ID); code != 409 {
		t.Fatal("live coordinator freed group")
	}
	if _, code, err := m.Submit(req); err != nil || code != 200 {
		t.Fatalf("original task query %d %v", code, err)
	}
}

func setupGroup(t *testing.T) (*TaskManager, *Monitor, Config, *Store) {
	t.Helper()
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	first := c.Machines[0]
	for i := 1; i < 8; i++ {
		host := first
		host.ID = string(rune('a'+i)) + "-group"
		host.Host = "192.0.2." + string(rune('1'+i))
		c.Machines = append(c.Machines, host)
	}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor.mu.Lock()
	for _, h := range c.Machines {
		monitor.states[h.ID] = State{MachineID: h.ID, Status: "online", UpdatedAt: time.Now()}
	}
	monitor.mu.Unlock()
	return m, monitor, c, store
}

func TestGroupReservationAtomicConflictAndRestart(t *testing.T) {
	m, monitor, c, store := setupGroup(t)
	ids := []string{}
	for _, h := range c.Machines {
		ids = append(ids, h.ID)
	}
	req := GroupReservationRequest{ID: "whole-eight", MachineID: ids[0], MachineIDs: ids, TaskID: "coordinator", PlanSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
	_, _, err := m.Reserve("busy", ids[7])
	if err != nil {
		t.Fatal(err)
	}
	if _, code, _ := m.ReserveGroup(req); code != 409 {
		t.Fatal("partly held a busy group", code)
	}
	if len(m.Reservations()) != 1 {
		t.Fatal("partial group lock leaked")
	}
	if _, _, err = m.ReleaseReservation("busy"); err != nil {
		t.Fatal(err)
	}
	r, code, err := m.ReserveGroup(req)
	if err != nil || code != 201 || len(r.Allocations) != 8 || r.GrantID == "" {
		t.Fatalf("group: %d %v %+v", code, err, r)
	}
	if _, code, _ = m.Reserve("intruder", ids[6]); code != 409 {
		t.Fatal("group member was not held")
	}
	m.Close()
	m, err = newTaskManager(store, monitor, &fakeTaskRemote{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	recovered, code, err := m.ReserveGroup(req)
	if err != nil || code != 200 || recovered.GrantID != r.GrantID {
		t.Fatal("restart changed grant", err)
	}
	if len(m.Ready("", "")) != 0 {
		t.Fatal("restart lost group occupancy")
	}
	stale := TaskRequest{ID: req.TaskID, MachineID: req.MachineID, ReservationID: r.ID, ReservationGrantID: "stale", ReservationGeneration: 1, Shell: "echo done"}
	if _, code, _ := m.Submit(stale); code != 409 {
		t.Fatal("stale group grant executed")
	}
	req.PlanSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, code, _ := m.ReserveGroup(req); code != 409 {
		t.Fatal("changed plan accepted")
	}
}

func TestConcurrentGroupReservationsHaveOneWinner(t *testing.T) {
	m, _, c, _ := setupGroup(t)
	defer m.Close()
	ids := []string{}
	for _, h := range c.Machines {
		ids = append(ids, h.ID)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for _, id := range []string{"group-one", "group-two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, code, err := m.ReserveGroup(GroupReservationRequest{ID: id, MachineID: ids[0], MachineIDs: ids, TaskID: id + "-task", PlanSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
			if err == nil && code == 201 {
				mu.Lock()
				won++
				mu.Unlock()
			} else if code != 409 {
				t.Errorf("unexpected: %d %v", code, err)
			}
		}(id)
	}
	wg.Wait()
	if won != 1 {
		t.Fatal("atomic reservation winners", won)
	}
}
