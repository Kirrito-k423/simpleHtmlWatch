# AIConnector 整组预留

能力 `atomic-group-reservations-v1`；GET `/api/tasks/reservations` 返回 `atomicGroups: 1`。原单机 API 保持兼容。机器 ID 来自同一实例的真实机器目录，2–12 台，包含固定控制机；物理 host/resourceId 不可重复。

POST `/api/tasks/reservations` 增加 `machineIds`、`taskId`、`planSha256`，后者是控制机固定完整命令（trim 后 UTF-8）的 SHA-256。一个互斥区和一次原子落盘分配全部机器；任意成员不可用，返回 409，不保留部分分配。返回固定 `grantId / generation / allocations`。同 ID、同计划查询幂等；改变机器或计划冲突；释放后的 ID 不再使用。

POST `/api/tasks` 仍指定固定控制机与任务 ID，增加 `reservationGrantId / reservationGeneration`。服务检查全部成员身份、监控新鲜度、命令 SHA 和 Grant，再持久化启动意图。旧 Grant/代数或修改命令拒绝。回复丢失只查询原任务。意图已落盘而任务记录缺失时，整组保留，不允许自动释放或再次启动。

本接口证明同实例调度的排他分配，不证明其他实例或手工 SSH 没有计算，也不证明 NPU 健康、显存或业务正确性。NPU 实验仍需原始健康与资源检查。

控制机须等待全部远端 rank 并收集真实退出记录。AIConnector 回收 `group-completion.json` 后核对运行 ID、固定机器集合、每机 rank 数、每个 rank 的原始进程标识及退出码，才请求释放。缺记录、unknown 或仅控制机退出都保留占用。失败 rank 保留非零退出码，不能伪造成功。每个组成员在界面显示同一预留 ID。

代码更新不会更新正在运行的实例。维护者核对实例名、目录、进程归属、在途任务和预留后按 `service-lifecycle.md` 升级；不启动另一份正式实例，不复制凭据或删除预留。AIConnector 包内 `watch-companion` 用于这一升级。
