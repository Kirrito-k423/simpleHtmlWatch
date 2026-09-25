# SSH 任务中台标准 SOP

此功能在本机 simpleHtmlWatch 中增加持久化的长任务流程。AI 和脚本通过本机 HTTP API 提交任务，中台选择一台 ready 机器，用既有 SSH 凭据和主机指纹发送命令，随后查询远端退出标记并回收结果。一次请求只调度一台机器；需要多机任务时分别提交并记录各自任务 ID。

## 1. 构建与启动

开发环境需要 Go 1.26 或更新版本；运行页面时间轴检查脚本还需要 Node.js。远端需要 Linux、Bash、`base64`、`nohup`、`tail` 和 `tar`，不需要安装 Agent。

```sh
go test ./...
node scripts/test-task-timeline.cjs
go build -o simpleHtmlWatch .
./simpleHtmlWatch -port 8765 -no-browser
```

预期控制台显示 `监控页面：http://127.0.0.1:8765` 和本机配置目录。保持程序运行。任务中台继续复用“管理机器”中的配置、密码和已信任主机指纹；先让目标机器完成一次监控采样。

AI 客户端在独立的 `agent-skills/cluster-task-controller/scripts/taskctl.py`，只依赖 Python 标准库。它先读取本机首页的会话令牌，再访问任务 API，不需要保存 SSH 密码。已安装 Skill 时可直接使用全局入口：

```sh
TASKCTL="${CODEX_HOME:-$HOME/.codex}/skills/cluster-task-controller/scripts/taskctl.py"
python3 "$TASKCTL" --url http://127.0.0.1:8765 ready --group 训练集群
```

浏览器打开 `http://127.0.0.1:8765/?tasks=1`，或从首页点“任务中台”，可在同一页发送任务、查看任务时间泳道、读取日志和下载结果包。界面继承本机会话令牌；它操作真实任务，不是原型演示。

## 2. 选机器与提交

请求可以不指定选择器，此时在所有 ready 机器中自动选机。`machineId` 精确选机；`group` 限定机器组；后两者不能同时指定。所有模式都按机器 ID 顺序选择第一台符合条件的机器，实际落点以提交响应的 `selectedMachineId` 为准。自动模式可能选到不同硬件；依赖具体型号的命令应指定机器或组。ready 的条件是：已启用、最近监控状态为 `online` 或 `partial`、状态未超过 `2 × 监控间隔 + 30 秒`，并且中台没有在同一机器或同一 SSH 地址上保留未完成任务。这里不解析 `npu-smi` 的显存或占用情况，不能据此声称 NPU 空闲。

先准备待运行命令。中台在远端设置 `SHW_RESULTS_DIR` 并从该目录启动 Bash，任务应把需回收的文件写入这个目录：

```sh
cat > /tmp/example-task.sh <<'SH'
printf '开始运行\n'
printf '示例结果\n' > "$SHW_RESULTS_DIR/answer.txt"
SH

python3 "$TASKCTL" --url http://127.0.0.1:8765 submit \
  --group 训练集群 --command-file /tmp/example-task.sh

# 若此命令适用于所有 ready 机器，可省略机器和组：
python3 "$TASKCTL" --url http://127.0.0.1:8765 submit \
  --command-file /tmp/example-task.sh
```

脚本在发送请求前打印任务 ID；保存这个 ID。提交成功返回 `202` 和 `selectedMachineId`，并进入 `dispatching`。相同 ID 与完全相同的命令、选择器再次提交，只返回原任务；同一 ID 换命令或选择器返回 `409`。提交超时或断线时，先按原 ID 查状态；需要重发时传 `--id <原ID>` 且保持请求内容完全相同。不要生成新 ID 自动重试。

## 3. 进度、退出与日志

