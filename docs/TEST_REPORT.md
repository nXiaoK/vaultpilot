# 实现验证报告

测试日期：2026-10-10。执行主机为 macOS arm64；应用使用 Go 1.26.4、Node 20.20.2、Restic 0.18.1、Rclone 1.70.3。Docker 运行镜像为 Debian 12.12 Linux arm64。

**本地真实引擎闭环已通过；真实 OneDrive 云端验收尚未执行。** 本地目录作为两个独立存储入口，业务备份和恢复实际调用官方 Restic，不模拟上传成功。微软 OAuth 与 Graph 用可控 HTTP transport 测试，不能视为微软线上授权成功。没有测试用微软账号、应用注册或 5 TB 实际源数据，不能宣称所有 V1 发布门槛已完成。

## 已执行的验证

| 验证 | 结果与证据 |
|---|---|
| Go 编译、静态检查、竞态 | `go build ./cmd/vaultpilot`、`go build ./cmd/vprescue`、`go vet ./...`、`VP_TEST_RESTIC=/path/to/restic go test -race ./...` 通过，含真实 Restic 故障用例 |
| 前端 | `npm run build` 通过，包含 `vue-tsc` 与 Vite production build。浏览器实际登录、浏览服务器目录、保存源、查看总览通过；截图见 [overview.png](screenshots/overview.png) |
| Docker | 镜像构建通过；隔离容器以 UID 1000、只读根目录、只读源挂载、去除 capabilities、no-new-privileges 运行，初始化、会话、目录 API 与 healthcheck 通过 |
| 历史与异机恢复 | 单源约 40 MiB，大于任一账号 34 MiB 预算，实际跨两入口备份、修改/删除/重命名；删除原状态、缓存及源文件，新状态、新账号 ID 发现登记，恢复两份历史，逐文件 SHA-256 相等 |
| 增量、清理与迁移 | 未修改数据实际上传量低于首轮的一半；保护首点，清理未保护点；完整迁移并全量读数据校验，确认后删除旧副本，历史仍可恢复 |
| 迁移旧副本清理故障 | 当前/最后可用位置、过期路径、已删除位置请求拒绝；破坏新位置一个 pack 字节时全量检查失败，旧副本保留；恢复 pack 后才允许清旧 |
| 恢复安全与离线账号 | 单文件恢复、重复发现、已有内容保护、显式覆盖、一个本地入口不可用时部分恢复，均有真实结果 |
| 恢复中途失败报告 | 真实 Restic 快照按序恢复普通文件、危险外部链接和后续文件；已输出文件保留，链接拒绝、后续未尝试，任务 partial_failed 保留 RESTORE_UNSAFE_LINK，2 个已恢复条目（含源根目录）/1 个失败/1 个未尝试计数准确 |
| 独立人工恢复 | `vprescue` 不读取 SQLite/节点文件，以恢复包和 `.vpc`/`.ok` 解密登记；按完整 snapshot ID 用 Restic 恢复两份历史文件，SHA-256 核对通过。错误密码/篡改证书拒绝，输出 0600，临时明文 ZIP 删除 |
| 索引规模 | 实际插入 1,000,000 条 SQLite 文件行，7.065 秒；100 条索引页查询 0.154 毫秒；数据库 285,380,608 Bytes；Go heap 约 2.9 MB。逻辑 5,242,880,000,000 Bytes 是合成统计值，没有对应物理上传 |

索引测试只衡量该主机的一次数据库操作，Go heap 不是进程 RSS；不证明扫描、历史目录提交、网络吞吐或百万物理文件恢复的资源表现。

## 需求验收逐项状态

“本地通过”限定为真实本地 Restic；“部分验证”表示有代码与部分证据，但尚未达到该用例完整场景。

