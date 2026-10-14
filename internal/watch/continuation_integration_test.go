package watch

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in subprocess fixture: empty temporary store, synthetic NPU, fake SSH.
// No fixture routes or registered machines are compiled into release binaries.
func TestContinuationIntegrationFixture(t *testing.T) {
	if os.Getenv("SHW_CONTINUATION_FIXTURE") != "1" {
		t.Skip("opt-in local fixture")
	}
	remote := &fakeTaskRemote{code: "done:0"}
	m, monitor, config, store := setupTasks(t, remote)
	second := config.Machines[0]
	second.ID, second.Name, second.Host = "second", "second", "192.0.2.42"
	config.Machines = append(config.Machines, second)
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	refreshContinuationSamples(monitor, config, time.Now())
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	app := NewServer(store, monitor, nil, os.DirFS(filepath.Join("..", "..", "web")), listener.Addr().String())
	app.tasks = m
	defer app.Close()
	info := ServiceInfo{Schema: ServiceSchema, APIVersion: ServiceAPIVersion, Instance: "test", ServiceID: "fixture-service", RunID: "fixture-run", Version: "fixture", BuildID: "fixture-build", URL: "http://" + listener.Addr().String(), DataDir: store.dir, Capabilities: []string{"tasks", "reservations", "experiment-continuations-v1"}}
	app.SetServiceInfo(info)
	m.startContinuations()
	stopSamples := make(chan struct{})
	defer close(stopSamples)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSamples:
				return
			case <-ticker.C:
				refreshContinuationSamples(monitor, config, time.Now())
			}
		}
	}()
	server := &http.Server{ReadHeaderTimeout: time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fixture/count":
			remote.mu.Lock()
			count := remote.launch
			remote.mu.Unlock()
			jsonResponse(w, 200, map[string]int{"launches": count})
		case "/fixture/stop":
			jsonResponse(w, 200, map[string]bool{"stopped": true})
			go server.Close()
		default:
			app.ServeHTTP(w, r)
		}
	})
	b, _ := json.Marshal(info)
	if err := os.WriteFile(os.Getenv("SHW_CONTINUATION_DISCOVERY"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
