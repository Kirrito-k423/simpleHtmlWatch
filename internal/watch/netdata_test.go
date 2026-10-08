package watch

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const metricFixtureNode = "52e10daa-d9d0-42e1-8888-11974aa054ba"

func metricPayload(after, before int64) map[string]any {
	return map[string]any{
		"db":     map[string]any{"update_every": 1, "last_entry": before},
		"view":   map[string]any{"units": "%", "update_every": 5, "after": after, "before": before, "dimensions": map[string]any{"units": []string{"%", "%"}}},
		"result": map[string]any{"labels": []string{"time", "user", "system"}, "data": [][]any{{before, 7.5, 0}, {before - 5, nil, 1}, {after, 12, 2}}},
	}
}

// Real SSH direct-tcpip framing to an isolated local HTTP fixture. No configured
// user servers or credentials are read; all fixtures bind ephemeral loopback.
func metricsSSH(t *testing.T, handler http.Handler) (Machine, Profile, *TrustStore, *atomic.Int32) {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	httpAddr := strings.TrimPrefix(httpServer.URL, "http://")
	_, rawPort, _ := net.SplitHostPort(httpAddr)
	port, _ := strconv.Atoi(rawPort)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sshConfig := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if c.User() != "fixture" || string(password) != "fixture-password" {
			return nil, fmt.Errorf("fixture authentication failed")
		}
		return nil, nil
	}}
	sshConfig.AddHostKey(signer)
	var wg sync.WaitGroup
	var connections sync.Map
	forwarded := &atomic.Int32{}
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, true)
			wg.Go(func() {
				defer conn.Close()
				defer connections.Delete(conn)
				server, channels, requests, err := ssh.NewServerConn(conn, sshConfig)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					var target struct {
						Host       string
						Port       uint32
						OriginHost string
						OriginPort uint32
					}
					if channel.ChannelType() != "direct-tcpip" || ssh.Unmarshal(channel.ExtraData(), &target) != nil || target.Host != "127.0.0.1" || int(target.Port) != port {
						_ = channel.Reject(ssh.Prohibited, "fixture permits only Netdata loopback")
						continue
					}
					forwarded.Add(1)
					upstream, err := net.Dial("tcp", httpAddr)
					if err != nil {
						_ = channel.Reject(ssh.ConnectionFailed, "HTTP fixture unavailable")
						continue
					}
					ch, req, err := channel.Accept()
					if err != nil {
						upstream.Close()
						continue
					}
					go ssh.DiscardRequests(req)
					wg.Go(func() {
						defer upstream.Close()
						defer ch.Close()
						copied := make(chan struct{})
						go func() { _, _ = io.Copy(upstream, ch); _ = upstream.(*net.TCPConn).CloseWrite(); close(copied) }()
						_, _ = io.Copy(ch, upstream)
						ch.Close()
						upstream.Close()
						<-copied
					})
				}
			})
		}
	})
	t.Cleanup(func() {
		listener.Close()
		connections.Range(func(k, _ any) bool { k.(net.Conn).Close(); return true })
		wg.Wait()
	})
	host, rawSSHPort, _ := net.SplitHostPort(listener.Addr().String())
	sshPort, _ := strconv.Atoi(rawSSHPort)
	trust, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trust.keys[listener.Addr().String()] = ssh.FingerprintSHA256(signer.PublicKey())
	return Machine{ID: "metrics-machine", Name: "指标测试", Host: host, Port: sshPort, ProfileID: "fixture", Enabled: true, Commands: []string{"python"}, Netdata: &NetdataConfig{Enabled: true, Port: port, Token: "secret-fixture-token"}}, Profile{ID: "fixture", Name: "fixture", Username: "fixture", Password: "fixture-password"}, trust, forwarded
}

