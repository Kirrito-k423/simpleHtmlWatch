# SSH 任务中台标准 SOP

此功能在本机 simpleHtmlWatch 中增加持久化的长任务流程。AI 和脚本通过本机 HTTP API 提交任务，中台选择一台 ready 机器，用既有 SSH 凭据和主机指纹发送命令，随后查询远端退出标记并回收结果。一次请求只调度一台机器；需要多机任务时分别提交并记录各自任务 ID。

## 1. 安装与启动

从 [预发布页面](https://github.com/Kirrito-k423/simpleHtmlWatch/releases) 下载与本机系统、架构对应的压缩包，解压后按 [内部 AI 试用说明](../AI-START-HERE.md) 启动。Windows 双击 `start.bat`，macOS 双击 `start.command`；Linux 运行 `start.sh`。本机运行程序不需要 Go、Node.js 或 Python。远端需要 Linux、Bash、`base64`、`nohup`、`tail` 和 `tar`，不需要安装 Agent。

开发者从源码验证时才需要 Go 1.26 或更新版本，以及运行页面时间轴检查脚本所需的 Node.js：

```sh
go test ./...
node scripts/test-task-timeline.cjs
go build -o simpleHtmlWatch .
./simpleHtmlWatch -no-browser
```

控制台显示当前监控页面地址、实例名和发现文件。首次自动选空闲端口，以后复用登记端口；端口冲突时报错，不自动漂移。保持程序运行。任务中台继续复用“管理机器”中的密码/SSH key 和已信任主机指纹，密码失败时自动尝试密钥；私钥路径属于运行中台的电脑，AI 无需取得私钥或口令。先让目标机器完成一次监控采样。

发布包内附 `skills/cluster-task-controller/scripts/taskctl.py`，只依赖 Python 3 标准库。它先按实例发现并校验 `/healthz` 身份，再读取首页的会话令牌、绑定当前 runId 访问任务 API，不需要保存 SSH 密码。内部 AI 先阅读包内 Skill；若已安装到 Codex 全局目录，也可使用全局入口：

```sh
TASKCTL="${CODEX_HOME:-$HOME/.codex}/skills/cluster-task-controller/scripts/taskctl.py"
python3 "$TASKCTL" discover
python3 "$TASKCTL" ready --group 训练集群
```

浏览器在控制台显示的地址后加 `/?tasks=1`，或从首页点“任务中台”，可在同一页发送任务、查看任务时间泳道、读取日志和下载结果包。界面继承本机会话令牌；它操作真实任务，不是原型演示。

端口、构建固定、多实例和旧版迁移详见 [服务实例与版本管理](service-lifecycle.md)。AI 不得用重启中台来修复连接失败。使用自定义目录时，每条命令传同一 `--data-dir`；测试实例传同一 `--instance`。

## 2. 选机器与提交

请求可以不指定选择器，此时在所有 ready 机器中自动选机。`machineId` 精确选机；`group` 限定机器组；后两者不能同时指定。所有模式都按机器 ID 顺序选择第一台符合条件的机器，实际落点以提交响应的 `selectedMachineId` 为准。自动模式可能选到不同硬件；依赖具体型号的命令应指定机器或组。ready 的条件是：已启用、最近监控状态为 `online` 或 `partial`、状态未超过 `2 × 监控间隔 + 30 秒`，并且中台没有在同一机器或同一 SSH 地址上保留未完成任务。这里不解析 `npu-smi` 的显存或占用情况，不能据此声称 NPU 空闲。

先准备待运行命令。中台在远端设置 `SHW_RESULTS_DIR` 并从该目录启动 Bash，任务应把需回收的文件写入这个目录：

```sh
cat > /tmp/example-task.sh <<'SH'
printf '开始运行\n'
printf '示例结果\n' > "$SHW_RESULTS_DIR/answer.txt"
SH

python3 "$TASKCTL" submit \
  --group 训练集群 --command-file /tmp/example-task.sh

# 若此命令适用于所有 ready 机器，可省略机器和组：
python3 "$TASKCTL" submit \
  --command-file /tmp/example-task.sh
```

脚本在发送请求前打印任务 ID；保存这个 ID。提交成功返回 `202` 和 `selectedMachineId`，并进入 `dispatching`。相同 ID 与完全相同的命令、选择器再次提交，只返回原任务；同一 ID 换命令或选择器返回 `409`。提交超时或断线时，先按原 ID 查状态；需要重发时传 `--id <原ID>` 且保持请求内容完全相同。不要生成新 ID 自动重试。

## 3. 进度、退出与日志

```sh
TASK_ID=粘贴提交输出中的实际任务ID
python3 "$TASKCTL" status "$TASK_ID"
python3 "$TASKCTL" logs "$TASK_ID"
python3 "$TASKCTL" wait "$TASK_ID"
```

状态含义：

| 状态 | 含义 | 处置 |
| --- | --- | --- |
| `dispatching` | 本地已持久化，即将尝试 SSH 启动 | 按原 ID 查询 |
| `running` | 远端记录的 PID 仍存在且未见退出标记；PID 可能复用 | 查询日志；不要再次启动 |
| `unknown` | SSH 启动或状态查询无法确认 | 只做读查询；该机器保持占用，避免重复执行 |
| `succeeded` | 读到远端退出码 0 | 检查 `archiveReady` 与产物 |
| `failed` | 读到非零退出码，或已确认未启动 | 查看 `error`、`exitCode` 与 stderr |
| `abandoned` | 人工核实远端已停止后解除本地占用 | 远端退出结果仍未知；不能当成功 |

`logs` 返回远端 `stdout.log`、`stderr.log` 各自最后 16 KiB。它是日志预览，不代表完整输出。远端目录为 `$HOME/.simplehtmlwatch/tasks/<任务ID>/`，包含 `command.sh`、`runner.sh`、`runner.pid`、`stdout.log`、`stderr.log`、`exit.code` 与 `results/`。远端任务后台运行，本机程序退出不会主动终止它；本机重启后重新读取任务记录、查询远端标记，**不会重发启动命令**。若进程消失但没有退出标记，会保留 `unknown`，不会推断成功。

每条新任务的 `events` 持久记录受理、启动确认、状态变为未知、退出码确认和归档结果。时间戳是**本机受理或观测到状态变化的时间**；5 秒轮询可能晚于远端实际退出。浏览器按实际机器分组，每条任务用一条长条表示受理至结束（未结束时至现在）的占用时间，受理、运行、退出与回收以不同颜色和形状的里程碑标记。横轴是连续等比例时间，完整范围从最早可记录的事件开始，包含全部任务；长时间无事件也不压缩。鼠标放在时间区域滚轮缩放，以鼠标位置为中心；Shift + 滚轮平移，也可用全局、放大、缩小、最早、最近按钮。任务按创建时间从新到旧排列，机器分组按最新可见任务排序。默认筛选“运行中（含发送中）”，成功完成和其他历史任务隐藏；可切换“结果未知”处理占用或“全部任务”查看历史与下载结果。筛选只影响任务行，注册机器及占用原因仍可见。机器名称区域保留纵向滚动；可以一键收起机器，再展开某一台查看任务。未知状态的长条不证明远端进程一直在运行。旧任务若没有 `events`，仅展示原有创建、结束时间，界面会标明其历史状态变化时间不可还原。超过 512 条状态事件的任务保留首条和最近 511 条，并报告裁剪数量。

任务可在 stdout 定期打印阶段、步数或百分比，AI 通过 `logs` 读取进度。若 `unknown` 长时间无法恢复，只有在用户或运维人员**独立核实远端进程已停止**后才能解除占用：

```sh
python3 "$TASKCTL" resolve "$TASK_ID" --confirm-remote-stopped
```

也可以直接在任务中台选中该任务，点击“重新探测远端状态”或“读取日志末尾”。探测只查询原任务，不重发命令；若确认退出码则更新状态，否则继续保留占用。确认远端进程及子进程已停止后，点击“已核实远端停止，解除占用…”并确认具体任务与机器。取消确认不会修改任务。

解除操作不发送远端终止命令，不补造退出码，也不能证明曾经成功。无法核实远端时保持 `unknown` 与机器占用。独立 `reservationId` 预约不会随任务解除而自动释放，仍由原调用方管理；页面会显示预约 ID。

页面显示全部注册机器，并分别统计在线与 ready 数量。忙碌、离线、停用、监控过期或被预约的机器仍显示在列表中，并注明不可调度原因与占用任务；例如 16 台在线、3 台 unknown 占用时，页面仍显示 16 台注册机器和 13 台 ready。调度仍只选择 ready 机器。

## 4. 回收结果

读到远端退出码后中台自动将 stdout、stderr 和 `results/` 打成 `result.tar.gz` 保存到本机配置目录的 `tasks/<任务ID>/`。`archiveReady` 与命令状态分别报告。`abandoned` 虽有本地 `finishedAt`，但没有远端退出码，不触发回收。回收失败会写入 `archiveError`；远端恢复后可安全重试：

```sh
python3 "$TASKCTL" collect "$TASK_ID"
python3 "$TASKCTL" download "$TASK_ID" --out /tmp/result.tar.gz
tar -tzf /tmp/result.tar.gz
```

结果包压缩后最多 10 GiB（10,737,418,240 字节），包含完整日志和 `results/` 文件，不再要求拆成 5 MB 小包。中台及 Skill 需同时更新到 v0.5.0-rc.3 或更新版本；旧版服务仍受原上限限制，应由维护者安排升级，客户端不能自动重启或替换中台。

SSH 回收和 HTTP 下载流式写盘，不一次性读入内存；网页交给浏览器下载管理器。回收不受原 40 秒总时限影响，SSH 连续 5 分钟无进度才超时；HTTP 下载也按每次传输刷新 5 分钟空闲时限。手动 `collect` 会等待回收完成，其间可以用另一个会话查询任务状态。客户端按 `Content-Length` 检查完整性，失败或中断时清理本次残缺文件，目标已存在时拒绝覆盖。中台回收失败时清理临时文件，远端原始结果不删除，可以重试 `collect`；下载失败可重新 `download`。日志预览仍只显示末尾 16 KiB，不影响结果包的全量日志。

`wait` 会继续等待自动归档，直到 `archiveReady` 或 `archiveError` 出现；不会在远端命令刚结束、归档仍在传输时提前退出。

请为中台归档、下载副本和解压文件预留磁盘空间。先查看归档清单，再按需解压。结果文件来自远端命令，按不可信内容处理。当前不提供自动清理、取消远端任务或资源配额控制，须由运维流程处理长期 `unknown` 任务和磁盘容量。

## 5. HTTP API

任务 API 沿用服务的本机 Host、Origin 和 `X-Watch-Token` 校验。`GET /healthz` 只读返回实例元数据，保留 Host/Origin 校验但无需会话令牌，不含凭据。新版客户端每次操作前校验身份，并发送 `X-Watch-Instance: <runId>`；若探测后发生实例替换，返回 409 且不执行请求。令牌来自首页 `<meta name="watch-token">`，随程序重启改变，不写入磁盘。AI 客户端已处理该流程。

浏览器原生下载额外支持 `POST /api/tasks/archive?id=...`，用 `application/x-www-form-urlencoded` 请求体的 `token` 字段传会话令牌（请求体最多 4 KiB），保留 Host/Origin 校验；URL 查询参数中的令牌不接受。Skill 继续使用带请求头的 GET 下载，支持 HTTP Range。

| 方法与路径 | 用途 |
| --- | --- |
| `GET /api/tasks/machines` | 全部注册机器及在线状态、ready、占用任务、预约和不可调度原因；不包含凭据 |
| `POST /api/tasks/probe?id=...` | 立即只读探测原任务；不重发、不因 PID 丢失自动解除占用 |
| `GET /api/tasks/ready` | 查看所有 ready 机器；可加 `group=...` 或 `machineId=...` 限定范围 |
| `POST /api/tasks` | 提交 `{ "id": "...", "shell": "..." }` 自动调度；可选 `group` 或 `machineId` |
| `GET /api/tasks` | 列出本机保存的任务 |
| `GET /api/tasks?id=...` | 任务状态与退出码 |
| `GET /api/tasks/logs?id=...` | 读取两路日志末尾 |
| `POST /api/tasks/collect?id=...` | 已完成任务的只读回收重试 |
| `POST /api/tasks/resolve` | 提交 `{ "id": "...", "confirm": "remote-stopped" }`，人工核实后解除 `unknown` 占用 |
| `GET /api/tasks/archive?id=...` | 下载已回收的归档 |

任务 ID 只允许 1–80 位英数字、下划线和连字符。命令最多 4096 字节。任务记录、命令、事件与结果包在本机配置目录的 `tasks/` 中以当前用户可读的明文保存；不要将该目录上传到公共仓库。SSH 密码仍留在加密配置中，不写入任务记录。每台机器只允许一个未完成中台任务，监控和旧版一次性执行仍是独立流程。

v0.5.0-rc.4 根据 issue #7 调整为全宽的机器 / 任务泳道和连续时间轴。测试夹具只用于开发验证，不编译进发布程序。

## 6. 验收边界

`go test ./...` 包含本地模拟 SSH 服务，验证后台启动、退出码、日志、归档、重复 ID 与重启后不重放。该结果证明协议链路和本地行为，不等于真实集群调度、NPU 资源空闲判断或多节点任务正确性。首次在真实集群使用时，先提交上面的无副作用示例任务，核对所选机器、日志、退出码和结果包，再运行训练或清理命令。
# AIConnector 多 Pi 预留接口（0.5 候选版）

`GET /api/tasks/reservations` 返回 `schema: simplehtmlwatch.reservations.v1` 与按 ID 索引的预留表。`POST /api/tasks/reservations` 接受 `{id, machineId}`，原子选择固定机器并持久化；同一 ID 和机器重试只返回原预留。当前不为预留提供 group/自动选择。

持有者提交 `/api/tasks` 时附带 `reservationId`，并指定相同 `machineId`。未持有预留的任务不能占用这台机器。释放通过 `POST /api/tasks/reservations/release` 和 `{id}`，存在未结束或 unknown 的关联任务时返回 409；无 TTL 自动释放。释放后的 ID 永久不能再用于启动新任务，旧客户端不能释放后来持有者的预留。

同一个 host 的不同 SSH 端口共用占用。若不同 IP/主机名实际属于一台物理机器，在机器配置里设置相同的 `resourceId`（1–80 位字母、数字、下划线或连字符）。保留 `tasks/reservations.json` 及原任务目录，重启恢复占用；不要通过删除这些文件解除任务。

预留作用于本实例 `/api/tasks` 调度入口。人工 SSH、即时批量命令 `/api/executions`、自定义监控命令、其他独立中台不会自动遵守这些预留；它们必须避开被预留的实验资源。`ready` 仍不证明 NPU 空闲。AIConnector 首版仅对固定机器入口开启并行。

浏览器回归（开发环境）：`npm ci`、`npx playwright install chromium firefox webkit`、`npm run test:browser`。它启动仅监听本机的 Go 测试夹具，使用合成任务和模拟 SSH，覆盖三种浏览器内核的下载、时间缩放、机器完整性和确认解除流程。
