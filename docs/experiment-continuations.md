# 实验等待与原 AI 对话自动继续

资源不足时保存持久化等待单；中台检查资源，整组暂留；原 AI 对话领取后使用固定任务 ID 继续实验。任务结束后产生结果核对事件，AI 核对实验判据并汇报完成后释放资源。

需要能力 `experiment-continuations-v1`。本说明对应源码开发版本，正在运行的旧构建不会因代码修改自动升级。唯一维护者应按 [实例生命周期](service-lifecycle.md) 升级，客户端不得自动重启或切换版本。

## 恢复入口

| 模式 | 适用对象 | 行为 |
|---|---|---|
| `heartbeat` | Codex 桌面原对话或无外部连接入口的宿主 | 原对话定时醒来，查询并即时评估资源，足够时立即领取 |
| `bridge` | 已核实归属的 Codex App Server | 中台先暂留，通过已有控制 socket 向原 threadId 发起新一轮输入 |

桌面原对话优先 heartbeat，由原对话调用宿主的定时工具创建，保留当前上下文，不创建 standalone cron、新对话或直接修改自动化文件。中台登记和宿主唤醒配置是两步，不能仅登记就声称会自动继续。

能读取对话历史不等于拥有桌面运行会话。bridge 默认延后 `notLoaded` 对话。确认选定宿主可续接该存储对话、没有其他写入者后，操作者可显式使用 `--allow-cold-resume`。桥接不新建/fork 对话、不改模型或权限，不自动回答审批请求。

电脑、中台与 AI 宿主应保持运行。睡眠/离线会延迟检查，恢复后只查询原记录，不重放任务。Unix socket 协议有本地 fixture；本次运行检查在 macOS，其他平台与 Windows 桌面连接需要单独验收。

## 资源与时间

固定北京时间窗口为 `[12:30,14:00)`、`[02:30,06:00)`。默认平时每 600 秒、波谷每 60 秒检查，进入/退出窗口时调整检查时机，中台任务结束或原预留释放后重新评估。登记后首次检查在下一次调度循环内执行，通常约 1 秒。

`ready` 保持原定义。等待合同额外检查确切 NPU 型号、整机设备总数、每设备利用率与可用 HBM、整机无计算进程，以及 `npu` 命令的成功退出、完整性和完成时间。devicesPerMachine 是预期整机设备行总数，必须与实际一致，不能只填本步骤需要的部分卡数。缺失进程表、显存未知、过期或截断采样均不计为空闲。按 resourceId 和同 host 去重，本实例任务、unknown、原预留、新暂留及 Grant 共同阻止重复占用。

可选 `resources.labels` 匹配机器配置的 `labels`，适合声明运行时或拓扑域。这些标签是维护者声明，不是实测环境证明。首版解析 A2/A3 与 A5 风格表格；布局不识别时保守拒绝，提供脱敏样例补充解析，不放宽门禁。

`stableForSeconds=600` 表示连续空闲 10 分钟，与检查周期独立。中台持续观察原始采样；忙碌、缺失或间隔超过 `min(2×监控间隔,maxSampleAgeSeconds)` 会重置计时。重启不沿用旧稳定计时。

## 登记合同

检查点应包括目标、步骤、代码 revision/未提交改动、已完成证据、原任务 ID、下一步计划、资源预算、成功判据与停止条件。在本步骤期间保持文件不变；后续步骤另存检查点并使用新 stepId。客户端计算 SHA-256，直接调用 HTTP 时提供 digest。文件最大 1 MiB。

下面为模板，替换所有路径、对话 ID、组名与命令；`model` 是 `npu-smi` 的确切型号，不是泛称“A3”。两条命令仅为无硬件计算的链路示例，实际实验需使用原授权计划。

```json
{
  "schema": "simplehtmlwatch.continuation.v1",
  "id": "exp-demo-step1-wait",
  "experimentId": "exp-demo",
  "stepId": "step1",
  "target": {
    "provider": "codex", "hostId": "local",
    "threadId": "EXPLICIT_ORIGINAL_THREAD_ID",
    "workspace": "/absolute/path/to/experiment"
  },
  "resources": {
    "group": "A3-test-pool", "machineCount": 2,
    "model": "910B", "devicesPerMachine": 2,
    "minFreeHbmMB": 60000, "maxUtilization": 5,
    "maxSampleAgeSeconds": 60
  },
  "policy": {
    "timezone": "Asia/Shanghai", "checkEverySeconds": 600,
    "windowCheckEverySeconds": 60, "stableForSeconds": 0,
    "offerTTLSeconds": 300, "wakeMode": "heartbeat"
  },
  "checkpoint": {"path": "/absolute/path/to/checkpoint.json"},
  "authorizationRef": "local-record-of-user-authorization",
  "authorized": true,
  "tasks": [
    {"id": "exp-demo-step1-node0", "shell": "printf 'node0\\n' > node0.txt"},
    {"id": "exp-demo-step1-node1", "shell": "printf 'node1\\n' > node1.txt"}
  ],
  "existingTaskIds": []
}
```

