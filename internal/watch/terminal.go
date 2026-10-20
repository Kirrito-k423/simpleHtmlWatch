package watch

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

const terminalIdleTimeout = 15 * time.Minute
const terminalChunk = 16 * 1024

type terminalManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	slots  chan struct{}
}

func newTerminalManager() *terminalManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &terminalManager{ctx: ctx, cancel: cancel, slots: make(chan struct{}, 4)}
}
func (m *terminalManager) start() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	select {
	case m.slots <- struct{}{}:
		m.wg.Add(1)
		return true
	default:
		return false
	}
}
func (m *terminalManager) finish() { <-m.slots; m.wg.Done() }
func (m *terminalManager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

type terminalMessage struct {
	Type      string `json:"type"`
	Token     string `json:"token,omitempty"`
	RunID     string `json:"runId,omitempty"`
	MachineID string `json:"machineId,omitempty"`
	Rows      int    `json:"rows,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Data      string `json:"data,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
}

func terminalSize(rows, cols int) bool { return rows >= 2 && rows <= 120 && cols >= 20 && cols <= 320 }
func terminalStatus(ctx context.Context, c *websocket.Conn, state, message string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, _ := json.Marshal(map[string]string{"type": "status", "state": state, "message": message})
	_ = c.Write(ctx, websocket.MessageText, b)
}

// One acknowledged chunk at a time bounds both server and browser queues, even
// when a command prints indefinitely or the browser stops consuming output.
type terminalOutput struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	c      *websocket.Conn
	ack    chan int
}

func (o *terminalOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	total := 0
	for len(p) > 0 {
		n := min(len(p), terminalChunk)
		ctx, cancel := context.WithTimeout(o.ctx, 20*time.Second)
		err := o.c.Write(ctx, websocket.MessageBinary, p[:n])
		if err == nil {
			select {
			case size := <-o.ack:
				if size != n {
					err = errors.New("terminal output acknowledgment mismatch")
				}
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		cancel()
		if err != nil {
			o.cancel()
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (s *Server) terminalAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Origin") != "http://"+s.host {
		fail(w, 403, "请从本机页面打开 SSH 终端")
		return
	}
	if !s.terminals.start() {
		fail(w, 429, "SSH 终端最多同时打开 4 个，或服务正在退出")
		return
	}
	defer s.terminals.finish()
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(32 * 1024)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.terminals.ctx, cancel)
	defer stop()
	stopWS := context.AfterFunc(ctx, func() { _ = c.CloseNow() })
	defer stopWS()
	authCtx, authCancel := context.WithTimeout(ctx, 5*time.Second)
	kind, raw, err := c.Read(authCtx)
	authCancel()
	var auth terminalMessage
	if err != nil || kind != websocket.MessageText || json.Unmarshal(raw, &auth) != nil || auth.Type != "auth" || subtle.ConstantTimeCompare([]byte(auth.Token), []byte(s.token)) != 1 {
		_ = c.Close(websocket.StatusPolicyViolation, "页面会话已过期，请刷新页面")
		return
	}
	if s.service != nil && auth.RunID != s.service.RunID {
		_ = c.Close(websocket.StatusPolicyViolation, "实例已变化，请刷新页面")
		return
	}
	if !identifier.MatchString(auth.MachineID) || !terminalSize(auth.Rows, auth.Cols) {
		_ = c.Close(websocket.StatusPolicyViolation, "机器或终端大小无效")
		return
	}
	var machine Machine
	var profile Profile
	config := s.store.Snapshot()
	for _, m := range config.Machines {
		if m.ID == auth.MachineID && m.Enabled {
			machine = m
			break
		}
	}
	for _, p := range config.Profiles {
		if p.ID == machine.ProfileID {
			profile = p
			break
		}
	}
	if machine.ID == "" || profile.ID == "" {
		terminalStatus(ctx, c, "error", "机器已停用或凭据配置缺失")
		return
	}
	terminalStatus(ctx, c, "connecting", "正在连接 "+machine.Name)
	address := net.JoinHostPort(machine.Host, strconv.Itoa(machine.Port))
	client, conn, err := dial(ctx, address, profile, s.trust.Callback(address, false))
	if err != nil {
		terminalStatus(ctx, c, "error", "SSH 连接失败："+taskText(err.Error()))
		return
	}
	defer client.Close()
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})
	stopSSH := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopSSH()
	session, err := client.NewSession()
	if err != nil {
		terminalStatus(ctx, c, "error", "无法创建 SSH 会话")
		return
	}
	defer session.Close()
	if err = session.RequestPty("xterm-256color", auth.Rows, auth.Cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}); err != nil {
		terminalStatus(ctx, c, "error", "服务器不支持交互终端："+taskText(err.Error()))
		return
	}
	input, err := session.StdinPipe()
	if err != nil {
		terminalStatus(ctx, c, "error", "无法打开终端输入")
		return
	}
	output := &terminalOutput{ctx: ctx, cancel: cancel, c: c, ack: make(chan int, 1)}
	session.Stdout, session.Stderr = output, output
	if err = session.Shell(); err != nil {
		terminalStatus(ctx, c, "error", "无法启动远端 shell："+taskText(err.Error()))
		return
	}
	terminalStatus(ctx, c, "connected", "已连接 "+machine.Name)
	idle := time.AfterFunc(terminalIdleTimeout, cancel)
	defer idle.Stop()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		err := session.Wait()
		if ctx.Err() == nil {
			message := "远端 shell 已退出"
			if err != nil {
				message = "远端 shell 已断开：" + taskText(err.Error())
			}
			terminalStatus(ctx, c, "closed", message)
		}
		cancel()
	}()
	defer func() { cancel(); _ = session.Close(); <-waited }()
	for {
		kind, raw, err = c.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			return
		}
		var msg terminalMessage
		if json.Unmarshal(raw, &msg) != nil {
			return
		}
		switch msg.Type {
		case "ack":
			if msg.Bytes <= 0 || msg.Bytes > terminalChunk {
				return
			}
			select {
			case output.ack <- msg.Bytes:
			default:
				return
			}
		case "input":
			if len(msg.Data) == 0 || len(msg.Data) > terminalChunk {
				return
			}
			idle.Reset(terminalIdleTimeout)
			if _, err = io.WriteString(input, msg.Data); err != nil {
				return
			}
		case "resize":
			if !terminalSize(msg.Rows, msg.Cols) {
				return
			}
			if err = session.WindowChange(msg.Rows, msg.Cols); err != nil {
				return
			}
		default:
			return
		}
	}
}
