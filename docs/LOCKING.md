# T15：锁范围、取消与阶段交接设计

状态：用户已确认本设计。已实现 context 锁原语、锁等待适配、恢复索引读写与 T14 清扫对接，以及 migrate/cleanupOld 的分阶段、短事务复核、CAS 发布与精确版本回退。复制、全量校验与旧位置 purge 不持 catalog；retain 的阶段拆分待后续提交完成。

实现基线为 `5c4a746ec36cffa8e1a0a2368817d3ae7466dec6`，已含 T05、T10、T13、T14 及后续 NULL/并发边界补修。已基于合并后的 `entry_cleanup.go` 重新检索锁调用点。

## 1. 建议确认的方案

1. `catalogMu` 只串行化目录生成/认证发布及相应本地恢复点、仓库映射、待清理状态的变更。复制整仓、全量 Restic 校验、forget/prune、删除旧物理副本期间不持有它。目录文件的上传、读回、prepare/ok 认证本身仍在此锁内，不能承诺所有网络等待都消失。
2. 恢复索引使用支持 context 的读写锁。restore 全程、文件列表和恢复预览持读锁；discover、账号清空持写锁；T14 清扫逐批持写锁。retain 的删除决策短持写锁，排空已开始的 restore，再切换到读锁执行 prune 与最终发布；不进行读写锁升级。
3. 迁移复制阶段持计划锁、排序账号锁和仓库锁。进入发布阶段前先释放账号/仓库锁，再依统一顺序获取恢复索引读锁、catalog、排序账号锁、仓库锁，重新验证版本和位置；有变化中止，保留旧物理位置。
4. 所有新等待均使用调用方 context。为让 protect/清空预览真正能取消等待，还要覆盖其外层配置锁，而非只替换 catalog。
5. 不新增外部依赖，不改变角色、配额、任务数量或审批规则，不做数据库结构迁移。使用小型内部锁实现，保留既有无 context API 的默认行为。

需要接受的调度边界：采用写者优先准入，防止 discover/清扫一直被新读者饿死。没有等待写者时，restore 与文件浏览并发；写者已经取得准入门闩、正在等待旧 restore 退出时，后到的浏览也可能排队。这是公平读写锁的边界，不能宣称“任何情况下浏览都不会等待”。若要求写者排队期间浏览也始终可用，需要另行设计恢复清单引用租约/版本固定，超出本次小范围方案。

## 2. 不变量与锁顺序

| 锁 | 保护的数据/不变量 | 本次变化 |
| --- | --- | --- |
| `Service.mu` | active/pending/cancels/cancelled、任务取消标记与执行器保存的合并顺序 | 保留 T10 语义；不在持有它时等待计划、索引、catalog 或资源锁，不改任务保存协议 |
| `runMu` | cron 注册表、scheduleIDs 的重载 | 保留；调度回调只 enqueue，不在其中获取计划/仓库锁 |
| `configurationMu` | 计划编辑/删除/保护操作与账号清空之间的排他关系 | 范围不扩大，等待改为 context 可取消 |
| `Runtime.accountMu` | 账号编辑、删除、验证、授权安装与清空之间的配置一致性；recentDrives 缓存 | 保留原阻塞 Lock/Unlock 方法；为引擎新增 context 入口。迁移最终验证/发布使用此配置门禁，阻止验证后立即停用/替换目标账号 |
| `plan:<id>` | 同计划备份、预览、retain、migrate/cleanupOld 的互斥；待清理快照集合的计划内生命周期 | 继续覆盖整个相应任务，等待使用任务 context |
| `recoveryIndex` | 文件清单被读取期间不被 discover 换代、T14 删除或账号清空；旧恢复地址在被使用期间不被物理清除 | 从字符串互斥锁变为专用读写锁。迁移切换当前映射保留旧副本，因此可在索引读锁下完成；删除旧副本前先写锁排空持旧地址的恢复读者 |
| `catalogMu` | 一次目录快照及认证发布与对应本地状态的顺序；保护、删除、迁移映射、pending_forget 的发布一致性 | context 互斥锁，仅元数据决策与发布阶段持有 |
| `account:<id>` | 对同一物理账号的引擎数据操作互斥 | 原有语义，多个账号先去重再按 ID 排序，按逆序释放；等待使用任务 context |
| `repo:<id>` | 同仓库 restic 写入/读取校验/prune/迁移的互斥 | 原有语义，在账号锁之后获取；等待使用任务 context |
| `cloud-connect` | 已有云端连接入口发布的串行化 | 不纳入目录/恢复索引嵌套关系，只把已有等待改为请求 context |

