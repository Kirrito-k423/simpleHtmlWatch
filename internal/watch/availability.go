package watch

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const availabilityCapability = "daily-idle-windows-v1"
const availabilityLimit = 256 * 1024
const availabilityRawLimit = 2 * 1024 * 1024
const availabilitySegmentLimit = 8192
const availabilityDayMS int64 = 86400000

// Offsets in milliseconds from Beijing midnight, end exclusive; state is
// 0=observed idle, 1=observed busy, 2=unknown after a clock overlap.
// Adjacent equal states are merged. No PID, command or raw samples are stored.
type availabilitySegment [3]int64
type availabilitySeries struct {
	ResourceKey string                `json:"resourceKey"`
	Machine     utilizationMachine    `json:"machine"`
	Segments    []availabilitySegment `json:"segments"`
}
type availabilityDisk struct {
	Inventory      []availabilityMember           `json:"inventory,omitempty"`
	InventoryKnown bool                           `json:"inventoryKnown,omitempty"`
	Version        int                            `json:"version"`
	Date           string                         `json:"date"`
	Truncated      bool                           `json:"truncated"`
	Series         map[string]*availabilitySeries `json:"series"`
}

func (u *Utilization) availabilityPath() string {
	return filepath.Join(filepath.Dir(u.dir), "occupancy", "daily-windows.json.gz")
}
func (u *Utilization) availabilityToday() time.Time {
	now := u.now().In(chinaTime)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, chinaTime)
}
func (u *Utilization) resetAvailabilityDay() {
	date := u.availabilityToday().Format("2006-01-02")
	if u.daily.Date != date || u.daily.Series == nil {
		if u.daily.Date != "" && u.daily.Date != date && !u.dailyBlocked {
			u.ensureAvailabilityHistory()
			u.availabilityHistory[u.daily.Date] = u.daily
			u.availabilityDirty[u.daily.Date] = true
		}
		u.daily = availabilityDisk{Version: 1, Date: date, Series: map[string]*availabilitySeries{}, Inventory: append([]availabilityMember(nil), u.availabilityInventory...), InventoryKnown: u.availabilityInventoryKnown}
	}
}
func readAvailabilityDay(path string) (availabilityDisk, int64, error) {
	var result availabilityDisk
	info, err := os.Lstat(path)
	if err != nil {
		return result, 0, err
	}
	if !info.Mode().IsRegular() {
		return result, 0, fmt.Errorf("窗口历史不是普通文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return result, 0, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || info.Size() > availabilityLimit {
		return result, 0, fmt.Errorf("全天窗口历史超过256 KiB或无法读取")
	}
	z, err := gzip.NewReader(f)
	if err != nil {
		return result, 0, fmt.Errorf("全天窗口历史压缩文件损坏")
	}
	defer z.Close()
	raw, err := io.ReadAll(io.LimitReader(z, availabilityRawLimit+1))
	if err != nil || len(raw) > availabilityRawLimit {
		return result, 0, fmt.Errorf("全天窗口历史解压失败或过大")
	}
	// Decode variable-width arrays first so omitted state cannot become idle.
	var disk struct {
		Inventory      []availabilityMember `json:"inventory"`
		InventoryKnown bool                 `json:"inventoryKnown"`
		Version        int                  `json:"version"`
		Date           string               `json:"date"`
		Truncated      bool                 `json:"truncated"`
		Series         map[string]*struct {
			ResourceKey string             `json:"resourceKey"`
			Machine     utilizationMachine `json:"machine"`
			Segments    [][]int64          `json:"segments"`
		} `json:"series"`
	}
	if json.Unmarshal(raw, &disk) != nil || disk.Version != 1 || disk.Series == nil || len(disk.Series) > 256 {
		return result, 0, fmt.Errorf("全天窗口历史格式无效")
	}
	if _, err := time.ParseInLocation("2006-01-02", disk.Date, chinaTime); err != nil {
		return result, 0, fmt.Errorf("全天窗口历史日期无效")
	}
	result = availabilityDisk{Version: 1, Date: disk.Date, Truncated: disk.Truncated, Series: map[string]*availabilitySeries{}, Inventory: disk.Inventory, InventoryKnown: disk.InventoryKnown}
	if len(disk.Inventory) > 256 {
		return result, 0, fmt.Errorf("窗口节点清单过大")
	}
	seenInventory := map[string]bool{}
	for _, member := range disk.Inventory {
		if member.ID == "" || member.Host == "" || member.Port < 1 || member.Port > 65535 || seenInventory[member.ID] {
			return result, 0, fmt.Errorf("窗口历史节点清单身份无效")
		}
		seenInventory[member.ID] = true
	}
	count := 0
	for key, s := range disk.Series {
		if s == nil || s.ResourceKey == "" || s.Machine.ID == "" || s.Machine.Model == "" || s.Machine.Metric == "" || s.Machine.Address == "" || key != recentSeriesKey(s.Machine, s.ResourceKey) {
			return result, 0, fmt.Errorf("全天窗口机器身份无效")
		}
		row := &availabilitySeries{ResourceKey: s.ResourceKey, Machine: s.Machine}
		var previous int64
		for _, seg := range s.Segments {
			count++
			if count > availabilitySegmentLimit || len(seg) != 3 || seg[0] < previous || seg[1] <= seg[0] || seg[1] > availabilityDayMS || seg[2] < 0 || seg[2] > 2 {
				return result, 0, fmt.Errorf("全天窗口时间段无效")
			}
			row.Segments = append(row.Segments, availabilitySegment{seg[0], seg[1], seg[2]})
			previous = seg[1]
		}
		result.Series[key] = row
	}
	return result, info.Size(), nil
}

func (u *Utilization) advanceAvailability(p utilizationPending, start, end time.Time) {
	if !p.occupancyKnown || !end.After(start) {
		return
	}
	u.resetAvailabilityDay()
	if u.dailyBlocked {
		return
	}
	// Split even a bounded observation crossing Beijing midnight into each day.
	for at := start; at.Before(end); {
		local := at.In(chinaTime)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, chinaTime)
		next := minTime(end, day.AddDate(0, 0, 1))
		date := day.Format("2006-01-02")
		if date == u.daily.Date {
			u.advanceAvailabilityDay(p, at, next, day)
		} else if !day.Before(u.availabilityToday().AddDate(0, 0, -availabilityPastDays)) && day.Before(u.availabilityToday()) {
			saved := u.daily
			disk, ok := u.availabilityHistory[date]
			if !ok {
				disk = availabilityDisk{Version: 1, Date: date, Series: map[string]*availabilitySeries{}, Inventory: append([]availabilityMember(nil), u.availabilityInventory...), InventoryKnown: u.availabilityInventoryKnown}
			}
			if u.availabilityHistoryErrors[date] == "" {
				u.daily = disk
				u.advanceAvailabilityDay(p, at, next, day)
				u.availabilityHistory[date] = u.daily
				u.availabilityDirty[date] = true
				u.daily = saved
			}
		}
		at = next
	}
}
func (u *Utilization) advanceAvailabilityDay(p utilizationPending, start, end, midnight time.Time) {
	a, b := max(start.UnixMilli()-midnight.UnixMilli(), 0), min(end.UnixMilli()-midnight.UnixMilli(), availabilityDayMS)
	if b <= a {
		return
	}
	key := recentSeriesKey(p.machine, p.resourceKey)
	s := u.daily.Series[key]
	if s == nil {
		if len(u.daily.Series) >= 256 {
			u.daily.Truncated = true
			return
		}
		s = &availabilitySeries{ResourceKey: p.resourceKey, Machine: p.machine}
		u.daily.Series[key] = s
	}
	state := int64(0)
	if p.occupied || p.basis != 0 {
		state = 1
	}
	if n := len(s.Segments); n > 0 {
		last := &s.Segments[n-1]
		if a < last[1] {
			// Never turn overlapping clock generations into continuous idle.
			b, state = max(b, last[1]), 2
			first := sort.Search(n, func(i int) bool { return s.Segments[i][1] > a })
			if first < n && s.Segments[first][0] < a {
				s.Segments[first][1] = a
				first++
			}
			s.Segments = s.Segments[:first]
		} else if a == last[1] && state == last[2] {
			last[1] = b
			return
		}
	}
	s.Segments = append(s.Segments, availabilitySegment{a, b, state})
	u.boundAvailabilitySegments(availabilitySegmentLimit)
}

