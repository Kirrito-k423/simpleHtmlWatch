# 内部 AI 测试包：从这里开始

本包已包含可执行程序、浏览器任务页面、`skills/cluster-task-controller/`、`skills/simplehtmlwatch-netdata/` 和完整 [任务 SOP](docs/task-controller.md)。运行本机程序不需要安装 Go、Node.js 或 Python；AI Skill 使用 Python 3 标准库。程序和 AI 客户端必须运行在同一台电脑上，因为服务只监听 `127.0.0.1`。

## 人工启动与页面试用

1. 选择与运行程序的电脑系统、架构相符的发布包并解压。Windows 双击 `start.bat` 或 `simpleHtmlWatch.exe`；macOS 双击 `start.command`；Linux 在文件管理器中运行 `start.sh`。保持启动窗口打开。macOS 包未签名，系统首次打开时可能要求在系统设置中允许。
2. 浏览器会自动打开本机页面。若没有自动打开，使用启动窗口“监控页面”后面的地址。点击“管理机器”，配置 SSH 地址、账号及密码或本机 SSH key。默认密码失败后尝试密钥；指定私钥时填写本机绝对路径或 `~/` 路径，加密私钥另填口令。先让监控完成采样，并核对主机指纹。
3. 点击“任务中台”。不指定机器或组时由中台选择 ready 机器；也可指定机器或组。提交前确认将运行的完整命令。页面显示选中的机器、任务状态、真实事件时间泳道、日志和结果下载。
4. 第一次只发送无副作用任务，例如 `printf 'ok\n' > "$SHW_RESULTS_DIR/check.txt"`。确认退出码为 0、日志和结果包后，再测试实际工作负载。

点击“系统指标”可查看已启用 Netdata 机器的四类系统曲线；任务详情可打开运行期间指标。默认关闭，接入与验收边界见 [Netdata 接入说明](docs/netdata.md)。实例能力增加 `netdata-metrics-v1`，这是只读观察能力，ready 与调度规则保持原定义。

查看集群指标的 AI 阅读 [simplehtmlwatch-netdata Skill](skills/simplehtmlwatch-netdata/SKILL.md)。也可单独分发 `simplehtmlwatch-netdata_*.zip`；包不包含机器凭据或内部清单。向 AI 提供实际实例名和可选数据目录，依次运行 `metricsctl.py --instance <实例名> discover`、`machines`、`summary`。批量查询会明确报告未接入机器与单项失败，不需向查询 AI 提供 SSH 密码；维护者部署方案见 Skill 的 `references/deployment.md`。

## 交给内部 AI

先把 [简短全局 Prompt](AI-GLOBAL-PROMPT.md) 复制到 Windows 端各 AI 工具的全局指令，让已有会话重新读取。Web 左上角始终显示当前程序版本；悬停版本号可查看实例和构建摘要。

把本包的 `skills/cluster-task-controller/` 目录提供给内部 AI 的 Skill 机制；发布页也单独提供 `cluster-task-controller_*.zip`。若该 AI 不支持自动安装 Skill，就直接让它阅读 `skills/cluster-task-controller/SKILL.md`。同时给它实例名（默认 `default`）；若使用自定义数据目录，再提供该目录。说明运行程序的电脑与 AI 客户端必须是同一台。可以复制下面这段作为测试指令：

> 请先阅读 `skills/cluster-task-controller/SKILL.md`，使用 simpleHtmlWatch 本机任务中台。使用实例【default 或实际实例名】，先运行 discover 校验地址和身份，再只读查询 ready 机器；随后把准备提交的完整命令、目标范围和结果目录展示给我。得到我对这条任务的授权后提交，记录任务 ID，跟踪退出码、日志、归档状态并下载结果。没有指定机器时允许自动调度，不要把 ready 当成 NPU 空闲。若状态为 unknown，不要换 ID 重复执行。端口不可达、版本不符或能力缺失时只报告并重新发现，禁止自动重启、杀进程、删锁或切换服务版本。

Skill 的客户端脚本是 `skills/cluster-task-controller/scripts/taskctl.py`。它默认从 `service.json` 发现地址、校验在线身份，再从页面取得临时会话令牌；无需把 SSH 密码交给 AI。内部 AI 使用它时需要本机 Python 3；浏览器页面本身不需要 Python。

## 试用范围

此版本是测试发布。自动选机依据最近一次 SSH 监控和中台任务占用，不检查 NPU 显存或资源。时间泳道的事件时间是中台受理或观测时间；连续等比例显示，支持滚轮缩放和 Shift + 滚轮平移，所有注册机器均可见。任务最新优先，默认仅显示运行中和发送中的任务，历史与结果下载从“全部任务”进入。未知任务可直接在页面重新探测、读日志，在人工核实远端及子进程停止后确认解除占用；独立预约仍由原调用方管理。当前每次任务只落一台机器，压缩结果归档上限为 10 GiB（10,737,418,240 字节）。服务端与包内 Skill 均流式传输；使用 10 GiB 上限需要同时更新中台和 Skill 至 v0.5.0-rc.3 或更新版本。模拟 SSH 测试通过不代表已在真实内网集群完成端到端验收。

