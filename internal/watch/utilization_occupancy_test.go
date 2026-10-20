package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func idleUtilizationTable(value string) string {
	return strings.Replace(npuTable(value, value), "| 0 | 42 | python | 100 |", "| No running processes found |", 1)
}

func TestOccupancyAnyDeviceProcessAndUnknownTables(t *testing.T) {
	for _, tc := range []struct {
		name, output    string
		occupied, known bool
	}{
		{"idle", idleUtilizationTable("0"), false, true},
		{"per_card_idle", strings.Replace(idleUtilizationTable("0"), "| No running processes found |", "| No running processes found in NPU 0 |\n| No running processes found in NPU 1 |", 1), false, true},
		{"per_card_idle_incomplete", strings.Replace(idleUtilizationTable("0"), "| No running processes found |", "| No running processes found in NPU 0 |", 1), false, false},
		{"zero_compute_with_process", npuTable("0", "0"), true, true},
		{"last_device_process", strings.Replace(npuTable("0", "0"), "| 0 | 42 | python | 100 |", "| 7 | 42 | python | 100 |", 1), true, true},
		{"chip_column", "| NPU ID | Chip ID | Process id | Process name | Process memory(MB) |\n| 7 | 1 | 123 | train | 100 |", true, true},
		{"missing_table", strings.Split(npuTable("0", "0"), "| NPU ID | Process id")[0], false, false},
		{"header_only", "| NPU ID | Process id | Process name | Process memory(MB) |", false, false},
		{"invalid_pid", strings.Replace(npuTable("0", "0"), "42", "N/A", 1), false, false},
		{"zero_pid", "| NPU ID | Process id | Process name |\n| 7 | 0 | train |", false, false},
		{"pid_not_other_numbers", "| NPU ID | Process id | Process name | Process memory(MB) |\n| 7 | N/A | train | 100 |\n| No running processes found |", false, false},
		{"name_cannot_spoof_idle", strings.Replace(npuTable("0", "0"), "python", "No running processes found", 1), true, true},
		{"marker_without_header", "| No running processes found |", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			occupied, known := parseNPUOccupancy(tc.output, map[string]bool{"0": true, "1": true})
			if occupied != tc.occupied || known != tc.known {
				t.Fatalf("got %v/%v want %v/%v", occupied, known, tc.occupied, tc.known)
			}
		})
	}
}

func TestOccupancyActualA5IdleMarkersCoverAllNPUs(t *testing.T) {
	output := strings.Replace(idleUtilizationTable("0"), "| No running processes found |", "| No running processes found in NPU 0 |\n| No running processes found in NPU 1 |", 1)
	r, err := parseNPUUtilization(output)
	if err != nil || !r.occupancyKnown || r.occupied {
		t.Fatalf("actual idle format rejected: %+v %v", r, err)
	}
	r, err = parseNPUUtilization(strings.Replace(output, "| No running processes found in NPU 1 |", "", 1))
	if err != nil || r.occupancyKnown {
		t.Fatal("missing card counted idle")
	}
}

func observeOccupancy(u *Utilization, m Machine, at time.Time, output string) {
	code := 0
	u.Observe(m, State{UpdatedAt: at, Results: []Result{{CommandID: "npu", Stdout: output, ExitCode: &code, EndedAt: at}}}, 10*time.Second)
}

func TestOccupancyDurationUnknownCoverageAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, chinaTime)
	dir := t.TempDir()
	u, _ := newUtilization(dir, func() time.Time { return now })
	c := testConfig()
	m := c.Machines[0]
	at := now.Add(-time.Hour)
	observeOccupancy(u, m, at, idleUtilizationTable("0"))
	observeOccupancy(u, m, at.Add(10*time.Second), npuTable("0", "0"))
	observeOccupancy(u, m, at.Add(30*time.Second), strings.Split(npuTable("0", "0"), "| NPU ID | Process id")[0])
	observeOccupancy(u, m, at.Add(50*time.Second), idleUtilizationTable("0"))
	u.Observe(m, State{UpdatedAt: at.Add(time.Minute)}, 10*time.Second)
	r := u.Report(c, at, now, "", "")
	h := r.Machines[0].Hours[0]
	closeEnough(t, h.Mean, 0)
	closeEnough(t, h.OccupiedPercent, 50)
	closeEnough(t, r.Machines[0].OccupiedPercent, 50)
	closeEnough(t, r.Machines[0].HourOfDay[11].OccupiedPercent, 50)
	if h.ObservedSeconds != 60 || h.OccupancyObservedSeconds != 40 || h.OccupancyCoveragePercent != 40.0/36 {
		t.Fatalf("wrong coverage: %+v", h)
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUtilization(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observeOccupancy(reopened, m, at.Add(10*time.Minute), npuTable("0", "0"))
	h = reopened.Report(c, at, now, "", "").Machines[0].Hours[0]
	closeEnough(t, h.OccupiedPercent, 50)
	if h.OccupancyObservedSeconds != 40 {
		t.Fatal("restart backfilled occupancy")
	}
}

func TestOccupancyHourBoundaryAndGapCap(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, chinaTime)
	u, _ := newUtilization(t.TempDir(), func() time.Time { return now })
	c := testConfig()
	at := now.Add(-time.Hour)
	observeOccupancy(u, c.Machines[0], at.Add(-5*time.Second), npuTable("0", "0"))
	observeOccupancy(u, c.Machines[0], at.Add(5*time.Second), idleUtilizationTable("0"))
	observeOccupancy(u, c.Machines[0], at.Add(10*time.Minute), npuTable("0", "0"))
	hours := u.Report(c, at.Add(-time.Hour), now, "", "").Machines[0].Hours
	closeEnough(t, hours[0].OccupiedPercent, 100)
	closeEnough(t, hours[1].OccupiedPercent, 100.0*5/55)
	if hours[1].OccupancyObservedSeconds != 55 {
		t.Fatal("unbounded occupation interval")
	}
}

