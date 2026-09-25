package watch

import (
	"net/http"
	"os"
	"strings"
)

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
		job, err := s.tasks.Collect(id)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		jsonResponse(w, 200, job)
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
		if r.Method != "GET" || id == "" {
			fail(w, 400, "请提供任务 ID")
			return
		}
		job, ok := s.tasks.Get(id)
		if !ok || !job.ArchiveReady {
			fail(w, 404, "结果包尚未回收")
			return
		}
		path := s.tasks.path(id, "result.tar.gz")
		if _, err := os.Stat(path); err != nil {
			fail(w, 404, "结果包文件不存在")
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(id, "\"", "")+".tar.gz\"")
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, path)
	default:
		w.WriteHeader(404)
	}
}
