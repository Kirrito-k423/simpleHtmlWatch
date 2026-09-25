package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TaskRequest describes one durable remote task. With no selector, any ready
// machine may be chosen; machineId and group remain mutually exclusive.
type TaskRequest struct {
	ID        string `json:"id"`
	Shell     string `json:"shell"`
	MachineID string `json:"machineId,omitempty"`
	Group     string `json:"group,omitempty"`
}

type TaskEvent struct {
	At   time.Time `json:"at"`
	Type string    `json:"type"`
	Text string    `json:"text"`
}

type TaskJob struct {
	TaskRequest
	SelectedMachineID string      `json:"selectedMachineId"`
	MachineName       string      `json:"machineName"`
	Host              string      `json:"host"`
	Port              int         `json:"port"`
	Username          string      `json:"username"`
	Status            string      `json:"status"`
	CreatedAt         time.Time   `json:"createdAt"`
	UpdatedAt         time.Time   `json:"updatedAt"`
	FinishedAt        *time.Time  `json:"finishedAt,omitempty"`
	ExitCode          *int        `json:"exitCode,omitempty"`
	Error             string      `json:"error,omitempty"`
	ArchiveReady      bool        `json:"archiveReady"`
	ArchiveError      string      `json:"archiveError,omitempty"`
	Events            []TaskEvent `json:"events,omitempty"`
	EventsDropped     int         `json:"eventsDropped,omitempty"`
}

func taskEvent(job *TaskJob, at time.Time, kind, message string) {
	events := make([]TaskEvent, len(job.Events), len(job.Events)+1)
	copy(events, job.Events)
	events = append(events, TaskEvent{At: at, Type: kind, Text: message})
	if len(events) > 512 {
		job.EventsDropped += len(events) - 512
		events = append(events[:1], events[len(events)-511:]...)
	}
	job.Events = events
}

func copyTask(job *TaskJob) TaskJob {
	out := *job
	out.Events = append([]TaskEvent(nil), job.Events...)
	return out
}

func taskStatusText(job *TaskJob) string {
	switch job.Status {
	case "running":
		return "远端启动已确认"
	case "unknown":
		return job.Error
	case "succeeded", "failed":
		if job.ExitCode != nil {
			return fmt.Sprintf("远端退出码 %d 已确认", *job.ExitCode)
		}
		return job.Error
	case "abandoned":
		return "人工核实远端已停止，解除本机占用；退出码未知"
	default:
		return job.Error
	}
}

