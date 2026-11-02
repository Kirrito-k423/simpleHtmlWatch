package watch

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestUtilizationWorkingHoursBoundaries(t *testing.T) {
	want := map[int]bool{9: true, 10: true, 11: true, 14: true, 15: true, 16: true, 17: true, 19: true}
	for hour := 0; hour < 24; hour++ {
		if utilizationWorkingHour(hour) != want[hour] {
			t.Fatalf("wrong work period at Beijing hour %d", hour)
		}
	}
}

// Synthetic hourly history tests weighting and coverage, without contacting hosts.
func TestUtilizationPeriodSummariesWeightedAcrossDays(t *testing.T) {
	start := time.Date(2026, 10, 30, 0, 0, 0, 0, chinaTime) // Friday, followed by a weekend.
	before := start.AddDate(0, 0, 3)
	u, err := newUtilization(t.TempDir(), func() time.Time { return before })
	if err != nil {
		t.Fatal(err)
	}
	const hourMS int64 = 3600000
	for day := 0; day < 3; day++ {
		date := start.AddDate(0, 0, day).Format("2006-01-02")
		d := &utilizationDay{Version: 2, Date: date,
			Machines: map[string]utilizationMachine{"key": {ID: "one", Address: "host:22"}},
			Hours:    map[string][]utilizationBucket{},
		}
		// Different observed durations make an arithmetic mean incorrect.
		observed := hourMS / int64(day+1)
		for _, hour := range []int64{0, 8, 9, 11, 12, 13, 14, 17, 18, 19, 20, 23} {
			basis := int64(1000 * (day + 1))
			occObserved := observed / 2
			occupied := occObserved * int64(day) / 2
			d.Hours["key"] = append(d.Hours["key"], utilizationBucket{hour, observed, observed * basis, 0, basis, 1, occObserved, occupied})
		}
		u.days[date] = d
	}
	c := Config{Machines: []Machine{{ID: "one", Host: "host", Port: 22}, {ID: "missing", Host: "missing", Port: 22}}}
	// Deliberately request in UTC; classification must still use Beijing time.
	r := u.Report(c, start.UTC(), before.UTC(), "one", "")
	if len(r.Machines) != 1 {
		t.Fatalf("unexpected machines: %+v", r.Machines)
	}
	row := r.Machines[0]
	work, rest := row.WorkingHoursSummary, row.NonWorkingHoursSummary
	closeEnough(t, work.Mean, 180.0/11)
	closeEnough(t, rest.Mean, 180.0/11)
	closeEnough(t, work.OccupiedPercent, 350.0/11)
	closeEnough(t, rest.OccupiedPercent, 350.0/11)
	if work.ExpectedSeconds != 3*8*3600 || rest.ExpectedSeconds != 3*16*3600 {
		t.Fatalf("work/non-work denominators incorrect: %+v %+v", work, rest)
	}
	if work.ObservedSeconds != 5*6600 || rest.ObservedSeconds != 7*6600 || work.OccupancyObservedSeconds != 5*3300 || rest.OccupancyObservedSeconds != 7*3300 {
		t.Fatalf("periods mixed or observations lost: %+v %+v", work, rest)
	}
	for _, s := range []UtilizationSummary{work, rest} {
		if math.Abs(s.CoveragePercent-s.ObservedSeconds*100/s.ExpectedSeconds) > 0.001 || math.Abs(s.OccupancyCoveragePercent-s.OccupancyObservedSeconds*100/s.ExpectedSeconds) > 0.001 {
			t.Fatalf("wrong independent coverage: %+v", s)
		}
	}
	if math.Abs(work.OccupancyObservedSeconds+rest.OccupancyObservedSeconds-row.OccupancyObservedSeconds) > 0.001 {
		t.Fatal("periods do not partition the full window")
	}
	missing := u.Report(c, start, before, "missing", "").Machines[0]
	for _, s := range []UtilizationSummary{missing.WorkingHoursSummary, missing.NonWorkingHoursSummary} {
		if s.Mean != nil || s.OccupiedPercent != nil || s.CoveragePercent != 0 || s.OccupancyCoveragePercent != 0 {
			t.Fatalf("missing history became zero: %+v", s)
		}
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || fields["workingHoursSummary"] == nil || fields["nonWorkingHoursSummary"] == nil {
		t.Fatalf("summary API fields missing: %s (%v)", encoded, err)
	}
}

func TestUtilizationPeriodSummariesTodayPartialAndFuture(t *testing.T) {
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, chinaTime)
	now := start.Add(9*time.Hour + 30*time.Minute)
	u, err := newUtilization(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	date := start.Format("2006-01-02")
	u.days[date] = &utilizationDay{Version: 2, Date: date,
		Machines: map[string]utilizationMachine{"key": {ID: "one", Address: "host:22"}},
		Hours: map[string][]utilizationBucket{"key": {
			{8, 3600000, 3600000 * 2000, 0, 2000, 1, 3600000, 0},
			{9, 1800000, 1800000 * 8000, 0, 8000, 1, 900000, 900000},
		}},
	}
	c := Config{Machines: []Machine{{ID: "one", Host: "host", Port: 22}}}
	row := u.Report(c, start, start.AddDate(0, 0, 1), "", "").Machines[0]
	work, rest := row.WorkingHoursSummary, row.NonWorkingHoursSummary
	closeEnough(t, work.Mean, 80)
	closeEnough(t, rest.Mean, 20)
	closeEnough(t, work.OccupiedPercent, 100)
	closeEnough(t, rest.OccupiedPercent, 0)
	if work.ExpectedSeconds != 1800 || rest.ExpectedSeconds != 9*3600 || work.CoveragePercent != 100 || work.OccupancyCoveragePercent != 50 {
		t.Fatalf("partial current hour or future hours counted incorrectly: %+v %+v", work, rest)
	}
	if math.Abs(rest.CoveragePercent-100.0/9) > 0.001 {
		t.Fatalf("non-work coverage used full-window denominator: %+v", rest)
	}
	// Before the first work period there is no work history or elapsed work time.
	now = start.Add(8 * time.Hour)
	row = u.Report(Config{Machines: []Machine{{ID: "missing", Host: "missing", Port: 22}}}, start, start.AddDate(0, 0, 1), "missing", "").Machines[0]
	work = row.WorkingHoursSummary
	if work.ExpectedSeconds != 0 || work.Mean != nil || work.OccupiedPercent != nil || work.CoveragePercent != 0 || work.OccupancyCoveragePercent != 0 {
		t.Fatalf("future work hours treated as idle or missing elapsed time: %+v", work)
	}
}
