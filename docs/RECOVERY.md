# 恢复手册

## 使用相同备份密码连接其他节点

A 开启「多节点密码连接」后，B 首次选择「从云端连接」，重新授权原 OneDrive 账号并输入 A 的原备份密码即可接入，无需手动传递恢复包。B 默认每分钟发现新恢复点，恢复文件仍需选择目标并执行。支持一个备份节点和多个恢复节点。具体步骤、路径和自动发现边界见 [多节点指南](MULTI_NODE.md)。下文保留离线恢复包接入与人工救援流程。

## 正常网页恢复

在恢复中心选择已完整提交的恢复点，浏览、搜索或选择整个 source_id / 子目录 / 文件。目标必须在部署授权的可写恢复根目录下；默认选择新目录，不覆盖已有文件。先查看容量、冲突和账号缺失范围，再启动异步恢复。恢复后的文件按稳定 source_id 建立根目录，源服务器原绝对路径不参与新路径映射。

备份完成仅证明本次快照与控制目录提交条件满足；仓库结构校验、数据抽查、全量数据读取校验是独立维护操作。恢复任务使用 Restic 的读回校验并报告结果。

## 原服务器损坏后的空白实例

1. 在新服务器准备全新状态目录与可写恢复目录，部署相同或兼容的版本。
2. 首次启动选择“连接已有备份库并恢复”，导入离线恢复包，并输入该包保护密码。此流程不初始化或覆盖旧云端仓库。
3. 重新添加并授权所需 OneDrive，显式选择与恢复资料匹配的原 Drive。新的账号 ID、Rclone remote、主机名和容器路径可以不同。
4. 执行“发现云端控制目录”，验证加密提交并重建本地索引。
5. 选择历史恢复点、范围和新目标，查看冲突、缺失和容量预检后恢复；核对关键文件内容。

恢复节点默认禁止备份、清理和迁移。不要把同一备份库同时连接成两个活跃写入节点。恢复包保存的是发现与解锁能力，不包含长期 OneDrive Token；新增仓库会自动登记到新的加密控制目录，无需每个仓库单独重新保存密码。

恢复包错误密码、篡改或不支持版本直接拒绝。若所有旧发现入口将失效、密钥或格式将改变，必须在旧入口仍可用时保存新的恢复包，并做空白实例验证。

## 恢复目录权限错误

`mkdirat <source_id>: permission denied` 表示进程无法在恢复目标中创建备份源子目录，不表示 OneDrive 授权失效。Compose 默认使用 `VP_UID=1000`、`VP_GID=1000`；宿主机管理员能写入该目录，不代表容器用户也能写入。新版本会在预检查阶段检查目标和已有父目录的写入、进入权限，返回 `RESTORE_TARGET_NOT_WRITABLE`，并显示实际进程 UID/GID；执行阶段的权限变化也使用该错误码。

在 B 机器项目目录中查看实际容器身份与挂载：

```sh
docker compose exec vaultpilot id
docker compose exec vaultpilot ls -ldn /restore
```

例如 `.env` 配置 `VP_RESTORE_HOST=/data/from-a`、`VP_UID=1000`、`VP_GID=1000`，使用专用恢复目录时，在宿主机准备它：

```sh
sudo mkdir -p /data/from-a
sudo chown 1000:1000 /data/from-a
sudo chmod u+rwx /data/from-a
docker compose up -d --force-recreate
```

UID/GID 和目录必须替换为实际设置；上述操作只调整该专用目录，不递归修改已有文件。网页填写容器路径 `/restore/first-restore`，宿主机对应 `/data/from-a/first-restore/<source_id>/...`。如果已有恢复子目录属于其他用户，可另选新的目标子目录，或为实际目录设置正确的权限。更改 `.env` 的路径或 UID/GID 后必须重建容器，单独 `restart` 不会应用新配置；更改运行 UID/GID 时还需确保该用户能够写入 state/cache。

若仍失败，检查宿主机文件系统或挂载是否只读，以及 NFS/CIFS/NAS 的 UID 映射、ACL 或访问策略。应用不会自动更改宿主机属主或放宽权限。

## 管理服务不可用时的人工恢复

独立 `vprescue` 工具不需要 SQLite、节点密钥或正在运行的管理服务。应和离线恢复包一起保存对应版本的工具/源码以及 Restic、Rclone 的版本信息。

1. 用 Rclone 完成新节点 OneDrive 授权，并明确配置原来的 Drive ID。查看恢复包中的公开发现规则：项目目录为 `VaultPilot/<vault_id>/catalog/`。
2. 下载一对同 ID 的 `<commit_id>.vpc` 和 `<commit_id>.ok`。只使用有完成标记的版本，不使用只有 `.vpc` 的未完成提交。选择最新认证提交，若需要可以退回较早版本。
3. 创建权限为 0600 的专用密码文件，不把密码写到命令行、日志或 shell 历史。编译工具：`go build -o bin/vprescue ./cmd/vprescue`。

```sh
vprescue -kit recovery-kit.json -catalog downloaded.vpc -commit downloaded.ok \
  -password-file /secure/recovery-password -out /secure/decrypted-register
```

工具校验恢复包认证与库 ID，解密输出 `control.json`、`points/<point_id>.jsonl` 与 `<repository_id>.password`（0600）。输出内容是敏感明文，必须保护。每个点的文件清单有 source_id、相对路径、repository_id 和该历史快照内的 snapshot_path；`control.json` 的 points 指定完整 snapshot_id，repositories 的 locations 给出稳定 Drive ID 和仓库路径。

4. 使用已重新授权的 remote 和指定仓库密码文件操作 Restic，不使用 `latest`：

```sh
RESTIC_PASSWORD_FILE=/secure/decrypted-register/<repository_id>.password \
  restic -r rclone:<new_remote>:<repository_path> snapshots
RESTIC_PASSWORD_FILE=/secure/decrypted-register/<repository_id>.password \
  restic -r rclone:<new_remote>:<repository_path> restore <full_snapshot_id> \
  --target /new-empty-output --include '<snapshot_path>' --verify
```

5. 将各分片的结果按照该恢复点 JSONL 的 source_id 和相对路径重新组合。人工模式需要恢复到全新目录、检查路径和符号链接，并禁止用当前分配表解释旧点。空目录元数据位于快照内的专用 metadata 路径，由清单标明。

人工流程中的 Rclone config 及解密登记包含敏感资料。恢复完成后关闭工具、保护或处理这些明文文件，不把它们上传回业务仓库或提交到版本库。

## 恢复验证与演练

至少定期恢复若干文件并计算 SHA-256；重大升级、扩容和迁移后执行空白实例演练。不能仅凭上传成功、仓库存在或 `restic check` 普通结构检查宣称所有业务数据均已全量读回。