也可用 `machineIds` 代替 group，二者互斥。任务计划条数等于 machineCount，ID 提前固定且不被其他等待单/已有任务占用，同一实验步骤只能登记一单。命令在各任务 `$SHW_RESULTS_DIR` 执行；进入代码目录时写清绝对路径。多机启动可能部分成功，整组分配不等于启动原子性。

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py discover
python3 skills/cluster-task-controller/scripts/taskctl.py await-resources --request /absolute/path/to/request.json
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-status exp-demo-step1-wait
```

传输不确定时先查询，不换 ID 自动重发。`existingTaskIds` 只引用本中台原任务；任一未结束/缺失记录都会继续等待。可选 `expiresAt` 是带时区的授权截止时间，到期不能再领取或启动；已领取占用不因到期释放。已获授权的实验不重复索要相同授权，也不扩大范围。

## 原对话心跳

登记后原对话应通过宿主自动化工具创建绑定自身的 heartbeat，保存等待 ID、稳定 serviceId、实例名、数据目录与检查点。建议每 10 分钟检查，按宿主支持情况增加波谷入口检查。保存指令可以使用：

> 继续本对话中已授权的实验。先发现绑定实例并查询指定等待单与原任务 ID。waiting_resources 时 continuation-evaluate；offered 时核对检查点、按当前代领取，再 continuation-submit 固定计划。已有任务只查询，不换 ID 重跑。任务结束时核对退出码、回收包与原实验判据，证据齐全再汇报完成。暂停时静默、不推进，保留网页恢复后的检查能力。状态不变或没有可领取资源时保持安静；只通知重要变化、完成、失败或需要用户输入。完成、取消或授权到期后停用心跳。

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-evaluate exp-demo-step1-wait
```

心跳模式下中台仍定期更新合格数和排除原因，原对话醒来时即时评估/暂留/领取，不提前生成 5 分钟暂留等待 10 分钟心跳。模型恢复延迟由宿主心跳决定，中台 60 秒资源检查不能声称为每 60 秒恢复模型。

仓库客户端不修改 Codex 内部定时文件，不代替原对话调用宿主自动化工具。包内 Skill 指导原对话完成配置，宿主必须实际提供该能力。

## 主动桥接

只读 `codex app-server daemon version` 可提供现有 socketPath 和版本。地址应来自真实宿主，不能使用例子推测，也不启动/重启 daemon。

```sh
codex app-server daemon version
python3 skills/cluster-task-controller/scripts/continuation_bridge.py \
  --sock /absolute/path/to/existing-control.sock \
  --probe-thread EXPLICIT_ORIGINAL_THREAD_ID --workspace /absolute/path/to/experiment
python3 skills/cluster-task-controller/scripts/continuation_bridge.py \
  --instance canary --data-dir /absolute/path/to/canary-data \
  --sock /absolute/path/to/verified-owner-control.sock --host-id local
```

探测只初始化 WebSocket 并读取 thread，不调用 resume/turn/start。主动等待单使用 `wakeMode=bridge`。桥接只用 Python 标准库，核对原 ID、cwd、归档与忙闲状态，不复制账号令牌或绕过审批。

turn/start 前保存 `sending`，响应或回执丢失后查询 `[SHW_EVENT:事件ID]` 用户输入及 turnId；找不到标记不能证明未送达，应保持 `delivery_uncertain`，不重放。明确拒绝时在当前事件内最多退避重试三次。迟到事件只能查询/领取当前代，旧代不能启动。

尚未启动、没有未核实发送/启动意图时可切换模式；这会清除暂留并推进代数。切换后配置对应心跳或启动验证过的桥接，一单只使用一种模式：

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-control exp-demo-step1-wait set-mode --wake-mode heartbeat
```

## 领取、执行、核对

使用查询响应里的 generation、wake.id、grantId，替换以下 G、EVENT_ID、GRANT_ID，不猜测：

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-claim exp-demo-step1-wait \
  --thread EXPLICIT_ORIGINAL_THREAD_ID --generation G --event EVENT_ID
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-submit exp-demo-step1-wait \
  --thread EXPLICIT_ORIGINAL_THREAD_ID --generation G --grant GRANT_ID
```

