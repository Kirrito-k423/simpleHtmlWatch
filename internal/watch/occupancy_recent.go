package watch

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const recentOccupancyStep = 5 * time.Minute
const recentOccupancyRetention = 6 * time.Hour
const recentOccupancyLimit = 64 * 1024
const recentOccupancyRawLimit = 2 * 1024 * 1024

// Bucket start Unix seconds, known process-state ms, occupied ms.
type occupancyBucket [3]int64
type occupancySeries struct {
	ResourceKey string             `json:"resourceKey"`
	Machine     utilizationMachine `json:"machine"`
	Buckets     []occupancyBucket  `json:"buckets"`
}

func (s *occupancySeries) UnmarshalJSON(raw []byte) error {
	var disk struct {
		ResourceKey string             `json:"resourceKey"`
		Machine     utilizationMachine `json:"machine"`
		Buckets     [][]int64          `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		return err
	}
	if len(disk.Buckets) > 73 {
		return fmt.Errorf("5分钟占用桶数量无效")
	}
	s.ResourceKey, s.Machine = disk.ResourceKey, disk.Machine
	for _, row := range disk.Buckets {
		if len(row) != 3 {
			return fmt.Errorf("5分钟占用桶字段数无效")
		}
		s.Buckets = append(s.Buckets, occupancyBucket{row[0], row[1], row[2]})
	}
	return nil
}

type occupancyDisk struct {
	Version int                         `json:"version"`
	Series  map[string]*occupancySeries `json:"series"`
}

func (u *Utilization) recentPath() string {
	return filepath.Join(filepath.Dir(u.dir), "occupancy", "recent.json.gz")
}

func (u *Utilization) loadRecent() error {
	u.recent = map[string]*occupancySeries{}
	if err := os.MkdirAll(filepath.Dir(u.recentPath()), 0700); err != nil {
		return err
	}
	scratch, err := filepath.Glob(filepath.Join(filepath.Dir(u.recentPath()), ".watch-*"))
	if err != nil {
		return err
	}
	for _, file := range scratch {
		if err := os.Remove(file); err != nil {
			return err
		}
	}
	f, err := os.Open(u.recentPath())
	if os.IsNotExist(err) {
		return nil // Hourly history cannot reconstruct five-minute occupancy.
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > recentOccupancyLimit {
		return fmt.Errorf("5分钟占用文件超过容量上限")
	}
	z, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("5分钟占用文件无效：%w", err)
	}
	defer z.Close()
	raw, err := io.ReadAll(io.LimitReader(z, recentOccupancyRawLimit+1))
	if err != nil || len(raw) > recentOccupancyRawLimit {
		return fmt.Errorf("5分钟占用文件解压失败或过大")
	}
	var disk occupancyDisk
	if json.Unmarshal(raw, &disk) != nil || disk.Version != 1 || disk.Series == nil || len(disk.Series) > 256 {
		return fmt.Errorf("5分钟占用文件格式无效")
	}
	for key, series := range disk.Series {
		if series == nil || key != recentSeriesKey(series.Machine, series.ResourceKey) || series.ResourceKey == "" || series.Machine.ID == "" || series.Machine.Model == "" || series.Machine.Address == "" || series.Machine.Metric == "" || len(series.Buckets) > 73 {
			return fmt.Errorf("5分钟占用机器身份无效")
		}
		var previous int64
		for _, b := range series.Buckets {
			if b[0] <= previous || b[0]%300 != 0 || b[0] > u.now().Add(recentOccupancyStep).Unix() || b[1] < 0 || b[1] > 300000 || b[2] < 0 || b[2] > b[1] {
				return fmt.Errorf("5分钟占用桶无效")
			}
			previous = b[0]
		}
	}
	u.recent, u.recentBytes = disk.Series, info.Size()
	u.pruneRecentLocked()
	return nil
}

func (u *Utilization) pruneRecentLocked() {
	cutoff := u.now().Add(-recentOccupancyRetention).Truncate(recentOccupancyStep).Unix()
	for key, series := range u.recent {
		first := sort.Search(len(series.Buckets), func(i int) bool { return series.Buckets[i][0] >= cutoff })
		series.Buckets = series.Buckets[first:]
		if len(series.Buckets) == 0 {
			delete(u.recent, key)
		}
	}
}

// Called only for the exact interval integrated into the hourly statistics.
// Failed/unknown process tables and the bounded hold cap therefore stay unknown.
func (u *Utilization) advanceRecent(p utilizationPending, start, end time.Time) {
	if !p.occupancyKnown || !end.After(start) {
		return
	}
	cutoff := u.now().Add(-recentOccupancyRetention).Truncate(recentOccupancyStep)
	if start.Before(cutoff) {
		start = cutoff
	}
	if !end.After(start) {
		return
	}
	key := recentSeriesKey(p.machine, p.resourceKey)
	series := u.recent[key]
	if series == nil {
		if len(u.recent) >= 256 {
			u.pruneRecentLocked()
			if len(u.recent) >= 256 {
				return
			}
		}
		series = &occupancySeries{Machine: p.machine, ResourceKey: p.resourceKey}
		u.recent[key] = series
	}
	for start.Before(end) {
		at := start.Truncate(recentOccupancyStep)
		next := at.Add(recentOccupancyStep)
		if next.After(end) {
			next = end
		}
		index := sort.Search(len(series.Buckets), func(i int) bool { return series.Buckets[i][0] >= at.Unix() })
		if index == len(series.Buckets) || series.Buckets[index][0] != at.Unix() {
			series.Buckets = append(series.Buckets, occupancyBucket{})
			copy(series.Buckets[index+1:], series.Buckets[index:])
			series.Buckets[index] = occupancyBucket{at.Unix(), 0, 0}
		}
		b := &series.Buckets[index]
		ms := next.Sub(start).Milliseconds()
		// A local wall-clock rollback must not produce >100% coverage.
		if ms > 300000-b[1] {
			ms = 300000 - b[1]
		}
		b[1] += ms
		if p.occupied {
			b[2] += ms
		}
		start = next
	}
	u.pruneRecentLocked()
}

func recentSeriesKey(meta utilizationMachine, resourceKey string) string {
	hash := sha256.Sum256([]byte(utilizationKey(meta) + "\x00" + resourceKey))
	return hex.EncodeToString(hash[:12])
}

func (u *Utilization) flushRecentLocked() error {
	u.pruneRecentLocked()
	raw, err := json.Marshal(occupancyDisk{1, u.recent})
	if err != nil {
		return err
	}
	if len(raw) > recentOccupancyRawLimit {
		return fmt.Errorf("5分钟占用数据超过容量上限")
	}
	var packed bytes.Buffer
	z := gzip.NewWriter(&packed)
	if _, err = z.Write(raw); err != nil {
		return err
	}
	if err = z.Close(); err != nil {
		return err
	}
	if packed.Len() > recentOccupancyLimit {
		return fmt.Errorf("5分钟占用压缩文件超过64 KiB上限")
	}
	if err = os.MkdirAll(filepath.Dir(u.recentPath()), 0700); err != nil {
		return err
	}
	if err = writeAtomic(u.recentPath(), packed.Bytes()); err != nil {
		return err
	}
	u.recentBytes = int64(packed.Len())
	return nil
}

type OccupancyWindow struct {
	After                        time.Time `json:"after"`
	Before                       time.Time `json:"before"`
	OccupiedPercent              *float64  `json:"occupiedPercent"`
	ObservedSeconds              float64   `json:"observedSeconds"`
	OccupiedSeconds              float64   `json:"occupiedSeconds"`
	CoveragePercent              float64   `json:"coveragePercent"`
	MinimumHourlyCoveragePercent float64   `json:"minimumHourlyCoveragePercent"`
	Complete                     bool      `json:"complete"`
}

func occupancyWindow(series *occupancySeries, before time.Time, duration time.Duration) OccupancyWindow {
	w := OccupancyWindow{After: before.Add(-duration), Before: before}
	if series == nil {
		return w
	}
	var known, busy int64
	hours := make([]int64, max(1, int(duration/time.Hour)))
	for _, b := range series.Buckets {
		if b[0] < w.After.Unix() || b[0] >= before.Unix() {
			continue
		}
		known += b[1]
		busy += b[2]
		i := int((b[0] - w.After.Unix()) / 3600)
		if i < len(hours) {
			hours[i] += b[1]
		}
	}
	w.ObservedSeconds, w.OccupiedSeconds = float64(known)/1000, float64(busy)/1000
	w.CoveragePercent = float64(known) * 100 / float64(duration.Milliseconds())
	w.MinimumHourlyCoveragePercent = 100
	for _, count := range hours {
		expected := min(duration, time.Hour).Milliseconds()
		w.MinimumHourlyCoveragePercent = min(w.MinimumHourlyCoveragePercent, float64(count)*100/float64(expected))
	}
	if known > 0 {
		w.OccupiedPercent = percent(float64(busy) * 100 / float64(known))
	}
	w.Complete = w.OccupiedPercent != nil && w.CoveragePercent >= 90 && w.MinimumHourlyCoveragePercent >= 90
	return w
}
