package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Only the Netdata port on the SSH server's loopback address is configurable.
// This is a metrics reader, not an arbitrary HTTP proxy or remote installer.
type NetdataConfig struct {
	Enabled    bool   `json:"enabled"`
	Port       int    `json:"port"`
	Token      string `json:"token,omitempty"`
	HasToken   bool   `json:"hasToken,omitempty"`
	ClearToken bool   `json:"clearToken,omitempty"`
}

func (n NetdataConfig) EffectivePort() int {
	if n.Port == 0 {
		return 19999
	}
	return n.Port
}

func (n NetdataConfig) Validate() error {
	if n.EffectivePort() < 1 || n.EffectivePort() > 65535 {
		return errors.New("Netdata 端口需为 1–65535")
	}
	if len(n.Token) > 4096 {
		return errors.New("Netdata 令牌最长 4096 字节")
	}
	for _, c := range n.Token {
		if c <= ' ' || c > '~' {
			return errors.New("Netdata 令牌不能包含空白或非 ASCII 字符")
		}
	}
	return nil
}

const maxMetricResponse = 1024 * 1024
const metricPoints = 360
const maxMetricWindow = int64(7 * 24 * 3600)

var errMetricsBusy = errors.New("系统指标查询繁忙，请稍后刷新")
var errMetricNoData = errors.New("该指标当前没有采样，请检查采集器与 Agent 历史保留范围")

var metricContexts = []struct{ Context, Name string }{
	{"system.cpu", "CPU"},
	{"system.ram", "内存"},
	{"system.io", "磁盘吞吐"},
	{"system.net", "网络流量"},
}

type MetricPoint struct {
	At    int64    `json:"at"`
	Value *float64 `json:"value"` // null is missing, never zero.
}

type MetricSeries struct {
	Name   string        `json:"name"`
	Unit   string        `json:"unit"`
	Points []MetricPoint `json:"points"`
}

type MetricChart struct {
	Context     string         `json:"context"`
	Name        string         `json:"name"`
	Unit        string         `json:"unit"`
	UpdateEvery int64          `json:"updateEvery"`
	Step        int64          `json:"step"`
	LastEntry   int64          `json:"lastEntry"`
	After       int64          `json:"after"`
	Before      int64          `json:"before"`
	Series      []MetricSeries `json:"series"`
	Error       string         `json:"error,omitempty"`
}

type MetricTask struct {
	ID     string      `json:"id"`
	Start  time.Time   `json:"start"`
	End    *time.Time  `json:"end,omitempty"`
	Events []TaskEvent `json:"events"`
}

type MetricsSnapshot struct {
	MachineID     string        `json:"machineId"`
	MachineName   string        `json:"machineName"`
	Source        string        `json:"source"`
	AgentVersion  string        `json:"agentVersion"`
	NodeID        string        `json:"nodeId"`
	AgentTime     *time.Time    `json:"agentTime,omitempty"`
	ClockSkew     int64         `json:"clockSkewSeconds"`
	FetchedAt     time.Time     `json:"fetchedAt"`
	After         int64         `json:"after"`
	Before        int64         `json:"before"`
	WindowClipped bool          `json:"windowClipped"`
	Charts        []MetricChart `json:"charts"`
	Task          *MetricTask   `json:"task,omitempty"`
}

type netdataConnector func(context.Context, Machine, Profile) (*http.Client, func(), error)

type Metrics struct {
	connect netdataConnector
	slots   chan struct{}
}

func NewMetrics(trust *TrustStore) *Metrics {
	return &Metrics{slots: make(chan struct{}, 2), connect: func(ctx context.Context, m Machine, p Profile) (*http.Client, func(), error) {
		addr := net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
		client, conn, err := dial(ctx, addr, p, trust.Callback(addr, false))
		if err != nil {
			return nil, nil, fmt.Errorf("系统指标 SSH 连接失败（先在监控页确认主机指纹）：%w", err)
		}
		// Closing the underlying connection also interrupts a refused/stalled
		// direct-tcpip channel. No listener is opened on the user's computer.
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		transport := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return client.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Netdata.EffectivePort())))
			},
			MaxResponseHeaderBytes: 16 * 1024,
		}
		cleanup := func() { transport.CloseIdleConnections(); stop(); client.Close(); conn.Close() }
		return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, cleanup, nil
	}}
}

