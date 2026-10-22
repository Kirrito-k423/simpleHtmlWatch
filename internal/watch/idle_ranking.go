package watch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const idleRankingCapability = "idle-machine-ranking-v1"
const idleRankingExportLimit = 128 * 1024

type IdleRequirements struct {
	Group             string  `json:"group,omitempty"`
	Model             string  `json:"model,omitempty"`
	DevicesPerMachine int     `json:"devicesPerMachine"`
	MinFreeHBMMB      int64   `json:"minFreeHbmMB"`
	MaxUtilization    float64 `json:"maxUtilization"`
	MaxSampleAge      int     `json:"maxSampleAgeSeconds"`
}

type IdleMachine struct {
	Rank              int                        `json:"rank"`
	MachineID         string                     `json:"machineId"`
	ResourceKey       string                     `json:"resourceKey"`
	Name              string                     `json:"name"`
	Group             string                     `json:"group"`
	Model             string                     `json:"model"`
	Tier              int                        `json:"tier"`
	TierLabel         string                     `json:"tierLabel"`
	HistoryReason     string                     `json:"historyReason"`
	Windows           map[string]OccupancyWindow `json:"windows"`
	Eligible          bool                       `json:"eligible"`
	Recommended       bool                       `json:"recommended"`
	CurrentReason     string                     `json:"currentReason"`
	ResourceCheckedAt time.Time                  `json:"resourceCheckedAt"`
	LastNpuSampleAt   *time.Time                 `json:"lastNpuSampleAt"`
	Devices           int                        `json:"devices"`
	MinFreeHBMMB      *int64                     `json:"minFreeHbmMB"`
	RequiresLiveCheck bool                       `json:"requiresLiveCheck"`
}

type IdleRankingReport struct {
	Schema                 string           `json:"schema"`
	PolicyVersion          string           `json:"rankingPolicyVersion"`
	Instance               string           `json:"instance"`
	ServiceID              string           `json:"serviceId"`
	RunID                  string           `json:"runId"`
	GeneratedAt            time.Time        `json:"generatedAt"`
	HistoryThrough         time.Time        `json:"historyThrough"`
	NextRefreshAt          time.Time        `json:"nextRefreshAt"`
	ExportedAt             *time.Time       `json:"exportedAt"`
	ExportPath             string           `json:"exportPath"`
	ExportError            string           `json:"exportError,omitempty"`
	HistoryStorageError    string           `json:"historyStorageError,omitempty"`
	HistoryBytes           int64            `json:"historyBytes"`
	HistoryLimitBytes      int64            `json:"historyLimitBytes"`
	ExportLimitBytes       int              `json:"exportLimitBytes"`
	MinimumCoveragePercent int              `json:"minimumCoveragePercent"`
	BucketSeconds          int              `json:"bucketSeconds"`
	RetentionHours         int              `json:"retentionHours"`
	RequiresLiveCheck      bool             `json:"requiresLiveCheck"`
	Requirements           IdleRequirements `json:"requirements"`
	Machines               []IdleMachine    `json:"machines"`
}

type IdleRanking struct {
	scheduleMu  sync.Mutex
	nextExport  time.Time
	store       *Store
	monitor     *Monitor
	tasks       *TaskManager
	service     ServiceInfo
	now         func() time.Time
	mu          sync.Mutex
	exportedAt  *time.Time
	exportError string
	stop        chan struct{}
	wg          sync.WaitGroup
}

func newIdleRanking(store *Store, monitor *Monitor, tasks *TaskManager, service ServiceInfo) (*IdleRanking, error) {
	path := filepath.Join(store.dir, "exports")
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	// Only our interrupted atomic export files, never user artifacts.
	files, err := filepath.Glob(filepath.Join(path, ".watch-*"))
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			return nil, err
		}
	}
	return &IdleRanking{store: store, monitor: monitor, tasks: tasks, service: service, now: time.Now}, nil
}

