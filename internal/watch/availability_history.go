package watch

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const availabilityPastDays = 7
const availabilityHistoryCapability = "weekly-idle-windows-v1"

// The inventory deliberately excludes SSH credentials, profiles and commands.
type availabilityMember struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Group      string `json:"group"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	ResourceID string `json:"resourceId,omitempty"`
}
type AvailabilityDate struct {
	Date      string `json:"date"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

func (u *Utilization) ensureAvailabilityHistory() {
	if u.availabilityHistory == nil {
		u.availabilityHistory = map[string]availabilityDisk{}
		u.availabilityHistoryBytes = map[string]int64{}
		u.availabilityHistoryErrors = map[string]string{}
		u.availabilityDirty = map[string]bool{}
	}
}
func (u *Utilization) availabilityHistoryPath(date string) string {
	return filepath.Join(filepath.Dir(u.availabilityPath()), "window-history", date+".json.gz")
}
func (u *Utilization) loadAvailability() error {
	u.ensureAvailabilityHistory()
	u.resetAvailabilityDay()
	for i := 1; i <= availabilityPastDays; i++ {
		date := u.availabilityToday().AddDate(0, 0, -i).Format("2006-01-02")
		disk, size, err := readAvailabilityDay(u.availabilityHistoryPath(date))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || disk.Date != date {
			u.availabilityHistoryErrors[date] = "该日期窗口历史损坏或身份不符"
			continue
		}
		u.availabilityHistory[date], u.availabilityHistoryBytes[date] = disk, size
	}
	disk, size, err := readAvailabilityDay(u.availabilityPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if disk.Date > u.daily.Date {
		return fmt.Errorf("窗口历史日期晚于当前日期，已保留原文件")
	}
	if disk.Date == u.daily.Date {
		u.daily, u.dailyBytes = disk, size
		u.availabilityInventory = append([]availabilityMember(nil), disk.Inventory...)
		u.availabilityInventoryKnown = disk.InventoryKnown
	} else if disk.Date < u.daily.Date && disk.Date >= u.availabilityToday().AddDate(0, 0, -availabilityPastDays).Format("2006-01-02") {
		// An archive written before a crash may be newer than the old current file.
		if _, exists := u.availabilityHistory[disk.Date]; !exists && u.availabilityHistoryErrors[disk.Date] == "" {
			u.availabilityHistory[disk.Date] = disk
			u.availabilityDirty[disk.Date] = true
		}
	}
	return nil
}
func (u *Utilization) SetAvailabilityInventory(config Config) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.resetAvailabilityDay()
	u.availabilityInventory = nil
	u.availabilityInventoryKnown = true
	u.daily.InventoryKnown = true
	for _, m := range config.Machines {
		if len(u.availabilityInventory) == 256 {
			u.daily.Truncated = true
			break
		}
		u.availabilityInventory = append(u.availabilityInventory, availabilityMember{m.ID, m.Name, m.Group, m.Host, m.Port, m.ResourceID})
	}
	u.daily.Inventory = append([]availabilityMember(nil), u.availabilityInventory...)
}
func (u *Utilization) flushAvailabilityHistory() error {
	u.ensureAvailabilityHistory()
	cutoff := u.availabilityToday().AddDate(0, 0, -availabilityPastDays).Format("2006-01-02")
	files, err := filepath.Glob(filepath.Join(filepath.Dir(u.availabilityHistoryPath(cutoff)), "????-??-??.json.gz"))
	if err != nil {
		return err
	}
	for _, path := range files {
		date := strings.TrimSuffix(filepath.Base(path), ".json.gz")
		if _, err := time.ParseInLocation("2006-01-02", date, chinaTime); err == nil && date < cutoff {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	for date := range u.availabilityHistory {
		if date < cutoff || date >= u.availabilityToday().Format("2006-01-02") {
			delete(u.availabilityHistory, date)
			delete(u.availabilityHistoryBytes, date)
			delete(u.availabilityDirty, date)
			continue
		}
		if !u.availabilityDirty[date] {
			continue
		}
		// Reuse the exact per-day validation and compression bounds.
		view := &Utilization{daily: u.availabilityHistory[date]}
		path := u.availabilityHistoryPath(date)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := view.saveAvailabilityDay(path); err != nil {
			return err
		}
		u.availabilityHistory[date], u.availabilityHistoryBytes[date] = view.daily, view.dailyBytes
		delete(u.availabilityDirty, date)
	}
	for date := range u.availabilityHistoryErrors {
		if date < cutoff {
			delete(u.availabilityHistoryErrors, date)
		}
	}
	return nil
}
func availabilityGroups(config Config) []string {
	seen := map[string]bool{}
	for _, m := range config.Machines {
		if m.Group != "" {
			seen[m.Group] = true
		}
	}
	groups := []string{}
	for group := range seen {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}
func availabilityHistoricalConfig(disk availabilityDisk) Config {
	config := Config{}
	if disk.InventoryKnown || disk.Inventory != nil {
		for _, m := range disk.Inventory {
			config.Machines = append(config.Machines, Machine{ID: m.ID, Name: m.Name, Group: m.Group, Host: m.Host, Port: m.Port, ResourceID: m.ResourceID})
		}
		return config
	}
	// Legacy daily ledgers have no inventory: only their recorded identities are
	// known, so never borrow today's inventory to manufacture old capacity.
	keys := make([]string, 0, len(disk.Series))
	for key := range disk.Series {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	selected := map[string]string{}
	for _, key := range keys {
		s := disk.Series[key]
		previous, ok := selected[s.Machine.ID]
		if !ok || len(s.Segments) > 0 && (len(disk.Series[previous].Segments) == 0 || s.Segments[len(s.Segments)-1][1] > disk.Series[previous].Segments[len(disk.Series[previous].Segments)-1][1]) {
			selected[s.Machine.ID] = key
		}
	}
	for _, key := range keys {
		s := disk.Series[key]
		m := s.Machine
		if selected[m.ID] != key {
			continue
		}
		host, port, err := net.SplitHostPort(m.Address)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		resource := ""
		if strings.HasPrefix(s.ResourceKey, "physical:") {
			resource = strings.TrimPrefix(s.ResourceKey, "physical:")
		}
		config.Machines = append(config.Machines, Machine{ID: m.ID, Name: m.Name, Group: m.Group, Host: host, Port: n, ResourceID: resource})
	}
	sort.Slice(config.Machines, func(i, j int) bool { return config.Machines[i].ID < config.Machines[j].ID })
	return config
}
func availabilityPercents(actual, theoretical IdleWindowCounts) map[string]*float64 {
	out := map[string]*float64{}
	for _, minutes := range []int{120, 60, 30, 15, 5} {
		key := fmt.Sprintf("minutes%d", minutes)
		out[key] = nil
		if capacity := availabilityCount(theoretical, minutes); capacity > 0 {
			value := float64(availabilityCount(actual, minutes)) * 100 / float64(capacity)
			out[key] = &value
		}
	}
	return out
}
func (u *Utilization) AvailabilityForDate(config Config, group, scope, date string, nodes int) (AvailabilityReport, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.ensureAvailabilityHistory()
	today := u.availabilityToday()
	if date == "" {
		date = today.Format("2006-01-02")
	}
	day, err := time.ParseInLocation("2006-01-02", date, chinaTime)
	if err != nil || day.Format("2006-01-02") != date || day.After(today) || day.Before(today.AddDate(0, 0, -availabilityPastDays)) {
		return AvailabilityReport{}, fmt.Errorf("date 须为今天或过去7天的 YYYY-MM-DD 日期")
	}
	var r AvailabilityReport
	if day.Equal(today) {
		r = u.availabilityReportAt(config, group, scope, nodes, u.now(), today)
	} else {
		disk, exists := u.availabilityHistory[date]
		// Queries are read-only, including when midnight has just passed.
		if u.daily.Date == date && !u.dailyBlocked {
			disk, exists = u.daily, true
		}
		view := &Utilization{daily: disk, dailyBytes: u.availabilityHistoryBytes[date], dailyError: u.availabilityHistoryErrors[date], now: u.now}
		r = view.availabilityReportAt(availabilityHistoricalConfig(disk), group, scope, nodes, day.AddDate(0, 0, 1), day)
		r.HistoryAvailable = exists && view.dailyError == ""
	}
	r.Dates = []AvailabilityDate{}
	for i := 0; i <= availabilityPastDays; i++ {
		d := today.AddDate(0, 0, -i).Format("2006-01-02")
		_, exists := u.availabilityHistory[d]
		r.Dates = append(r.Dates, AvailabilityDate{d, i == 0 || exists || u.daily.Date == d, u.availabilityHistoryErrors[d]})
	}
	return r, nil
}