type ReadyMachine struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Group     string    `json:"group"`
	Host      string    `json:"host"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type taskRemote interface {
	Launch(context.Context, TaskJob, Profile) error
	Probe(context.Context, TaskJob, Profile) (string, error)
	Logs(context.Context, TaskJob, Profile) (TaskLogs, error)
	Collect(context.Context, TaskJob, Profile, string) error
}

type TaskLogs struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type TaskManager struct {
	mu         sync.Mutex
	dir        string
	jobs       map[string]*TaskJob
	store      *Store
	monitor    *Monitor
	remote     taskRemote
	collecting map[string]bool
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	poll       time.Duration
}

func NewTaskManager(store *Store, monitor *Monitor, trust *TrustStore) (*TaskManager, error) {
	return newTaskManager(store, monitor, &sshTaskRemote{trust: trust})
}

func newTaskManager(store *Store, monitor *Monitor, remote taskRemote) (*TaskManager, error) {
	dir := filepath.Join(store.dir, "tasks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &TaskManager{dir: dir, jobs: map[string]*TaskJob{}, collecting: map[string]bool{}, store: store, monitor: monitor, remote: remote, ctx: ctx, cancel: cancel, poll: 5 * time.Second}
	entries, err := os.ReadDir(dir)
	if err != nil {
		cancel()
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !identifier.MatchString(entry.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name(), "job.json"))
		if err != nil {
			cancel()
			return nil, err
		}
		var job TaskJob
		if err := json.Unmarshal(b, &job); err != nil || job.ID != entry.Name() {
			cancel()
			return nil, fmt.Errorf("任务记录损坏：%s", entry.Name())
		}
		if job.Status == "dispatching" {
			job.Status = "unknown" // A crash may have happened after SSH accepted the launch.
			job.Error = "启动过程被中断；只查询远端状态，不自动重发命令"
			job.UpdatedAt = time.Now()
			taskEvent(&job, job.UpdatedAt, "unknown", job.Error)
			if err := m.save(&job); err != nil {
				cancel()
				return nil, err
			}
		}
		m.jobs[job.ID] = &job
	}
	for id, job := range m.jobs {
		if job.FinishedAt == nil {
			m.wg.Add(1)
			go m.follow(id, false)
		} else if !job.ArchiveReady && job.ExitCode != nil {
			m.wg.Add(1)
			go func(id string) { defer m.wg.Done(); _, _ = m.Collect(id) }(id)
		}
	}
	return m, nil
}

func (m *TaskManager) Close() { m.cancel(); m.wg.Wait() }

func (m *TaskManager) path(id, name string) string { return filepath.Join(m.dir, id, name) }

func (m *TaskManager) save(job *TaskJob) error {
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(m.path(job.ID, "job.json"), b)
}

func (m *TaskManager) Get(id string) (TaskJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return TaskJob{}, false
	}
	return copyTask(job), true
}

func (m *TaskManager) List() []TaskJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		out = append(out, copyTask(job))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *TaskManager) readyLocked(c Config, states map[string]State, machineID, group string) []ReadyMachine {
	busy := map[string]bool{}
	busyAddress := map[string]bool{}
	for _, job := range m.jobs {
		if job.FinishedAt == nil {
			busy[job.SelectedMachineID] = true
			busyAddress[net.JoinHostPort(job.Host, strconv.Itoa(job.Port))] = true
		}
	}
	maxAge := time.Duration(c.Interval*2+30) * time.Second
	ready := []ReadyMachine{}
	for _, machine := range c.Machines {
		state := states[machine.ID]
		if !machine.Enabled || busy[machine.ID] || busyAddress[net.JoinHostPort(machine.Host, strconv.Itoa(machine.Port))] || machineID != "" && machine.ID != machineID || group != "" && machine.Group != group ||
			(state.Status != "online" && state.Status != "partial") || state.UpdatedAt.IsZero() || time.Since(state.UpdatedAt) > maxAge {
			continue
		}
		ready = append(ready, ReadyMachine{machine.ID, machine.Name, machine.Group, machine.Host, state.UpdatedAt})
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	return ready
}

func (m *TaskManager) Ready(machineID, group string) []ReadyMachine {
	c := m.store.Snapshot()
	states := m.monitor.Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readyLocked(c, states, machineID, group)
}

func (m *TaskManager) Submit(request TaskRequest) (TaskJob, int, error) {
	request.Shell = strings.TrimSpace(request.Shell)
	if !identifier.MatchString(request.ID) || request.Shell == "" || len(request.Shell) > 4096 || strings.ContainsRune(request.Shell, 0) ||
		request.MachineID != "" && request.Group != "" || request.MachineID != "" && !identifier.MatchString(request.MachineID) || len(request.Group) > 80 || strings.ContainsRune(request.Group, 0) {
		return TaskJob{}, 400, errors.New("请提供有效任务 ID 和最多 4096 字节的命令；machineId 与 group 不能同时指定")
	}
	if watchPrefix.MatchString(request.Shell) {
		return TaskJob{}, 400, errors.New("任务命令不能使用 watch 前缀")
	}
	c := m.store.Snapshot()
	states := m.monitor.Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.jobs[request.ID]; ok {
		if old.TaskRequest != request {
			return TaskJob{}, 409, errors.New("同一任务 ID 不能对应不同请求")
		}
		return copyTask(old), 200, nil
	}
	ready := m.readyLocked(c, states, request.MachineID, request.Group)
	if len(ready) == 0 {
		return TaskJob{}, 409, errors.New("没有符合条件且监控状态新鲜的空闲机器")
	}
	selected := ready[0]
	var host Machine
	for _, machine := range c.Machines {
		if machine.ID == selected.ID {
			host = machine
			break
		}
	}
	var username string
	for _, p := range c.Profiles {
		if p.ID == host.ProfileID {
			username = p.Username
			break
		}
	}
	now := time.Now()
	job := &TaskJob{TaskRequest: request, SelectedMachineID: selected.ID, MachineName: selected.Name, Host: host.Host, Port: host.Port, Username: username, Status: "dispatching", CreatedAt: now, UpdatedAt: now}
	taskEvent(job, now, "dispatching", "任务已受理，分配至 "+selected.Name)
	if err := os.Mkdir(m.path(job.ID, ""), 0700); err != nil {
		return TaskJob{}, 500, err
	}
	if err := m.save(job); err != nil {
		return TaskJob{}, 500, err
	}
	m.jobs[job.ID] = job
	m.wg.Add(1)
	go m.follow(job.ID, true)
	return copyTask(job), 202, nil
}

func (m *TaskManager) profile(job TaskJob, requireEnabled bool) (Profile, error) {
	c := m.store.Snapshot()
	for _, host := range c.Machines {
		if host.ID != job.SelectedMachineID || requireEnabled && !host.Enabled {
			continue
		}
		for _, p := range c.Profiles {
			if p.ID == host.ProfileID && host.Host == job.Host && host.Port == job.Port && p.Username == job.Username {
				return p, nil
			}
		}
	}
	return Profile{}, errors.New("机器地址、用户或凭据配置已变化，无法安全连接原任务")
}

func (m *TaskManager) update(id string, change func(*TaskJob)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	beforeStatus, beforeArchive, beforeArchiveError := job.Status, job.ArchiveReady, job.ArchiveError
	change(job)
	job.UpdatedAt = time.Now()
	if job.Status != beforeStatus {
		taskEvent(job, job.UpdatedAt, job.Status, taskStatusText(job))
	}
	if job.ArchiveReady && !beforeArchive {
		taskEvent(job, job.UpdatedAt, "archive_ready", "本机结果包已回收")
	} else if job.ArchiveError != "" && job.ArchiveError != beforeArchiveError {
		taskEvent(job, job.UpdatedAt, "archive_error", "结果回收失败："+job.ArchiveError)
	}
	if err := m.save(job); err != nil {
		job.Error = "本机任务记录写入失败：" + err.Error()
	}
}

func (m *TaskManager) observe(id, status, problem string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job.FinishedAt != nil {
		return
	}
	if job.Status == status && job.Error == problem && time.Since(job.UpdatedAt) < 30*time.Second {
		return
	}
	changed := job.Status != status
	job.Status = status
	job.Error = problem
	job.UpdatedAt = time.Now()
	if changed {
		taskEvent(job, job.UpdatedAt, status, taskStatusText(job))
	}
	if err := m.save(job); err != nil {
		job.Error = "本机任务记录写入失败：" + err.Error()
	}
}

func (m *TaskManager) complete(id string, code int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job.FinishedAt != nil {
		return false
	}
	now := time.Now()
	job.FinishedAt = &now
	job.UpdatedAt = now
	job.ExitCode = &code
	job.Status = "succeeded"
	if code != 0 {
		job.Status = "failed"
	}
	job.Error = ""
	taskEvent(job, now, job.Status, taskStatusText(job))
	if err := m.save(job); err != nil {
		job.Error = "本机任务记录写入失败：" + err.Error()
	}
	return true
}

// Abandon releases an unknown reservation only after an operator independently
// verifies that the remote process has stopped. It never sends a remote command.
func (m *TaskManager) Abandon(id string) (TaskJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return TaskJob{}, os.ErrNotExist
	}
	if job.Status != "unknown" || job.FinishedAt != nil {
		return TaskJob{}, errors.New("仅可解除结果未知且尚未结束的任务")
	}
	now := time.Now()
	prior := *job
	job.Status = "abandoned"
	job.FinishedAt = &now
	job.UpdatedAt = now
	job.Error = "人工确认远端任务已停止后解除占用；远端退出结果仍未知"
	taskEvent(job, now, "abandoned", taskStatusText(job))
	if err := m.save(job); err != nil {
		*job = prior
		return TaskJob{}, err
	}
	return copyTask(job), nil
}

func (m *TaskManager) follow(id string, launch bool) {
	defer m.wg.Done()
	job, _ := m.Get(id)
	if launch {
		profile, err := m.profile(job, true)
		if err != nil {
			m.update(id, func(j *TaskJob) {
				now := time.Now()
				j.Status = "failed"
				j.FinishedAt = &now
				j.Error = "启动前配置检查失败：" + err.Error()
			})
			return
		}
		err = m.remote.Launch(m.ctx, job, profile)
		if err != nil {
			m.update(id, func(j *TaskJob) {
				j.Status = "unknown"
				j.Error = "启动结果未确认：" + err.Error() + "；不会自动重发"
			})
		} else {
			m.update(id, func(j *TaskJob) { j.Status = "running"; j.Error = "" })
		}
	}
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(m.poll):
		}
		job, _ = m.Get(id)
		if job.FinishedAt != nil {
			return
		}
		profile, err := m.profile(job, false)
		if err != nil {
			m.observe(id, "unknown", err.Error())
			continue
		}
		state, err := m.remote.Probe(m.ctx, job, profile)
		if err != nil {
			m.observe(id, "unknown", "查询远端状态失败："+err.Error())
			continue
		}
		if state == "missing" {
			m.observe(id, "unknown", "远端任务目录不存在；未确认命令是否曾启动")
			continue
		}
		if state == "lost" || state == "incomplete" {
			m.observe(id, "unknown", "远端任务未见退出标记，启动文件或进程状态为 "+state)
			continue
		}
		if state == "running" {
			m.observe(id, "running", "")
			continue
		}
		if !strings.HasPrefix(state, "done:") {
			m.observe(id, "unknown", "远端状态格式无效")
			continue
		}
		code, err := strconv.Atoi(strings.TrimPrefix(state, "done:"))
		if err != nil || code < 0 || code > 255 {
			m.observe(id, "unknown", "远端退出码无效")
			continue
		}
		if m.complete(id, code) {
			_, _ = m.Collect(id)
		}
		return
	}
}

func (m *TaskManager) Logs(id string) (TaskLogs, error) {
	job, ok := m.Get(id)
	if !ok {
		return TaskLogs{}, os.ErrNotExist
	}
	profile, err := m.profile(job, false)
	if err != nil {
		return TaskLogs{}, err
	}
	return m.remote.Logs(m.ctx, job, profile)
}

func (m *TaskManager) Collect(id string) (TaskJob, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return TaskJob{}, os.ErrNotExist
	}
	if job.FinishedAt == nil {
		m.mu.Unlock()
		return TaskJob{}, errors.New("任务尚未完成")
	}
	if job.ExitCode == nil {
		m.mu.Unlock()
		return TaskJob{}, errors.New("远端退出结果未知，不能自动回收")
	}
	if job.ArchiveReady {
		if _, err := os.Stat(m.path(id, "result.tar.gz")); err == nil {
			out := copyTask(job)
			m.mu.Unlock()
			return out, nil
		}
		job.ArchiveReady = false
	}
	if m.collecting[id] {
		m.mu.Unlock()
		return TaskJob{}, errors.New("结果包正在回收")
	}
	m.collecting[id] = true
	copy := copyTask(job)
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.collecting, id); m.mu.Unlock() }()
	profile, err := m.profile(copy, false)
	if err == nil {
		err = m.remote.Collect(m.ctx, copy, profile, m.path(id, "result.tar.gz"))
	}
	m.update(id, func(j *TaskJob) {
		j.ArchiveReady = err == nil
		j.ArchiveError = ""
		if err != nil {
			j.ArchiveError = err.Error()
		}
	})
	out, _ := m.Get(id)
	return out, err
}
