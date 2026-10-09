package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUtilizationMissingModelAndPersistFailure(t *testing.T) {
	for _, model := range []string{"N/A", "NA", ""} {
		if _, err := parseNPUUtilization(strings.Replace(npuTable("0", "0"), "Ascend950DT", model, 1)); err == nil {
			t.Fatal("partial machine with missing model accepted")
		}
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, chinaTime)
	dir := t.TempDir()
	u, _ := newUtilization(dir, func() time.Time { return now })
	c := testConfig()
	observeUtilization(u, c.Machines[0], now.Add(-20*time.Second), "50")
	observeUtilization(u, c.Machines[0], now.Add(-10*time.Second), "50")
	u.limit = 1
	if err := u.Flush(); err == nil {
		t.Fatal("write limit ignored")
	}
	r := u.Report(c, now.Add(-time.Hour).Truncate(time.Hour), now.Add(time.Hour), "", "")
	if r.StorageError == "" || r.Machines[0].Mean == nil {
		t.Fatal("persist failure stopped or hid live statistics")
	}
	u.limit = utilizationLimit
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "utilization", "2026-10-09.json")
	raw, _ := os.ReadFile(file)
	if err := os.WriteFile(file, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newUtilization(dir, func() time.Time { return now }); err == nil {
		t.Fatal("corrupt day silently accepted")
	}
	if data, _ := os.ReadFile(file); string(data) != "corrupt" {
		t.Fatal("corrupt evidence overwritten")
	}
	_ = os.WriteFile(file, raw, 0600)
	scratch := filepath.Join(dir, "utilization", ".watch-interrupted")
	_ = os.WriteFile(scratch, []byte("interrupted write"), 0600)
	if _, err := newUtilization(dir, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatal("interrupted scratch consumes quota")
	}
}