涉及这些锁的嵌套统一为：

`plan（如需） → configurationMu（如需） → Runtime.accountMu（如需） → recoveryIndex（R/W） → catalogMu（如需） → 排序去重后的 account 锁 → repo 锁`

每条路径只获取所需的子序列，逆序释放。现有 clear/protect 不获取计划锁，符合此顺序。持有 account/repo 时绝不再等待上游配置锁、恢复索引或 catalog。跨阶段保留计划锁可以，但须先释放本阶段的 account/repo 再进入下一阶段。禁止索引 R→W 升级；释放再重取后必须重新读取并验证状态。

当前代码的细节：discover 和 clearAccounts 直接获取的是 recovery-index→catalog，未在这两个函数内逐个调用 account/repo 锁；clearAccounts 还有更外层的两个配置门禁。cleanupOld/migrate 已经存在 catalog→account/repo 的真实路径。因此将迁移简单改成“先持 account/repo 复制、然后直接等 catalog”会与另一条 catalog→account 路径构成死锁，不能这样实现。

`Runtime.accountMu` 与引擎的 `account:<id>` 不是同一把锁：前者约束账号配置/凭据安装，后者约束任务的数据操作。不可用后者代替前者宣称账号不会被并发停用或替换。

## 3. 可实现的 context 锁

建议新增 `internal/ctxlock`，仅实现 Mutex、RWMutex 两种原语，以便 engine 与 platform 共用，避免包循环。不是引入新的业务公共接口。

### Mutex

- 零值可用；`sync.Once` 初始化容量为 1 的 token channel。
- `LockContext(ctx) error` 在 token 与 `ctx.Done()` 间 select；获得 token 后再次检查 `ctx.Err()`，若已取消立即归还 token 并返回错误，避免“锁空闲与取消同时就绪”时错误地继续业务。
- `Unlock()` 归还 token。配对方法的使用约束与 sync.Mutex 相同。
- 兼容方法 `Lock()` 使用 Background，原有调用的成功/失败和事件顺序保持不变。新 HTTP/任务入口使用 `LockContext`。
- 不为每个等待者启动 goroutine，不轮询、不 sleep，不增加等待人数限制。

### RWMutex

使用两个 context Mutex（准入门闩、空房间）与一个仅保护读者计数的短 `sync.Mutex`：

- 读者先以 context 获取准入门闩；在短计数锁内，首个读者占用空房间，增加计数，释放准入门闩。所有读者业务并发执行。最后一个读者归还空房间。
- 写者先以 context 获取准入门闩，再以 context 等待空房间；持写锁期间不允许新读者进入。等待空房间时取消，必须释放已持有的准入门闩。
- 写者释放空房间后释放准入门闩，顺序固定。读者计数锁内没有数据库、网络或外部长等待。
- `RLockContext/LockContext` 均检查取消并正确退还已经获得的资源；提供原有 `RLock/Lock` 兼容方法。不得实现锁升级或在取消后留下占位 token。
- 单元测试验证：互斥、并行读、写者等待、等待中取消、刚获锁即取消、最后读者释放、写者取消后新读者可继续、反复竞争不泄漏 token。

Service 增加专用 `recoveryIndex`，不再把它存进通用 keyed mutex map。catalog/configuration 字段使用相应 context 原语。keyed plan/account/repo 锁也使用同一 Mutex，实现一个带 context 的获取入口；所有生产调用点逐一迁移，避免任务取消后仍永久等资源锁。

