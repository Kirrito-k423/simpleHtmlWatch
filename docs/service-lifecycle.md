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

客户端每次操作前重新读取同一实例登记，正常情况下可以刷新 URL 与会话令牌。维护窗口内请求可能失败退出；恢复后用相同实例和原任务 ID 查询。任务 API 携带 `X-Watch-Instance: <runId>`：探测与请求之间若进程已更换，请求被拒绝，不会自动重发 POST。进程重启后的任务恢复仍只查询远端状态，不重新启动任务。

需要预留功能可加 `--require-capability reservations`；需固定版本可加 `--expect-version <版本>`。不满足要求表示接口不兼容，不表示服务已宕机。

## 历史客户端和旧二进制的迁移边界

历史程序不会认识 `service.json`、构建固定或实例发现，新版无法让已经发布的旧二进制自动遵守这些规则。同一系统账号仍有能力杀掉自己的进程；本功能防止遵守协议的客户端误操作，不隔离拥有相同账号权限的任意命令。

迁移应统一所有 AI 的 Skill、taskctl.py 和外部启停脚本，停止“探测旧端口失败就 pkill/重启”的规则。保留旧服务时，把它的启动入口指向单独数据目录。首次迁移原正式目录，由维护者确认原持有者已正常退出，再启动新版登记；不要删除任务记录、密钥或锁文件。

确实需要连接未升级的历史服务时：

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py \
  --url http://127.0.0.1:18765 --allow-legacy ready
```

地址必须来自其启动窗口。该选项只允许身份接口返回 404 的历史模式，不绕过新版协议不兼容；没有实例身份、版本和能力保证，不允许附带 `--expect-version` 或能力要求。即使是历史模式，也不能自动重启服务。

## 验证

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s tests -p 'test_*.py' -v
```

进程测试构建两个测试版本，使用临时空数据目录验证并发启动、单实例复用、端口保持与冲突、显式端口迁移、构建固定、多个版本共存、旧记录/假身份拒绝、会话刷新和写请求不重放。只启动并结束测试创建的进程，不访问用户 SSH 凭据或真实服务器。CI 在 macOS、Windows、Linux 上运行这些测试；交叉编译本身不等于各系统运行验收。
