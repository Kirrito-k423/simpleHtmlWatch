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
	"strings"
	"sync"
	"time"
)

const utilizationRetention = 30
const utilizationLimit int64 = 2_000_000
const utilizationDayLimit = 2 * 1024 * 1024

var chinaTime = time.FixedZone("Asia/Shanghai", 8*60*60)

type utilizationMachine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Group   string `json:"group"`
	Address string `json:"address"`
	Model   string `json:"model"`
	Metric  string `json:"metric"`
}

// Hour, covered ms, sum(percent*100*ms), low ms, peak(percent*100), samples,
// process-state covered ms, occupied ms. No device or process identifiers.
// Fixed arrays avoid repeating field names in every hourly record.
type utilizationBucket [8]int64
type utilizationDay struct {
	Version  int                            `json:"v"`
	Date     string                         `json:"d"`
	Machines map[string]utilizationMachine  `json:"m"`
	Hours    map[string][]utilizationBucket `json:"h"`
}

// Version 1 has six fields and no occupancy history. Normalize it in memory
// without backfilling zero occupancy or rewriting untouched historical files.
func (d *utilizationDay) UnmarshalJSON(raw []byte) error {
	var disk struct {
		Version  int                           `json:"v"`
		Date     string                        `json:"d"`
		Machines map[string]utilizationMachine `json:"m"`
		Hours    map[string][][]int64          `json:"h"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		return err
	}
	width := 8
	if disk.Version == 1 {
		width = 6
	} else if disk.Version != 2 {
		return fmt.Errorf("未知利用率日文件版本")
	}
	*d = utilizationDay{Version: 2, Date: disk.Date, Machines: disk.Machines}
	if disk.Hours != nil {
		d.Hours = map[string][]utilizationBucket{}
	}
	for key, rows := range disk.Hours {
		for _, row := range rows {
			if len(row) != width {
				return fmt.Errorf("利用率小时桶字段数无效")
			}
			var b utilizationBucket
			copy(b[:], row)
			d.Hours[key] = append(d.Hours[key], b)
		}
	}
	return nil
}

type utilizationPending struct {
	resourceKey    string
	key            string
	machine        utilizationMachine
	at, cursor     time.Time
	hold           time.Duration
	basis          int64
	occupancyKnown bool
	occupied       bool
}
type Utilization struct {
	recent      map[string]*occupancySeries
	recentBytes int64
	recentError string
	mu          sync.Mutex
	dir         string
	days        map[string]*utilizationDay
	dirty       map[string]bool
	pending     map[string]utilizationPending
	bytes       int64
	limit       int64
	lastError   string
	now         func() time.Time
	stop        chan struct{}
	wg          sync.WaitGroup
}

func NewUtilization(dir string) (*Utilization, error) {
	return newUtilization(dir, time.Now)
}
func newUtilization(dir string, now func() time.Time) (*Utilization, error) {
	u := &Utilization{dir: filepath.Join(dir, "utilization"), days: map[string]*utilizationDay{}, dirty: map[string]bool{}, pending: map[string]utilizationPending{}, limit: utilizationLimit, now: now}
	if err := os.MkdirAll(u.dir, 0700); err != nil {
		return nil, err
	}
	// The instance's OS lock is already held. Remove only our interrupted
	// atomic-write scratch files in this dedicated directory.
	scratch, err := filepath.Glob(filepath.Join(u.dir, ".watch-*"))
	if err != nil {
		return nil, err
	}
	for _, path := range scratch {
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	}
	if err := u.pruneLocked(); err != nil {
		return nil, err
	}
	files, err := u.files()
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		if info.Size() > utilizationDayLimit {
			return nil, fmt.Errorf("利用率日文件超过 2 MiB")
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(file, ".gz") {
			reader, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return nil, err
			}
			raw, err = io.ReadAll(io.LimitReader(reader, utilizationDayLimit+1))
			reader.Close()
			if err != nil || len(raw) > utilizationDayLimit {
				return nil, fmt.Errorf("利用率压缩日文件无效：%s", filepath.Base(file))
			}
		}
		var day utilizationDay
		if err = json.Unmarshal(raw, &day); err != nil {
			return nil, fmt.Errorf("利用率日文件损坏：%s", filepath.Base(file))
		}
		if err = validateUtilizationDay(day, strings.TrimSuffix(filepath.Base(file), ".gz")); err != nil {
			return nil, err
		}
		u.days[day.Date] = &day
	}
	if err := u.loadRecent(); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *Utilization) files() ([]string, error) {
	plain, err := filepath.Glob(filepath.Join(u.dir, "????-??-??.json"))
	if err != nil {
		return nil, err
	}
	compressed, err := filepath.Glob(filepath.Join(u.dir, "????-??-??.json.gz"))
	if err != nil {
		return nil, err
	}
	files := append(plain, compressed...)
	sort.Strings(files)
	return files, nil
}

func validateUtilizationDay(day utilizationDay, filename string) error {
	date, err := time.ParseInLocation("2006-01-02", day.Date, chinaTime)
	if err != nil || date.Format("2006-01-02")+".json" != filename || day.Version != 2 || day.Machines == nil || day.Hours == nil {
		return fmt.Errorf("利用率日文件格式无效：%s", filename)
	}
	for key, rows := range day.Hours {
		m, exists := day.Machines[key]
		if !exists || m.ID == "" || m.Address == "" || m.Metric == "" || m.Model == "" {
			return fmt.Errorf("利用率机器元数据无效：%s", filename)
		}
		seen := map[int64]bool{}
		for _, b := range rows {
			if b[0] < 0 || b[0] > 23 || seen[b[0]] || b[1] < 0 || b[1] > 3600000 || b[2] < 0 || b[2] > b[1]*10000 || b[3] < 0 || b[3] > b[1] || b[4] < 0 || b[4] > 10000 || b[5] < 0 || b[6] < 0 || b[6] > b[1] || b[7] < 0 || b[7] > b[6] {
				return fmt.Errorf("利用率小时桶无效：%s", filename)
			}
			seen[b[0]] = true
		}
	}
	return nil
}

func (u *Utilization) Start() {
	u.stop = make(chan struct{})
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-u.stop:
				return
			case <-ticker.C:
				_ = u.Flush()
			}
		}
	}()
}
func (u *Utilization) Close() error {
	if u.stop != nil {
		close(u.stop)
		u.wg.Wait()
	}
	return u.Flush()
}

func (u *Utilization) bucket(at time.Time, p utilizationPending) *utilizationBucket {
	local := at.In(chinaTime)
	date := local.Format("2006-01-02")
	day := u.days[date]
	if day == nil {
		day = &utilizationDay{2, date, map[string]utilizationMachine{}, map[string][]utilizationBucket{}}
		u.days[date] = day
	}
	day.Machines[p.key] = p.machine
	u.dirty[date] = true
	for i := range day.Hours[p.key] {
		if day.Hours[p.key][i][0] == int64(local.Hour()) {
			return &day.Hours[p.key][i]
		}
	}
	day.Hours[p.key] = append(day.Hours[p.key], utilizationBucket{int64(local.Hour())})
	return &day.Hours[p.key][len(day.Hours[p.key])-1]
}
func (u *Utilization) advance(p utilizationPending, end time.Time) utilizationPending {
	// Buckets and query windows use wall-clock calendar hours. Mixed monotonic
	// and wall durations can exceed an hour after a local clock correction.
	p.at, p.cursor, end = p.at.Round(0), p.cursor.Round(0), end.Round(0)
	if cap := p.at.Add(p.hold); end.After(cap) {
		end = cap
	}
	for p.cursor.Before(end) {
		next := p.cursor.Truncate(time.Hour).Add(time.Hour)
		if next.After(end) {
			next = end
		}
		ms := next.Sub(p.cursor).Milliseconds()
		if ms > 0 {
			u.advanceRecent(p, p.cursor, next)
			b := u.bucket(p.cursor, p)
			b[1] += ms
			b[2] += p.basis * ms
			if p.occupancyKnown {
				b[6] += ms
				if p.occupied {
					b[7] += ms
				}
			}
			if p.basis < 1000 {
				b[3] += ms
			}
			if p.basis > b[4] {
				b[4] = p.basis
			}
		}
		p.cursor = next
	}
	return p
}
func (u *Utilization) advanceAll(at time.Time) {
	for id, p := range u.pending {
		u.pending[id] = u.advance(p, at)
	}
}

// Observe reuses the monitor's NPU result and never issues additional SSH commands.
func (u *Utilization) Observe(machine Machine, state State, interval time.Duration) {
	allowed := false
	for _, command := range machine.Commands {
		if command == "npu" && machine.Enabled {
			allowed = true
		}
	}
	var reading machineReading
	at, valid := state.UpdatedAt, false
	for _, r := range state.Results {
		if r.CommandID != "npu" {
			continue
		}
		if !r.EndedAt.IsZero() {
			at = r.EndedAt
		}
		if allowed && r.ExitCode != nil && *r.ExitCode == 0 && r.Error == "" && r.Warning == "" && !r.Truncated {
			var err error
			reading, err = parseNPUUtilization(r.Stdout)
			valid = err == nil
		}
		break
	}
	if at.IsZero() {
		return
	}
	at = at.Round(0)
	u.mu.Lock()
	defer u.mu.Unlock()
	if at.After(u.now().Add(time.Second)) {
		return
	}
	old, exists := u.pending[machine.ID]
	if exists && !at.After(old.at) {
		return
	}
	if !valid {
		if exists {
			u.advance(old, at)
			delete(u.pending, machine.ID)
		}
		return
	}
	meta := utilizationMachine{machine.ID, machine.Name, machine.Group, fmt.Sprintf("%s:%d", machine.Host, machine.Port), reading.model, reading.metric}
	key := utilizationKey(meta)
	if exists && old.key == key {
		u.advance(old, at)
	}
	cursor := at
	if exists && old.key == key && old.cursor.After(cursor) {
		cursor = old.cursor // Never count a previously integrated interval twice.
	}
	p := utilizationPending{resourceKey: machineResource(machine), key: key, machine: meta, at: at, cursor: cursor, hold: 2*interval + 30*time.Second, basis: reading.basis, occupancyKnown: reading.occupancyKnown, occupied: reading.occupied}
	u.pending[machine.ID] = p
	b := u.bucket(at, p)
	b[5]++
	if p.basis > b[4] {
		b[4] = p.basis
	}
}

// End a monitor generation before changing host identities or disabling machines.
func (u *Utilization) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.advanceAll(u.now())
	u.pending = map[string]utilizationPending{}
}

func (u *Utilization) pruneLocked() error {
	files, err := u.files()
	if err != nil {
		return err
	}
	sort.Strings(files)
	cutoff := u.now().In(chinaTime).AddDate(0, 0, 1-utilizationRetention).Format("2006-01-02")
	u.bytes = 0
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		date := filepath.Base(file)[:10]
		if date < cutoff {
			if err = os.Remove(file); err != nil {
				return err
			}
			delete(u.days, date)
			delete(u.dirty, date)
		} else {
			u.bytes += info.Size()
		}
	}
	for _, file := range files {
		if u.bytes <= u.limit {
			break
		}
		info, err := os.Stat(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err = os.Remove(file); err != nil {
			return err
		}
		u.bytes -= info.Size()
		date := filepath.Base(file)[:10]
		delete(u.days, date)
		delete(u.dirty, date)
	}
	for date := range u.days {
		if date < cutoff {
			delete(u.days, date)
			delete(u.dirty, date)
		}
	}
	return nil
}

func (u *Utilization) Flush() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	err := u.flushLocked()
	recentErr := u.flushRecentLocked()
	u.recentError = ""
	if recentErr != nil {
		u.recentError = recentErr.Error()
	}
	if err != nil {
		u.lastError = err.Error()
	} else {
		u.lastError = ""
	}
	if err != nil {
		return err
	}
	return recentErr
}

func utilizationKey(meta utilizationMachine) string {
	hash := sha256.Sum256([]byte(meta.ID + "\x00" + meta.Address + "\x00" + meta.Model + "\x00" + meta.Metric))
	return hex.EncodeToString(hash[:12])
}
func (u *Utilization) flushLocked() error {
	if err := u.pruneLocked(); err != nil {
		return err
	}
	dates := make([]string, 0, len(u.dirty))
	for date := range u.dirty {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates {
		day := u.days[date]
		if day == nil {
			continue
		}
		raw, err := json.Marshal(day)
		if err != nil {
			return err
		}
		if len(raw) > utilizationDayLimit {
			return fmt.Errorf("利用率日文件超过 2 MiB，停止写入；实时监控继续")
		}
		var packed bytes.Buffer
		zipper := gzip.NewWriter(&packed)
		if _, err = zipper.Write(raw); err != nil {
			return err
		}
		if err = zipper.Close(); err != nil {
			return err
		}
		raw = packed.Bytes()
		file := filepath.Join(u.dir, date+".json.gz")
		var previous int64
		if info, err := os.Stat(file); err == nil {
			previous = info.Size()
		} else if !os.IsNotExist(err) {
			return err
		}
		// Reserve space for the atomic replacement too, not only the final file.
		for u.bytes+int64(len(raw)) > u.limit {
			files, err := u.files()
			if err != nil {
				return err
			}
			sort.Strings(files)
			removed := false
			for _, old := range files {
				if filepath.Base(old)[:10] >= date {
					continue
				}
				info, err := os.Stat(old)
				if err != nil {
					return err
				}
				if err = os.Remove(old); err != nil {
					return err
				}
				u.bytes -= info.Size()
				oldDate := filepath.Base(old)[:10]
				delete(u.days, oldDate)
				delete(u.dirty, oldDate)
				removed = true
				break
			}
			if !removed {
				return fmt.Errorf("利用率存储达到上限，无法安全覆盖当前日文件")
			}
		}
		if err = writeAtomic(file, raw); err != nil {
			return err
		}
		u.bytes += int64(len(raw)) - previous
		legacy := filepath.Join(u.dir, date+".json")
		if info, err := os.Stat(legacy); err == nil {
			if err = os.Remove(legacy); err != nil {
				return err
			}
			u.bytes -= info.Size()
		} else if !os.IsNotExist(err) {
			return err
		}
		delete(u.dirty, date)
	}
	return nil
}
