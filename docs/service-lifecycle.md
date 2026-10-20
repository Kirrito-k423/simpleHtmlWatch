# 服务实例与版本管理

多个 AI 可以共用一个中台，但应由一个维护者负责启停和升级。AI 客户端使用稳定实例身份连接；端口只是该实例当前地址。连接失败、版本不符、缺少能力分别报错，都不能触发自动重启。

## 默认使用

```sh
# 维护者启动，首次自动选空闲端口，以后复用
./simpleHtmlWatch -no-browser

# 只读发现，不启动、不结束任何进程
./simpleHtmlWatch -status
python3 skills/cluster-task-controller/scripts/taskctl.py discover
python3 skills/cluster-task-controller/scripts/taskctl.py ready
```

默认实例名是 `default`，数据目录沿用原来的用户配置目录。命名实例 `canary` 使用该目录下的 `instances/canary/`。服务端 `-data-dir` 与客户端 `--data-dir` 可指定任意独立目录；显式目录不再自动拼接实例名。同一目录始终使用相同实例名。

客户端也接受 `SHW_INSTANCE` / `SHW_DATA_DIR`。`--url` 或 `SHW_URL` 表示固定地址，此时不会回退其他实例；迁移到自动发现前先清除旧 `SHW_URL`。`--url` 与 `--data-dir` 不能混用。显式 URL 仍需校验身份、协议、实例名和能力；需要固定逻辑身份可加 `--service-id <discover 返回值>`。

## 启动时发生什么

1. 先取得该目录的操作系统 `app.lock`，再访问配置、任务和远端监控。
2. 锁已有持有者时，只读核对其身份。若是兼容的在线服务，则打印并复用实际 URL 和运行版本；当前启动器退出成功，不更换进程、构建或端口。并发首次启动会短暂等待登记就绪。
3. 已占锁但无法核实身份时，报错退出。可能是历史旧版、正在初始化或无响应的进程；不得删锁或根据 PID 猜测应当终止谁。
4. 取得锁后，核对上次固定的构建 SHA-256 和版本。不同构建默认拒绝启动，即使二者都叫 `dev`；显式 `-adopt-build` 才可切换。
5. 默认绑定上次的端口。被占用就报冲突，不停止占用者，也不自动换端口。首次无登记时自动选空闲端口。维护者显式 `-port <端口>` 才改变它，`-port 0` 表示重新自动分配。
6. 初始化成功后原子写入 `service.json` 并提供 `/healthz`。退出/崩溃后保留登记，所以下一次启动能复用配置；文件存在不表示在线。

`service.json` 包含稳定的 `serviceId`、实例名、每次启动变化的 `runId`、版本、构建摘要、API 版本、能力、地址、数据目录、程序路径、PID 和启动时间。不保存密码或会话令牌。客户端把登记与 `/healthz` 的身份逐项核对，不能只测试“端口能连”或“HTTP 200”。

## 多版本共存

```sh
# 两个支持实例管理的版本分别运行，默认数据与端口独立
/path/to/stable/simpleHtmlWatch -no-browser
/path/to/new/simpleHtmlWatch -instance canary -no-browser

python3 skills/cluster-task-controller/scripts/taskctl.py --instance canary discover
python3 skills/cluster-task-controller/scripts/taskctl.py --instance canary ready
```

历史旧版没有 `-instance` 时，为它显式指定独立 `-data-dir`。不要复制整个正式目录作为测试目录：会复制凭据、任务和服务身份。测试实例初始为空，需单独配置授权的测试机器。不同实例的任务占用和预留互不协调，不能靠多实例保障同一 NPU 资源互斥。

## 计划升级或迁移端口

先核对 `-status` 输出的实例、数据目录、程序路径和版本，通知使用该实例的客户端进入维护窗口，由唯一维护者正常停止准确的旧进程。然后：

```sh
# 保留原目录、逻辑身份和端口，明确选择当前构建
/path/to/new/simpleHtmlWatch -adopt-build -no-browser

# 如需同时迁移端口，明确指定；这里的 18765 只是示例
/path/to/new/simpleHtmlWatch -adopt-build -port 18765 -no-browser
```

仍在线时 `-adopt-build` / `-port` 不会抢占，只返回在线实例。显式 `-adopt-build` 也可选择旧构建，因此它是维护动作，使用前必须核查该版本对数据格式的兼容性；程序不会保证任意降级兼容。

Windows 发布包提供 `upgrade.bat`，等价于运行本包程序并显式附加 `-adopt-build`。维护者核对并关闭准确旧实例的启动窗口后，可双击它采用新构建；命名实例、自定义数据目录或其他原启动参数需继续传入。脚本不结束进程、不删除登记或锁，也不改变普通 `start.bat` 的构建固定行为。

客户端每次操作前重新读取同一实例登记，正常情况下可以刷新 URL 与会话令牌。维护窗口内请求可能失败退出；恢复后用相同实例和原任务 ID 查询。任务 API 携带 `X-Watch-Instance: <runId>`：探测与请求之间若进程已更换，请求被拒绝，不会自动重发 POST。进程重启后的任务恢复仍只查询远端状态，不重新启动任务。

需要预留功能可加 `--require-capability reservations`；需固定版本可加 `--expect-version <版本>`。不满足要求表示接口不兼容，不表示服务已宕机。