## 机器级 NPU 利用率历史

点击“NPU 利用率”查看今天、昨天或近 7 个完整自然日的跨机器小时均值、峰值、低负载占比和覆盖率。复用已有 NPU 采样，不需要 Netdata 或开启文本历史；只保存机器级小时桶，最多 20 MiB，保留最近 30 个自然日。未采集的过去无法补回。

AI 使用已更新的 `simplehtmlwatch-netdata` Skill，先 discover 核对 `accelerator-utilization-v1`，再执行 `metricsctl.py --instance <实例名> utilization --period yesterday` 或 `--period week --group <机器组>`。退出码 3 代表历史不全，不是服务宕机。时间按北京时间，缺失不补零；低谷不证明当前资源允许部署。详细口径见 [利用率说明](docs/utilization.md)。

## 常驻排队清理

在浏览器进入「任务中台」，点击顶部「常驻排队清理」开关即可启用，页面自动保存操作授权记录，无需 Python 或命令。展开「诊断与清理日志」可查看判定、进程日志片段、清理回执和完整证据，并下载单条记录。开关以服务端保存结果为准；读写不确定时只查询，不自动重试写入。

Windows 已安装旧版时，由维护者核对实例并关闭对应启动窗口，再双击新包的 `upgrade.bat` 明确采用新构建，保留原数据目录。旧实例仍在线时会复用旧实例，不会抢占；普通 `start.bat` 继续遵循固定构建规则。命名实例或自定义数据目录需在启动入口沿用原 `-instance` / `-data-dir` 参数。

具备 `queue-residue-cleanup-v1` 时，AI 或维护脚本也可按已有用户授权启用 `taskctl.py cleanup-policy --enable --authorization-ref <授权记录>`，策略重启后保留。至少四台机器的有效等待单资源不足时，诊断原资源范围内的单卡零利用率占用；确认失败残留和五分钟无进展后才通过 pidfd 定点 SIGTERM。debug 和无法判断的继续等待，已有任务/unknown/预留继续受保护。

`cleanup-status` 查看启停、错误及日志摘要，`cleanup-status --id <日志ID>` 查看证据；`cleanup-policy --disable` 停用。诊断日志、终止意图和回执持久化，丢失回执不重发信号。详见 [规则与边界](docs/experiment-continuations.md#常驻失败残留清理)。此处需要远端 Linux/Python/pidfd 能力，测试夹具通过不等于真实硬件上的自动清理验收。

## 多 AI 与版本升级

AIConnector 八机任务需要 `atomic-group-reservations-v1`，同实例一次分配完整物理组。控制机绑定固定命令 SHA、任务 ID、Grant 与代数；原启动意图不重放。参见 [整组预留](docs/atomic-group-reservations.md)。本机 API 验收不代表真实 64-rank 实验已通过。

支持 `experiment-continuations-v1` 的构建可登记资源等待单，默认在北京时间 12:30–14:00、02:30–06:00 每分钟检查，其他时间每 10 分钟检查。满足逐设备 NPU、显存、型号和整组数量条件后继续原对话；ready 自身不证明 NPU 空闲。详见 [实验等待与续聊](docs/experiment-continuations.md)。

先保存准确原对话 ID、检查点、固定子任务 ID 和已授权计划，再用 `taskctl.py await-resources --request ...`。桌面原对话优先配置宿主 heartbeat；核实归属的 App Server 可用 `continuation_bridge.py` 主动接续。能读历史不等于拥有当前会话。登记与唤醒入口配置是两步，不能只登记就声称会自动继续。原授权范围内不重复确认，响应不确定先查询，已领取占用不超时释放；完成后核对退出码、回收结果和原实验判据。

正式实例由一个维护者管理启停。各 AI 使用相同实例名和数据目录；不要给每个 AI 建一份正式服务，也不要把历史端口写进守护重启脚本。测试版本使用 `-instance canary`，历史旧版使用单独 `-data-dir`；任务记录和资源预留不跨实例共享。清除历史 `SHW_URL` 后可改用自动发现。

同实例重复启动会复用现有进程。换构建需先停止原实例，再显式 `-adopt-build`；端口被占用时报冲突，不停止占用者或自动改端口。旧二进制不认识新规则，需要维护者统一旧启动入口。步骤及边界见 [服务实例与版本管理](docs/service-lifecycle.md)。
