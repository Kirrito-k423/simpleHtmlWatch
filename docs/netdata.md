# Netdata 系统指标接入

本功能基于 `v0.5.0-rc.6` 继续开发。Netdata 是按机器启用的独立数据源，用于整机 CPU、内存、磁盘吞吐和网络流量曲线；NPU 原始输出、命令历史与任务管理沿用 simpleHtmlWatch。发布包不包含 Netdata Agent 或 Netdata 仪表盘。

## 启用一台机器

1. 在待监控的 Linux 服务器安装独立 Netdata Agent，确认其支持 `/api/v3/data`。可采用[官方离线安装方式](https://learn.netdata.cloud/docs/netdata-agent/installation/linux/offline-systems)：在联网机器准备静态安装包，再传入内网。安装、升级和系统服务管理由服务器维护者执行。
2. 将 Agent HTTP 服务绑定到服务器本地回环地址。例如在 Agent 的 `netdata.conf` 中设置：

   ```ini
   [web]
       bind to = 127.0.0.1:19999
   ```

   端口可调整。按照 Netdata 安装方式重启准确的 Agent 服务使配置生效；这一步不涉及 simpleHtmlWatch 实例的重启。SSH 服务须允许此账号向 `127.0.0.1:<Netdata 端口>` 建立 TCP 转发。SSH 指纹和认证沿用现有共享凭据，包括 rc.6 的密码/密钥回退。
3. 在 simpleHtmlWatch 的“管理机器”中勾选该机器的“Netdata 系统指标”，填写 Agent 的本地端口，保存。默认端口为 `19999`。若 Agent 已配置 Bearer 保护，填写该 Agent 接受的令牌；不把云端令牌假定为本地 Agent 令牌。
4. 点击“系统指标”，选择机器。首次查询使用最近 15 分钟；可以切换范围、指定开始与结束时间，或用滚轮及工具栏缩放、平移。开启“跟随现在”后，页面可见且弹窗打开时，每 10 秒查询一次。
5. 在“任务中台”选择任务，点击“运行期间系统指标”。查询默认覆盖本机受理前 60 秒至结束观测后 60 秒，结束上限为现在；未结束任务覆盖至现在。超过 7 天时提示并显示最后 7 天，仍可手动指定更早的窗口。

Agent 在服务器持续采集，指标历史保留在 Agent；本机程序退出后 Agent 可继续工作。实际保留时长取决于 Agent 配置，查询到的范围可能只有部分数据。现有 simpleHtmlWatch 文本历史记录与 Netdata 指标历史独立。

## 内网与令牌

- Go 后端通过独立 SSH 连接访问远端 `127.0.0.1`，浏览器只访问 simpleHtmlWatch 的本机 API。不需要开放服务器的 `19999` 网络入站端口。
- 本功能不会安装 Agent、修改远端配置、运行远端 shell 或认领 Netdata Cloud。它只发送固定的 HTTP GET 查询，拒绝跳转，不接受任意 URL、路径或指标表达式。
- Netdata 的匿名遥测默认开启；严格内网部署请按[官方说明](https://learn.netdata.cloud/docs/security-and-privacy-design/netdata-agent)关闭 Agent 与仪表盘遥测，管理自动更新，并使用随包的离线资源。Agent 与云端连接由 Netdata 自身配置控制。
- Bearer 令牌随机器配置加密保存在本机 `config.enc`；公开配置只返回 `hasToken`，编辑留空保持。勾选“清除令牌”删除原值；更换机器 SSH 地址、凭据组或 Netdata 端口时，空白编辑不会带入旧令牌。
- 导出配置排除令牌；导入时停用 Netdata 接入并清除令牌，核对后手动启用。不要把本机完整配置目录作为发布包或测试数据。

## 曲线含义与边界

| 图表 | Netdata context | 展示内容 |
| --- | --- | --- |
| CPU | `system.cpu` | 非 idle 维度，例如 user、system、iowait；每条线是该维度时间占比，不能把单条线当作总利用率 |
| 内存 | `system.ram` | used、cached、buffers、free 等维度，单位以 Agent 返回值为准 |
| 磁盘吞吐 | `system.io` | 系统汇总读写速率，方向值取绝对值；不代表磁盘延迟或 NPU 通信带宽 |
| 网络流量 | `system.net` | Agent 系统汇总收发速率，方向值取绝对值；接口纳入范围由 Agent 决定 |

原生 macOS Agent 没有 `system.net` 汇总采样时，客户端查询 `system.ipv4`，图表明确标注“网络流量（IPv4）”，响应的 `context` 也使用实际来源。这个视图不包含 IPv6 流量；不会因鉴权或 HTTP 失败而切换指标来源。CPU 维度过滤使用 v3 API 的管道分隔表达式 `!idle|*`。

每次查询请求最多 360 个显示点，时间聚合采用平均值；图表同时标注 Agent 原始采样间隔和查询显示间隔。缩放长窗口可能隐藏短时尖峰。缺失值保留 `null`；缺失桶及时间断层会拆开曲线，不补零、不连过缺口。Agent 最新数据库时间与当前查询窗口内的数值分别显示，历史图例是窗口内最后一桶的值，并不表示现在的状态。

悬停联动四张图；任务阴影与竖线使用中台本机的受理/观测时间。应保持本机与服务器校时；若 Agent 响应时钟的估计偏差超过 5 秒，页面提示核对时钟与网络延迟。观测区间不证明进程持续运行，更不能证明 NPU 空闲、显存足够或某个系统波动造成了性能下降。`ready` 与任务调度规则没有因为接入指标而改变。

这次检查未发现 Netdata 内置昇腾 NPU 采集器。首版不从这些系统指标推导 NPU 利用率、HBM 占用或算子性能。

## 本机只读 API

发现接口 `/healthz` 的能力列表包含 `netdata-metrics-v1`。客户端先按[服务实例规则](service-lifecycle.md)核对身份，使用本次会话的 `X-Watch-Token`，并携带已核对的 `X-Watch-Instance: <runId>`。

```text
GET /api/metrics?machineId=<机器ID>
GET /api/metrics?machineId=<机器ID>&after=<Unix秒>&before=<Unix秒>
GET /api/metrics?taskId=<任务ID>
GET /api/metrics?taskId=<任务ID>&after=<Unix秒>&before=<Unix秒>
```

只接受这四个参数，每个只能出现一次。`after` 与 `before` 成对提供，跨度不超过 7 天。任务查询会检查当前机器 SSH 地址、端口与用户名是否仍符合任务记录，配置变化时拒绝关联；这不保证被复用的同一地址仍代表原物理机器。

响应包含 `source`、`nodeId`、`agentVersion`、查询范围、`fetchedAt`、四项 `charts`，以及可选 `task` 标记。曲线包含原始 `updateEvery`、显示 `step`、`lastEntry`、各维度单位和 `{at, value}` 数据点；`value: null` 是缺失。单项失败记录在该 chart 的 `error`，其余曲线仍返回。整个查询失败时返回 HTTP 错误，页面清空旧图，不把旧响应当作本轮成功。

后端通过仍受支持的 `/api/v1/info` 确定 Agent 的 `uid`，再以 `scope_nodes=<uid>` 查询 `/api/v3/data`，避免 Netdata Parent 自动汇总其他节点。每个查询使用独立 SSH 连接，总时限 20 秒；全实例最多同时查询 2 台机器。单项响应最多 1 MiB，最多 32 个维度及 1000 行。此连接与已有 SSH 监控和任务连接独立，接口不提供写操作或任意代理。

## 试用验收

先在一台授权的 A3/A2 服务器验证：实际 Agent 版本及四个 context、SSH TCP 转发策略、令牌访问、断网/断连表现、时钟对应关系和所需历史保留。再对比开启前后的 CPU、内存、磁盘增长和实验指标，决定采样间隔、保留容量及是否关闭不需要的采集器/ML。

已执行的本地验证使用隔离的 SSH/HTTP fixtures 和合成浏览器数据，覆盖实际 SSH direct-tcpip 链路、主机密钥拒绝、取消超时、JSON 格式、空值/零值、单项失败、请求边界、令牌和任务关联。它们证明客户端行为，不代表真实 Netdata Agent 部署、A3/A2 硬件兼容性或性能开销验收。

另外已在原生 ARM64 macOS 与 ARM64 openEuler 22.03 的 A2 主机上运行真实 Netdata v2.12.0，通过中台独立 SSH 查询取得四个图项的实际数值。macOS 的网络来源为 `system.ipv4`，Linux 为 `system.net`。这些结果证明此次部署和读取链路可用；没有执行训练前后对照，不代表 NPU 性能开销验收，也不能外推尚未部署的 A3 主机。机器清单、认证失败与部署记录保存在本机实例数据目录，不随发布包分发。

批量查询和维护者部署步骤已整理为 [simplehtmlwatch-netdata Skill](../skills/simplehtmlwatch-netdata/SKILL.md)，包含只读 `metricsctl.py`、离线独立试用部署脚本和失败边界。先发现实际实例，再查询 `machines` 与 `summary`；停用、未启用 Agent 和单项失败会明确报告。

Netdata Agent 使用 GPLv3+，现代 Netdata UI 使用 NCUL1。simpleHtmlWatch 使用自己的曲线视图，通过公开 API 与独立 Agent 通信。参见[Netdata 仓库许可说明](https://github.com/netdata/netdata#scroll-license)、[官方 API](https://learn.netdata.cloud/docs/netdata-cloud/authentication-%26-authorization/api-tokens)。