func TestOccupancyLegacyHistoryPreservedWithoutBackfill(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, chinaTime)
	dir := t.TempDir()
	u, _ := newUtilization(dir, func() time.Time { return now })
	legacy := `{"v":1,"d":"2026-10-13","m":{"key":{"id":"one","name":"old","address":"host:22","model":"Ascend950DT","metric":"NPU Util(%)"}},"h":{"key":[[11,60000,60000000,0,1000,6]]}}`
	file := filepath.Join(u.dir, "2026-10-13.json")
	if err := os.WriteFile(file, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	u, err := newUtilization(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Machines: []Machine{{ID: "one", Host: "host", Port: 22}}}
	r := u.Report(c, now.Add(-time.Hour), now, "", "")
	closeEnough(t, r.Machines[0].Mean, 10)
	if r.Machines[0].OccupiedPercent != nil || r.Machines[0].OccupancyCoveragePercent != 0 {
		t.Fatal("legacy occupancy became zero")
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != legacy {
		t.Fatal("untouched legacy file rewritten")
	}
	u.dirty["2026-10-13"] = true
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	raw := readUtilizationBytes(t, file+".gz")
	var disk struct {
		Version int                  `json:"v"`
		Hours   map[string][][]int64 `json:"h"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Version != 2 || len(disk.Hours["key"][0]) != 8 || disk.Hours["key"][0][6] != 0 {
		t.Fatalf("bad migration: %s", raw)
	}
	if _, err := newUtilization(dir, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(legacy, "[11,60000,60000000,0,1000,6]", "[11,60000,60000000,0,1000,6,0]", 1),
		strings.Replace(string(raw), ",0,0]]", ",60001,0]]", 1),
		strings.Replace(string(raw), ",0,0]]", ",0,1]]", 1),
		strings.Replace(string(raw), `"v":2`, `"v":3`, 1),
	} {
		if err := os.WriteFile(file, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newUtilization(dir, func() time.Time { return now }); err == nil {
			t.Fatalf("invalid occupancy bucket accepted: %s", bad)
		}
	}
}

func TestOccupancySameHourAcrossDaysUsesObservedDuration(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, chinaTime)
	u, _ := newUtilization(t.TempDir(), func() time.Time { return now })
	c := testConfig()
	m := c.Machines[0]
	for _, day := range []struct {
		date          string
		covered, busy int64
	}{{"2026-10-11", 3600000, 3600000}, {"2026-10-12", 1200000, 0}} {
		u.days[day.date] = &utilizationDay{2, day.date, map[string]utilizationMachine{"key": {m.ID, m.Name, m.Group, m.Host + ":22", "Ascend950DT", "NPU Util(%)"}}, map[string][]utilizationBucket{"key": {{11, 3600000, 0, 3600000, 0, 360, day.covered, day.busy}}}}
	}
	after := time.Date(2026, 10, 11, 0, 0, 0, 0, chinaTime)
	r := u.Report(c, after, after.AddDate(0, 0, 2), "", "")
	closeEnough(t, r.Machines[0].OccupiedPercent, 75)
	closeEnough(t, r.Machines[0].HourOfDay[11].OccupiedPercent, 75)
	if r.Machines[0].HourOfDay[11].OccupancyObservedSeconds != 4800 {
		t.Fatal("wrong cross-day denominator")
	}
}
