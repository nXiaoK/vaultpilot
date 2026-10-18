# VaultPilot 修复与验证报告（2026-10-18）

审计范围：`4029295` → `d7b6bd6`（冻结源码）。可在当前环境执行的验收已通过；Docker 镜像构建与运行、真实 OneDrive 仍未验证。全部代码按独立功能提交，未推送。

已按真实父子提交差异提取 41 个非合并功能/补修/测试提交、19 个合并提交、107 条新增测试声明。参数化模板与表驱动子项按声明记录，不能当作运行用例总数。完整提交、67个计划子项及新增测试证据已写入 [逐项文件与测试证据](evidence/fix-plan-validation.json)。

## 明确保留的边界

- T16.5.3：baseline 已先检查账户删除成功再清凭据，原实现已满足要求，核对后保留。
- App.vue 组件拆分按计划暂不执行；基础镜像 digest 可选，未启用。
- 旧版本已经 deleted、但没有 pending_forget 的空间泄漏未自动回填。
- 真实 OneDrive、Docker、生产规模与多节点仍按实际环境标为未验证；本地真实引擎及假脚本不能替代这些边界。

## 共享能力影响

`internal/ctxlock` 被 engine 的 catalog/configuration/keyed 资源锁/恢复索引与 Runtime 账号配置门禁复用。任务和受影响 HTTP 入口显式使用调用方 context；部分资源获取失败按逆序释放，锁等待被取消后不继续启动目录提交。Runtime 的旧 Lock/Unlock 保留，普通账号编辑、验证等旧调用仍兼容阻塞，不能写成“全应用所有请求等待均可取消”。

恢复读锁允许 restore 与列表、预览并发；等待写者取得准入后，新读者可能排队。discover、账号清空仍长持 catalog；目录认证上传和读回也在锁内，不能宣称所有跨计划等待均已消除。T15 不改数据库结构、请求 JSON、角色、业务限额或审批规则。

maintenance 在数据操作与发布阶段之间释放资源并重取；迁移/旧副本映射发布的非降级认证失败用精确版本 CAS 回滚，PROTECTION_DEGRADED 是已逻辑提交，不能回滚。T05 认证删除决策后的本地 tombstone/pending 仍使用有界 WithoutCancel 原子登记；最终仅移除已处理 pending，保留并发新增项。T14 live point、活动 task、partial_failed 的已发布点与 discover 正在暂存/发布及仍被有效恢复点引用的 import 清单受保护，每批最多 5000 行；取消时机会清扫可以留待启动补扫。

详细锁不变量与阶段交接参见 [LOCKING.md](LOCKING.md)；索引 v3、SQLite NORMAL 及升级备份边界参见 [MIGRATIONS.md](MIGRATIONS.md)。NORMAL 保证一致性，不保证断电最近事务零丢失。

## 性能

同 Go 1.27.1、原索引条件下：OFFSET `14.637802833s` → 键集 `698.913541ms`。最终 Go 1.26.9 / v3 索引样本：20 万行遍历 `280.204792ms`；百万行插入 `4.957s`，读取 100 行 `85.292µs`，数据库 `257,961,984` 字节，Go heap `4,262,528` 字节（不是进程 RSS）。不同工具链与索引样本不能直接当作单变量对照。生产构建仍为 Go 1.26.9；这是本地合成数据，不代表真实云端耗时。

## 最终验收

| 检查 | 结果 |
|---|---|
| Go vet / build / staticcheck / govulncheck | 通过；未发现可达漏洞，仍报告 4 项未调用模块漏洞。另完成 Linux amd64/arm64 三个后端程序交叉编译 |
| 整仓 race -count=5 | 通过，真实 Restic 0.18.1 / Rclone 1.70.3 已启用；总耗时 727.220s |
| 无缓存 -count=1 / 真实本地 Restic、Rclone | 通过，总耗时 144.236s；扩展备份/清理/迁移/全新节点恢复与离线救援也通过 |
| npm ci / build / test / 浏览器 | Node 22.23.3，构建及 44 项测试通过；隔离浏览器自动焦点、Esc 回焦和实际加密恢复包下载通过 |
| diff --check / OpenAPI 等静态检查 | 通过；Compose config、YAML/本地 schema 引用、Python 脚本和 JSON 解析通过 |
| Docker / 真实 OneDrive / 生产规模或多节点 | 未验证：本机 Docker daemon 未运行，无真实微软账号/云盘；未执行百万物理文件、5 TB 上传、多写节点及云端并发耗时验收 |

## 复现命令与证据

工具链：Go 1.26.9、Node 22.23.3、官方 Restic 0.18.1、官方 Rclone 1.70.3。真实引擎开关在两轮全仓测试中均已设置。`-count=1` 禁用 Go 测试结果缓存，连续五轮另用 `-count=5`。

```sh
GOTOOLCHAIN=go1.26.9 go vet ./...
GOTOOLCHAIN=go1.26.9 go run honnef.co/go/tools/cmd/staticcheck@latest ./...
GOTOOLCHAIN=go1.26.9 go run golang.org/x/vuln/cmd/govulncheck@latest ./...
VP_TEST_RESTIC=/path/to/restic VP_TEST_RCLONE=/path/to/rclone GOTOOLCHAIN=go1.26.9 go test -json -race -count=5 -timeout=30m ./...
VP_TEST_RESTIC=/path/to/restic VP_TEST_RCLONE=/path/to/rclone GOTOOLCHAIN=go1.26.9 go test -json -race -count=1 -timeout=15m ./...
GOTOOLCHAIN=go1.26.9 go build -buildvcs=false -trimpath -o bin/vaultpilot ./cmd/vaultpilot
GOTOOLCHAIN=go1.26.9 go build -buildvcs=false -trimpath -o bin/vprescue ./cmd/vprescue
npm ci --prefix web && npm run build --prefix web && npm test --prefix web
VP_SCALE_TEST=1 GOTOOLCHAIN=go1.26.9 go test ./internal/store -run '^TestMillionEntryIndex$' -count=1 -v
VP_ENTRY_SCALE_TEST=1 GOTOOLCHAIN=go1.26.9 go test ./internal/engine -run '^TestEntryTraversalScale$' -count=1 -v
python3 scripts/e2e_local.py --extended --restic /path/to/restic --rclone /path/to/rclone
python3 scripts/e2e_rescue.py test-artifacts/e2e-<该轮目录> --restic /path/to/restic
docker compose --env-file .env.example config --quiet
git diff --check
```

本机父目录同时存在 SVN 元数据，普通 `go build` 的 VCS 自动识别会失败；本轮构建使用 `-buildvcs=false`（统一验收通过 `GOFLAGS` 传入），没有改项目默认 Makefile。原环境没有 rtk，本次已获授权使用原生命令。下载的引擎均核对官方 SHA-256。

[完整命令、环境、返回码与耗时](evidence/validation-summary.json)；[Go 分包结果及实际测试名称](evidence/go-validation.json)；[前端与浏览器验收](evidence/frontend-validation.json)；[本地闭环](evidence/local-e2e-report.json)；[离线救援](evidence/offline-rescue-report.json)。

## 共享能力复用明细