Runtime 的改动限于替换 accountMu 的内部原语并增加 `LockAccountConfigurationContext(ctx) (release func(), err error)`。现有 `LockAccountConfiguration()` 和 resources/onedrive/account_transfer/rclone_import 的既有 Lock/Unlock 调用继续有效；不顺带改变这些业务入口的默认校验或响应。

HTTP 锁等待取消时立即返回，不再启动目录提交，也不向已经取消的请求拼出一个新的通用 400 错误。后台任务返回 context 错误，交给已有 execute 取消终态处理。已经产生认证提交后的有界本地收尾继续使用 T05 等既有 WithoutCancel 语义，不能因新增锁取消而丢掉已提交删除决策。

## 4. 每个获取点的前后范围

| 获取点 | 当前范围 | 建议范围 |
| --- | --- | --- |
| `backup` 计划锁 | 全备份/预览 | 保持，仅取消等待可响应 context |
| `backup` 每分片 account→repo | 单次 backupShard，发布目录前已释放 | 保持；显式保证任何路径均释放后再等 catalog |
| `backup` catalog | 最终 makeCatalog/上传认证/保存恢复点/assignments | 保持这段原子语义；等待可取消 |
| `retain` 计划锁 | 决策、prune、最终目录和清理完成 | 保持全程 |
| `retain` catalog | 全函数，包含 forget/prune | 只在“重新预览和删除决策发布+本地记录”与“最终完成发布+pending 清理”两段持有 |
| `prunePending` account→repo | 每仓库 snapshots/forget/prune | 保持，补重新读取仓库/账号和引用集合，禁止带资源锁回头等 catalog |
| `migrate` 计划锁 | 全函数含 cleanupOld | 保持全程 |
| `migrate` catalog | size/capacity/copy/check/映射发布全过程 | 复制校验不持有；释放资源锁后，只在重新验证→CAS 映射→认证提交/必要回滚阶段持有 |
| `migrate` account→repo | 从 copy 前到整个函数退出，含 catalog 提交 | 分成复制校验一段、最终发布一段；两段间全部释放后按统一顺序重取 |
| `cleanupOld` catalog | 当前全量 check、引用 cat、retirement、purge、最终 commit | 校验和 purge 都不持有；retirement 发布、删除完成发布分别短持 |
| `cleanupOld` account→repo | 当前全函数内部长持 | 每阶段独立获取；跨阶段重验 repo/位置/引用集合，不能带着资源锁等 catalog 或索引写锁 |
| `protect` configuration R、catalog | 全 handler；两处等待均不响应取消 | configuration R 范围保持；catalog 从读取恢复点版本到认证发布/本地 CAS；两处等待使用请求 context |
| `clearPreviewHTTP` Runtime account 配置、catalog | 预览全过程；外层配置等待和 catalog 等待均不可取消 | 预览范围保持，两处等待均使用请求 context；不触碰实际清空语义 |
| `clearAccounts` 两个配置门禁、索引、catalog | 全清空生命周期 | 本次保持破坏性清空的全程排他边界，索引改 W，各等待使用任务 context；不把本次 T15 顺带变成清空协议重构 |
| `discover` 索引、catalog | 远端发现、认证、暂存清单、事务换代全过程 | 本次保持这段独立协议；索引改 W、catalog 改可取消。需要后续缩小 discover 的目录锁时另行设计暂存/换代验证 |
| `restore` recovery-index | 全恢复生命周期的互斥锁 | 全程 R；因此同一索引可被列表/预览并发读取，又可防旧 RunID/地址在恢复期间被清扫或换代 |
| `restorePreviewHTTP` recovery-index | 整个预览互斥 | 请求级 R，等待和已有预览查询/检查均沿用请求 context |
| `pointFiles` recovery-index | 读恢复点、count、分页、写响应期间互斥 | 读恢复点/count/分页期间 R；查询结果完全物化后释放，再序列化响应，避免慢客户端拖住索引 |
| T14 `deleteEntryBatches` | 每批旧 recovery-index 互斥→DELETE→释放 | 每批 context W→同一 DELETE→释放；不扩大批大小、查询范围或删除条件 |
| `check` account→repo | 每个仓库检查 | 保持，仅使用任务 context 获取 |
| `cloudConnectionEnable` cloud-connect | 发布连接资料 | 保持，仅使用已有请求/超时 context 获取 |
| `planSave`/`planDelete` configuration R | 原有配置处理范围 | 范围保持，等待使用请求 context |