// All callers supply a bounded context. Each request opens a dedicated SSH
// connection, separate from monitoring, one-shot execution and task control.
func (m *Metrics) Query(ctx context.Context, machine Machine, profile Profile, after, before int64) (MetricsSnapshot, error) {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	default:
		return MetricsSnapshot{}, errMetricsBusy
	}
	client, closeConnection, err := m.connect(ctx, machine, profile)
	if err != nil {
		return MetricsSnapshot{}, err
	}
	defer closeConnection()
	var info struct {
		UID     string `json:"uid"`
		Version string `json:"version"`
		OSName  string `json:"os_name"`
	}
	// v1/info is supported discovery metadata. Metric queries use v3, and
	// scope_nodes pins the source so a Parent cannot mix in its Child nodes.
	agentTime, err := netdataJSON(ctx, client, machine.Netdata.Token, "/api/v1/info", nil, &info)
	if err != nil {
		return MetricsSnapshot{}, err
	}
	if !identifier.MatchString(info.UID) {
		return MetricsSnapshot{}, errors.New("Netdata 未返回有效节点 ID，无法确认指标来源")
	}
	out := MetricsSnapshot{MachineID: machine.ID, MachineName: machine.Name, Source: "netdata", AgentVersion: info.Version, NodeID: info.UID, AgentTime: agentTime, After: after, Before: before, Charts: make([]MetricChart, len(metricContexts))}
	if agentTime != nil {
		// Measure when info was received, not after all metric queries finish.
		out.ClockSkew = time.Now().Unix() - agentTime.Unix()
	}
	var wg sync.WaitGroup
	for i, item := range metricContexts {
		wg.Go(func() {
			chart := MetricChart{Context: item.Context, Name: item.Name, Series: []MetricSeries{}}
			q := url.Values{"scope_nodes": {info.UID}, "contexts": {item.Context}, "after": {strconv.FormatInt(after, 10)}, "before": {strconv.FormatInt(before, 10)}, "points": {strconv.Itoa(metricPoints)}, "format": {"json"}, "options": {"jsonwrap,seconds,absolute"}, "group_by": {"dimension"}, "aggregation": {"sum"}, "time_group": {"average"}, "timeout": {"10000"}}
			if item.Context == "system.cpu" {
				// API dimension patterns are pipe-separated, unlike the CLI's
				// whitespace-separated simple patterns.
				q.Set("dimensions", "!idle|*")
			}
			var data netdataData
			if _, err := netdataJSON(ctx, client, machine.Netdata.Token, "/api/v3/data", q, &data); err != nil {
				chart.Error = err.Error()
			} else {
				// The native macOS collector exposes IPv4 bandwidth but may not
				// publish Linux's system.net aggregate. Keep the narrower scope
				// explicit, and never fall back after authentication/HTTP errors.
				if item.Context == "system.net" && strings.EqualFold(info.OSName, "macOS") && data.noData() {
					chart.Context, chart.Name = "system.ipv4", "网络流量（IPv4）"
					q.Set("contexts", chart.Context)
					if _, err := netdataJSON(ctx, client, machine.Netdata.Token, "/api/v3/data", q, &data); err != nil {
						chart.Error = err.Error()
					}
				}
				if chart.Error == "" {
					if err := data.normalize(&chart); err != nil {
						chart.Error = err.Error()
					}
				}
			}
			out.Charts[i] = chart
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return MetricsSnapshot{}, errors.New("系统指标查询超时或已取消，请检查 SSH 转发及 Agent 状态")
	}
	out.FetchedAt = time.Now()
	return out, nil
}

func netdataJSON(ctx context.Context, client *http.Client, token, path string, query url.Values, target any) (*time.Time, error) {
	u := "http://netdata.local" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("无法读取 Netdata：请检查 Agent 是否监听远端 127.0.0.1，以及 SSH 是否允许 TCP 转发")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case 401, 403:
			return nil, errors.New("Netdata 拒绝访问，请检查 Agent 的 Bearer 令牌与访问配置")
		case 404:
			return nil, errors.New("Netdata 未提供该指标或 v3 API，请检查采集器和 Agent 版本")
		default:
			return nil, fmt.Errorf("Netdata 返回 HTTP %d", response.StatusCode)
		}
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, maxMetricResponse+1))
	if err != nil {
		return nil, errors.New("Netdata 响应未完整读取")
	}
	if len(b) > maxMetricResponse {
		return nil, errors.New("Netdata 单项响应超过 1 MiB 上限")
	}
	if err := json.Unmarshal(b, target); err != nil {
		return nil, errors.New("Netdata 响应不是受支持的 JSON 数据")
	}
	if date, err := http.ParseTime(response.Header.Get("Date")); err == nil {
		return &date, nil
	}
	return nil, nil
}