| 能力 | 复用范围 | 默认/成功路径 | 失败/取消路径 | 兼容边界 |
|---|---|---|---|---|
| process.BaseEnvironment / Run | restic、rclone、平台授权及credential obscure | 允许基础路径/locale/证书/代理，显式env保持优先 | 超长行持续drain并报错；ctx优先于进程/扫描错误 | 宿主RESTIC_*/RCLONE_*/VP_*不继承，自定义白名单外环境需显式适配 |
| Runtime.RcloneConfigFor | 备份/恢复/检查/迁移/retention/目录/云连接/账号清空 | 仅显式账号，local跳过，Enabled仍由业务控制 | ctx传到续期，当前账号或同步失败明确返回，任务临时配置释放 | 无关坏账号不拖垮任务，纯quota版本变化允许，物理配置变化仍拒绝 |
| 认证、限流、decode、deriveKey | login/setup/unlock/cloud_secret/transfer/reauth | 代理默认不信任，action/IP分桶；普通decode默认16MiB保留 | 密码入口8KiB/setup1MiB，Argon2两槽排队，成功复位独立桶 | admin_password是敏感导出明确新增契约，非所有API默认收紧 |
| eachEntry / 页写入 / allocate | 规划分配、manifest、快照验证、恢复布局 | 内部游标，HTTP offset兼容，页事务 | 页错误回滚、回调/ctx停止；最终短预留事务批量校验防超售 | 空键/可变where受测；索引v3首次建表成本与state备份见MIGRATIONS |
| entry_cleanup | Start、backup/preview终态、retain删除点、snapshot验证 | 仅无live引用/无活动task可删，每批最多5000；import暂存/发布受索引保护 | NULL-safe活动保护；partial_failed已发布点保留；取消机会清扫留启动重试 | 不改assignments/预留，共享RunID保留，不承诺所有孤儿import永久保留 |
| ctxlock / Runtime账号Context门禁 | engine catalog/config/keyed/index与Runtime账号门禁 | 零值可用，旧Lock/Unlock保留；平台普通旧调用继续阻塞 | 显式ctx获取失败归还部分锁/token，锁等待取消不开始提交，禁止R→W升级 | 写者准入后新读者排队；discover/clear/目录认证仍可长占catalog |
| maintenance阶段/CAS/pending批移除 | migrate、cleanupOld、retain与最终认证catalog | 计划锁全程，数据IO与元数据发布分离 | 迁移/旧副本映射非降级失败精确版本回滚，逻辑降级不回滚；T05认证后WithoutCancel原子登记 | 请求JSON/DB结构/角色/业务限额不变；旧deleted无pending不自动回填 |
| web api / downloadBlob / polling | 两类导出下载、共享API活动计数、App/NetworkActivity轮询 | 成功null保留、下载文件名保留、可见页周期保留 | 错误null包装、延迟revoke、隐藏停止/卸载清监听、后台连续3次失败提醒 | 浏览器stub不能替代Safari/Firefox真实下载、真实云端和多节点验收 |

## 计划子项、文件与新增测试矩阵

以下67项按冻结源码真实feature提交关联；文件和新增测试来自父子差异，无新增声明的项明确列明。

### T01

**T01.1 Go生产构建版本** - 已实现; 提交: `3a4dfb9`

- 文件: `Dockerfile`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 后端镜像固定Go 1.26.9，go.mod语言版本保持1.26.0。
- 兼容/风险: 标准库安全修复；本机性能工具链不能替代生产构建版本；Docker运行待验收。

**T01.2 版本文字与历史证据** - 已实现; 提交: `a79b138`

- 文件: `README.md`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 开发要求同步为Go1.26.9+；历史证据不在构建版本修复中改写。
- 兼容/风险: 最终无缓存证据另由T16.8更新。

### T02

**T02.1 Host白名单与配置** - 已实现; 提交: `26255c9`

- 文件: `.env.example`, `README.md`, `cmd/vaultpilot/main.go`, `compose.yaml`, `docs/CONFIGURATION.md`, `docs/SECURITY.md`, `docs/openapi.yaml`, `internal/platform/auth.go`, `internal/platform/auth_test.go`, `internal/platform/runtime.go`
- 真实新增测试: `internal/platform/auth_test.go::TestHostAllowlistBlocksRebinding` (26255c9)
- 行为: 覆盖API和静态入口，loopback/PublicURL/AllowedHosts显式允许，未知Host返回421。
- 兼容/风险: LAN/反代保留Host需正确配置允许地址；loopback健康检查继续允许。

**T02.2 未登录status与测试入口** - 已实现; 提交: `26255c9`

- 文件: `internal/engine/account_clear_test.go`, `internal/engine/cloud_connect_test.go`, `internal/platform/auth.go`, `internal/platform/auth_test.go`, `internal/platform/notifications_test.go`, `internal/platform/platform_test.go`
- 真实新增测试: `internal/platform/auth_test.go::TestStatusHidesConfigurationWhenUnauthenticated` (26255c9)
- 行为: 未登录只返回登录/初始化需要的状态，登录后重读完整状态；harness显式localhost。
- 兼容/风险: 未认证客户端不再获取库/源/恢复目录等配置字段。

### T03

**T03.1 按任务账号构建Rclone配置** - 已实现; 提交: `d92fba0`

- 文件: `internal/platform/rclone.go`, `internal/platform/rclone_scope_test.go`
- 真实新增测试: `internal/platform/rclone_scope_test.go::TestRcloneConfigForIsScopedAndIsolatesBrokenAccounts` (d92fba0); `internal/platform/rclone_scope_test.go::TestRcloneConfigForAllowsQuotaUpdatesButRejectsRemoteChanges` (d92fba0); `internal/platform/rclone_scope_test.go::TestRcloneConfigForPropagatesSyncFailure` (d92fba0); `internal/platform/rclone_scope_test.go::TestRcloneConfigForCancelsTokenRefresh` (d92fba0)
- 行为: 显式IDs去重、local跳过、ctx传到续期、错误带账号原因；0600临时配置按任务释放。
- 兼容/风险: Enabled仍由业务决定；不把纯quota版本变化当物理变化；必需账号或同步失败仍拒绝。

**T03.2 引擎调用显式账号与ctx** - 已实现; 提交: `d92fba0`

- 文件: `internal/engine/account_clear.go`, `internal/engine/backup.go`, `internal/engine/catalog.go`, `internal/engine/cloud_connect.go`, `internal/engine/maintenance.go`, `internal/engine/rclone_scope_test.go`
- 真实新增测试: `internal/engine/rclone_scope_test.go::TestRcloneHelpersIgnoreUnrelatedBrokenAccount` (d92fba0)
- 行为: backup/restore/check/目录/cloud连接/清空传实际账号，迁移copy同时传源与目标。
- 兼容/风险: 无关损坏账号不拖垮当前任务；错误向上保留，不输出token/secret。

### T04

**T04 超长输出持续排空** - 已实现; 提交: `a673666`

- 文件: `internal/process/process.go`, `internal/process/process_test.go`
- 真实新增测试: `internal/process/process_test.go::TestRunDrainsOversizedLines` (a673666)
- 行为: 超过4MiB行后持续drain，记录扫描失败，不把截断JSON当成功。
- 兼容/风险: 错误优先级仍为ctx、进程退出、扫描错误；不向onLine传截断片段。

### T05

**T05.1 删除决策前预检** - 已实现; 提交: `198b02a`, `5e4cfa9`