```sh
TASK_ID=粘贴提交输出中的实际任务ID
python3 "$TASKCTL" --url http://127.0.0.1:8765 status "$TASK_ID"
python3 "$TASKCTL" --url http://127.0.0.1:8765 logs "$TASK_ID"
python3 "$TASKCTL" --url http://127.0.0.1:8765 wait "$TASK_ID"
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

每条新任务的 `events` 持久记录受理、启动确认、状态变为未知、退出码确认和归档结果。时间戳是**本机受理或观测到状态变化的时间**；5 秒轮询可能晚于远端实际退出。浏览器的时间泳道以这些真实时间排序，把相邻 1 分钟内的事件叠在一列，把超过 2 分钟的无状态事件区间压缩为窄列并标出实际时长。横向间距不是线性时间比例，长时间无事件不表示远端空闲。旧任务若没有 `events`，仅展示原有创建、结束时间，界面会标明其历史状态变化时间不可还原。超过 512 条状态事件的任务保留首条和最近 511 条，并报告裁剪数量。

任务可在 stdout 定期打印阶段、步数或百分比，AI 通过 `logs` 读取进度。若 `unknown` 长时间无法恢复，只有在用户或运维人员**独立核实远端进程已停止**后才能解除占用：

```sh
python3 "$TASKCTL" --url http://127.0.0.1:8765 resolve "$TASK_ID" --confirm-remote-stopped
```

此操作不发送远端终止命令，不补造退出码，也不能证明曾经成功。无法核实远端时保持 `unknown` 与机器占用。

## 4. 回收结果

读到远端退出码后中台自动将 stdout、stderr 和 `results/` 打成 `result.tar.gz` 保存到本机配置目录的 `tasks/<任务ID>/`。`archiveReady` 与命令状态分别报告。`abandoned` 虽有本地 `finishedAt`，但没有远端退出码，不触发回收。回收失败会写入 `archiveError`；远端恢复后可安全重试：

```sh
python3 "$TASKCTL" --url http://127.0.0.1:8765 collect "$TASK_ID"
python3 "$TASKCTL" --url http://127.0.0.1:8765 download "$TASK_ID" --out /tmp/result.tar.gz
tar -tzf /tmp/result.tar.gz
```

结果包压缩后最多 5 MiB，下载目标已存在时脚本拒绝覆盖。先查看归档清单，再按需解压。结果文件来自远端命令，按不可信内容处理。当前不提供自动清理、取消远端任务或资源配额控制，须由运维流程处理长期 `unknown` 任务和磁盘容量。

## 5. HTTP API

所有 API 沿用服务的本机 Host、Origin 和 `X-Watch-Token` 校验。令牌来自首页 `<meta name="watch-token">`，随程序重启改变，不写入磁盘。AI 客户端已处理该流程。

| 方法与路径 | 用途 |
| --- | --- |
| `GET /api/tasks/ready` | 查看所有 ready 机器；可加 `group=...` 或 `machineId=...` 限定范围 |
| `POST /api/tasks` | 提交 `{ "id": "...", "shell": "..." }` 自动调度；可选 `group` 或 `machineId` |
| `GET /api/tasks` | 列出本机保存的任务 |
| `GET /api/tasks?id=...` | 任务状态与退出码 |
| `GET /api/tasks/logs?id=...` | 读取两路日志末尾 |
| `POST /api/tasks/collect?id=...` | 已完成任务的只读回收重试 |
| `POST /api/tasks/resolve` | 提交 `{ "id": "...", "confirm": "remote-stopped" }`，人工核实后解除 `unknown` 占用 |
| `GET /api/tasks/archive?id=...` | 下载已回收的归档 |

任务 ID 只允许 1–80 位英数字、下划线和连字符。命令最多 4096 字节。任务记录、命令、事件与结果包在本机配置目录的 `tasks/` 中以当前用户可读的明文保存；不要将该目录上传到公共仓库。SSH 密码仍留在加密配置中，不写入任务记录。每台机器只允许一个未完成中台任务，监控和旧版一次性执行仍是独立流程。

页面设计来自隔离原型分支 `codex/prototype-task-visualization` 的方案 C；验证后的选择是阶段泳道，加上真实事件时间轴与无事件区间压缩。原型中的虚构任务和模拟按钮没有进入正式页面。

## 6. 验收边界

`go test ./...` 包含本地模拟 SSH 服务，验证后台启动、退出码、日志、归档、重复 ID 与重启后不重放。该结果证明协议链路和本地行为，不等于真实集群调度、NPU 资源空闲判断或多节点任务正确性。首次在真实集群使用时，先提交上面的无副作用示例任务，核对所选机器、日志、退出码和结果包，再运行训练或清理命令。
