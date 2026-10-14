package watch

import "net/http"

func (s *Server) continuationsAPI(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		fail(w, 503, "任务中台未启用")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/continuations" {
		if id := r.URL.Query().Get("id"); id != "" {
			c, found := s.tasks.GetContinuation(id)
			if !found {
				fail(w, 404, "CONTINUATION_NOT_FOUND")
				return
			}
			jsonResponse(w, 200, c)
		} else {
			s.tasks.mu.Lock()
			diagnostic := s.tasks.continuationError
			s.tasks.mu.Unlock()
			jsonResponse(w, 200, map[string]any{"schema": continuationStoreSchema, "continuations": s.tasks.Continuations(), "schedulerError": diagnostic})
		}
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var c Continuation
	var status int
	var err error
	switch r.URL.Path {
	case "/api/continuations":
		var v ContinuationRequest
		if err = decode(w, r, &v); err == nil {
			c, status, err = s.tasks.RegisterContinuation(v)
		}
	case "/api/continuations/claim":
		var v ContinuationClaim
		if err = decode(w, r, &v); err == nil {
			c, status, err = s.tasks.ClaimContinuation(v)
		}
	case "/api/continuations/report":
		var v ContinuationReport
		if err = decode(w, r, &v); err == nil {
			c, status, err = s.tasks.ReportContinuation(v)
		}
	case "/api/continuations/control", "/api/continuations/evaluate":
		var v ContinuationControl
		if err = decode(w, r, &v); err == nil {
			if r.URL.Path == "/api/continuations/control" {
				c, status, err = s.tasks.ControlContinuation(v)
			} else {
				c, status, err = s.tasks.EvaluateContinuation(v)
			}
		}
	case "/api/continuations/delivery":
		var v ContinuationDelivery
		if err = decode(w, r, &v); err == nil {
			c, status, err = s.tasks.DeliveryContinuation(v)
		}
	default:
		w.WriteHeader(404)
		return
	}
	if err != nil {
		if status == 0 {
			status = 400
		}
		fail(w, status, err.Error())
		return
	}
	jsonResponse(w, status, c)
}
