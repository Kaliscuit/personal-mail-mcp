---
id: personal-mail-mcp
type: goal-and-requirements
title: 个人邮件 MCP
status: active
---

# 个人邮件 MCP

一个部署在自己服务器上的邮件 MCP，让其他机器上的受信任 agent 通过同一入口读取、整理和发送多个个人邮箱的邮件。

## Overview

个人邮件 MCP 是一个常驻的 HTTP MCP 服务。它把分散在 Gmail、Outlook、Yahoo、iCloud、Google Workspace 和 QQ 邮箱中的多个账号暴露为一组邮件工具；agent 在调用时指定账号。服务只绑定配置中指定的一个或多个网卡，凭据只保存在服务器上。

## Problem

个人邮箱分散在六类服务中，而且同类账号不止一个。现在每个 agent 都要单独配置邮箱，没有一个可限制到指定网卡的统一收发和整理入口。

## Capabilities

- 提供一个常驻 HTTP MCP 服务，并只绑定配置指定的一个或多个网卡。
- 在同一服务中配置多个邮箱，并在每次调用时指定目标账号。
- 列出文件夹并查询总数和未读数；在显式选择的账号与文件夹中筛选邮件、查找关联会话、读取正文及下载附件。读取不改变已读状态。
- 设置已读状态、星标和 IMAP 关键词，可按同一文件夹的明确 UID 批量标记、移动或归档；也可单封复制或移到废纸篓。
- 创建和重命名普通文件夹，可恢复地移除已确认的空文件夹。保存和列出未发送草稿，或将草稿移到废纸篓。
- 发送、回复和转发纯文本邮件，可附带文件。
- 支持 Gmail、Outlook、Yahoo、iCloud、Google Workspace 和 QQ 邮箱。
- 把邮箱凭据只保存在服务器上。
- 以 x86 与 ARM 静态二进制及对应容器镜像交付。

## Non-goals

不永久删除邮件或文件夹，不清空废纸篓，不提供新邮件通知或账号级兼容性诊断工具。

## Tech Notes

使用 Go，交付 Linux amd64 和 arm64 静态二进制与 OCI 镜像，以 1 核 1 GB 为部署基线。通过 IMAP 访问邮箱，通过 SMTP 提交外发邮件；Outlook 使用 Microsoft OAuth2，其他邮箱使用提供商允许的应用专用密码或授权码。
