package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Leave room for the 2 MB utilization ring, configuration and directory entries
// within a 5 MB steady-state growth target. Existing evidence is never evicted.
const metadataGrowthLimit int64 = 2_000_000
const taskCompletionReserve int64 = 64 * 1024

type storageBudget struct {
	mu                    sync.Mutex
	dir                   string
	limit, baseline, used int64
	sizes                 map[string]int64
	reserves              map[string]int64
}

func storageCharge(size int64) int64 { return (size + 4095) / 4096 * 4096 }

// Count allocation units, including one directory unit per task. Result archives
// and manually installed third-party services are deliberately outside this ledger.
func newStorageBudget(dir string) (*storageBudget, error) {
	b := &storageBudget{dir: dir, sizes: map[string]int64{}, reserves: map[string]int64{}}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		parent := filepath.Dir(rel)
		metadataDir := parent == "." || parent == ".cleanup" || identifier.MatchString(parent)
		if metadataDir && strings.HasPrefix(e.Name(), ".watch-") {
			// Called only after the instance lock is acquired. These are our own
			// interrupted atomic metadata writes, never remote result files.
			if err := os.Remove(path); err != nil {
				return err
			}
			return nil
		}
		if filepath.Base(path) == "storage-budget.json" || filepath.Ext(path) != ".json" {
			return nil
		}
		if strings.Contains(rel, string(filepath.Separator)+".") {
			return nil
		}
		i, err := e.Info()
		if err != nil {
			return err
		}
		if !i.Mode().IsRegular() {
			return fmt.Errorf("STORAGE_METADATA_NOT_REGULAR: %s", rel)
		}
		b.sizes[path] = storageCharge(i.Size())
		if filepath.Base(path) == "job.json" {
			b.sizes[path] += 4096
		}
		b.used += b.sizes[path]
		return nil
	})
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "storage-budget.json")
	var saved struct {
		Version  int   `json:"version"`
		Baseline int64 `json:"baselineBytes"`
		Limit    int64 `json:"limitBytes"`
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b.baseline, b.limit = b.used, b.used+metadataGrowthLimit
		saved.Version, saved.Baseline, saved.Limit = 1, b.baseline, b.limit
		raw, _ = json.Marshal(saved)
		if err = writeAtomic(path, raw); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		if json.Unmarshal(raw, &saved) != nil || saved.Version != 1 || saved.Baseline < 0 || saved.Limit != saved.Baseline+metadataGrowthLimit {
			return nil, errors.New("STORAGE_BUDGET_INVALID; 不会重置已有预算")
		}
		b.baseline, b.limit = saved.Baseline, saved.Limit
	}
	return b, nil
}

func (b *storageBudget) checkLocked(path string, data []byte, reserve int64) error {
	charge := storageCharge(int64(len(data)))
	if filepath.Base(path) == "job.json" {
		charge += 4096
	}
	total := b.used - b.sizes[path] + charge + reserve
	for p, n := range b.reserves {
		if p != path {
			total += n
		}
	}
	if total > b.limit && charge+reserve > b.sizes[path]+b.reserves[path] {
		return errors.New("STORAGE_BUDGET_FULL: 元数据增长已达 2 MB 预算；保留任务身份和占用，停止新增写入，请由维护者处理历史数据")
	}
	return nil
}

func (b *storageBudget) Check(path string, data []byte, reserve int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkLocked(path, data, reserve)
}

func (b *storageBudget) Write(path string, data []byte, reserve int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkLocked(path, data, reserve); err != nil {
		return err
	}
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	charge := storageCharge(int64(len(data)))
	if filepath.Base(path) == "job.json" {
		charge += 4096
	}
	b.used += charge - b.sizes[path]
	b.sizes[path], b.reserves[path] = charge, reserve
	return nil
}

func (b *storageBudget) Status() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var reserved int64
	for _, n := range b.reserves {
		reserved += n
	}
	return map[string]any{"baselineBytes": b.baseline, "bytes": b.used, "limitBytes": b.limit, "reservedBytes": reserved, "growthLimitBytes": metadataGrowthLimit}
}
