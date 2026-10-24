# OneDrive / Google Drive 可视化授权与配置导入

Microsoft 365 Family 属于个人订阅，可以使用 Rclone 自带的微软应用连接个人 OneDrive。默认不需要 Azure 账号，也不需要填写客户端 ID、密钥或自行注册回调地址。

以下两种方式先介绍 OneDrive；Google Drive 的差异见后面的专节。

## OneDrive 方式一：在网页中使用标准授权

1. 打开「存储账号」，点击「添加账号」，填写便于辨认的名称并继续。
2. 保持默认「Rclone 标准授权」，点击「开始 Rclone 标准授权」。服务器启动实际 Rclone，界面出现微软授权按钮。
3. 点击「打开微软授权页面」，在新页中选择对应的个人微软账号，检查权限并同意。Family 中各成员要分别使用自己的微软账号授权。
4. 微软跳转到 `http://localhost:53682/`。如果 VaultPilot 直接运行在这台电脑上，通常自动完成；返回原页即可。
5. 远程服务器或 Docker 部署时，浏览器可能显示“无法访问此网站”。保留这一页，从地址栏复制包含 `code` 和 `state` 的完整 URL，回到原页，粘贴到「授权后的完整回调地址」并提交。此步骤不需要在服务器公开 53682 端口。
6. 平台加密保存授权结果并读取真实 Drive 列表。手动选择 Personal Drive，设置容量预算，点击「验证访问并保存预算」。只有实际读写和配额验证通过后，账号才能加入存储池。

每次授权有效 10 分钟，同一实例同时仅支持一个标准授权。可随时取消并重新开始；关闭对话框会请求取消。浏览器异常关闭或断网时，服务端最多等待到授权过期。端口占用、网络不可达、用户拒绝、微软限制或授权过期时，页面显示失败，可重新尝试或导入已有配置。不要把回调地址发给他人。

## OneDrive 方式二：导入已有 Rclone 配置

1. 创建账号并继续，切换到「导入 Rclone 配置」。
2. 上传未加密的 `rclone.conf` 或 dump JSON，也可以粘贴授权结果。上限 1 MiB。
3. 点击「解析并预览可导入账号」。列表仅显示名称、应用来源和 Drive 参考信息，不回传 Token。
4. 显式选择一个 remote，点击「加密保存并读取 Drive」。一份多账号配置可重复操作，每次对应一个平台账号。
5. 从微软返回的真实列表选择 Drive 并完成读写验证。导入文件中的 Drive ID 不会自动成为已验证身份。

支持以下内容：

- 未加密 INI 配置：包含 `[名称]`、`type = onedrive` 与 `token = {...}`。已有自定义应用的 `client_id`、obscure 格式的 `client_secret` 和受支持刷新设置会与 Token 一起加密保存。
- `rclone config dump` 的 JSON 输出。
- `rclone authorize onedrive` 输出中的 Token JSON，或官方 Base64 配置授权结果；可以粘贴完整的输出标记段。

没有现成配置、又希望在本机 Rclone 中授权时，在有浏览器的电脑安装 Rclone，并运行 `rclone authorize onedrive`，将输出粘贴到上述网页入口。默认网页标准授权无需执行这条命令。

配置须包含可刷新的 `refresh_token`、Bearer Token 类型和过期时间。到期后由服务器上的 Rclone 使用该账号原有应用刷新。加密的 Rclone 配置需先在原电脑解密；入口不索要其加密密码。只支持全球区 Personal，暂不接入 Business、SharePoint 或其他区域。无关的配置选项不会用于执行任意命令或请求任意端点。

## Google Drive（Google One）

仅接入个人 Google 账号的“我的云端硬盘”，对应平台 `backend = gdrive`、Rclone `type = drive`。Google One 是该账号的容量订阅，不需要另建 remote 类型。

1. 添加账号时选择「Google Drive（Google One）」。共享同一 Google One 家庭容量的成员填写相同的可选「共享存储组」（去除首尾空白后最多 64 字符），同组账号不能加入同一池。以后可在账号设置中修改或清空；修改不能绕过已有池的共享容量冲突检查。
2. 点击 Rclone 标准授权，打开 Google 授权页面，选择账号并同意。标准流程使用 `prompt=consent select_account`，重新授权也要求同意，避免 Google 不返回 `refresh_token`。
3. Google 跳转到 **`http://127.0.0.1:53682/`**。VPS / Docker 部署时，复制地址栏完整回调 URL 并粘贴提交；无需公开服务器 53682 端口。不要使用微软网页回调 `/api/oauth/callback`。
4. 读取列表后显式选择唯一的“我的云端硬盘”，设置预算并验证。身份和容量来自 Google Drive REST v3。Google 未返回容量上限时会拒绝验证，不把未知空间视为无限。

