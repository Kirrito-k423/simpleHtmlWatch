# SSH 采集异常排查

## `wait: remote command exited without exit status or exit signal`

对应 [issue #1](https://github.com/Kirrito-k423/simpleHtmlWatch/issues/1)。这个错误表示 SSH 会话没有收到退出码或退出信号；它本身不能区分「远端没有发送状态」和「连接提前断开」，也不能证明命令成功或完整输出。

### v0.1.4 的处理

首先发送一个受命令超时限制的 SSH 请求：有响应则保留正文并标记「退出未确认」，无响应或传输失败则尝试重连。每轮采集最多重连一次，仅重试当前内置的只读命令；已完成的其他命令不会重跑。持续错误仍会显示，重试不会循环堆积。每次重连均经过原有认证和主机指纹校验。

「退出未确认」表示连接可用，但无法判断该命令是否成功，正文也可能不完整。如果没有正文，会显示「未获得命令输出」，而不是说没有 Python 进程。真正断连时，本轮已收到的正文仍可查看，并带有采集未完成标记。

### 如果升级后仍频繁发生

记录机器卡片上显示的错误、发生时间和具体命令。状态悬停提示中的「自动重连 1 次」表示已经进行过本轮恢复。

从同一客户端电脑，用系统 SSH 客户端分别运行以下命令，并检查退出码。这样可以区分单个命令问题和整体 SSH 连接问题；命令的 Bash 登录环境与程序保持一致。

```powershell
ssh -p 22 root@YOUR_HOST "bash -o pipefail -lc 'npu-smi info'"
$LASTEXITCODE

ssh -p 22 root@YOUR_HOST "bash -o pipefail -lc 'ps -ef'"
$LASTEXITCODE
```

如果系统 SSH 同样断开，进一步检查对应时间的 SSH 服务日志、机器重启 / OOM 记录，以及内网网关或堡垒机的会话限制。不同 Linux 发行版的日志位置不同，可由机器管理员检查 `journalctl -u sshd` 或 `journalctl -u ssh`。不要为排查直接关闭主机密钥校验或提高 SSH 权限。

现有 issue 只提供了一条错误信息，没有远端服务版本、日志或连接拓扑，因此尚不能归因到具体机器、网络策略或 NPU 驱动。

### 开发者回归验证

```sh
go test -race ./internal/watch -run TestIssue1 -count=2 -v
```

测试通过本地 SSH 服务复现「有输出但没有 exit-status」，以及空输出、失效的复用连接、命令中途断线、持续断线和真实非零退出码。原客户端在第一种情况下会显示整机离线并停止后续命令；修复后保留输出、明确表示退出状态未知，并继续采集。测试服务不代替真实故障机器上的确认。
