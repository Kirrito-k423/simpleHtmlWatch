package watch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ContinuationSchema = "simplehtmlwatch.continuation.v1"
const continuationStoreSchema = "simplehtmlwatch.continuations.v1"
const continuationStoreLimit = 8 * 1024 * 1024

type ContinuationTarget struct {
	Provider  string `json:"provider"`
	HostID    string `json:"hostId"`
	ThreadID  string `json:"threadId"`
	Workspace string `json:"workspace"`
}

type ContinuationCheckpoint struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type ContinuationPolicy struct {
	Timezone         string `json:"timezone"`
	CheckEvery       int    `json:"checkEverySeconds"`
	WindowCheckEvery int    `json:"windowCheckEverySeconds"`
	StableFor        int    `json:"stableForSeconds"`
	OfferTTL         int    `json:"offerTTLSeconds"`
	WakeMode         string `json:"wakeMode"`
}

type ContinuationTask struct {
	ID    string `json:"id"`
	Shell string `json:"shell"`
}

type ContinuationRequest struct {
	Schema           string                 `json:"schema"`
	ID               string                 `json:"id"`
	ExperimentID     string                 `json:"experimentId"`
	StepID           string                 `json:"stepId"`
	Target           ContinuationTarget     `json:"target"`
	Resources        ResourceRequirements   `json:"resources"`
	Policy           ContinuationPolicy     `json:"policy"`
	Checkpoint       ContinuationCheckpoint `json:"checkpoint"`
	AuthorizationRef string                 `json:"authorizationRef"`
	Authorized       bool                   `json:"authorized"`
	Tasks            []ContinuationTask     `json:"tasks"`
	ExistingTaskIDs  []string               `json:"existingTaskIds,omitempty"`
	ExpiresAt        *time.Time             `json:"expiresAt,omitempty"`
}

