# 个人邮件 MCP

[English](README.md) · 简体中文

自托管的多账号邮件 MCP。受信任的 agent 连接 Streamable HTTP `/mcp`，用账号别名查找、整理和发送邮件；邮箱凭据和 Outlook 刷新令牌不交给 agent。不需要数据库、后台同步或 Tailscale。

## Linux 部署

用 Go 1.26 或更新版本执行 `make binaries`，从 `dist/` 取适合服务器的 `personal-mail-mcp-linux-amd64` 或 `personal-mail-mcp-linux-arm64`。服务器需要系统 CA 证书包，用于校验上游 IMAP、SMTP 和 Microsoft OAuth 的 TLS 证书。将二进制、示例配置和密钥放在服务账户有权读取的目录中：

```sh
cp config.example.json config.json
mkdir -p secrets
chmod 700 secrets
(umask 077; openssl rand -hex 32 > secrets/mcp-token)
```

修改 `config.json` 中的账号和网卡。把 Gmail 应用密码写入 `secrets/personal-gmail`（单行，移除 Google 展示时为便于阅读添加的空格），设置文件权限为 `0600`。不要把密钥放进命令行参数、版本库或代理日志。

```sh
./personal-mail-mcp-linux-amd64 -config config.json serve
```

配置中的文件路径相对于配置文件所在目录解析。`interfaces` 是一个或多个网卡名，Linux 上用 `ip -brief address` 查看。服务启动时绑定所选网卡已有的 IPv4/IPv6 单播地址，不监听通配地址；网卡缺失、没有可用地址或端口被占用都会让启动失败。地址变化后需重启。绑定的是本机目标地址，不是入站流量的防火墙规则；需要限制实际入站网卡时，请另外设置防火墙。

**服务不提供 HTTPS。** 示例绑定 `lo`，只接受本机连接。远程访问建议由 HTTPS 反向代理转发到 `http://127.0.0.1:8080/mcp`，保留 `Authorization`。默认情况下，MCP SDK 只接受回环连接上的回环 `Host`，因此代理可将上游 `Host` 设置为 `127.0.0.1:8080`。不要把明文入口直接暴露到不可信网络。**同一个密钥对实例内所有配置账号都有读取、修改、下载附件和发信权限。** 重新部署时生成新密钥，只交给受信任的客户端；不要复用只读部署的密钥。

如果 HTTPS 代理保留客户端使用的域名（例如 Tailscale Serve），可在 `config.json` 中增加 `"additional_allowed_hosts": ["mail.example.test"]`，并保持服务绑定 `lo`。只填 DNS 主机名，不填协议、路径、端口、末尾的点或通配符。请求按完整主机名匹配，不区分大小写；可带有效端口。该选项只影响回环连接，不信任 `X-Forwarded-Host`，也不替代 Bearer 认证或代理的访问策略。

## 邮箱配置

在 `accounts` 中为每个邮箱添加独立别名；同一提供商可以配置多个账号。单账号可从 [`config.example.json`](config.example.json) 开始，也可参考 [`config.multi-account.example.json`](config.multi-account.example.json) 中 Gmail、Google Workspace、Yahoo、QQ 和 Outlook 共存的完整示例。

### 添加和验证账号

1. 首次部署可以把示例复制为 `config.json`；已有部署只需把需要的条目合并到现有的 `accounts`，不要覆盖其他账号或监听配置。替换虚构的邮箱地址，删除不用的条目。同一提供商的多个邮箱使用不同的别名和凭据文件。
2. 在对应邮箱中生成凭据：Gmail / Workspace 启用两步验证并生成 Google 应用专用密码（Workspace 需符合组织策略）；Yahoo 在账号安全设置生成应用专用密码；QQ 启用 IMAP/SMTP 并生成客户端授权码。将凭据以单行写入各自的 `password_file`，仅允许服务账号和管理员按需读取，不提交到 Git。Outlook 不使用 `password_file`，按下文完成设备码授权。
3. 重启服务加载配置。让 MCP 客户端调用 `list_accounts` 确认别名，再用 `list_folders` 传入 `{"account":"personal-qq"}`（替换为实际别名）验证 IMAP。能列出文件夹不代表 SMTP 已验证；此检查不会发信或修改邮件。

