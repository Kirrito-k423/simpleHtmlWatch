package watch

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

type Server struct {
	executor *Executor
	store    *Store
	monitor  *Monitor
	trust    *TrustStore
	assets   fs.FS
	host     string
	token    string
	mu       sync.Mutex
}

func NewServer(s *Store, m *Monitor, t *TrustStore, assets fs.FS, host string) *Server {
	return &Server{executor: NewExecutor(t), store: s, monitor: m, trust: t, assets: assets, host: host, token: randomToken()}
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err string) {
	jsonResponse(w, status, map[string]string{"error": err})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return &extraJSON{}
		}
		return err
	}
	return nil
}

type extraJSON struct{}

func (*extraJSON) Error() string { return "只允许一个 JSON 对象" }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.Host != s.host {
		fail(w, 403, "仅允许本机访问")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+s.host {
		fail(w, 403, "不允许跨站请求")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Watch-Token")), []byte(s.token)) != 1 {
			fail(w, 403, "页面会话已过期，请重新加载")
			return
		}
		s.api(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(405)
		return
	}
	if r.URL.Path == "/" {
		b, err := fs.ReadFile(s.assets, "index.html")
		if err != nil {
			fail(w, 500, "页面不存在")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, strings.ReplaceAll(string(b), "__WATCH_TOKEN__", s.token))
		}
		return
	}
	http.FileServer(http.FS(s.assets)).ServeHTTP(w, r)
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/executions" {
		s.executionAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/history/") {
		s.historyAPI(w, r)
		return
	}
	switch r.URL.Path {
	case "/api/config":
		switch r.Method {
		case "GET":
			jsonResponse(w, 200, map[string]any{"config": s.store.Public(), "commands": Commands})
		case "PUT":
			var c Config
			if err := decode(w, r, &c); err != nil {
				fail(w, 400, "配置格式无效："+err.Error())
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := s.store.Save(c); err != nil {
				fail(w, 400, err.Error())
				return
			}
			s.monitor.Replace(s.store.Snapshot())
			jsonResponse(w, 200, s.store.Public())
		default:
			w.WriteHeader(405)
		}
	case "/api/custom-commands":
		if r.Method != "PUT" && r.Method != "DELETE" {
			w.WriteHeader(405)
			return
		}
		var custom CustomCommand
		if err := decode(w, r, &custom); err != nil {
			fail(w, 400, "指令格式无效："+err.Error())
			return
		}
		custom.Shell = strings.TrimSpace(custom.Shell)
		s.mu.Lock()
		defer s.mu.Unlock()
		c := s.store.Snapshot()
		index := -1
		for i, cmd := range c.CustomCommands {
			if cmd.ID == custom.ID {
				index = i
				break
			}
		}
		if r.Method == "DELETE" {
			if index < 0 {
				fail(w, 404, "自定义指令不存在")
				return
			}
			c.CustomCommands = append(c.CustomCommands[:index], c.CustomCommands[index+1:]...)
		} else if index >= 0 {
			c.CustomCommands[index] = custom
		} else {
			c.CustomCommands = append(c.CustomCommands, custom)
		}
		if err := s.store.Save(c); err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.monitor.Replace(s.store.Snapshot())
		jsonResponse(w, 200, s.store.Public())
	case "/api/status":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		jsonResponse(w, 200, s.monitor.Snapshot())
	case "/api/refresh":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		s.monitor.Refresh()
		jsonResponse(w, 202, map[string]bool{"ok": true})
	case "/api/trust":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var v struct {
			Address     string `json:"address"`
			Fingerprint string `json:"fingerprint"`
		}
		if err := decode(w, r, &v); err != nil {
			fail(w, 400, "指纹参数无效")
			return
		}
		if err := s.trust.Accept(v.Address, v.Fingerprint); err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.monitor.Refresh()
		jsonResponse(w, 200, map[string]bool{"ok": true})
	default:
		w.WriteHeader(404)
	}
}

func (s *Server) Close() { s.executor.Close() }
