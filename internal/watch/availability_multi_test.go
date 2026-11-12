package watch

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"
)

func multiTestConfig(count int, pool string) Config {
	c := testConfig()
	base := c.Machines[0]
	c.Machines = nil
	for i := 0; i < count; i++ {
		m := base
		m.ID = fmt.Sprintf("node-%02d", i)
		m.ResourceID = m.ID
		m.Host = fmt.Sprintf("192.0.2.%d", i+1)
		m.Name = fmt.Sprintf("%s-%d", pool, i+1)
		m.Group = pool
		c.Machines = append(c.Machines, m)
	}
	return c
}
func assertMultiPlanFeasible(t *testing.T, r AvailabilityReport) {
	t.Helper()
	byKey := map[string]AvailabilityMachine{}
	for _, row := range r.Machines {
		byKey[row.ResourceKey] = row
	}
	for _, minutes := range []int{5, 15, 30, 60, 120} {
		key := fmt.Sprintf("minutes%d", minutes)
		perNode := map[string][]AvailabilityPlan{}
		listed := 0
		for _, p := range r.Plans[key] {
			listed += p.Count
			if p.Count < 1 || p.Minutes != minutes || len(p.Resources) != r.Nodes || p.Before.Sub(p.After) != time.Duration(minutes*p.Count)*time.Minute {
				t.Fatalf("invalid fixed-member plan: %+v", p)
			}
			seen := map[string]bool{}
			for _, resource := range p.Resources {
				row, ok := byKey[resource]
				if !ok || seen[resource] || row.Pool != p.Pool {
					t.Fatalf("duplicate resource or crossed B pool: %+v", p)
				}
				seen[resource] = true
				a, b := p.After.Sub(r.After).Milliseconds(), p.Before.Sub(r.After).Milliseconds()
				covered := false
				for _, span := range row.idleSpans {
					if span[0] <= a && span[1] >= b {
						covered = true
						break
					}
				}
				if !covered {
					t.Fatalf("members not simultaneously idle for whole plan: %+v", p)
				}
				perNode[resource] = append(perNode[resource], p)
			}
		}
		if listed+r.OmittedPlans[key] != availabilityCount(r.Counts, minutes) {
			t.Fatalf("plan count lost at limit: %s", key)
		}
		participations := 0
		for _, row := range r.Machines {
			participations += availabilityCount(row.Counts, minutes)
			if float64(availabilityCount(row.Counts, minutes)*minutes*60) > row.IdleSeconds+.001 {
				t.Fatal("node over-allocated")
			}
			plans := perNode[row.ResourceKey]
			sort.Slice(plans, func(i, j int) bool { return plans[i].After.Before(plans[j].After) })
			for i := 1; i < len(plans); i++ {
				if plans[i].After.Before(plans[i-1].Before) {
					t.Fatal("shared physical node allocated twice simultaneously")
				}
			}
		}
		if participations != r.Nodes*availabilityCount(r.Counts, minutes) {
			t.Fatal("node participations confused with independent experiments")
		}
	}
}

func TestMultiAvailabilityEightNodesIndependentPacking(t *testing.T) {
	now := time.Date(2026, 11, 12, 1, 0, 0, 0, chinaTime)
	u, _, start := availabilityTestStore(t, &now)
	c := multiTestConfig(8, "A5 B07")
	for _, m := range c.Machines {
		recordAvailability(u, m, start, start.Add(30*time.Minute), false)
	}
	before, _ := json.Marshal(u.daily)
	for _, nodes := range []int{2, 4, 8} {
		r := u.AvailabilityReport(c, "", "all", nodes)
		if r.Counts.Minutes30 != 8/nodes || r.Counts.Minutes15 != 16/nodes || r.Counts.Minutes5 != 48/nodes || r.Counts.Minutes60 != 0 || r.CountPolicy != "nonoverlapping-feasible-packing-v1" {
			t.Fatalf("8 idle nodes inflated into candidate combinations: %+v", r.Counts)
		}
		assertMultiPlanFeasible(t, r)
		for _, row := range r.Machines {
			if math.Abs(float64(row.Counts.Minutes30)*1800+row.FragmentSeconds+row.BusySeconds+row.UnknownSeconds-r.ExpectedSeconds) > .001 {
				t.Fatal("ring capacity does not conserve elapsed time")
			}
		}
	}
	after, _ := json.Marshal(u.daily)
	if string(before) != string(after) {
		t.Fatal("read-only planning rewrote history")
	}
	if r := u.AvailabilityReport(c, "", "all", 1); r.Counts.Minutes30 != 8 {
		t.Fatal("multi query changed single-node counts")
	}
}