type ContinuationAllocation struct {
	ProfileID   string   `json:"profileId"`
	Username    string   `json:"username"`
	MachineID   string   `json:"machineId"`
	ResourceKey string   `json:"resourceKey"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	TaskID      string   `json:"taskId"`
	DeviceIDs   []string `json:"deviceIds"`
}

type ContinuationWake struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	SenderID    string     `json:"senderId,omitempty"`
	TurnID      string     `json:"turnId,omitempty"`
	Attempts    int        `json:"attempts"`
	SendingAt   *time.Time `json:"sendingAt,omitempty"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
	RetryAt     *time.Time `json:"retryAt,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

type Continuation struct {
	ContinuationRequest
	State         string                   `json:"state"`
	Generation    int                      `json:"generation"`
	CreatedAt     time.Time                `json:"createdAt"`
	UpdatedAt     time.Time                `json:"updatedAt"`
	LastCheckAt   *time.Time               `json:"lastCheckAt,omitempty"`
	NextCheckAt   time.Time                `json:"nextCheckAt"`
	OfferUntil    *time.Time               `json:"offerUntil,omitempty"`
	GrantID       string                   `json:"grantId,omitempty"`
	Allocations   []ContinuationAllocation `json:"allocations"`
	Wake          *ContinuationWake        `json:"wake,omitempty"`
	WakeHistory   []ContinuationWake       `json:"wakeHistory,omitempty"`
	Intents       map[string]string        `json:"intents,omitempty"`
	StopRequested string                   `json:"stopRequested,omitempty"`
	Reason        string                   `json:"reason,omitempty"`
	EligibleCount int                      `json:"eligibleCount"`
	Excluded      map[string]string        `json:"excluded,omitempty"`
	Outcome       string                   `json:"outcome,omitempty"`
	Evidence      string                   `json:"evidence,omitempty"`
}

type continuationStore struct {
	Schema string                  `json:"schema"`
	Jobs   map[string]Continuation `json:"continuations"`
}

type resourceStability struct{ Since, Last time.Time }

func normalizeContinuation(r *ContinuationRequest) error {
	if r.ExpiresAt != nil {
		utc := r.ExpiresAt.UTC()
		r.ExpiresAt = &utc
	}
	if r.Schema == "" {
		r.Schema = ContinuationSchema
	}
	if r.Policy.Timezone == "" {
		r.Policy.Timezone = "Asia/Shanghai"
	}
	if r.Policy.CheckEvery == 0 {
		r.Policy.CheckEvery = 600
	}
	if r.Policy.WindowCheckEvery == 0 {
		r.Policy.WindowCheckEvery = 60
	}
	if r.Policy.OfferTTL == 0 {
		r.Policy.OfferTTL = 300
	}
	if r.Policy.WakeMode == "" {
		r.Policy.WakeMode = "bridge"
	}
	if r.Resources.MaxSampleAge == 0 {
		r.Resources.MaxSampleAge = 60
	}
	if r.Schema != ContinuationSchema || !identifier.MatchString(r.ID) || !identifier.MatchString(r.ExperimentID) || !identifier.MatchString(r.StepID) {
		return errors.New("INVALID_CONTINUATION_ID_OR_SCHEMA")
	}
	if !r.Authorized || strings.TrimSpace(r.AuthorizationRef) == "" || len(r.AuthorizationRef) > 1024 {
		return errors.New("EXPERIMENT_AUTHORIZATION_REQUIRED")
	}
	if r.Target.Provider != "codex" || !identifier.MatchString(r.Target.HostID) || !identifier.MatchString(r.Target.ThreadID) || !filepath.IsAbs(r.Target.Workspace) || strings.ContainsAny(r.Target.Workspace, "\x00\r\n") {
		return errors.New("INVALID_CONTINUATION_TARGET")
	}
	if !filepath.IsAbs(r.Checkpoint.Path) || strings.ContainsAny(r.Checkpoint.Path, "\x00\r\n") || len(r.Checkpoint.Digest) != 64 {
		return errors.New("INVALID_CHECKPOINT")
	}
	if _, err := hex.DecodeString(r.Checkpoint.Digest); err != nil {
		return errors.New("INVALID_CHECKPOINT_DIGEST")
	}
	r.Checkpoint.Digest = strings.ToLower(r.Checkpoint.Digest)
	p := r.Policy
	if p.Timezone != "Asia/Shanghai" || p.CheckEvery < 60 || p.CheckEvery > 86400 || p.WindowCheckEvery < 10 || p.WindowCheckEvery > p.CheckEvery || p.StableFor < 0 || p.StableFor > 86400 || p.OfferTTL < 30 || p.OfferTTL > 1800 || (p.WakeMode != "bridge" && p.WakeMode != "heartbeat") {
		return errors.New("INVALID_CONTINUATION_POLICY")
	}
	v := r.Resources
	if v.MachineCount < 1 || v.MachineCount > 200 || v.DevicesPerMachine < 1 || v.DevicesPerMachine > 256 || strings.TrimSpace(v.Model) == "" || len(v.Model) > 80 || v.MinFreeHBMMB < 0 || math.IsNaN(v.MaxUtilization) || math.IsInf(v.MaxUtilization, 0) || v.MaxUtilization < 0 || v.MaxUtilization > 100 || v.MaxSampleAge < 5 || v.MaxSampleAge > 3600 || len(v.Labels) > 16 || (v.Group != "" && len(v.MachineIDs) > 0) || (v.Group == "" && len(v.MachineIDs) == 0) || len(v.Group) > 80 {
		return errors.New("INVALID_RESOURCE_CONTRACT")
	}
	seen := map[string]bool{}
	for _, id := range v.MachineIDs {
		if !identifier.MatchString(id) || seen[id] {
			return errors.New("INVALID_RESOURCE_MACHINE_IDS")
		}
		seen[id] = true
	}
	if len(v.MachineIDs) > 0 && len(v.MachineIDs) < v.MachineCount {
		return errors.New("INSUFFICIENT_RESOURCE_SCOPE")
	}
	for key, value := range v.Labels {
		if !identifier.MatchString(key) || value == "" || len(value) > 160 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("INVALID_RESOURCE_LABEL")
		}
	}
	if len(r.Tasks) != v.MachineCount || len(r.ExistingTaskIDs) > 200 {
		return errors.New("TASK_PLAN_COUNT_MISMATCH")
	}
	seen = map[string]bool{}
	for i := range r.Tasks {
		r.Tasks[i].Shell = strings.TrimSpace(r.Tasks[i].Shell)
		v := r.Tasks[i]
		if !identifier.MatchString(v.ID) || seen[v.ID] || v.Shell == "" || len(v.Shell) > 4096 || strings.ContainsRune(v.Shell, 0) || watchPrefix.MatchString(v.Shell) {
			return errors.New("INVALID_CONTINUATION_TASK_PLAN")
		}
		seen[v.ID] = true
	}
	for _, id := range r.ExistingTaskIDs {
		if !identifier.MatchString(id) || seen[id] {
			return errors.New("INVALID_PRIOR_TASK_ID")
		}
		seen[id] = true
	}
	return nil
}

func cloneContinuation(c Continuation) Continuation {
	b, _ := json.Marshal(c)
	var out Continuation
	_ = json.Unmarshal(b, &out)
	return out
}

func (m *TaskManager) continuationSnapshotLocked() map[string]Continuation {
	next := make(map[string]Continuation, len(m.continuations))
	for id, job := range m.continuations {
		next[id] = cloneContinuation(job)
	}
	return next
}

func (m *TaskManager) saveContinuationsLocked(next map[string]Continuation) error {
	b, err := json.Marshal(continuationStore{continuationStoreSchema, next})
	if err != nil {
		return err
	}
	if len(b) > continuationStoreLimit {
		return errors.New("CONTINUATION_STORE_TOO_LARGE")
	}
	if err := m.budget.Write(filepath.Join(m.dir, "continuations.json"), b, 0); err != nil {
		return err
	}
	m.continuations = next
	return nil
}

func (m *TaskManager) loadContinuations() error {
	m.continuations = map[string]Continuation{}
	m.stability = map[string]map[string]resourceStability{}
	path := filepath.Join(m.dir, "continuations.json")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() > continuationStoreLimit {
		return errors.New("CONTINUATION_STORE_TOO_LARGE")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var saved continuationStore
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil || saved.Schema != continuationStoreSchema || saved.Jobs == nil || len(saved.Jobs) > 1000 {
		return errors.New("CONTINUATION_STORE_CORRUPT")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("CONTINUATION_STORE_CORRUPT")
	}
	states := map[string]bool{"waiting_resources": true, "offered": true, "claimed": true, "executing": true, "awaiting_review": true, "recovery_needed": true, "completed": true, "paused": true, "cancelled": true, "expired": true}
	usedTasks := map[string]bool{}
	held := []ReservationAllocation{}
	for id, c := range saved.Jobs {
		if normalizeContinuation(&c.ContinuationRequest) != nil || id != c.ID || !states[c.State] || c.CreatedAt.IsZero() || c.Generation < 0 {
			return errors.New("CONTINUATION_STORE_CORRUPT")
		}
		for _, task := range c.Tasks {
			if usedTasks[task.ID] {
				return errors.New("CONTINUATION_TASK_PLAN_CORRUPT")
			}
			usedTasks[task.ID] = true
		}
		if c.Wake != nil {
			validStatus := map[string]bool{"pending": true, "sending": true, "delivered": true, "delivery_uncertain": true, "failed": true}
			if !identifier.MatchString(c.Wake.ID) || !validStatus[c.Wake.Status] || (c.Wake.Kind != "resources_ready" && c.Wake.Kind != "tasks_finished") || c.Wake.Attempts < 0 {
				return errors.New("CONTINUATION_WAKE_CORRUPT")
			}
		}
		if c.Wake != nil && c.Wake.Status == "sending" {
			c.Wake.Status, c.Wake.Reason = "delivery_uncertain", "bridge_or_service_restarted; query original event before retry"
		}
		if continuationOccupied(c) && (len(c.Allocations) != c.Resources.MachineCount || !identifier.MatchString(c.GrantID) || c.Generation < 1 || c.Wake == nil || (c.State == "offered" && c.OfferUntil == nil)) {
			return errors.New("CONTINUATION_GRANT_CORRUPT")
		}
		if continuationOccupied(c) {
			for i, allocation := range c.Allocations {
				target := continuationAllocation(allocation)
				if !identifier.MatchString(allocation.MachineID) || !identifier.MatchString(allocation.ProfileID) || allocation.Username == "" || allocation.ResourceKey == "" || strings.TrimSpace(allocation.Host) == "" || allocation.Port < 1 || allocation.Port > 65535 || allocation.TaskID != c.Tasks[i].ID || conflictsWithAny(target, held) {
					return errors.New("CONTINUATION_GRANT_CORRUPT")
				}
				for _, reservation := range m.reservations {
					if reservation.ReleasedAt == nil {
						for _, a := range reservationAllocations(reservation) {
							if resourcesConflict(a, target) {
								return errors.New("CONTINUATION_RESERVATION_CONFLICT")
							}
						}
					}
				}
				held = append(held, target)
			}
		}
		m.continuations[id] = c
	}
	return nil
}

func continuationOccupied(c Continuation) bool {
	switch c.State {
	case "offered", "claimed", "executing", "awaiting_review", "recovery_needed":
		return true
	}
	return false
}

func retainContinuationWake(c *Continuation) {
	if c.Wake == nil {
		return
	}
	c.WakeHistory = append(append([]ContinuationWake(nil), c.WakeHistory...), *c.Wake)
	if len(c.WakeHistory) > 32 {
		c.WakeHistory = append([]ContinuationWake(nil), c.WakeHistory[len(c.WakeHistory)-32:]...)
	}
}

func (m *TaskManager) validateContinuationTask(job TaskJob) error {
	if job.ContinuationID == "" {
		return nil
	}
	c, ok := m.continuations[job.ContinuationID]
	if !ok || c.Intents[job.ID] != job.requestHash() || (c.State != "completed" && (!continuationOccupied(c) || c.GrantID != job.GrantID || c.Generation != job.Generation)) {
		return errors.New("CONTINUATION_STORE_MISSING_OR_CHANGED_FOR_TASK")
	}
	for _, planned := range c.Tasks {
		if planned.ID == job.ID {
			request := job.TaskRequest
			request.Shell = planned.Shell
			if continuationTaskHash(request) == job.requestHash() {
				return nil
			}
		}
	}
	return errors.New("CONTINUATION_TASK_PLAN_CHANGED")
}

func continuationTaskHash(r TaskRequest) string {
	b, _ := json.Marshal(r)
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func (m *TaskManager) RegisterContinuation(request ContinuationRequest) (Continuation, int, error) {
	if err := normalizeContinuation(&request); err != nil {
		return Continuation{}, 400, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.continuations[request.ID]; ok {
		oldJSON, _ := json.Marshal(old.ContinuationRequest)
		requestJSON, _ := json.Marshal(request)
		if !bytes.Equal(oldJSON, requestJSON) {
			return Continuation{}, 409, errors.New("CONTINUATION_ID_CONFLICT")
		}
		return cloneContinuation(old), 200, nil
	}
	if len(m.continuations) >= 1000 {
		return Continuation{}, 409, errors.New("CONTINUATION_LIMIT")
	}
	for _, task := range request.Tasks {
		if m.jobs[task.ID] != nil {
			return Continuation{}, 409, errors.New("TASK_ID_ALREADY_EXISTS")
		}
		for _, c := range m.continuations {
			for _, planned := range c.Tasks {
				if task.ID == planned.ID {
					return Continuation{}, 409, errors.New("TASK_ID_RESERVED_BY_CONTINUATION")
				}
			}
		}
	}
	for _, c := range m.continuations {
		if c.ExperimentID == request.ExperimentID && c.StepID == request.StepID {
			return Continuation{}, 409, errors.New("EXPERIMENT_STEP_ALREADY_REGISTERED")
		}
	}
	now := m.now()
	c := Continuation{ContinuationRequest: request, State: "waiting_resources", CreatedAt: now, UpdatedAt: now, NextCheckAt: now, Allocations: []ContinuationAllocation{}}
	next := m.continuationSnapshotLocked()
	next[c.ID] = cloneContinuation(c)
	if err := m.saveContinuationsLocked(next); err != nil {
		return Continuation{}, 500, err
	}
	return cloneContinuation(c), 201, nil
}

func (m *TaskManager) Continuations() []Continuation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Continuation, 0, len(m.continuations))
	for _, c := range m.continuations {
		out = append(out, cloneContinuation(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (m *TaskManager) GetContinuation(id string) (Continuation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.continuations[id]
	return cloneContinuation(c), ok
}

type ContinuationClaim struct {
	ID               string `json:"id"`
	Generation       int    `json:"generation"`
	WakeEventID      string `json:"wakeEventId"`
	ThreadID         string `json:"threadId"`
	CheckpointDigest string `json:"checkpointDigest"`
}

func (m *TaskManager) ClaimContinuation(r ContinuationClaim) (Continuation, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.continuations[r.ID]
	if !ok {
		return Continuation{}, 404, errors.New("CONTINUATION_NOT_FOUND")
	}
	c = cloneContinuation(c)
	if r.Generation != c.Generation || c.Wake == nil || r.WakeEventID != c.Wake.ID || r.ThreadID != c.Target.ThreadID || r.CheckpointDigest != c.Checkpoint.Digest {
		return Continuation{}, 409, errors.New("CONTINUATION_CLAIM_FENCED")
	}
	if c.StopRequested != "" {
		return Continuation{}, 409, errors.New("CONTINUATION_STOP_REQUESTED")
	}
	if c.State == "claimed" || c.State == "executing" || c.State == "awaiting_review" {
		return cloneContinuation(c), 200, nil
	}
	now := m.now()
	if c.State != "offered" || c.OfferUntil == nil || !now.Before(*c.OfferUntil) || (c.ExpiresAt != nil && !now.Before(*c.ExpiresAt)) {
		return Continuation{}, 410, errors.New("CONTINUATION_OFFER_EXPIRED")
	}
	config, states := m.store.Snapshot(), m.monitor.Snapshot()
	now = m.now()
	if !now.Before(*c.OfferUntil) || (c.ExpiresAt != nil && !now.Before(*c.ExpiresAt)) {
		return Continuation{}, 410, errors.New("CONTINUATION_OFFER_EXPIRED")
	}
	for _, allocation := range c.Allocations {
		machine, found := continuationMachine(config, allocation)
		if !found || !continuationScope(machine, c.Resources) || len(m.readyForReservationLocked(config, states, machine.ID, "", c.GrantID)) == 0 {
			return Continuation{}, 409, errors.New("CONTINUATION_RESOURCE_CHANGED")
		}
		if _, _, err := resourceSample(machine, states[machine.ID], c.Resources, now); err != nil {
			return Continuation{}, 409, err
		}
	}
	c.State, c.Reason, c.OfferUntil, c.UpdatedAt = "claimed", "", nil, now
	c.Wake.Status, c.Wake.DeliveredAt = "delivered", &now // Claim is independent proof of delivery.
	next := m.continuationSnapshotLocked()
	next[c.ID] = c
	if err := m.saveContinuationsLocked(next); err != nil {
		return Continuation{}, 500, err
	}
	return cloneContinuation(c), 200, nil
}

func continuationMachine(config Config, a ContinuationAllocation) (Machine, bool) {
	for _, machine := range config.Machines {
		if machine.ID == a.MachineID && machine.ProfileID == a.ProfileID && machineResource(machine) == a.ResourceKey && machine.Host == a.Host && machine.Port == a.Port {
			for _, profile := range config.Profiles {
				if profile.ID == a.ProfileID && profile.Username == a.Username {
					return machine, true
				}
			}
		}
	}
	return Machine{}, false
}

func continuationScope(machine Machine, r ResourceRequirements) bool {
	if r.Group != "" {
		return machine.Group == r.Group
	}
	for _, id := range r.MachineIDs {
		if id == machine.ID {
			return true
		}
	}
	return false
}

// Validate and persist the launch intent BEFORE job.json and the remote goroutine.
// A recovered intent without a job is uncertain, never a licence to relaunch.
func (m *TaskManager) continuationSubmitLocked(r TaskRequest, config Config, states map[string]State) (string, error) {
	for _, c := range m.continuations {
		for _, planned := range c.Tasks {
			if planned.ID == r.ID && c.ID != r.ContinuationID {
				return "", errors.New("TASK_ID_RESERVED_BY_CONTINUATION")
			}
		}
	}
	if r.ContinuationID == "" {
		if r.GrantID != "" || r.Generation != 0 {
			return "", errors.New("CONTINUATION_FIELDS_INCOMPLETE")
		}
		return r.ReservationID, nil
	}
	c, ok := m.continuations[r.ContinuationID]
	if !ok || r.ReservationID != "" || c.GrantID != r.GrantID || c.Generation != r.Generation || c.StopRequested != "" || (c.State != "claimed" && c.State != "executing") {
		return "", errors.New("CONTINUATION_GRANT_FENCED")
	}
	if c.ExpiresAt != nil && !m.now().Before(*c.ExpiresAt) {
		return "", errors.New("CONTINUATION_AUTHORIZATION_EXPIRED")
	}
	var allocation *ContinuationAllocation
	for i := range c.Allocations {
		if c.Allocations[i].TaskID == r.ID && c.Allocations[i].MachineID == r.MachineID {
			allocation = &c.Allocations[i]
		}
	}
	if allocation == nil || r.Group != "" {
		return "", errors.New("CONTINUATION_TASK_NOT_PLANNED")
	}
	matched := false
	for _, planned := range c.Tasks {
		if planned.ID == r.ID && planned.Shell == r.Shell {
			matched = true
		}
	}
	if !matched {
		return "", errors.New("CONTINUATION_TASK_REQUEST_CHANGED")
	}
	if c.Intents[r.ID] != "" {
		return "", errors.New("CONTINUATION_LAUNCH_INTENT_UNCERTAIN")
	}
	machine, found := continuationMachine(config, *allocation)
	if !found || !continuationScope(machine, c.Resources) {
		return "", errors.New("CONTINUATION_TARGET_CHANGED")
	}
	if _, _, err := resourceSample(machine, states[machine.ID], c.Resources, m.now()); err != nil {
		return "", err
	}
	return c.GrantID, nil
}

func (m *TaskManager) continuationIntentLocked(r TaskRequest) error {
	if r.ContinuationID == "" {
		return nil
	}
	next := m.continuationSnapshotLocked()
	c := next[r.ContinuationID]
	if c.Intents == nil {
		c.Intents = map[string]string{}
	}
	c.Intents[r.ID] = continuationTaskHash(r)
	c.State, c.UpdatedAt = "executing", m.now()
	next[c.ID] = c
	return m.saveContinuationsLocked(next)
}

type ContinuationControl struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	WakeMode string `json:"wakeMode,omitempty"`
}

func (m *TaskManager) ControlContinuation(r ContinuationControl) (Continuation, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.continuations[r.ID]
	if !ok {
		return Continuation{}, 404, errors.New("CONTINUATION_NOT_FOUND")
	}
	if c.State == "completed" || c.State == "cancelled" || c.State == "expired" {
		return Continuation{}, 409, errors.New("CONTINUATION_TERMINAL")
	}
	switch r.Action {
	case "set-mode":
		if (r.WakeMode != "bridge" && r.WakeMode != "heartbeat") || len(c.Intents) != 0 || (c.State != "waiting_resources" && c.State != "offered" && c.State != "paused") {
			return Continuation{}, 409, errors.New("CONTINUATION_MODE_CHANGE_UNSAFE")
		}
		if c.Wake != nil && (c.Wake.Status == "sending" || c.Wake.Status == "delivery_uncertain") {
			return Continuation{}, 409, errors.New("QUERY_UNCERTAIN_DELIVERY_BEFORE_MODE_CHANGE")
		}
		retainContinuationWake(&c)
		c.Policy.WakeMode, c.Generation, c.Wake = r.WakeMode, c.Generation+1, nil
		c.Allocations, c.GrantID, c.OfferUntil = []ContinuationAllocation{}, "", nil
		if c.State != "paused" {
			c.State, c.NextCheckAt = "waiting_resources", m.now()
		}
	case "pause", "cancel":
		if c.State == "waiting_resources" || c.State == "offered" || c.State == "paused" {
			c.State = "paused"
			if r.Action == "cancel" {
				c.State = "cancelled"
			}
			c.Allocations, c.GrantID, c.OfferUntil = []ContinuationAllocation{}, "", nil
		} else {
			c.StopRequested = r.Action
		}
	case "resume":
		if c.State == "paused" {
			c.State, c.NextCheckAt = "waiting_resources", m.now()
		}
		c.StopRequested = ""
	default:
		return Continuation{}, 400, errors.New("INVALID_CONTINUATION_CONTROL")
	}
	c.UpdatedAt = m.now()
	next := m.continuationSnapshotLocked()
	next[c.ID] = c
	if err := m.saveContinuationsLocked(next); err != nil {
		return Continuation{}, 500, err
	}
	return cloneContinuation(c), 200, nil
}

type ContinuationReport struct {
	ContinuationClaim
	Action   string `json:"action"`
	Outcome  string `json:"outcome,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

func (m *TaskManager) ReportContinuation(r ContinuationReport) (Continuation, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.continuations[r.ID]
	if !ok {
		return Continuation{}, 404, errors.New("CONTINUATION_NOT_FOUND")
	}
	if c.Wake == nil || r.Generation != c.Generation || r.WakeEventID != c.Wake.ID || r.ThreadID != c.Target.ThreadID || r.CheckpointDigest != c.Checkpoint.Digest {
		return Continuation{}, 409, errors.New("CONTINUATION_REPORT_FENCED")
	}
	if c.State == "completed" && r.Action == "complete" && r.Outcome == c.Outcome && r.Evidence == c.Evidence {
		return cloneContinuation(c), 200, nil
	}
	if !continuationOccupied(c) || c.State == "offered" {
		return Continuation{}, 409, errors.New("CONTINUATION_NOT_CLAIMED")
	}
	switch r.Action {
	case "release-unused":
		if len(c.Intents) != 0 {
			return Continuation{}, 409, errors.New("LAUNCH_INTENTS_PREVENT_RELEASE")
		}
		for _, task := range c.Tasks {
			if m.jobs[task.ID] != nil {
				return Continuation{}, 409, errors.New("TASKS_PREVENT_RELEASE")
			}
		}
		c.State, c.NextCheckAt = "waiting_resources", m.now()
		if c.StopRequested == "pause" {
			c.State = "paused"
		}
		if c.StopRequested == "cancel" {
			c.State = "cancelled"
		}
	case "complete":
		if (r.Outcome != "succeeded" && r.Outcome != "failed") || strings.TrimSpace(r.Evidence) == "" || len(r.Evidence) > 4096 {
			return Continuation{}, 400, errors.New("EXPERIMENT_EVIDENCE_REQUIRED")
		}
		for _, task := range c.Tasks {
			job := m.jobs[task.ID]
			if job == nil || job.ContinuationID != c.ID || job.GrantID != c.GrantID || job.FinishedAt == nil || job.ExitCode == nil {
				return Continuation{}, 409, fmt.Errorf("TASK_NOT_FINISHED:%s", task.ID)
			}
		}
		c.State, c.Outcome, c.Evidence = "completed", r.Outcome, r.Evidence
	default:
		return Continuation{}, 400, errors.New("INVALID_CONTINUATION_REPORT")
	}
	c.Allocations, c.GrantID, c.OfferUntil, c.UpdatedAt = []ContinuationAllocation{}, "", nil, m.now()
	next := m.continuationSnapshotLocked()
	next[c.ID] = c
	if err := m.saveContinuationsLocked(next); err != nil {
		return Continuation{}, 500, err
	}
	delete(m.stability, c.ID)
	return cloneContinuation(c), 200, nil
}
