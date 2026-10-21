package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Leave room for the 2 MB utilization ring, configuration and directory entries
// within a 5 MB steady-state growth target. This is an advisory target, not admission control.
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
		if filepath.Base(path) == "storage-budget.json" || (filepath.Ext(path) != ".json" && filepath.Base(path) != "completed.json.gz") {
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
	// Only a maintainer's explicit setting may increase the persisted budget.
	// No task IDs, evidence, reservations or unknown states are removed.
	var requested int64
	if value := os.Getenv("SHW_TASK_METADATA_GROWTH_BYTES"); value != "" {
		requested, err = strconv.ParseInt(value, 10, 64)
		if err != nil || requested < metadataGrowthLimit || requested > 64_000_000 {
			return nil, errors.New("STORAGE_BUDGET_SETTING_INVALID: expected 2000000..64000000 bytes")
		}
	}
	path := filepath.Join(dir, "storage-budget.json")
	var saved struct {
		Version  int   `json:"version"`
		Baseline int64 `json:"baselineBytes"`
		Limit    int64 `json:"limitBytes"`
		Growth   int64 `json:"growthLimitBytes,omitempty"`
	}
	raw, err := os.ReadFile(path)
	write := false
	if os.IsNotExist(err) {
		saved.Version, saved.Baseline, saved.Limit = 1, b.used, b.used+metadataGrowthLimit
		if requested > metadataGrowthLimit {
			saved.Version, saved.Growth, saved.Limit = 2, requested, b.used+requested
		}
		write = true
	} else if err != nil {
		return nil, err
	} else {
		if json.Unmarshal(raw, &saved) != nil || saved.Baseline < 0 {
			return nil, errors.New("STORAGE_BUDGET_INVALID; 不会重置已有预算")
		}
		growth := saved.Growth
		if saved.Version == 1 && growth == 0 {
			growth = metadataGrowthLimit
		}
		if (saved.Version != 1 && saved.Version != 2) || (saved.Version == 1 && saved.Growth != 0) || growth < metadataGrowthLimit || growth > 64_000_000 || saved.Limit != saved.Baseline+growth {
			return nil, errors.New("STORAGE_BUDGET_INVALID; 不会重置已有预算")
		}
		if requested != 0 && requested < growth {
			return nil, errors.New("STORAGE_BUDGET_SHRINK_REFUSED; 保留已采用的预算")
		}
		if requested > growth {
			saved.Version, saved.Growth, saved.Limit = 2, requested, saved.Baseline+requested
			write = true
		}
	}
	if write {
		raw, _ = json.Marshal(saved)
		if err = writeAtomic(path, raw); err != nil {
			return nil, err
		}
	}
	b.baseline, b.limit = saved.Baseline, saved.Limit
	return b, nil
}

// Advisory target: availability wins over a metadata quota. Completed-task
// retention reclaims old detail; active state must still be persisted when full.
func (b *storageBudget) overTarget() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := b.used
	for _, reserve := range b.reserves {
		total += reserve
	}
	return total > b.limit
}

func (b *storageBudget) Forget(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= b.sizes[path]
	delete(b.sizes, path)
	delete(b.reserves, path)
}

func (b *storageBudget) Write(path string, data []byte, reserve int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	return map[string]any{"baselineBytes": b.baseline, "bytes": b.used, "limitBytes": b.limit, "reservedBytes": reserved, "growthLimitBytes": b.limit - b.baseline, "policy": "prune-completed", "admissionBlocked": false, "overTargetBytes": max(int64(0), b.used+reserved-b.limit)}
}