- 文件: `internal/engine/engine_test.go`, `internal/engine/maintenance.go`, `internal/engine/retention_state.go`
- 真实新增测试: `internal/engine/engine_test.go::TestRetentionPrecheckFailsBeforeCommit` (198b02a)
- 行为: 认证删除前核对plan/pool读取、账号Verified/Enabled和维护预留，prune前重读可用性。
- 兼容/风险: 预检失败尚未发布/标删除；不因缺预留留下不可重试删除。

**T05.2 持久pending与原子登记** - 已实现; 提交: `198b02a`, `5e4cfa9`

- 文件: `docs/RECOVERY.md`, `internal/engine/catalog.go`, `internal/engine/engine_test.go`, `internal/engine/maintenance.go`, `internal/engine/model.go`, `internal/engine/retention_state.go`, `internal/engine/retention_state_test.go`
- 真实新增测试: `internal/engine/engine_test.go::TestRetentionRegistrationIsAtomic` (198b02a); `internal/engine/retention_state_test.go::TestMaintenancePendingRemovalIsAtomicAndPreservesJSON` (5e4cfa9)
- 行为: 新增omitempty pending_forget随认证目录保存；逻辑删除后有界WithoutCancel事务登记tombstone/pending，最终增量移除已处理ID。
- 兼容/风险: PROTECTION_DEGRADED已逻辑提交仍登记；旧已deleted且无pending的空间泄漏未自动回填。

**T05.3 prune重试与引用保护** - 已实现; 提交: `198b02a`, `5e4cfa9`

