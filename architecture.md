---
id: mail-mcp-architecture
type: architecture-design
title: 邮件 MCP 架构
status: active
parent: personal-mail-mcp
---

# 邮件 MCP 架构

## Overview

一个 Go 进程同时承担 HTTP MCP 入口、共享密钥校验和只读 IMAP 访问。它不保存邮件副本，也不提供写入或发送路径。

## System shape

```mermaid
flowchart LR
  Agent[远程 agent] -->|HTTP MCP + 共享密钥| Server[邮件 MCP]
  Server -->|只读 IMAP| Providers[六个邮箱服务]
```

配置声明一个或多个监听网卡、共享密钥文件和多个命名邮箱账号。每次邮箱操作都显式指定账号；服务不为调用方保存当前账号。

## Decisions

- `/mcp` 使用无状态 Streamable HTTP，每个请求必须携带 `Authorization: Bearer …`；不提供独立事件流，不接受 JSON-RPC 批处理。共享密钥可访问该实例全部账号。服务只监听指定网卡在启动时的 IPv4/IPv6 单播地址，不承担防火墙或 HTTPS 终止；需要 HTTPS 时，由部署方在外部代理终止。
- 回环监听保留 SDK 默认的 DNS 重绑定防护。`additional_allowed_hosts` 可为保留原始 Host 的反向代理增加精确 DNS 主机名：仅在原始请求通过跨来源和 Bearer 认证检查后，才为匹配请求的 SDK 副本使用回环 Host。不信任转发 Host 头，不改变非回环连接的行为。
- 搜索的发件人、主题和 IMAP 到达日期条件由上游处理，不下载正文来匹配。结果按 UID 倒序分页，读取邮件和附件时要求匹配同一文件夹的 UID 有效期（`UIDVALIDITY`），避免 UID 重用后读错邮件。
- IMAP 通过校验证书的 TLS 连接，使用 `EXAMINE` 和 `BODY.PEEK` 保持已读状态不变。只按需下载有大小限制的文本 MIME 部分；附件列表只读取 `BODYSTRUCTURE` 中的文件名、类型和传输编码后的大小。
- 凭据来自服务器本地文件，不通过 MCP 工具创建、修改或返回。Outlook 使用独立 CLI 发起 Microsoft 设备授权，服务按账号串行刷新并持久化轮换后的令牌；其余账号由各自的应用密码文件提供凭据。
- 为小机器限制连接数、并发请求、请求体、上游读取量、FETCH 结构复杂度和邮件正文大小。没有长期连接池、同步任务或本地邮件库。

## Invariants

- 服务对所有邮箱保持只读，不发送、移动、删除、标记或创建邮件。
- 一个账号的 IMAP 或授权失败不影响其他已配置账号的后续调用。
