# simpleHtmlWatch 全局 Prompt（Windows 优先）

> v0.8.1 升级由唯一维护者在已有授权内执行，采用不同构建前程序自动保存旧程序和核心状态。使用持久化外置归档策略，不在本机手工堆积多代副本；外置失联保留最近本机回滚并报告，不能自动降级或重启。恢复钥匙不发给AI、不放发布包；维护前阅读 `docs/backup-archive.md`，用 `-backup-status` 查看回滚和归档状态。

> 用户要求自动等待并继续实验时，发现 `experiment-continuations-v1` 后用包内 Skill 登记 waiting_resources，保存准确原对话 ID、检查点、授权和固定任务计划。桌面原对话使用宿主 heartbeat；归属已核实的 App Server 可主动桥接。登记与唤醒入口是两步，不能只登记就声称会自动继续。恢复先查原 ID，领取当前代再执行；未知请求不重放、已领取资源不超时释放。原授权范围内不重复确认，状态不变时静默，完成后核对实验判据并停用心跳。

将下面这一段复制到各 AI 工具的全局指令中，并让已有会话重新读取。它不会自动安装或修改任何 AI 工具配置。需要客户端脚本时，使用本发布包的 `skills/cluster-task-controller/scripts/taskctl.py`（Python 3）。

> 使用 simpleHtmlWatch 时，优先读取 `cluster-task-controller` Skill，先用 `python <Skill目录>/scripts/taskctl.py discover` 按实例发现并核验服务，不猜测端口。Windows 默认登记文件为 `%APPDATA%\simpleHtmlWatch\service.json`；自定义目录或实例始终使用同一组 `--data-dir` / `--instance` 参数。多个 AI 共用正式实例；连接失败、版本不符或能力缺失时，只重新发现并报告，禁止擅自杀进程、重启、换版本、改端口或删除锁文件。启停和升级由唯一维护者负责；测试版本使用独立实例及数据目录。任务提交结果不确定时，保留原任务 ID 查询，禁止换 ID 自动重发。

> 查询整机 CPU、内存、磁盘吞吐和网络流量时，读取 `simplehtmlwatch-netdata` Skill，以同一实例运行 `metricsctl.py discover`、`machines`、`summary`。明确报告未接入与部分失败，保留缺失数据；这些指标和 ready 状态不证明 NPU 空闲。远端部署或凭据修改仅在已有维护授权范围内执行。

> 查询昨天／近一周各机器 NPU 利用率、占用比例或低负载时段时，使用已更新的 `simplehtmlwatch-netdata` Skill：先 discover 核对 `accelerator-utilization-v1` 和 `accelerator-occupancy-v1`，再运行 `metricsctl.py --instance <实际实例名> utilization --period yesterday` 或 `--period week --group <机器组>`。按北京时间并列报告平均算力、整机占用时长比例及各自覆盖率；任意卡有 NPU 进程就计整机占用，按进程状态明确的观测时长加权。旧占用历史、未知及缺失不补空闲，整机均值不代表每卡空闲。查询退出码 3 时保留有效部分并说明缺失；不要据此重启中台。部署任务前另核对当前 HBM、进程和任务占用。

Web 左上角显示运行版本，悬停可查看实例和构建摘要。历史旧版没有发现接口时，由维护者核对实际启动地址；不要因 `discover` 失败而自动重启。详细规则见 [服务实例与版本管理](docs/service-lifecycle.md)。

> 选空闲NPU机器时，发现 `idle-machine-ranking-v1` 后使用 `taskctl.py --instance <实例名> idle-ranking` 或对应指标客户端，按真实任务限定组、型号、设备数和最少空闲HBM。五档排序结合最近6小时的5分钟占用桶与当前资源检查；`eligible` 不等于有历史优先级，`recommended` 才同时满足。每半小时导出的 `exports/idle-ranking.json` 仅提供候选顺序，所有响应有 `requiresLiveCheck:true`；实际领用前重新核验同一实例和实时状态，再用原预约/提交协议明确选机。历史不足、缺失和过期采样不补空闲；缺能力不自动重启或升级。默认ready和不指定机器的提交仍按原规则。
