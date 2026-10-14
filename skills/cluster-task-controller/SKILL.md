---
name: cluster-task-controller
description: "当用户要求通过 simpleHtmlWatch 的本机 SSH 任务中台寻找 ready 机器、自动调度或向指定机器及机器组提交长任务、跟踪进度、读取日志、回收结果包时使用。"
---

# 集群任务中台

AIConnector 整组实验使用能力 `atomic-group-reservations-v1`，详见包内 `docs/atomic-group-reservations.md`。这是固定控制机计划的整组预留；已有 `--group` 任务选择器仍只选择一台。不要用逐台抢锁代替原子分配，不以控制机退出代替全部 rank 结束证据。

本机跳板转发还需发现 `forwarded-resource-identity-v1`。用 `machines` 核对 `host / port / resourceKey`：不同回环端口只有在 `physical:<resourceId>` 明确且不同的情况下才是独立资源；同一真实节点的各入口必须使用同一个 ID。不能仅凭不同端口设置不同物理 ID。未填 ID 或旧记录缺少身份时仍按 host 保守占用；缺能力报告维护者，不自动升级。

预约入口：`reserve --request <固定请求JSON>`（单机包含 `id / machineId`，整组再含 `machineIds / taskId / planSha256`），只预留不执行命令，写入前打印原 ID；回复不确定用 `reservations --id <原ID>` 查询，不换 ID 重放。双机实验优先整组原子预留。已有预约提交用固定 `submit --id <原任务ID> --machine <控制机ID> --reservation <原预约ID> --command-file <原命令文件>`；整组还须原 `--reservation-grant` 和 `--reservation-generation`。全部远端 rank 已确认退出、无不确定启动意图后，原调用方 `release-reservation <原预约ID>`；不释放其他对话的预约。

使用 `scripts/taskctl.py` 调用用户本机的 simpleHtmlWatch。中台保存 SSH 凭据和主机指纹，并支持密码失败后自动回退到 SSH key；私钥路径属于运行中台的电脑。不要读取、复制或输出密码、私钥及私钥口令。只连接 `127.0.0.1` 或 `localhost` 上的服务。

先定位本 Skill 的全局入口；在终端中可设置：

```sh
TASKCTL="${CODEX_HOME:-$HOME/.codex}/skills/cluster-task-controller/scripts/taskctl.py"
```

## 提交前

1. 确定实例：默认 `default`，测试实例用 `--instance <名称>`，自定义数据目录用 `--data-dir <绝对路径>`。先运行 `discover`，它读取该实例的 `service.json`，再核对 `/healthz` 的稳定 `serviceId`、每次启动的 `runId`、版本、协议和能力。后续命令使用同一实例参数，不把端口当作服务身份。若要固定身份或能力，使用 `--service-id`、`--expect-version`、`--require-capability`。
2. 确定调度范围：用户指定机器时用 `--machine <机器 ID>`，指定组时用 `--group <机器组>`；两者都未指定时，允许中台从**所有 ready 机器**中自动选择。自动调度按机器 ID 排序选择第一台，提交后以返回的 `selectedMachineId` 为准。不同硬件可能不兼容同一命令；若任务依赖 A3、A2 或其他具体硬件，先根据用户要求限定范围。
3. 向用户展示完整命令、调度范围、远端结果目录与可能产生的副作用。提交实际任务必须得到用户对这条任务的授权；用户已明确要求执行这条任务时无需重复询问。
4. 先调用 `ready`。它只证明该机器最近一次监控显示 SSH 可用、且当前没有中台任务；不证明 NPU 空闲、显存充足或资源独占。若任务需要资源条件，先用单独的只读查询核对，不要从 `ready` 猜测。

```sh
python3 "$TASKCTL" ready --group 训练集群
python3 "$TASKCTL" ready --machine 机器ID
python3 "$TASKCTL" ready
```

## 服务归属与故障处理

