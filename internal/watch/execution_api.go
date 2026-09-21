package watch

import "net/http"

func (s *Server) executionAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		if id := r.URL.Query().Get("id"); id != "" {
			job, ok := s.executor.Get(id)
			if !ok {
				fail(w, 404, "执行记录不存在或已过期")
				return
			}
			jsonResponse(w, 200, job)
		} else {
			jsonResponse(w, 200, s.executor.List())
		}
	case "POST":
		var request ExecutionRequest
		if err := decode(w, r, &request); err != nil {
			fail(w, 400, "执行参数无效")
			return
		}
		job, status, err := s.executor.Start(request, s.store.Snapshot())
		if err != nil {
			fail(w, status, err.Error())
			return
		}
		jsonResponse(w, status, job)
	default:
		w.WriteHeader(405)
	}
}