func (u *Utilization) boundAvailabilitySegments(limit int) {
	count := 0
	for _, s := range u.daily.Series {
		count += len(s.Segments)
	}
	for count > limit {
		var oldest *availabilitySeries
		for _, s := range u.daily.Series {
			if len(s.Segments) > 0 && (oldest == nil || s.Segments[0][1] < oldest.Segments[0][1]) {
				oldest = s
			}
		}
		if oldest == nil {
			break
		}
		oldest.Segments = oldest.Segments[1:]
		u.daily.Truncated = true
		count--
	}
}

func (u *Utilization) flushAvailabilityLocked() error {
	if u.dailyBlocked {
		return fmt.Errorf("%s", u.dailyError)
	}
	u.resetAvailabilityDay()
	if err := u.flushAvailabilityHistory(); err != nil {
		return err
	}
	return u.saveAvailabilityDay(u.availabilityPath())
}
func (u *Utilization) saveAvailabilityDay(path string) error {
	for {
		raw, err := json.Marshal(u.daily)
		if err != nil || len(raw) > availabilityRawLimit {
			return fmt.Errorf("全天窗口历史超过内存格式上限")
		}
		var packed bytes.Buffer
		z := gzip.NewWriter(&packed)
		if _, err = z.Write(raw); err != nil {
			return err
		}
		if err = z.Close(); err != nil {
			return err
		}
		if packed.Len() <= availabilityLimit {
			if err = writeAtomic(path, packed.Bytes()); err != nil {
				return err
			}
			u.dailyBytes = int64(packed.Len())
			return nil
		}
		count := 0
		for _, s := range u.daily.Series {
			count += len(s.Segments)
		}
		if count == 0 {
			return fmt.Errorf("全天窗口机器元数据超过256 KiB上限")
		}
		u.boundAvailabilitySegments(count / 2)
	}
}

