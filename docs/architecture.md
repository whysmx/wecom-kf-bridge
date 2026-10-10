# 架构概览

微信客服与未修改的 [cc-connect](https://github.com/chenhg5/cc-connect) 之间的独立协议转换网关：一个进程、单企业、SQLite，不调用模型、不修改客户端源码。

## 1. 数据流

```text
个人微信客户（可多人同时对话）
        ↕  微信客服回调 / 客服 API
WeCom KF Bridge（单进程，进程内并发）
        ↕  自建应用兼容 HTTP（gettoken / user/get / message/send）
           + 加密 XML 主动回调
cc-connect（现有 WeChat Work 配置，高级 api_base_url 指向网关）
        ↕
Codex / 现有 Agent 执行端
```

网关同时是：

- **微信 API 的客户端**：收回调、同步消息、发送客服消息、管理客服账号；
- **cc-connect 的兼容服务端**：提供虚拟企业凭证与 `/cgi-bin/*`；
- **回调发起方**：必须能访问 cc-connect 的回调 URL（`api_base_url` 不是反向隧道）。

## 2. 核心概念

| 概念 | 含义 |
|---|---|
| **企业（enterprise）** | 一个真实企业微信主体；首版仅支持配置一个，并驱动管理后台范围。 |
| **客服账号（open_kfid）** | 微信侧真实微信客服；经官方接口同步/增删改，不可用本地假数据代替。 |
| **绑定（binding）** | 把一个客服账号接到一个 cc-connect 平台实例：虚拟 corp_id/secret、agent_id、回调 Token/AES、callback_url。 |
| **客户（customer）** | 以 `external_userid` 为根；映射为稳定 `customer_id` + 代际 UID（给 cc-connect 的 userid）。 |
| **代际（generation）** | 人工恢复、改绑或删除账号冻结时递增；旧 UID 永久失去发送权，客户端会看到新会话。 |
| **Inbox** | 从微信同步下来的入站消息/事件，带 scope 游标与去重。 |
| **Outbox** | 发往微信的出站记录；先落库再发送，受代际、人工栅栏、绑定 revision、发送窗口约束。 |
| **管理后台** | 可选独立监听（默认 `127.0.0.1:8091`）；单企业中文页面，不挂在公网 listener 上。 |

## 3. 进程内职责

| 职责 | 做什么 |
|---|---|
| 微信适配 | Token、验签解密、sync、发送、客服账号 API |
| 兼容适配 | 虚拟凭证、gettoken / user/get / message/send、加密 XML 回调 |
| 状态与身份 | Inbox/Outbox、游标、代际、人工接管、预算与审计 |
| 管理后台 | 账号/绑定/客户/诊断/设置（可选） |
| 基础设施 | SQLite、配置与密钥、单实例文件锁、有界 worker |

**单进程 ≠ 单客户。** 不同客户独立并发；同一客户的状态更新、同一 sync scope 的游标局部串行。Worker 数限制的是网关后台任务并发，不是客户数或 AI 会话数。

## 4. 收发要点

1. **收**：微信通知 → 验签 → 记待同步 → 尽快 200；后台按 scope 串行拉页，授权客户内容再投递给 cc-connect。
2. **投递**：校验绑定与客户资格 → 持久化 → 加密 XML POST 到 callback_url。HTTP 200 只表示客户端接受了请求，不表示 AI 已答完。
3. **回传**：cc-connect 调网关 `message/send` → 先写 Outbox → 核查代际/人工/窗口 → 调微信发送；明确成功才回 `errcode:0`，不确定则记 UNKNOWN，默认不盲重试。
4. **人工**：先调官方 `service_state` 并回读确认，再改本地；不确定时标 UNKNOWN 并暂停 AI。

## 5. 部署形态与边界

- 推荐：单企业、单实例、同机或受控内网；跨机用已有隧道/反代，不自研隧道协议。
- SQLite 为唯一状态源；重复启动争抢同一数据库会失败（文件锁）。
- 不保证：端到端 exactly-once、重启后历史补答、撤回已在途消息、停止正在跑的 Codex、无限并发。
- 尚未完成真实企业微信全链路联调；发布包可用于试验，**非生产就绪**。更细的历史设计与评审记录见 [archive/](archive/)。