func TestMultiAvailabilityRequiresFixedSimultaneousMembers(t *testing.T) {
	now := time.Date(2026, 11, 12, 2, 0, 0, 0, chinaTime)
	u, _, start := availabilityTestStore(t, &now)
	c := multiTestConfig(3, "A5 B07")
	recordAvailability(u, c.Machines[0], start, start.Add(time.Hour), false)
	recordAvailability(u, c.Machines[1], start, start.Add(30*time.Minute), false)
	recordAvailability(u, c.Machines[2], start.Add(30*time.Minute), start.Add(time.Hour), false)
	r := u.AvailabilityReport(c, "", "all", 2)
	if r.Counts.Minutes60 != 0 || r.Counts.Minutes30 != 2 {
		t.Fatalf("rotating partners stitched into a one-hour experiment: %+v", r.Counts)
	}
	assertMultiPlanFeasible(t, r)
	// Reverse config order must not change the deterministic feasible packing.
	c.Machines[0], c.Machines[2] = c.Machines[2], c.Machines[0]
	r2 := u.AvailabilityReport(c, "", "all", 2)
	if !reflect.DeepEqual(r.Plans, r2.Plans) {
		t.Fatal("packing depends on config iteration order")
	}
}

func TestMultiAvailabilityPoolAndPhysicalIdentityBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 12, 2, 0, 0, 0, chinaTime)
	u, _, start := availabilityTestStore(t, &now)
	c := multiTestConfig(2, "A5 B07")
	c.Machines[1].Group = "A5 B06"
	for _, m := range c.Machines {
		recordAvailability(u, m, start, start.Add(time.Hour), false)
	}
	if r := u.AvailabilityReport(c, "", "all", 2); r.Counts.Minutes5 != 0 {
		t.Fatal("B06 and B07 combined")
	}
	c = multiTestConfig(2, "A5 B07")
	c.Machines[1].ID = "alias"
	c.Machines[1].ResourceID = c.Machines[0].ResourceID
	recordAvailability(u, c.Machines[1], start, start.Add(time.Hour), false)
	if r := u.AvailabilityReport(c, "", "all", 2); r.MachineCount != 1 || r.Counts.Minutes5 != 0 {
		t.Fatal("two aliases treated as two physical machines")
	}
	c.Machines[1].Group = "A5 B06"
	if r := u.AvailabilityReport(c, "A5 B07", "all", 2); r.Machines[0].Pool != "" {
		t.Fatal("group filtering hid conflicting physical membership")
	}
	for _, pool := range []string{"", "A3 集群", "A5 B07+B06"} {
		if availabilityPoolName(pool) != "" {
			t.Fatal("unconfirmed B pool accepted", pool)
		}
	}
	c = multiTestConfig(9, "A5 B07")
	for _, m := range c.Machines {
		recordAvailability(u, m, start, start.Add(time.Hour), false)
	}
	r := u.AvailabilityReport(c, "", "all", 2)
	if r.Counts.Minutes5 != 0 || r.Pools[0].Reason != "pool_over_eight" {
		t.Fatal("over-eight pool silently combined")
	}
}

