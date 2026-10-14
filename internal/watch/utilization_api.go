package watch

import (
	"net/http"
	"sort"
	"strconv"
	"time"
)

type UtilizationHour struct {
	At                       int64    `json:"at"`
	Mean                     *float64 `json:"mean"`
	Peak                     *float64 `json:"peak"`
	LowPercent               *float64 `json:"lowPercent"`
	CoveragePercent          float64  `json:"coveragePercent"`
	ObservedSeconds          float64  `json:"observedSeconds"`
	Samples                  int64    `json:"samples"`
	OccupiedPercent          *float64 `json:"occupiedPercent"`
	OccupancyCoveragePercent float64  `json:"occupancyCoveragePercent"`
	OccupancyObservedSeconds float64  `json:"occupancyObservedSeconds"`
}
type UtilizationWindow struct {
	After  int64 `json:"after"`
	Before int64 `json:"before"`
}
type UtilizationRow struct {
	ID                       string              `json:"id"`
	Name                     string              `json:"name"`
	Group                    string              `json:"group"`
	Model                    string              `json:"model"`
	Metric                   string              `json:"metric"`
	Address                  string              `json:"address"`
	Enabled                  bool                `json:"enabled"`
	CollectionEnabled        bool                `json:"collectionEnabled"`
	Mean                     *float64            `json:"mean"`
	CoveragePercent          float64             `json:"coveragePercent"`
	OccupiedPercent          *float64            `json:"occupiedPercent"`
	OccupancyCoveragePercent float64             `json:"occupancyCoveragePercent"`
	OccupancyObservedSeconds float64             `json:"occupancyObservedSeconds"`
	Hours                    []UtilizationHour   `json:"hours"`
	HourOfDay                []UtilizationHour   `json:"hourOfDay"`
	LowWindows               []UtilizationWindow `json:"lowWindows"`
}
type UtilizationReport struct {
	Source                 string           `json:"source"`
	TimeZone               string           `json:"timeZone"`
	After                  int64            `json:"after"`
	Before                 int64            `json:"before"`
	GeneratedAt            time.Time        `json:"generatedAt"`
	LowThresholdPercent    int              `json:"lowThresholdPercent"`
	MinimumCoveragePercent int              `json:"minimumCoveragePercent"`
	RetentionDays          int              `json:"retentionDays"`
	Bytes                  int64            `json:"bytes"`
	LimitBytes             int64            `json:"limitBytes"`
	StorageError           string           `json:"storageError,omitempty"`
	OccupancySource        string           `json:"occupancySource"`
	Machines               []UtilizationRow `json:"machines"`
}

func percent(value float64) *float64 { return &value }
func utilizationHour(at int64, b utilizationBucket, expectedMS int64) UtilizationHour {
	h := UtilizationHour{At: at, ObservedSeconds: float64(b[1]) / 1000, Samples: b[5], OccupancyObservedSeconds: float64(b[6]) / 1000}
	if expectedMS > 0 {
		h.CoveragePercent = float64(b[1]) * 100 / float64(expectedMS)
		h.OccupancyCoveragePercent = float64(b[6]) * 100 / float64(expectedMS)
	}
	if b[6] > 0 {
		h.OccupiedPercent = percent(float64(b[7]) * 100 / float64(b[6]))
	}
	if b[1] > 0 {
		h.Mean = percent(float64(b[2]) / float64(b[1]) / 100)
		h.Peak = percent(float64(b[4]) / 100)
		h.LowPercent = percent(float64(b[3]) * 100 / float64(b[1]))
	}
	return h
}
func addUtilization(a *utilizationBucket, b utilizationBucket) {
	for _, i := range []int{1, 2, 3, 5, 6, 7} {
		a[i] += b[i]
	}
	if b[4] > a[4] {
		a[4] = b[4]
	}
}

