package watch

import (
	"testing"
	"time"
)

func TestAvailabilityTheoreticalCountsRespectPeriodBoundaries(t *testing.T) {
	// At 21:00 work has 3h + 4h + 2h; off has 9h + 2h + 1h.
	// In particular off's 12h cannot be stitched into six 2h windows.
	for _, tc := range []struct {
		scope string
		want  IdleWindowCounts
	}{
		{"work", IdleWindowCounts{Minutes120: 4, Minutes60: 9, Minutes30: 18, Minutes15: 36, Minutes5: 108}},
		{"off", IdleWindowCounts{Minutes120: 5, Minutes60: 12, Minutes30: 24, Minutes15: 48, Minutes5: 144}},
		{"all", IdleWindowCounts{Minutes120: 10, Minutes60: 21, Minutes30: 42, Minutes15: 84, Minutes5: 252}},
	} {
		periods, _ := availabilityPeriods(21*3600000, tc.scope)
		if got := availabilityTheoreticalCounts(periods, 1); got != tc.want {
			t.Fatalf("%s: got %+v, want %+v", tc.scope, got, tc.want)
		}
	}
}

func TestAvailabilityTheoreticalCountsUseElapsedTimeAndUniqueEligibleNodes(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 6, 0, 0, chinaTime)
	u, _, _ := availabilityTestStore(t, &now)
	c := multiTestConfig(8, "A5 B07")
	alias := c.Machines[0]
	alias.ID = "same-node-alias"
	c.Machines = append(c.Machines, alias)
	for _, nodes := range []int{1, 2, 4, 8} {
		r := u.AvailabilityReport(c, "", "work", nodes)
		// No observed history: actual is zero, theoretical includes unknown time.
		if r.Counts != (IdleWindowCounts{}) || r.TheoreticalCounts.Minutes120 != 0 || r.TheoreticalCounts.Minutes60 != 8/nodes || r.TheoreticalCounts.Minutes30 != 16/nodes || r.TheoreticalCounts.Minutes15 != 32/nodes || r.TheoreticalCounts.Minutes5 != 104/nodes {
			t.Fatalf("%d nodes: actual %+v, theoretical %+v", nodes, r.Counts, r.TheoreticalCounts)
		}
	}
	// Split two groups with 3 and 5 members: double-node capacity is 1+2,
	// four-node capacity is 0+1, eight-node capacity is zero.
	c = multiTestConfig(8, "A5 B07")
	for i := 0; i < 3; i++ {
		c.Machines[i].Group = "A5 B06"
	}
	for nodes, slots := range map[int]int{2: 3, 4: 1, 8: 0} {
		r := u.AvailabilityReport(c, "", "work", nodes)
		if r.TheoreticalCounts.Minutes60 != slots {
			t.Fatalf("cross-pool rounding for %d: %+v", nodes, r.TheoreticalCounts)
		}
	}
	if r := u.AvailabilityReport(c, "A5 B06", "work", 2); r.TheoreticalCounts.Minutes30 != 2 {
		t.Fatalf("group filter ignored: %+v", r.TheoreticalCounts)
	}
	if r := u.AvailabilityReport(multiTestConfig(9, "A5 B07"), "", "work", 2); r.TheoreticalCounts != (IdleWindowCounts{}) {
		t.Fatal("invalid over-eight pool contributed capacity")
	}
	now = time.Date(2026, 11, 12, 8, 59, 0, 0, chinaTime)
	if r := u.AvailabilityReport(c, "", "work", 1); r.TheoreticalCounts != (IdleWindowCounts{}) {
		t.Fatal("future work time contributed capacity")
	}
}