func TestMetricsSSHNodeScopeAndPartialData(t *testing.T) {
	var requests atomic.Int32
	after, before := time.Now().Unix()-900, time.Now().Unix()
	machine, profile, trust, forwarded := metricsSSH(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret-fixture-token" || r.Method != "GET" {
			t.Errorf("request did not use the scoped Bearer token/read-only method")
		}
		if r.URL.Path == "/api/v1/info" {
			jsonResponse(w, 200, map[string]string{"uid": metricFixtureNode, "version": "fixture-v3"})
			return
		}
		if r.URL.Path != "/api/v3/data" || r.URL.Query().Get("scope_nodes") != metricFixtureNode || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("after") != strconv.FormatInt(after, 10) {
			t.Errorf("unsafe or malformed metric query: %s", r.URL)
		}
		if r.URL.Query().Get("contexts") == "system.cpu" && r.URL.Query().Get("dimensions") != "!idle|*" {
			t.Error("CPU dimension expression must use the v3 API pipe separator")
		}
		if r.URL.Query().Get("contexts") == "system.ram" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte("agent response must not leak secret-fixture-token"))
			return
		}
		jsonResponse(w, 200, metricPayload(after, before))
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := NewMetrics(trust).Query(ctx, machine, profile, after, before)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 5 || forwarded.Load() == 0 || out.NodeID != metricFixtureNode || len(out.Charts) != 4 {
		t.Fatalf("missing scoped SSH queries: %+v (%d requests)", out, requests.Load())
	}
	if out.Charts[1].Error == "" || strings.Contains(out.Charts[1].Error, machine.Netdata.Token) {
		t.Fatal("single chart failure or response-body secrecy lost")
	}
	points := out.Charts[0].Series[0].Points
	if points[0].At != after || points[1].Value != nil || points[2].Value == nil || *out.Charts[0].Series[1].Points[2].Value != 0 {
		t.Fatal("descending timestamps/nulls/real zero normalized incorrectly")
	}
}

func TestMetricsTrustAndDeadline(t *testing.T) {
	machine, profile, trust, _ := metricsSSH(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := NewMetrics(trust).Query(ctx, machine, profile, 1, 2); err == nil {
		t.Fatal("stalled HTTP request did not fail")
	}
	if time.Since(start) > time.Second {
		t.Fatal("metric query did not honor cancellation")
	}
	clear(trust.keys)
	ctx2, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := NewMetrics(trust).Query(ctx2, machine, profile, 1, 2); err == nil || !strings.Contains(err.Error(), "主机指纹") {
		t.Fatalf("untrusted metrics connection bypassed trust: %v", err)
	}
}

func TestNetdataConfigSecretsAndMigration(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Machines[0].Netdata != nil {
		t.Fatal("legacy configuration enabled Agent collection")
	}
	c.Machines[0].Netdata = &NetdataConfig{Enabled: true, Token: "saved-bearer-secret"}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	public := store.Public()
	b, _ := json.Marshal(public)
	if bytes.Contains(b, []byte("saved-bearer-secret")) || !public.Machines[0].Netdata.HasToken || public.Machines[0].Netdata.Port != 19999 {
		t.Fatal("public config exposes token or loses default port")
	}
	if err := store.Save(public); err != nil || store.Snapshot().Machines[0].Netdata.Token != "saved-bearer-secret" {
		t.Fatal("blank token edit erased secret")
	}
	public.Machines[0].Host = "192.0.2.90"
	if err := store.Save(public); err != nil || store.Snapshot().Machines[0].Netdata.Token != "" {
		t.Fatal("token reused on a different host")
	}
	public.Machines[0].Netdata.Token = "new-secret"
	_ = store.Save(public)
	public = store.Public()
	public.Machines[0].Netdata.ClearToken = true
	if err := store.Save(public); err != nil || store.Snapshot().Machines[0].Netdata.Token != "" {
		t.Fatal("explicit token clearing did not work")
	}
	for _, n := range []NetdataConfig{{Port: -1}, {Port: 65536}, {Token: "header\r\ninjection"}, {Token: strings.Repeat("a", 4097)}} {
		if n.Validate() == nil {
			t.Fatal("unsafe Netdata config accepted")
		}
	}
}

func TestNetdataResponseBoundsAndRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	for _, mode := range []string{"large", "redirect", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "large":
					_, _ = w.Write([]byte(strings.Repeat(" ", maxMetricResponse+1)))
				case "redirect":
					http.Redirect(w, r, destination.URL, 302)
				default:
					_, _ = w.Write([]byte(`{"broken":`))
				}
			}))
			defer server.Close()
			client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
			}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			defer client.CloseIdleConnections()
			var data any
			if _, err := netdataJSON(context.Background(), client, "secret", "/api/v1/info", nil, &data); err == nil {
				t.Fatal("invalid/oversized/redirect response accepted")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("Bearer request followed a redirect")
	}
}

