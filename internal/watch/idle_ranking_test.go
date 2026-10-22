package watch

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func seedOccupancy(u *Utilization, m Machine, before time.Time, hourly []int64) {
	meta := utilizationMachine{m.ID, m.Name, m.Group, fmt.Sprintf("%s:%d", m.Host, m.Port), "Ascend950DT", "NPU Util(%)"}
	series := &occupancySeries{Machine: meta, ResourceKey: machineResource(m)}
	start := before.Add(-time.Duration(len(hourly)) * time.Hour)
	for i, pct := range hourly {
		for j := 0; j < 12; j++ {
			series.Buckets = append(series.Buckets, occupancyBucket{start.Add(time.Duration(i)*time.Hour + time.Duration(j)*5*time.Minute).Unix(), 300000, pct * 3000})
		}
	}
	u.recent[recentSeriesKey(meta, series.ResourceKey)] = series
}

func TestIdleFiveTiersAndCoverage(t *testing.T) {
	before := time.Date(2026, 10, 22, 10, 30, 0, 0, chinaTime)
	for _, tc := range []struct {
		name string
		busy []int64
		want int
	}{
		{"three_or_more", []int64{100, 0, 0, 0}, 1}, {"four_is_same_tier", []int64{0, 0, 0, 0}, 1},
		{"two", []int64{100, 100, 0, 0}, 2}, {"one", []int64{100, 100, 100, 0}, 3},
		{"under15", []int64{100, 100, 100, 14}, 4}, {"exact15", []int64{100, 100, 100, 15}, 5},
		{"under25", []int64{100, 100, 100, 24}, 5}, {"exact25", []int64{100, 100, 100, 25}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := newUtilization(t.TempDir(), func() time.Time { return before })
			c := testConfig()
			seedOccupancy(u, c.Machines[0], before, tc.busy)
			history, _, _ := u.idleHistory(c, nil, before)
			if history[c.Machines[0].ID].Tier != tc.want {
				t.Fatalf("got %+v", history)
			}
		})
	}
	u, _ := newUtilization(t.TempDir(), func() time.Time { return before })
	c := testConfig()
	m := c.Machines[0]
	seedOccupancy(u, m, before, []int64{0, 0, 0, 0})
	for _, s := range u.recent {
		for i := 12; i < 24; i++ {
			s.Buckets[i][1] = 240000
		} // One older hour below 90%.
	}
	history, _, _ := u.idleHistory(c, nil, before)
	if history[m.ID].Tier != 2 {
		t.Fatal("partial old hour granted 3h zero priority")
	}
	for _, s := range u.recent {
		s.Buckets[47][2] = 1
	} // Never round a short process to zero.
	history, _, _ = u.idleHistory(c, nil, before)
	if history[m.ID].Tier != 4 {
		t.Fatal("occupied millisecond rounded to zero")
	}
}

func TestRecentActualObservationBoundaryRestartAndIdentity(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 5, 0, 0, chinaTime)
	dir := t.TempDir()
	u, _ := newUtilization(dir, func() time.Time { return now })
	c := testConfig()
	m := c.Machines[0]
	start := now.Add(-10 * time.Minute)
	for at := start; !at.After(now); at = at.Add(10 * time.Second) {
		observeOccupancy(u, m, at, idleUtilizationTable("0"))
	}
	history, _, _ := u.idleHistory(c, nil, now)
	if history[m.ID].Windows["5m"].CoveragePercent != 100 || history[m.ID].Tier != 0 {
		t.Fatal("five-minute integration or cold start wrong")
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUtilization(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	history, _, _ = reopened.idleHistory(c, nil, now)
	if history[m.ID].Windows["5m"].ObservedSeconds != 300 {
		t.Fatal("restart lost five-minute data")
	}
	now = now.Add(10 * time.Minute)
	observeOccupancy(reopened, m, now, idleUtilizationTable("0"))
	history, _, _ = reopened.idleHistory(c, nil, now)
	if history[m.ID].Windows["5m"].ObservedSeconds != 0 {
		t.Fatal("restart bridged missing samples")
	}
	c.Machines[0].ResourceID = "replacement"
	history, _, _ = reopened.idleHistory(c, nil, now.Add(-10*time.Minute))
	if history[m.ID].Windows["5m"].OccupiedPercent != nil {
		t.Fatal("physical identity change inherited old idle")
	}
	c.Machines[0] = m
	c.Machines[0].Host = "192.0.2.44"
	history, _, _ = reopened.idleHistory(c, nil, now.Add(-10*time.Minute))
	if history[m.ID].Windows["5m"].OccupiedPercent != nil {
		t.Fatal("SSH identity change inherited old idle")
	}
}

func TestRecentGapIsUnknownNotIdle(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 30, 0, 0, chinaTime)
	u, _ := newUtilization(t.TempDir(), func() time.Time { return now })
	c := testConfig()
	m := c.Machines[0]
	observeOccupancy(u, m, now.Add(-time.Hour), idleUtilizationTable("0"))
	observeOccupancy(u, m, now, idleUtilizationTable("0"))
	history, _, _ := u.idleHistory(c, nil, now)
	if history[m.ID].Windows["1h"].ObservedSeconds != 50 || history[m.ID].Tier != 0 {
		t.Fatal("unobserved gap became 1h idle")
	}
}