导入可使用未加密的 `type = drive` 配置、`rclone config dump` JSON 或 **`rclone authorize drive`** 的结果。预览按当前账号的 provider 解析，仍须显式选择 remote。必须有可刷新的 `refresh_token`；自行运行 authorize 时若重新授权结果没有它，可使用网页标准授权重新取得。`root_folder_id` 只作预览参考，不能代替从 Google 返回的真实根目录身份与访问验证。

Google 导入边界：

- `scope` 只能为空或 `drive`，统一保存为 `drive`。`drive.file`、只读 scope 及多个 scope 均拒绝；整个 Drive 读写权限意味着切换应用后仍能看到原备份目录。
- 不支持 `team_drive`、`service_account_file`、`service_account_credentials`、`impersonate`、`resource_key`；拒绝 `env_auth = true` 与 `shared_with_me = true`。Workspace 与 Shared Drives 不在支持范围。
- `auth_url` 只接受默认 `https://accounts.google.com/o/oauth2/auth`，`token_url` 只接受默认 `https://oauth2.googleapis.com/token`，均可留空。
- Token 与自定义 `client_id` / 原始 `client_secret` 按白名单加密保存。标准 Google 配置中的 `client_secret` 是 Rclone 直接使用的敏感字符串，不要把它转换成 obscure 值；导入的 `use_trash`、`stop_on_upload_limit`、`chunk_size` 等运行设置不会覆盖平台策略；生成的所有 Google remote 固定 `scope = drive`、`use_trash = false`、`stop_on_upload_limit = true`。

**永久删除：Google Drive 中的备份被 prune、历史清理、迁移旧副本清理或批量清空删除后，不进入回收站，无法找回。** 该配置仅用于 VaultPilot 自己的 remote 与项目目录；Google 网页及其他工具遵循它们各自的删除设置。Google 同名目录可能有多个，平台遇到重复 `VaultPilot` 或库目录会拒绝继续，需在 Google Drive 手动核对处理，不会自动合并备份。

自建 Google OAuth 客户端现在可在 Google 授权向导中按账号使用，也可通过已有 Rclone 配置导入；类型使用“桌面应用”。向导要求填写 Google 提供的原始 Client Secret，不是 obscure 值。OAuth 同意屏幕须发布为 **In production**，处于 Testing 的刷新令牌通常 7 天过期。该发布状态与 Google 的审核状态不同，未审核应用仍可能显示提示。自建客户端仅按当前账号保存，不通过 Google client 环境变量配置。首次未配置自有客户端的账号继续使用 Rclone 内置共享 client；已有自建客户端再次授权时会沿用，不能静默切回共享应用。

Google 账号授权成功、重复授权取得 refresh token、过期刷新并跨重启保留、读写及混合云盘恢复均需真实账号验收；当前未验证。详见 [验收计划](ACCEPTANCE.md)。

## 已有账号重新授权

在已有账号的连接入口重新标准授权或导入即可。重新授权后会要求再次验证；已有账号必须选择原 Drive，以保护历史仓库的位置身份。连接其他 Family 成员或 Google 账号的 Drive，应添加新的账号。重新授权不会删除云端备份。

## 可选自定义应用

已有微软应用的部署者仍可设置 `VP_OAUTH_CLIENT_ID`、`VP_OAUTH_CLIENT_SECRET`、`VP_PUBLIC_URL` 和 `VP_OAUTH_TENANT`。配置后，授权界面的「高级」选项提供原网页授权方式，其回调是 `<VP_PUBLIC_URL>/api/oauth/callback`。该路径与 Rclone 标准授权独立，不是默认方案的前置条件。

## 界面示例

以下截图来自隔离测试节点，未完成真实微软账号授权。

![标准授权入口](screenshots/rclone-authorize.png)

![配置导入入口](screenshots/rclone-import.png)

## 微软账号身份

存储卡片保留自定义名称，并显示 Microsoft Graph 返回的邮箱或显示名。已有账号可点击“重新授权 / 验证”读取账号身份，或刷新配额读取 Drive 所有者信息；不必重新登录微软。缺少 User.Read 权限时身份读取失败不阻断 Drive 选择，未提供邮箱时显示名称或身份未读取提示。重新授权会清空旧身份，避免误标。相同稳定 Drive ID 的多个别名合并为一个选择项，优先显示 OneDrive 名称。
