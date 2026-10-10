# 内部实现接口（开发协作）

模块：`module vaultpilot`。公共存储与进程模块由主 Agent 实现；平台模块负责初始化/身份/账号/源/池；引擎模块负责计划/备份/恢复/维护；web 独立。

本文件保留开发初期的模块协作约定；完整外部接口与最终字段以 [API](API.md)、[OpenAPI](openapi.yaml) 及源码为准。

## 公共模块

`internal/store`: `Open(path string) (*Store,error)`, `Store.DB *sql.DB`。`Get(ctx,table,id,out any) error`; `List(ctx,table,limit,offset) ([]json.RawMessage,int,error)`; `All(ctx,table) ([]json.RawMessage,error)`; `Save(ctx,table,id string,expected int,value any) (int,error)`：expected=0 创建，>0 版本更新，-1 内部 upsert；写入自动添加 id/version。`Delete(ctx,table,id string) error`; `Meta(ctx,key) (string,error)`; `SetMeta(ctx,key,value) error`。实体表 accounts, storage_pools, sources, backup_plans, repositories, backup_runs, shard_runs, recovery_points, restore_jobs, maintenance_jobs, audit_events。辅助索引 file_entries(run_id,source_id,path,kind,size,mtime_ns,mode,link_target,repo_id,snapshot_path); assignments(plan_id,source_id,path,repo_id,size,mtime_ns); capacity_reservations(run_id,account_id,bytes)。明确 schema 见 store.go。

`internal/process`: `Output(ctx context.Context, bin string,args []string,env []string) ([]byte,error)`; `Run(ctx,bin,args,env,onLine func(string)) ([]byte,error)`；`ExitCode(err) int`，非零均失败，取消杀进程组。禁止 shell。

## 平台模块提供

`platform.Config`: Addr, StateDir, CacheDir, PublicURL, ResticBin, RcloneBin, AutoUnlockFile string; SourceRoots, RestoreRoots []string; AllowLocal bool; GlobalConcurrency int; OAuthClientID, OAuthClientSecret, OAuthTenant string。

部署资源新增 LogMaxRows int、CacheMaxBytes int64、MaintenanceWindow/MaintenanceTimezone string；Runtime.CheckCacheBudget() 与 AllowedMaintenance(time.Time) 提供实际资源预检查。Rclone 临时配置使用完调用 ReleaseRcloneConfig(path)，持久化刷新并删除文件。

`platform.Runtime`: Config Config, Store *store.Store, Keys *Keyring。
`platform.New(Config) (*Runtime,error)`；`Runtime.Close() error`；`Runtime.Register(*http.ServeMux)`；`Runtime.Handler(http.Handler) http.Handler` 提供会话/CSRF/静态前端 fallback（web/dist）。`Keyring.Unlocked() bool`、`Seal([]byte)([]byte,error)`、`Open([]byte)([]byte,error)`、`Lock()`。平台生成本地节点配置与 rclone 配置，`Runtime.RcloneConfig() string` 返回临时权限受控配置路径（调用前更新凭据，并合并 Rclone 刷新到加密持久化）；或实现 `Runtime.SyncRcloneCredentials() error` 在引擎调用后执行。`Runtime.VaultID() string`、`Runtime.RecoveryMode() bool`。

通用响应 JSON：列表 `{items:[],total:n}`；异常 `{error:中文说明,code:稳定码,retryable:false}`。HTTP 身份/Cookie/CSRF 由平台统一处理，GET /api/status、POST /api/setup、POST /api/auth/login、GET /api/oauth/callback 公共；其他 /api 需登录，写入需 X-CSRF-Token。GET status 给 initialized/authenticated/csrf_token/unlocked/vault_id/mode/engines/oauth_configured/source_roots/restore_roots。

账号 JSON 字段：id/version/name/backend(`onedrive`或受配置启用的`local`)/remote/drive_id/drive_name/drive_type/region/tenant_id/quota_domain/status/enabled/budget_bytes/cloud_reserve_bytes/maintenance_reserve_bytes/total_bytes/used_bytes/free_bytes/project_used_bytes/quota_at/verified/local_path。local 为真实 Restic 本地联调入口，界面仅在 allow_local=true 显示。敏感 Token 不在账号对象中。

池：id/version/name/account_ids/catalog_replicas/shard_target_bytes。默认 catalog_replicas=2，不静默降级。
源：id/version/name/path/mount_identity/excludes/include_hidden/max_file_bytes/one_file_system。

## 引擎模块提供

`engine.New(*platform.Runtime) (*Service,error)`、`Service.Register(*http.ServeMux)`、`Service.Start(ctx)`、`Service.Close()`。计划、任务、恢复点及维护 API 按 openapi 和 README 实际字段交付；自有导出结构，不需要平台 domain 类型。

计划：id/version/name/source_ids/pool_id/schedule/timezone/enabled/excludes/retention({daily,weekly,monthly})/retry_count/bandwidth_kib。
任务：id/version/kind/plan_id/status/stage/progress/started_at/finished_at/scanned_bytes/uploaded_bytes/cloud_growth_bytes/error/code/logs（日志另分页）。
恢复点：id/version/plan_id/name/started_at/finished_at/status/protected/file_count/logical_bytes/snapshots/commit_id。
GET /api/recovery-points/{id}/files?source_id=&prefix=&search=&limit=&offset=；POST /api/recovery-points/{id}/protect {protected,version}。
POST restore/preview 与 restore：point_id,source_ids,paths（逻辑 source_id/path）,target,overwrite,allow_partial。
所有任务创建返回 task 对象 202，Idempotency-Key。

## 前端

使用上述 API；按页面运行 GET status 决定初始化/登录/解锁。不使用静态假业务数据。允许空状态与引擎缺失说明。Vue 3 TS，路由总览/账号/存储池/源/计划/恢复/任务/维护。Chinese 默认。以响应数据和真实 polling/SSE 展示阶段。