// Queries read hourly aggregates; they neither contact hosts nor persist raw samples.
func (u *Utilization) Report(config Config, after, before time.Time, machineID, group string) UtilizationReport {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	report := UtilizationReport{Source: "npu-smi", TimeZone: "Asia/Shanghai", After: after.Unix(), Before: before.Unix(), GeneratedAt: now, LowThresholdPercent: 10, MinimumCoveragePercent: 90, RetentionDays: utilizationRetention, Bytes: u.bytes, LimitBytes: u.limit, StorageError: u.lastError, Machines: []UtilizationRow{}}
	report.OccupancySource = "npu-smi process table: any device with a process"
	metadata := map[string]utilizationMachine{}
	data := map[string]map[int64]utilizationBucket{}
	dates := make([]string, 0, len(u.days))
	for date := range u.days {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates {
		day := u.days[date]
		start, _ := time.ParseInLocation("2006-01-02", day.Date, chinaTime)
		if !start.Before(before) || !start.AddDate(0, 0, 1).After(after) {
			continue
		}
		for key, rows := range day.Hours {
			metadata[key] = day.Machines[key]
			if data[key] == nil {
				data[key] = map[int64]utilizationBucket{}
			}
			for _, b := range rows {
				data[key][start.Add(time.Duration(b[0])*time.Hour).Unix()] = b
			}
		}
	}
	configured := map[string]Machine{}
	for _, m := range config.Machines {
		configured[m.ID] = m
	}
	for _, m := range config.Machines {
		found := false
		for _, meta := range metadata {
			if meta.ID == m.ID && meta.Address == m.Host+":"+strconv.Itoa(m.Port) {
				found = true
				break
			}
		}
		if !found {
			metadata["configured:"+m.ID] = utilizationMachine{ID: m.ID, Name: m.Name, Group: m.Group, Address: m.Host + ":" + strconv.Itoa(m.Port)}
		}
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		meta := metadata[key]
		if machineID != "" && meta.ID != machineID || group != "" && meta.Group != group {
			continue
		}
		m := configured[meta.ID]
		currentAddress := m.Host + ":" + strconv.Itoa(m.Port)
		row := UtilizationRow{ID: meta.ID, Name: meta.Name, Group: meta.Group, Model: meta.Model, Metric: meta.Metric, Address: meta.Address, Enabled: m.Enabled && currentAddress == meta.Address, Hours: []UtilizationHour{}, HourOfDay: []UtilizationHour{}, LowWindows: []UtilizationWindow{}}
		for _, cmd := range m.Commands {
			if cmd == "npu" && row.Enabled {
				row.CollectionEnabled = true
			}
		}
		var total utilizationBucket
		var expectedTotal int64
		var hourSums [24]utilizationBucket
		var expectedHours [24]int64
		var window *UtilizationWindow
		for at := after; at.Before(before); at = at.Add(time.Hour) {
			expected := int64(0)
			end := at.Add(time.Hour)
			if end.After(now) {
				end = now
			}
			if end.After(at) {
				expected = end.Sub(at).Milliseconds()
			}
			b := data[key][at.Unix()]
			h := utilizationHour(at.Unix(), b, expected)
			row.Hours = append(row.Hours, h)
			addUtilization(&total, b)
			expectedTotal += expected
			hour := at.In(chinaTime).Hour()
			addUtilization(&hourSums[hour], b)
			expectedHours[hour] += expected
			// A candidate requires a completed hour with >=90% coverage and
			// >=90% of observed time below 10% MACHINE mean utilization.
			low := !at.Add(time.Hour).After(now) && h.CoveragePercent >= 90 && h.LowPercent != nil && *h.LowPercent >= 90
			if low {
				if window == nil {
					window = &UtilizationWindow{After: at.Unix()}
				}
				window.Before = at.Add(time.Hour).Unix()
			} else if window != nil {
				row.LowWindows = append(row.LowWindows, *window)
				window = nil
			}
		}
		if window != nil {
			row.LowWindows = append(row.LowWindows, *window)
		}
		summary := utilizationHour(0, total, expectedTotal)
		row.Mean, row.CoveragePercent = summary.Mean, summary.CoveragePercent
		row.OccupiedPercent, row.OccupancyCoveragePercent, row.OccupancyObservedSeconds = summary.OccupiedPercent, summary.OccupancyCoveragePercent, summary.OccupancyObservedSeconds
		for hour := 0; hour < 24; hour++ {
			row.HourOfDay = append(row.HourOfDay, utilizationHour(int64(hour), hourSums[hour], expectedHours[hour]))
		}
		report.Machines = append(report.Machines, row)
	}
	sort.Slice(report.Machines, func(i, j int) bool {
		a, b := report.Machines[i], report.Machines[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Address+a.Metric < b.Address+b.Metric
	})
	return report
}

func (s *Server) utilizationAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	u := s.monitor.utilization
	if u == nil {
		fail(w, 503, "利用率存储未初始化")
		return
	}
	q := r.URL.Query()
	now := u.now().In(chinaTime)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, chinaTime)
	after, days := today.AddDate(0, 0, -1), 1
	if q.Get("date") != "" {
		if q.Get("period") != "" {
			fail(w, 400, "date 与 period 不能同时指定")
			return
		}
		var err error
		after, err = time.ParseInLocation("2006-01-02", q.Get("date"), chinaTime)
		if err != nil {
			fail(w, 400, "日期格式须为 YYYY-MM-DD")
			return
		}
		if q.Get("days") != "" {
			days, err = strconv.Atoi(q.Get("days"))
			if err != nil {
				days = 0
			}
		}
	} else {
		if q.Get("days") != "" {
			fail(w, 400, "days 须与 date 一起提供")
			return
		}
		switch q.Get("period") {
		case "", "yesterday":
		case "today":
			after = today
		case "week":
			after, days = today.AddDate(0, 0, -7), 7
		default:
			fail(w, 400, "范围须为 today、yesterday 或 week")
			return
		}
	}
	if days < 1 || days > 30 || after.After(today) || after.Before(today.AddDate(0, 0, 1-utilizationRetention)) || after.AddDate(0, 0, days).After(today.AddDate(0, 0, 1)) {
		fail(w, 400, "查询须位于含今天的最近 30 个自然日")
		return
	}
	if machine := q.Get("machineId"); machine != "" && !identifier.MatchString(machine) {
		fail(w, 400, "机器 ID 无效")
		return
	}
	jsonResponse(w, 200, u.Report(s.store.Public(), after, after.AddDate(0, 0, days), q.Get("machineId"), q.Get("group")))
}
