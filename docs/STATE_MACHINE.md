# 持久化任务状态机

`queued → checking → scanning → planning → backing_up → committing → completed`

任意运行阶段可进入 `failed / partial_failed / cancelled / interrupted`。退出码 3、扫描遗漏、分片失败、快照无法确认、目录提交不足均不允许进入 completed。提交不足使用明确错误码 CATALOG_REPLICAS_INSUFFICIENT。完整性验证另有状态，不随备份成功自动通过。

API 的 `status` 使用 `queued/running/completed/failed/partial_failed/cancelled/interrupted`；检查、扫描、规划、备份和提交等细分过程保存在 `stage`。完成证书冗余不足返回 `PROTECTION_DEGRADED`，任务为 partial_failed，点可恢复但保护状态为 degraded。

恢复：`queued → checking → restoring → verifying → completed / partial_failed / failed / cancelled / interrupted`。

维护：`queued → checking → maintaining → committing → completed / failed / interrupted`。

重启将非终态任务标记 interrupted；预留在重新查询云端占用后结算。幂等键唯一索引避免请求重试启动多个任务。计划运行时只保存一个 pending 标记；下一轮不无限累积。错过时间只补跑一次。任务记录拥有开始、结束时间和独立的扫描 Bytes、上传 Bytes、云端净增长 Bytes。

冲突矩阵：同一仓库的备份、恢复、校验、清理和迁移互斥；不同仓库受全局和账号并发限额限制。迁移先复制、验证和提交新位置，旧位置保留，不自动删除。