type IdleWindowCounts struct {
	Minutes5   int `json:"minutes5"`
	Minutes15  int `json:"minutes15"`
	Minutes30  int `json:"minutes30"`
	Minutes60  int `json:"minutes60"`
	Minutes120 int `json:"minutes120"`
}
type AvailabilityMachine struct {
	Pool               string `json:"pool"`
	MultiReason        string `json:"multiReason,omitempty"`
	idleSpans          [][2]int64
	ResourceKey        string           `json:"resourceKey"`
	MachineIDs         []string         `json:"machineIds"`
	Name               string           `json:"name"`
	Group              string           `json:"group"`
	Counts             IdleWindowCounts `json:"counts"`
	IdleSeconds        float64          `json:"idleSeconds"`
	BusySeconds        float64          `json:"busySeconds"`
	UnknownSeconds     float64          `json:"unknownSeconds"`
	FragmentSeconds    float64          `json:"fragmentSeconds"`
	CoveragePercent    float64          `json:"coveragePercent"`
	LongestIdleSeconds float64          `json:"longestIdleSeconds"`
}
type AvailabilityReport struct {
	Date              string                        `json:"date"`
	Today             string                        `json:"today"`
	Dates             []AvailabilityDate            `json:"dates"`
	Groups            []string                      `json:"groups"`
	HistoryAvailable  bool                          `json:"historyAvailable"`
	CapacityPercent   map[string]*float64           `json:"capacityPercent"`
	Nodes             int                           `json:"nodes"`
	CountPolicy       string                        `json:"countPolicy"`
	Pools             []AvailabilityPool            `json:"pools"`
	Plans             map[string][]AvailabilityPlan `json:"plans"`
	OmittedPlans      map[string]int                `json:"omittedPlans"`
	Scope             string                        `json:"scope"`
	ExpectedSeconds   float64                       `json:"expectedSeconds"`
	Schema            string                        `json:"schema"`
	TimeZone          string                        `json:"timeZone"`
	After             time.Time                     `json:"after"`
	Before            time.Time                     `json:"before"`
	GeneratedAt       time.Time                     `json:"generatedAt"`
	Counts            IdleWindowCounts              `json:"counts"`
	TheoreticalCounts IdleWindowCounts              `json:"theoreticalCounts"`
	MachineCount      int                           `json:"machineCount"`
	ConfiguredEntries int                           `json:"configuredEntries"`
	IdleSeconds       float64                       `json:"idleSeconds"`
	BusySeconds       float64                       `json:"busySeconds"`
	UnknownSeconds    float64                       `json:"unknownSeconds"`
	FragmentSeconds   float64                       `json:"fragmentSeconds"`
	CoveragePercent   float64                       `json:"coveragePercent"`
	HistoryBytes      int64                         `json:"historyBytes"`
	HistoryLimitBytes int                           `json:"historyLimitBytes"`
	HistoryTruncated  bool                          `json:"historyTruncated"`
	StorageError      string                        `json:"storageError,omitempty"`
	Machines          []AvailabilityMachine         `json:"machines"`
}
type availabilityEvent struct {
	at         int64
	free, busy int
}