所有 `commit()` 调用继续由上层显式持 catalog；commit 本身不隐式再锁，防重入死锁。迁移/清理阶段需使用清晰局部函数或局部作用域保证释放顺序；不能靠“整个 migrate 的 defer”让复制资源锁穿透到最终发布。

## 5. migrate 与 cleanupOld 的阶段交接

### 普通迁移

**复制/校验阶段**

1. 获取计划锁；获取排序去重后的源/目标 account 锁和 repo 锁。重新读仓库，保存基线 version、plan_id、当前 AccountID/Path、Locations 及源/目标物理身份。
2. 按现有规则检查目标已验证启用、属于存储池，刷新容量，创建持久容量预留；复制到已有的任务专属 destination，并完整读取校验。此阶段没有 catalog 和索引锁。
3. 不修改仓库当前映射，不删除旧位置。成功或失败都先释放 account/repo，随后才进入发布阶段。

**发布阶段**

4. 保留计划锁；按顺序获取必要的配置读门禁/Runtime 账号配置门禁、索引 R、catalog、排序 account、repo。
5. 再次读取仓库：version 与基线必须相同；plan_id、当前位置、旧位置集合不得发生变化，状态不得为 retired。重新读取最新计划/池、源/目标账号：目标仍存在、已验证启用、属于当前计划的当前存储池，物理 backend/remote/local_path/Drive/region/tenant 未被替换，容量预留仍有效。账号配额刷新本身不应被当作物理身份变更。已复制数据不能再被当成尚未复制容量重复扣除。
6. 任何不一致均在写映射前中止，返回现有冲突/不可用错误；源仓库映射和源数据保持原样。已复制的任务专属目录不自动 purge，避免误删并保留诊断/重试余地；现有配额收尾应计入其真实占用。
7. 最终的 repo 版本、计划/池归属和账号条件复核，与新映射/已验证 Locations 的 CAS 保存放进同一个短 SQLite 事务；同时维护实体 JSON version 与列 version。不能在复核后结束事务，再单独无条件写映射。事务提交后才进行远端目录认证，绝不持数据库事务跨网络。非降级的认证提交失败时，在同一锁范围内用保存后的精确版本、有界本地 context 回退旧映射；不使用无条件覆盖其他版本。PROTECTION_DEGRADED 沿用既有“已逻辑提交、保护降级”语义，不能假装未提交而回退。

这里的重验发生在配置转换的线性化点。计划/池版本和 membership 使用该时刻的快照，不承诺禁止用户随后合法编辑存储池；本次不新增整个迁移期间禁止编辑存储池的业务规则。

### 清理旧副本

1. **只读校验**：计划→索引 R→排序 account→repo；校验当前位置、完整 read-data 和所有有效快照引用。记录 repo 版本、当前/待删旧位置、账号物理身份、引用集合摘要；没有 catalog。
2. **退休决策**：先释放资源锁及索引 R，再获取配置门禁→索引 W→catalog→排序 account→repo。W 排空此前可能仍持旧地址的 restore。重新验证版本、位置、账号与引用集合；有变化中止，保留旧副本。发布旧位置 Verified=false、cleanup_pending 与认证目录；保持现有先提交退休再删除的顺序。
3. **物理删除**：释放上一阶段的资源/catalog/W，再按配置门禁→索引 R→排序 account→repo 获取；重验当前有效位置和待删位置仍完全一致且旧位置绝不是当前地址。执行旧位置 purge，无 catalog。阶段间若 discover 改过映射，停止删除，不能沿用旧内存快照。
4. **完成发布**：先释放 account/repo，保持或按顺序重新取得索引 R，再取 catalog→排序 account→repo，重新读取并以 CAS 标记旧位置 deleted，发布认证目录。purge 后取消/提交失败时保留 durable cleanup_pending，重试沿用现有“目录不存在”处理；不重新激活已删除位置。