| 编号 | 状态 | 实际证据 / 未执行部分 |
|---|---|---|
| T01 多账号网页授权 | 未验证真实云端 | 网页向导和 state/PKCE 回调已实现；可控 transport 验证。待至少两个真实 OneDrive 网页授权及重新授权 |
| T02 元数据 Drive 入口 | 模拟接口通过 | 不默认选择 Drive；ODCMetadataArchive 读写验证拒绝。待真实微软列表回归 |
| T03 同 Drive 去重 | 模拟接口通过 | 稳定 Drive 身份去重及池校验测试。待真实共享配额域对账 |
| T04 大于单账号预算 | 本地通过 | 单源 41,943,109 Bytes，每个账号预算 35,651,584 Bytes，实际分配两账号并恢复一致；事务预留防超卖与复用仓库开销回归通过 |
| T05 单顶层目录拆组 | 本地通过 | 单源包含两个 20 MiB 文件，以文件集合拆仓，各仓仅含分配清单，组合恢复正确 |
| T06 修改/删除/重命名 | 本地通过 | 两份历史内容与应不存在路径分别核对，没有旧文件复活 |
| T07 未变更增量 | 本地通过 | 第三轮 Restic 实际 packed 上传统计低于首轮一半，允许快照与控制目录元数据开销 |
| T08 上传中满额 | 部分验证 | 容量公式与事务预留测试；账号不可访问、目录副本故障不发布完整成功。未执行真实云上传期间耗尽配额 |
| T09 源挂载丢失 | 本地检查通过 | 挂载 device/inode 身份、路径及链接边界测试；未执行宿主机实际卸载挂载点演练 |
| T10 源权限读取失败 | 部分验证 | 进程退出码 3 明确失败、扫描读取错误不静默跳过；真实同类型源变更拒绝提交。未执行真实非 root EACCES 的完整备份流程 |
| T11 上传时重启 | 部分验证 | pending 合并持久化重启一次、进程组取消、Rclone 刷新凭据崩溃后回收测试通过；未执行真实云大文件上传中强制杀容器 |
| T12 控制目录副本不足 | 本地故障通过 | Restic 产生快照但所需 catalog 副本不足，发布恢复点数为零；完成标记冗余不足走保护降级，不显示完整成功 |
| T13 丢失全部原状态 | 本地通过 | 原状态/缓存/源确实删除，恢复包、新账号 ID、重新发现及历史 SHA-256 恢复；待全新真实服务器+OneDrive 演练 |
| T14 单文件/子目录恢复 | 本地通过 | 单文件实际 restore `--include`；子目录选择边界单元测试。未做大库子目录网络下载计量 |
| T15 账号离线 | 本地通过 | 移走一个实际存储入口，显式部分恢复为 partial_failed，有真实恢复/缺失计数 |
| T16 错误或篡改恢复资料 | 通过 | 恢复包密码/认证测试，独立工具错误密码与被篡改 `.ok` 拒绝，无失败输出目录 |
| T17 恢复冲突 | 本地通过 | 默认冲突保留已有内容；显式覆盖成功；读取失败原文件保留；链接父目录不越界 |
| T18 历史清理 | 本地通过 | 保护点不进删除集合，受控 forget/prune 后保留点及历史内容可恢复。V1 保留策略经预览后手动确认执行 |
| T19 迁移中断 | 部分验证 | 迁移复制成功保留旧位置，校验故障拒绝删旧；未在实际网络复制中间强制断进程 |
| T20 新增仓库发现 | 本地通过 | 两仓库时导出恢复包，之后加 5 MiB 文件形成第三仓库；没有重新导出包，新状态从最新云端登记取得新仓库密钥并 SHA-256 恢复新增文件 |
| T21 特殊文件名/目录/链接 | 本地通过 | 中文、空格、换行、空目录、相对符号链接保存恢复；恢复目标链接越界拒绝；其他元数据范围见 LIMITATIONS.md |
| T22 百万/多 TB/10 账号 | 部分验证 | 100 万实际 SQLite 索引行通过。百万物理文件、5 TB 实际数据、10 个真实 OneDrive、云端限流和吞吐未验证 |

## 可复现命令

先构建后端与前端，将官方引擎路径换成测试机实际路径；所有测试均使用隔离测试目录。

```sh
go build -o bin/vaultpilot ./cmd/vaultpilot
go build -o bin/vprescue ./cmd/vprescue
go vet ./...
VP_TEST_RESTIC=/path/to/restic go test -race ./...
python3 scripts/e2e_local.py --extended --restic /path/to/restic --rclone /path/to/rclone
python3 scripts/e2e_rescue.py test-artifacts/e2e-<该轮时间> --restic /path/to/restic
VP_SCALE_TEST=1 go test ./internal/store -run TestMillionEntryIndex -v
docker compose build
python3 scripts/smoke_docker.py
```

脚本会输出真实报告路径。`test-artifacts/` 包含测试用恢复包、明文恢复结果与临时状态，默认被 Git 忽略；请勿公开整个目录。可分享的无凭据报告副本见 [Go 验证](evidence/go-validation.json)、[本地闭环](evidence/local-e2e-report.json)、[离线救援](evidence/offline-rescue-report.json)、[容器](evidence/docker-report.json)、[百万索引](evidence/scale-report.json)。

## 真实云端验收方法

