package watch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const historyLimit int64 = 1024 * 1024 * 1024
const historySegment int64 = 8 * 1024 * 1024
const historyRecordLimit = 32 * 1024 * 1024

type HistoryRecord struct {
	ID          string    `json:"id"`
	MachineID   string    `json:"machineId"`
	MachineName string    `json:"machineName"`
	Host        string    `json:"host"`
	StartedAt   time.Time `json:"startedAt"`
	State       State     `json:"state"`
}
type historyRef struct {
	id, machine, name, host, file string
	offset                        int64
	size                          int
	at                            time.Time
	results                       map[string]historyResult
}
type historyResult struct {
	Name, Shell, Hash string
	Missing           bool
	NPUCount          int
	NPUKnown          bool
}
type History struct {
	mu          sync.Mutex
	dir         string
	refs        []historyRef
	file        *os.File
	size, total int64
	enabled     bool
	epoch       uint64
	lastError   string
	limit       int64
}

func NewHistory(dir string) (*History, error) {
	h := &History{dir: filepath.Join(dir, "history"), limit: historyLimit}
	if err := os.MkdirAll(h.dir, 0700); err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(h.dir, "samples-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, path := range files {
		f, err := os.OpenFile(path, os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		reader := bufio.NewReader(f)
		var offset int64
		for {
			line, readErr := reader.ReadBytes('\n')
			if readErr == io.EOF {
				if len(line) > 0 {
					err = f.Truncate(offset)
				}
				break
			}
			if readErr != nil {
				err = readErr
				break
			}
			var record HistoryRecord
			if len(line) > historyRecordLimit || json.Unmarshal(line, &record) != nil || record.ID == "" {
				err = fmt.Errorf("历史文件损坏：%s", filepath.Base(path))
				break
			}
			h.refs = append(h.refs, makeHistoryRef(record, path, offset, len(line)))
			offset += int64(len(line))
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		h.total += offset
	}
	if err = h.prune(); err != nil {
		return nil, err
	}
	return h, nil
}
func makeHistoryRef(r HistoryRecord, path string, offset int64, size int) historyRef {
	ref := historyRef{r.ID, r.MachineID, r.MachineName, r.Host, path, offset, size, r.StartedAt, map[string]historyResult{}}
	for _, v := range r.State.Results {
		exit := "unknown"
		if v.ExitCode != nil {
			exit = strconv.Itoa(*v.ExitCode)
		}
		hash := sha256.Sum256([]byte(v.Shell + "\x00" + v.Stdout + "\x00" + v.Stderr + "\x00" + v.Error + exit))
		count, known := npuCount(v.Stdout)
		ref.results[v.CommandID] = historyResult{v.Name, v.Shell, hex.EncodeToString(hash[:]), v.ExitCode == nil || v.Truncated, count, known}
	}
	return ref
}
func (h *History) Status() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{"enabled": h.enabled, "bytes": h.total, "limitBytes": h.limit, "records": len(h.refs), "error": h.lastError, "retentionDays": 7}
}
func (h *History) SetEnabled(value bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = value
	h.epoch++
	h.lastError = ""
}
func (h *History) Epoch() (uint64, bool) { h.mu.Lock(); defer h.mu.Unlock(); return h.epoch, h.enabled }
func (h *History) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		return h.file.Close()
	}
	return nil
}
func (h *History) Append(epoch uint64, r HistoryRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.enabled || h.epoch != epoch {
		return
	}
	r.ID = randomToken()
	b, err := json.Marshal(r)
	if err == nil && len(b) > historyRecordLimit-1 {
		err = fmt.Errorf("单份历史超过 32 MiB，记录已停止")
	}
	if err == nil && (h.file == nil || h.size+int64(len(b)+1) > historySegment) {
		if h.file != nil {
			h.file.Close()
			h.file = nil
		}
		h.file, err = os.OpenFile(filepath.Join(h.dir, fmt.Sprintf("samples-%020d.jsonl", time.Now().UnixNano())), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		h.size = 0
	}
	if err == nil {
		b = append(b, '\n')
		var n int
		n, err = h.file.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = h.file.Sync()
		}
		if err == nil {
			h.refs = append(h.refs, makeHistoryRef(r, h.file.Name(), h.size, n))
			h.size += int64(n)
			h.total += int64(n)
			err = h.prune()
		}
	}
	if err != nil {
		h.lastError = err.Error()
		h.enabled = false
		h.epoch++
		if h.file != nil {
			h.file.Close()
			h.file = nil
		}
	}
}
func (h *History) prune() error {
	files, err := filepath.Glob(filepath.Join(h.dir, "samples-*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if h.total <= h.limit && time.Since(info.ModTime()) <= 7*24*time.Hour {
			continue
		}
		if h.file != nil && path == h.file.Name() {
			continue
		}
		if err = os.Remove(path); err != nil {
			return err
		}
		h.total -= info.Size()
		kept := h.refs[:0]
		for _, ref := range h.refs {
			if ref.file != path {
				kept = append(kept, ref)
			}
		}
		h.refs = kept
	}
	return nil
}
func readHistory(ref historyRef) (HistoryRecord, error) {
	var r HistoryRecord
	f, err := os.Open(ref.file)
	if err != nil {
		return r, err
	}
	defer f.Close()
	b := make([]byte, ref.size)
	_, err = f.ReadAt(b, ref.offset)
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	return r, err
}
func (h *History) Get(id string) (HistoryRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ref := range h.refs {
		if ref.id == id {
			return readHistory(ref)
		}
	}
	return HistoryRecord{}, os.ErrNotExist
}
func (h *History) Catalog() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	machines := map[string]map[string]string{}
	commands := map[string]string{}
	var first, last time.Time
	for _, ref := range h.refs {
		machines[ref.machine] = map[string]string{"id": ref.machine, "name": ref.name, "host": ref.host}
		for id, r := range ref.results {
			commands[id] = r.Name
		}
		if first.IsZero() || ref.at.Before(first) {
			first = ref.at
		}
		if ref.at.After(last) {
			last = ref.at
		}
	}
	list := []map[string]string{}
	for _, m := range machines {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["id"] < list[j]["id"] })
	return map[string]any{"machines": list, "commands": commands, "first": first, "last": last}
}