添加到同一个实例的邮箱会向已持有该实例 Bearer token 的客户端开放，包括写入和发信。需要隔离的邮箱应放在使用独立 token 的实例中。

### 供应商设置

| `provider` | 默认 IMAP 地址 | 默认 SMTP 地址 | 凭据 |
| --- | --- | --- | --- |
| `gmail` | `imap.gmail.com:993` | `smtp.gmail.com:465` | 应用专用密码 |
| `workspace` | `imap.gmail.com:993` | `smtp.gmail.com:465` | 应用专用密码，须由组织策略允许 |
| `outlook` | `outlook.office365.com:993` | 个人账号 `smtp-mail.outlook.com:587`；组织账号 `smtp.office365.com:587` | Microsoft OAuth2，见下文 |
| `yahoo` | `imap.mail.yahoo.com:993` | `smtp.mail.yahoo.com:465` | 应用专用密码 |
| `icloud` | `imap.mail.me.com:993` | `smtp.mail.me.com:587` | Apple App 专用密码 |
| `qq` | `imap.qq.com:993` | `smtp.qq.com:465` | 开启 IMAP/SMTP 后生成的授权码，不是 QQ 登录密码 |

非 Outlook 账号配置 `provider`、`username`、`password_file`，必要时可用 `address` 指定其他隐式 TLS IMAP 地址。账号策略禁止应用密码时，这个接入方式不可用；服务不会自动切换到其他认证方式。各账号的凭据独立读取，一个账号没授权不妨碍其他账号使用。

- `from_address` 默认等于 `username`。如果登录名不是完整邮箱，填写提供商允许的完整发件地址。iCloud 短用户名默认补 `@icloud.com` 用于 SMTP 登录和发信；其他 iCloud 地址建议写完整登录名。
- 465 默认使用隐式 TLS，表中的 587 端点使用 STARTTLS。可用 `smtp_address` 和 `smtp_tls`（`implicit` 或 `starttls`）指定 SMTP 端点和加密模式；Outlook 只允许表中的两个 Microsoft SMTP 端点并强制 STARTTLS。两种模式都校验证书，不会回退到明文。
- `archive_folder`、`trash_folder`、`drafts_folder` 可填写 `list_folders` 返回的对应文件夹全名。未配置时，服务要求找到唯一的 `\Archive`、`\Trash`、`\Drafts` 特殊用途文件夹；服务不猜测本地化名称、不自动创建这些文件夹。移动（包括归档和移入废纸篓）要求原生 IMAP `MOVE`，不回退到可能永久删除邮件的操作。

### Outlook

