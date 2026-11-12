package watch

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func recordAvailability(u *Utilization, m Machine, start, end time.Time, busy bool) {
	p := utilizationPending{resourceKey: machineResource(m), machine: utilizationMachine{m.ID, m.Name, m.Group, fmt.Sprintf("%s:%d", m.Host, m.Port), "Ascend950DT", "NPU Util(%)"}, occupancyKnown: true, occupied: busy}
	u.advanceAvailability(p, start, end)
}
func availabilityTestStore(t *testing.T, now *time.Time) (*Utilization, Config, time.Time) {
	t.Helper()
	u, err := newUtilization(t.TempDir(), func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	c.Machines = c.Machines[:1]
	c.Machines[0].ResourceID = "physical-one"
	return u, c, u.availabilityToday()
}
func TestAvailabilityIndependentWindowsAndConservation(t *testing.T) {
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, chinaTime)
	u, c, start := availabilityTestStore(t, &now)
	m := c.Machines[0]
	recordAvailability(u, m, start, start.Add(70*time.Minute), false)
	recordAvailability(u, m, start.Add(70*time.Minute), start.Add(80*time.Minute), true)
	recordAvailability(u, m, start.Add(80*time.Minute), start.Add(120*time.Minute), false)
	r := u.AvailabilityReport(c, "", "all")
	row := r.Machines[0]
	want := IdleWindowCounts{Minutes5: 22, Minutes15: 6, Minutes30: 3, Minutes60: 1, Minutes120: 0}
	if row.Counts != want || r.Counts != want || row.IdleSeconds != 110*60 || row.BusySeconds != 10*60 || row.UnknownSeconds != 60*60 || row.FragmentSeconds != 20*60 || row.LongestIdleSeconds != 70*60 {
		t.Fatalf("wrong independent windows: %+v", row)
	}
	if float64(row.Counts.Minutes30)*1800+row.FragmentSeconds+row.BusySeconds+row.UnknownSeconds != now.Sub(start).Seconds() {
		t.Fatal("half-hour chart does not conserve elapsed time")
	}
	// Two 35-minute runs must not become one one-hour experiment opportunity.
	u.daily = availabilityDisk{}
	u.resetAvailabilityDay()
	recordAvailability(u, m, start, start.Add(35*time.Minute), false)
	recordAvailability(u, m, start.Add(40*time.Minute), start.Add(75*time.Minute), false)
	row = u.AvailabilityReport(c, "", "all").Machines[0]
	if row.Counts.Minutes60 != 0 || row.Counts.Minutes30 != 2 {
		t.Fatalf("unknown gap joined: %+v", row)
	}
}

