# 配置与运维

| 环境变量 | 默认值 | 含义 |
|---|---|---|
| VP_ADDR | 127.0.0.1:8080 | 服务监听；compose 中为 0.0.0.0:8080 |
| VP_BIND_IP | 127.0.0.1 | 仅用于 Compose 的宿主机监听 IP；设为 0.0.0.0 监听所有 IPv4 网卡，或填写实际网卡 IP；未设置或空值保留回环监听 |
| VP_PORT | 8080 | 仅用于 Compose 的宿主机端口；容器内仍为 8080 |
| VP_PUBLIC_URL | http://localhost:8080 | 对外 URL，Host 白名单、Cookie 安全与可选自定义应用回调依赖它；Rclone 标准回调不依赖此 URL |
| VP_ALLOWED_HOSTS | 空 | 额外允许的 Host，逗号分隔，主机名或主机名:端口，与请求 Host 完整匹配（忽略大小写） |
| VP_TRUSTED_PROXIES | 空 | 逗号分隔的可信代理 CIDR；仅这些代理可通过 X-Forwarded-For 提供客户端 IP，格式错误启动失败 |
| VP_STATE_DIR | ./state | SQLite、受保护恢复包、节点凭据密钥、排他锁 |
| VP_CACHE_DIR | ./cache | 引擎缓存与权限受控临时文件 |
| VP_SOURCE_ROOTS | /sources | 逗号分隔的允许源目录；生产只读绑定挂载 |
| VP_RESTORE_ROOTS | /restore | 逗号分隔的授权恢复根目录 |
| VP_RESTIC_BIN | restic | 引擎路径 |
| VP_RCLONE_BIN | rclone | 引擎路径 |
| VP_OAUTH_CLIENT_ID | 空 | 可选自定义微软应用 ID；标准 Rclone 授权留空 |
| VP_OAUTH_CLIENT_SECRET | 空 | 可选自定义 Web 应用 Secret；标准 Rclone 授权留空，不提交到 Git |
| VP_OAUTH_TENANT | consumers | 仅自定义应用的租户；默认 OneDrive Personal |
| VP_GLOBAL_CONCURRENCY | 2 | 全局后台任务数量上限 |
| VP_GDRIVE_TPS_LIMIT | 4 | 每个 Drive-only Rclone 进程的事务速率；0 关闭，范围 0.01–1000。混合 Drive/OneDrive 操作使用 Google remote 的 backend pacer，不将全局速率限制施加到 OneDrive。 |
| VP_GDRIVE_TPS_LIMIT_BURST | 1 | Drive 请求速率令牌桶突发值，范围 1–100。 |
| VP_LOG_MAX_ROWS | 100000 | 全局日志保留条数；批量修剪最多临时超出 127 行，单行最多 8 KiB |
| VP_CACHE_MAX_BYTES | 21474836480 | 缓存目录容量阈值，任务阶段边界检查，超限中止，不删除活动文件 |
| VP_MAINTENANCE_WINDOW | 空 | 检查、清理、迁移启动窗口，HH:MM-HH:MM，支持跨午夜；空为全天 |
| VP_MAINTENANCE_TIMEZONE | Asia/Shanghai | 维护窗口的 IANA 时区 |
| VP_AUTO_UNLOCK_FILE | 空 | 外部专用恢复密码文件，权限必须为 0600 |
| VP_ALLOW_LOCAL | false | 仅用于真实本地 Restic 联调的存储入口 |

管理员登录密码、库解锁恢复密码、恢复包保护密码各有用途。导出恢复包可以选择新的包保护密码，它不自动修改当前库重启解锁密码。手动模式重启后需网页解锁，自动模式在 compose 中增加只读 Secret 文件挂载后配置 `VP_AUTO_UNLOCK_FILE`。

账号容量从真实接口查询。云端安全预留、项目维护预留和预算均为 Bytes；界面使用 GiB / TiB。停用禁止新分配，不删除旧仓库。刷新配额失败或过期时任务应中止，不将空值视作无限空间。

只允许一个节点共享状态目录，并且 V1 要求同一备份库只有一个活跃写入节点。不要以相同恢复资料在两个服务器同时启用写入。异机恢复创建 recovery 模式节点，默认禁止备份、迁移和清理。

通过 [多节点密码连接](MULTI_NODE.md) 可以省去手动导入恢复包：A 升级保留原状态，B 使用独立全新状态目录，在首次设置选择「从云端连接」。B 接入原账号和备份密码后默认每分钟发现恢复点，重启后仍需解锁或配置上述自动解锁文件。

缓存阈值不是磁盘硬配额，大文件写入过程中可能暂时超过阈值。请为 state/cache 与恢复目标预留足够磁盘空间；需要硬上限时在宿主机设置文件系统配额。维护窗口限制任务启动，不在窗口结束时强行终止正在执行的任务。

