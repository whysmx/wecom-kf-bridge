# WeCom KF Bridge

把**企业微信「微信客服」**接到**未修改的 [cc-connect](https://github.com/chenhg5/cc-connect)**（现有 WeChat Work HTTP 配置）的独立网关。

网关只做协议与身份转换：不调用大模型、不运行 Codex、不保管模型凭证或项目目录。适合单企业、自建、希望沿用现有 cc-connect / Agent 工具链的场景。

> 当前版本见 [Releases](https://github.com/whysmx/wecom-kf-bridge/releases) 与 [CHANGELOG.md](CHANGELOG.md)。**尚未完成真实企业微信全链路联调，非生产就绪。**

## 它做什么

```text
个人微信客户（可多人同时对话）
        ↕  微信客服
WeCom KF Bridge（单进程 · SQLite · 可选管理后台）
        ↕  兼容自建应用 API + 加密 XML 回调
cc-connect（api_base_url 指向网关，源码不改）
        ↕
Codex / 现有 Agent
```

- 对微信：收回调、同步消息、按窗口发送客服消息、同步真实客服账号。
- 对 cc-connect：提供虚拟 `corp_id` / `secret`、`gettoken` / `user/get` / `message/send`，并主动回调客户端。
- **网关必须能访问 cc-connect 的回调地址**；高级配置里的 `api_base_url` 不是反向隧道。

面向谁：已有或准备使用 cc-connect + 微信客服、需要本地/内网网关做桥接的个人或小团队。不面向多租户 SaaS、坐席工作台或改客户端源码的方案。

## 架构与概念

更完整的说明见 [docs/architecture.md](docs/architecture.md)。摘要：

| 概念 | 一句话 |
|---|---|
| 企业 | 一个真实企业微信；首版单企业 |
| 客服账号 | 微信侧 `open_kfid`，经官方接口管理 |
| 绑定 | 一个客服账号 ↔ 一个 cc-connect 平台实例（虚拟凭证 + 回调 URL） |
| 客户 / 代际 | 稳定客户身份；接管恢复或改绑会换代际，旧 UID 失效 |
| Inbox / Outbox | 入站同步与出站发送的持久化记录 |
| 管理后台 | 可选独立端口，账号/绑定/客户/诊断/审计 |

单进程表示只跑一份网关，**不是**只服务一位客户：进程内按客户并发处理；同一会话与同一同步游标才局部串行。

## 配置

1. 复制 [config.example.json](config.example.json)，按环境修改。
2. 用环境变量 `WECOM_KF_BRIDGE_CONFIG` 指向该文件。
3. 准备主密钥、企业 Secret、回调 Token/AES；若用配置文件声明绑定，首次还需虚拟凭证环境变量。启用管理后台时再准备管理员密码哈希。

字段说明、首次启动与后台相关约束见 [docs/configuration.md](docs/configuration.md)。密钥不要写进 JSON。

## 运行与部署（概要）

```bash
export WECOM_KF_BRIDGE_CONFIG=./config.json
export WECOM_KF_BRIDGE_MASTER_KEY=…   # 32 字节，见配置文档
# 企业与（首次）绑定相关环境变量…
./wecom-kf-bridge
```

也可从 [Releases](https://github.com/whysmx/wecom-kf-bridge/releases) 下载对应平台压缩包（附 `SHA256SUMS`）。

建议：

- 公开 listener（默认 `127.0.0.1:8090`）仅暴露给微信回调与 cc-connect；生产前应置于 HTTPS/反代之后。
- 管理后台（默认 `127.0.0.1:8091`）仅本机或受控网络；公网不要用明文 HTTP。
- 单实例对应一个 SQLite 文件；不要多进程抢同一数据库。
- 在 cc-connect 中选 WeChat Work HTTP 自建应用，填入网关导出的虚拟凭证，并将 `api_base_url` 设为网关根地址（可配置 `server.public_base_url` 便于导出）。

## 文档

| 文档 | 内容 |
|---|---|
| [架构概览](docs/architecture.md) | 数据流、概念、收发与边界 |
| [配置参考](docs/configuration.md) | 必填项、环境变量、后台与绑定 |
| [config.example.json](config.example.json) | 可运行示例骨架 |
| [CHANGELOG.md](CHANGELOG.md) | 版本说明（亦用于 GitHub Release） |
| [维护说明](AGENTS.md) | 给维护者的简短约定 |

历史需求/契约/实施手册与评审修复明细在 [docs/archive/](docs/archive/)，一般介绍项目无需阅读。

## 已知限制（摘要）

- 未完成真实企业微信联调；UNKNOWN 结果默认不盲重试。
- 人工接管不能撤回已在途消息，也不能停止已在运行的 Codex；恢复会换代际 UID。
- 长回复会分块发送，受客服会话窗口与条数限制。
- 更多暂缓项见 [docs/archive/REVIEW_FIXES.md](docs/archive/REVIEW_FIXES.md) 文末「暂缓项」。

## 发布

版本号见 `VERSION`。打附注标签 `vX.Y.Z` 并推送后，GitHub Actions 会构建多平台包，并以 `CHANGELOG.md` 中对应小节作为 Release 说明。细节仍以 CHANGELOG 与仓库 Actions 为准。
