package watch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type brokenArchiveSink struct{}

func (brokenArchiveSink) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestArchiveWriterTenGiBBoundaryAndAbort(t *testing.T) {
	if maxTaskArchive != 10737418240 {
		t.Fatal("archive cap must be 10 GiB")
	}
	var output bytes.Buffer
	progress, aborted := 0, false
	writer := &archiveWriter{file: &output, size: maxTaskArchive - 4,
		progress: func() error { progress++; return nil }, abort: func() { aborted = true }}
	if n, err := writer.Write([]byte("last")); n != 4 || err != nil || writer.size != maxTaskArchive || progress != 1 {
		t.Fatalf("exact boundary rejected: %d %v %+v", n, err, writer)
	}
	if n, err := writer.Write([]byte("!")); n != 0 || err == nil || !aborted || output.String() != "last" {
		t.Fatalf("overflow was written or did not abort: %d %v", n, err)
	}
	aborted = false
	writer = &archiveWriter{file: brokenArchiveSink{}, abort: func() { aborted = true }}
	if _, err := writer.Write([]byte("data")); err == nil || !aborted {
		t.Fatal("disk failure must abort SSH immediately")
	}
}

type slowArchiveRemote struct{ fakeTaskRemote }

func (r *slowArchiveRemote) Collect(ctx context.Context, job TaskJob, profile Profile, path string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(150 * time.Millisecond):
		return r.fakeTaskRemote.Collect(ctx, job, profile, path)
	}
}

func TestTaskArchiveHTTPStreamingAndTimeout(t *testing.T) {
	remote := &slowArchiveRemote{fakeTaskRemote: fakeTaskRemote{code: "done:0"}}
	m, monitor, c, store := setupTasks(t, remote)
	app := NewServer(store, monitor, nil, nil, "")
	app.tasks = m
	defer app.Close()
	job, _, err := m.Submit(TaskRequest{ID: "large-http", Shell: "printf ok", MachineID: c.Machines[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	waitTask(t, m, job.ID)
	path := m.path(job.ID, "result.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("large-result"), 8192)
	want := sha256.New()
	var size int64
	for i := 0; i < 64; i++ {
		n, err := io.MultiWriter(f, want).Write(chunk)
		if err != nil {
			t.Fatal(err)
		}
		size += int64(n)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exceed the normal API deadline before handling this download.
		time.Sleep(100 * time.Millisecond)
		app.ServeHTTP(w, r)
	}))
	app.host = server.Listener.Addr().String()
	server.Config.WriteTimeout = 30 * time.Millisecond
	server.Start()
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	for _, method := range []string{"GET", "POST"} {
		body := url.Values{"token": {app.token}}.Encode()
		req, _ := http.NewRequest(method, server.URL+"/api/tasks/archive?id="+job.ID, strings.NewReader(body))
		if method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req.Header.Set("X-Watch-Token", app.token)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.New()
		n, err := io.CopyBuffer(got, response.Body, make([]byte, 65536))
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || response.ContentLength != size || n != size || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
			t.Fatalf("%s large download incomplete: status=%d size=%d/%d err=%v", method, response.StatusCode, n, size, err)
		}
		if !strings.Contains(response.Header.Get("Content-Disposition"), "attachment") {
			t.Fatal("browser must download the response")
		}
	}
	req, _ := http.NewRequest("GET", server.URL+"/api/tasks/archive?id="+job.ID, nil)
	req.Header.Set("X-Watch-Token", app.token)
	req.Header.Set("Range", "bytes=10-19")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rangeData, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 206 || !bytes.Equal(rangeData, chunk[10:20]) {
		t.Fatalf("range download failed: %d %v", response.StatusCode, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest("POST", server.URL+"/api/tasks/collect?id="+job.ID, nil)
	req.Header.Set("X-Watch-Token", app.token)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("slow manual collection failed: %d", response.StatusCode)
	}
}

func TestTaskArchiveFormAuthentication(t *testing.T) {
	m, monitor, _, store := setupTasks(t, &fakeTaskRemote{})
	app := NewServer(store, monitor, nil, nil, "127.0.0.1:9999")
	app.tasks = m
	defer app.Close()
	for _, tc := range []struct {
		name, path, token, origin string
		status                    int
	}{
		{"same origin browser form", "/api/tasks/archive?id=absent", app.token, "http://127.0.0.1:9999", 404},
		{"opaque origin remains blocked", "/api/tasks/archive?id=absent", app.token, "null", 403},
		{"valid form", "/api/tasks/archive?id=absent", app.token, "", 404},
		{"missing token", "/api/tasks/archive?id=absent", "", "", 403},
		{"bad token", "/api/tasks/archive?id=absent", "bad", "", 403},
		{"query token rejected", "/api/tasks/archive?id=absent&token=" + app.token, "", "", 403},
		{"cross origin", "/api/tasks/archive?id=absent", app.token, "http://evil.invalid", 403},
		{"other routes reject form", "/api/tasks", app.token, "", 403},
		{"oversized form", "/api/tasks/archive?id=absent", strings.Repeat("x", 4096), "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+app.host+tc.path, strings.NewReader(url.Values{"token": {tc.token}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
		})
	}
}
