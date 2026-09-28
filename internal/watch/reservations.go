package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reservations have no TTL. Losing a client never proves its remote process ended.
// An ID is single-use, including after release, and fences all later submissions.
type Reservation struct {
	ID          string     `json:"id"`
	MachineID   string     `json:"machineId"`
	ResourceKey string     `json:"resourceKey"`
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	CreatedAt   time.Time  `json:"createdAt"`
	ReleasedAt  *time.Time `json:"releasedAt,omitempty"`
}

func machineResource(machine Machine) string {
	if machine.ResourceID != "" {
		return "physical:" + machine.ResourceID
	}
	// Conservatively share occupancy across SSH ports on the same host.
	return "host:" + strings.ToLower(strings.TrimSpace(machine.Host))
}

func (m *TaskManager) loadReservations() error {
	m.reservations = map[string]Reservation{}
	b, err := os.ReadFile(filepath.Join(m.dir, "reservations.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) > 4*1024*1024 {
		return errors.New("RESERVATION_STORE_TOO_LARGE")
	}
	if err := json.Unmarshal(b, &m.reservations); err != nil || m.reservations == nil {
		return errors.New("RESERVATION_STORE_CORRUPT")
	}
	for id, r := range m.reservations {
		if !identifier.MatchString(id) || r.ID != id || !identifier.MatchString(r.MachineID) || r.ResourceKey == "" || r.CreatedAt.IsZero() {
			return errors.New("RESERVATION_STORE_CORRUPT")
		}
	}
	return nil
}

func (m *TaskManager) saveReservations(next map[string]Reservation) error {
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > 4*1024*1024 {
		return errors.New("RESERVATION_STORE_TOO_LARGE")
	}
	if err := writeAtomic(filepath.Join(m.dir, "reservations.json"), b); err != nil {
		return err
	}
	m.reservations = next
	return nil
}

func (m *TaskManager) reservationSnapshot() map[string]Reservation {
	out := map[string]Reservation{}
	for id, r := range m.reservations {
		out[id] = r
	}
	return out
}

func (m *TaskManager) Reservations() map[string]Reservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reservationSnapshot()
}

func (m *TaskManager) Reserve(id, machineID string) (Reservation, int, error) {
	if !identifier.MatchString(id) || !identifier.MatchString(machineID) {
		return Reservation{}, 400, errors.New("INVALID_RESERVATION")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.reservations[id]; ok {
		if old.MachineID != machineID {
			return Reservation{}, 409, errors.New("RESERVATION_CONFLICT")
		}
		if old.ReleasedAt != nil {
			return Reservation{}, 410, errors.New("RESERVATION_RELEASED")
		}
		return old, 200, nil
	}
	c := m.store.Snapshot()
	ready := m.readyLocked(c, m.monitor.Snapshot(), machineID, "")
	if len(ready) == 0 {
		return Reservation{}, 409, errors.New("RESOURCE_BUSY_OR_NOT_READY")
	}
	var host Machine
	for _, machine := range c.Machines {
		if machine.ID == machineID {
			host = machine
		}
	}
	r := Reservation{ID: id, MachineID: machineID, ResourceKey: machineResource(host), Host: host.Host, Port: host.Port, CreatedAt: time.Now()}
	next := m.reservationSnapshot()
	next[id] = r
	if err := m.saveReservations(next); err != nil {
		return Reservation{}, 500, err
	}
	return r, 201, nil
}

func (m *TaskManager) ReleaseReservation(id string) (Reservation, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.reservations[id]
	if !ok {
		return Reservation{}, 404, errors.New("RESERVATION_NOT_FOUND")
	}
	if r.ReleasedAt != nil {
		return r, 200, nil
	}
	for _, job := range m.jobs {
		if job.ReservationID == id && job.FinishedAt == nil {
			return Reservation{}, 409, fmt.Errorf("REMOTE_TASK_NOT_STOPPED:%s", job.ID)
		}
	}
	now := time.Now()
	r.ReleasedAt = &now
	next := m.reservationSnapshot()
	next[id] = r
	if err := m.saveReservations(next); err != nil {
		return Reservation{}, 500, err
	}
	return r, 200, nil
}
