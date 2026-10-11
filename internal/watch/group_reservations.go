package watch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type ReservationAllocation struct {
	MachineID   string `json:"machineId"`
	ResourceKey string `json:"resourceKey"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
}
type GroupReservationRequest struct {
	ID         string   `json:"id"`
	MachineID  string   `json:"machineId"`
	MachineIDs []string `json:"machineIds"`
	TaskID     string   `json:"taskId"`
	PlanSHA256 string   `json:"planSha256"`
}

func hexDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func reservationAllocations(r Reservation) []ReservationAllocation {
	if len(r.Allocations) > 0 {
		return r.Allocations
	}
	return []ReservationAllocation{{r.MachineID, r.ResourceKey, r.Host, r.Port}}
}
func validateGroupReservation(r Reservation) error {
	if len(r.MachineIDs) == 0 {
		if len(r.Allocations) > 0 || r.GrantID != "" || r.PlanSHA256 != "" || r.TaskID != "" || r.Generation != 0 || r.IntentSHA256 != "" {
			return errors.New("GROUP_RESERVATION_CORRUPT")
		}
		return nil
	}
	if len(r.MachineIDs) < 2 || len(r.MachineIDs) > 12 || len(r.Allocations) != len(r.MachineIDs) || !identifier.MatchString(r.TaskID) || !identifier.MatchString(r.GrantID) || r.Generation != 1 || !hexDigest(r.PlanSHA256) || r.IntentSHA256 != "" && !hexDigest(r.IntentSHA256) {
		return errors.New("GROUP_RESERVATION_CORRUPT")
	}
	seen, hosts, resources := map[string]bool{}, map[string]bool{}, map[string]bool{}
	coordinator := false
	for i, a := range r.Allocations {
		host := strings.ToLower(strings.TrimSpace(a.Host))
		if !identifier.MatchString(a.MachineID) || a.MachineID != r.MachineIDs[i] || seen[a.MachineID] || host == "" || hosts[host] || a.ResourceKey == "" || resources[a.ResourceKey] || a.Port < 1 || a.Port > 65535 {
			return errors.New("GROUP_RESERVATION_CORRUPT")
		}
		seen[a.MachineID], hosts[host], resources[a.ResourceKey] = true, true, true
		if a.MachineID == r.MachineID {
			coordinator = a.Host == r.Host && a.Port == r.Port && a.ResourceKey == r.ResourceKey
		}
	}
	if !coordinator {
		return errors.New("GROUP_RESERVATION_CORRUPT")
	}
	return nil
}

// One mutex and one atomic store write: either the complete physical group is
// reserved or no machine is held. Grants are single-use and have no expiry.
func (m *TaskManager) ReserveGroup(req GroupReservationRequest) (Reservation, int, error) {
	if !identifier.MatchString(req.ID) || !identifier.MatchString(req.MachineID) || !identifier.MatchString(req.TaskID) || !hexDigest(req.PlanSHA256) || len(req.MachineIDs) < 2 || len(req.MachineIDs) > 12 {
		return Reservation{}, 400, errors.New("INVALID_GROUP_RESERVATION")
	}
	seen := map[string]bool{}
	for _, id := range req.MachineIDs {
		if !identifier.MatchString(id) || seen[id] {
			return Reservation{}, 400, errors.New("INVALID_GROUP_MACHINES")
		}
		seen[id] = true
	}
	if !seen[req.MachineID] {
		return Reservation{}, 400, errors.New("GROUP_COORDINATOR_NOT_INCLUDED")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.reservations[req.ID]; ok {
		a, _ := json.Marshal(old.MachineIDs)
		b, _ := json.Marshal(req.MachineIDs)
		if old.MachineID != req.MachineID || old.TaskID != req.TaskID || old.PlanSHA256 != req.PlanSHA256 || string(a) != string(b) {
			return Reservation{}, 409, errors.New("RESERVATION_CONFLICT")
		}
		if old.ReleasedAt != nil {
			return Reservation{}, 410, errors.New("RESERVATION_RELEASED")
		}
		return old, 200, nil
	}
	c, states := m.store.Snapshot(), m.monitor.Snapshot()
	if m.jobs[req.TaskID] != nil {
		return Reservation{}, 409, errors.New("GROUP_TASK_ID_IN_USE")
	}
	ready := map[string]bool{}
	for _, h := range m.readyLocked(c, states, "", "") {
		ready[h.ID] = true
	}
	r := Reservation{ID: req.ID, MachineID: req.MachineID, MachineIDs: append([]string(nil), req.MachineIDs...), TaskID: req.TaskID, PlanSHA256: req.PlanSHA256, GrantID: "group-" + randomToken()[:32], Generation: 1, CreatedAt: m.now()}
	for _, id := range req.MachineIDs {
		if !ready[id] {
			return Reservation{}, 409, errors.New("GROUP_RESOURCE_BUSY_OR_NOT_READY")
		}
		for _, h := range c.Machines {
			if h.ID == id {
				r.Allocations = append(r.Allocations, ReservationAllocation{h.ID, machineResource(h), h.Host, h.Port})
				if id == r.MachineID {
					r.ResourceKey, r.Host, r.Port = machineResource(h), h.Host, h.Port
				}
			}
		}
	}
	if err := validateGroupReservation(r); err != nil {
		return Reservation{}, 400, err
	}
	next := m.reservationSnapshot()
	next[r.ID] = r
	if err := m.saveReservations(next); err != nil {
		return Reservation{}, 500, err
	}
	return r, 201, nil
}
func (m *TaskManager) validateReservationRequestLocked(r Reservation, request TaskRequest, c Config, states map[string]State) error {
	for _, a := range reservationAllocations(r) {
		valid := false
		for _, h := range c.Machines {
			if h.ID == a.MachineID && machineResource(h) == a.ResourceKey && h.Host == a.Host && h.Port == a.Port {
				valid = true
			}
		}
		if !valid {
			return errors.New("RESERVATION_TARGET_CHANGED")
		}
	}
	if len(r.MachineIDs) == 0 {
		if request.ReservationGrantID != "" || request.ReservationGeneration != 0 {
			return errors.New("GROUP_GRANT_NOT_OWNED")
		}
		return nil
	}
	digest := sha256.Sum256([]byte(request.Shell))
	if request.ID != r.TaskID || hex.EncodeToString(digest[:]) != r.PlanSHA256 || request.ReservationGrantID != r.GrantID || request.ReservationGeneration != r.Generation {
		return errors.New("GROUP_GRANT_OR_PLAN_FENCED")
	}
	if r.IntentSHA256 != "" {
		return errors.New("GROUP_LAUNCH_INTENT_UNCERTAIN")
	}
	ready := map[string]bool{}
	for _, h := range m.readyForReservationLocked(c, states, "", "", r.ID) {
		ready[h.ID] = true
	}
	for _, a := range r.Allocations {
		if !ready[a.MachineID] {
			return errors.New("GROUP_RESOURCE_BUSY_OR_NOT_READY")
		}
	}
	return nil
}
func (m *TaskManager) reservationIntentLocked(request TaskRequest) error {
	r := m.reservations[request.ReservationID]
	if len(r.MachineIDs) == 0 {
		return nil
	}
	r.IntentSHA256 = continuationTaskHash(request)
	next := m.reservationSnapshot()
	next[r.ID] = r
	return m.saveReservations(next)
}