- 文件: `internal/engine/engine_test.go`, `internal/engine/integration_test.go`, `internal/engine/maintenance.go`, `internal/engine/maintenance_concurrency_test.go`, `internal/engine/retention_state.go`
- 真实新增测试: `internal/engine/engine_test.go::TestRetentionRetrySelectionAndReferenceProtection` (198b02a); `internal/engine/integration_test.go::TestRealResticRetentionPruneRetriesAfterFailure` (198b02a); `internal/engine/maintenance_concurrency_test.go::TestMaintenancePruneFailureKeepsDecisionForRetry` (5e4cfa9); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceRetentionDoesNotForgetSharedSnapshot` (5e4cfa9)
- 行为: 仅forget现存且无live引用ID；空point_ids可重试，forget已完成仍prune；最终目录/本地pending共同收敛。
- 兼容/风险: prune、最终发布失败或取消均保留重试意图；不吞并发新增pending/共享快照。

**T05.4 前端重试与契约文档** - 已实现; 提交: `198b02a`

- 文件: `docs/RECOVERY.md`, `docs/openapi.yaml`, `web/src/App.vue`, `web/src/ResultReport.vue`, `web/tests/maintenance.test.mjs`
- 真实新增测试: `web/tests/maintenance.test.mjs::retry cleanup asks for confirmation and submits only an empty selection for the chosen plan` (198b02a)
- 行为: 展示pending仓库数，确认后提交所选计划的point_ids:[]，展示cleanup_pending。
- 兼容/风险: 不重新提交已删除ID，不绕过确认或原预检。

### T06

**T06.1 迁移目标必须在当前计划池** - 已实现; 提交: `4e83484`, `ad9de2c`

- 文件: `docs/LIMITATIONS.md`, `docs/LOCKING.md`, `docs/openapi.yaml`, `internal/engine/backup.go`, `internal/engine/engine_test.go`, `internal/engine/maintenance.go`, `internal/engine/migration_state.go`
- 真实新增测试: `internal/engine/engine_test.go::TestMigrateRejectsTargetOutsidePlanPool` (4e83484)
- 行为: 网络操作前核对repo→plan→pool；最终CAS事务重验当前池，历史归属错误给出加回池提示。
- 兼容/风险: 不自动改池成员；复制期池/目标变化拒绝新映射。

**T06.2 迁移目标UI筛选** - 已实现; 提交: `4e83484`

- 文件: `web/src/App.vue`, `web/tests/maintenance.test.mjs`
- 真实新增测试: `web/tests/maintenance.test.mjs::migration targets belong to the selected repository plan pool and remain enabled and verified` (4e83484)
- 行为: computed仅列出所属计划池内启用/验证且非当前账号，缺候选提示先加入池。
- 兼容/风险: UI不能替代后端检查；缺计划/池时保守空列表。

### T07

**T07.1 宿主环境统一白名单** - 已实现; 提交: `c46880e`

- 文件: `docs/SECURITY.md`, `internal/engine/restic_rclone_test.go`, `internal/platform/rclone_credentials.go`, `internal/process/process.go`, `internal/process/process_test.go`
- 真实新增测试: `internal/process/process_test.go::TestProcessEnvironmentIsAllowlisted` (c46880e)
- 行为: BaseEnvironment复用到引擎/平台Rclone，允许基础环境/locale/CA/代理；显式env仍优先。
- 兼容/风险: 不继承宿主RESTIC_*/RCLONE_*/VP_*；依赖白名单外环境的部署需显式适配。

**T07.2 obscure旁路补修** - 已实现; 提交: `39fde3b`

- 文件: `internal/platform/rclone_auth_test.go`, `internal/platform/rclone_credentials.go`
- 真实新增测试: `internal/platform/rclone_auth_test.go::TestRcloneObscureFiltersHostEnvironment` (39fde3b)
- 行为: credential obscure子进程同样使用隔离环境。
- 兼容/风险: 补齐非通用process入口，不改变凭据明文/加密协议。

### T08

**T08 容器PID1 tini** - 已实现; 提交: `46a4037`

- 文件: `Dockerfile`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 最终镜像安装tini并以tini -- vaultpilot启动，compose不叠加第二层init。
- 兼容/风险: PID1/信号转发/僵尸回收/30秒关闭需真实Docker运行验证。

### T09

**T09 下载共享与延迟revoke** - 已实现; 提交: `61c7460`

- 文件: `web/src/AccountTransfer.vue`, `web/src/App.vue`, `web/src/api.ts`, `web/tests/api.test.mjs`
- 真实新增测试: `web/tests/api.test.mjs::downloads attach the link and revoke its URL only after the timer fires` (61c7460)
- 行为: downloadBlob挂载/点击/移除链接后延迟1秒释放URL，两类敏感包共用。
- 兼容/风险: 保留文件名；Node stub不等于Safari/Firefox真实下载验收。

### T10

**T10.1 取消条件PATCH与执行器合并** - 已实现; 提交: `73dd4df`

- 文件: `internal/engine/model.go`, `internal/engine/service.go`, `internal/engine/task_cancel_test.go`
- 真实新增测试: `internal/engine/task_cancel_test.go::TestCancelDoesNotOverwriteTerminalStatus` (73dd4df); `internal/engine/task_cancel_test.go::TestCancelFlagSurvivesExecutorSave` (73dd4df)
- 行为: 只对非终态写cancel_requested及版本；save在mu内合并取消，镜像表由执行器继续同步。
- 兼容/风险: 已完成/失败等终态不回退running，ctx终态规则保持。

**T10.2 execute提前失败释放槽位** - 已实现; 提交: `eedccac`

- 文件: `internal/engine/execution_slot_test.go`, `internal/engine/service.go`
- 真实新增测试: `internal/engine/execution_slot_test.go::TestExecuteLoadFailureReleasesActivePlan` (eedccac); `internal/engine/execution_slot_test.go::TestExecuteLoadFailureMarksExistingRowFailed` (eedccac); `internal/engine/execution_slot_test.go::TestExecuteLoadFailurePreservesNewerActiveSlot` (eedccac)
- 行为: 所有返回清对应active；加载失败日志与条件failed持久化，不删后来替换的新槽位。
- 兼容/风险: 保留正常pending重放与重复触发合并。

**T10.3 旧任务迟到取消保留新pending** - 已实现; 提交: `d31a720`

- 文件: `internal/engine/service.go`, `internal/engine/task_cancel_test.go`
- 真实新增测试: `internal/engine/task_cancel_test.go::TestCancelOldTaskPreservesNewTaskPending` (d31a720)
- 行为: 仅active[plan]==task.ID清pending/meta，保护同计划新B的触发。
- 兼容/风险: 当前任务取消仍清pending；旧A终态/结果不动。

### T11

**T11.1 配额乐观合并** - 已实现; 提交: `910014f`

- 文件: `internal/engine/backup.go`, `internal/engine/quota_refresh_test.go`
- 真实新增测试: `internal/engine/quota_refresh_test.go::TestRefreshAccountDoesNotClobberConcurrentEdit` (910014f); `internal/engine/quota_refresh_test.go::TestAccountQuotaConflictsAreBounded` (910014f); `internal/engine/quota_refresh_test.go::TestRefreshAccountCancelledLocalDoesNotWrite` (910014f)
- 行为: 重读最新版本，只写5个配额字段，冲突最多3次重试。
- 兼容/风险: 仍递增版本，前端旧编辑可能409；不额外加不可重入账号锁。

**T11.2 收尾刷新生命周期** - 已实现; 提交: `910014f`

- 文件: `internal/engine/backup.go`, `internal/engine/maintenance.go`, `internal/engine/quota_refresh_test.go`
- 真实新增测试: `internal/engine/quota_refresh_test.go::TestPostTaskQuotaRefreshStopsOnShutdown` (910014f)
- 行为: 收尾刷新从s.ctx派生2分钟上限，关机取消，失败仅日志。
- 兼容/风险: 不覆盖原任务结果；刷新失败可保留预留避免低估占用。

### T12

**T12.1 可信代理及分操作/IP限流** - 已实现; 提交: `a6b6652`

- 文件: `.env.example`, `README.md`, `cmd/vaultpilot/main.go`, `compose.yaml`, `docs/CONFIGURATION.md`, `docs/SECURITY.md`, `internal/engine/cloud_connect.go`, `internal/platform/account_transfer.go`, `internal/platform/auth.go`, `internal/platform/auth_test.go`, `internal/platform/cloud_connect.go`, `internal/platform/platform_test.go`, `internal/platform/runtime.go`
- 真实新增测试: `internal/platform/auth_test.go::TestLoginSuccessResetsThrottle` (a6b6652); `internal/platform/auth_test.go::TestThrottleIsPerAction` (a6b6652); `internal/platform/auth_test.go::TestTrustedProxyUsesForwardedFor` (a6b6652)
- 行为: 新增TrustedProxies CIDR并从右解析可信XFF；action/IP分桶，成功清桶，维持15分钟10次。
- 兼容/风险: 默认不信转发头；非法CIDR启动失败，反代未配仍共用代理IP桶。

**T12.2 Argon2全局并发** - 已实现; 提交: `41bb938`

- 文件: `internal/platform/account_transfer.go`, `internal/platform/auth_test.go`, `internal/platform/crypto.go`
- 真实新增测试: `internal/platform/auth_test.go::TestDeriveKeyWaitsForGlobalSlot` (41bb938)
- 行为: 所有Argon2派生经过2槽信号量。
- 兼容/风险: KDF参数/密文格式不变；排队并不等于密码请求即时完成。

**T12.3 密码接口体积限制与文档** - 已实现; 提交: `41bb938`

- 文件: `docs/CONFIGURATION.md`, `docs/SECURITY.md`, `docs/openapi.yaml`, `internal/engine/cloud_connect.go`, `internal/engine/cloud_connect_test.go`, `internal/platform/account_transfer.go`, `internal/platform/auth.go`, `internal/platform/auth_test.go`, `internal/platform/runtime.go`, `web/src/App.vue`
- 真实新增测试: `internal/engine/cloud_connect_test.go::TestCloudSecretRejectsOversizedBodies` (41bb938); `internal/platform/auth_test.go::TestLoginRejectsOversizedBody` (41bb938); `internal/platform/auth_test.go::TestSecretRequestLimitsAndEmptyLogout` (41bb938); `internal/platform/auth_test.go::TestSetupRequestLimit` (41bb938)
- 行为: decode默认16MiB保留；密码入口8KiB，setup恢复包1MiB，cloud secret拒多JSON对象。
- 兼容/风险: 非密码API不默认收紧；账号包本体保持相应解析路径。

### T13

**T13.1 内部键集/统一遍历** - 已实现; 提交: `273d5d8`

- 文件: `internal/engine/backup.go`, `internal/engine/entries_scale_test.go`, `internal/engine/entries_test.go`, `internal/engine/restore.go`, `internal/engine/service.go`
- 真实新增测试: `internal/engine/entries_scale_test.go::TestEntryTraversalScale` (273d5d8); `internal/engine/entries_test.go::TestEachEntryPreservesEmptyKeysAndMutableFilters` (273d5d8); `internal/engine/entries_test.go::TestEachEntryStopsOnErrorAndCancellation` (273d5d8)
- 行为: eachEntry/entriesAfter按(source_id,path)游标，覆盖空键/可变过滤/取消和错误。
- 兼容/风险: HTTP浏览保留offset；不擅改外部JSON分页契约。

**T13.2 每页写事务** - 已实现; 提交: `8d93f61`

- 文件: `internal/engine/allocation_paging_test.go`, `internal/engine/backup.go`
- 真实新增测试: `internal/engine/allocation_paging_test.go::TestAllocationPreservesAssignmentsAcrossPages` (8d93f61); `internal/engine/allocation_paging_test.go::TestEntryPageRollsBackOnWriteFailure` (8d93f61)
- 行为: 按计划每页Prepare/逐行Exec/Commit，页失败回滚，不持整个遍历事务。
- 兼容/风险: 页大小非业务总量上限，跨页归属不遗漏。

**T13.3 短容量预留事务** - 已实现; 提交: `8d93f61`

- 文件: `internal/engine/allocation_paging_test.go`, `internal/engine/backup.go`, `internal/engine/engine_test.go`
- 真实新增测试: `internal/engine/allocation_paging_test.go::TestReservationRechecksCurrentAccountAndPlanningSnapshot` (8d93f61); `internal/engine/allocation_paging_test.go::TestConcurrentReservationsDoNotOversell` (8d93f61); `internal/engine/allocation_paging_test.go::TestReservationValidatesWholeAccountBatchBeforeWriting` (8d93f61)
- 行为: 归属计算在事务外，最终短事务批量重验账号/规划及占用再整体预留。
- 兼容/风险: 保留防超售/整体回滚/稳定归属；不把应用去重替代DB约束。

**T13.4 幂等索引迁移v3** - 已实现; 提交: `b34e414`

- 文件: `docs/MIGRATIONS.md`, `internal/store/entry_indexes_migration_test.go`, `internal/store/store.go`
- 真实新增测试: `internal/store/entry_indexes_migration_test.go::TestEntryIndexMigrationPreservesRowsAndIsIdempotent` (b34e414)
- 行为: 新增run/repo/source/path索引，移除旧repo和重复tree；标记v3避开已用通知v2。
- 兼容/风险: 事务保留存量数据；首次建索引有耗时空间成本，升级前停机备份。

### T14

**T14.1 终态清单与引用保护** - 已实现; 提交: `1dc8632`, `5e4cfa9`

- 文件: `docs/MIGRATIONS.md`, `internal/engine/entry_cleanup.go`, `internal/engine/entry_cleanup_test.go`, `internal/engine/retention_state_test.go`, `internal/engine/service.go`
- 真实新增测试: `internal/engine/entry_cleanup_test.go::TestFailedRunEntriesAreRemoved` (1dc8632); `internal/engine/entry_cleanup_test.go::TestStartupSweepPreservesLiveImportedAndActiveEntries` (1dc8632); `internal/engine/retention_state_test.go::TestMaintenanceExecuteCancellationDoesNotWaitForEntryCleanup` (5e4cfa9)
- 行为: 仅backup/preview清无引用自有清单，每批最多5000；保留live点、活动task和正在发布import。
- 兼容/风险: partial_failed已发布点受保护；无自有清单的restore/migrate/retain不额外等写清扫。

**T14.2 deleted点清单回收** - 已实现; 提交: `1dc8632`, `5e4cfa9`

- 文件: `docs/MIGRATIONS.md`, `internal/engine/entry_cleanup.go`, `internal/engine/entry_cleanup_test.go`, `internal/engine/integration_test.go`, `internal/engine/maintenance.go`, `internal/engine/maintenance_concurrency_test.go`
- 真实新增测试: `internal/engine/entry_cleanup_test.go::TestDeletedPointCleanupPreservesOtherLiveReferences` (1dc8632); `internal/engine/maintenance_concurrency_test.go::TestMaintenancePruneAllowsConcurrentCommitAndPreservesNewPending` (5e4cfa9)
- 行为: 选在标记deleted后回收，prune失败仍不保留不可浏览清单；相关锁先释放。
- 兼容/风险: 共享RunID仍live不删；不动归属/预留；取消机会清扫由启动重试。

**T14.3 启动清扫/NULL安全** - 已实现; 提交: `1dc8632`, `eadace5`

- 文件: `internal/engine/entry_cleanup.go`, `internal/engine/entry_cleanup_test.go`, `internal/engine/service.go`
- 真实新增测试: `internal/engine/entry_cleanup_test.go::TestStartupSweepPreservesLiveImportedAndActiveEntries` (1dc8632); `internal/engine/entry_cleanup_test.go::TestStartSweepsInterruptedEntriesAsynchronously` (1dc8632); `internal/engine/entry_cleanup_test.go::TestCleanupWaitsForImportedIndexPublication` (1dc8632); `internal/engine/entry_cleanup_test.go::TestCleanupHandlesNullableHistoricalTaskIDs` (eadace5)
- 行为: Start异步逐批重查引用；活动任务NOT EXISTS防NULL污染，snapshot NULL task_id可清理。
- 兼容/风险: 保留live run_id coalesce空串语义；discover暂存/发布与仍被live引用的import受保护。

**T14.4 校验临时行defer** - 已实现; 提交: `1dc8632`

- 文件: `internal/engine/backup.go`, `internal/engine/entry_cleanup_test.go`, `internal/engine/integration_test.go`
- 真实新增测试: `internal/engine/entry_cleanup_test.go::TestSnapshotVerificationCleansUpOnEarlyReturn` (1dc8632)
- 行为: 按task/repo有界清理校验行，覆盖成功/引擎失败/取消/验证提前返回。
- 兼容/风险: 清理失败只日志、启动重试，不替换原错误。

### T15

**T15.1 设计批准与context原语** - 已实现; 提交: `238936f`, `a182de8`, `42f8547`

- 文件: `docs/LOCKING.md`, `internal/ctxlock/lock.go`, `internal/ctxlock/lock_test.go`
- 真实新增测试: `internal/ctxlock/lock_test.go::TestMutexCancellationAndReuse` (238936f); `internal/ctxlock/lock_test.go::TestMutexSerializesConcurrentUpdates` (238936f); `internal/ctxlock/lock_test.go::TestRWConcurrentReadersAndCancelledWriter` (238936f); `internal/ctxlock/lock_test.go::TestRWCancelledReaderDoesNotAffectWriter` (238936f); `internal/ctxlock/lock_test.go::TestRWCompetingReadersAndWriters` (238936f); `internal/ctxlock/lock_test.go::TestMutexCancellationAfterAcquisitionReturnsToken` (a182de8)
- 行为: 用户批准后零值Mutex/RWMutex；显式ctx失败归还token，旧Lock兼容；补刚获锁取消/有界复取测试。
- 兼容/风险: 写者取得准入后新读者排队，禁止R→W升级；不称全应用旧等待均可取消。

**T15.2 资源/HTTP配置门禁** - 已实现; 提交: `238936f`

- 文件: `internal/engine/account_clear.go`, `internal/engine/cloud_connect.go`, `internal/engine/context_lock_test.go`, `internal/engine/service.go`, `internal/platform/context_lock_test.go`, `internal/platform/runtime.go`
- 真实新增测试: `internal/engine/context_lock_test.go::TestCancelledMaintenanceHTTPWaitsReleaseOuterGates` (238936f); `internal/engine/context_lock_test.go::TestResourceLockCancellationReleasesPartialAcquisition` (238936f); `internal/platform/context_lock_test.go::TestAccountConfigurationContextCancellationPreservesLegacyLock` (238936f); `internal/platform/context_lock_test.go::TestContextConfigurationLockExcludesLegacyCaller` (238936f)
- 行为: 排序去重account→repo，部分失败逆序退锁；受影响HTTP配置/catalog等待使用请求ctx。
- 兼容/风险: 锁等待取消不开始提交；平台普通旧Lock调用继续兼容阻塞，避免accountMu重入。

**T15.3 恢复共享读与清扫写** - 已实现; 提交: `238936f`, `5e4cfa9`

- 文件: `internal/engine/account_clear.go`, `internal/engine/catalog.go`, `internal/engine/context_lock_test.go`, `internal/engine/entry_cleanup.go`, `internal/engine/restore.go`, `internal/engine/retention_state_test.go`, `internal/engine/service.go`
- 真实新增测试: `internal/engine/context_lock_test.go::TestRestoreReadLockAllowsFileBrowsing` (238936f); `internal/engine/context_lock_test.go::TestCleanupIndexWriteWaitCanBeCancelled` (238936f); `internal/engine/retention_state_test.go::TestMaintenanceExecuteCancellationDoesNotWaitForEntryCleanup` (5e4cfa9)
- 行为: restore/列表/预览R，discover/clear/逐批清扫W；列表物化后释放再写响应。
- 兼容/风险: discover/clear和认证网络仍长持catalog；取消无清单任务不抢清扫W。

**T15.4 迁移数据/发布分阶段** - 已实现; 提交: `ad9de2c`, `5e4cfa9`

- 文件: `docs/LOCKING.md`, `internal/engine/maintenance.go`, `internal/engine/maintenance_concurrency_test.go`, `internal/engine/migration_state.go`, `internal/engine/migration_state_test.go`
- 真实新增测试: `internal/engine/maintenance_concurrency_test.go::TestMaintenanceMigrationDataWorkAllowsOtherPlanCommit` (ad9de2c); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceMigrationReleasesResourcesBeforeCatalogWait` (ad9de2c); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceMigrationRejectsChangesDuringCopy` (ad9de2c); `internal/engine/migration_state_test.go::TestMaintenanceRepositoryCASPreservesConcurrentChanges` (ad9de2c); `internal/engine/migration_state_test.go::TestMaintenanceRepositoryCASChecksValidationAndAffectedRows` (ad9de2c); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceMigrationPublicationFailureRestoresOldMapping` (5e4cfa9); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceDegradedMigrationKeepsCommittedMapping` (5e4cfa9)
- 行为: copy/read-data无catalog，释放资源后按顺序重取；短Tx重验repo/物理账号/当前池/预留并CAS。
- 兼容/风险: quota版本变化允许；非降级映射失败精确回滚；逻辑降级不回滚，源与任务副本保留。

**T15.5 旧副本四阶段交接** - 已实现; 提交: `ad9de2c`

- 文件: `docs/LOCKING.md`, `internal/engine/maintenance.go`, `internal/engine/maintenance_concurrency_test.go`, `internal/engine/migration_state.go`
- 真实新增测试: `internal/engine/maintenance_concurrency_test.go::TestMaintenanceOldCopyDataWorkAllowsOtherPlanCommit` (ad9de2c); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceOldCopyRejectsMappingChangeBeforeRetirement` (ad9de2c); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceOldCopyPublicationFailureRemainsRetryable` (ad9de2c); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceOldCopyRejectsUnusableCurrentAccountAtHandoff` (ad9de2c)
- 行为: 校验/退休W/无catalog purge/完成CAS逐阶段重验，检查当前账号仍可恢复。
- 兼容/风险: 不purge当前；purge后失败/取消保留cleanup_pending且不重新Verified，缺目录可重试。

