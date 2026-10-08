package watch

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) metricsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	for k, values := range query {
		if (k != "machineId" && k != "taskId" && k != "after" && k != "before") || len(values) != 1 {
			fail(w, 400, "系统指标查询参数无效")
			return
		}
	}
	id, taskID := query.Get("machineId"), query.Get("taskId")
	if id != "" && !identifier.MatchString(id) || taskID != "" && !identifier.MatchString(taskID) {
		fail(w, 400, "机器或任务 ID 无效")
		return
	}
	var job *TaskJob
	if taskID != "" {
		if s.tasks == nil {
			fail(w, 503, "任务中台未启用")
			return
		}
		v, ok := s.tasks.Get(taskID)
		if !ok {
			fail(w, 404, "任务不存在")
			return
		}
		if id != "" && id != v.SelectedMachineID {
			fail(w, 400, "任务与查询机器不匹配")
			return
		}
		id, job = v.SelectedMachineID, &v
	}
	machine, profile, ok := metricMachine(s.store.Snapshot(), id)
	if !ok {
		fail(w, 404, "机器不存在，请在管理机器中配置")
		return
	}
	if !machine.Enabled || machine.Netdata == nil || !machine.Netdata.Enabled {
		fail(w, 409, "该机器未启用系统指标；安装 Netdata 后在管理机器中启用")
		return
	}
	if job != nil && (job.Host != machine.Host || job.Port != machine.Port || job.Username != profile.Username) {
		fail(w, 409, "机器连接配置已不同于任务执行时，不能将当前指标关联到旧任务")
		return
	}
	now := time.Now().Unix()
	before, after := now, now-900
	if job != nil {
		after = job.CreatedAt.Unix() - 60
		if job.FinishedAt != nil {
			before = min(now, job.FinishedAt.Unix()+60)
		}
	}
	var err error
	if query.Has("after") != query.Has("before") {
		fail(w, 400, "开始与结束时间需同时提供")
		return
	}
	if query.Has("after") {
		after, err = strconv.ParseInt(query.Get("after"), 10, 64)
		if err == nil {
			before, err = strconv.ParseInt(query.Get("before"), 10, 64)
		}
	}
	clipped := false
	if !query.Has("after") && before-after > maxMetricWindow {
		after, clipped = before-maxMetricWindow, true
	}
	if err != nil || after < 1 || before <= after || before > now+60 || before-after > maxMetricWindow {
		fail(w, 400, "查询范围需为有效 Unix 秒，跨度最多 7 天，结束不能晚于当前时间")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	out, err := s.metrics.Query(ctx, machine, profile, after, before)
	if err != nil {
		status := 502
		if errors.Is(err, errMetricsBusy) {
			status = 429
		}
		fail(w, status, sanitizeMetricError(err, machine.Netdata))
		return
	}
	current, currentProfile, ok := metricMachine(s.store.Snapshot(), id)
	if !ok || !sameMetricsTarget(machine, current, profile, currentProfile) {
		fail(w, 409, "查询期间机器配置已变化，请重新查询系统指标")
		return
	}
	out.WindowClipped = clipped
	if job != nil {
		out.Task = &MetricTask{ID: job.ID, Start: job.CreatedAt, End: job.FinishedAt, Events: job.Events}
	}
	jsonResponse(w, 200, out)
}