领取/提交前检查摘要，服务复核资源、授权机器范围、SSH profile/用户名和固定计划。机器分组、物理身份、地址或 SSH 用户发生变化时拒绝旧分配；密钥轮换不改变相同 SSH 用户的身份。每个子任务提交前先查询原 ID，写请求不确定即退出；部分启动只能查询各原任务，不能整组重跑。

`offered` 默认 5 分钟过期；已领取的 claimed/executing/recovery_needed 无 TTL。任务之前持久化启动意图，缺失记录但存在意图时保留占用。不能用删文件、超时或 AI 口头确认解除不确定资源。

所有子任务结束后 `awaiting_review` 生成新的 tasks_finished 事件。查询当前 wake.id，核对退出码、回收包和原实验判据。所有任务退出码明确且包已回收，才可完成：

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-report exp-demo-step1-wait \
  --thread EXPLICIT_ORIGINAL_THREAD_ID --generation G --event EVENT_ID \
  --action complete --outcome succeeded --evidence /absolute/path/to/verified-result-report.md
```

任务成功不等于实验成功，outcome 按原实验判据确定。完全没有启动意图/任务时可 `--action release-unused` 退回等待。后续步骤建立新检查点与新 stepId。

## 控制、存储与接口

任务中台显示等待单、原对话、合格数量、代数、送达与排除原因，支持 pause/resume/cancel。等待时清除未领取暂留；执行中只设置 stopRequested，不杀远端、不释放占用。

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-control exp-demo-step1-wait pause
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-control exp-demo-step1-wait resume
python3 skills/cluster-task-controller/scripts/taskctl.py continuation-control exp-demo-step1-wait cancel
```

| 方法 | 路径 | 用途 |
|---|---|---|
| GET / POST | `/api/continuations` | 列表、`?id=...` 查询 / 幂等登记 |
| POST | `/api/continuations/evaluate` | heartbeat 即时评估 |
| POST | `/api/continuations/claim` | ID、generation、wakeEventId、threadId、checkpointDigest 领取 |
| POST | `/api/continuations/report` | 完成证据 / 退回未使用资源 |
| POST | `/api/continuations/control` | pause / resume / cancel / set-mode |
| POST | `/api/continuations/delivery` | 桥接意图、回执、查询恢复和退避 |

新子任务 `/api/tasks` 附 continuationId、generation、grantId、固定 machineId，不与原 reservationId 混用。保留会话令牌和 X-Watch-Instance 校验，重新发现但不自动重放写请求。

`tasks/continuations.json` 保存等待单、分配、启动意图和事件箱，最多 1000 单/8 MiB，另保留最近 32 个旧事件。损坏、重叠占用或任务关联缺失会拒绝恢复，不清空重建。私有记录包含命令与路径，文件仅当前用户可读写，不进入公共仓库或发布包。

预留仅覆盖本实例的任务/预留入口；人工 SSH、即时执行、其他实例需遵守协作规则。旧版本不认识新占用，有活动等待单/Grant 时不得直接降级。

## 验证边界

Go race/vet、Python unittest 和浏览器回归使用模拟 NPU、模拟 SSH、临时存储、假宿主与本地 WebSocket，验证分配竞争、过期、重启、响应丢失和固定 ID 去重。

真实 socket 只读握手/原 ID 读取与假宿主主动发送分别记录。真实原对话主动接续、宿主心跳及 A2/A3 空闲识别/实验正确性应在选定试点验收，不能以 fixture、编译或读历史代替。

## 常驻失败残留清理

支持能力 `queue-residue-cleanup-v1` 的构建提供实例级策略，初始关闭。维护者根据用户已有授权启用一次后，策略保存在 `tasks/.cleanup/policy.json`，服务重启后继续生效，不要求每个等待单重复授权。

Windows、macOS 和 Linux 均可直接在浏览器的「任务中台」顶部操作「常驻排队清理」开关。点击启用即授权当前实例按页面所列规则执行，页面自动保存带唯一操作标识的授权说明，无需安装 Python、手写授权文件或运行命令。看到「已启用」即表示服务端已保存；关闭后保留诊断和清理日志。