在 Debian 12.12 新部署上使用 Rclone 标准授权或导入已有配置（也可配置自定义微软应用），通过可信 HTTPS/localhost 页面，使用两个真实 OneDrive Personal 执行 T01–T21。先用小规模数据完成首备、修改历史、全量数据检查、导出恢复包；停止原服务，另一个服务器用全新状态和新的 OAuth 授权恢复，并 SHA-256 核对。补充 429 Retry-After、真实配额耗尽、断网、上传中重启、迁移中断注入，再开展 10 账号/百万文件/5 TB 测试，记录 RSS、缓存峰值、扫描/提交耗时、请求次数、带宽与恢复结果。通过后才扩大正式备份规模。

## Rclone 标准授权与导入增补（2026-10-10）

新增功能默认不要求 Azure 或微软应用注册。网页提供标准授权、远程 localhost 回调粘贴、取消、配置文件上传和文本导入；自定义应用保留为高级入口。

- Go race 全套、go vet、后端构建与 Vue 类型检查/构建通过。
- 导入覆盖 INI、dump JSON、Token JSON、Base64 和输出标记段，拒绝加密文件、重复字段、非微软授权端点、非全球区域和不支持的 Drive 类型。未知运行选项不进入引擎配置。
- 验证预览/账号 API 不泄露 Token，持久化认证加密，CSRF、显式 remote 选择、乐观版本冲突、跨重启保留应用来源，以及旧任务刷新不会覆盖新授权。
- 覆盖原自定义应用刷新与导入授权刷新协议。后者使用命令协议夹具模拟 Token 轮换，**不等于真实微软长期刷新验收**。
- 真实 Rclone 1.70.3 完成标准授权进程启动、官方共享应用链接/回调检查、取消、远程拒绝回调以及配置重写后保留授权版本标记。微软登录及成功同意没有代替用户执行。此测试需独占本机 53682 端口，避免与网页授权测试同时运行。
- 实际浏览器验证空自定义 OAuth 配置时可创建账号，标准链接出现、错误回调被拒绝、取消成功；文件上传解析预览、显式选择控制及加密配置拒绝提示正常。截图见 [授权指南](RCLONE_AUTH.md)。
- 真实本地扩展 E2E 继续通过：历史版本、彻底移除原状态后的发现/恢复、SHA-256、受控清理、迁移、冲突和部分恢复。该测试是本地存储入口，不是 OneDrive。
- 最终 Debian 12 非 root 只读容器通过标准授权与远程拒绝回调，无需映射 53682；导入协议使用明确的测试凭据，未执行真实云盘操作。

增补测试证据见 [Rclone 功能验证](evidence/rclone-validation.json)、[本地回归](evidence/rclone-local-regression.json) 和 [容器回归](evidence/rclone-docker-regression.json)。真实微软账号的成功授权、Drive 读写及长期刷新仍待用户环境验收。

```sh
VP_TEST_RCLONE=/path/to/rclone go test -race ./internal/platform -run TestRealRclone -v
```

## 2026-10-11 账号身份与加密迁移

- 授权步骤连接线构建通过；浏览器检查桌面和 390px 窄屏下步骤布局，Drive 同 ID 别名合并，微软邮箱显示。
- `go test -race ./...` 全部通过（包含真实本地 Restic/Rclone 引擎回归）；`npm test` 5 项通过，Vue/TypeScript/Vite 生产构建通过，OpenAPI YAML 解析通过。
- 新增账号身份测试覆盖客户端伪造邮箱拒绝、可选 /me 权限失败、身份更新版本、重新授权清空旧身份。
- 加密迁移测试覆盖 A/B 独立节点密钥、Rclone 和 custom 原应用参数保留、无明文凭据泄露、待验证状态、未知选择原子拒绝、重复导入跳过、恢复节点只读验证、错误密码/篡改/恶意 KDF/租户及配置行注入拒绝、恢复包域隔离。
- 浏览器用自建虚拟账号走通导出选择/密码不一致提示/导出完成提示，以及加密文件上传 → 预览 → B 两账号导入并自动验证完成；云端由模拟 Microsoft Graph 响应提供，未使用真实个人 OneDrive Token。浏览器下载事件的文件落地未由工具确认，导出 API 的完整加密响应由集成测试确认。
- 临时 QA 服务曾因工具等待超过服务生命周期而中断，重新启动后导入全流程完成；这不是功能回归失败。QA 服务、临时测试代码及虚拟凭据文件在完成后清理。

界面截图：[步骤连线](screenshots/account-stepper.png)、[B 账号导入完成](screenshots/account-import.png)。真实 Microsoft 账号跨机器与长期 Token 刷新仍需部署后演练。
