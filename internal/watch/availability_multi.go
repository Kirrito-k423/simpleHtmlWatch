package watch

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const availabilityMultiCapability = "simultaneous-idle-windows-v1"
const availabilityPlanLimit = 256 // Per duration; counts always include the full feasible schedule.

var availabilityBPool = regexp.MustCompile(`\bB[0-9]{2}\b`)

type AvailabilityPool struct {
	ConfiguredMembers int              `json:"configuredMembers"`
	Name              string           `json:"name"`
	Resources         []string         `json:"resources"`
	Counts            IdleWindowCounts `json:"counts"`
	Reason            string           `json:"reason,omitempty"`
}
type AvailabilityPlan struct {
	Pool      string    `json:"pool"`
	After     time.Time `json:"after"`
	Before    time.Time `json:"before"`
	Count     int       `json:"count"`
	Minutes   int       `json:"minutes"`
	Resources []string  `json:"resources"`
}

func availabilityNodes(raw string) (int, error) {
	if raw == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n != 1 && n != 2 && n != 4 && n != 8 {
		return 0, fmt.Errorf("nodes 须为 1、2、4 或 8")
	}
	return n, nil
}
func availabilityPoolName(group string) string {
	name := strings.ToUpper(strings.Join(strings.Fields(group), " "))
	if len(availabilityBPool.FindAllString(name, -1)) != 1 {
		return ""
	}
	return name
}
func availabilityCount(c IdleWindowCounts, minutes int) int {
	switch minutes {
	case 5:
		return c.Minutes5
	case 15:
		return c.Minutes15
	case 30:
		return c.Minutes30
	case 60:
		return c.Minutes60
	case 120:
		return c.Minutes120
	}
	return 0
}
func addAvailabilityCount(c *IdleWindowCounts, minutes, count int) {
	switch minutes {
	case 5:
		c.Minutes5 += count
	case 15:
		c.Minutes15 += count
	case 30:
		c.Minutes30 += count
	case 60:
		c.Minutes60 += count
	case 120:
		c.Minutes120 += count
	}
}
func intersectAvailability(a, b [][2]int64) [][2]int64 {
	out := [][2]int64{}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		start, end := max(a[i][0], b[j][0]), min(a[i][1], b[j][1])
		if end > start {
			out = append(out, [2]int64{start, end})
		}
		if a[i][1] < b[j][1] {
			i++
		} else {
			j++
		}
	}
	return out
}

type availabilityCohort struct {
	members []int
	spans   [][2]int64
	cursor  int
	key     string
}

func availabilityCohorts(rows []AvailabilityMachine, members []int, nodes int) []availabilityCohort {
	out := []availabilityCohort{}
	var choose func([]int, int)
	choose = func(selected []int, start int) {
		if len(selected) == nodes {
			spans := rows[selected[0]].idleSpans
			keys := []string{}
			for i, index := range selected {
				keys = append(keys, rows[index].ResourceKey)
				if i > 0 {
					spans = intersectAvailability(spans, rows[index].idleSpans)
				}
			}
			if len(spans) > 0 {
				out = append(out, availabilityCohort{members: append([]int(nil), selected...), spans: spans, key: strings.Join(keys, "\x00")})
			}
			return
		}
		for i := start; i <= len(members)-(nodes-len(selected)); i++ {
			choose(append(selected, members[i]), i+1)
		}
	}
	choose(nil, 0)
	return out
}

// Build one deterministic feasible packing, not a claimed global optimum.
// Every job holds the same fixed physical members for its complete duration.
// Earliest finish is chosen first, then the soonest-ending common idle span,
// then resource identity. Shared members cannot participate in overlapping jobs.
func packAvailability(rows []AvailabilityMachine, members []int, nodes, minutes int, emit func([]int, int64, int64)) int {
	cohorts := availabilityCohorts(rows, members, nodes)
	ready := make([]int64, len(rows))
	duration := int64(minutes) * 60000
	count := 0
	for {
		best := -1
		var first, deadline int64
		for i := range cohorts {
			c := &cohorts[i]
			var next int64
			for _, member := range c.members {
				next = max(next, ready[member])
			}
			for c.cursor < len(c.spans) && max(c.spans[c.cursor][0], next)+duration > c.spans[c.cursor][1] {
				c.cursor++
			}
			if c.cursor == len(c.spans) {
				continue
			}
			start, end := max(c.spans[c.cursor][0], next), c.spans[c.cursor][1]
			if best < 0 || start < first || start == first && (end < deadline || end == deadline && c.key < cohorts[best].key) {
				best, first, deadline = i, start, end
			}
		}
		if best < 0 {
			return count
		}
		c := &cohorts[best]
		for _, member := range c.members {
			ready[member] = first + duration
		}
		emit(c.members, first, first+duration)
		count++
	}
}