type netdataData struct {
	DB struct {
		UpdateEvery int64 `json:"update_every"`
		LastEntry   int64 `json:"last_entry"`
	} `json:"db"`
	View struct {
		Units       string `json:"units"`
		UpdateEvery int64  `json:"update_every"`
		After       int64  `json:"after"`
		Before      int64  `json:"before"`
		Dimensions  struct {
			Units []string `json:"units"`
		} `json:"dimensions"`
	} `json:"view"`
	Result struct {
		Labels []string         `json:"labels"`
		Data   [][]*json.Number `json:"data"`
	} `json:"result"`
}

func (d netdataData) noData() bool {
	return d.DB.UpdateEvery == 0 && len(d.Result.Data) == 0 && len(d.Result.Labels) == 2 && d.Result.Labels[0] == "time" && d.Result.Labels[1] == "no data"
}

func (d netdataData) normalize(chart *MetricChart) error {
	if d.noData() {
		return errMetricNoData
	}
	if len(d.Result.Labels) < 2 || d.Result.Labels[0] != "time" || len(d.Result.Labels) > 33 || len(d.Result.Data) > 1000 || d.View.Units == "" || d.DB.UpdateEvery < 1 || d.View.UpdateEvery < 1 {
		return errors.New("Netdata 指标结构不兼容或规模超出上限")
	}
	chart.Unit, chart.UpdateEvery, chart.Step, chart.LastEntry = d.View.Units, d.DB.UpdateEvery, d.View.UpdateEvery, d.DB.LastEntry
	chart.After, chart.Before = d.View.After, d.View.Before
	series := make([]MetricSeries, len(d.Result.Labels)-1)
	for i, name := range d.Result.Labels[1:] {
		if name == "" || len(name) > 256 {
			return errors.New("Netdata 指标维度名称无效")
		}
		unit := chart.Unit
		if i < len(d.View.Dimensions.Units) && d.View.Dimensions.Units[i] != "" {
			unit = d.View.Dimensions.Units[i]
		}
		series[i] = MetricSeries{Name: name, Unit: unit, Points: make([]MetricPoint, 0, len(d.Result.Data))}
	}
	seen := map[int64]bool{}
	for _, row := range d.Result.Data {
		if len(row) != len(d.Result.Labels) || row[0] == nil {
			return errors.New("Netdata 指标行缺失时间戳或维度")
		}
		at, err := row[0].Int64()
		if err != nil || at <= 0 || seen[at] {
			return errors.New("Netdata 指标时间戳无效或重复")
		}
		seen[at] = true
		for i := range series {
			point := MetricPoint{At: at}
			if row[i+1] != nil {
				value, err := row[i+1].Float64()
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
					return errors.New("Netdata 指标数值无效")
				}
				point.Value = &value
			}
			series[i].Points = append(series[i].Points, point)
		}
	}
	for i := range series {
		sort.Slice(series[i].Points, func(a, b int) bool { return series[i].Points[a].At < series[i].Points[b].At })
	}
	chart.Series = series
	return nil
}

func sameMetricsTarget(a, b Machine, pa, pb Profile) bool {
	return a.Enabled == b.Enabled && a.Host == b.Host && a.Port == b.Port && a.ProfileID == b.ProfileID && a.Netdata != nil && b.Netdata != nil && *a.Netdata == *b.Netdata && pa == pb
}

func metricMachine(c Config, id string) (Machine, Profile, bool) {
	for _, m := range c.Machines {
		if m.ID == id {
			for _, p := range c.Profiles {
				if p.ID == m.ProfileID {
					return m, p, true
				}
			}
		}
	}
	return Machine{}, Profile{}, false
}

// sanitizeMetricError guards even errors returned by a custom transport. Agent
// response bodies are never forwarded, and configured tokens stay in the vault.
func sanitizeMetricError(err error, n *NetdataConfig) string {
	s := err.Error()
	if n != nil && n.Token != "" {
		s = strings.ReplaceAll(s, n.Token, "[令牌已隐藏]")
	}
	return s
}