**T15.6 retain决策/prune/发布** - 已实现; 提交: `5e4cfa9`

- 文件: `docs/LOCKING.md`, `internal/engine/maintenance.go`, `internal/engine/maintenance_concurrency_test.go`, `internal/engine/retention_state.go`, `internal/engine/retention_state_test.go`
- 真实新增测试: `internal/engine/maintenance_concurrency_test.go::TestMaintenancePruneAllowsConcurrentCommitAndPreservesNewPending` (5e4cfa9); `internal/engine/maintenance_concurrency_test.go::TestMaintenancePruneFailureKeepsDecisionForRetry` (5e4cfa9); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceRetentionWaitsForRestoreReaders` (5e4cfa9); `internal/engine/maintenance_concurrency_test.go::TestMaintenanceRetentionDoesNotForgetSharedSnapshot` (5e4cfa9); `internal/engine/retention_state_test.go::TestMaintenancePendingRemovalIsAtomicAndPreservesJSON` (5e4cfa9)
- 行为: W+catalog认证删除并原子登记，释放后R下prune无catalog，最后最新目录+单条批CAS增量移除。
- 兼容/风险: 旧restore先排空，新restore拒deleted；共享snapshot/并发point/new pending保留；失败/取消可重试。

### T16

**T16.1.1 未使用符号清理** - 已实现; 提交: `ace9599`

- 文件: `internal/engine/restore.go`, `internal/platform/crypto.go`, `internal/platform/onedrive.go`, `internal/platform/resources.go`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 移除request/formatVersion/tokenEndpoint/retryDuration/describeError及无用import。
- 兼容/风险: 先检索引用；无新功能，不虚构新增测试。

**T16.1.2 用户文案/测试简化** - 已实现; 提交: `ace9599`

- 文件: `internal/platform/notifications.go`, `internal/platform/platform_test.go`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: Telegram产品名用户文案保留并加精确ST1005理由；Body.String替换冗余转换。
- 兼容/风险: 不强行小写用户品牌文案；复用既有测试。

**T16.1.3 直接依赖整理** - 已实现; 提交: `ace9599`

- 文件: `go.mod`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: doublestar/v4标为直接依赖。
- 兼容/风险: 依赖版本不变，不顺带升级。

**T16.2.1 重连PRAGMA** - 已实现; 提交: `332c339`

- 文件: `docs/MIGRATIONS.md`, `internal/store/connection_logs_test.go`, `internal/store/store.go`
- 真实新增测试: `internal/store/connection_logs_test.go::TestConnectionPragmasSurviveReconnection` (332c339)
- 行为: DSN设置busy_timeout/foreign_keys/WAL/synchronous=NORMAL，重建连接仍生效。
- 兼容/风险: NORMAL保一致性，但断电可丢最近事务，不能替代认证目录/备份。

**T16.2.2 审计ID去冲突** - 已实现; 提交: `332c339`

- 文件: `docs/MIGRATIONS.md`, `internal/store/connection_logs_test.go`, `internal/store/store.go`
- 真实新增测试: `internal/store/connection_logs_test.go::TestAuditEventsAtSameNanosecondAreDistinct` (332c339)
- 行为: 时间前缀加随机后缀，列表仍按更新时间。
- 兼容/风险: ID需当不透明字符串，不依赖旧数字排序。

**T16.2.3 UTF-8日志截断** - 已实现; 提交: `332c339`

- 文件: `internal/store/connection_logs_test.go`, `internal/store/store.go`
- 真实新增测试: `internal/store/connection_logs_test.go::TestLogTruncationPreservesUTF8` (332c339)
- 行为: 8192字节附近按合法UTF-8截断。
- 兼容/风险: 保留字节限制，非字符数上限。

**T16.3.1 监听成功再调度** - 已实现; 提交: `16a0643`

- 文件: `cmd/vaultpilot/main.go`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: net.Listen成功后才启动Service与Serve。
- 兼容/风险: 绑定失败不先执行恢复任务；未新增独立端口冲突测试声明。

**T16.3.2 健康检查VP_ADDR** - 已实现; 提交: `16a0643`

- 文件: `cmd/healthcheck/main.go`, `cmd/healthcheck/main_test.go`
- 真实新增测试: `cmd/healthcheck/main_test.go::TestHealthcheckURLUsesListenAddress` (16a0643)
- 行为: 解析监听端口，通配/空地址改127.0.0.1访问。
- 兼容/风险: 非默认端口/IPv6有回归；Docker健康状态仍待环境运行。

**T16.3.3 镜像HEALTHCHECK** - 已实现; 提交: `16a0643`

- 文件: `Dockerfile`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 镜像声明/app/healthcheck，直接docker run也启用。
- 兼容/风险: compose可覆盖镜像健康配置。

**T16.4.1 敏感导出密码重新验证** - 已实现; 提交: `4345395`

- 文件: `internal/platform/account_transfer.go`, `internal/platform/account_transfer_test.go`, `internal/platform/auth.go`, `internal/platform/auth_test.go`, `internal/platform/cloud_connect_test.go`, `internal/platform/platform_test.go`
- 真实新增测试: `internal/platform/auth_test.go::TestSensitiveExportsRequireAdminPassword` (4345395); `internal/platform/auth_test.go::TestExportReauthenticationThrottleAndReset` (4345395)
- 行为: 两种导出必须admin_password，错误计独立reauth桶，成功复位。
- 兼容/风险: 有意新增请求契约；缺/错密码401，CSRF继续要求，包密码与登录密码语义区分。

**T16.4.2 导出UI/API/e2e同步** - 已实现; 提交: `4345395`

- 文件: `docs/API.md`, `docs/SECURITY.md`, `docs/openapi.yaml`, `scripts/e2e_local.py`, `web/src/AccountTransfer.vue`, `web/src/App.vue`, `web/tests/sensitive-exports.test.mjs`
- 真实新增测试: `web/tests/sensitive-exports.test.mjs::recovery export sends current login password and clears it after ${failed ? 'failure' : 'success'}` (4345395); `web/tests/sensitive-exports.test.mjs::account export sends current login password and clears it after ${failed ? 'failure' : 'success'}` (4345395)
- 行为: 弹窗发送当前登录密码，成功/失败/关闭清空；同步OpenAPI/API/安全文档和e2e。
- 兼容/风险: 失败不误报下载，密码不写入导出包或持久状态。

**T16.5.1 Drive GET只读** - 已实现; 提交: `92f3a31`

- 文件: `docs/openapi.yaml`, `internal/platform/account_drives_readonly_test.go`, `internal/platform/account_identity_test.go`, `internal/platform/cloud_account_test.go`, `internal/platform/onedrive.go`, `internal/platform/onedrive_performance_test.go`, `internal/platform/platform_test.go`
- 真实新增测试: `internal/platform/account_drives_readonly_test.go::TestVerificationPersistsOptionalIdentityAfterCloudChecks` (92f3a31); `internal/platform/account_drives_readonly_test.go::TestVerificationFailureAndMissingCSRFDoNotPersistIdentity` (92f3a31)
- 行为: GET只临时回显身份，不写账户/版本；verify访问/配额成功后保存，同版本有界TTL缓存，缺User.Read不阻断。
- 兼容/风险: 旧授权结果拒绝、过期重取、POST需CSRF；不泛称OAuth续期/通知也完全无内部状态。

**T16.5.2 更新名称边界** - 已实现; 提交: `d9f975e`

- 文件: `internal/platform/account_name_test.go`, `internal/platform/resources.go`
- 真实新增测试: `internal/platform/account_name_test.go::TestUpdateAccountNameMatchesCreateLimits` (d9f975e)
- 行为: update与create同样trim、非空且len<=200。
- 兼容/风险: 保持既有按字节而非rune口径，中文边界有测试。

**T16.5.3 删除后清凭据原有即符合** - 原有即符合; 提交: 无独立代码提交（原实现符合、按计划暂缓/可选，或由本报告文档提交记录）

- 参考文件（非本项新增修改）: `internal/platform/resources.go`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: baseline已先检查Store.Delete成功再清credential，最终顺序仍符合，未改代码。
- 兼容/风险: 账户→凭据锁顺序已核对；不虚构修复提交/新测试。

**T16.5.4 既有目录显式0700** - 已实现; 提交: `564f43d`

- 文件: `internal/platform/runtime.go`, `internal/platform/runtime_permissions_test.go`
- 真实新增测试: `internal/platform/runtime_permissions_test.go::TestNewRestrictsExistingStateAndCacheDirectories` (564f43d)
- 行为: MkdirAll后显式Chmod state/cache 0700，失败只日志。
- 兼容/风险: 覆盖默认与独立cache；未注入系统Chmod失败，不宣称设置必然成功。

**T16.6.1 隐藏页暂停轮询** - 已实现; 提交: `84a997d`

- 文件: `web/src/App.vue`, `web/src/NetworkActivity.vue`, `web/src/polling.ts`, `web/tests/polling.test.mjs`
- 真实新增测试: `web/tests/polling.test.mjs::hidden tabs pause polling and becoming visible refreshes immediately before resuming the interval` (84a997d); `web/tests/polling.test.mjs::polling mounted in a hidden tab waits for visibility and never overlaps an in-flight refresh` (84a997d)
- 行为: App/NetworkActivity共享可见性轮询，隐藏停、恢复立即刷新，卸载清监听，避免并行重入。
- 兼容/风险: 可见页周期保留；进行中请求不重复启动。

**T16.6.2 后台错误连续阈值** - 已实现; 提交: `e1ed8c7`

- 文件: `web/src/App.vue`, `web/tests/background-errors.test.mjs`
- 真实新增测试: `web/tests/background-errors.test.mjs::${method} suppresses the first two background failures, resets on success and reports manual failures immediately` (e1ed8c7); `web/tests/background-errors.test.mjs::task and log polling failures are counted independently` (e1ed8c7); `web/tests/background-errors.test.mjs::status polling shows connection trouble only after three failures and clears the streak on success` (e1ed8c7)
- 行为: 任务/日志/status独立计数，前两次静默、第三次提示，成功复位；手动失败即时显示。
- 兼容/风险: 不把不同入口混桶，不永久吞故障。

**T16.6.3 键盘/焦点/aria** - 已实现; 提交: `1e955e1`

- 文件: `web/src/App.vue`, `web/src/RcloneAuthorization.vue`, `web/src/SourceBrowser.vue`, `web/tests/accessibility.test.mjs`
- 真实新增测试: `web/tests/accessibility.test.mjs::opening a modal focuses its first enabled input and closing restores the trigger` (1e955e1); `web/tests/accessibility.test.mjs::a confirmation without inputs receives focus without replacing the original trigger` (1e955e1); `web/tests/accessibility.test.mjs::${flag} keeps modal and pending confirmation open` (1e955e1); `web/tests/accessibility.test.mjs::removed trigger is not focused after closing` (1e955e1); `web/tests/accessibility.test.mjs::authorization and import requests publish busy state until success or failure` (1e955e1); `web/tests/accessibility.test.mjs::icon buttons have explicit accessible names and modal Escape uses the guarded close action` (1e955e1)
- 行为: 图标按钮aria-label；Esc经busy/授权保护；开启聚焦输入/确认窗、关闭恢复有效触发元素。
- 兼容/风险: 忙碌不误关；已移除触发元素不强行聚焦，不新增业务审批。

**T16.6.4 JSON null错误体** - 已实现; 提交: `07487a2`

- 文件: `web/src/api.ts`, `web/tests/api.test.mjs`
- 真实新增测试: `web/tests/api.test.mjs::JSON null error responses become ApiError and release the active request` (07487a2); `web/tests/api.test.mjs::structured errors retain their server message, code, retry policy and status` (07487a2); `web/tests/api.test.mjs::successful JSON null remains null` (07487a2)
- 行为: 错误null转ApiError并释放活动计数，结构化错误字段保留，成功null仍返回null。
- 兼容/风险: 共享API成功/失败契约不被判空误改。

**T16.6.5 开发默认本机监听** - 已实现; 提交: `442acc4`

- 文件: `web/package.json`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: dev去掉--host 0.0.0.0。
- 兼容/风险: LAN开发显式传参数；生产监听不变。

**T16.6.6 维护中Vue图标包** - 已实现; 提交: `958960b`

- 文件: `web/package-lock.json`, `web/package.json`, `web/src/AccountClear.vue`, `web/src/AccountTransfer.vue`, `web/src/App.vue`, `web/src/CloudConnection.vue`, `web/src/LoadingNotice.vue`, `web/src/NetworkActivity.vue`, `web/src/RcloneAuthorization.vue`, `web/src/ResultReport.vue`, `web/src/SourceBrowser.vue`, `web/src/TelegramNotifications.vue`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 所有组件import换@lucide/vue并同步lockfile。
- 兼容/风险: 保留图标用途；无新独立测试声明，最终前端构建验证待填。

**T16.6.8 App.vue拆分暂缓** - 暂不执行; 提交: 无独立代码提交（原实现符合、按计划暂缓/可选，或由本报告文档提交记录）

- 参考文件（非本项新增修改）: `web/src/App.vue`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 按计划不执行本次大重构。
- 兼容/风险: 另行立项，不能计为已完成拆分。

**T16.7.1 Node22具体补丁** - 已实现; 提交: `a79b138`

- 文件: `Dockerfile`, `README.md`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 镜像固定Node22.23.3-bookworm-slim。
- 兼容/风险: 后续升级显式改版本；未同时固定基础镜像digest。

**T16.7.2 固定引擎两架构哈希** - 已实现; 提交: `a79b138`

- 文件: `Dockerfile`, `README.md`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 选ARG固定Restic0.18.1/Rclone1.70.3 amd64/arm64 SHA-256，不再临时信任同源checksum。
- 兼容/风险: 并非GPG签名；版本升级需同步哈希；显式ARG覆盖依赖可信构建配置。

**T16.7.3 基础镜像digest可选未启用** - 可选未启用; 提交: 无独立代码提交（原实现符合、按计划暂缓/可选，或由本报告文档提交记录）

- 参考文件（非本项新增修改）: `Dockerfile`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 保留具体tag，不加@sha256镜像digest。
- 兼容/风险: 计划标为可选需用户决定，不能称已启用。

**T16.7.B 构建上下文排除worktree** - 已实现; 提交: `df15de9`

- 文件: `.dockerignore`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 排除.worktrees避免本地工具/工作树/产物进入Docker上下文。
- 兼容/风险: 附加构建卫生修复，实际镜像构建仍待Docker环境。

**T16.8.1 无缓存最终证据/哈希** - 已完成; 提交: 无独立代码提交（原实现符合、按计划暂缓/可选，或由本报告文档提交记录）

- 参考文件（非本项新增修改）: `docs/evidence/`, `docs/TEST_REPORT.md`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 已用无缓存-count=1及连续五轮-count=5刷新证据与TEST_REPORT，标注验收源码d7b6bd6。
- 兼容/风险: 结果绑定验收源码；真实外部环境限制单独列明。

**T16.8.2 真实外部环境边界** - 已记录未验证边界; 提交: 无独立代码提交（原实现符合、按计划暂缓/可选，或由本报告文档提交记录）

- 参考文件（非本项新增修改）: `docs/TEST_REPORT.md`
- 真实新增测试: 无独立新增测试声明；使用静态检查、构建或既有测试，结果见本轮最终验收与证据。
- 行为: 真实OneDrive/Docker/生产规模和多节点按实际环境标注，不沿用旧证据。
- 兼容/风险: 本地真实引擎/假并发脚本不能替代云端重授权、断网和容器验证。

## 独立提交索引

| 提交 | 主题 | 新增测试声明 |
|---|---|---|
| `3a4dfb9` | fix: build with Go 1.26.9 to pick up net/http and crypto/tls security fixes | 0 |
| `46a4037` | fix: run under tini so orphaned engine subprocesses are reaped | 0 |
| `a673666` | fix: drain subprocess pipes after oversized output lines to prevent task hangs | 1 |
| `d92fba0` | fix: build task-scoped rclone configs so one broken account cannot fail every task | 5 |
| `4e83484` | fix: restrict repository migration targets to the plan's storage pool | 2 |
| `26255c9` | fix: reject requests with unknown Host headers to block DNS rebinding | 2 |
| `c46880e` | fix: pass only allowlisted host environment to engine subprocesses | 1 |
| `910014f` | fix: merge quota refreshes with optimistic retries and bound post-task refresh time | 4 |
| `a6b6652` | fix: rate limit auth attempts per action and client with trusted proxy support | 3 |
| `61c7460` | fix: delay object URL revocation so recovery kit downloads complete | 1 |
| `39fde3b` | fix: isolate the environment of rclone credential obscuring | 1 |
| `41bb938` | fix: bound concurrent Argon2 derivations and auth request sizes | 5 |
| `198b02a` | fix: precheck retention and persist pending prunes so cleanup can be retried | 5 |
| `84a997d` | fix: pause dashboard polling while the tab is hidden | 2 |
| `73dd4df` | fix: make task cancellation a conditional flag update | 2 |
| `273d5d8` | perf: page file entries by key instead of OFFSET | 3 |
| `a79b138` | build: pin engine checksums and move the frontend build to Node 22 | 0 |
| `eedccac` | fix: release active plan slot when task execution fails to start | 3 |
| `4345395` | fix: require the admin password before exporting recovery or account kits | 4 |
| `16a0643` | fix: bind the listener before starting the scheduler and honor VP_ADDR in healthcheck | 1 |
| `e1ed8c7` | fix: delay background polling errors until three consecutive failures | 3 |
| `ace9599` | chore: remove unused code and tidy go.mod | 0 |
| `1e955e1` | fix: label icon controls and support modal keyboard navigation | 6 |
| `07487a2` | fix: handle JSON null API error responses | 3 |
| `442acc4` | fix: bind the development server to localhost by default | 0 |
| `92f3a31` | fix: persist cloud account identity only after verification | 2 |
| `958960b` | chore: migrate Vue icons to the maintained lucide package | 0 |
| `d9f975e` | fix: enforce account name limits when updating accounts | 1 |
| `564f43d` | fix: restrict existing runtime directories to owner access | 1 |
| `8d93f61` | perf: batch entry writes and shorten allocation transactions | 5 |
| `b34e414` | perf: replace redundant file_entries indexes | 1 |
| `332c339` | fix: set SQLite pragmas in the DSN and harden audit/log writes | 3 |
| `d31a720` | fix: preserve new task pending triggers when cancelling old tasks | 1 |
| `1dc8632` | fix: clean unreferenced run and snapshot verification entries | 6 |
| `eadace5` | fix: use null-safe activity guards when cleaning orphan entries | 1 |
| `238936f` | fix: make maintenance locks cancellable and share recovery index reads | 11 |
| `ad9de2c` | fix: separate migration data work from catalog publication | 9 |
| `df15de9` | build: exclude local worktrees from the Docker build context | 0 |
| `a182de8` | test: cover cancellation immediately after lock acquisition | 1 |
| `5e4cfa9` | fix: separate retention decisions and publication from prune | 8 |
| `42f8547` | test: bound lock reacquisition checks after cancellation | 0 |

合并提交仅用于整合，不代替独立feature提交。文件状态、新测试文件、声明源提交和冻结源码行号见 [逐项文件与测试证据](evidence/fix-plan-validation.json)。没有自动推送。