展开「诊断与清理日志」，可查看最近 200 条记录的机器、PID、分类、原因与更新时间；点选记录可查看进程日志末尾、诊断经过、终止意图和回执，以及完整 JSON 证据并下载。页面区分调试保护、无法判断、观察中、终止请求返回与后续核查，不将请求成功显示成资源已释放。页面可见时每五秒刷新状态，读写都核对实例身份；写入响应不确定时只重新读取状态，不自动重发。读取失败时保留上次显示并禁用开关，缺少能力时提示维护者升级。

以下命令行方式仍供 AI 或脚本使用：

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py --instance 实例名 cleanup-status
python3 skills/cluster-task-controller/scripts/taskctl.py --instance 实例名 cleanup-policy --enable --authorization-ref /absolute/path/to/user-authorization.txt
python3 skills/cluster-task-controller/scripts/taskctl.py --instance 实例名 cleanup-status --id residue-实际日志ID
python3 skills/cluster-task-controller/scripts/taskctl.py --instance 实例名 cleanup-policy --disable
```

只在已授权、未暂停、未到期的 `waiting_resources` 等待单需要至少 4 台机器且当前合格空闲机器不足时触发。诊断限定于原 group/machineIds、型号、标签和设备总数范围；HBM 预算保留在清理后的原资源分配检查中，不要求失败进程退出前已满足可用 HBM 预算；不清理被本中台 task、unknown、reservation、暂留或 Grant 持有的机器，也不替旧的未知任务解除占用。普通单机 `/api/tasks` 和只申请资源的整组 reservation 没有排队等待单，不触发本规则。

候选必须有完整、新鲜、成功退出的 NPU 原始采样，整机仅一个 NPU 进程行、逐设备利用率均为 0。诊断进一步检查程序年龄超过 300 秒、进程和父进程身份、日志、CPU 与调试证据。当前自动分类器明确支持 Python multiprocessing forkserver 原监督进程退出、仅剩一个 worker 的失败残留；其他结构没有足够事实时记为 unknown，继续等待。不能因为未发现调试器就推断可以终止。

调试器附着、停止状态、调试命令、交互输入、`SHW_DEBUG_PROTECT=1`、非零 `PYTHONBREAKPOINT` 或进程工作目录的 `.shw-debug-protect` 文件均会保护进程。Python 内部断点无法仅靠命令行完全排除，因此清理还要求明确失败日志、原监督进程已退出、无活跃同父 worker/子进程，以及连续至少五分钟的日志和 CPU 无进展证据。每台最多约每 60 秒诊断一次；观测断档超过 150 秒、进程身份、日志或 CPU 进展变化会重新计时，服务重启也重新计时。判定允许五分钟内不足 1 秒的累计 CPU housekeeping，不把微量驱动线程活动误认成计算进展。

终止前重新核对等待状态、范围、机器配置、进程、父进程、调试保护、日志和全部设备当前状态；诊断/清理期间阻止本实例分配同一物理资源。清理意图先持久化，再通过 Linux pidfd 仅发送 SIGTERM，不发送 SIGKILL、不按名称匹配批量终止。缺少 Python 3.9+ 的 pidfd 支持或 Linux 相应能力时保守失败。暂停/取消和终止决策使用同一锁串行化；已开始的终止操作不因后续暂停撤回。

本机 `tasks/.cleanup/residue-*.json` 保留初始/最近诊断、日志末尾、判定原因、终止意图、回执和后续进程核查；远端 `$HOME/.simplehtmlwatch/cleanup/` 保存发送前的证据与意图。传输不确定、回执丢失或服务重启后，只查询原 boot ID/PID/启动身份，不重放信号。确认原进程退出后，由原等待单重新核对整组资源；进程退出不代表 NPU 已空闲、实验已启动或实验成功。

日志权限为当前用户可读写，敏感字段做基本脱敏，不保存环境变量全集或 SSH 凭据。日志末尾是片段，不是完整日志或内存转储。本机日志上限 20 MiB，不自动删除清理证据；满额或持久化失败时停止终止操作并报告，维护者归档日志后处理容量。完整单条日志通过 `cleanup-status --id` 读取，摘要最多列出最近 200 条。

HTTP：`GET /api/tasks/cleanup` 查询策略、摘要和错误；`GET /api/tasks/cleanup?id=...` 读取完整记录；`POST /api/tasks/cleanup` 提交 `{ "enabled": true, "authorizationRef": "已有用户授权记录" }`。保留本机会话令牌、Origin/Host 和 runId 校验。能力缺失时客户端报错，不自动升级或重启中台。