func (r *IdleRanking) exportPath() string {
	return filepath.Join(r.store.dir, "exports", "idle-ranking.json")
}
func nextIdleRefresh(now time.Time) time.Time {
	return now.Truncate(30 * time.Minute).Add(30 * time.Minute)
}

func defaultIdleRequirements(c Config) IdleRequirements {
	return IdleRequirements{MinFreeHBMMB: 1024, MaxUtilization: 0, MaxSampleAge: min(120, c.Interval*2+30)}
}

func idleTier(windows map[string]OccupancyWindow) (int, string) {
	for i, key := range []string{"3h", "2h", "1h"} {
		w := windows[key]
		if w.Complete && w.OccupiedSeconds == 0 {
			return i + 1, "observed_zero_" + key
		}
	}
	w := windows["1h"]
	if !w.Complete {
		return 0, "history_insufficient"
	}
	if *w.OccupiedPercent < 15 {
		return 4, "recent_1h_under_15"
	}
	if *w.OccupiedPercent < 25 {
		return 5, "recent_1h_under_25"
	}
	return 0, "recent_1h_at_least_25"
}

var idleTierLabels = []string{"未分档", "≥3小时观测零占用", "2小时观测零占用", "1小时观测零占用", "最近1小时非零且低于15%", "最近1小时15%至25%"}

// Only complete five-minute buckets are used. No hourly backfill or proportional
// split can invent what happened in the second half of a calendar hour.
func (u *Utilization) idleHistory(config Config, states map[string]State, through time.Time) (map[string]IdleMachine, int64, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]IdleMachine{}
	for _, machine := range config.Machines {
		var selected *occupancySeries
		var newest int64
		currentModel := ""
		for _, result := range states[machine.ID].Results {
			if result.CommandID == "npu" {
				if reading, err := parseNPUUtilization(result.Stdout); err == nil {
					currentModel = reading.model
				}
			}
		}
		for _, series := range u.recent {
			meta := series.Machine
			if meta.ID != machine.ID || series.ResourceKey != machineResource(machine) || meta.Address != fmt.Sprintf("%s:%d", machine.Host, machine.Port) || len(series.Buckets) == 0 || currentModel != "" && currentModel != meta.Model {
				continue
			}
			at := series.Buckets[len(series.Buckets)-1][0]
			if at > newest {
				selected, newest = series, at
			}
		}
		row := IdleMachine{Windows: map[string]OccupancyWindow{}}
		if selected != nil {
			row.Model = selected.Machine.Model
		}
		if currentModel != "" {
			row.Model = currentModel
		}
		for _, entry := range []struct {
			key      string
			duration time.Duration
		}{{"5m", 5 * time.Minute}, {"1h", time.Hour}, {"2h", 2 * time.Hour}, {"3h", 3 * time.Hour}, {"4h", 4 * time.Hour}} {
			row.Windows[entry.key] = occupancyWindow(selected, through, entry.duration)
		}
		row.Tier, row.HistoryReason = idleTier(row.Windows)
		if u.recentError != "" {
			row.Tier, row.HistoryReason = 0, "history_storage_error"
		}
		row.TierLabel = idleTierLabels[row.Tier]
		out[machine.ID] = row
	}
	return out, u.recentBytes, u.recentError
}

