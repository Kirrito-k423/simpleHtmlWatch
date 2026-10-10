package watch

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func continuationLowPeak(now time.Time) bool {
	t := now.In(chinaTime)
	minute := t.Hour()*60 + t.Minute()
	return minute >= 150 && minute < 360 || minute >= 750 && minute < 840
}

func nextContinuationCheck(now time.Time, p ContinuationPolicy) time.Time {
	seconds := p.CheckEvery
	if continuationLowPeak(now) {
		seconds = p.WindowCheckEvery
	}
	next := now.Add(time.Duration(seconds) * time.Second)
	t := now.In(chinaTime)
	for _, minute := range []int{150, 360, 750, 840} {
		boundary := time.Date(t.Year(), t.Month(), t.Day(), minute/60, minute%60, 0, 0, chinaTime)
		if !boundary.After(now) {
			boundary = boundary.AddDate(0, 0, 1)
		}
		if boundary.Before(next) {
			next = boundary
		}
	}
	return next
}

func (m *TaskManager) startContinuations() {
	m.mu.Lock()
	if m.continuationStarted {
		m.mu.Unlock()
		return
	}
	m.continuationStarted = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				if err := m.CheckContinuations(); err != nil {
					m.mu.Lock()
					m.continuationError = err.Error()
					m.mu.Unlock()
				}
			}
		}
	}()
}

// The shared TaskManager mutex serializes offers with ALL submission/reservation
// paths. Save a whole bundle and its outbox event in one atomic snapshot.
func (m *TaskManager) CheckContinuations() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.continuations) == 0 {
		return nil
	}
	now := m.now()
	old := m.continuations
	next := make(map[string]Continuation, len(old))
	for id, c := range old {
		next[id] = c
	}
	var config Config
	var states map[string]State
	loadSamples := func() {
		if states == nil {
			config, states = m.store.Snapshot(), m.monitor.Snapshot()
			now = m.now() // A concurrently completed sample may postdate tick entry.
		}
	}
	m.continuations = next // Private transactional view; mutex prevents observers.
	changed := false
	defer func() {
		if !changed {
			m.continuations = old
		}
	}()
	ids := make([]string, 0, len(next))
	for id := range next {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := next[ids[i]], next[ids[j]]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	parts := []string{}
	for id, task := range m.jobs {
		if task.FinishedAt == nil {
			parts = append(parts, "task:"+id)
		}
	}
	for id, reservation := range m.reservations {
		if reservation.ReleasedAt == nil {
			parts = append(parts, "reservation:"+id)
		}
	}
	sort.Strings(parts)
	revision := strings.Join(parts, ",")
	capacityChanged := m.availabilityRevision != revision
	for _, id := range ids {
		c := next[id]
		if c.Wake != nil {
			wake := *c.Wake
			c.Wake = &wake
		}
		localChanged := false
		if c.Wake != nil && c.Wake.Status == "sending" && c.Wake.SendingAt != nil && now.Sub(*c.Wake.SendingAt) > 60*time.Second {
			c.Wake.Status, c.Wake.Reason = "delivery_uncertain", "delivery_receipt_timeout; query original event"
			localChanged = true
		}
		if c.State == "offered" && (c.OfferUntil == nil || !now.Before(*c.OfferUntil)) {
			c.State, c.Reason, c.NextCheckAt = "waiting_resources", "offer_expired", now
			c.Allocations, c.GrantID, c.OfferUntil = []ContinuationAllocation{}, "", nil
			localChanged = true
		}
		if (c.State == "waiting_resources" || c.State == "offered" || c.State == "paused") && c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
			c.State, c.Reason = "expired", "authorization_expired"
			c.Allocations, c.GrantID, c.OfferUntil = []ContinuationAllocation{}, "", nil
			localChanged = true
		}
		if c.State == "claimed" || c.State == "executing" || c.State == "recovery_needed" {
			allFinished, uncertain := len(c.Intents) > 0, false
			for _, task := range c.Tasks {
				job := m.jobs[task.ID]
				if job == nil {
					allFinished = false
					if c.Intents[task.ID] != "" {
						uncertain = true
					}
					continue
				}
				if job.FinishedAt == nil {
					allFinished = false
					if job.Status == "unknown" {
						uncertain = true
					}
				}
			}
			if allFinished {
				retainContinuationWake(&c)
				c.State, c.Reason = "awaiting_review", "remote_tasks_finished; verify results before completing"
				c.Wake = &ContinuationWake{ID: "wake-" + randomToken()[:24], Kind: "tasks_finished", Status: "pending"}
				localChanged = true
			} else if uncertain && c.State != "recovery_needed" {
				c.State, c.Reason = "recovery_needed", "query_original_task_ids; never_relaunch_or_expire_grant"
				localChanged = true
			} else if !uncertain && c.State == "recovery_needed" {
				c.State, c.Reason = "executing", ""
				localChanged = true
			}
		}
		if c.State == "waiting_resources" && c.StopRequested == "" {
			// Observe each fresh sample even between scheduling checks so stableFor
			// cannot be inferred from two endpoints ten minutes apart.
			due := capacityChanged || !now.Before(c.NextCheckAt)
			if c.Policy.StableFor > 0 || due {
				loadSamples()
				candidates, excluded := m.continuationCandidatesLocked(c, config, states, now)
				if due {
					c.LastCheckAt, c.NextCheckAt, c.Excluded, c.EligibleCount = &now, nextContinuationCheck(now, c.Policy), excluded, len(candidates)
					c.Reason = fmt.Sprintf("eligible %d / required %d", len(candidates), c.Resources.MachineCount)
					if c.Wake != nil && (c.Wake.Status == "sending" || c.Wake.Status == "delivery_uncertain") {
						c.Reason = "delivery_uncertain; query_old_wake_before_new_generation"
					} else if len(candidates) >= c.Resources.MachineCount && c.Policy.WakeMode == "bridge" {
						m.offerContinuation(&c, candidates, now)
					}
					localChanged = true
				}
			}
		}
		if localChanged {
			c.UpdatedAt = now
			next[id] = c
			changed = true
		}
	}
	if changed {
		if err := m.saveContinuationsLocked(next); err != nil {
			m.continuations = old
			changed = false
			return err
		}
	}
	m.availabilityRevision, m.continuationError = revision, ""
	return nil
}

