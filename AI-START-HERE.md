# 内部 AI 测试包：从这里开始

本包已包含可执行程序、浏览器任务页面、`skills/cluster-task-controller/` 和完整 [任务 SOP](docs/task-controller.md)。运行本机程序不需要安装 Go、Node.js 或 Python；AI Skill 使用 Python 3 标准库。程序和 AI 客户端必须运行在同一台电脑上，因为服务只监听 `127.0.0.1`。

## 人工启动与页面试用

1. 选择与运行程序的电脑系统、架构相符的发布包并解压。Windows 双击 `start.bat` 或 `simpleHtmlWatch.exe`；macOS 双击 `start.command`；Linux 在文件管理器中运行 `start.sh`。保持启动窗口打开。macOS 包未签名，系统首次打开时可能要求在系统设置中允许。
2. 浏览器会自动打开本机页面。若没有自动打开，使用启动窗口“监控页面”后面的地址。点击“管理机器”，配置 SSH 地址、账号和密码；先让监控完成采样，并核对主机指纹。
3. 点击“任务中台”。不指定机器或组时由中台选择 ready 机器；也可指定机器或组。提交前确认将运行的完整命令。页面显示选中的机器、任务状态、真实事件时间泳道、日志和结果下载。
4. 第一次只发送无副作用任务，例如 `printf 'ok\n' > "$SHW_RESULTS_DIR/check.txt"`。确认退出码为 0、日志和结果包后，再测试实际工作负载。

## 交给内部 AI

先把 [简短全局 Prompt](AI-GLOBAL-PROMPT.md) 复制到 Windows 端各 AI 工具的全局指令，让已有会话重新读取。Web 左上角始终显示当前程序版本；悬停版本号可查看实例和构建摘要。

把本包的 `skills/cluster-task-controller/` 目录提供给内部 AI 的 Skill 机制；发布页也单独提供 `cluster-task-controller_*.zip`。若该 AI 不支持自动安装 Skill，就直接让它阅读 `skills/cluster-task-controller/SKILL.md`。同时给它实例名（默认 `default`）；若使用自定义数据目录，再提供该目录。说明运行程序的电脑与 AI 客户端必须是同一台。可以复制下面这段作为测试指令：

> 请先阅读 `skills/cluster-task-controller/SKILL.md`，使用 simpleHtmlWatch 本机任务中台。使用实例【default 或实际实例名】，先运行 discover 校验地址和身份，再只读查询 ready 机器；随后把准备提交的完整命令、目标范围和结果目录展示给我。得到我对这条任务的授权后提交，记录任务 ID，跟踪退出码、日志、归档状态并下载结果。没有指定机器时允许自动调度，不要把 ready 当成 NPU 空闲。若状态为 unknown，不要换 ID 重复执行。端口不可达、版本不符或能力缺失时只报告并重新发现，禁止自动重启、杀进程、删锁或切换服务版本。

Skill 的客户端脚本是 `skills/cluster-task-controller/scripts/taskctl.py`。它默认从 `service.json` 发现地址、校验在线身份，再从页面取得临时会话令牌；无需把 SSH 密码交给 AI。内部 AI 使用它时需要本机 Python 3；浏览器页面本身不需要 Python。

## 试用范围

此版本是测试发布。自动选机依据最近一次 SSH 监控和中台任务占用，不检查 NPU 显存或资源。时间泳道的事件时间是中台受理或观测时间；无事件长区间会压缩显示。当前每次任务只落一台机器，结果归档上限为 5 MiB。模拟 SSH 测试通过不代表已在真实内网集群完成端到端验收。

## 多 AI 与版本升级

正式实例由一个维护者管理启停。各 AI 使用相同实例名和数据目录；不要给每个 AI 建一份正式服务，也不要把历史端口写进守护重启脚本。测试版本使用 `-instance canary`，历史旧版使用单独 `-data-dir`；任务记录和资源预留不跨实例共享。清除历史 `SHW_URL` 后可改用自动发现。

同实例重复启动会复用现有进程。换构建需先停止原实例，再显式 `-adopt-build`；端口被占用时报冲突，不停止占用者或自动改端口。旧二进制不认识新规则，需要维护者统一旧启动入口。步骤及边界见 [服务实例与版本管理](docs/service-lifecycle.md)。
