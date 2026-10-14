# AIConnector 整组预留

能力 `atomic-group-reservations-v1`；GET `/api/tasks/reservations` 返回 `atomicGroups: 1`。原单机 API 保持兼容。机器 ID 来自同一实例的真实机器目录，2–12 台，包含固定控制机；同一物理资源及同一 SSH 入口不可重复。

本机跳板转发需要同时发现 `forwarded-resource-identity-v1`。各节点在机器管理中配置不同的 `resourceId`，同一真实节点的别名使用相同 ID。不同回环端口且物理 ID 明确不同可组成整组，即使 host 都是 `127.0.0.1`；未声明物理 ID 时继续按 host 保守去重。普通远端 host 的多端口仍不能重复计数。具体互斥规则见 [任务 SOP](task-controller.md)。

包内客户端提供固定请求入口，不自动生成替代预约 ID，也不重试写请求：

```sh
python3 skills/cluster-task-controller/scripts/taskctl.py --instance <实例名> discover
python3 skills/cluster-task-controller/scripts/taskctl.py --instance <实例名> machines
python3 skills/cluster-task-controller/scripts/taskctl.py --instance <实例名> reserve --request /absolute/path/reservation.json
python3 skills/cluster-task-controller/scripts/taskctl.py --instance <实例名> reservations --id <原预约ID>
```

请求文件是下面的结构；机器 ID 取自目录，`planSha256` 替换为固定完整控制命令 trim 后 UTF-8 的 SHA-256。`reserve` 只预留，不执行命令。双机实验优先一次整组预留，避免逐机预约留下部分占用。

```json
{"id":"fixed-reservation","machineId":"node-one","machineIds":["node-one","node-two"],"taskId":"fixed-task","planSha256":"<64位小写SHA256>"}
```

提交使用原 `--id`、固定 `--machine`、`--reservation`、返回的 `--reservation-grant` 与 `--reservation-generation`，并通过 `--command-file` 传入原完整控制命令。写回复不确定先 `reservations --id` / `status` 查询。确认所有 rank 退出、且无不确定启动意图后，原调用方执行 `release-reservation <原预约ID>`；服务仍执行占用与意图检查。

POST `/api/tasks/reservations` 增加 `machineIds`、`taskId`、`planSha256`，后者是控制机固定完整命令（trim 后 UTF-8）的 SHA-256。一个互斥区和一次原子落盘分配全部机器；任意成员不可用，返回 409，不保留部分分配。返回固定 `grantId / generation / allocations`。同 ID、同计划查询幂等；改变机器或计划冲突；释放后的 ID 不再使用。

POST `/api/tasks` 仍指定固定控制机与任务 ID，增加 `reservationGrantId / reservationGeneration`。服务检查全部成员身份、监控新鲜度、命令 SHA 和 Grant，再持久化启动意图。旧 Grant/代数或修改命令拒绝。回复丢失只查询原任务。意图已落盘而任务记录缺失时，整组保留，不允许自动释放或再次启动。

本接口证明同实例调度的排他分配，不证明其他实例或手工 SSH 没有计算，也不证明 NPU 健康、显存或业务正确性。NPU 实验仍需原始健康与资源检查。

控制机须等待全部远端 rank 并收集真实退出记录。AIConnector 回收 `group-completion.json` 后核对运行 ID、固定机器集合、每机 rank 数、每个 rank 的原始进程标识及退出码，才请求释放。缺记录、unknown 或仅控制机退出都保留占用。失败 rank 保留非零退出码，不能伪造成功。每个组成员在界面显示同一预留 ID。

代码更新不会更新正在运行的实例。维护者核对实例名、目录、进程归属、在途任务和预留后按 `service-lifecycle.md` 升级；不启动另一份正式实例，不复制凭据或删除预留。AIConnector 包内 `watch-companion` 用于这一升级。
