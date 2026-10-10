# HTTP API

基地址 `/api`，同源 JSON。列表 `{items: [...], total: n}` 支持 `limit`（最大500）/`offset`。错误 `{error: "中文说明", code: "稳定错误码", retryable: false}`。网页登录后所有写请求携带 `X-CSRF-Token`；登录响应或 `/status` 提供该 token。Cookie 为 HttpOnly，前端不存登录密码或库密钥。

修改实体携带当前 `version`，旧版本应拒绝。创建后台任务使用 `Idempotency-Key`，返回 HTTP 202 及 Task；浏览器关闭不取消。任务结果从 `/tasks/{id}` 重新读取，任务、恢复点和日志均可分页。

## 初始化与安全

| Method | Path | 请求/响应 |
|---|---|---|
| GET | /status | initialized, authenticated, csrf_token（已登录）, unlocked, vault_id, mode, engines, oauth_configured, source_roots, restore_roots, allow_local；已登录另返回 cache_max_bytes, log_max_rows, maintenance_window, maintenance_timezone |
| POST | /setup | username, password, recovery_password, mode=create/recovery；recovery 模式加 recovery_kit 对象或完整JSON字符串 |
| POST | /auth/login | username, password → csrf_token, unlocked |
| POST | /auth/logout | 结束当前会话 |
| POST | /security/unlock | password（当前库恢复密码） |
| POST | /security/lock | 清除内存主密钥 |
| POST | /security/recovery-kit | password（新包保护密码） → 加密恢复包JSON，不含OAuth Token |
| POST | /security/discover | {} → 发现/重建任务；导入节点需先重新授权正确Drive |

登录密码和恢复密码分别至少12字符且必须不同。初次导入恢复包使用保护该包的密码。恢复包导出可使用不同密码，但不隐式更改当前库重启解锁密码。

## 账号与存储池

`GET /metrics/network` 要求登录，返回 `available`、`supported`、`server_time` 和最近 60 秒的 `history`。每条采样包含 `at`、`upload_bytes_per_second`（出站）和 `download_bytes_per_second`（入站）。服务每秒读取 Linux `/proc/net/dev`，排除回环接口；Docker 默认桥接网络下为容器网络流量，原生或 host 网络部署下为节点全部非回环接口流量，包含其他程序流量，不代表单个备份任务的文件处理速度。计数器重置、接口变化或采样中断会重新建立基线；不支持的系统返回不可用，界面不生成模拟数据。历史仅保留在内存中，服务重启后重新采集。

| Method | Path | 说明 |
|---|---|---|
| GET/POST | /accounts | 列表/创建：name, backend=onedrive, budget_bytes, cloud_reserve_bytes, maintenance_reserve_bytes |
| PUT/PATCH | /accounts/{id} | name, version, enabled, budget_bytes, cloud_reserve_bytes, maintenance_reserve_bytes |
| DELETE | /accounts/{id} | 拒绝仍被仓库/位置/池引用的账号；先停用新分配 |
| POST | /accounts/{id}/authorize | {} → authorization_url；PKCE和会话绑定state |
| GET | /oauth/callback | 微软回调，检验后跳转网页Drive选择阶段 |
| GET | /accounts/{id}/drives | Drive列表，返回id/name/drive_type/supported/quota；不默认选择 |
| POST | /accounts/{id}/verify | version, drive_id, 可选预算；根访问及专用目录读写验证 |
| POST | /accounts/{id}/quota | 刷新容量和项目实际占用 |
| GET/POST | /pools | name, account_ids, catalog_replicas（默认2）, shard_target_bytes（默认100GiB） |
| PUT/PATCH/DELETE | /pools/{id} | 修改必须version，删除检查计划依赖 |

Account 输出有 Drive 稳定身份、配额域、verified/status/enabled、整数Bytes和quota_at；没有 Token、节点密钥或完整 Rclone config。`local` backend + local_path 仅在显式启用 VP_ALLOW_LOCAL 时提供，用于真实本地引擎验收。

## 目录与计划

| Method | Path | 说明 |
|---|---|---|
| GET/POST | /sources | name, path, excludes[], include_hidden, max_file_bytes, one_file_system；服务生成source_id及mount_identity |
| PUT/PATCH/DELETE | /sources/{id} | 修改携带version，路径限于SourceRoots，删除检查计划 |
| GET | /browse?path=… | 部署授权目录浏览；未指定path列根目录，不跟随链接 |
| GET/POST | /plans | name, source_ids, pool_id, schedule, timezone, enabled, excludes, retention, retry_count, bandwidth_kib |
| PUT/DELETE | /plans/{id} | 更新/删除；运行中计划不能删除 |
| POST | /plans/{id}/preview | {} → 异步扫描/分配预览；不上传数据 |
| POST | /plans/{id}/run | {} → 异步备份；同一计划重叠触发合并 |

schedule 为5字段 Cron 或固定间隔 `@every 6h`；timezone 为 IANA 名称，默认 Asia/Shanghai，计划返回 next_runs 三次执行时间。retention 为 `{daily, weekly, monthly}`。配置限制和估算不替代真实存储配额对账。

## 任务、恢复和维护