func (m *TaskManager) continuationCandidatesLocked(c Continuation, config Config, states map[string]State, now time.Time) ([]ContinuationAllocation, map[string]string) {
	excluded := map[string]string{}
	for _, id := range c.ExistingTaskIDs {
		if job := m.jobs[id]; job == nil || job.FinishedAt == nil {
			excluded["prior_task:"+id] = "prior_task_unresolved"
			delete(m.stability, c.ID)
			return nil, excluded
		}
	}
	ready := map[string]bool{}
	for _, machine := range m.readyLocked(config, states, "", "") {
		ready[machine.ID] = true
	}
	allowed := map[string]bool{}
	for _, id := range c.Resources.MachineIDs {
		allowed[id] = true
	}
	if m.stability[c.ID] == nil {
		m.stability[c.ID] = map[string]resourceStability{}
	}
	stable := m.stability[c.ID]
	out := []ContinuationAllocation{}
	seen := map[string]bool{}
	seenHosts := map[string]bool{}
	machines := append([]Machine(nil), config.Machines...)
	sort.Slice(machines, func(i, j int) bool { return machines[i].ID < machines[j].ID })
	for _, machine := range machines {
		if c.Resources.Group != "" && machine.Group != c.Resources.Group || len(allowed) > 0 && !allowed[machine.ID] {
			continue
		}
		key := machineResource(machine)
		if !ready[machine.ID] {
			excluded[machine.ID] = "not_ready_or_occupied"
			delete(stable, key)
			continue
		}
		devices, at, err := resourceSample(machine, states[machine.ID], c.Resources, now)
		if err != nil {
			excluded[machine.ID] = err.Error()
			delete(stable, key)
			continue
		}
		previous := stable[key]
		maxGap := time.Duration(min(config.Interval*2, c.Resources.MaxSampleAge)) * time.Second
		if c.Policy.StableFor > 0 && now.Sub(at) > maxGap {
			excluded[machine.ID] = "sample_gap_during_continuous_idle"
			delete(stable, key)
			continue
		}
		if previous.Last.IsZero() || at.Before(previous.Last) || at.Sub(previous.Last) > maxGap {
			previous.Since = now
		}
		previous.Last = at
		stable[key] = previous
		if c.Policy.StableFor > 0 && now.Sub(previous.Since) < time.Duration(c.Policy.StableFor)*time.Second {
			excluded[machine.ID] = "waiting_for_continuous_idle"
			continue
		}
		if seen[key] || seenHosts[strings.ToLower(machine.Host)] {
			excluded[machine.ID] = "physical_resource_alias"
			continue
		}
		seen[key] = true
		seenHosts[strings.ToLower(machine.Host)] = true
		ids := make([]string, len(devices))
		for i, device := range devices {
			ids[i] = device.ID
		}
		username := ""
		for _, profile := range config.Profiles {
			if profile.ID == machine.ProfileID {
				username = profile.Username
			}
		}
		out = append(out, ContinuationAllocation{MachineID: machine.ID, ProfileID: machine.ProfileID, Username: username, ResourceKey: key, Host: machine.Host, Port: machine.Port, DeviceIDs: ids})
	}
	return out, excluded
}

func (m *TaskManager) offerContinuation(c *Continuation, candidates []ContinuationAllocation, now time.Time) {
	retainContinuationWake(c)
	c.State, c.Reason, c.Generation, c.GrantID = "offered", "", c.Generation+1, "grant-"+randomToken()[:24]
	until := now.Add(time.Duration(c.Policy.OfferTTL) * time.Second)
	c.OfferUntil = &until
	c.Allocations = append([]ContinuationAllocation(nil), candidates[:c.Resources.MachineCount]...)
	for i := range c.Allocations {
		c.Allocations[i].TaskID = c.Tasks[i].ID
	}
	c.Wake = &ContinuationWake{ID: "wake-" + randomToken()[:24], Kind: "resources_ready", Status: "pending"}
}