## 6. retain：保留 T05 的决策持久性，释放 prune 的 catalog

1. 计划锁保持全程。涉及新删除决策时先取得索引 W，再取得 catalog；没有新增删除、只是 retry prune 的入口可直接使用索引 R。
2. 在锁内重新预览并检查保护、保留策略和全局引用；认证发布删除决策后，使用 T05 已有的有界 WithoutCancel 本地事务记录 tombstone 和 pending_forget。这个原子边界不能被用户取消拆开。
3. 释放 catalog 和索引 W；重新取得索引 R。W→R 不是原子降级，因此重新读当前仓库、pending_forget 和全局有效引用。若 discover 在间隙换代，必须基于新状态重新判断，不能使用阶段 1 的旧引用集合。
4. prune 阶段保持索引 R，逐仓库 account→repo 执行 snapshots/forget/prune，不持 catalog。索引 R 防 discover 重建引用与账号清空；计划锁防本计划备份/迁移增加同仓库快照引用。其他计划备份只发布自己仓库的新快照；protect 只改变保护位，其他 retain 只减少引用。仍按每仓库最新全局引用排除任何被引用的 snapshot，不把“不同计划”当作一定没有共享历史引用。
5. 完成物理清理后先释放所有 account/repo，才重新取 catalog。保持索引 R，重新读取仓库，基于真实处理过的 ID 发布完成目录，再用 updatePendingForget 的版本校验仅移除本任务处理的集合，保留期间新增的其他 pending ID。任何发布失败保留 pending，允许原有 retry。
6. `t.prunedSnapshots` 和 pending_removals 的快照不能跨阶段被当作完整仓库实体回写；最终目录取最新仓库/点状态组装，避免覆盖别的计划在 prune 期间完成的提交。
7. T14 的已删除清单清扫必须在计划、索引、catalog、账号/仓库锁全部释放后进行；不能在持索引 R 时触发它的 W，否则是锁升级死锁。

索引 W 的新短边界还有一个安全目的：已开始的 restore 必须先读完，才能提交使其快照可删除的决策。否则“restore 全程改 R、retain 完全不碰索引”仍可能让 prune 先删掉一个旧 restore 正准备读取的快照。

## 7. T14 接口与公共复用影响

T14 负责者确认的接口为 `cleanupFileEntries`→`deleteEntryBatches(ctx, query, args, lockIndex)`：每次 DELETE 前获取旧字符串锁，执行后释放；入口包括 Start 异步 orphan 清扫、execute 终态后的 task 清扫、retain defer 的 deleted-point 清扫。

T15 仅替换 `lockIndex=true` 分支为专用索引的 `LockContext(ctx)`/Unlock；原 SQL 每批重查 live point RunID 与非终态 task ID 的条件和 5000 行批大小保持不变。discover 未发布的 import-* 暂存清单继续由同一索引 W 保护。snapshot 校验临时表仍按 task/repo 独立清扫，不强行加入索引锁。

公共变化的复用范围已检索：

- engine 的 keyed 锁：backup、restore、check/prune/migrate/cleanupOld、cloud-connect；恢复索引读写调用另见上表。
- catalog：backup、retain、migrate/cleanupOld、protect、clearPreview、clearAccounts、discover。
- configuration：planSave、planDelete、protect、clearAccounts。
- Runtime account 配置：resources 中更新/删除账号，onedrive Drive 缓存/验证，account_transfer 导出读取/导入，rclone_import 凭据安装，以及 engine 的 clearPreview/clearAccounts。