func checkIdleResources(machine Machine, state State, inventory TaskMachine, req IdleRequirements, now time.Time, row *IdleMachine) string {
	if !machine.Enabled {
		return "machine_disabled"
	}
	hasNPU := false
	for _, command := range machine.Commands {
		if command == "npu" {
			hasNPU = true
		}
	}
	if !hasNPU {
		return "npu_collection_disabled"
	}
	if !inventory.Ready {
		return inventory.Reason
	}
	if state.SampleIdentity != monitorSampleIdentity(machine) {
		return "npu_sample_identity_changed"
	}
	if state.KeyChanged {
		return "ssh_key_changed"
	}
	var sample *Result
	for i := range state.Results {
		if state.Results[i].CommandID != "npu" {
			continue
		}
		if sample != nil {
			return "npu_sample_duplicate"
		}
		sample = &state.Results[i]
	}
	if sample == nil {
		return "npu_sample_unknown"
	}
	if !sample.EndedAt.IsZero() {
		at := sample.EndedAt
		row.LastNpuSampleAt = &at
	}
	if sample.ExitCode == nil || *sample.ExitCode != 0 || sample.Error != "" || sample.Warning != "" || sample.Truncated {
		return "npu_sample_unknown"
	}
	if sample.EndedAt.IsZero() || sample.EndedAt.After(now) || now.Sub(sample.EndedAt) > time.Duration(req.MaxSampleAge)*time.Second {
		return "npu_sample_stale"
	}
	devices, err := parseNPUResources(sample.Stdout)
	if err != nil {
		return err.Error()
	}
	row.Devices = len(devices)
	var minFree int64 = 1<<63 - 1
	for _, device := range devices {
		row.Model = device.Model
		minFree = min(minFree, device.FreeHBMMB)
	}
	row.MinFreeHBMMB = &minFree
	if req.DevicesPerMachine > 0 && len(devices) != req.DevicesPerMachine {
		return "npu_device_count_mismatch"
	}
	for _, device := range devices {
		if req.Model != "" && device.Model != req.Model {
			return "npu_model_mismatch"
		}
		if device.Util > req.MaxUtilization {
			return "npu_utilization_busy"
		}
		if device.FreeHBMMB < req.MinFreeHBMMB {
			return "hbm_insufficient"
		}
	}
	return "ready"
}

func (r *IdleRanking) Report(req IdleRequirements) IdleRankingReport {
	config := r.store.Snapshot()
	states := r.monitor.Snapshot()
	inventory := map[string]TaskMachine{}
	for _, machine := range r.tasks.Machines() {
		inventory[machine.ID] = machine
	}
	now := r.now().In(chinaTime)
	through := now.Truncate(recentOccupancyStep)
	history, used, historyError := r.monitor.utilization.idleHistory(config, states, through)
	r.mu.Lock()
	exportedAt, exportError := r.exportedAt, r.exportError
	r.mu.Unlock()
	report := IdleRankingReport{Schema: "simplehtmlwatch.idle-ranking.v1", PolicyVersion: "five-tier-v1", Instance: r.service.Instance, ServiceID: r.service.ServiceID, RunID: r.service.RunID, GeneratedAt: now, HistoryThrough: through, NextRefreshAt: nextIdleRefresh(now), ExportedAt: exportedAt, ExportPath: r.exportPath(), ExportError: exportError, HistoryStorageError: historyError, HistoryBytes: used, HistoryLimitBytes: recentOccupancyLimit, ExportLimitBytes: idleRankingExportLimit, MinimumCoveragePercent: 90, BucketSeconds: 300, RetentionHours: 6, RequiresLiveCheck: true, Requirements: req, Machines: []IdleMachine{}}
	for _, machine := range config.Machines {
		if req.Group != "" && machine.Group != req.Group {
			continue
		}
		row := history[machine.ID]
		row.MachineID, row.ResourceKey, row.Name, row.Group = machine.ID, machineResource(machine), machine.Name, machine.Group
		row.ResourceCheckedAt, row.RequiresLiveCheck = now, true
		row.CurrentReason = checkIdleResources(machine, states[machine.ID], inventory[machine.ID], req, now, &row)
		row.Eligible = row.CurrentReason == "ready"
		row.Recommended = row.Eligible && row.Tier > 0
		report.Machines = append(report.Machines, row)
	}
	sort.Slice(report.Machines, func(i, j int) bool {
		a, b := report.Machines[i], report.Machines[j]
		if a.Recommended != b.Recommended {
			return a.Recommended
		}
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		at, bt := a.Tier, b.Tier
		if at == 0 {
			at = 6
		}
		if bt == 0 {
			bt = 6
		}
		if at != bt {
			return at < bt
		}
		return a.MachineID < b.MachineID // Stable tie order; no unvalidated score.
	})
	rank := 0
	for i := range report.Machines {
		if report.Machines[i].Recommended {
			rank++
			report.Machines[i].Rank = rank
		}
	}
	return report
}

