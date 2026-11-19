---
id: personal-mail-mcp
type: goal-and-requirements
title: 个人邮件 MCP
status: active
---

# 个人邮件 MCP

一个部署在自己服务器上的只读邮件 MCP，让其他机器上的 agent 通过同一入口访问多个个人邮箱。

## Overview

个人邮件 MCP 是一个常驻的 HTTP MCP 服务。它把分散在 Gmail、Outlook、Yahoo、iCloud、Google Workspace 和 QQ 邮箱中的多个账号暴露为一组只读工具；agent 在调用时指定账号。服务只绑定配置中指定的一个或多个网卡，凭据只保存在服务器上。

## Problem

个人邮箱分散在六类服务中，而且同类账号不止一个。现在每个 agent 都要单独配置邮箱，没有一个可限制到指定网卡的统一只读入口。

## Capabilities

- 提供一个常驻 HTTP MCP 服务，并只绑定配置指定的一个或多个网卡。
- 在同一服务中配置多个邮箱，并在每次调用时指定目标账号。
- 通过 IMAP 列出文件夹、读取邮件、搜索邮件和查看附件信息。
- 支持 Gmail、Outlook、Yahoo、iCloud、Google Workspace 和 QQ 邮箱。
- 把邮箱凭据只保存在服务器上。
- 以 x86 与 ARM 静态二进制及对应容器镜像交付。

## Tech Notes

使用 Go，交付 Linux amd64 和 arm64 静态二进制与 OCI 镜像，以 1 核 1 GB 为部署基线。邮件统一经 IMAP 访问；Outlook 使用 Microsoft OAuth2，其他邮箱使用提供商允许的应用专用密码或授权码。