func (m *TaskManager) EvaluateContinuation(r ContinuationControl) (Continuation, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.continuations[r.ID]
	if !ok {
		return Continuation{}, 404, errors.New("CONTINUATION_NOT_FOUND")
	}
	if c.Policy.WakeMode != "heartbeat" {
		return Continuation{}, 409, errors.New("HEARTBEAT_MODE_REQUIRED")
	}
	now := m.now()
	if c.State == "offered" && c.OfferUntil != nil && now.Before(*c.OfferUntil) {
		return cloneContinuation(c), 200, nil
	}
	if c.State != "waiting_resources" {
		return cloneContinuation(c), 200, nil
	}
	if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		return Continuation{}, 410, errors.New("CONTINUATION_AUTHORIZATION_EXPIRED")
	}
	config, states := m.store.Snapshot(), m.monitor.Snapshot()
	now = m.now()
	if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		return Continuation{}, 410, errors.New("CONTINUATION_AUTHORIZATION_EXPIRED")
	}
	candidates, excluded := m.continuationCandidatesLocked(c, config, states, now)
	c.LastCheckAt, c.NextCheckAt, c.EligibleCount, c.Excluded = &now, nextContinuationCheck(now, c.Policy), len(candidates), excluded
	if len(candidates) >= c.Resources.MachineCount {
		m.offerContinuation(&c, candidates, now)
	}
	c.UpdatedAt = now
	next := m.continuationSnapshotLocked()
	next[c.ID] = c
	if err := m.saveContinuationsLocked(next); err != nil {
		return Continuation{}, 500, err
	}
	return cloneContinuation(c), 200, nil
}

type ContinuationDelivery struct {
	ID          string `json:"id"`
	WakeEventID string `json:"wakeEventId"`
	Generation  int    `json:"generation"`
	SenderID    string `json:"senderId"`
	Action      string `json:"action"`
	TurnID      string `json:"turnId,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func (m *TaskManager) DeliveryContinuation(r ContinuationDelivery) (Continuation, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.continuations[r.ID]
	if !ok {
		return Continuation{}, 404, errors.New("CONTINUATION_NOT_FOUND")
	}
	c = cloneContinuation(c)
	if c.Wake == nil || c.Wake.ID != r.WakeEventID || c.Generation != r.Generation || !identifier.MatchString(r.SenderID) || len(r.Reason) > 1024 {
		return Continuation{}, 409, errors.New("CONTINUATION_DELIVERY_FENCED")
	}
	now := m.now()
	w := c.Wake
	switch r.Action {
	case "begin":
		if c.Policy.WakeMode != "bridge" || c.StopRequested != "" || (c.State != "offered" && c.State != "awaiting_review") || (c.State == "offered" && (c.OfferUntil == nil || !now.Before(*c.OfferUntil))) || w.Status != "pending" || (w.RetryAt != nil && now.Before(*w.RetryAt)) {
			return Continuation{}, 409, errors.New("CONTINUATION_EVENT_NOT_SENDABLE")
		}
		w.Status, w.SenderID, w.SendingAt, w.Attempts = "sending", r.SenderID, &now, w.Attempts+1
	case "delivered":
		if w.Status != "sending" && w.Status != "delivery_uncertain" && w.Status != "delivered" {
			return Continuation{}, 409, errors.New("CONTINUATION_DELIVERY_STATE")
		}
		if r.TurnID == "" || len(r.TurnID) > 160 {
			return Continuation{}, 400, errors.New("TURN_ID_REQUIRED")
		}
		w.Status, w.TurnID, w.DeliveredAt, w.Reason = "delivered", r.TurnID, &now, ""
	case "uncertain":
		if w.SenderID != r.SenderID || (w.Status != "sending" && w.Status != "delivery_uncertain") {
			return Continuation{}, 409, errors.New("CONTINUATION_DELIVERY_OWNER")
		}
		w.Status, w.Reason = "delivery_uncertain", r.Reason
	case "not-sent":
		// Only a definitive failed RPC response BEFORE turn acceptance can use
		// this action. A timeout or lost connection must use uncertain instead.
		if w.SenderID != r.SenderID || w.Status != "sending" {
			return Continuation{}, 409, errors.New("CONTINUATION_DELIVERY_OWNER")
		}
		if w.Attempts >= 3 {
			w.Status, w.Reason = "failed", r.Reason
		} else {
			retry := now.Add(time.Duration(w.Attempts) * 60 * time.Second)
			w.Status, w.Reason, w.RetryAt = "pending", r.Reason, &retry
		}
	case "defer":
		if w.Status != "pending" {
			return Continuation{}, 409, errors.New("CONTINUATION_DELIVERY_STATE")
		}
		retry := now.Add(30 * time.Second)
		w.RetryAt, w.Reason = &retry, r.Reason
	default:
		return Continuation{}, 400, errors.New("INVALID_DELIVERY_ACTION")
	}
	c.UpdatedAt = now
	next := m.continuationSnapshotLocked()
	next[c.ID] = c
	if err := m.saveContinuationsLocked(next); err != nil {
		return Continuation{}, 500, err
	}
	return cloneContinuation(c), 200, nil
}
