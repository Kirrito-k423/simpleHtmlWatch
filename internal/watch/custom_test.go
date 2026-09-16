package watch

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/crypto/ssh"
)

func TestCustomCommandValidation(t *testing.T) {
	for _, c := range []CustomCommand{{}, {Shell: "ps -ef | grep tilexr", Enabled: true}, {Shell: "printf '%s\\n' \"a'b\" | grep tilexr", Enabled: true}} {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []CustomCommand{{Enabled: true}, {Shell: "  ", Enabled: true}, {Shell: "watch -n 4 ps -ef", Enabled: true}, {Shell: "/usr/bin/watch ps -ef"}, {Shell: strings.Repeat("x", 4097)}, {Shell: "ps\x00-ef"}} {
		if c.Validate() == nil {
			t.Fatalf("accepted invalid custom command: %+v", c)
		}
	}
}

func customApp(t *testing.T, f *fixture) (*Server, *Store, Config) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trust, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	addr := f.listener.Addr().String()
	_ = trust.Callback(addr, false)("", f.listener.Addr(), f.signer.PublicKey())
	if err = trust.Accept(addr, ssh.FingerprintSHA256(f.signer.PublicKey())); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(trust)
	t.Cleanup(m.Close)
	c := testConfig()
	c.Interval = 3600
	c.Machines[0].Port = f.listener.Addr().(*net.TCPAddr).Port
	c.Machines[0].Commands = []string{"python"}
	disabled := c.Machines[0]
	disabled.ID = "disabled"
	disabled.Enabled = false
	c.Machines = append(c.Machines, disabled)
	if err = store.Save(c); err != nil {
		t.Fatal(err)
	}
	return NewServer(store, m, trust, fstest.MapFS{}, "127.0.0.1:9999"), store, c
}

func putCustom(t *testing.T, app *Server, value CustomCommand, token string, want int) {
	t.Helper()
	b, _ := json.Marshal(value)
	r := httptest.NewRequest("PUT", "http://127.0.0.1:9999/api/custom-command", bytes.NewReader(b))
	r.Header.Set("X-Watch-Token", token)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("got %d want %d: %s", w.Code, want, w.Body)
	}
	if strings.Contains(w.Body.String(), "only-a-test-password") {
		t.Fatal("password leaked")
	}
}

func TestCustomCommandAPIExecutionPersistenceAndStop(t *testing.T) {
	f := newFixture(t)
	app, store, original := customApp(t, f)
	value := CustomCommand{Shell: "ps -ef | grep 'tilexr'", Enabled: true}
	putCustom(t, app, value, "bad", 403)
	putCustom(t, app, CustomCommand{Enabled: true}, app.token, 400)
	if f.customRuns.Load() != 0 {
		t.Fatal("rejected request executed command")
	}
	putCustom(t, app, value, app.token, 200)
	s := waitState(t, app.monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	expected := `bash -o pipefail -lc 'ps -ef | grep '"'"'tilexr'"'"''`
	if s.Status != "online" || len(s.Results) != 2 || s.Results[0].CommandID != "custom" || s.Results[0].Output != expected || s.Results[1].CommandID != "python" {
		t.Fatalf("custom pipeline / shell quoting lost: %+v", s)
	}
	if f.customRuns.Load() != 1 || app.monitor.Snapshot()["disabled"].Status != "disabled" {
		t.Fatal("disabled machine ran custom command")
	}
	reopened, err := NewStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := reopened.Snapshot()
	if saved.CustomCommand != value || saved.Profiles[0].Password != original.Profiles[0].Password || len(saved.Machines) != 2 {
		t.Fatal("custom save lost command, shared password, or machines")
	}
	app.monitor.Refresh()
	waitState(t, app.monitor, func(states map[string]State) bool { return states["m1"].UpdatedAt.After(s.UpdatedAt) })
	if f.customRuns.Load() != 2 {
		t.Fatal("custom command not collected again")
	}
	value.Enabled = false
	putCustom(t, app, value, app.token, 200)
	s = waitState(t, app.monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if f.customRuns.Load() != 2 || len(s.Results) != 1 || s.Results[0].CommandID != "python" || store.Snapshot().CustomCommand != value {
		t.Fatalf("stop did not retain shell / stop custom / preserve built-ins: %+v", s)
	}
}

func TestCustomCommandDisconnectDoesNotReplayWithinSample(t *testing.T) {
	f := newFixture(t)
	f.dropCustom.Store(true)
	app, _, _ := customApp(t, f)
	putCustom(t, app, CustomCommand{Shell: "ps -ef | grep tilexr", Enabled: true}, app.token, 200)
	s := waitState(t, app.monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "offline" || s.Reconnects != 0 || f.customRuns.Load() != 1 {
		t.Fatalf("custom command replayed after uncertain transport error: %+v", s)
	}
}