1. 在 [Microsoft Entra](https://entra.microsoft.com/) 注册支持所需账号类型的应用，在认证设置允许公共客户端流（Allow public client flows）。MCP 在设备授权时请求 Office 365 Exchange Online 委派权限 `IMAP.AccessAsUser.All`、`SMTP.Send` 与 `offline_access`。如果门户未列出这两个 Exchange 权限，可先尝试设备授权，不要用 Microsoft Graph 权限代替。个人 Outlook.com 邮箱需要启用 IMAP；组织账号可能需要管理员允许 IMAP、SMTP AUTH、设备码授权和权限同意。参考 [微软 IMAP OAuth 文档](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth)。
2. 把[多账号示例](config.multi-account.example.json)中的 `personal-outlook` 条目加到 `accounts`，将 `username` 和 `client_id` 替换为邮箱地址和应用程序（客户端）ID。`tenant` 默认 `consumers`；如需使用组织账号，可填写组织 tenant ID。每个账号使用独立的 `oauth_file`，文件所在目录须允许服务账号写入。不需要客户端密码。

3. 在服务器执行 `./personal-mail-mcp-linux-amd64 -config config.json authorize-outlook personal-outlook`。按提示用浏览器输入设备码，并确认登录的是对应邮箱。令牌保存为 `0600` 文件，服务会在需要时刷新。不要让多个进程同时刷新同一令牌文件；重新授权前先停止服务。若先前只授权了 IMAP，必须重新运行此命令授权 SMTP 发信。

## Agent 接入

用支持 Streamable HTTP 和自定义请求头的 MCP 客户端。以本机连接为例：

```json
{
  "mcpServers": {
    "personal-mail": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer YOUR_MCP_TOKEN" }
    }
  }
}
```

配置格式因客户端而异；远程使用代理提供的 HTTPS URL。所有查询由 agent 主动调用；没有后台轮询、新邮件通知或独立事件流。

| 工具 | 用法 |
| --- | --- |
| `list_accounts` | 获取账号别名，不返回用户名或凭据。 |
| `list_folders` | 按账号列出可打开的文件夹。 |
| `search_messages` | 在指定账号和文件夹（默认 `INBOX`）内筛选 `seen`、`starred`、`keyword`、`from`、`to`、`cc`、`subject`、`body`、`since`、`before`。日期是到达日期，`since` 含当天，`before` 不含当天。 |
| `search_mailboxes` | 用相同条件查最多 8 个显式 `{account, folder}` 范围。每个范围单独返回结果、分页或错误。每范围默认 10 条，最多 20 条。 |
| `get_mailbox_status` | 在 `scopes` 中指定 1–8 个 `{account, folder}`，返回各文件夹的邮件总数、未读数和 UIDVALIDITY。 |
| `get_thread` | 指定一封邮件，按 `Message-ID`、`In-Reply-To`、`References` 查找确切关联；可指定同账号最多 3 个额外文件夹。结果含源文件夹与 UID，`truncated` 表示未查完。不根据主题猜测。 |
| `read_message` | 优先返回纯文本，其次是未渲染的 HTML；返回已读状态、星标、可识别的 IMAP 关键词和原信声明的回复地址，不标记为已读。 |
| `list_attachments` | 返回文件名、媒体类型、传输编码后的字节数和 `part`。 |
| `download_attachment` | 传入 `part`，返回解码后文件的 `content_base64`；不改变已读状态。 |
| `set_message_read` | 指定 `seen: true` 标记为已读，或 `seen: false` 标记为未读。 |
| `move_message` | 指定同账号下已有的 `destination` 文件夹。 |
| `archive_message` | 将邮件移到账号的归档文件夹。 |
| `delete_message` | 仅移到废纸篓，不永久删除。 |
| `copy_message` | 将一封邮件复制到同账号的另一个现有文件夹，保留原信。 |
| `update_message_flags` | 传入同一文件夹的 `uids` 和 `uid_validity`，修改 `seen`、`starred` 或 `add_keywords`、`remove_keywords`。支持 1–50 个 UID，回读确认结果。 |
| `move_messages` / `archive_messages` | 按一个源文件夹中的 1–50 个 UID 批量移动或归档。未确认的 UID 需重新查询，不要直接重试。 |
| `create_folder` / `rename_folder` | 创建或重命名普通文件夹。重命名可能同时移动其子文件夹。 |
| `retire_empty_folder` | 把已确认的空叶子文件夹重命名为可恢复的 `MCP-Retired-…` 文件夹，不执行 IMAP DELETE；可用 `rename_folder` 改回。 |
| `save_draft` / `list_drafts` / `discard_draft` | 在草稿文件夹保存一封新的未发送邮件、分页列出草稿、或把指定草稿移到废纸篓。保存不会覆盖已有草稿，也不会发信。 |
| `send_message` | 传入 `account`、`to`（地址数组）、`subject` 和纯文本 `body`；可选 `cc`、`bcc` 和 `attachments`。 |
| `reply_message` | 传入原信标识与纯文本 `body`。使用原信的 Reply-To（没有则用 From），不回复全部，不自动引用原信正文；可附加文件。发送前用 `read_message` 核对 `reply_to`。 |
| `forward_message` | 传入原信标识、`to` 和纯文本 `body`，可选 `cc`、`bcc` 和额外附件；完整原信作为 `original.eml` 附加发送。含非空 `Bcc` 或 `Resent-Bcc` 头的原信拒绝转发，避免泄露密送人。 |

例如 `search_messages` 传 `{"account":"personal-gmail","seen":false,"limit":10}` 直接查询未读，不用逐页读完再过滤。`seen` 和 `starred` 省略时不限状态。正文匹配由上游服务器完成；自定义关键词是标准 IMAP flags，不等同于 Gmail 标签。

搜索按 UID 降序分页，后续请求传上页的 `next_before_uid`（作为 `before_uid`）、`uid_validity` 和原搜索条件。单文件夹每页最多 50 条；跨文件夹时把上述两个分页字段放到对应的 `scopes` 条目。摘要限制字段长度和列表大小，触及限制时返回 `summary_truncated`。草稿保存结果不明时，可用 `list_drafts` 返回的 `message_id` 核对，不要盲目重试。操作单封邮件时，使用搜索时的 `account` 和 `folder`，以及搜索返回的 `uid` 和 `uid_validity`；移动后需要在目标文件夹重新搜索，或使用服务商返回的目标 `uid` 和 `uid_validity`。

发送附件格式为 `{"filename":"notes.txt","media_type":"text/plain","content_base64":"aGVsbG8="}`，`media_type` 可省略。单件下载的解码上限为 2 MiB，发送最多 10 件附件且总计 2 MiB；转发时的原始 `.eml` 也占用这两个限额。外发最多 25 名收件人，正文最多 64 KiB。SMTP 成功仅表示服务商接收；是否保留已发送副本取决于提供商。提交结果不明时，请先核对，不要直接重试。

正文 MIME 部分超过 256 KiB 不下载；返回正文最多 64 KiB。每个 IMAP 请求最多读取 8 MiB 上游数据，MCP 请求体最多 4 MiB。邮件正文、回复地址和附件内容是不可信数据，不应被当作工具指令执行。

## 供应商兼容性

五个 IMAP 端点（Gmail 和 Google Workspace 共用）在未登录的 TLS `CAPABILITY` 检查中都声明了 IMAP4rev1。检索、`STATUS` 计数、复制、旗标、文件夹重命名和草稿 `APPEND` 使用标准 IMAP 命令；这确认了协议路径，不代表账号有对应权限，也不代表所有供应商的搜索或标签语义一致。

| 供应商 | 登录前的观察与限制 |
| --- | --- |
| Gmail / Google Workspace | 未在本次响应中声明 `MOVE`。Gmail 的[官方扩展](https://developers.google.com/workspace/gmail/imap/imap-extensions)将标签呈现为文件夹，且提供独立的标签及线程 ID；本项目不将这些等同于标准 IMAP 关键词，也不依赖这些扩展。Workspace 组织策略另可能禁止 IMAP 或应用密码。 |
| Outlook | 声明 `MOVE` 与 `UIDPLUS`；需要 OAuth2，并受管理员策略和邮箱权限限制。 |
| Yahoo | 声明 `MOVE`、`UIDPLUS` 与 `SPECIAL-USE`。 |
| iCloud | 未在本次响应中声明 `MOVE`，登录后的支持情况未实测。 |
| QQ | 声明 `MOVE` 与 `UIDPLUS`。 |

实际移动仅在当前 IMAP 会话支持原生 `MOVE` 时执行。没有账号级兼容性诊断工具；也未对真实邮箱执行写入、移动或删除验证。草稿等特殊用途文件夹未被服务商标记时可手工指定。批量标记只在文件夹声明所需标记可持久化时执行；工具回读后将 UID 标为 `confirmed` 或 `unconfirmed`。

## 容器

`make image` 生成 `dist/personal-mail-mcp.oci.tar`（Linux amd64/arm64 OCI 镜像）。构建需要 Go 和支持 OCI 导出的 Docker Buildx；镜像打包不拉取基础镜像。Makefile 默认使用构建机的系统 CA 包，可用 `make image CA_FILE=/path/to/trusted-ca.pem` 指定。镜像不包含配置或凭据。

在 Linux 上导入镜像，用 host 网络访问真实网卡；挂载目录必须允许服务用户读取，Outlook 授权及刷新还要求令牌目录可写。以下命令从放置 `config.json` 和 `secrets/` 的目录运行：

```sh
docker load -i /path/to/personal-mail-mcp.oci.tar
docker run --rm --network host --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$PWD,dst=/config" personal-mail-mcp:local
```

如需在容器内执行 Outlook 授权，先停止服务，再在上述 `docker run` 命令的镜像名后接 `-config /config/config.json authorize-outlook personal-outlook`。若使用桥接网络，`interfaces` 指容器内的网卡，宿主机的对外地址由 Docker 的端口映射或代理控制。

`make test` 运行测试（包含本地 IMAP 服务、MCP 客户端和模拟 OAuth 端点）与 `go vet`。真实邮箱的授权仍需使用你自己的账号验证。