func TestMultiAvailabilityRejectsHistoricalGroupReassignment(t *testing.T) {
	now := time.Date(2026, 11, 12, 2, 0, 0, 0, chinaTime)
	u, _, start := availabilityTestStore(t, &now)
	c := multiTestConfig(2, "A5 B07")
	for _, m := range c.Machines {
		recordAvailability(u, m, start, start.Add(time.Hour), false)
	}
	c.Machines[0].Group = "A5 B06"
	c.Machines[1].Group = "A5 B06"
	r := u.AvailabilityReport(c, "", "all", 2)
	if r.Counts.Minutes5 != 0 {
		t.Fatal("old B07 history borrowed into B06")
	}
	for _, row := range r.Machines {
		if row.MultiReason != "historical_pool_changed" {
			t.Fatalf("missing historical pool warning: %+v", row)
		}
	}
	if single := u.AvailabilityReport(c, "", "all", 1); single.Counts.Minutes60 != 2 {
		t.Fatal("group reassignment destroyed single-node history")
	}
}

func TestMultiAvailabilityUnknownGapAndScopeBoundary(t *testing.T) {
	now := time.Date(2026, 11, 12, 21, 0, 0, 0, chinaTime)
	u, _, start := availabilityTestStore(t, &now)
	c := multiTestConfig(2, "A5 B07")
	recordAvailability(u, c.Machines[0], start, now, false)
	recordAvailability(u, c.Machines[1], start, start.Add(15*time.Minute), false)
	recordAvailability(u, c.Machines[1], start.Add(15*time.Minute+time.Millisecond), start.Add(30*time.Minute), false)
	r := u.AvailabilityReport(c, "", "all", 2)
	if r.Counts.Minutes30 != 0 || r.Counts.Minutes15 != 1 {
		t.Fatal("unknown gap became simultaneous continuous window")
	}
	u.daily = availabilityDisk{}
	u.resetAvailabilityDay()
	for _, m := range c.Machines {
		recordAvailability(u, m, start, now, false)
	}
	for _, tc := range []struct {
		scope string
		want  int
	}{{"work", 4}, {"off", 5}, {"all", 10}} {
		r = u.AvailabilityReport(c, "", tc.scope, 2)
		if r.Counts.Minutes120 != tc.want {
			t.Fatalf("scope %s boundary crossed: %+v", tc.scope, r.Counts)
		}
		assertMultiPlanFeasible(t, r)
	}
}

func TestMultiAvailabilityBoundedPlanDetails(t *testing.T) {
	now := time.Date(2026, 11, 12, 23, 59, 59, 0, chinaTime)
	u, _, start := availabilityTestStore(t, &now)
	c := multiTestConfig(8, "A5 B07")
	for _, m := range c.Machines {
		for i := 0; i < 144; i++ {
			at := start.Add(time.Duration(i) * 10 * time.Minute)
			recordAvailability(u, m, at, at.Add(5*time.Minute), false)
		}
	}
	r := u.AvailabilityReport(c, "", "all", 2)
	if r.Counts.Minutes5 != 576 || len(r.Plans["minutes5"]) > availabilityPlanLimit || r.OmittedPlans["minutes5"] == 0 {
		t.Fatal("detail limit changed count or was not enforced")
	}
	assertMultiPlanFeasible(t, r)
}

func TestMultiAvailabilityAPINodesValidation(t *testing.T) {
	now := time.Date(2026, 11, 12, 2, 0, 0, 0, chinaTime)
	u, _, _ := availabilityTestStore(t, &now)
	store, _ := NewStore(t.TempDir())
	c := multiTestConfig(2, "A5 B07")
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	monitor := NewMonitor(nil)
	monitor.SetUtilization(u)
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:12345")
	for _, tc := range []struct {
		query       string
		code, nodes int
	}{{"", 200, 1}, {"?nodes=2", 200, 2}, {"?nodes=4", 200, 4}, {"?nodes=8", 200, 8}, {"?nodes=3", 400, 0}, {"?nodes=-1", 400, 0}, {"?nodes=two", 400, 0}, {"?nodes=2&nodes=4", 400, 0}} {
		req := httptest.NewRequest("GET", "http://127.0.0.1:12345/api/availability"+tc.query, nil)
		req.Header.Set("X-Watch-Token", app.token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.query, w.Code, w.Body)
		}
		if tc.code == 200 {
			var r AvailabilityReport
			if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.Nodes != tc.nodes {
				t.Fatal("wrong returned node count", err)
			}
		}
	}
}