func availabilityResource(m Machine) string {
	if m.ResourceID != "" {
		return machineResource(m)
	}
	if host, loopback := resourceHost(m.Host); loopback {
		// Unidentified forwarding ports must not be collapsed into one node.
		return fmt.Sprintf("entry:%s:%d", host, m.Port)
	}
	return machineResource(m)
}

// Sweep all explicit aliases of a physical resource. Known busy dominates an
// idle reading from another alias; overlapping observations count only once.
func summarizeAvailability(events []availabilityEvent, elapsed, expected int64) AvailabilityMachine {
	row := AvailabilityMachine{}
	events = append(events, availabilityEvent{at: 0}, availabilityEvent{at: elapsed})
	sort.Slice(events, func(i, j int) bool { return events[i].at < events[j].at })
	var free, busy int
	var previous, run, idleMS, busyMS, longest int64
	finish := func() {
		if run > 0 {
			row.idleSpans = append(row.idleSpans, [2]int64{previous - run, previous})
		}
		row.Counts.Minutes5 += int(run / 300000)
		row.Counts.Minutes15 += int(run / 900000)
		row.Counts.Minutes30 += int(run / 1800000)
		row.Counts.Minutes60 += int(run / 3600000)
		row.Counts.Minutes120 += int(run / 7200000)
		longest = max(longest, run)
		run = 0
	}
	for i := 0; i < len(events); {
		at := events[i].at
		duration := at - previous
		if busy > 0 {
			busyMS += duration
			finish()
		} else if free > 0 {
			idleMS += duration
			run += duration
		} else {
			finish()
		}
		for i < len(events) && events[i].at == at {
			free += events[i].free
			busy += events[i].busy
			i++
		}
		previous = at
	}
	finish()
	row.IdleSeconds, row.BusySeconds = float64(idleMS)/1000, float64(busyMS)/1000
	row.UnknownSeconds = float64(max(expected-idleMS-busyMS, 0)) / 1000
	row.LongestIdleSeconds = float64(longest) / 1000
	row.FragmentSeconds = row.IdleSeconds - float64(row.Counts.Minutes30)*1800
	if expected > 0 {
		row.CoveragePercent = float64(idleMS+busyMS) * 100 / float64(expected)
	}
	return row
}

