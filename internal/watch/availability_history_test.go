package watch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAvailabilityWeekRetentionRestartAndHistoricalInventory(t *testing.T) {
	today := time.Date(2026, 11, 12, 0, 0, 0, 0, chinaTime)
	now := today.AddDate(0, 0, -8)
	u, _, _ := availabilityTestStore(t, &now)
	c := multiTestConfig(8, "A5 B07")
	u.SetAvailabilityInventory(c)
	for i := -8; i < 0; i++ {
		day := today.AddDate(0, 0, i)
		now = day.Add(23 * time.Hour)
		for _, m := range c.Machines {
			recordAvailability(u, m, day, day.Add(24*time.Hour), false)
		}
		now = day.AddDate(0, 0, 1)
		if err := u.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(u.availabilityHistoryPath("")), "*.json.gz"))
	if len(files) != 7 {
		t.Fatalf("retention files=%d", len(files))
	}
	reopened, err := newUtilization(filepath.Dir(u.dir), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// Today's inventory has changed completely; old groups/capacity stay frozen.
	reopened.SetAvailabilityInventory(Config{})
	for i := 1; i <= 7; i++ {
		date := today.AddDate(0, 0, -i).Format("2006-01-02")
		before, _ := os.ReadFile(u.availabilityHistoryPath(date))
		if len(before) > availabilityLimit {
			t.Fatal("per-day archive exceeds budget")
		}
		for _, scope := range []string{"work", "off", "all"} {
			for _, nodes := range []int{1, 2, 4, 8} {
				r, err := reopened.AvailabilityForDate(Config{}, "", scope, date, nodes)
				if err != nil || !r.HistoryAvailable || r.MachineCount != 8 || r.ConfiguredEntries != 8 || r.Counts != r.TheoreticalCounts || r.CoveragePercent != 100 || len(r.Dates) != 8 || len(r.Groups) != 1 || r.Groups[0] != "A5 B07" {
					t.Fatalf("%s %s %d: %+v %v", date, scope, nodes, r, err)
				}
				if r.CapacityPercent["minutes30"] == nil || *r.CapacityPercent["minutes30"] != 100 {
					t.Fatal("historical percentage wrong")
				}
			}
		}
		after, _ := os.ReadFile(u.availabilityHistoryPath(date))
		if !bytes.Equal(before, after) {
			t.Fatal("read-only historical query rewrote archive")
		}
	}
	if _, err := reopened.AvailabilityForDate(c, "", "all", "2026-11-04", 1); err == nil {
		t.Fatal("expired date accepted")
	}
	if _, err := os.Stat(u.availabilityHistoryPath("2026-11-04")); !os.IsNotExist(err) {
		t.Fatal("expired archive retained")
	}
}

func TestAvailabilityWeekMidnightAllMembersAndMissingDays(t *testing.T) {
	now := time.Date(2026, 11, 11, 23, 30, 0, 0, chinaTime)
	u, _, _ := availabilityTestStore(t, &now)
	c := multiTestConfig(2, "A5 B07")
	u.SetAvailabilityInventory(c)
	start := u.availabilityToday().Add(23*time.Hour + 20*time.Minute)
	for _, m := range c.Machines {
		recordAvailability(u, m, start, now, false)
	}
	previous := now
	now = now.Add(90 * time.Minute)
	for _, m := range c.Machines {
		recordAvailability(u, m, previous, previous.Add(time.Hour), false)
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	old, err := u.AvailabilityForDate(c, "", "all", "2026-11-11", 2)
	current, _ := u.AvailabilityForDate(c, "", "all", "", 2)
	if err != nil || old.Counts.Minutes30 != 1 || current.Counts.Minutes30 != 1 || old.Counts.Minutes60 != 0 || current.Counts.Minutes60 != 0 {
		t.Fatalf("cross-midnight tails lost or joined: old=%+v current=%+v", old.Counts, current.Counts)
	}
	missing, err := u.AvailabilityForDate(c, "", "work", "2026-11-10", 1)
	if err != nil || missing.HistoryAvailable || missing.MachineCount != 0 || missing.CapacityPercent["minutes30"] != nil {
		t.Fatal("missing date manufactured zero/percentage history")
	}
	for _, date := range []string{"2026-11-13", "2026-11-04", "2026-1-1", "../2026-11-11"} {
		if _, err := u.AvailabilityForDate(c, "", "all", date, 1); err == nil {
			t.Fatal("invalid date accepted", date)
		}
	}
	zero := availabilityPercents(IdleWindowCounts{}, IdleWindowCounts{})
	if zero["minutes120"] != nil {
		t.Fatal("0/0 represented as a percentage")
	}
	ratio := availabilityPercents(IdleWindowCounts{Minutes30: 1}, IdleWindowCounts{Minutes30: 4})
	if ratio["minutes30"] == nil || *ratio["minutes30"] != 25 {
		t.Fatal("1/4 ratio wrong")
	}
}

func TestAvailabilityWeekLegacyRestartAndCorruptArchiveIsolation(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, chinaTime)
	u, c, start := availabilityTestStore(t, &now)
	recordAvailability(u, c.Machines[0], start, start.Add(time.Hour), false)
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	now = now.AddDate(0, 0, 1)
	reopened, err := newUtilization(filepath.Dir(u.dir), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	r, err := reopened.AvailabilityForDate(Config{}, "", "all", "2026-11-11", 1)
	if err != nil || !r.HistoryAvailable || r.Counts.Minutes60 != 1 {
		t.Fatal("legacy previous-day current file discarded")
	}
	if err := reopened.Flush(); err != nil {
		t.Fatal(err)
	}
	path := u.availabilityHistoryPath("2026-11-10")
	bad := []byte("bad gzip")
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	again, err := newUtilization(filepath.Dir(u.dir), func() time.Time { return now })
	if err != nil {
		t.Fatal("one corrupt archive blocked monitor", err)
	}
	r, err = again.AvailabilityForDate(c, "", "all", "2026-11-10", 1)
	if err != nil || r.HistoryAvailable || r.StorageError == "" {
		t.Fatal("corrupt archive hidden")
	}
	if err := again.Flush(); err != nil {
		t.Fatal("historical error blocked current flush", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(bad, after) {
		t.Fatal("corrupt evidence overwritten")
	}
}
