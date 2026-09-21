package watch

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNormalizeWatch(t *testing.T) {
	for input, want := range map[string]string{"watch npu-smi info": "npu-smi info", "watch -n 4 'ps -ef | grep tilexr'": "ps -ef | grep tilexr", "/usr/bin/watch --interval=2 -d -- ps -ef": "ps -ef", "printf '%s' \"hello\"": "printf '%s' \"hello\""} {
		got, e := NormalizeWatch(input)
		if e != nil || got != want {
			t.Fatalf("%q => %q %v", input, got, e)
		}
	}
	for _, s := range []string{"watch", "watch --exec ps -ef", "watch 'unterminated", "watch 'a' && 'b'", "watch watch ps -ef"} {
		if _, e := NormalizeWatch(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func historySample(at time.Time, output string) HistoryRecord {
	code := 0
	return HistoryRecord{MachineID: "m1", MachineName: "test", StartedAt: at, State: State{Results: []Result{{CommandID: "custom-one", Name: "任务", Shell: "ps -ef", Original: "watch ps -ef", Stdout: output, ExitCode: &code}}}}
}
func TestHistoryPersistenceRulesAndStopBoundary(t *testing.T) {
	dir := t.TempDir()
	h, e := NewHistory(dir)
	if e != nil {
		t.Fatal(e)
	}
	h.SetEnabled(true)
	epoch, _ := h.Epoch()
	at := time.Now().UTC().Truncate(time.Second)
	h.Append(epoch, historySample(at, "old"))
	h.Append(epoch, historySample(at.Add(4*time.Second), "tilexr"))
	h.Append(epoch, historySample(at.Add(8*time.Second), "tilexr"))
	h.SetEnabled(false)
	h.Append(epoch, historySample(at.Add(12*time.Second), "not saved"))
	if len(h.refs) != 3 {
		t.Fatal("stop accepted stale record")
	}
	id := h.refs[1].id
	rows, e := h.Query(at, time.Minute, []string{"m1"}, "custom-one", "change", "")
	if e != nil || rows[0].Cells[0].Unknown != 1 || rows[0].Cells[1].Hits != 1 || rows[0].Cells[2].Hits != 0 {
		t.Fatalf("change %+v %v", rows, e)
	}
	rows, e = h.Query(at, time.Hour, []string{"m1"}, "custom-one", "contains", "tilexr")
	if e != nil || rows[0].Cells[0].Hits != 2 || rows[0].Cells[0].ID != id {
		t.Fatal("aggregation lost match")
	}
	rows, _ = h.Query(at, time.Hour, []string{"m1"}, "not-recorded", "none", "")
	if rows[0].Cells[0].Unknown != 3 {
		t.Fatal("missing command was treated as empty success")
	}
	path := h.file.Name()
	h.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("{torn tail")
	f.Close()
	reopened, e := NewHistory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	r, e := reopened.Get(id)
	if e != nil || r.State.Results[0].Stdout != "tilexr" {
		t.Fatalf("lost history %v", e)
	}
	if _, enabled := reopened.Epoch(); enabled {
		t.Fatal("recording silently resumed")
	}
	if len(reopened.refs) != 3 {
		t.Fatal("tail recovery lost complete records")
	}
}
func TestHistoryQuotaAndWriteFailure(t *testing.T) {
	h, e := NewHistory(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	h.SetEnabled(true)
	epoch, _ := h.Epoch()
	h.Append(epoch, historySample(time.Now(), "one"))
	first := h.refs[0]
	h.limit = h.total + 10
	h.size = historySegment
	h.Append(epoch, historySample(time.Now(), "two"))
	if len(h.refs) != 1 || h.refs[0].id == first.id {
		t.Fatal("old segment not pruned")
	}
	if _, e = os.Stat(first.file); !os.IsNotExist(e) {
		t.Fatal("old segment remains")
	}
	broken, e := NewHistory(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(broken.dir)
	os.WriteFile(broken.dir, []byte("block"), 0600)
	broken.SetEnabled(true)
	epoch, _ = broken.Epoch()
	broken.Append(epoch, historySample(time.Now(), "x"))
	if broken.Status()["enabled"] != false || broken.Status()["error"] == "" {
		t.Fatal("write failure not surfaced")
	}
}
func TestNPUFormatUnknownAndCounts(t *testing.T) {
	for _, tc := range []struct {
		text  string
		count int
		known bool
	}{{"NPU 0 OK", 0, false}, {"| NPU Chip Process id Process name Memory |\n| 0 0 42 python 100 |\n| 0 0 99 python 100 |", 2, true}, {"| NPU Chip Process id Process name Memory |\n| 0 0 42 python 100 |\n| 1 0 99 python 100 |", 1, true}, {"| Process id |\nmalformed", 0, false}} {
		count, known := npuCount(tc.text)
		if count != tc.count || known != tc.known {
			t.Fatalf("parse %q: %d %v", tc.text, count, known)
		}
	}
}
func TestHistorySSHIntegrationAndAPIBoundary(t *testing.T) {
	f := newFixture(t)
	app, _, _ := customApp(t, f)
	h, e := NewHistory(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	app.monitor.SetHistory(h)
	h.SetEnabled(true)
	putCustom(t, app, CustomCommand{ID: "custom-one", Name: "tilexr", Shell: "watch -n 4 'ps -ef | grep tilexr'", Enabled: true}, app.token, 200)
	waitState(t, app.monitor, func(_ map[string]State) bool { return h.Status()["records"].(int) > 0 })
	h.mu.Lock()
	id := h.refs[0].id
	h.mu.Unlock()
	record, e := h.Get(id)
	if e != nil {
		t.Fatal(e)
	}
	found := map[string]Result{}
	for _, r := range record.State.Results {
		found[r.CommandID] = r
	}
	if found["custom-one"].Shell != "ps -ef | grep tilexr" || strings.Contains(found["custom-one"].Stdout, "watch -n") || !strings.Contains(found["history-cwd"].Stdout, "demo-boot") || found["history-ps"].Stdout == "" {
		t.Fatalf("missing evidence: %+v", found)
	}
	for _, token := range []string{"wrong", app.token} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:9999/api/history/sample?id="+id, nil)
		r.Header.Set("X-Watch-Token", token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		want := 200
		if token == "wrong" {
			want = 403
		}
		if w.Code != want {
			t.Fatal(w.Code)
		}
	}
	request := httptest.NewRequest("POST", "http://127.0.0.1:9999/api/history/status", bytes.NewBufferString(`{"enabled":false}`))
	request.Header.Set("X-Watch-Token", app.token)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, request)
	if w.Code != 200 || h.Status()["enabled"] != false {
		t.Fatal("stop API failed")
	}
}
func TestHistoryOneHourSixteenMachines(t *testing.T) {
	// Build a durable fixture directly; avoid 14,400 fsync calls in a unit test.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "history"), 0700)
	f, e := os.Create(filepath.Join(dir, "history", "samples-00000000000000000001.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	enc := json.NewEncoder(f)
	at := time.Now().UTC().Truncate(time.Hour)
	ids := []string{}
	for m := 0; m < 16; m++ {
		ids = append(ids, fmt.Sprintf("m%d", m))
	}
	for n := 0; n < 900; n++ {
		for _, id := range ids {
			r := historySample(at.Add(time.Duration(n)*4*time.Second), "stable")
			r.ID = fmt.Sprintf("%s-%d", id, n)
			r.MachineID = id
			if n == 608 {
				r.State.Results[0].Stdout = "tilexr"
			}
			if e = enc.Encode(r); e != nil {
				t.Fatal(e)
			}
		}
	}
	f.Close()
	h, e := NewHistory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if len(h.refs) != 14400 {
		t.Fatal(len(h.refs))
	}
	rows, e := h.Query(at, time.Hour, ids, "custom-one", "contains", "tilexr")
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range rows {
		if row.Cells[40].Hits != 1 || row.Cells[40].ID != row.ID+"-608" {
			t.Fatal("minute aggregation lost single-frame event")
		}
	}
}

func TestCwdCollectorOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc")
	}
	data, err := exec.Command("bash", "-lc", cwdCommand).CombinedOutput()
	if err != nil {
		t.Fatalf("collector: %v %s", err, data)
	}
	prefix := strconv.Itoa(os.Getpid()) + "\t"
	cwd, _ := os.Getwd()
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			fields := strings.Split(line, "\t")
			decoded, e := base64.StdEncoding.DecodeString(fields[2])
			if e != nil || string(decoded) != cwd || fields[1] == "" {
				t.Fatalf("cwd identity: %q %v", line, e)
			}
			return
		}
	}
	t.Fatal("collector did not find its own test process")
}
