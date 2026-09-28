package watch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

const ServiceSchema = "simplehtmlwatch.service.v1"
const ServiceAPIVersion = 1

var ErrDirectoryInUse = errors.New("配置目录已被运行实例占用")
var instanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ServiceInfo is discovery metadata, not proof of liveness. The file survives
// shutdown so the next owner reuses its identity, port and selected build.
// Neither credentials nor the per-process session token belong here.
type ServiceInfo struct {
	Schema       string   `json:"schema"`
	ServiceID    string   `json:"serviceId"`
	Instance     string   `json:"instance"`
	RunID        string   `json:"runId"`
	Version      string   `json:"version"`
	BuildID      string   `json:"buildId"`
	APIVersion   int      `json:"apiVersion"`
	Capabilities []string `json:"capabilities"`
	URL          string   `json:"url"`
	DataDir      string   `json:"dataDir"`
	Executable   string   `json:"executable"`
	PID          int      `json:"pid"`
	StartedAt    string   `json:"startedAt"`
}

// ResolveServiceDir makes named instances independent of release locations.
// Explicit directories must still carry the same instance name on every use.
func ResolveServiceDir(dir, instance string) (string, error) {
	if !instanceName.MatchString(instance) {
		return "", errors.New("实例名只能含字母、数字、下划线和连字符，长度 1–64，首字符必须为字母或数字")
	}
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "simpleHtmlWatch")
		if instance != "default" {
			dir = filepath.Join(dir, "instances", instance)
		}
	}
	return filepath.Abs(dir)
}

func ReadServiceInfo(dir string) (ServiceInfo, error) {
	var info ServiceInfo
	data, err := os.ReadFile(filepath.Join(dir, "service.json"))
	if err != nil {
		return info, err
	}
	if err = json.Unmarshal(data, &info); err != nil {
		return info, fmt.Errorf("service.json 无效（不会覆盖）：%w", err)
	}
	if info.Schema != ServiceSchema || info.ServiceID == "" || info.RunID == "" || info.BuildID == "" || !instanceName.MatchString(info.Instance) {
		return info, errors.New("service.json 协议或身份无效（不会覆盖）")
	}
	if _, err := servicePort(info.URL); err != nil {
		return info, err
	}
	return info, nil
}

func servicePort(raw string) (int, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return 0, errors.New("服务登记必须为 127.0.0.1 的 HTTP 端口地址")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("服务登记端口无效")
	}
	return port, nil
}

// ProbeService verifies the current process, never just a TCP port or PID. It
// deliberately bypasses proxies and refuses redirects to another service.
func ProbeService(expected ServiceInfo) (ServiceInfo, error) {
	var live ServiceInfo
	if _, err := servicePort(expected.URL); err != nil {
		return live, err
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(expected.URL + "/healthz")
	if err != nil {
		return live, fmt.Errorf("实例不可达，禁止据此结束进程或自动重启：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return live, fmt.Errorf("实例身份接口返回 HTTP %d；可能是旧版或其他服务，禁止接管", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32*1024)).Decode(&live); err != nil {
		return live, fmt.Errorf("实例身份响应无效：%w", err)
	}
	if live.Schema != ServiceSchema || live.APIVersion != ServiceAPIVersion || live.ServiceID != expected.ServiceID || live.RunID != expected.RunID || live.Instance != expected.Instance || live.URL != expected.URL || live.BuildID != expected.BuildID || live.DataDir != expected.DataDir || live.Version != expected.Version {
		return live, errors.New("端口上的服务与登记身份或协议不符，禁止接管或重启")
	}
	return live, nil
}

func DiscoverService(dir, instance string) (ServiceInfo, error) {
	info, err := ReadServiceInfo(dir)
	if err != nil {
		return info, fmt.Errorf("无法发现实例；旧版未登记时请核对启动窗口，禁止按进程名重启：%w", err)
	}
	if info.Instance != instance {
		return info, fmt.Errorf("目录属于实例 %q，不是 %q", info.Instance, instance)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return info, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return info, err
	}
	if info.DataDir != canonical {
		return info, errors.New("登记文件属于其他数据目录；拒绝连接")
	}
	return ProbeService(info)
}

type ServiceLease struct {
	Info     ServiceInfo
	Listener net.Listener
	unlock   func()
}

func (s *ServiceLease) Close() {
	_ = s.Listener.Close()
	s.unlock()
}

// AcquireService must run before reading credentials, recovering tasks, or
// starting monitors. The OS lock is also understood by historical releases.
// A healthy existing owner is returned for reuse; it is never replaced.
func AcquireService(dir, instance, version string, port int, explicitPort, adoptBuild bool) (*ServiceLease, *ServiceInfo, error) {
	if port < 0 || port > 65535 {
		return nil, nil, errors.New("端口必须在 0–65535 之间")
	}
	unlock, err := LockDirectory(dir)
	if err != nil {
		if !errors.Is(err, ErrDirectoryInUse) {
			return nil, nil, err
		}
		// A concurrent first launch may still be initializing/publishing.
		var probeErr error
		for attempt := 0; attempt < 10; attempt++ {
			info, e := DiscoverService(dir, instance)
			if e == nil {
				return nil, &info, nil
			}
			probeErr = e
			if attempt < 9 {
				time.Sleep(100 * time.Millisecond)
			}
		}
		return nil, nil, fmt.Errorf("%w；持有者可能是旧版或正在启动；不会删除锁或结束进程：%v", err, probeErr)
	}
	success := false
	defer func() {
		if !success {
			unlock()
		}
	}()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(executable)
	if err != nil {
		return nil, nil, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, f)
	_ = f.Close()
	if err != nil {
		return nil, nil, err
	}
	buildID := hex.EncodeToString(hash.Sum(nil))
	previous, err := ReadServiceInfo(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	serviceID := previous.ServiceID
	if err == nil {
		if previous.Instance != instance || previous.DataDir != canonical {
			return nil, nil, errors.New("实例名或数据目录与 service.json 不符；请使用原实例目录，独立测试请新建实例")
		}
		if (previous.BuildID != buildID || previous.Version != version) && !adoptBuild {
			return nil, nil, fmt.Errorf("实例 %s 已固定构建 %s（%s）；当前为 %s（%s）。不会自动替换或降级；测试请使用独立 -instance，计划切换请在服务停止后显式加 -adopt-build", instance, previous.Version, previous.BuildID[:min(12, len(previous.BuildID))], version, buildID[:12])
		}
		if !explicitPort {
			port, _ = servicePort(previous.URL)
		}
	} else {
		serviceID = randomToken()
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, nil, fmt.Errorf("实例端口 %d 无法绑定；不会换端口或停止占用者。计划迁移请显式指定 -port：%w", port, err)
	}
	info := ServiceInfo{
		Schema: ServiceSchema, ServiceID: serviceID, Instance: instance, RunID: randomToken(),
		Version: version, BuildID: buildID, APIVersion: ServiceAPIVersion,
		Capabilities: []string{"tasks", "reservations", "instance-fencing"},
		URL:          "http://" + listener.Addr().String(), DataDir: canonical, Executable: executable,
		PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	success = true
	return &ServiceLease{Info: info, Listener: listener, unlock: unlock}, nil, nil
}

// Publish only after the application has initialized successfully. Keep the last
// record on shutdown/crash; only a successful health probe proves it is live.
func (s *ServiceLease) Publish() error {
	data, err := json.MarshalIndent(s.Info, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.Info.DataDir, "service.json"), append(data, '\n'))
}
