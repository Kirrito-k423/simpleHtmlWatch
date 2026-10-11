package watch

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

type groupBridgeRemote struct {
	fakeTaskRemote
	ids []string
}

func (f *groupBridgeRemote) Collect(_ context.Context, _ TaskJob, _ Profile, file string) error {
	members := []any{}
	for i, id := range f.ids {
		ranks := []any{}
		for j := 0; j < 8; j++ {
			ranks = append(ranks, map[string]any{"rank": i*8 + j, "exit_code": 0, "process_id": fmt.Sprintf("fixture-%d-%d", i, j)})
		}
		members = append(members, map[string]any{"machine_id": id, "ranks": ranks})
	}
	completion, _ := json.Marshal(map[string]any{"schema": "aiconnector.group-completion.v1", "run_key": "native-group/1/run-fixture", "members": members})
	out, e := os.Create(file)
	if e != nil {
		return e
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{"stdout.log": []byte("mock SSH only"), "stderr.log": {}, "results/aiconnector-revision.txt": []byte("fixture-rev\n"), "results/metrics.json": []byte(`{"sum":7}`), "results/group-completion.json": completion} {
		if e = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); e != nil {
			return e
		}
		if _, e = tw.Write(body); e != nil {
			return e
		}
	}
	if e = tw.Close(); e != nil {
		return e
	}
	return gz.Close()
}
func TestAIConnectorNativeGroupBridge(t *testing.T) {
	root := os.Getenv("AIC_SOURCE_ROOT")
	if root == "" {
		t.Skip("run with AIC_SOURCE_ROOT for actual Node/Go protocol acceptance")
	}
	node := os.Getenv("AIC_NODE")
	if node == "" {
		node = "node"
	}
	m, monitor, c, store := setupGroup(t)
	defer m.Close()
	ids := []string{}
	for _, h := range c.Machines {
		ids = append(ids, h.ID)
	}
	m.remote = &groupBridgeRemote{fakeTaskRemote: fakeTaskRemote{code: "done:0"}, ids: ids}
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(`<meta name="watch-token" content="__WATCH_TOKEN__">`)}}
	app := NewServer(store, monitor, nil, assets, "127.0.0.1:9999")
	defer app.Close()
	app.tasks = m
	app.SetServiceInfo(ServiceInfo{Schema: ServiceSchema, ServiceID: "fixture-service", RunID: "fixture-instance"})
	server := httptest.NewUnstartedServer(app)
	app.host = server.Listener.Addr().String()
	server.Start()
	defer server.Close()
	raw, _ := json.Marshal(ids)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(root, "tools", "validate_group_bridge.mjs"), server.URL, string(raw))
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bridge: %v\n%s", err, output)
	}
	t.Log(string(output))
	if len(m.Ready("", "")) != 8 {
		t.Fatal("successful native bridge did not release complete group")
	}
}
