# 五档空闲排序：外部 AI 协议

先按实例 discover，要求 `idle-machine-ranking-v1`，查询均为 GET 且携带会话令牌和当前 runId。两个 Skill 的 `idle-ranking` 命令都支持 `--group`、`--model`、`--devices-per-machine`（1–128）、`--min-free-hbm-mb`（0–1099511627776）和 `--max-sample-age`（1–300秒）。示例资源数必须按实际任务修改。

接口：`GET /api/idle-ranking?group=...&model=...&devicesPerMachine=...&minFreeHbmMB=...&maxSampleAgeSeconds=...`。默认资源条件为所有设备无进程、算力为0、每设备至少1024 MB空闲HBM，样本年龄不超过 `min(120,2×监控间隔+30)` 秒，机器在线启用且无中台任务/预约/等待单等占用；型号和设备数须由任务补充。

- `schema` 必须为 `simplehtmlwatch.idle-ranking.v1`，`rankingPolicyVersion` 为 `five-tier-v1`。
- 以 `instance`、`serviceId`、`runId` 绑定来源，勿把端口当逻辑身份。
- `generatedAt` 为本次资源检查时间；`historyThrough` 为最后完整5分钟边界；`exportedAt` 是自动文件保存时间；`nextRefreshAt` 是北京时间下一整点/半点。
- `machines` 包含 `machineId`、`resourceKey`、`group`、`model`、`tier`、`tierLabel`、`historyReason`、`eligible`、`recommended`、`currentReason`、`rank`、`resourceCheckedAt`、`lastNpuSampleAt`、`devices`、`minFreeHbmMB` 和 `windows`。
- `windows` 的键是 `5m/1h/2h/3h/4h`；每个窗口有 `after/before`、`occupiedPercent`（未知为null）、`observedSeconds`、`occupiedSeconds`、`coveragePercent`、`minimumHourlyCoveragePercent`、`complete`。比例按明确进程状态时长加权；相关每小时覆盖≥90%才授予对应档位。
- 档位依次为最近≥3h/2h/1h观测零占用、最近1h非零且<15%、最近1h为15%–25%。未分档为0，理由保留；≥4h不无限加分。同档按机器ID稳定排序。
- `eligible` 只表示当前资源通过，`recommended` 还要求有效历史档位；只有后者有正 `rank`。历史不足或当前未知不能补空闲。
- `requiresLiveCheck:true` 始终存在，文件和查询都不是预约或执行许可。用同一实例、任务真实硬件条件重新查询，按 `resourceKey` 和原资源冲突协议核对物理机器，通过原预约接口预留并明确选机提交；不释放其他调用方的预约。

自动文件是 `<discover.dataDir>/exports/idle-ranking.json`，启动及北京时间整点/半点原子覆盖，最多128 KiB。细粒度历史保留最近6小时，压缩上限64 KiB；升级前小时桶不拆分补回，因此初期历史不足是正常状态。文件过期、身份不符、采样过期或 `exportError/historyStorageError` 非空时，先处理具体问题，不能凭旧文件直接部署。读取成功但无候选返回0；保存/导出异常返回3；参数/身份/请求失败返回2。禁止据此重启、杀进程、切换构建或重放任务。

排序查询不提交任务、不预约、不触发清理；不指定机器的旧任务提交仍按ID选择ready，不能声称它自动使用五档排序。历史延续比例不等于未来独占或任务成功保证。
