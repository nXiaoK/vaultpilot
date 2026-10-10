# VaultPilot

中文多账号 OneDrive 加密备份管理平台。技术栈：Go、Vue 3 / TypeScript、SQLite、Restic 0.18.1、Rclone 1.70.3。一个独立 Restic 仓库完整存放在一个 Drive 中，按稳定文件集合分配容量；多账号容量分摊不提供业务数据多副本。

项目按 [需求规格](多账号%20OneDrive%20加密备份平台.md) 开发。实现与验证范围以 [测试报告](docs/TEST_REPORT.md) 和 [已知限制](docs/LIMITATIONS.md) 为准。真实 OneDrive Personal、多 TB 和百万文件的测试需要对应环境；本地引擎测试不能替代云端验收。

## Debian 12 / Docker Compose

需要 Docker Engine、Compose 插件、至少两个可参与存储池的 OneDrive 账号，以及微软应用注册信息。公开访问请在 HTTPS 反向代理后部署。不要把宿主机根目录或 Docker Socket 挂载给应用。

```sh
git clone https://github.com/nXiaoK/vaultpilot.git
cd vaultpilot
cp .env.example .env
# 编辑 .env：真实源目录、恢复目录、运行 UID/GID 和 OAuth 配置。
mkdir -p state cache example-data/source example-data/restore
# 让 .env 中指定的 UID/GID 能写入 state/cache/恢复目标，能读取备份源。
sudo chown -R 1000:1000 state cache example-data/restore
chmod 700 state cache
docker compose build
docker compose up -d
```

默认仅监听宿主机 `127.0.0.1:8080`。访问 `http://localhost:8080`，创建管理员和独立恢复密码，添加账号并逐一授权、明确选择 Drive、完成读写验证，然后创建存储池、备份源和计划。首轮备份后立即导出并离线保存恢复包，执行一次恢复演练后再扩大规模。

在 VPS 部署时，可以在自己的电脑运行 `ssh -L 8080:127.0.0.1:8080 <用户>@<VPS地址>`，再从浏览器访问 `http://localhost:8080`；此时 OAuth 的公开 URL 与微软注册回调也应使用对应的 localhost 地址。使用域名访问时请配置 HTTPS 反向代理，再修改 `VP_PUBLIC_URL` 与微软回调地址。

生产环境替换 `.env` 的 `VP_SOURCE_HOST` 与 `VP_RESTORE_HOST`。源以只读方式挂载到 `/sources`；恢复目标独立挂载到 `/restore`。新增宿主机目录需要更新 compose 的只读挂载和 `VP_SOURCE_ROOTS`，重建容器后才会出现在网页选择器中。

## 微软应用注册

在 Microsoft Entra 应用注册中选择适合个人账号的账户类型，添加 Web 回调：`<VP_PUBLIC_URL>/api/oauth/callback`，填写 Client ID 和适用的 Client Secret。应用使用授权码、state、PKCE 和离线权限。公开 URL、注册回调与实际访问地址必须一致；开发环境可使用微软允许的 localhost 回调。

网页授权结束后必须手动选择 Drive。授权成功不代表该 Drive 通过备份访问验证；例如 `ODCMetadataArchive` 不能被自动当成业务存储。企业版及 SharePoint 未经真实验收不承诺支持。

## 开发与本地验证

需要 Go 1.26、Node 20.20+、Restic 和 Rclone。可以把引擎安装到 PATH，或通过环境变量指定绝对路径。

```sh
cd web
npm ci
npm run build
cd ..
go build -o bin/vaultpilot ./cmd/vaultpilot
go test -race ./...
VP_RESTIC_BIN=/path/to/restic VP_RCLONE_BIN=/path/to/rclone ./bin/vaultpilot
```

API 同源提供编译后的 `web/dist`。开发前端可单独运行 `npm run dev`，Vite 将 `/api` 代理到 Go 服务。

本地真实引擎闭环（两个文件目录模拟存储入口，并非 OneDrive）：

```sh
python3 scripts/e2e_local.py --extended --restic /path/to/restic --rclone /path/to/rclone
```

此脚本会创建隔离测试源、执行加密备份、修改删除与重命名，再彻底移除原数据库/缓存和业务源，以恢复包在新状态目录发现登记并恢复新旧版本，核对 SHA-256。测试专用本地账号仅在 `VP_ALLOW_LOCAL=true` 时开放，生产 compose 默认关闭。

扩展用例包括全局历史清理、整仓迁移与旧副本安全清理、覆盖冲突、离线账号部分恢复。随后可用 `python3 scripts/e2e_rescue.py test-artifacts/e2e-<该轮时间> --restic /path/to/restic` 验证独立工具人工恢复；`python3 scripts/smoke_docker.py` 验证隔离非 root 容器。

## 界面预览

以下截图来自实际初始化后的本地测试实例，尚未连接云账号；业务数据由 API 返回。

![VaultPilot 总览](docs/screenshots/overview.png)

## 文档

- [架构决策](docs/ARCHITECTURE.md)、[任务状态机](docs/STATE_MACHINE.md)、[恢复协议](docs/RECOVERY_PROTOCOL.md)
- [部署与配置](docs/CONFIGURATION.md)、[恢复手册](docs/RECOVERY.md)、[安全边界](docs/SECURITY.md)
- [API](docs/API.md)、[OpenAPI](docs/openapi.yaml)、[数据库迁移](docs/MIGRATIONS.md)
- [验收计划](docs/ACCEPTANCE.md)、[测试报告](docs/TEST_REPORT.md)、[已知限制](docs/LIMITATIONS.md)
