package watch

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Reduced fixtures preserve actual A5 and A2/A3 table layouts.
func npuTable(a, b string) string {
	return "| NPU ID | Name | Health | Power(W) Temp(C) Hugepages-Usage(page) |\n" +
		"| | | Bus-Id | NPU Util(%) Memory-Usage(MB) HBM-Usage(MB) |\n" +
		"| 0 | Ascend950DT | Warning | 391.0 50 0 / 0 |\n" +
		"| | | NA | " + a + " 0 / 0 4792 / 98304 |\n" +
		"| 1 | Ascend950DT | Warning | 395.0 52 0 / 0 |\n" +
		"| | | NA | " + b + " 0 / 0 4789 / 98304 |\n" +
		"| NPU ID | Process id | Process name | Process memory(MB) |\n| 0 | 42 | python | 100 |"
}
func TestUtilizationParsers(t *testing.T) {
	a2 := "| NPU Name | Health | Power(W) Temp(C) |\n| Chip Device | Bus-Id | AICore(%) Memory-Usage(MB) HBM-Usage(MB) |\n| 0 910B | OK | 100 40 |\n| 0 0 | 0000:01:00.0 | 20 0 / 0 1000 / 65536 |\n| 1 910B | OK | 100 40 |\n| 0 1 | 0000:02:00.0 | 80 0 / 0 1000 / 65536 |"
	for _, fixture := range []struct {
		output, model, metric string
		want                  int64
	}{{npuTable("0", "100"), "Ascend950DT", "NPU Util(%)", 5000}, {a2, "910B", "AICore(%)", 5000}, {npuTable("0", "0"), "Ascend950DT", "NPU Util(%)", 0}} {
		got, err := parseNPUUtilization(fixture.output)
		if err != nil || got.basis != fixture.want || got.model != fixture.model || got.metric != fixture.metric {
			t.Fatalf("parse: %+v %v", got, err)
		}
	}
	for _, bad := range []string{npuTable("N/A", "0"), npuTable("101", "0"), npuTable("NaN", "0"), npuTable("-1", "0"), strings.Replace(npuTable("0", "0"), "| | | NA | 0 0 / 0 4789 / 98304 |", "", 1), "random 0% text", strings.Replace(npuTable("0", "0"), "| | | NA | 0 0 / 0 4792 / 98304 |", "| | | NA | 0 0 / 0 4792 / 98304 |\n| | | NA | 0 0 / 0 4792 / 98304 |", 1)} {
		if _, err := parseNPUUtilization(bad); err == nil {
			t.Fatalf("invalid output accepted: %s", bad)
		}
	}
}
func observeUtilization(u *Utilization, m Machine, at time.Time, value string) {
	code := 0
	u.Observe(m, State{UpdatedAt: at, Results: []Result{{CommandID: "npu", Stdout: npuTable(value, value), ExitCode: &code, EndedAt: at}}}, 10*time.Second)
}
func closeEnough(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 0.001 {
		t.Fatalf("got %v want %v", got, want)
	}
}
func TestUtilizationWeightsHourBoundaryMissingAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, chinaTime)
	dir := t.TempDir()
	u, err := newUtilization(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	m := c.Machines[0]
	at := now.Add(-2 * time.Hour)
	observeUtilization(u, m, at.Add(-5*time.Second), "10")
	observeUtilization(u, m, at.Add(5*time.Second), "50")
	observeUtilization(u, m, at.Add(15*time.Second), "0")
	u.Observe(m, State{UpdatedAt: at.Add(25 * time.Second)}, 10*time.Second)
	report := u.Report(c, at.Add(-time.Hour), at.Add(time.Hour), "", "")
	hours := report.Machines[0].Hours
	closeEnough(t, hours[0].Mean, 10)
	closeEnough(t, hours[1].Mean, 22)
	closeEnough(t, hours[1].Peak, 50)
	closeEnough(t, hours[1].LowPercent, 40)
	if hours[1].ObservedSeconds != 25 || hours[1].CoveragePercent >= 1 {
		t.Fatalf("missing treated as coverage: %+v", hours[1])
	}
	if err = u.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUtilization(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observeUtilization(reopened, m, at.Add(10*time.Minute), "100")
	hours = reopened.Report(c, at, at.Add(time.Hour), "", "").Machines[0].Hours
	closeEnough(t, hours[0].Mean, 22)
	if hours[0].ObservedSeconds != 25 {
		t.Fatal("restart bridged a missing interval")
	}
}
func TestUtilizationGapCapInvalidResultAndIdentity(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, chinaTime)
	u, _ := newUtilization(t.TempDir(), func() time.Time { return now })
	c := testConfig()
	m := c.Machines[0]
	at := now.Add(-time.Hour)
	observeUtilization(u, m, at, "0")
	observeUtilization(u, m, at.Add(10*time.Minute), "100")
	r := u.Report(c, at, now, "", "")
	if r.Machines[0].Hours[0].ObservedSeconds != 50 {
		t.Fatal("unbounded gap interpolation")
	}
	m.Host = "192.0.2.99"
	observeUtilization(u, m, at.Add(11*time.Minute), "50")
	r = u.Report(c, at, now, "", "")
	if len(r.Machines) != 2 {
		t.Fatal("changed host identities mixed")
	}
	code := 0
	u.Observe(m, State{UpdatedAt: at.Add(12 * time.Minute), Results: []Result{{CommandID: "npu", Stdout: npuTable("0", "0"), ExitCode: &code, Truncated: true}}}, 10*time.Second)
	if len(u.pending) != 0 {
		t.Fatal("truncated result accepted")
	}
}
func TestUtilizationCompactThirtyDaysAndQuota(t *testing.T) {
	now := time.Date(2026, 10, 9, 23, 59, 0, 0, chinaTime)
	dir := t.TempDir()
	u, _ := newUtilization(dir, func() time.Time { return now })
	for day := 0; day < 30; day++ {
		date := now.AddDate(0, 0, -day).Format("2006-01-02")
		d := &utilizationDay{1, date, map[string]utilizationMachine{}, map[string][]utilizationBucket{}}
		for machine := 0; machine < 60; machine++ {
			key := fmt.Sprintf("%024d", machine)
			d.Machines[key] = utilizationMachine{fmt.Sprintf("machine-%02d", machine), "机器", "A5", "192.0.2.1:22", "Ascend950DT", "NPU Util(%)"}
			for hour := 0; hour < 24; hour++ {
				d.Hours[key] = append(d.Hours[key], utilizationBucket{int64(hour), 3600000, 18000000000, 1800000, 10000, 360})
			}
		}
		u.days[date], u.dirty[date] = d, true
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic 60-machine / 30-day footprint: %d bytes (%.2f MiB)", u.bytes, float64(u.bytes)/1048576)
	if u.bytes > 9*1024*1024 {
		t.Fatal("hourly storage exceeded budget")
	}
	reopened, err := newUtilization(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.days) != 30 {
		t.Fatal("lost persisted days")
	}
	now = now.AddDate(0, 0, 1)
	if err = reopened.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.days) != 29 {
		t.Fatal("30-day retention did not expire oldest date")
	}
	reopened.limit = 200000
	if err = reopened.Flush(); err != nil {
		t.Fatal(err)
	}
	if reopened.bytes > reopened.limit {
		t.Fatal("disk limit not enforced")
	}
	for _, file := range mustGlob(t, filepath.Join(dir, "utilization", "*.json")) {
		raw, _ := os.ReadFile(file)
		if strings.Contains(string(raw), "stdout") || strings.Contains(string(raw), "cardId") || strings.Contains(string(raw), "password") {
			t.Fatal("raw or device data persisted")
		}
	}
}
func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	files, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return files
}
func TestUtilizationLowWindowsCoverageAndAPI(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, chinaTime)
	dir := t.TempDir()
	u, _ := newUtilization(dir, func() time.Time { return now })
	c := testConfig()
	m := c.Machines[0]
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, chinaTime)
	for at := start; !at.After(start.Add(2 * time.Hour)); at = at.Add(10 * time.Second) {
		observeUtilization(u, m, at, "0")
	}
	r := u.Report(c, start, start.AddDate(0, 0, 1), "", "")
	if len(r.Machines[0].LowWindows) != 1 || r.Machines[0].LowWindows[0].Before != start.Add(2*time.Hour).Unix() {
		t.Fatal("low windows wrong")
	}
	if r.Machines[0].Hours[2].Mean != nil {
		t.Fatal("unobserved hour became zero")
	}
	store, _ := NewStore(t.TempDir())
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor := NewMonitor(nil)
	monitor.SetUtilization(u)
	s := NewServer(store, monitor, nil, nil, "127.0.0.1:12345")
	for _, test := range []struct {
		query string
		code  int
	}{{"period=yesterday", 200}, {"period=week", 200}, {"period=today", 200}, {"date=2026-10-08&days=1", 200}, {"date=2026-10-08&period=week", 400}, {"date=2026-10-11", 400}, {"date=2026-09-01", 400}, {"period=bad", 400}, {"days=2", 400}, {"date=2026-10-08&days=0", 400}} {
		req := httptest.NewRequest("GET", "http://127.0.0.1:12345/api/utilization?"+test.query, nil)
		req.Header.Set("X-Watch-Token", s.token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != test.code {
			t.Fatalf("%s: %d %s", test.query, w.Code, w.Body)
		}
		if test.code == 200 {
			var result UtilizationReport
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.TimeZone != "Asia/Shanghai" {
				t.Fatal("wrong timezone")
			}
		}
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:12345/api/utilization", nil)
	req.Header.Set("X-Watch-Token", s.token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 405 {
		t.Fatal("write accepted")
	}
}