func planAvailability(r *AvailabilityReport) {
	r.CountPolicy = "nonoverlapping-feasible-packing-v1"
	r.Counts = IdleWindowCounts{}
	membersByPool := map[string][]int{}
	poolSizes := map[string]int{}
	for i := range r.Machines {
		row := &r.Machines[i]
		row.Counts = IdleWindowCounts{}
		if row.Pool != "" {
			poolSizes[row.Pool]++
			if membersByPool[row.Pool] == nil {
				membersByPool[row.Pool] = []int{}
			}
		}
		if row.MultiReason != "" {
			continue
		}
		if row.Pool == "" {
			row.MultiReason = "pool_unconfirmed"
			continue
		}
		if strings.HasPrefix(row.ResourceKey, "entry:") {
			row.MultiReason = "physical_identity_unconfirmed"
			continue
		}
		membersByPool[row.Pool] = append(membersByPool[row.Pool], i)
	}
	pools := []string{}
	for pool := range membersByPool {
		pools = append(pools, pool)
	}
	sort.Strings(pools)
	for _, pool := range pools {
		members := membersByPool[pool]
		sort.Slice(members, func(i, j int) bool { return r.Machines[members[i]].ResourceKey < r.Machines[members[j]].ResourceKey })
		p := AvailabilityPool{Name: pool, Resources: []string{}, ConfiguredMembers: poolSizes[pool]}
		for _, index := range members {
			p.Resources = append(p.Resources, r.Machines[index].ResourceKey)
		}
		if poolSizes[pool] > 8 {
			p.Reason = "pool_over_eight"
		} else if len(members) < r.Nodes {
			p.Reason = "not_enough_members"
		}
		if p.Reason != "" {
			for _, index := range members {
				r.Machines[index].MultiReason = p.Reason
			}
			r.Pools = append(r.Pools, p)
			continue
		}
		for _, minutes := range []int{120, 60, 30, 15, 5} {
			key := fmt.Sprintf("minutes%d", minutes)
			if r.Plans[key] == nil {
				r.Plans[key] = []AvailabilityPlan{}
			}
			lastByCohort := map[string]int{}
			count := packAvailability(r.Machines, members, r.Nodes, minutes, func(selected []int, start, end int64) {
				resources := []string{}
				for _, index := range selected {
					addAvailabilityCount(&r.Machines[index].Counts, minutes, 1)
					resources = append(resources, r.Machines[index].ResourceKey)
				}
				after, before := r.After.Add(time.Duration(start)*time.Millisecond), r.After.Add(time.Duration(end)*time.Millisecond)
				cohortKey := strings.Join(resources, "\x00")
				if previous, ok := lastByCohort[cohortKey]; ok && r.Plans[key][previous].Before.Equal(after) {
					r.Plans[key][previous].Before = before
					r.Plans[key][previous].Count++
					return
				}
				if len(r.Plans[key]) >= availabilityPlanLimit {
					r.OmittedPlans[key]++
					return
				}
				lastByCohort[cohortKey] = len(r.Plans[key])
				r.Plans[key] = append(r.Plans[key], AvailabilityPlan{Pool: pool, After: after, Before: before, Count: 1, Minutes: minutes, Resources: resources})
			})
			addAvailabilityCount(&p.Counts, minutes, count)
			addAvailabilityCount(&r.Counts, minutes, count)
		}
		r.Pools = append(r.Pools, p)
	}
	// In multi mode these are per-node participations, not independent jobs.
	// The ring remainder includes idle time that this feasible packing did not use.
	r.FragmentSeconds = 0
	for i := range r.Machines {
		row := &r.Machines[i]
		row.FragmentSeconds = row.IdleSeconds - float64(row.Counts.Minutes30)*1800
		r.FragmentSeconds += row.FragmentSeconds
	}
}
