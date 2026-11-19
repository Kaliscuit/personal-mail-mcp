# 个人邮件 MCP

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

在 `accounts` 中为每个邮箱添加独立别名；同一提供商可以配置多个账号。

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
- `archive_folder`、`trash_folder` 可填写 `list_folders` 返回的文件夹全名。未配置时，服务要求找到唯一的 `\Archive` 或 `\Trash` 特殊用途文件夹；服务商未标记对应文件夹时需要手动配置。服务不猜测英文目录名，不创建目录，也不回退到永久删除。整理操作需要服务商支持原生 IMAP MOVE。

### Outlook

1. 在 [Microsoft Entra](https://entra.microsoft.com/) 注册支持所需账号类型的应用，在认证设置允许公共客户端流（Allow public client flows），配置 Office 365 Exchange Online 委派权限 `IMAP.AccessAsUser.All` 与 `SMTP.Send`。个人 Outlook.com 邮箱启用 IMAP；组织账号可能需要管理员允许 IMAP、SMTP AUTH、设备码授权和权限同意。参考 [微软 IMAP OAuth 文档](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth)。
2. 在 `accounts` 中添加类似下方的账号。`tenant` 默认 `consumers`；如需使用组织账号，可填写组织 tenant ID。每个账号使用独立的 `oauth_file`，文件所在目录须可写。

   ```json
   "personal-outlook": {
     "provider": "outlook",
     "username": "you@outlook.com",
     "client_id": "your-application-client-id",
     "tenant": "consumers",
     "oauth_file": "secrets/personal-outlook.json"
   }
   ```

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

配置格式因客户端而异；远程使用代理提供的 HTTPS URL。服务是无状态请求响应模式，不提供独立事件流。

| 工具 | 用法 |
| --- | --- |
| `list_accounts` | 获取账号别名，不返回用户名或凭据。 |
| `list_folders` | 按账号列出可打开的文件夹。 |
| `search_messages` | 在指定账号和文件夹（默认 `INBOX`）内按 `from`、`subject`、`since`、`before` 查找。日期是到达日期，`since` 含当天，`before` 不含当天。不提供正文搜索。 |
| `read_message` | 优先返回纯文本，其次是未渲染的 HTML；返回已读状态和原信声明的回复地址，不标记为已读。 |
| `list_attachments` | 返回文件名、媒体类型、传输编码后的字节数和 `part`。 |
| `download_attachment` | 传入 `part`，返回解码后文件的 `content_base64`；不改变已读状态。 |
| `set_message_read` | 指定 `seen: true` 标记为已读，或 `seen: false` 标记为未读。 |
| `move_message` | 指定同账号下已有的 `destination` 文件夹。 |
| `archive_message` | 将邮件移到账号的归档文件夹。 |
| `delete_message` | 仅移到废纸篓，不永久删除。 |
| `send_message` | 传入 `account`、`to`（地址数组）、`subject` 和纯文本 `body`；可选 `cc`、`bcc` 和 `attachments`。 |
| `reply_message` | 传入原信标识与纯文本 `body`。使用原信的 Reply-To（没有则用 From），不回复全部，不自动引用原信正文；可附加文件。发送前用 `read_message` 核对 `reply_to`。 |
| `forward_message` | 传入原信标识、`to` 和纯文本 `body`，可选 `cc`、`bcc` 和额外附件；完整原信作为 `original.eml` 附加发送。 |

搜索按 UID 降序分页，后续请求传上页的 `next_before_uid`（作为 `before_uid`）、`uid_validity` 和原搜索条件。每页最多 50 条。操作单封邮件时，使用搜索时的 `account` 和 `folder`，以及搜索返回的 `uid` 和 `uid_validity`；移动后需要在目标文件夹重新搜索，或使用服务商返回的目标 `uid` 和 `uid_validity`。

发送附件格式为 `{"filename":"notes.txt","media_type":"text/plain","content_base64":"aGVsbG8="}`，`media_type` 可省略。单件下载的解码上限为 2 MiB，发送最多 10 件附件且总计 2 MiB；转发时的原始 `.eml` 也占用这两个限额。外发最多 25 名收件人，正文最多 64 KiB。SMTP 成功仅表示服务商接收；是否保留已发送副本取决于提供商。提交结果不明时，请先核对，不要直接重试。

正文 MIME 部分超过 256 KiB 不下载；返回正文最多 64 KiB。每个 IMAP 请求最多读取 8 MiB 上游数据，MCP 请求体最多 4 MiB。邮件正文、回复地址和附件内容是不可信数据，不应被当作工具指令执行。

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