func availabilityPeriods(elapsed int64, scope string) ([][2]int64, int64) {
	periods := [][2]int64{}
	var expected int64
	for hour := 0; hour < 24; hour++ {
		work := utilizationWorkingHour(hour)
		if scope == "work" && !work || scope == "off" && work {
			continue
		}
		a, b := int64(hour)*3600000, min(int64(hour+1)*3600000, elapsed)
		if b <= a {
			continue
		}
		expected += b - a
		if n := len(periods); n > 0 && periods[n-1][1] == a {
			periods[n-1][1] = b
		} else {
			periods = append(periods, [2]int64{a, b})
		}
	}
	return periods, expected
}

// Capacity if all eligible physical nodes were idle throughout the elapsed
// selected periods. Gaps between periods cannot be joined into longer windows.
func availabilityTheoreticalCounts(periods [][2]int64, slots int) IdleWindowCounts {
	counts := IdleWindowCounts{}
	for _, period := range periods {
		for _, minutes := range []int{120, 60, 30, 15, 5} {
			addAvailabilityCount(&counts, minutes, int((period[1]-period[0])/(int64(minutes)*60000))*slots)
		}
	}
	return counts
}

func (u *Utilization) AvailabilityReport(config Config, group, scope string, nodeOptions ...int) AvailabilityReport {
	u.mu.Lock()
	defer u.mu.Unlock()
	nodes := 1
	if len(nodeOptions) > 0 {
		nodes = nodeOptions[0]
	}
	return u.availabilityReportAt(config, group, scope, nodes, u.now(), u.availabilityToday())
}
func (u *Utilization) availabilityReportAt(config Config, group, scope string, nodes int, now, after time.Time) AvailabilityReport {
	elapsed := max(now.UnixMilli()-after.UnixMilli(), 0)
	periods, expected := availabilityPeriods(elapsed, scope)
	r := AvailabilityReport{Schema: "simplehtmlwatch.daily-idle-windows.v1", TimeZone: "Asia/Shanghai", After: after, Before: now, GeneratedAt: u.now(), HistoryBytes: u.dailyBytes, HistoryLimitBytes: availabilityLimit, HistoryTruncated: u.daily.Date == after.Format("2006-01-02") && u.daily.Truncated, StorageError: u.dailyError, Machines: []AvailabilityMachine{}}
	r.Scope, r.ExpectedSeconds = scope, float64(expected)/1000
	r.Nodes = nodes
	r.Date, r.Today, r.HistoryAvailable = after.Format("2006-01-02"), u.availabilityToday().Format("2006-01-02"), true
	r.Groups = availabilityGroups(config)
	r.CountPolicy = "independent-single-node"
	r.Pools = []AvailabilityPool{}
	r.Plans = map[string][]AvailabilityPlan{}
	r.OmittedPlans = map[string]int{}
	poolByResource := map[string]string{}
	for _, m := range config.Machines {
		key, pool := availabilityResource(m), availabilityPoolName(m.Group)
		if previous, ok := poolByResource[key]; ok && previous != pool {
			poolByResource[key] = ""
		} else if !ok {
			poolByResource[key] = pool
		}
	}
	groups := map[string][]Machine{}
	for _, m := range config.Machines {
		if group != "" && m.Group != group {
			continue
		}
		r.ConfiguredEntries++
		key := availabilityResource(m)
		groups[key] = append(groups[key], m)
	}
	for resource, aliases := range groups {
		sort.Slice(aliases, func(i, j int) bool { return aliases[i].ID < aliases[j].ID })
		events := []availabilityEvent{}
		historicalPoolChanged := false
		for _, m := range aliases {
			if u.daily.Date != after.Format("2006-01-02") {
				continue
			}
			var selected *availabilitySeries
			var selectedKey string
			for key, candidate := range u.daily.Series {
				if candidate.ResourceKey != machineResource(m) || candidate.Machine.ID != m.ID || candidate.Machine.Address != fmt.Sprintf("%s:%d", m.Host, m.Port) || len(candidate.Segments) == 0 {
					continue
				}
				if selected == nil || candidate.Segments[len(candidate.Segments)-1][1] > selected.Segments[len(selected.Segments)-1][1] || candidate.Segments[len(candidate.Segments)-1][1] == selected.Segments[len(selected.Segments)-1][1] && key < selectedKey {
					selected, selectedKey = candidate, key
				}
			}
			for _, s := range []*availabilitySeries{selected} {
				if s == nil {
					continue
				}
				if availabilityPoolName(s.Machine.Group) != poolByResource[resource] {
					historicalPoolChanged = true
				}
				for _, seg := range s.Segments {
					for _, period := range periods {
						a, b := max(seg[0], period[0]), min(seg[1], period[1])
						if b <= a || seg[2] == 2 {
							continue
						}
						first, last := availabilityEvent{at: a}, availabilityEvent{at: b}
						if seg[2] == 0 {
							first.free, last.free = 1, -1
						} else {
							first.busy, last.busy = 1, -1
						}
						events = append(events, first, last)
					}
				}
			}
		}
		row := summarizeAvailability(events, elapsed, expected)
		row.ResourceKey, row.Name, row.Group = resource, aliases[0].Name, aliases[0].Group
		row.Pool = poolByResource[resource]
		if r.Nodes > 1 && historicalPoolChanged {
			row.MultiReason = "historical_pool_changed"
		}
		row.MachineIDs = []string{}
		for _, m := range aliases {
			row.MachineIDs = append(row.MachineIDs, m.ID)
		}
		if row.Name == "" {
			row.Name = aliases[0].ID
		}
		r.Machines = append(r.Machines, row)
		r.Counts.Minutes15 += row.Counts.Minutes15
		r.Counts.Minutes5 += row.Counts.Minutes5
		r.Counts.Minutes30 += row.Counts.Minutes30
		r.Counts.Minutes60 += row.Counts.Minutes60
		r.Counts.Minutes120 += row.Counts.Minutes120
		r.IdleSeconds += row.IdleSeconds
		r.BusySeconds += row.BusySeconds
		r.UnknownSeconds += row.UnknownSeconds
		r.FragmentSeconds += row.FragmentSeconds
	}
	sort.Slice(r.Machines, func(i, j int) bool {
		a, b := r.Machines[i], r.Machines[j]
		if a.Counts.Minutes30 != b.Counts.Minutes30 {
			return a.Counts.Minutes30 < b.Counts.Minutes30
		}
		return a.ResourceKey < b.ResourceKey
	})
	r.MachineCount = len(r.Machines)
	if expected > 0 && r.MachineCount > 0 {
		r.CoveragePercent = (r.IdleSeconds + r.BusySeconds) * 100 / (float64(expected) / 1000 * float64(r.MachineCount))
	}
	if r.Nodes != 1 {
		planAvailability(&r)
	}
	slots := r.MachineCount
	if r.Nodes > 1 {
		slots = 0
		for _, pool := range r.Pools {
			if pool.Reason == "" {
				slots += len(pool.Resources) / r.Nodes
			}
		}
	}
	r.TheoreticalCounts = availabilityTheoreticalCounts(periods, slots)
	r.CapacityPercent = availabilityPercents(r.Counts, r.TheoreticalCounts)
	return r
}

func (s *Server) availabilityAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	for key, values := range r.URL.Query() {
		if key != "group" && key != "scope" && key != "nodes" && key != "date" || len(values) != 1 {
			fail(w, 400, "窗口统计仅接受单个 group、scope、nodes 和 date 参数")
			return
		}
	}
	if s.monitor.utilization == nil {
		fail(w, 503, "窗口历史尚未初始化")
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "work"
	}
	if scope != "work" && scope != "off" && scope != "all" {
		fail(w, 400, "scope 须为 work、off 或 all")
		return
	}
	nodes, err := availabilityNodes(r.URL.Query().Get("nodes"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	report, err := s.monitor.utilization.AvailabilityForDate(s.store.Public(), r.URL.Query().Get("group"), scope, r.URL.Query().Get("date"), nodes)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	jsonResponse(w, 200, report)
}