func TestRecentRejectsMissingBusyField(t *testing.T) {
	for _, raw := range []string{`{"buckets":[[1800000000,300000]]}`, `{"buckets":[[1800000000,300000,0,0]]}`} {
		var series occupancySeries
		if json.Unmarshal([]byte(raw), &series) == nil {
			t.Fatal("missing/truncated occupancy field accepted as zero")
		}
	}
}

func setupIdleRanking(t *testing.T) (*IdleRanking, *TaskManager, *Monitor, Config, *Store, *time.Time) {
	t.Helper()
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{})
	t.Cleanup(m.Close)
	now := time.Date(2026, 10, 22, 10, 30, 0, 0, chinaTime)
	m.now = func() time.Time { return now }
	u, err := newUtilization(store.dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	monitor.SetUtilization(u)
	machine := c.Machines[0]
	code := 0
	monitor.states[machine.ID] = State{SampleIdentity: monitorSampleIdentity(machine), Status: "online", UpdatedAt: now, Results: []Result{{CommandID: "npu", Stdout: idleUtilizationTable("0"), EndedAt: now, ExitCode: &code}}}
	seedOccupancy(u, machine, now, []int64{0, 0, 0, 0})
	r, err := newIdleRanking(store, monitor, m, ServiceInfo{Instance: "test", ServiceID: "service", RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now }
	return r, m, monitor, c, store, &now
}

func TestIdleCurrentResourceGates(t *testing.T) {
	for _, name := range []string{"ready", "busy", "unknown_table", "partial_idle_markers", "stale", "truncated", "hbm", "count", "model", "reservation", "task", "identity"} {
		t.Run(name, func(t *testing.T) {
			r, m, monitor, c, _, now := setupIdleRanking(t)
			machine := c.Machines[0]
			req := defaultIdleRequirements(c)
			s := monitor.states[machine.ID]
			want := ""
			switch name {
			case "busy":
				s.Results[0].Stdout = npuTable("0", "0")
				want = "npu_process_busy"
			case "unknown_table":
				s.Results[0].Stdout = strings.Split(idleUtilizationTable("0"), "| NPU ID | Process id")[0]
				want = "npu_process_unknown"
			case "partial_idle_markers":
				s.Results[0].Stdout = strings.Replace(idleUtilizationTable("0"), "| No running processes found |", "| No running processes found in NPU 0 |", 1)
				want = "npu_process_unknown"
			case "stale":
				s.Results[0].EndedAt = now.Add(-time.Hour)
				want = "npu_sample_stale"
			case "truncated":
				s.Results[0].Truncated = true
				want = "npu_sample_unknown"
			case "hbm":
				req.MinFreeHBMMB = 98304
				want = "hbm_insufficient"
			case "count":
				req.DevicesPerMachine = 8
				want = "npu_device_count_mismatch"
			case "model":
				req.Model = "910B"
				want = "npu_model_mismatch"
			case "reservation":
				m.reservations["owner"] = Reservation{ID: "owner", MachineID: machine.ID, Host: machine.Host, Port: machine.Port}
				want = "已被预约"
			case "task":
				m.jobs["unknown"] = &TaskJob{TaskRequest: TaskRequest{ID: "unknown"}, SelectedMachineID: machine.ID, Host: machine.Host, Port: machine.Port, Status: "unknown"}
				want = "任务占用"
			case "identity":
				s.SampleIdentity = "previous-physical-resource"
				want = "npu_sample_identity_changed"
			}
			monitor.states[machine.ID] = s
			report := r.Report(req)
			row := report.Machines[0]
			if name == "ready" {
				if !row.Eligible || !row.Recommended || row.Rank != 1 || row.Tier != 1 {
					t.Fatalf("ready %+v", row)
				}
			} else if row.Eligible || row.Rank != 0 || row.CurrentReason != want {
				t.Fatalf("%s got %+v", name, row)
			}
		})
	}
}

func TestIdleExportScheduleAPIAndFailures(t *testing.T) {
	r, m, monitor, _, store, now := setupIdleRanking(t)
	*now = now.Add(-time.Second)
	if err := r.CheckExport(); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(r.exportPath())
	*now = now.Add(time.Second)
	if err := r.CheckExport(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(r.exportPath())
	if bytes.Equal(first, second) {
		t.Fatal("half-hour boundary did not update export")
	}
	var report IdleRankingReport
	if json.Unmarshal(second, &report) != nil || !report.RequiresLiveCheck || report.ExportedAt == nil || !report.NextRefreshAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("invalid export: %s", second)
	}
	for _, secret := range []string{"only-a-test-password", "watch-token", "Stdout", "stdout", "privateKey", "username"} {
		if bytes.Contains(second, []byte(secret)) {
			t.Fatal("export includes credential/raw sample", secret)
		}
	}
	info, _ := os.Stat(r.exportPath())
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("export not private")
	}
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	app.idleRanking = r
	app.service = &r.service
	for _, tc := range []struct {
		method, path string
		token        bool
		run          string
		status       int
	}{
		{"GET", "/api/idle-ranking", false, "", 403}, {"GET", "/api/idle-ranking", true, "wrong", 409},
		{"POST", "/api/idle-ranking", true, "run", 405}, {"GET", "/api/idle-ranking?maxSampleAgeSeconds=0", true, "run", 400},
		{"GET", "/api/idle-ranking?devicesPerMachine=NaN", true, "run", 400}, {"GET", "/api/idle-ranking?minFreeHbmMB=-1", true, "run", 400},
		{"GET", "/api/idle-ranking?group=a&group=b", true, "run", 400}, {"GET", "/api/idle-ranking?ignored=x", true, "run", 400},
		{"GET", "/api/idle-ranking?group=absent", true, "run", 200}, {"GET", "/api/idle-ranking?minFreeHbmMB=98304", true, "run", 200},
	} {
		request := httptest.NewRequest(tc.method, "http://"+app.host+tc.path, nil)
		if tc.token {
			request.Header.Set("X-Watch-Token", app.token)
		}
		if tc.run != "" {
			request.Header.Set("X-Watch-Instance", tc.run)
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, request)
		if w.Code != tc.status {
			t.Fatalf("%s got %d %s", tc.path, w.Code, w.Body)
		}
		if tc.status == 200 && strings.Contains(tc.path, "98304") {
			var p IdleRankingReport
			_ = json.Unmarshal(w.Body.Bytes(), &p)
			if p.Machines[0].Eligible {
				t.Fatal("API ignored HBM requirement")
			}
		}
	}
	// An export failure is visible and does not silently advance its timestamp.
	previous := *r.exportedAt
	if err := os.Rename(filepath.Dir(r.exportPath()), filepath.Join(store.dir, "exports-saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "exports"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(30 * time.Minute)
	if r.CheckExport() == nil || r.exportError == "" || !r.exportedAt.Equal(previous) {
		t.Fatal("failed export was treated as success")
	}
}

func TestRecentRetentionAndSixtyMachineCapacity(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 30, 0, 0, chinaTime)
	u, _ := newUtilization(t.TempDir(), func() time.Time { return now })
	c := testConfig()
	for i := 0; i < 60; i++ {
		m := c.Machines[0]
		m.ID = fmt.Sprintf("machine-%02d", i)
		m.ResourceID = m.ID
		seedOccupancy(u, m, now, []int64{0, 17, 100, 3, 0, 0})
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	if u.recentBytes > recentOccupancyLimit {
		t.Fatal("recent store exceeded cap")
	}
	now = now.Add(7 * time.Hour)
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(u.recent) != 0 {
		t.Fatal("six-hour retention did not prune")
	}
	// Corrupt duration must never become a fabricated zero-occupancy history.
	var packed bytes.Buffer
	z := gzip.NewWriter(&packed)
	_, _ = z.Write([]byte(`{"version":1,"series":{"bad":{"machine":{"id":"m"},"buckets":[[1,-1,0]]}}}`))
	_ = z.Close()
	if err := os.WriteFile(u.recentPath(), packed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newUtilization(filepath.Dir(u.dir), func() time.Time { return now }); err == nil {
		t.Fatal("corrupt recent history accepted")
	}
}

func TestIdleExportSixtyMachineCapacity(t *testing.T) {
	r, _, monitor, c, store, now := setupIdleRanking(t)
	base := c.Machines[0]
	baseState := monitor.states[base.ID]
	c.Machines = nil
	for i := 0; i < 60; i++ {
		machine := base
		machine.ID = fmt.Sprintf("a5-b03-node-%02d", i)
		machine.Name = fmt.Sprintf("A5 B03-%02d · 192.0.2.%d · B05跳板", i, i+1)
		machine.ResourceID = machine.ID
		machine.Host = fmt.Sprintf("192.0.2.%d", i+1)
		c.Machines = append(c.Machines, machine)
		s := baseState
		s.MachineID = machine.ID
		s.SampleIdentity = monitorSampleIdentity(machine)
		monitor.states[machine.ID] = s
		seedOccupancy(monitor.utilization, machine, *now, []int64{0, 0, 0, 0, 0, 0})
	}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	i, _ := os.Stat(r.exportPath())
	t.Logf("synthetic 60-machine JSON: %d bytes (cap %d)", i.Size(), idleRankingExportLimit)
	if i.Size() > idleRankingExportLimit {
		t.Fatal("export exceeded cap")
	}
}