func TestMetricsAPIWindowDisabledAndTaskIdentity(t *testing.T) {
	m, monitor, c, store := setupTasks(t, &fakeTaskRemote{code: "lost"})
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	var connects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/info" {
			jsonResponse(w, 200, map[string]string{"uid": metricFixtureNode})
			return
		}
		query := r.URL.Query()
		after, _ := strconv.ParseInt(query.Get("after"), 10, 64)
		before, _ := strconv.ParseInt(query.Get("before"), 10, 64)
		jsonResponse(w, 200, metricPayload(after, before))
	}))
	defer server.Close()
	app.metrics.connect = func(ctx context.Context, machine Machine, p Profile) (*http.Client, func(), error) {
		connects.Add(1)
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
		}}
		return &http.Client{Transport: transport}, transport.CloseIdleConnections, nil
	}
	call := func(query string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "http://127.0.0.1:9999/api/metrics?"+query, nil)
		r.Header.Set("X-Watch-Token", app.token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: got %d, want %d: %s", query, w.Code, want, w.Body.String())
		}
		return w
	}
	id := c.Machines[0].ID
	call("machineId="+id, 409)
	call("machineId="+id+"&url=http://other", 400)
	if connects.Load() != 0 {
		t.Fatal("disabled metrics or arbitrary target opened a connection")
	}
	c.Machines[0].Netdata = &NetdataConfig{Enabled: true}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	call("machineId="+id+"&after=1", 400)
	call("machineId="+id+"&after=1&before=604802", 400)
	call("machineId="+id+"&after=10&before=10", 400)
	call("machineId="+id+"&machineId=another", 400)
	call("machineId="+id, 200)
	now := time.Now()
	start, finish := now.Add(-8*24*time.Hour), now.Add(-time.Minute)
	job := &TaskJob{TaskRequest: TaskRequest{ID: "metrics-task"}, SelectedMachineID: id, Host: c.Machines[0].Host, Port: c.Machines[0].Port, Username: c.Profiles[0].Username, CreatedAt: start, FinishedAt: &finish}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()
	w := call("taskId=metrics-task", 200)
	var out MetricsSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.WindowClipped || out.Task.ID != job.ID || out.Before-out.After != maxMetricWindow {
		t.Fatalf("task time window or clipping lost: %s", w.Body.String())
	}
	call("taskId=metrics-task&machineId=another", 400)
	c.Machines[0].Host = "192.0.2.99"
	_ = store.Save(c)
	call("taskId=metrics-task", 409)
}

func TestMetricsMalformedRows(t *testing.T) {
	for _, rows := range []string{`[[1,2]]`, `[[null,2,3]]`, `[[1.5,2,3]]`, `[[1,2,3],[1,3,4]]`, `[[1,"not-a-number",3]]`} {
		var data netdataData
		input := `{"db":{"update_every":1},"view":{"units":"%","update_every":1},"result":{"labels":["time","a","b"],"data":` + rows + `}}`
		if err := json.Unmarshal([]byte(input), &data); err == nil {
			if err := data.normalize(&MetricChart{}); err == nil {
				t.Fatalf("bad row accepted: %s", rows)
			}
		}
	}
}

func TestMetricsMacIPv4FallbackScope(t *testing.T) {
	for _, mode := range []string{"macOS", "Linux", "denied"} {
		t.Run(mode, func(t *testing.T) {
			var ipv4Requests atomic.Int32
			now := time.Now().Unix()
			machine, profile, trust, _ := metricsSSH(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/info" {
					osName := mode
					if mode == "denied" {
						osName = "macOS"
					}
					jsonResponse(w, 200, map[string]string{"uid": metricFixtureNode, "os_name": osName})
					return
				}
				switch r.URL.Query().Get("contexts") {
				case "system.net":
					if mode == "denied" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					// Actual v2.12.0 response shape for an unavailable Mac context.
					jsonResponse(w, 200, map[string]any{"db": map[string]any{"update_every": 0}, "view": map[string]any{"update_every": 5}, "result": map[string]any{"labels": []string{"time", "no data"}, "data": [][]any{}}})
				case "system.ipv4":
					ipv4Requests.Add(1)
					jsonResponse(w, 200, metricPayload(now-300, now))
				default:
					jsonResponse(w, 200, metricPayload(now-300, now))
				}
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := NewMetrics(trust).Query(ctx, machine, profile, now-300, now)
			if err != nil {
				t.Fatal(err)
			}
			chart := out.Charts[3]
			if mode == "macOS" {
				if ipv4Requests.Load() != 1 || chart.Context != "system.ipv4" || chart.Name != "网络流量（IPv4）" || chart.Error != "" {
					t.Fatalf("Mac fallback must expose its actual narrower scope: %+v", chart)
				}
			} else if ipv4Requests.Load() != 0 || chart.Context != "system.net" || chart.Error == "" {
				t.Fatalf("Linux or authorization failure must not switch metric source: %+v", chart)
			}
		})
	}
}
