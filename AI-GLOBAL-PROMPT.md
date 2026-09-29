# simpleHtmlWatch 全局 Prompt（Windows 优先）

将下面这一段复制到各 AI 工具的全局指令中，并让已有会话重新读取。它不会自动安装或修改任何 AI 工具配置。需要客户端脚本时，使用本发布包的 `skills/cluster-task-controller/scripts/taskctl.py`（Python 3）。

> 使用 simpleHtmlWatch 时，优先读取 `cluster-task-controller` Skill，先用 `python <Skill目录>/scripts/taskctl.py discover` 按实例发现并核验服务，不猜测端口。Windows 默认登记文件为 `%APPDATA%\simpleHtmlWatch\service.json`；自定义目录或实例始终使用同一组 `--data-dir` / `--instance` 参数。多个 AI 共用正式实例；连接失败、版本不符或能力缺失时，只重新发现并报告，禁止擅自杀进程、重启、换版本、改端口或删除锁文件。启停和升级由唯一维护者负责；测试版本使用独立实例及数据目录。任务提交结果不确定时，保留原任务 ID 查询，禁止换 ID 自动重发。

Web 左上角显示运行版本，悬停可查看实例和构建摘要。历史旧版没有发现接口时，由维护者核对实际启动地址；不要因 `discover` 失败而自动重启。详细规则见 [服务实例与版本管理](docs/service-lifecycle.md)。
