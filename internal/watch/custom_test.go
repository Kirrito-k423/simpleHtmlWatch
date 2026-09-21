package watch

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/crypto/ssh"
)

func TestCustomCommandValidation(t *testing.T) {
	for _, c := range []CustomCommand{{}, {Shell: "ps -ef | grep tilexr", Enabled: true}, {Shell: "watch -n 4 ps -ef", Enabled: true}, {Shell: "/usr/bin/watch ps -ef"}, {Shell: "printf '%s\\n' \"a'b\" | grep tilexr", Enabled: true}} {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []CustomCommand{{Enabled: true}, {Shell: "  ", Enabled: true}, {Shell: "watch --exec ps -ef", Enabled: true}, {Shell: "watch"}, {Shell: strings.Repeat("x", 4097)}, {Shell: "ps\x00-ef"}} {
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
	r := httptest.NewRequest("PUT", "http://127.0.0.1:9999/api/custom-commands", bytes.NewReader(b))
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
	value := CustomCommand{ID: "custom-one", Name: "进程", Shell: "ps -ef | grep 'tilexr'", Enabled: true}
	putCustom(t, app, value, "bad", 403)
	putCustom(t, app, CustomCommand{Enabled: true}, app.token, 400)
	if f.customRuns.Load() != 0 {
		t.Fatal("rejected request executed command")
	}
	putCustom(t, app, value, app.token, 200)
	s := waitState(t, app.monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	expected := `bash -o pipefail -lc 'ps -ef | grep '"'"'tilexr'"'"''`
	if s.Status != "online" || len(s.Results) != 2 || s.Results[0].CommandID != "custom-one" || s.Results[0].Output != expected || s.Results[1].CommandID != "python" {
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
	if len(saved.CustomCommands) != 1 || saved.CustomCommands[0] != value || saved.Profiles[0].Password != original.Profiles[0].Password || len(saved.Machines) != 2 {
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
	if f.customRuns.Load() != 2 || len(s.Results) != 1 || s.Results[0].CommandID != "python" || store.Snapshot().CustomCommands[0] != value {
		t.Fatalf("stop did not retain shell / stop custom / preserve built-ins: %+v", s)
	}
}

func TestCustomCommandDisconnectDoesNotReplayWithinSample(t *testing.T) {
	f := newFixture(t)
	f.dropCustom.Store(true)
	app, _, _ := customApp(t, f)
	putCustom(t, app, CustomCommand{ID: "custom-one", Name: "进程", Shell: "ps -ef | grep tilexr", Enabled: true}, app.token, 200)
	s := waitState(t, app.monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if s.Status != "offline" || s.Reconnects != 0 || f.customRuns.Load() != 1 {
		t.Fatalf("custom command replayed after uncertain transport error: %+v", s)
	}
}

func TestMultipleCustomCommandsIndependentLifecycle(t *testing.T) {
	f := newFixture(t)
	app, store, _ := customApp(t, f)
	one := CustomCommand{ID: "custom-one", Name: "tilexr", Shell: "ps -ef | grep tilexr", Enabled: true}
	two := CustomCommand{ID: "custom-two", Name: "other", Shell: "ps -ef | grep tilexr-other", Enabled: true}
	sample := func() State {
		return waitState(t, app.monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	}
	putCustom(t, app, one, app.token, 200)
	sample()
	putCustom(t, app, two, app.token, 200)
	s := sample()
	if len(s.Results) != 3 || s.Results[0].CommandID != one.ID || s.Results[1].CommandID != two.ID || !strings.Contains(s.Results[1].Output, "tilexr-other") {
		t.Fatalf("multiple results mixed: %+v", s)
	}
	one.Enabled = false
	one.Name = "renamed"
	putCustom(t, app, one, app.token, 200)
	s = sample()
	if len(s.Results) != 2 || s.Results[0].CommandID != two.ID {
		t.Fatalf("stopping one affected another: %+v", s)
	}
	reopened, err := NewStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.Snapshot().CustomCommands, []CustomCommand{one, two}) {
		t.Fatal("multiple commands not persisted")
	}
	b, _ := json.Marshal(CustomCommand{ID: one.ID})
	r := httptest.NewRequest("DELETE", "http://127.0.0.1:9999/api/custom-commands", bytes.NewReader(b))
	r.Header.Set("X-Watch-Token", app.token)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s = sample()
	if len(store.Snapshot().CustomCommands) != 1 || len(s.Results) != 2 || s.Results[0].CommandID != two.ID {
		t.Fatal("delete affected another command")
	}
}

func TestLegacyCustomCommandMigration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		dir := t.TempDir()
		store, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := testConfig()
		c.CustomCommand = &CustomCommand{Shell: "ps -ef | grep tilexr", Enabled: enabled}
		// Write the old encrypted schema directly, bypassing the new Save migration.
		plain, _ := json.Marshal(c)
		encrypted, err := store.encrypt(plain)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "config.enc"), encrypted, 0600); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		saved := reopened.Snapshot()
		if saved.CustomCommand != nil || len(saved.CustomCommands) != 1 || saved.CustomCommands[0].Shell != c.CustomCommand.Shell || saved.CustomCommands[0].Enabled != enabled || saved.CustomCommands[0].ID != "custom-legacy" {
			t.Fatalf("migration lost old command: %+v", saved.CustomCommands)
		}
		if err = reopened.Save(saved); err != nil {
			t.Fatal(err)
		}
		again, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(again.Snapshot().CustomCommands) != 1 {
			t.Fatal("migration duplicated command")
		}
	}
}

func TestMultipleCustomCommandsValidation(t *testing.T) {
	valid := CustomCommand{ID: "custom-one", Name: "one", Shell: "ps -ef", Enabled: true}
	for _, list := range [][]CustomCommand{{valid, valid}, {{ID: "npu", Name: "collision", Shell: "ps"}}, {{ID: "custom-one", Name: "", Shell: "ps"}}, {{ID: "custom-one", Name: "one", Enabled: true}}, make([]CustomCommand, 33)} {
		c := testConfig()
		c.CustomCommands = list
		if c.Validate() == nil {
			t.Fatalf("invalid list accepted: %+v", list)
		}
	}
}