func (r *IdleRanking) Refresh() error {
	report := r.Report(defaultIdleRequirements(r.store.Snapshot()))
	at := report.GeneratedAt
	report.ExportedAt, report.ExportError = &at, ""
	raw, err := json.Marshal(report)
	if err == nil && len(raw) > idleRankingExportLimit {
		err = fmt.Errorf("空闲排序JSON超过128 KiB上限")
	}
	if err == nil {
		err = writeAtomic(r.exportPath(), append(raw, '\n'))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.exportError = err.Error()
		return err
	}
	r.exportedAt, r.exportError = &at, ""
	return nil
}

func (r *IdleRanking) Start() {
	_ = r.CheckExport()
	r.stop = make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				_ = r.CheckExport()
			}
		}
	}()
}

func (r *IdleRanking) CheckExport() error {
	r.scheduleMu.Lock()
	defer r.scheduleMu.Unlock()
	now := r.now()
	if !r.nextExport.IsZero() && now.Before(r.nextExport) {
		return nil
	}
	err := r.Refresh()
	r.nextExport = nextIdleRefresh(now)
	if err != nil {
		r.nextExport = minTime(r.nextExport, now.Add(time.Minute))
	}
	return err
}

func monitorSampleIdentity(machine Machine) string {
	return machine.ID + "\x00" + machine.Host + ":" + strconv.Itoa(machine.Port) + "\x00" + machineResource(machine)
}

func (r *IdleRanking) Close() {
	if r.stop != nil {
		close(r.stop)
		r.wg.Wait()
	}
}

func (s *Server) EnableIdleRanking() error {
	if s.tasks == nil || s.monitor.utilization == nil || s.service == nil {
		return fmt.Errorf("空闲排序依赖未初始化")
	}
	ranking, err := newIdleRanking(s.store, s.monitor, s.tasks, *s.service)
	if err != nil {
		return err
	}
	s.idleRanking = ranking
	ranking.Start()
	return nil
}

func (s *Server) idleRankingAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.idleRanking == nil {
		fail(w, 503, "空闲排序未初始化")
		return
	}
	req := defaultIdleRequirements(s.store.Snapshot())
	query := r.URL.Query()
	allowed := map[string]bool{"group": true, "model": true, "devicesPerMachine": true, "minFreeHbmMB": true, "maxSampleAgeSeconds": true}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			fail(w, 400, "排序查询参数无效")
			return
		}
	}
	req.Group, req.Model = query.Get("group"), query.Get("model")
	for _, field := range []struct {
		key      string
		min, max int64
		target   *int64
	}{
		{"minFreeHbmMB", 0, 1 << 40, &req.MinFreeHBMMB},
	} {
		if raw, ok := query[field.key]; ok {
			value, err := strconv.ParseInt(raw[0], 10, 64)
			if err != nil || value < field.min || value > field.max {
				fail(w, 400, "排序资源参数无效")
				return
			}
			*field.target = value
		}
	}
	for _, field := range []struct {
		key      string
		min, max int
		target   *int
	}{
		{"devicesPerMachine", 1, 128, &req.DevicesPerMachine}, {"maxSampleAgeSeconds", 1, 300, &req.MaxSampleAge},
	} {
		if raw, ok := query[field.key]; ok {
			value, err := strconv.Atoi(raw[0])
			if err != nil || value < field.min || value > field.max {
				fail(w, 400, "排序资源参数无效")
				return
			}
			*field.target = value
		}
	}
	jsonResponse(w, 200, s.idleRanking.Report(req))
}
