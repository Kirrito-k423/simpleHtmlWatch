package watch

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

const completedDetailLimit = 100
const completedPressureLimit = 20
const completedIndexLimit = 64 * 1024 * 1024

func (j TaskJob) requestHash() string {
	if j.HistoryCompacted {
		return j.RequestSHA256
	}
	return continuationTaskHash(j.TaskRequest)
}

// Only confirmed exits are disposable history. Unknown, dispatching, running,
// manually abandoned and unreported experiment tasks keep all their evidence.
func completedTask(j TaskJob) bool {
	return j.FinishedAt != nil && j.ExitCode != nil && (j.Status == "succeeded" || j.Status == "failed")
}

func (m *TaskManager) protectedHistoryLocked() map[string]bool {
	protected := map[string]bool{}
	for _, r := range m.reservations {
		if r.ReleasedAt == nil {
			protected[r.TaskID] = true
			for id, job := range m.jobs {
				if job.ReservationID == r.ID {
					protected[id] = true
				}
			}
		}
	}
	for _, c := range m.continuations {
		if c.State != "completed" && c.State != "cancelled" && c.State != "expired" {
			for _, t := range c.Tasks {
				protected[t.ID] = true
			}
			for _, id := range c.ExistingTaskIDs {
				protected[id] = true
			}
		}
	}
	return protected
}

func compactTask(j TaskJob) TaskJob {
	j.RequestSHA256 = j.requestHash()
	j.HistoryCompacted = true
	j.Shell, j.Error, j.ArchiveError = "", "", ""
	j.EventsDropped += len(j.Events)
	j.Events = nil
	return j
}

func (m *TaskManager) loadCompleted() error {
	path := filepath.Join(m.dir, "completed.json.gz")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("COMPLETED_INDEX_INVALID: %w", err)
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, completedIndexLimit+1))
	if err != nil || len(raw) > completedIndexLimit {
		return errors.New("COMPLETED_INDEX_INVALID")
	}
	var jobs []TaskJob
	if json.Unmarshal(raw, &jobs) != nil {
		return errors.New("COMPLETED_INDEX_INVALID")
	}
	for _, job := range jobs {
		digest, err := hex.DecodeString(job.RequestSHA256)
		if !job.HistoryCompacted || !completedTask(job) || !identifier.MatchString(job.ID) || err != nil || len(digest) != 32 || job.Shell != "" || m.jobs[job.ID] != nil {
			return errors.New("COMPLETED_INDEX_INVALID")
		}
		if job.ReservationID != "" {
			r, ok := m.reservations[job.ReservationID]
			if !ok || (len(r.MachineIDs) > 0 && (r.IntentSHA256 != job.requestHash() || r.GrantID != job.ReservationGrantID || r.Generation != job.ReservationGeneration)) {
				return errors.New("COMPLETED_RESERVATION_MISSING_OR_CHANGED")
			}
		}
		if err := m.validateContinuationTask(job); err != nil {
			return err
		}
		m.jobs[job.ID] = &job
	}
	return nil
}

// Persist the deduplication/status/download index before unlinking any detailed
// job file. A crash between those steps leaves two copies, never a reusable ID.
func (m *TaskManager) compactCompletedLocked() error {
	protected := m.protectedHistoryLocked()
	candidates := []*TaskJob{}
	for _, job := range m.jobs {
		if !job.HistoryCompacted && completedTask(*job) && !protected[job.ID] {
			candidates = append(candidates, job)
		}
	}
	keep := completedDetailLimit
	if m.budget.overTarget() {
		keep = completedPressureLimit
	}
	if len(candidates) <= keep {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].FinishedAt.Equal(*candidates[j].FinishedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].FinishedAt.Before(*candidates[j].FinishedAt)
	})
	candidates = candidates[:len(candidates)-keep]
	index := []TaskJob{}
	for _, job := range m.jobs {
		if job.HistoryCompacted {
			index = append(index, copyTask(job))
		}
	}
	for _, job := range candidates {
		index = append(index, compactTask(*job))
	}
	sort.Slice(index, func(i, j int) bool { return index[i].ID < index[j].ID })
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if len(raw) > completedIndexLimit {
		return errors.New("COMPLETED_INDEX_TOO_LARGE; history kept; new tasks remain enabled")
	}
	var encoded bytes.Buffer
	w := gzip.NewWriter(&encoded)
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := m.budget.Write(filepath.Join(m.dir, "completed.json.gz"), encoded.Bytes(), 0); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(m.dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return err
		}
	}
	for _, job := range candidates {
		path := m.path(job.ID, "job.json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		m.budget.Forget(path)
		compacted := compactTask(*job)
		m.jobs[job.ID] = &compacted
		// Never RemoveAll: cached results and any other user files stay in place.
		_ = os.Remove(m.path(job.ID, ""))
	}
	return nil
}

func (m *TaskManager) retainCompletedLocked() {
	if err := m.compactCompletedLocked(); err != nil {
		m.retentionError = err.Error()
	} else {
		m.retentionError = ""
	}
}

func (m *TaskManager) retainCompletedLoop() {
	defer m.wg.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-tick.C:
			m.mu.Lock()
			m.retainCompletedLocked()
			m.mu.Unlock()
		}
	}
}

func (m *TaskManager) StorageStatus() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := m.budget.Status()
	compacted := 0
	for _, job := range m.jobs {
		if job.HistoryCompacted {
			compacted++
		}
	}
	status["compactedTasks"], status["detailedTasks"] = compacted, len(m.jobs)-compacted
	status["completedDetailLimit"], status["retentionError"] = completedDetailLimit, m.retentionError
	return status
}