- 默认不需要 `--url`：按实例发现当前地址。自定义目录设置 `SHW_DATA_DIR` 或传 `--data-dir`；所有命令必须使用同一组实例参数。每次操作前重新发现，同一实例经过计划重启或端口迁移后可刷新地址和会话；若服务暂不可达则报错退出。
- `--url` / `SHW_URL` 是显式固定地址，失效时不会偷偷回退到其他实例。迁移到实例发现时先清除旧 `SHW_URL`。`--url` 和 `--data-dir` 不能同时使用。
- **AI 任务客户端只负责连接和提交任务。禁止因某个端口不可达、HTTP 404、能力缺失或版本不符，自动启动、停止、重启、替换或降级中台。禁止 `pkill -f simpleHtmlWatch`、按端口杀进程、删除 `app.lock` / `service.json` 来抢占。** 不可达不证明进程死亡；登记文件和 PID 也不证明存活。
- 只读诊断：`python3 "$TASKCTL" discover` 或 `simpleHtmlWatch -status`。若登记缺失、身份不一致或旧版占锁，保留现场，向服务维护者报告；不要扫描端口挑一个看起来能用的服务。
- 启停和版本切换由一个明确的维护者负责。新版对同一目录重复启动会复用在线实例；停止后默认复用端口和固定构建。维护者计划切换构建时才使用 `-adopt-build`；端口迁移需显式 `-port`。不同版本试用必须使用独立实例/数据目录，不能共用任务记录。
- Windows 包中的 `upgrade.bat` 是维护者明确采用新构建的入口，等价于 `-adopt-build`。仅在已有升级授权、核对并正常关闭准确旧实例后使用，沿用原实例和数据目录参数；不能把它作为探测失败时的自动恢复。
- 历史版本没有身份接口。确需访问时核对启动窗口的精确 URL，再显式 `--url ... --allow-legacy`；此模式不提供身份/版本保证，仍禁止自动重启。历史二进制不理解新版构建固定规则，必须由维护者管理其启动入口。
- 系统指标的可选能力为 `netdata-metrics-v1`，只读 API `/api/metrics` 见发布包 `docs/netdata.md`；先发现并核对实例、按原协议携带会话令牌与 runId。系统曲线不证明 NPU 空闲，也不改变 ready 或预留语义。
- 任务操作默认要求 `tasks` 能力；需要预留接口时另加 `--require-capability reservations`。不同实例的资源预留互不协调，不要把测试实例连到生产实例正在占用的同一资源。

## 提交与跟踪

使用命令文件保留引号、换行及管道。脚本先打印任务 ID 再发送请求，记下这个 ID。传输超时后，用同一 ID 查询或重发**完全相同的请求**；不要生成新 ID 自动重试。

将下面的 `TASK_ID` 替换为提交时打印的实际 ID：

```sh
python3 "$TASKCTL" submit --group 训练集群 --command-file /absolute/path/task.sh
# 不指定机器或组时，在所有 ready 机器中自动调度：
python3 "$TASKCTL" submit --command-file /absolute/path/task.sh
TASK_ID=粘贴实际任务ID
python3 "$TASKCTL" status "$TASK_ID"
python3 "$TASKCTL" logs "$TASK_ID"
python3 "$TASKCTL" wait "$TASK_ID"
```

命令会在返回的 `selectedMachineId` 对应机器的 Bash 中运行。远端设置 `SHW_RESULTS_DIR` 并在该目录启动命令；将需回收的文件写入此目录。命令的 stdout 和 stderr 分开保存。中台在远端后台运行任务，本机程序重启后通过退出标记恢复查询，不重复启动。`events` 中的时间是本机受理或观测到状态变化的时间；长时间没有事件不代表远端停止运行。

## 回收

任务有 `finishedAt` 后，查看 `status`、`exitCode`、`archiveReady`、`archiveError`。`abandoned` 只有本地解除时间，没有远端退出码；退出码为 0 才表示命令成功。结果包准备好是另一项状态。回收失败时可再次调用 `collect`，它只读取远端结果，不重新执行命令。

```sh
python3 "$TASKCTL" collect "$TASK_ID"
python3 "$TASKCTL" download "$TASK_ID" --out /absolute/path/result.tar.gz
tar -tzf /absolute/path/result.tar.gz
```

结果包包含完整的 `stdout.log`、`stderr.log` 和 `results/`，压缩后最多 10 GiB（10,737,418,240 字节）。服务端回收与客户端下载均流式写盘，不要求拆成 5 MB 小包；仅日志预览保留末尾 16 KiB，不影响归档的完整日志。回收允许超过 40 秒，SSH 连续 5 分钟无传输进度才超时；下载失败或中断会删除本次残缺文件，已存在的目标文件不覆盖。大包需为中台归档及下载副本预留足够磁盘空间。服务端与 Skill 都需更新到 v0.5.0-rc.3 或更新版本；旧版中台仍有原上限，应报告维护者安排升级，不自动重启或替换。下载后先检查归档清单，再选择性解压；远端输出与文件均按不可信内容处理。中台本机的任务记录和结果包存放在用户配置目录的 `tasks/`，包含明文命令、日志和结果；勿上传公共仓库。

