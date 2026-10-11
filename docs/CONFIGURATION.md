# 配置与运维

| 环境变量 | 默认值 | 含义 |
|---|---|---|
| VP_ADDR | 127.0.0.1:8080 | 服务监听；compose 中为 0.0.0.0:8080 |
| VP_PUBLIC_URL | http://localhost:8080 | 对外 URL，Cookie 安全与可选自定义应用回调依赖它；Rclone 标准回调不依赖此 URL |
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

## 升级

先停止任务并导出最新恢复包，备份 `state`，记录当前镜像版本，然后停止旧容器、构建新镜像、启动。不要在 SQLite 仍有写入时只复制 `.db` 而忽略 WAL；使用停机目录备份或 SQLite 在线备份。恢复包与云端目录提供异机恢复能力，`state` 备份只用于方便升级回退。

引擎版本固定在 Dockerfile。更改版本后先运行本地闭环，再用两个真实 OneDrive 重新执行云端验收。不要通过 `restic unlock --remove-all` 自动解除其他节点的锁。

## Rclone 标准授权与导入

默认无需设置 OAuth 客户端 ID、密钥或微软回调地址。使用内置 Rclone 引擎完成授权，账号的应用来源、令牌及刷新设置分别加密保存；全局自定义应用不会覆盖标准或导入账号。标准授权的 `common` 端点来自 Rclone，完成授权后平台仍仅接受全球区 Personal。

Rclone 授权监听进程内部的 `127.0.0.1:53682`。Docker 无需新增端口映射，远程访问使用网页回调粘贴入口。端口被其他 Rclone 授权占用时，先结束该授权后重试。服务器需可访问微软登录和 Graph 服务。共享应用可能受微软策略、配额或 Rclone 应用状态影响；可以导入已有授权或选择自己的应用，平台不会绕过微软同意页面。

导入上限 1 MiB，支持未加密 INI 配置、dump JSON、authorize token JSON 与其 Base64 结果。加密配置须在原电脑解密后导入。每次只选择一个 remote，忽略不用于凭据的选项，不执行配置中的命令、代理或任意端点。导入凭据到期后由 Rclone 使用原应用刷新；必须有 refresh_token。详见 [操作指南](RCLONE_AUTH.md)。
