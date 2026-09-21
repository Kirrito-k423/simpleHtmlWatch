package watch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ExecutionTarget struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}
type ExecutionRequest struct {
	ID      string            `json:"id"`
	Shell   string            `json:"shell"`
	Targets []ExecutionTarget `json:"targets"`
}
type ExecutionResult struct {
	Target ExecutionTarget `json:"target"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Result Result          `json:"result"`
}
type ExecutionJob struct {
	ID         string            `json:"id"`
	Shell      string            `json:"shell"`
	CreatedAt  time.Time         `json:"createdAt"`
	FinishedAt *time.Time        `json:"finishedAt"`
	Results    []ExecutionResult `json:"results"`
}
type executionSeen struct{ hash [32]byte }
type Executor struct {
	mu      sync.Mutex
	trust   *TrustStore
	jobs    map[string]*ExecutionJob
	order   []string
	seen    map[string]executionSeen
	active  bool
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	timeout time.Duration
}

func NewExecutor(trust *TrustStore) *Executor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Executor{trust: trust, jobs: map[string]*ExecutionJob{}, seen: map[string]executionSeen{}, ctx: ctx, cancel: cancel, timeout: 12 * time.Second}
}
func (e *Executor) Close() { e.mu.Lock(); e.closed = true; e.cancel(); e.mu.Unlock(); e.wg.Wait() }
func copyJob(job *ExecutionJob) ExecutionJob {
	copy := *job
	copy.Results = append([]ExecutionResult{}, job.Results...)
	return copy
}
func (e *Executor) Get(id string) (ExecutionJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	job, ok := e.jobs[id]
	if !ok {
		return ExecutionJob{}, false
	}
	return copyJob(job), true
}

type ExecutionSummary struct {
	ID         string     `json:"id"`
	Shell      string     `json:"shell"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Count      int        `json:"count"`
}