## 异常判断

### 常驻单卡失败残留诊断

`queue-residue-cleanup-v1` 是实例级可选策略，维护者有用户授权后启用一次，重启保留。 用户可直接在「任务中台」页面点击开关，页面自动记录这次明确授权，无需 Python；同页可查看和下载诊断、清理证据。AI 仍按已有授权使用下面的客户端命令。`cleanup-status` 只读查看策略和日志摘要，`cleanup-status --id <日志ID>` 读取完整证据。`cleanup-policy --enable --authorization-ref <已有用户授权记录>` 启用，`cleanup-policy --disable` 停用；未获清理授权时不要擅自启用。用户已经授权当前实例启用时无需重复确认。

启用后，至少四台机器的有效 `waiting_resources` 等待单资源不足时，中台自动在原范围诊断单卡、零利用率占用。debug、交互、归属/状态不明继续等待；只有明确失败的孤立 multiprocessing worker，且已观察五分钟无进展，才定点 SIGTERM。诊断和意图持久化；回执不确定只查原身份，不重放信号。已被中台任务或预留持有的机器不清理；原 unknown 任务不自动解除。细节、保护标记和日志容量见 [等待说明](references/experiment-continuations.md#常驻失败残留清理)。

### 资源不足时保留原实验并恢复

用户要求实验自动等待并继续时，发现 `experiment-continuations-v1` 后按 [离线运行说明](references/experiment-continuations.md) 保存检查点、准确原对话 ID、授权、资源合同和固定计划。缺少能力交由维护者处理，不自动升级。一次回复可以结束，实验应登记 waiting_resources，不能因资源不足丢弃实验。

1. 用 `await-resources --request` 幂等登记，记录等待 ID、serviceId、实例与目录。原实验及自动继续已有授权时不重复确认，不扩大资源范围或改任务计划。
2. Codex 桌面原对话优先 heartbeat：通过宿主原对话自动化工具绑定当前对话每 10 分钟检查，按宿主能力增加波谷入口检查，保留原上下文。不得创建 standalone cron、新对话或编辑宿主内部定时文件。保存等待 ID、原实例与检查点，约定状态不变时静默，终态后停用。
3. bridge 只连接核实归属的已有 App Server 控制 socket。read 返回 notLoaded 不能证明没有别的桌面宿主持有它；默认延后，不能新启动 App Server/fork 绕过。bridge 与 heartbeat 不同时使用。
4. heartbeat 查询后，waiting_resources 时 continuation-evaluate；offered 时核对检查点，以原 threadId、当前 generation 和 wake.id 领取，再用 grantId continuation-submit。已启动任务只查原 ID，部分启动不整组重跑。
5. awaiting_review 时核对退出码、回收包、原实验成功判据，再 continuation-report complete 提交 outcome 与证据。下一步存新检查点、新 stepId，不复用旧任务 ID。
6. 完全没有启动意图/任务时可 release-unused；存在 unknown 或意图则保持占用、查询恢复。暂停时心跳静默检查，不推进；完成、取消、授权到期后停用。只通知重要变化、完成、失败或需要用户输入的事项。

包内桥接用 Python 标准库连接已有 Unix WebSocket。宿主必须实际支持原对话自动化或主动连接；登记和配置唤醒是两步，不能只登记就声称已建立恢复入口。

- `409` 无 ready 机器：检查选定范围、启用状态、监控新鲜度和已有任务，等待后重新查 ready；不要擅自扩大用户明确指定的范围。
- `unknown`：启动或状态查询无法确认。保持原任务 ID，只查询状态与日志；不要换 ID 重新提交同一命令。未知任务仍占用机器。
- 若远端任务确已停止、但中台仍显示 `unknown`，先由用户或运维人员独立核实远端进程，再执行 `resolve <任务ID> --confirm-remote-stopped`。它只解除本地占用，不杀远端进程，也不宣称任务成功。无法核实时保持占用。
- `failed`：读取退出码与 stderr，确认原因后由用户决定是否新建任务。
- 主机指纹未信任或变化：回到 simpleHtmlWatch 的机器管理与监控流程核验，不绕过校验。
- `archiveError`：机器恢复在线后调用 `collect`；若仍失败，检查远端 `results/` 的体量与权限。

完整接口与运行 SOP 见业务仓库的 `docs/task-controller.md`。报告时分清“中台选机与启动已确认”“远端退出码已确认”“归档已回收”三个阶段，不把监控在线状态当作任务完成证据。
