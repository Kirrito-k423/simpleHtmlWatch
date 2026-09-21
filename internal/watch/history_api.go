package watch

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) historyAPI(w http.ResponseWriter, r *http.Request) {
	h := s.monitor.history
	if h == nil {
		fail(w, 503, "历史存储未初始化")
		return
	}
	switch r.URL.Path {
	case "/api/history/status":
		if r.Method == "POST" {
			var v struct {
				Enabled bool `json:"enabled"`
			}
			if err := decode(w, r, &v); err != nil {
				fail(w, 400, "记录参数无效")
				return
			}
			h.SetEnabled(v.Enabled)
			if v.Enabled {
				s.monitor.Refresh()
			}
		} else if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		jsonResponse(w, 200, h.Status())
	case "/api/history/catalog":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		jsonResponse(w, 200, h.Catalog())
	case "/api/history/sample":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		record, err := h.Get(r.URL.Query().Get("id"))
		if err != nil {
			fail(w, 404, "记录不存在或已过期")
			return
		}
		jsonResponse(w, 200, record)
	case "/api/history/frames":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		q := r.URL.Query()
		start, err := time.Parse(time.RFC3339, q.Get("start"))
		seconds, e := strconv.Atoi(q.Get("seconds"))
		if err != nil || e != nil || seconds < 60 || seconds > 3600 {
			fail(w, 400, "时间范围无效")
			return
		}
		jsonResponse(w, 200, h.Frames(start, time.Duration(seconds)*time.Second, q.Get("machine")))
	case "/api/history/matrix":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		q := r.URL.Query()
		start, err := time.Parse(time.RFC3339, q.Get("start"))
		seconds, e := strconv.Atoi(q.Get("seconds"))
		ids := strings.Split(q.Get("machines"), ",")
		rule := q.Get("rule")
		if err != nil || e != nil || seconds < 60 || seconds > 3600 || len(ids) > 16 || len(q.Get("keyword")) > 256 || (rule != "none" && rule != "change" && rule != "contains" && rule != "npu") || (rule == "contains" && q.Get("keyword") == "") {
			fail(w, 400, "历史范围/规则无效（60–3600 秒，最多 16 台）")
			return
		}
		rows, err := h.QueryContext(r.Context(), start, time.Duration(seconds)*time.Second, ids, q.Get("command"), rule, q.Get("keyword"))
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		jsonResponse(w, 200, rows)
	default:
		w.WriteHeader(404)
	}
}