func (e *Executor) List() []ExecutionSummary {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []ExecutionSummary{}
	for i := len(e.order) - 1; i >= 0; i-- {
		job := e.jobs[e.order[i]]
		out = append(out, ExecutionSummary{job.ID, job.Shell, job.CreatedAt, job.FinishedAt, len(job.Results)})
	}
	return out
}
func (e *Executor) Start(request ExecutionRequest, c Config) (ExecutionJob, int, error) {
	request.Shell = strings.TrimSpace(request.Shell)
	if !identifier.MatchString(request.ID) || request.Shell == "" || len(request.Shell) > 4096 || strings.ContainsRune(request.Shell, 0) || len(request.Targets) == 0 || len(request.Targets) > 200 {
		return ExecutionJob{}, 400, errors.New("请选择机器并填写最多 4096 字节的单次命令")
	}
	if watchPrefix.MatchString(request.Shell) {
		return ExecutionJob{}, 400, errors.New("批量执行只运行一次，请去掉 watch 前缀；定时采集请使用监控页的自定义指令")
	}
	sort.Slice(request.Targets, func(i, j int) bool { return request.Targets[i].ID < request.Targets[j].ID })
	encoded, _ := json.Marshal(request)
	hash := sha256.Sum256(encoded)
	e.mu.Lock()
	defer e.mu.Unlock()
	if prior, ok := e.seen[request.ID]; ok {
		if prior.hash != hash {
			return ExecutionJob{}, 409, errors.New("同一执行编号不能用于不同命令或机器")
		}
		if job, ok := e.jobs[request.ID]; ok {
			return copyJob(job), 200, nil
		}
		return ExecutionJob{}, 410, errors.New("该编号已执行，结果已过期；不会再次执行")
	}
	if e.closed {
		return ExecutionJob{}, 503, errors.New("程序正在退出")
	}
	if e.active {
		return ExecutionJob{}, 409, errors.New("上一批命令仍在执行，请等待完成")
	}
	profiles := map[string]Profile{}
	for _, p := range c.Profiles {
		profiles[p.ID] = p
	}
	machines := map[string]Machine{}
	for _, m := range c.Machines {
		machines[m.ID] = m
	}
	credentials := make([]Profile, len(request.Targets))
	job := &ExecutionJob{ID: request.ID, Shell: request.Shell, CreatedAt: time.Now(), Results: make([]ExecutionResult, len(request.Targets))}
	addresses := map[string]bool{}
	for i, target := range request.Targets {
		address := target.Username + "@" + net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
		if addresses[address] {
			return ExecutionJob{}, 409, errors.New("所选机器包含重复 SSH 地址和用户名，请只勾选其中一项")
		}
		addresses[address] = true
		m, ok := machines[target.ID]
		p, present := profiles[m.ProfileID]
		if !ok || !m.Enabled || !present || m.Host != target.Host || m.Port != target.Port || p.Username != target.Username || (i > 0 && request.Targets[i-1].ID == target.ID) {
			return ExecutionJob{}, 409, errors.New("所选机器已停用、配置已变化或重复，请重新选择并核对")
		}
		credentials[i] = p
		job.Results[i] = ExecutionResult{Target: target, Name: m.Name, Status: "queued"}
	}
	// Retain a small number of result sets. Tombstones keep old request IDs from replaying.
	if len(e.order) >= 5 {
		delete(e.jobs, e.order[0])
		e.order = e.order[1:]
	}
	e.seen[request.ID] = executionSeen{hash}
	e.jobs[job.ID] = job
	e.order = append(e.order, job.ID)
	e.active = true
	e.wg.Add(1)
	go e.execute(job.ID, request.Shell, append([]ExecutionTarget{}, request.Targets...), credentials)
	return copyJob(job), 202, nil
}
func trimExecutionResult(r Result) Result {
	const limit = 16 * 1024
	for _, value := range []*string{&r.Stdout, &r.Stderr, &r.Output} {
		if len(*value) > limit {
			*value = (*value)[:limit]
			r.Truncated = true
		}
	}
	return r
}
func (e *Executor) execute(id, shell string, targets []ExecutionTarget, profiles []Profile) {
	defer e.wg.Done()
	slots := make(chan struct{}, 4)
	var workers sync.WaitGroup
	for index, target := range targets {
		workers.Add(1)
		go func(index int, target ExecutionTarget, profile Profile) {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-e.ctx.Done():
				e.finish(id, index, "cancelled", Result{Error: "程序退出，未执行"})
				return
			}
			if e.ctx.Err() != nil {
				e.finish(id, index, "cancelled", Result{Error: "程序退出，未执行"})
				return
			}
			e.mu.Lock()
			e.jobs[id].Results[index].Status = "running"
			e.mu.Unlock()
			addr := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
			client, conn, err := dial(e.ctx, addr, profile, e.trust.Callback(addr, false))
			if err != nil {
				e.finish(id, index, "failed", Result{Error: "未执行：" + err.Error()})
				return
			}
			defer client.Close()
			defer conn.Close()
			result, transportErr := runWithDeadline(e.ctx, client, conn, Command{ID: "action-" + id, Name: "一次性执行", Shell: shell}, e.timeout)
			status := "succeeded"
			if transportErr != nil || result.ExitCode == nil {
				status = "unknown"
				if transportErr != nil {
					result.Error = fmt.Sprintf("执行结果未知：%v；不会自动重试", transportErr)
				}
			} else if *result.ExitCode != 0 {
				status = "failed"
			}
			e.finish(id, index, status, trimExecutionResult(result))
		}(index, target, profiles[index])
	}
	workers.Wait()
	e.mu.Lock()
	now := time.Now()
	e.jobs[id].FinishedAt = &now
	e.active = false
	e.mu.Unlock()
}
func (e *Executor) finish(id string, index int, status string, result Result) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.jobs[id].Results[index].Status = status
	e.jobs[id].Results[index].Result = result
}