| Method | Path | 说明 |
|---|---|---|
| GET | /tasks | 分页Task集合 |
| GET | /tasks/{id} | 任务kind/status/stage/progress/时间/统计/结果/错误 |
| GET | /tasks/{id}/logs | 分页 `{seq,line,at}` |
| POST | /tasks/{id}/cancel | 请求取消整个引擎进程树 |
| GET | /events | SSE `tasks` 事件；断线后重新读任务 |
| GET | /repositories | 仓库登记（不返回password） |
| GET | /recovery-points | 完整历史恢复点，附完整快照引用和commit_id |
| GET | /recovery-points/{id}/files | source_id/prefix/search/limit/offset；返回该点独立版本的目录视图 |
| POST | /recovery-points/{id}/protect | protected, version → 更新保护状态并提交目录 |
| POST | /restore/preview | point_id, source_ids[], paths[], target, overwrite, allow_partial → 空间、冲突及缺失范围 |
| POST | /restore | 相同请求 → 异步恢复任务 |
| POST | /maintenance/check | repository_ids[]（省略表示全部仓库）, mode（structure/sample/full）, sample（抽查比例，例如10%） → 检查任务 |
| POST | /maintenance/retention/preview | plan_id → 全局保留与删除集合及阻塞项 |
| POST | /maintenance/retention | plan_id, point_ids[], confirm=true → 重新验证后清理 |
| POST | /maintenance/migrate | repository_id, target_account_id, confirm=true → 整仓复制、验证、位置提交；保留源。旧副本清理使用 repository_id, cleanup_old=true, old_location_index, old_location_path, confirm=true |

paths 元素为 `{source_id, path}`，路径是该源的相对路径；省略范围恢复整个点。target 必须为授权RestoreRoots之内的绝对路径。默认overwrite=false、allow_partial=false。覆盖和清理需要网页风险确认，API不接受任意引擎命令。

Task 分别提供 scanned_bytes、uploaded_bytes、cloud_growth_bytes；备份完成不是全量完整性读取检查已通过。`result` 随任务种类变化。恢复开始执行后，恢复报告提供 `restored`、`skipped`、`conflicts`、`failed`、`unattempted`，以及 `target`、`point_id` 和 `verification`；预检查即拒绝的任务可能尚无恢复报告。已复制部分条目后发生错误，任务标记 `partial_failed` 并保留实际错误的稳定 `code`；不会统一改成未知错误或完整成功。

恢复报告的 `verification_status` 为 `passed`、`not_started`、`partial`、`unknown` 或 `failed`。`verification_errors` 是已识别的校验异常数；Restic 错误无法区分发生在恢复还是校验阶段时返回 `null`，同时标记 `unknown`，不能将其显示为零异常。`failure_entries` 列出 `{source_id,path,code,stage,error?}`；`failure_entry_count` 给出完整异常条目数量，`failure_entries_truncated` 明确列表是否被截断。部分账号不可用时另行报告跳过及缺失范围。

`old_location_index` 来自仓库 `locations` 数组，建议同时提供读取到的 `old_location_path`，路径变化会拒绝操作。清理旧副本会重新验证当前仓库及所有有效快照引用，先提交旧位置退休登记再删除；不会清理当前仓库位置。完成证书冗余未满足时，任务返回 `PROTECTION_DEGRADED`，恢复点 `protection_status=degraded`；该点可恢复，但不能当作达到副本要求的完整成功。

## Rclone 标准授权与配置导入

以下接口均要求管理员会话，写请求要求 `X-CSRF-Token`。默认标准授权无需全局 OAuth 配置。配置与回调地址仅通过请求体提交；不应写入访问日志。

| 方法 | 路径 | 参数与行为 |
|---|---|---|
| POST | /accounts/{id}/rclone/authorize | `{version}`，启动实际 Rclone 授权，202 返回会话 `id` |
| GET | /accounts/{id}/rclone/authorize?flow_id=… | 查询状态、授权 URL、过期时间；仅原登录会话可读取，完成时返回更新的 Account |
| DELETE | /accounts/{id}/rclone/authorize?flow_id=… | 取消子进程；等待状态 cancelled 后可重新开始 |
| POST | /accounts/{id}/rclone/callback?flow_id=… | `{url}`，校验固定 localhost 回调、state、有效期和一次使用状态，202 表示已提交，继续轮询 |
| POST | /rclone/import/preview | `{config}`，返回 remote 名称、Drive 参考信息及是否自定义应用；不返回任何凭据 |
| POST | /accounts/{id}/rclone/import | `{config,remote,version}`，显式选择 remote，加密保存并返回待选择 Drive 的 Account |

标准授权状态：starting → waiting → exchanging（粘贴回调时）→ completed / failed；取消经 cancelling → cancelled。有效期 10 分钟，同一实例仅一个活动授权。重启使未完成会话失效，临时配置在完成、取消和重启时删除。已保存凭据跨重启保留。

导入和授权不会把 `drive_id` 参考信息直接登记为已验证身份，仍需调用 `/drives` 与 `/verify`。已有账号必须选原 Drive；其他 Drive 应创建新账号。重新授权同时原子保存账号状态与凭据；冲突时不覆盖已有凭据。旧引擎配置仅能同步同一授权版本的刷新结果。
