# 个人邮件 MCP

自托管的多账号只读邮件 MCP。agent 连接 Streamable HTTP `/mcp`，用账号别名查找邮件；邮箱凭据和 Outlook 刷新令牌不交给 agent。不需要数据库、后台同步或 Tailscale。

## Linux 部署

用 Go 1.26 或更新版本执行 `make binaries`，从 `dist/` 取适合服务器的 `personal-mail-mcp-linux-amd64` 或 `personal-mail-mcp-linux-arm64`。服务器需要系统 CA 证书包，用于校验上游 IMAP 和 Microsoft 的 TLS 证书。将二进制、示例配置和密钥放在服务账户有权读取的目录中：

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

**服务不提供 HTTPS。** 示例绑定 `lo`，只接受本机连接。远程访问建议让你已有的 HTTPS 反向代理监听目标网卡，再转发到 `http://127.0.0.1:8080/mcp`，保留 `Authorization`，并将上游 `Host` 设置为 `127.0.0.1:8080`（MCP SDK 会检查回环监听上的 Host）。不要把明文入口直接暴露到不可信网络。密钥可访问该实例的所有配置账号，不是逐账号授权。

## 邮箱配置

在 `accounts` 中为每个邮箱添加独立别名；同一提供商可以配置多个账号。

| `provider` | 默认 IMAP 地址 | 凭据 |
| --- | --- | --- |
| `gmail` | `imap.gmail.com:993` | 应用专用密码 |
| `workspace` | `imap.gmail.com:993` | 应用专用密码，须由组织允许 IMAP 和应用密码 |
| `outlook` | `outlook.office365.com:993` | Microsoft OAuth2，见下文 |
| `yahoo` | `imap.mail.yahoo.com:993` | 应用专用密码 |
| `icloud` | `imap.mail.me.com:993` | Apple App 专用密码；通常用 `@icloud.com` 前的短用户名 |
| `qq` | `imap.qq.com:993` | 开启 IMAP 后生成的授权码，不是 QQ 登录密码 |

非 Outlook 账号配置 `provider`、`username`、`password_file`，必要时可用 `address` 指定其他隐式 TLS IMAP 地址。账号策略禁止应用密码时，这个接入方式不可用；服务不会自动切换到其他认证方式。各账号的凭据独立读取，一个账号没授权不妨碍其他账号使用。

### Outlook

1. 在 [Microsoft Entra](https://entra.microsoft.com/) 注册支持所需账号类型的应用，在认证设置允许公共客户端流（Allow public client flows），配置 Office 365 Exchange Online 委派权限 `IMAP.AccessAsUser.All`。个人 Outlook.com 邮箱启用 IMAP；组织账号可能需要管理员允许 IMAP、设备码授权和权限同意。参考 [微软 IMAP OAuth 文档](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth)。
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

3. 在服务器执行 `./personal-mail-mcp-linux-amd64 -config config.json authorize-outlook personal-outlook`。按提示用浏览器输入设备码，并确认登录的是对应邮箱。令牌保存为 `0600` 文件，服务会在需要时刷新。不要让多个进程同时刷新同一令牌文件；重新授权前先停止服务。

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
| `read_message` | 传入搜索返回的 `uid` 和 `uid_validity`，保持账号和文件夹不变。优先返回纯文本，其次是未渲染的 HTML；不标记为已读。 |
| `list_attachments` | 使用相同的消息标识，只返回文件名、媒体类型和传输编码后的字节数。 |

搜索按 UID 降序分页，后续请求传上页的 `next_before_uid`（作为 `before_uid`）、`uid_validity` 和原搜索条件。每页最多 50 条。正文 MIME 部分超过 256 KiB 不下载；可返回的解码正文最多 64 KiB。每个 IMAP 请求最多读取 8 MiB 上游数据，宽泛搜索失败时缩小日期范围。邮件正文和文件名是不可信数据，不应被当作工具指令执行。

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
