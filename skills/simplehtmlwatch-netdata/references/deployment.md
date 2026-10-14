# 维护者部署与验收

## 确定目标和已有能力

1. 读取用户明确提供的机器配置。程序内解析凭据，日志只输出名称、主机、端口、账号和状态；不要把包含密码的字段当作普通元数据打印。实际 NPU 型号以 `npu-smi info` 为准；A2/A3 名称不能替代硬件核验。
2. 使用现有 SSH 认证及严格主机校验。读取 `uname -m`、`/etc/os-release`、`npu-smi info`、`command -v netdata`，并在服务器上检查 `curl --noproxy '*' --max-time 2 http://127.0.0.1:19999/api/v1/info`。不要停止或修改现有训练进程。
3. 有健康 Agent 时先核验版本、UID、监听地址、四个 context 与授权策略，复用它。没有 Agent 时，确认维护授权、专属目录不存在、端口未被其他服务占用，以及系统资源余量。按实机架构选包：`aarch64` 对应 ARM64；`x86_64` 对应 AMD64。

SSH 主机密钥变化必须从控制台、管理员或其他可信渠道核验。`ssh-keyscan` 只获取候选公钥，不能自行证明可信。保存旧记录，核对新指纹后只修改该目标的记录或使用专属 known_hosts 文件。认证失效时请维护者更新本地凭据文件；不要反复猜密码、关闭校验或将密码放入命令行参数。

## 内网离线部署

优先参考 [Netdata 官方离线安装文档](https://learn.netdata.cloud/docs/netdata-agent/installation/linux/offline-systems)。联网机器通过官方 kickstart 的 `--prepare-offline-install-source` 准备完整目录，再传入服务器执行生成的 `install.sh`。提前核对启动、更新、遥测、已有服务处理与安装路径参数。

本 Skill 另提供独立试用脚本 `scripts/deploy-preview.sh`。它仅支持已授权的 Linux root/systemd 部署，固定使用 `/home/t00906153/simplehtmlwatch-netdata` 和 `shw-netdata-preview.service`。专属目录或服务已存在、已有健康 Agent 或包校验失败时拒绝覆盖。不要直接在其他操作系统运行它。

在联网机器从 [Netdata 官方 v2.12.0 Release](https://github.com/netdata/netdata/releases/tag/v2.12.0) 下载与实际架构一致的 `netdata-<架构>-v2.12.0.gz.run`，并核对可信 Release 资产 `digest` 或同页 SHA256 清单。记录版本、来源、架构和校验值。需要代理时仅在联网机器下载阶段使用本机代理；不要假定远端有外网。

```bash
# 将包和脚本传到目标服务器的本次专属临时目录；保留严格 SSH 校验。
scp <官方静态包> <Skill目录>/scripts/deploy-preview.sh <授权账号>@<服务器>:<本次临时目录>/
# 在目标服务器运行，SHA256 必须来自已核验的官方来源。
bash <本次临时目录>/deploy-preview.sh <静态包绝对路径> <可信SHA256>
```

脚本以 `--noexec` 解压官方自解压静态包，不运行其系统安装器；直接运行 `runtime/bin/srv/netdata`，显式指定专属配置和数据目录。包内 `bin/netdata` 包装脚本硬编码 `/opt/netdata`，不能用于此目录。此方法是独立试用部署方案，升级前重新核对新版本包结构和兼容性。

openEuler 启用 SELinux enforcing 时，脚本参照 `/usr/bin/true` 为本次独立二进制设置可执行标签，不关闭 SELinux。全机重新标记后需核验该文件标签；需要长期正式部署时，由维护者按本机策略持久化准确路径的文件上下文。若安装中断，保留目录和日志，先诊断并修复本次专属服务；脚本不会覆盖残留目录来掩盖失败。

脚本配置：2 秒采样，dbengine 一个层级、128 MiB 保留容量、16 MiB 页面缓存；关闭 ML、告警、StatsD 和外部采集插件，启用系统 proc 采集；HTTP 只绑定 `127.0.0.1:19999`。使用匿名统计退出标记、`DISABLE_TELEMETRY=1` 与 `DO_NOT_TRACK=1`；不执行 Cloud claim，不配置自动更新。128 MiB 只限制该数据库层保留目标，不等于 Agent 进程内存上限，也不保证特定历史时长。可根据实际开销和所需历史调整。

## 接到中台

1. 用本 Skill 的 `discover` 核验目标实例。读取公开 `/api/config`；保留其他机器、凭据组与编辑留空的秘密，按物理主机去重。不要覆盖运行实例的加密配置文件。
2. 通过“管理机器”或维护者已授权的配置 API 添加对应主机、SSH 端口、账号凭据组和 NPU/进程监控指令。凭据留在本机加密存储，不写入 Skill。Netdata 端口填实际远端回环端口，Agent 健康后启用其开关。
3. 中台有独立的 SSH 指纹存储。核对中台待信任的指纹与可信公钥记录相同后，准确批准该主机；不能因为系统 ssh 成功就打开全局自动信任。若有 Bearer 保护，配置 Agent 实际接受的本地令牌，不用 Cloud API 令牌猜测。
4. 用 `query --machine <ID>` 经中台实际查询，再用 `summary` 检查全部已授权目标。服务器 SSH 必须允许该账号的 `direct-tcpip` 到远端回环端口，无需在 Mac 开额外转发端口，也无需开放远端 19999 入站。
5. 认证暂不可用的目标仍登记，保持 Netdata 开关关闭并标明原因；不能以登记代替实际接入成功。将机器名称、架构、Agent 版本/UID、服务名、目录、四项指标点数和失败原因保存在本地部署记录，不分发内部机器清单。

## 实际验收和回退

- 通过 `ss -lntp` 核验只监听回环，`systemctl show shw-netdata-preview.service -p MainPID -p ActiveState -p MemoryCurrent` 核验专属进程。用实际 API 核验四个 context 均有数值；零是有效数据。核验时钟、可读历史以及 SSH 断连错误显示。
- 检查 `ps` 和专属目录增长；结合开启前后同条件实验衡量 CPU、内存、磁盘及训练影响。单次观察的 RSS、CPU 或曲线不能证明开销足够低，不声称完成了 NPU 性能验收。
- 此脚本只启动其专属服务。维护者需要停止时，先核对 `systemctl cat shw-netdata-preview.service` 的执行路径及目录里的 `owner-marker`，再执行 `systemctl disable --now shw-netdata-preview.service`；保留目录、日志和数据库作为恢复证据。不要停止名称相近的其他 Agent，更不要据此重启 simpleHtmlWatch 或清理其锁文件。
- Agent UID 是数据源身份，但相同地址、账号或实例名称不能保证物理主机未被重装。任务关联前核对主机变化和指标历史；重装后的数据不能冒充旧任务观测。

## 分发给其他 AI

只分发 Skill 目录或 Skill zip。告知实际实例名和可选数据目录；其他 AI 先运行 `discover`，再运行 `machines` 与 `summary`。当前 Mac 试用使用 `netdata-mac-preview`，这是交接信息，不是跨电脑通用实例名。AI 日常查询不需要服务器密码，不自动部署、修改凭据或管理实例生命周期。