func TestAvailabilityScopesDoNotJoinAcrossExcludedPeriods(t *testing.T) {
	now := time.Date(2026, 11, 11, 21, 0, 0, 0, chinaTime)
	u, c, start := availabilityTestStore(t, &now)
	recordAvailability(u, c.Machines[0], start, now, false)
	for _, tc := range []struct {
		scope                       string
		hours                       float64
		twoHours, oneHour, halfHour int
	}{
		{"work", 9, 4, 9, 18}, {"off", 12, 5, 12, 24}, {"all", 21, 10, 21, 42},
	} {
		r := u.AvailabilityReport(c, "", tc.scope)
		if r.ExpectedSeconds != tc.hours*3600 || r.UnknownSeconds != 0 || r.CoveragePercent != 100 || r.Counts.Minutes120 != tc.twoHours || r.Counts.Minutes60 != tc.oneHour || r.Counts.Minutes30 != tc.halfHour {
			t.Fatalf("scope %s joined excluded periods or used wrong denominator: %+v", tc.scope, r)
		}
	}
	now = start.Add(20*time.Hour + 30*time.Minute)
	r := u.AvailabilityReport(c, "", "work")
	if r.ExpectedSeconds != 8.5*3600 || r.Counts.Minutes120 != 3 || r.Counts.Minutes60 != 8 {
		t.Fatalf("20:00 hour missing or future work counted: %+v", r)
	}
	now = start.Add(8 * time.Hour)
	r = u.AvailabilityReport(c, "", "work")
	if r.ExpectedSeconds != 0 || r.UnknownSeconds != 0 || r.Counts.Minutes5 != 0 {
		t.Fatal("future work time counted")
	}
}
func TestAvailabilityExactBoundsAndOngoingRun(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		want     IdleWindowCounts
	}{
		{5*time.Minute - time.Millisecond, IdleWindowCounts{}},
		{5 * time.Minute, IdleWindowCounts{Minutes5: 1}},
		{15*time.Minute - time.Millisecond, IdleWindowCounts{Minutes5: 2}},
		{15 * time.Minute, IdleWindowCounts{Minutes5: 3, Minutes15: 1}},
		{70 * time.Minute, IdleWindowCounts{Minutes5: 14, Minutes15: 4, Minutes30: 2, Minutes60: 1}},
		{2 * time.Hour, IdleWindowCounts{Minutes5: 24, Minutes15: 8, Minutes30: 4, Minutes60: 2, Minutes120: 1}},
	} {
		now := time.Date(2026, 11, 11, 3, 0, 0, 0, chinaTime)
		u, c, start := availabilityTestStore(t, &now)
		// An idle run may begin off any wall-clock grid.
		at := start.Add(2*time.Minute + 123*time.Millisecond)
		recordAvailability(u, c.Machines[0], at, at.Add(tc.duration), false)
		if row := u.AvailabilityReport(c, "", "all").Machines[0]; row.Counts != tc.want {
			t.Fatalf("%s: %+v", tc.duration, row)
		}
	}
}
func TestAvailabilitySamplingFailureComputeAndMidnight(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 45, 0, 0, chinaTime)
	u, c, start := availabilityTestStore(t, &now)
	m := c.Machines[0]
	recordAvailability(u, m, start.Add(-10*time.Minute), start.Add(20*time.Minute), false)
	row := u.AvailabilityReport(c, "", "all").Machines[0]
	if row.Counts.Minutes15 != 1 || row.IdleSeconds != 1200 || row.UnknownSeconds != 1500 {
		t.Fatalf("midnight clipping: %+v", row)
	}
	now = start.AddDate(0, 0, 1).Add(time.Hour)
	if r := u.AvailabilityReport(c, "", "all"); r.Counts.Minutes15 != 0 || r.UnknownSeconds != 3600 {
		t.Fatalf("previous day leaked: %+v", r)
	}
	u.daily = availabilityDisk{}
	u.resetAvailabilityDay()
	at := u.availabilityToday()
	observeOccupancy(u, m, at, idleUtilizationTable("0"))
	// A failed sample ends the bounded hold; a distant new sample cannot fill it.
	u.Observe(m, State{UpdatedAt: at.Add(10 * time.Second)}, 10*time.Second)
	observeOccupancy(u, m, at.Add(30*time.Minute), idleUtilizationTable("0"))
	if r := u.AvailabilityReport(c, "", "all"); r.Counts.Minutes15 != 0 || r.IdleSeconds > 50 {
		t.Fatalf("failure/gap backfilled: %+v", r)
	}
	u.daily = availabilityDisk{}
	u.resetAvailabilityDay()
	p := utilizationPending{resourceKey: machineResource(m), machine: utilizationMachine{m.ID, m.Name, m.Group, fmt.Sprintf("%s:%d", m.Host, m.Port), "Ascend950DT", "NPU Util(%)"}, occupancyKnown: true, basis: 100}
	u.advanceAvailability(p, at, at.Add(time.Hour))
	if row := u.AvailabilityReport(c, "", "all").Machines[0]; row.BusySeconds != 3600 || row.Counts.Minutes15 != 0 {
		t.Fatalf("nonzero compute counted idle: %+v", row)
	}
}
func TestAvailabilityAliasesBusyDominatesAndIdentityChanges(t *testing.T) {
	now := time.Date(2026, 11, 11, 2, 0, 0, 0, chinaTime)
	u, c, start := availabilityTestStore(t, &now)
	a := c.Machines[0]
	b := a
	b.ID = "alias-b"
	b.Host = "192.0.2.52"
	c.Machines = append(c.Machines, b)
	recordAvailability(u, a, start, start.Add(70*time.Minute), false)
	recordAvailability(u, b, start, start.Add(70*time.Minute), false)
	r := u.AvailabilityReport(c, "", "all")
	if r.MachineCount != 1 || r.ConfiguredEntries != 2 || r.Counts.Minutes30 != 2 || r.IdleSeconds != 4200 {
		t.Fatalf("duplicate physical capacity: %+v", r)
	}
	u.daily = availabilityDisk{}
	u.resetAvailabilityDay()
	recordAvailability(u, a, start, start.Add(70*time.Minute), false)
	recordAvailability(u, b, start.Add(30*time.Minute), start.Add(40*time.Minute), true)
	r = u.AvailabilityReport(c, "", "all")
	if r.Counts.Minutes60 != 0 || r.Counts.Minutes30 != 2 || r.BusySeconds != 600 {
		t.Fatalf("alias busy lost: %+v", r)
	}
	c.Machines[0].ResourceID = "replaced"
	c.Machines = c.Machines[:1]
	if r := u.AvailabilityReport(c, "", "all"); r.IdleSeconds != 0 {
		t.Fatal("changed physical identity inherited availability")
	}
	c.Machines[0] = a
	c.Machines[0].Host = "changed-host"
	if r := u.AvailabilityReport(c, "", "all"); r.IdleSeconds != 0 {
		t.Fatal("changed address inherited availability")
	}
	a.ResourceID = ""
	a.Host = "127.0.0.1"
	a.Port = 12345
	b = a
	b.ID = "other"
	b.Port = 12346
	c.Machines = []Machine{a, b}
	if r := u.AvailabilityReport(c, "", "all"); r.MachineCount != 2 {
		t.Fatal("unidentified forwarding ports collapsed")
	}
}
func TestAvailabilityRestartBoundsAndClockOverlap(t *testing.T) {
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, chinaTime)
	u, c, start := availabilityTestStore(t, &now)
	m := c.Machines[0]
	recordAvailability(u, m, start, start.Add(40*time.Minute), false)
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUtilization(filepath.Dir(u.dir), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	recordAvailability(reopened, m, start.Add(50*time.Minute), start.Add(90*time.Minute), false)
	if r := reopened.AvailabilityReport(c, "", "all"); r.Counts.Minutes60 != 0 || r.Counts.Minutes30 != 2 {
		t.Fatalf("restart bridged unknown: %+v", r)
	}
	recordAvailability(reopened, m, start.Add(20*time.Minute), start.Add(30*time.Minute), true)
	r := reopened.AvailabilityReport(c, "", "all")
	if r.Counts.Minutes30 != 0 || r.IdleSeconds+r.BusySeconds > now.Sub(start).Seconds() {
		t.Fatalf("clock overlap fabricated continuity: %+v", r)
	}
	for i := 0; i < availabilitySegmentLimit+20; i++ {
		a := start.Add(time.Duration(i) * time.Second)
		recordAvailability(u, m, a, a.Add(time.Second), i%2 == 0)
	}
	if !u.daily.Truncated {
		t.Fatal("unbounded daily segment history")
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(u.availabilityPath()); err != nil || info.Size() > availabilityLimit {
		t.Fatal("daily disk budget exceeded", err)
	}
	now = start.AddDate(0, 0, 1).Add(time.Hour)
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(u.daily.Series) != 0 || u.daily.Truncated {
		t.Fatal("calendar day did not reset")
	}
}
func TestAvailabilityCorruptHistoryPreservedAndReadOnlyAPI(t *testing.T) {
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, chinaTime)
	u, c, _ := availabilityTestStore(t, &now)
	var packed bytes.Buffer
	z := gzip.NewWriter(&packed)
	meta := utilizationMachine{c.Machines[0].ID, "name", "group", "host:22", "model", "metric"}
	key := recentSeriesKey(meta, "physical:id")
	raw := fmt.Sprintf(`{"version":1,"date":"2026-11-11","series":{"%s":{"resourceKey":"physical:id","machine":{"id":%q,"address":"host:22","model":"model","metric":"metric"},"segments":[[0,900000]]}}}`, key, meta.ID)
	_, _ = z.Write([]byte(raw))
	_ = z.Close()
	before := packed.Bytes()
	if err := os.WriteFile(u.availabilityPath(), before, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUtilization(filepath.Dir(u.dir), func() time.Time { return now })
	if err != nil {
		t.Fatal("optional ledger blocked monitoring", err)
	}
	if reopened.dailyError == "" || reopened.Flush() == nil {
		t.Fatal("corrupt optional ledger not reported")
	}
	after, _ := os.ReadFile(u.availabilityPath())
	if !bytes.Equal(before, after) {
		t.Fatal("corrupt evidence overwritten")
	}
	store, _ := NewStore(t.TempDir())
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor := NewMonitor(nil)
	monitor.SetUtilization(reopened)
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:12345")
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"GET", "/api/availability", 200}, {"GET", "/api/availability?group=missing", 200}, {"GET", "/api/availability?date=2026-11-12", 400}, {"GET", "/api/availability?group=x&group=y", 400}, {"POST", "/api/availability", 405}} {
		req := httptest.NewRequest(tc.method, "http://127.0.0.1:12345"+tc.path, nil)
		req.Header.Set("X-Watch-Token", app.token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
		if tc.code == 200 {
			var r AvailabilityReport
			if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.TimeZone != "Asia/Shanghai" || r.StorageError == "" {
				t.Fatalf("report lost missing/error context: %+v %v", r, err)
			}
		}
	}
}
