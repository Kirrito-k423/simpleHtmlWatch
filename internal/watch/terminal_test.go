package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func terminalTestServer(t *testing.T) (*Server, *fixture, *httptest.Server, Config) {
	t.Helper()
	f := newFixture(t)
	f.terminalEnabled.Store(true)
	app, _, config := customApp(t, f)
	srv := httptest.NewUnstartedServer(app)
	app.host = srv.Listener.Addr().String()
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(app.Close)
	return app, f, srv, config
}
func terminalTestDial(t *testing.T, app *Server, srv *httptest.Server, auth terminalMessage) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ssh", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(auth)
	if err = c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}
func terminalReadUntil(t *testing.T, c *websocket.Conn, needle string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var output strings.Builder
	for !strings.Contains(output.String(), needle) {
		kind, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read terminal %q: %v (received %q)", needle, err, output.String())
		}
		if kind == websocket.MessageBinary {
			output.Write(raw)
			ack, _ := json.Marshal(terminalMessage{Type: "ack", Bytes: len(raw)})
			if err = c.Write(ctx, websocket.MessageText, ack); err != nil {
				t.Fatal(err)
			}
		} else {
			output.Write(raw)
		}
	}
	return output.String()
}

func TestTerminalPTYInputResizeAndDisconnectWithoutDiskTranscript(t *testing.T) {
	app, f, srv, config := terminalTestServer(t)
	before, err := os.ReadDir(app.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	c := terminalTestDial(t, app, srv, terminalMessage{Type: "auth", Token: app.token, MachineID: config.Machines[0].ID, Rows: 24, Cols: 80})
	terminalReadUntil(t, c, "fixture$")
	// Live shells outlast the HTTP request timeouts without reconnect/replay.
	time.Sleep(150 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, message := range []terminalMessage{{Type: "resize", Rows: 35, Cols: 120}, {Type: "input", Data: "pwd\r\x03"}} {
		raw, _ := json.Marshal(message)
		if err = c.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
	}
	terminalReadUntil(t, c, "pwd\r\x03")
	if f.terminalRows.Load() != 35 || f.terminalCols.Load() != 120 || f.terminalRuns.Load() != 1 {
		t.Fatal("PTY size or shell launch mismatch")
	}
	_ = c.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for len(app.terminals.slots) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(app.terminals.slots) != 0 {
		t.Fatal("closed window retained its SSH session")
	}
	after, err := os.ReadDir(app.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("terminal created local transcript files")
	}
}

func TestTerminalRejectsUnauthenticatedCrossOriginAndDisabledMachines(t *testing.T) {
	app, f, srv, config := terminalTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, origin := range []string{"http://evil.example", "null", ""} {
		_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ssh", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
		if err == nil || response.StatusCode != 403 {
			t.Fatal("foreign or absent origin reached terminal")
		}
	}
	for _, auth := range []terminalMessage{
		{Type: "auth", Token: "bad", MachineID: config.Machines[0].ID, Rows: 24, Cols: 80},
		{Type: "auth", Token: app.token, MachineID: "disabled", Rows: 24, Cols: 80},
		{Type: "auth", Token: app.token, MachineID: config.Machines[0].ID, Rows: 0, Cols: 80},
	} {
		c := terminalTestDial(t, app, srv, auth)
		for {
			_, _, err := c.Read(ctx)
			if err != nil {
				break
			}
		}
	}
	if f.authenticated.Load() != 0 || f.terminalRuns.Load() != 0 {
		t.Fatal("rejected terminal authenticated SSH")
	}
}

func TestTerminalFencesOldPagesAndRefusesUntrustedHost(t *testing.T) {
	app, f, srv, config := terminalTestServer(t)
	app.SetServiceInfo(ServiceInfo{RunID: "new-run"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := terminalTestDial(t, app, srv, terminalMessage{Type: "auth", Token: app.token, RunID: "old-run", MachineID: config.Machines[0].ID, Rows: 24, Cols: 80})
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("old run opened a shell")
	}
	trust, _ := NewTrustStore(t.TempDir())
	app.trust = trust
	c = terminalTestDial(t, app, srv, terminalMessage{Type: "auth", Token: app.token, RunID: "new-run", MachineID: config.Machines[0].ID, Rows: 24, Cols: 80})
	terminalReadUntil(t, c, "SSH 连接失败")
	if f.terminalRuns.Load() != 0 {
		t.Fatal("untrusted host opened a shell")
	}
}

func TestTerminalCapacityAndServerCloseCancelAllSessions(t *testing.T) {
	app, _, srv, config := terminalTestServer(t)
	for i := 0; i < 4; i++ {
		c := terminalTestDial(t, app, srv, terminalMessage{Type: "auth", Token: app.token, MachineID: config.Machines[0].ID, Rows: 24, Cols: 80})
		terminalReadUntil(t, c, "fixture$")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ssh", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {srv.URL}}})
	if err == nil || response.StatusCode != 429 {
		t.Fatal("terminal limit bypassed")
	}
	done := make(chan struct{})
	go func() { app.terminals.Close(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("server close retained terminals")
	}
}

func TestTerminalOutputWaitsForBrowserAcknowledgment(t *testing.T) {
	app, _, srv, config := terminalTestServer(t)
	c := terminalTestDial(t, app, srv, terminalMessage{Type: "auth", Token: app.token, MachineID: config.Machines[0].ID, Rows: 24, Cols: 80})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	promptSize, connected := 0, false
	for promptSize == 0 || !connected {
		kind, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if kind == websocket.MessageBinary {
			promptSize = len(raw)
		} else if strings.Contains(string(raw), `"state":"connected"`) {
			connected = true
		}
	}
	input, _ := json.Marshal(terminalMessage{Type: "input", Data: "x"})
	if err := c.Write(ctx, websocket.MessageText, input); err != nil {
		t.Fatal(err)
	}
	type chunk struct {
		kind websocket.MessageType
		raw  []byte
		err  error
	}
	next := make(chan chunk, 1)
	go func() { kind, raw, err := c.Read(ctx); next <- chunk{kind, raw, err} }()
	select {
	case got := <-next:
		t.Fatalf("output advanced without acknowledgment: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	ack, _ := json.Marshal(terminalMessage{Type: "ack", Bytes: promptSize})
	if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
		t.Fatal(err)
	}
	got := <-next
	if got.err != nil || got.kind != websocket.MessageBinary || string(got.raw) != "x" {
		t.Fatalf("acknowledged output: %+v", got)
	}
}