需要可选系统指标接口时可核对 `netdata-metrics-v1` 能力，具体查询格式见 [Netdata 接入说明](netdata.md)。能力广告只表示客户端 API 存在，服务器是否已安装、启用 Netdata 仍需按机器确认。

机器级 NPU 利用率小时汇总使用独立能力 `accelerator-utilization-v1`，复用服务的 NPU 监控结果，不依赖 Netdata。其存储、北京时间窗口和覆盖率见 [利用率历史说明](utilization.md)。升级前保留同一实例数据目录；旧构建没有该能力，客户端应报告不兼容，不能因缺少能力自行重启。

整机占用时长汇总广告为 `accelerator-occupancy-v1`，在同一只读接口新增占用比例及独立覆盖率。日文件升级为八字段版本 2，新版兼容读取六字段版本 1，旧占用历史保持未知；旧构建不能读取新文件。维护者升级时备份旧二进制、登记与利用率日文件，保留任务、预留、等待单和清理策略；不要直接降级运行旧格式读者。

## 历史客户端和旧二进制的迁移边界

`forwarded-resource-identity-v1` 统一本机跳板多端口的单机预约、整组预约、实验等待及机器占用显示。升级保留原物理资源映射、任务、预约和等待单；旧记录缺少物理身份时不会自动缩小其占用范围。新任务增加可选 `resourceKey`，新整组预约和等待 Grant 可以含多个回环入口；旧版会把这些记录判成重复 host，因此产生新记录后不可直接降级。保留升级前的旧构建与只读备份供恢复调查，不删除新记录来强行回退。

历史程序不会认识 `service.json`、构建固定或实例发现，新版无法让已经发布的旧二进制自动遵守这些规则。同一系统账号仍有能力杀掉自己的进程；本功能防止遵守协议的客户端误操作，不隔离拥有相同账号权限的任意命令。

迁移应统一所有 AI 的 Skill、taskctl.py 和外部启停脚本，停止“探测旧端口失败就 pkill/重启”的规则。保留旧服务时，把它的启动入口指向单独数据目录。首次迁移原正式目录，由维护者确认原持有者已正常退出，再启动新版登记；不要删除任务记录、密钥或锁文件。

确实需要连接未升级的历史服务时：

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py \
  --url http://127.0.0.1:18765 --allow-legacy ready
```

地址必须来自其启动窗口。该选项只允许身份接口返回 404 的历史模式，不绕过新版协议不兼容；没有实例身份、版本和能力保证，不允许附带 `--expect-version` 或能力要求。即使是历史模式，也不能自动重启服务。

## 验证

实验等待功能广告为 `experiment-continuations-v1`，契约见 [实验等待与原对话继续](experiment-continuations.md)。活动等待单、暂留、Grant 和事件箱保存在 `tasks/continuations.json`，升级时保留。旧构建不认识该占用，有活动记录时不得直接降级运行旧任务调度器；显式 adopt-build 仍需维护者核对数据兼容性。客户端和桥接均不能因能力缺失自动重启或替换。

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s tests -p 'test_*.py' -v
```

进程测试构建两个测试版本，使用临时空数据目录验证并发启动、单实例复用、端口保持与冲突、显式端口迁移、构建固定、多个版本共存、旧记录/假身份拒绝、会话刷新和写请求不重放。只启动并结束测试创建的进程，不访问用户 SSH 凭据或真实服务器。CI 在 macOS、Windows、Linux 上运行这些测试；交叉编译本身不等于各系统运行验收。

常驻失败残留清理广告为 `queue-residue-cleanup-v1`。启停策略和诊断/清理日志位于 `tasks/.cleanup/`，升级时保留；未完成终止意图只恢复查询，不重放信号，普通观测重启后重新计时。维护者升级后核对策略是否按原授权启用；客户端不能因为缺少能力自行升级。详见 [排队清理规则](experiment-continuations.md#常驻失败残留清理)。

## 空间预算版本

`bounded-storage-v1` 和 `task-result-stream-v1` 表示持久化元数据新增预算及按需结果下载。升级保留 `tasks/storage-budget.json`，不可删除或重置它来绕过预算。完整输出历史和自动结果缓存已移除；包内客户端需同步更新。NPU 日文件改为 `.json.gz`，旧构建不能读取，也不遵守空间预算，不可直接降级。现有结果、身份、预约和等待单仍保留，具体边界见 [空间设计](storage.md)。

`ssh-terminal-v1` 提供只驻内存的人工 SSH 终端，最多 4 个连接；服务退出会关闭终端，任务中台已启动的后台任务仍只恢复查询。终端页面通过同源 WebSocket 首帧验证会话与 runId，令牌不放在 URL 中；升级后旧页面需刷新，不自动重连或重发命令。

维护者可在正式启动参数中重复指定 `-bounded-log service-logs/app-stderr.log` 等诊断日志相对路径，最多 8 个。每 30 秒检查，单个超过 64 KiB 后原位保留末尾 32 KiB，保持已有 launchd/SSH 写入文件身份。仅在确认日志归属和允许截取诊断内容后配置，不能指定实验结果；探测失败不构成启用或改写日志参数的授权。详见 [空间设计](storage.md)。