Compose 的 `VP_UID`/`VP_GID` 默认均为 1000，`VP_RESTORE_HOST` 是宿主机目录，而网页使用对应容器路径 `/restore`。自定义恢复目录必须提前创建并授权给该运行用户；Docker 自动创建的宿主机目录可能属于 root。恢复预检查检查有效用户、补充组和 ACL 下的目录写入及进入权限，不会创建探测文件。出现 `RESTORE_TARGET_NOT_WRITABLE` 时按 [恢复目录权限排查](RECOVERY.md#恢复目录权限错误) 调整实际宿主机目录及挂载。

所有 API 与静态资源均校验 Host，未知主机返回 421。`localhost`、`127.0.0.1`、`[::1]` 允许任意端口；其他访问地址必须匹配 `VP_PUBLIC_URL` 的 Host 或 `VP_ALLOWED_HOSTS`，包含实际端口。局域网示例：`VP_ALLOWED_HOSTS=192.168.1.20:8080,nas.example:8080`。反向代理若保留原始 Host，需设置与之匹配的公开 URL 或白名单；若改写 Host，需允许实际传到上游的 Host。健康检查及本地脚本使用回环地址。

登录、初始化、解锁、云端密码、账号包分别按操作与客户端 IP 限制 15 分钟内 10 次尝试，成功验证清除对应计数。默认忽略 `X-Forwarded-For`，使用直连 IP。在反向代理之后部署时，配置 `VP_TRUSTED_PROXIES`，否则所有客户端共用同一个限流计数器。例如单一回环代理可配置 `127.0.0.1/32,::1/128`；容器代理应填写实际源地址网段。代理必须追加或重写真实客户端地址；服务从右往左跳过可信代理，取第一个非可信 IP，遇到无效链则退回直连 IP。不要信任所有网络或客户端可直达的网段。

小型认证请求（登录、解锁、登出、恢复包/账号包导出、云端密码）最大 8 KiB，初始化请求含恢复包最大 1 MiB。常规恢复包只包含库密钥与发现入口，实测 1000 个代表性入口的加密初始化请求约 234 KiB；发现入口、路径特别多的旧恢复包可能超过新上限，导入前需检查请求大小。恢复包加密格式未改变，离线工具仍保留原大小兼容。Argon2id 派生全局最多并发 2 次，超出的请求等待槽位。

## 升级

服务器专属配置放在被 Git 忽略的 `.env` 中，升级时保留原文件，不要用新版 `.env.example` 覆盖。端口通过 `VP_BIND_IP` 与 `VP_PORT` 配置，默认仍只监听宿主机回环地址；直接从外部访问时设置 `VP_BIND_IP=0.0.0.0` 或实际网卡 IP，同时核对 `VP_PUBLIC_URL` / `VP_ALLOWED_HOSTS`。应用的容器内监听地址与健康检查不变。

若 `git pull` 报告本地 `compose.yaml` 会被覆盖，先用 `git diff HEAD -- compose.yaml` 核对差异并将原文件备份到仓库外。旧部署若仅将 `127.0.0.1:${VP_PORT:-8080}:8080` 改成 `${VP_PORT:-8080}:8080`，可迁移为 `.env` 中的 `VP_BIND_IP=0.0.0.0`，保留所有 IPv4 网卡的监听；如还需 IPv6 访问，须另外核对发布地址。

迁移前必须确认拉取的目标版本已在 `compose.yaml` 的 `ports` 中引用 `VP_BIND_IP`，旧版本不会读取它。确认备份完成、该文件没有其他定制或暂存改动后，才能还原工作区文件并拉取支持该变量的版本：

```sh
git restore --source=HEAD --worktree -- compose.yaml
git pull --ff-only origin main
docker compose config --quiet
```

其他挂载、用户或环境变量定制必须逐项保留，不能直接还原；配置校验失败时先修正配置，不要停止旧服务。

日常升级先暂停计划，等待活动任务结束，并导出最新恢复包；确认工作区没有未处理的版本跟踪文件修改后执行：

```sh
git pull --ff-only origin main
docker compose config --quiet
docker compose build
```

以上每一步成功后再执行下一步。构建成功后记录当前容器的镜像 ID，并给旧镜像另加保留标签以便回退，再停止服务：

```sh
docker compose stop vaultpilot
```

停止后，备份完整 `state` 目录以及当前 `.env` 和部署配置到仓库外的受限目录，再启动新容器。不要在 SQLite 仍有写入时只复制 `.db` 而忽略 WAL；使用停机目录备份或 SQLite 在线备份。恢复包与云端目录提供异机恢复能力，`state` 备份只用于方便升级回退。旧版本可能无法读取升级后的凭据格式，回退时需同时恢复对应的状态备份，见 [数据库与迁移](MIGRATIONS.md)。

```sh
docker compose up -d --no-build vaultpilot
docker compose ps
docker compose logs --tail=100 vaultpilot
```

确认服务健康后在网页解锁（已配置自动解锁则无需此步），核对账号、备份源和恢复点，再恢复计划。

引擎版本固定在 Dockerfile。更改版本后先运行本地闭环，再用两个真实 OneDrive 重新执行云端验收。不要通过 `restic unlock --remove-all` 自动解除其他节点的锁。

## Rclone 标准授权与导入

默认无需设置 OAuth 客户端 ID、密钥或微软回调地址。使用内置 Rclone 引擎完成授权，账号的应用来源、令牌及刷新设置分别加密保存；全局自定义应用不会覆盖标准或导入账号。标准授权的 `common` 端点来自 Rclone，完成授权后平台仍仅接受全球区 Personal。

Rclone 授权监听进程内部的 `127.0.0.1:53682`。Docker 无需新增端口映射，远程访问使用网页回调粘贴入口。端口被其他 Rclone 授权占用时，先结束该授权后重试。服务器需可访问微软登录和 Graph 服务。共享应用可能受微软策略、配额或 Rclone 应用状态影响；可以导入已有授权或选择自己的应用，平台不会绕过微软同意页面。

导入上限 1 MiB，支持未加密 INI 配置、dump JSON、authorize token JSON 与其 Base64 结果。加密配置须在原电脑解密后导入。每次只选择一个 remote，忽略不用于凭据的选项，不执行配置中的命令、代理或任意端点。导入凭据到期后由 Rclone 使用原应用刷新；必须有 refresh_token。详见 [操作指南](RCLONE_AUTH.md)。
