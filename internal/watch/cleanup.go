package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const cleanupSchema = "simplehtmlwatch.cleanup.v1"
const cleanupLimit int64 = 20 * 1024 * 1024
const cleanupMinimum = 5 * time.Minute

type CleanupPolicy struct {
	Enabled          bool      `json:"enabled"`
	AuthorizationRef string    `json:"authorizationRef"`
	UpdatedAt        time.Time `json:"updatedAt"`
}
type CleanupLog struct {
	FD      string `json:"fd"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtimeNS"`
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	Hash    string `json:"hash"`
	Tail    string `json:"tail"`
}
type CleanupSnapshot struct {
	Schema            string       `json:"schema"`
	BootID            string       `json:"bootId"`
	PID               int          `json:"pid"`
	StartTicks        uint64       `json:"startTicks"`
	ParentPID         int          `json:"parentPid"`
	ParentStartTicks  uint64       `json:"parentStartTicks"`
	CommandHash       string       `json:"commandHash"`
	ParentCommandHash string       `json:"parentCommandHash"`
	Command           string       `json:"command"`
	Cwd               string       `json:"cwd"`
	State             string       `json:"state"`
	Uptime            float64      `json:"uptime"`
	AgeSeconds        float64      `json:"ageSeconds"`
	CPUSeconds        float64      `json:"cpuSeconds"`
	OrphanForkserver  bool         `json:"orphanForkserver"`
	Debug             bool         `json:"debug"`
	Interactive       bool         `json:"interactive"`
	Children          bool         `json:"children"`
	Siblings          bool         `json:"siblings"`
	Failure           bool         `json:"failure"`
	Missing           bool         `json:"missing"`
	NPU               string       `json:"npu"`
	Logs              []CleanupLog `json:"logs"`
}
type CleanupEvent struct {
	At     time.Time `json:"at"`
	Phase  string    `json:"phase"`
	Reason string    `json:"reason"`
}
type CleanupAudit struct {
	Schema           string          `json:"schema"`
	ID               string          `json:"id"`
	MachineID        string          `json:"machineId"`
	Host             string          `json:"host"`
	Port             int             `json:"port"`
	ProfileID        string          `json:"profileId"`
	Username         string          `json:"username"`
	ResourceKey      string          `json:"resourceKey"`
	ContinuationID   string          `json:"continuationId"`
	AuthorizationRef string          `json:"authorizationRef"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	Since            time.Time       `json:"since"`
	Phase            string          `json:"phase"`
	Reason           string          `json:"reason"`
	Baseline         CleanupSnapshot `json:"baseline"`
	Last             CleanupSnapshot `json:"last"`
	SignalIntentAt   *time.Time      `json:"signalIntentAt,omitempty"`
	SignalResult     string          `json:"signalResult,omitempty"`
	Events           []CleanupEvent  `json:"events"`
}
type cleanupState struct {
	Policy CleanupPolicy
	Audits map[string]CleanupAudit
	Busy   map[string]bool
	Next   map[string]time.Time
	Error  string
	Remote cleanupRemote
}
type cleanupRequest struct {
	Mode         string               `json:"mode"`
	ID           string               `json:"id"`
	PID          int                  `json:"pid"`
	Baseline     *CleanupSnapshot     `json:"baseline,omitempty"`
	Last         *CleanupSnapshot     `json:"last,omitempty"`
	Requirements ResourceRequirements `json:"requirements"`
}
type cleanupRemote interface {
	Cleanup(context.Context, Machine, Profile, cleanupRequest) (CleanupSnapshot, string, error)
}

func (m *TaskManager) cleanupDir() string { return filepath.Join(m.dir, ".cleanup") }
func (m *TaskManager) loadCleanup() error {
	m.cleanup = cleanupState{Audits: map[string]CleanupAudit{}, Busy: map[string]bool{}, Next: map[string]time.Time{}}
	if remote, ok := m.remote.(cleanupRemote); ok {
		m.cleanup.Remote = remote
	}
	dir := m.cleanupDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Size() > 256*1024 || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("CLEANUP_AUDIT_INVALID")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if e.Name() == "policy.json" {
			if err = json.Unmarshal(b, &m.cleanup.Policy); err != nil {
				return err
			}
			continue
		}
		var a CleanupAudit
		if err = json.Unmarshal(b, &a); err != nil || a.Schema != cleanupSchema || a.ID+".json" != e.Name() || !identifier.MatchString(a.ID) {
			return errors.New("CLEANUP_AUDIT_INVALID")
		}
		// Recovery observes old identities only. It never replays signal intentions.
		if a.SignalIntentAt != nil && a.Phase != "verified" {
			a.Phase = "uncertain"
		} else if a.SignalIntentAt == nil {
			a.Phase = "unknown"
			a.Reason = "服务重启后重新观测，不沿用旧稳定计时"
		}
		m.cleanup.Audits[a.ID] = a
	}
	return nil
}
func durableCleanup(path string, b []byte) error {
	if err := writeAtomic(path, b); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (m *TaskManager) saveCleanup(a CleanupAudit) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	var size int64
	entries, err := os.ReadDir(m.cleanupDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == a.ID+".json" {
			continue
		}
		i, err := e.Info()
		if err != nil {
			return err
		}
		size += i.Size()
	}
	if len(b) > 256*1024 || size+int64(len(b)) > cleanupLimit {
		return errors.New("CLEANUP_AUDIT_STORAGE_FULL; cleanup stopped; archive logs before resuming")
	}
	return durableCleanup(filepath.Join(m.cleanupDir(), a.ID+".json"), b)
}
func (m *TaskManager) SetCleanupPolicy(p CleanupPolicy) error {
	if p.Enabled && (strings.TrimSpace(p.AuthorizationRef) == "" || len(p.AuthorizationRef) > 1024) {
		return errors.New("CLEANUP_AUTHORIZATION_REQUIRED")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p.UpdatedAt = m.now()
	b, _ := json.Marshal(p)
	if err := durableCleanup(filepath.Join(m.cleanupDir(), "policy.json"), b); err != nil {
		return err
	}
	m.cleanup.Policy = p
	return nil
}
func (m *TaskManager) CleanupStatus(id string) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != "" {
		a, ok := m.cleanup.Audits[id]
		if !ok {
			return nil, errors.New("CLEANUP_AUDIT_NOT_FOUND")
		}
		a.Events = append([]CleanupEvent(nil), a.Events...)
		return map[string]any{"audit": a}, nil
	}
	audits := make([]CleanupAudit, 0, len(m.cleanup.Audits))
	for _, a := range m.cleanup.Audits {
		audits = append(audits, a)
	}
	sort.Slice(audits, func(i, j int) bool { return audits[i].UpdatedAt.After(audits[j].UpdatedAt) })
	// Summary omits full remote outputs; individual audit reads retain evidence.
	summaries := []map[string]any{}
	for i, a := range audits {
		if i == 200 {
			break
		}
		summaries = append(summaries, map[string]any{"id": a.ID, "machineId": a.MachineID, "continuationId": a.ContinuationID, "pid": a.Last.PID, "phase": a.Phase, "reason": a.Reason, "updatedAt": a.UpdatedAt})
	}
	return map[string]any{"schema": cleanupSchema, "policy": m.cleanup.Policy, "minimumMachines": 4, "minimumSeconds": 300, "probeIntervalSeconds": 60, "auditLimitBytes": cleanupLimit, "auditCount": len(audits), "audits": summaries, "error": m.cleanup.Error}, nil
}
func auditIdentity(key string, s CleanupSnapshot) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d/%d", key, s.BootID, s.PID, s.StartTicks)))
	return "residue-" + hex.EncodeToString(h[:16])
}
func sameCleanupIdentity(a, b CleanupSnapshot) bool {
	return a.BootID == b.BootID && a.PID == b.PID && a.StartTicks == b.StartTicks && a.ParentPID == b.ParentPID && a.ParentStartTicks == b.ParentStartTicks && a.CommandHash == b.CommandHash && a.ParentCommandHash == b.ParentCommandHash && a.Cwd == b.Cwd
}
func cleanupClassification(s CleanupSnapshot) (string, string) {
	if s.Debug || s.Interactive || s.State == "T" || s.State == "t" {
		return "debug", "调试或交互进程，继续等待"
	}
	if s.Schema != "simplehtmlwatch.cleanup.remote.v1" || s.BootID == "" || s.Missing || s.PID <= 1 || s.StartTicks == 0 || s.ParentStartTicks == 0 || !hexDigest(s.CommandHash) || !hexDigest(s.ParentCommandHash) || s.State != "S" || !s.OrphanForkserver || !s.Failure || s.AgeSeconds <= 300 || len(s.Logs) == 0 || s.Children || s.Siblings {
		return "unknown", "缺少明确失败、原监督进程退出或完整进程证据，继续等待"
	}
	for _, l := range s.Logs {
		if l.Size < 0 || l.MtimeNS <= 0 || !hexDigest(l.Hash) {
			return "unknown", "日志证据不完整，继续等待"
		}
	}
	return "observing", "已发现失败残留证据，观察至少五分钟无进展"
}

// Parse a single PID on a single device, then reuse the existing complete resource gate.
func cleanupCandidate(state State, req ResourceRequirements, now time.Time) (int, error) {
	var sample *Result
	for i := range state.Results {
		if state.Results[i].CommandID == "npu" {
			if sample != nil {
				return 0, errors.New("duplicate_sample")
			}
			sample = &state.Results[i]
		}
	}
	if sample == nil || sample.ExitCode == nil || *sample.ExitCode != 0 || sample.Error != "" || sample.Warning != "" || sample.Truncated || sample.EndedAt.IsZero() || sample.EndedAt.After(now) || now.Sub(sample.EndedAt) > time.Duration(req.MaxSampleAge)*time.Second {
		return 0, errors.New("sample_unknown_or_stale")
	}
	lines := strings.Split(sample.Stdout, "\n")
	header := -1
	pi := -1
	columns := 0
	var pid int
	count := 0
	for i, l := range lines {
		if strings.Contains(l, "Process id") {
			if header >= 0 {
				return 0, errors.New("duplicate_process_table")
			}
			header = i
			columns = len(tableCells(l))
			for j, c := range tableCells(l) {
				if c == "Process id" {
					pi = j
				}
			}
			continue
		}
		if header < 0 {
			continue
		}
		cells := tableCells(l)
		if len(cells) == 0 || strings.Contains(l, "No running process") {
			continue
		}
		if pi < 0 || pi >= len(cells) || len(cells) != columns {
			return 0, errors.New("process_table_unknown")
		}
		id, err := strconv.Atoi(cells[pi])
		if err != nil || id <= 1 {
			return 0, errors.New("process_table_unknown")
		}
		ids := strings.Fields(cells[0])
		if len(ids) != 1 {
			return 0, errors.New("process_device_unknown")
		}
		if _, err = strconv.Atoi(ids[0]); err != nil {
			return 0, err
		}
		pid = id
		count++
	}
	if header < 0 || count != 1 {
		return 0, errors.New("not_single_process")
	}
	sanitized := strings.Join(lines[:header+1], "\n") + "\n| No running processes found |\n"
	clone := state
	clone.Results = []Result{*sample}
	clone.Results[0].Stdout = sanitized
	zero := req
	zero.MaxUtilization = 0
	if _, _, err := resourceSample(Machine{Labels: req.Labels}, clone, zero, now); err != nil {
		return 0, err
	}
	return pid, nil
}

type cleanupTarget struct {
	Machine      Machine
	Profile      Profile
	Continuation Continuation
	PID          int
	Audit        *CleanupAudit
}

func (m *TaskManager) cleanupTargetsLocked(config Config, states map[string]State, now time.Time) []cleanupTarget {
	if m.cleanup.Remote == nil {
		return nil
	}
	ready := map[string]bool{}
	for _, r := range m.readyLocked(config, states, "", "") {
		ready[r.ID] = true
	}
	profiles := map[string]Profile{}
	for _, p := range config.Profiles {
		profiles[p.ID] = p
	}
	cs := m.continuationsLockedForCleanup()
	out := []cleanupTarget{}
	seen := map[string]bool{}
	// Finish read-only verification even after the waiter is cancelled or cleanup disabled.
	for _, a := range m.cleanup.Audits {
		if a.SignalIntentAt == nil || a.Phase == "verified" || now.Before(m.cleanup.Next[a.ResourceKey]) || m.cleanup.Busy[a.ResourceKey] {
			continue
		}
		for _, h := range config.Machines {
			if h.ID == a.MachineID && machineResource(h) == a.ResourceKey && h.Host == a.Host && h.Port == a.Port && h.ProfileID == a.ProfileID {
				if p, ok := profiles[h.ProfileID]; ok && p.Username == a.Username {
					copy := a
					out = append(out, cleanupTarget{Machine: h, Profile: p, PID: a.Last.PID, Audit: &copy})
					seen[a.ResourceKey] = true
				}
			}
		}
	}
	if !m.cleanup.Policy.Enabled {
		return out
	}
	for _, c := range cs {
		if c.State != "waiting_resources" || c.Resources.MachineCount < 4 || !c.Authorized || c.StopRequested != "" || c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
			continue
		}
		priorOK := true
		for _, id := range c.ExistingTaskIDs {
			if j := m.jobs[id]; j == nil || j.FinishedAt == nil {
				priorOK = false
			}
		}
		if !priorOK {
			continue
		}
		// A sufficient idle pool needs no preemption, even while stableFor is counting.
		idle := 0
		for _, h := range config.Machines {
			if ready[h.ID] && continuationScope(h, c.Resources) {
				if _, _, err := resourceSample(h, states[h.ID], c.Resources, now); err == nil {
					idle++
				}
			}
		}
		if idle >= c.Resources.MachineCount {
			continue
		}
		for _, h := range config.Machines {
			key := machineResource(h)
			if !ready[h.ID] || !continuationScope(h, c.Resources) || seen[key] || now.Before(m.cleanup.Next[key]) {
				continue
			}
			pid, err := cleanupCandidate(states[h.ID], c.Resources, now)
			if err != nil {
				continue
			}
			p, ok := profiles[h.ProfileID]
			if !ok {
				continue
			}
			out = append(out, cleanupTarget{Machine: h, Profile: p, Continuation: c, PID: pid})
			seen[key] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine.ID < out[j].Machine.ID })
	return out
}
func (m *TaskManager) continuationsLockedForCleanup() []Continuation {
	cs := []Continuation{}
	for _, c := range m.continuations {
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].CreatedAt.Before(cs[j].CreatedAt) })
	return cs
}
func (m *TaskManager) CheckCleanup() error {
	config, states := m.store.Snapshot(), m.monitor.Snapshot()
	m.mu.Lock()
	targets := m.cleanupTargetsLocked(config, states, m.now())
	if len(targets) == 0 {
		m.mu.Unlock()
		return nil
	}
	target := targets[0]
	key := machineResource(target.Machine)
	m.cleanup.Busy[key] = true
	m.cleanup.Next[key] = m.now().Add(time.Minute)
	remote := m.cleanup.Remote
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.cleanup.Busy, key); m.mu.Unlock() }()
	// A provisional inspect ID cannot signal; the durable ID is derived from boot+start identity.
	provisional := auditIdentity(key, CleanupSnapshot{PID: target.PID})
	if target.Audit != nil {
		provisional = target.Audit.ID
	}
	snapshot, _, err := remote.Cleanup(m.ctx, target.Machine, target.Profile, cleanupRequest{Mode: "inspect", ID: provisional, PID: target.PID, Requirements: target.Continuation.Resources})
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if err != nil {
		m.cleanup.Error = err.Error()
		id := provisional
		a, ok := m.cleanup.Audits[id]
		if !ok {
			a = CleanupAudit{Schema: cleanupSchema, ID: id, MachineID: target.Machine.ID, Host: target.Machine.Host, Port: target.Machine.Port, ProfileID: target.Profile.ID, Username: target.Profile.Username, ResourceKey: key, ContinuationID: target.Continuation.ID, CreatedAt: now, Last: CleanupSnapshot{PID: target.PID}, AuthorizationRef: m.cleanup.Policy.AuthorizationRef}
		}
		a.UpdatedAt = now
		a.Phase = "unknown"
		if a.SignalIntentAt != nil {
			a.Phase = "uncertain"
		}
		a.Reason = "只读诊断失败：" + err.Error()
		a.Events = append(a.Events, CleanupEvent{now, a.Phase, a.Reason})
		if len(a.Events) > 64 {
			a.Events = a.Events[len(a.Events)-64:]
		}
		if saveErr := m.saveCleanup(a); saveErr != nil {
			return saveErr
		}
		m.cleanup.Audits[id] = a
		return err
	}
	id := auditIdentity(key, snapshot)
	if target.Audit != nil {
		id = target.Audit.ID
	}
	a, exists := m.cleanup.Audits[id]
	if !exists {
		a = CleanupAudit{Schema: cleanupSchema, ID: id, MachineID: target.Machine.ID, Host: target.Machine.Host, Port: target.Machine.Port, ProfileID: target.Profile.ID, Username: target.Profile.Username, ResourceKey: key, ContinuationID: target.Continuation.ID, AuthorizationRef: m.cleanup.Policy.AuthorizationRef, CreatedAt: now, Since: now, Baseline: snapshot}
	}
	phase, reason := cleanupClassification(snapshot)
	before := a.Last
	previousAt := a.UpdatedAt
	a.Last = snapshot
	a.UpdatedAt = now
	// Confirm fresh NPU facts from this SSH inspection rather than a historical machine mean.
	code := 0
	fresh := State{Status: "online", Results: []Result{{CommandID: "npu", Stdout: snapshot.NPU, ExitCode: &code, EndedAt: now}}}
	if p, e := cleanupCandidate(fresh, target.Continuation.Resources, now); e != nil || p != target.PID {
		phase, reason = "unknown", "当前设备占用或资源条件已变化，继续等待"
	}
	if a.SignalIntentAt != nil {
		phase, reason = "uncertain", "已有终止意图，只核查原进程，不重放信号"
		if snapshot.Missing || snapshot.State == "Z" || !sameCleanupIdentity(a.Baseline, snapshot) {
			phase, reason = "verified", "原进程退出或身份变化已确认；资源仍由原等待合同重新检查"
		}
	} else if phase == "observing" {
		if exists && (!sameCleanupIdentity(a.Baseline, snapshot) || now.Sub(previousAt) > 150*time.Second || before.Uptime > snapshot.Uptime || snapshot.Uptime-before.Uptime > 150 || !reflect.DeepEqual(a.Baseline.Logs, snapshot.Logs) || snapshot.CPUSeconds < a.Baseline.CPUSeconds || snapshot.CPUSeconds-a.Baseline.CPUSeconds >= 1 || a.Phase != "observing") {
			a.Baseline = snapshot
			a.Since = now
		}
		if snapshot.Uptime-a.Baseline.Uptime >= 300 && now.Sub(a.Since) >= cleanupMinimum {
			phase, reason = "confirmed", "失败残留已确认：原监督进程退出，五分钟无 CPU/日志进展，无调试迹象"
		}
	} else {
		a.Baseline = snapshot
		a.Since = now
	}
	// Serialize the decision with pause/cancel, submission and reservations.
	c, ok := m.continuations[target.Continuation.ID]
	scopeValid := ok && c.State == "waiting_resources" && c.StopRequested == "" && c.Resources.MachineCount >= 4 && c.Authorized && (c.ExpiresAt == nil || now.Before(*c.ExpiresAt)) && m.cleanup.Policy.Enabled
	currentConfig := m.store.Snapshot()
	machineValid := false
	for _, h := range currentConfig.Machines {
		if h.ID == target.Machine.ID && reflect.DeepEqual(h, target.Machine) {
			for _, p := range currentConfig.Profiles {
				if p.ID == h.ProfileID && reflect.DeepEqual(p, target.Profile) {
					machineValid = true
				}
			}
		}
	}
	if a.SignalIntentAt == nil && (!scopeValid || !machineValid) {
		phase, reason = "unknown", "等待单或机器配置已变化，继续等待"
	}
	if phase == "confirmed" {
		idle := 0
		statesNow := m.monitor.Snapshot()
		for _, h := range m.readyLocked(currentConfig, statesNow, "", "") {
			for _, candidate := range currentConfig.Machines {
				if candidate.ID == h.ID && continuationScope(candidate, c.Resources) {
					if _, _, e := resourceSample(candidate, statesNow[candidate.ID], c.Resources, now); e == nil {
						idle++
					}
				}
			}
		}
		if idle >= c.Resources.MachineCount {
			phase, reason = "unknown", "已有足够空闲机器，继续原资源分配，不清理其他进程"
		}
	}
	a.Phase, a.Reason = phase, reason
	if len(a.Events) == 0 || a.Events[len(a.Events)-1].Phase != phase {
		a.Events = append(a.Events, CleanupEvent{now, phase, reason})
	}
	if len(a.Events) > 64 {
		a.Events = append(a.Events[:1], a.Events[len(a.Events)-63:]...)
	}
	if err = m.saveCleanup(a); err != nil {
		m.cleanup.Error = err.Error()
		return err
	}
	m.cleanup.Audits[id] = a
	m.cleanup.Error = ""
	if phase != "confirmed" {
		return nil
	}
	// A durable intent must precede the first and only signal request, including crashes/lost replies.
	a.SignalIntentAt = &now
	a.Phase = "signal_intent"
	a.Events = append(a.Events, CleanupEvent{now, "signal_intent", "发送定点 SIGTERM；不使用 SIGKILL"})
	if err = m.saveCleanup(a); err != nil {
		m.cleanup.Error = err.Error()
		return err
	}
	m.cleanup.Audits[id] = a
	_, result, signalErr := remote.Cleanup(m.ctx, target.Machine, target.Profile, cleanupRequest{Mode: "terminate", ID: id, PID: target.PID, Baseline: &a.Baseline, Last: &a.Last, Requirements: c.Resources})
	a.UpdatedAt = m.now()
	a.SignalResult = result
	a.Phase = "terminated"
	a.Reason = "终止请求完成，等待重新核查 NPU 进程表"
	if signalErr != nil {
		a.Phase = "uncertain"
		a.Reason = "终止结果未知，只核查原身份，不重放信号：" + signalErr.Error()
	}
	a.Events = append(a.Events, CleanupEvent{a.UpdatedAt, a.Phase, a.Reason})
	if err = m.saveCleanup(a); err != nil {
		m.cleanup.Error = err.Error()
		return err
	}
	m.cleanup.Audits[id] = a
	return nil
}