默认路径保持旧阻塞锁方法可用；context 入口按需启用。取消失败路径必须归还部分获取的锁；异步队列中的任务使用 execute 派生的任务 ctx，不能改成 Background 逃避取消。新增实现只保护同步边界，不改变请求 JSON、null、数字序列化或数据库实体字段。

## 8. 实施后的验证与提交拆分

第一实现提交已执行：`go vet ./...`、`go build -buildvcs=false ./cmd/...`、`git diff --check`；启用真实 Restic 0.18.1 的 `go test -race ./...` 全部通过；context 原语、HTTP 门禁、资源锁取消与共享恢复读取的定向 race 测试使用 `-count=5` 通过。此时尚未执行整仓 `-count=5`，未启用真实 Rclone 传输测试；后者与阶段拆分一起在最终验收执行。

建议分三次提交，每次完成对应构建/检查后提交，不推送：

1. context 锁原语、catalog/配置门禁、专用恢复索引读写锁及 T14 对接；取消和读写互斥测试。
2. migrate 与 cleanupOld 的阶段拆分、CAS 重验和回滚；跨计划提交/锁顺序回归测试。
3. retain 分阶段发布与 prune、恢复安全边界；整合并发回归和最终 `go test -race -count=5 ./...`。

必须包含的可控并发测试：

- 假 rclone copy、假 restic read-data 阻塞期间，另一个计划的备份能取得 catalog 并完成元数据认证提交；脚本对“数据复制/校验”和“目录小文件发布”分别控制，不能把所有 rclone 命令一起暂停。
- migrate 复制结束、catalog 被另一协程占用时，复制阶段 account/repo 已释放；该 catalog 持有者能取得同一账号锁，证明不存在 account→catalog 倒序。
- 复制期间修改 repo 版本/位置、停用目标、改变 Drive 身份或从池移除目标，最终不发布新映射、不删除旧数据；已有成功/PROTECTION_DEGRADED/失败回滚路径分别验证。
- cleanupOld 校验与退休、purge 与完成发布之间发生映射/引用变更，不能 purge 当前位置，不能用旧实体覆盖新行。
- 长 prune 期间另一计划可提交目录；最终目录包含期间新增恢复点，且 pending_forget 清理不吞掉新加入 ID。prune/最终发布失败后仍可重试。
- restore 全程读锁期间，文件列表/恢复预览在无等待写者时正常返回；discover、T14 sweep、clear 的写者等待并在取消后退出；写者取消后浏览无需等待原 restore 结束。
- retain 删除决策等待已有 restore 结束，后续新 restore 拒绝已删除点；共享历史 snapshot 仍被任何有效点引用时不 forget。
- 分别占用 catalog、configuration、Runtime account 配置门禁，取消 protect/clearPreview 请求，handler 在测试超时内退出，不启动 commit、不遗留锁。
- T14 暂存 import 清单、非终态 task 清单、有效 point 清单保护，以及批间让出/取消后的再次获取，均继续通过。
- `go vet ./...`、`git diff --check`、前端 npm ci/build/test（若触及构建产物依赖）、真实本地引擎测试（工具可用时）、完整 race 连续五次。

## 9. 尚未验证的边界

本节记录整体方案的验证边界；具体提交实际执行的检查见交付记录。第一实现提交仍保留 maintenance 的长持 catalog 范围，完整阶段拆分及整仓 race 连续五次尚待后续提交与最终验收。真实 OneDrive 长复制/断网/并发重授权、Docker、生产规模大清单耗时均不能由假脚本测试替代。

写者排队对新浏览的公平调度、清空/discover 仍长持 catalog、元数据认证发布本身仍有网络等待，是本方案明确保留的限制。尤其 discover 的进一步分阶段不包含在此次实现中，避免把未经评审的目录换代协议一起改动。
