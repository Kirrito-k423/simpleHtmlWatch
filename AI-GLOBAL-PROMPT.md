# simpleHtmlWatch 全局 Prompt（Windows 优先）

将下面这一段复制到各 AI 工具的全局指令中，并让已有会话重新读取。它不会自动安装或修改任何 AI 工具配置。需要客户端脚本时，使用本发布包的 `skills/cluster-task-controller/scripts/taskctl.py`（Python 3）。

> 使用 simpleHtmlWatch 时，优先读取 `cluster-task-controller` Skill，先用 `python <Skill目录>/scripts/taskctl.py discover` 按实例发现并核验服务，不猜测端口。Windows 默认登记文件为 `%APPDATA%\simpleHtmlWatch\service.json`；自定义目录或实例始终使用同一组 `--data-dir` / `--instance` 参数。多个 AI 共用正式实例；连接失败、版本不符或能力缺失时，只重新发现并报告，禁止擅自杀进程、重启、换版本、改端口或删除锁文件。启停和升级由唯一维护者负责；测试版本使用独立实例及数据目录。任务提交结果不确定时，保留原任务 ID 查询，禁止换 ID 自动重发。

> 查询整机 CPU、内存、磁盘吞吐和网络流量时，读取 `simplehtmlwatch-netdata` Skill，以同一实例运行 `metricsctl.py discover`、`machines`、`summary`。明确报告未接入与部分失败，保留缺失数据；这些指标和 ready 状态不证明 NPU 空闲。远端部署或凭据修改仅在已有维护授权范围内执行。

> 查询昨天／近一周各机器 NPU 利用率或低负载时段时，使用已更新的 `simplehtmlwatch-netdata` Skill：先 discover 核对 `accelerator-utilization-v1`，再运行 `metricsctl.py --instance <实际实例名> utilization --period yesterday` 或 `--period week --group <机器组>`。按北京时间报告小时均值、峰值、低负载占比和覆盖率；缺失不补零，整机均值不代表每卡空闲。查询退出码 3 时保留有效部分并说明缺失；不要据此重启中台。部署任务前另核对当前 HBM、进程和任务占用。

Web 左上角显示运行版本，悬停可查看实例和构建摘要。历史旧版没有发现接口时，由维护者核对实际启动地址；不要因 `discover` 失败而自动重启。详细规则见 [服务实例与版本管理](docs/service-lifecycle.md)。
