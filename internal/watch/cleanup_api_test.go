package watch

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCleanupAPIIdentityAuthorizationAndLogs(t *testing.T) {
	m, mon, _, _, _ := cleanupFixture(t)
	app := NewServer(m.store, mon, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	app.SetServiceInfo(ServiceInfo{RunID: "run-one"})
	defer app.executor.Close()
	call := func(method, path, token, run, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:9999"+path, strings.NewReader(body))
		r.Header.Set("X-Watch-Token", token)
		r.Header.Set("X-Watch-Instance", run)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		token, run string
		code       int
	}{{"wrong", "run-one", 403}, {app.token, "old-run", 409}} {
		if w := call("POST", "/api/tasks/cleanup", tc.token, tc.run, `{"enabled":false}`); w.Code != tc.code {
			t.Fatal(w.Code)
		}
	}
	if w := call("POST", "/api/tasks/cleanup", app.token, "run-one", `{"enabled":true}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call("POST", "/api/tasks/cleanup?id=some-log", app.token, "run-one", `{"enabled":false}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/api/tasks/cleanup", app.token, "run-one", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"minimumMachines":4`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := m.CheckCleanup(); err != nil {
		t.Fatal(err)
	}
	for id := range m.cleanup.Audits {
		if w := call("GET", "/api/tasks/cleanup?id="+id, app.token, "run-one", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "baseline") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := call("POST", "/api/tasks/cleanup", app.token, "run-one", `{"enabled":false}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if err := m.loadCleanup(); err != nil || m.cleanup.Policy.Enabled {
		t.Fatal("policy not durable", err)
	}
}
