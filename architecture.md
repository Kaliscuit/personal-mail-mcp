---
id: mail-mcp-architecture
type: architecture-design
title: 邮件 MCP 架构
status: active
parent: personal-mail-mcp
---

# 邮件 MCP 架构

## Overview

一个 Go 进程同时承担 HTTP MCP 入口、共享密钥校验、IMAP 邮箱读写和 SMTP 邮件提交。它不保存邮件副本。

## System shape

```mermaid
flowchart LR
  Agent[远程 agent] -->|HTTP MCP + 共享密钥| Server[邮件 MCP]
  Server -->|IMAP 读写| Providers[六个邮箱服务]
  Server -->|SMTP 提交| Providers
```

配置声明一个或多个监听网卡、共享密钥文件和多个命名邮箱账号。每次邮箱操作都显式指定账号；服务不为调用方保存当前账号。

## Decisions

- `/mcp` 使用无状态 Streamable HTTP，每个请求必须携带 `Authorization: Bearer …`；不提供独立事件流，不接受 JSON-RPC 批处理。共享密钥授予该实例全部账号的读、写和发信权限；没有账号级的权限隔离或旧密钥只读模式（全量重部署下的权限设计推断，待用户确认）。服务只监听指定网卡在启动时的 IPv4/IPv6 单播地址，不承担防火墙或 HTTPS 终止；需要 HTTPS 时，由部署方在外部代理终止。
- 回环监听保留 SDK 默认的 DNS 重绑定防护。`additional_allowed_hosts` 可为保留原始 Host 的反向代理增加精确 DNS 主机名：仅在原始请求通过跨来源和 Bearer 认证检查后，才为匹配请求的 SDK 副本使用回环 Host。不信任转发 Host 头，不改变非回环连接的行为。
- 搜索的邮件头、到达日期、正文和标记条件由 IMAP 处理。跨范围查询并发查询显式列出的账号和文件夹，受全局 IMAP 会话上限和摘要大小上限约束，按范围报告结果或错误，不合并不同文件夹的 UID。会话以邮件引用头在有限范围内查找，不依赖供应商专用线程扩展。搜索在每个文件夹内按 UID 倒序分页；对单封邮件的操作以 `UIDVALIDITY` 防止 UID 重用。
- IMAP 使用校验证书的 TLS。读取使用 `EXAMINE` 和 `BODY.PEEK`；附件按 `BODYSTRUCTURE` 的部件编号按需取回，不将文件落盘。附件在 MCP 边界以 Base64 传输，并限制传输和解码后的大小。
- 批量写入仅处理同一文件夹的显式 UID 集合。批量旗标更新仅增减邮箱声明可持久化的请求标记，并回读确认。移动只允许在连接上发出原生 `UID MOVE`，不退化为会隐式清除已删除邮件的操作。归档、废纸篓与草稿文件夹使用配置值或唯一的特殊用途文件夹，缺失或冲突时拒绝。
- 草稿通过 IMAP `APPEND` 保存，不调用 SMTP。废弃草稿只移入废纸篓。文件夹移除只重命名已确认的空叶子文件夹；标准 IMAP 无法将空目录检查与 `DELETE` 原子化，重命名保证迟到的邮件也不被永久删除。
- 外发使用验证证书的隐式 TLS 或强制 STARTTLS 后的认证 SMTP，发件人来自账号配置。密送仅写入 SMTP 信封；回复使用原信回复地址和线程头；转发保留完整 `.eml`，但拒绝包含密送头内容的原信。成功表示 SMTP 确认接收，不保证最终送达或已发送文件夹副本；提交结果未知时不重试。
- 凭据来自服务器本地文件，不通过 MCP 工具创建、修改或返回。Outlook 使用独立 CLI 发起 Microsoft 设备授权，获取 IMAP 和 SMTP 权限；两种协议共享按账号串行刷新并持久化轮换的令牌。其余账号由各自的应用密码文件提供凭据。
- 为小机器限制连接数、并发请求、请求体、上游读取量、FETCH 结构复杂度和邮件正文大小。没有长期连接池、同步任务或本地邮件库。

## Invariants

- 读取邮件、下载附件和获取回复或转发原文均不改变已读状态。
- 一个账号的 IMAP、SMTP 或授权失败不影响其他已配置账号的后续调用。