type HistoryCell struct {
	Count   int    `json:"count"`
	Hits    int    `json:"hits"`
	Unknown int    `json:"unknown"`
	ID      string `json:"id"`
	reason  string
}
type HistoryRow struct {
	ID    string        `json:"id"`
	Cells []HistoryCell `json:"cells"`
}

// Query uses fixed time buckets; it never averages away short rule matches.
func (h *History) Query(start time.Time, span time.Duration, ids []string, command, rule, keyword string) ([]HistoryRow, error) {
	return h.QueryContext(context.Background(), start, span, ids, command, rule, keyword)
}
func (h *History) QueryContext(ctx context.Context, start time.Time, span time.Duration, ids []string, command, rule, keyword string) ([]HistoryRow, error) {
	h.mu.Lock()
	refs := append([]historyRef(nil), h.refs...)
	h.mu.Unlock()
	columns := 60
	if span == time.Minute {
		columns = 15
	}
	rows := []HistoryRow{}
	lookup := map[string]int{}
	for _, id := range ids {
		lookup[id] = len(rows)
		rows = append(rows, HistoryRow{id, make([]HistoryCell, columns)})
	}
	previous := map[string]historyResult{}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, ok := lookup[ref.machine]
		if !ok {
			continue
		}
		result, exists := ref.results[command]
		if ref.at.Before(start) {
			if exists {
				previous[ref.machine] = result
			} else {
				delete(previous, ref.machine)
			}
			continue
		}
		if !ref.at.Before(start.Add(span)) {
			continue
		}
		bin := int(ref.at.Sub(start) * time.Duration(columns) / span)
		cell := &rows[row].Cells[bin]
		cell.Count++
		hit, unknown := false, !exists || result.Missing
		if !unknown {
			switch rule {
			case "change":
				prev, ok := previous[ref.machine]
				unknown = !ok || prev.Missing || prev.Shell != result.Shell
				hit = !unknown && prev.Hash != result.Hash
			case "npu":
				unknown = !result.NPUKnown
				hit = result.NPUKnown && result.NPUCount > 1
			case "contains":
				h.mu.Lock()
				record, err := readHistory(ref)
				h.mu.Unlock()
				if err != nil {
					if os.IsNotExist(err) {
						unknown = true
						break
					}
					return nil, err
				}
				for _, r := range record.State.Results {
					if r.CommandID == command {
						hit = strings.Contains(r.Stdout, keyword)
					}
				}
			}
		}
		if hit {
			cell.Hits++
			if cell.reason != "hit" {
				cell.ID = ref.id
				cell.reason = "hit"
			}
		} else if unknown {
			cell.Unknown++
			if cell.reason != "hit" && cell.reason != "unknown" {
				cell.ID = ref.id
				cell.reason = "unknown"
			}
		} else if cell.ID == "" {
			cell.ID = ref.id
		}
		if exists {
			previous[ref.machine] = result
		} else {
			delete(previous, ref.machine)
		}
	}
	return rows, nil
}

// Only recognize a process table with an explicit Process id header and numeric NPU/Chip/PID columns.
// An unrecognized format is unknown, never a zero-process claim.
func npuCount(output string) (int, bool) {
	active := false
	seen := map[string]map[string]bool{}
	recognized := false
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(strings.ToLower(line), "process id") {
			active = true
			recognized = true
			continue
		}
		if !active {
			continue
		}
		f := strings.Fields(strings.Trim(strings.TrimSpace(line), "|"))
		if len(f) == 0 {
			continue
		}
		if _, e := strconv.Atoi(f[0]); e != nil {
			continue
		}
		if len(f) < 4 {
			return 0, false
		}
		if _, e := strconv.Atoi(f[1]); e != nil {
			return 0, false
		}
		if _, e := strconv.Atoi(f[2]); e != nil {
			return 0, false
		}
		key := f[0] + ":" + f[1]
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		seen[key][f[2]] = true
	}
	max := 0
	for _, pids := range seen {
		if len(pids) > max {
			max = len(pids)
		}
	}
	return max, recognized && (len(seen) > 0 || strings.Contains(strings.ToLower(output), "no running processes"))
}

func (h *History) Frames(start time.Time, span time.Duration, machine string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []map[string]any{}
	for _, ref := range h.refs {
		if ref.machine == machine && !ref.at.Before(start) && ref.at.Before(start.Add(span)) {
			out = append(out, map[string]any{"id": ref.id, "at": ref.at})
		}
	}
	return out
}
