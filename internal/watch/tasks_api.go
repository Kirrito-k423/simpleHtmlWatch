package watch

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// Refresh the deadline for every streamed chunk, rather than imposing the
// ordinary API's 40-second total response budget on a multi-GiB download.
type archiveResponseWriter struct{ http.ResponseWriter }

func (w archiveResponseWriter) Write(p []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(archiveIdleTimeout))
	return w.ResponseWriter.Write(p)
}

func (w archiveResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type archiveStreamWriter struct {
	archiveResponseWriter
	started bool
}

func (w *archiveStreamWriter) Write(p []byte) (int, error) {
	w.started = true
	return w.archiveResponseWriter.Write(p)
}

func (s *Server) tasksAPI(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		fail(w, 503, "任务中台未启用")
		return
	}
	id := r.URL.Query().Get("id")
	if id != "" && !identifier.MatchString(id) {
		fail(w, 400, "任务 ID 无效")
		return
	}
	switch r.URL.Path {
	case "/api/tasks/storage":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		jsonResponse(w, 200, s.tasks.budget.Status())
	case "/api/tasks/cleanup":
		if r.Method == "POST" {
			if id != "" {
				fail(w, 400, "策略写入不接受日志 ID")
				return
			}
			var policy CleanupPolicy
			if err := decode(w, r, &policy); err != nil {
				fail(w, 400, err.Error())
				return
			}
			if err := s.tasks.SetCleanupPolicy(policy); err != nil {
				fail(w, 400, err.Error())
				return
			}
		} else if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		status, err := s.tasks.CleanupStatus(id)
		if err != nil {
			fail(w, 404, err.Error())
			return
		}
		jsonResponse(w, 200, status)
	case "/api/tasks/machines":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		jsonResponse(w, 200, s.tasks.Machines())
	case "/api/tasks/probe":
		if r.Method != "POST" || id == "" {
			fail(w, 400, "请提供任务 ID 并使用 POST 重新探测")
			return
		}
		if job, ok := s.tasks.Get(id); !ok {
			fail(w, 404, "任务不存在")
			return
		} else if job.Status == "dispatching" {
			fail(w, 409, "任务仍在发送，请等待启动结果")
			return
		}
		s.tasks.probe(id)
		job, _ := s.tasks.Get(id)
		jsonResponse(w, 200, job)
	case "/api/tasks/reservations", "/api/tasks/reservations/release":
		if r.Method == "GET" && r.URL.Path == "/api/tasks/reservations" {
			jsonResponse(w, 200, map[string]any{"schema": "simplehtmlwatch.reservations.v1", "atomicGroups": 1, "reservations": s.tasks.Reservations()})
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var request struct {
			ID         string   `json:"id"`
			MachineID  string   `json:"machineId"`
			MachineIDs []string `json:"machineIds,omitempty"`
			TaskID     string   `json:"taskId,omitempty"`
			PlanSHA256 string   `json:"planSha256,omitempty"`
		}
		if err := decode(w, r, &request); err != nil || !identifier.MatchString(request.ID) {
			fail(w, 400, "INVALID_RESERVATION")
			return
		}
		var reservation Reservation
		var status int
		var err error
		if r.URL.Path == "/api/tasks/reservations/release" {
			reservation, status, err = s.tasks.ReleaseReservation(request.ID)
		} else if len(request.MachineIDs) > 0 {
			reservation, status, err = s.tasks.ReserveGroup(GroupReservationRequest{request.ID, request.MachineID, request.MachineIDs, request.TaskID, request.PlanSHA256})
		} else if request.TaskID != "" || request.PlanSHA256 != "" {
			fail(w, 400, "INVALID_GROUP_RESERVATION")
			return
		} else {
			reservation, status, err = s.tasks.Reserve(request.ID, request.MachineID)
		}
		if err != nil {
			fail(w, status, err.Error())
			return
		}
		jsonResponse(w, status, reservation)
	case "/api/tasks/ready":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		jsonResponse(w, 200, s.tasks.Ready(r.URL.Query().Get("machineId"), r.URL.Query().Get("group")))
	case "/api/tasks":
		switch r.Method {
		case "GET":
			if id == "" {
				jsonResponse(w, 200, s.tasks.List())
				return
			}
			job, ok := s.tasks.Get(id)
			if !ok {
				fail(w, 404, "任务不存在")
				return
			}
			jsonResponse(w, 200, job)
		case "POST":
			var request TaskRequest
			if err := decode(w, r, &request); err != nil {
				fail(w, 400, "任务参数无效")
				return
			}
			job, status, err := s.tasks.Submit(request)
			if err != nil {
				fail(w, status, err.Error())
				return
			}
			jsonResponse(w, status, job)
		default:
			w.WriteHeader(405)
		}
	case "/api/tasks/logs":
		if r.Method != "GET" || id == "" {
			fail(w, 400, "请提供任务 ID")
			return
		}
		logs, err := s.tasks.Logs(id)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		jsonResponse(w, 200, logs)
	case "/api/tasks/collect":
		if r.Method != "POST" || id == "" {
			fail(w, 400, "请提供已完成任务 ID")
			return
		}
		fail(w, 410, "已移除本机结果缓存；请用 download 或任务页直接下载远端结果")
	case "/api/tasks/resolve":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var request struct {
			ID      string `json:"id"`
			Confirm string `json:"confirm"`
		}
		if err := decode(w, r, &request); err != nil || !identifier.MatchString(request.ID) || request.Confirm != "remote-stopped" {
			fail(w, 400, "需提供任务 ID，并在独立核实远端已停止后设置 confirm=remote-stopped")
			return
		}
		job, err := s.tasks.Abandon(request.ID)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		jsonResponse(w, 200, job)
	case "/api/tasks/archive":
		if (r.Method != "GET" && r.Method != "POST") || id == "" {
			fail(w, 400, "请提供任务 ID")
			return
		}
		job, ok := s.tasks.Get(id)
		if !ok {
			fail(w, 404, "任务不存在")
			return
		}
		path := s.tasks.path(id, "result.tar.gz")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(id, "\"", "")+".tar.gz\"")
		w.Header().Set("Content-Type", "application/gzip")
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(archiveIdleTimeout))
		if job.ArchiveReady {
			if _, err := os.Stat(path); err == nil {
				http.ServeFile(archiveResponseWriter{w}, r, path)
				return
			} else if !os.IsNotExist(err) {
				fail(w, 500, err.Error())
				return
			}
		}
		if job.FinishedAt == nil || job.ExitCode == nil {
			w.Header().Del("Content-Disposition")
			fail(w, 409, "远端退出结果尚未确认")
			return
		}
		if r.Header.Get("Range") != "" {
			w.Header().Del("Content-Disposition")
			fail(w, 416, "远端流式结果不支持断点续传，请重新下载")
			return
		}
		out := &archiveStreamWriter{archiveResponseWriter: archiveResponseWriter{w}}
		if err := s.tasks.StreamArchive(r.Context(), id, out); err != nil {
			if out.started {
				panic(http.ErrAbortHandler)
			}
			w.Header().Del("Content-Disposition")
			fail(w, 502, err.Error())
		}
	default:
		w.WriteHeader(404)
	}
}
