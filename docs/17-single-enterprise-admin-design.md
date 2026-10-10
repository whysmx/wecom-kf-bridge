# 单企业管理后台设计

> 本文是单企业部署的管理后台设计基线。后台只管理一个企业及其多个微信客服账号，不提供企业列表、租户切换、租户注册或复杂 RBAC。本文描述目标设计，不能替代真实企业微信联调和安全验收。

## 1. 目标与边界

### 1.1 目标

- 用一个中文后台完成单企业的客服账号、转发绑定、客户接管、消息诊断和运行配置管理。
- 后台与客户兼容接口隔离，不能用 `access_token` 登录后台。
- 管理操作可审计、可重试但不盲目重放；外部结果不确定时保留 UNKNOWN 和人工处置入口。
- 优先同进程服务端渲染 HTML 和普通表单，不引入 SPA、通用 API 平台、独立消息队列或新的管理服务。

### 1.2 明确不做

- 不做企业列表和企业切换；企业信息是全局唯一配置。
- 不做租户注册、组织架构、排班、工单、坐席聊天和复杂角色体系。
- 不把微信客服账号删除等同于停用 AI 转发。
- 不在后台提供 UNKNOWN 消息的一键重放或“确认成功”伪造按钮。

## 2. 运行与网络

- 管理后台使用独立监听地址，默认 `127.0.0.1:8091`，不注册到 public listener。
- public listener 只提供兼容 `/cgi-bin/*`、微信回调 `/webhooks/wechat-kf/{tenant_key}` 和最小健康探针。
- 管理监听可以放在受控反向代理之后；公网部署必须使用 HTTPS。未配置可信代理时不信任 `X-Forwarded-For`。
- 后台与 public listener 共用进程、SQLite 和 worker 生命周期，但使用独立路由和鉴权中间件。

## 3. 页面与导航

固定左侧导航，不显示企业选择器：

1. 概览
2. 客服账号
3. 转发绑定
4. 客户与接管
5. 消息诊断
6. 系统设置
7. 操作审计

顶部显示企业名称、CorpID 掩码、网关状态、微信客服 API 状态和最近同步时间。

### 3.1 概览

显示网关、SQLite、SyncWorker、DeliveryWorker、微信 API、启用绑定、暂停客户、UNKNOWN、重试和最近异常。异常只显示对象、时间、错误类别和建议，不显示密钥或默认客户正文。

### 3.2 客服账号

单企业下允许多个 `open_kfid`。列表支持分页和搜索，字段包括客服名称、`open_kfid`、启用状态、绑定项目、官方链接、最近同步时间和 revision。

操作包括同步、新建、编辑名称/头像、查看详情、生成官方客服链接、编辑本地备注和删除。删除必须在危险区域二次确认，确认票据绑定账号、动作、revision 和短有效期，并且只能使用一次。外部响应丢失时显示 UNKNOWN 并暂停相关自动操作，不按同名自动合并。

### 3.3 转发绑定

每行表示一个客服账号与 cc-connect 项目的绑定，显示项目、`open_kfid`、callback URL、启用状态、revision 和最近投递结果。

操作包括新建、启用、停用、改绑、回调挑战验证、虚拟凭证轮换和一次性导出。真实微信 Secret 只保存环境变量引用；虚拟 corp、secret、token、AES key 首次生成时一次显示，之后只显示掩码。改绑、轮换和停用必须使旧 revision、旧 UID 和相关未发送任务失效。

### 3.4 客户与接管

查询客户 UID、昵称、`external_userid`、当前代际、官方接待状态、本地状态和最近消息时间。提供转人工、恢复 AI、新建代际和查看最近 inbox/outbox。

页面必须区分官方接待状态、本地暂停状态和服务存活状态。恢复 AI 时明确提示会生成新的 UID/客户端上下文；已在途的 Codex 不承诺可撤回。

### 3.5 消息诊断

分为 Inbox、Outbox 和待核查三个视图，重点显示 `RETRY_WAIT`、`HELD`、`DELIVERY_UNKNOWN`、`UNKNOWN`、`UPSTREAM_ACCEPTED`、`REJECTED`。

默认只显示对象 ID、客户标识、时间、attempt、chunk 进度、错误类别和 revision，不显示完整正文。查看敏感正文必须二次认证并写审计。只允许标记人工核实或关闭诊断项，不提供盲目重发。

### 3.6 系统设置

按单企业分组展示：

- 企业名称、CorpID 掩码、Secret/回调 Token/AES 的环境变量名。
- WeCom API 地址、customer origins、`service_state_map`。
- 48 小时窗口、发送预算、分块大小、最大投递次数。
- 同步周期、投递周期、worker 并发、关闭宽限时间。
- callback allowlist 和连接测试。

配置保存带 revision；callback URL 保存与测试使用同一 SSRF 校验。凭证不进入 URL、localStorage、日志或第三方脚本。

### 3.7 操作审计

记录操作者、动作、对象、revision、operation_id、结果、时间和脱敏摘要。至少覆盖登录、同步、账号 CRUD、绑定启停/改绑/轮换、接管/恢复、凭证导出和连接测试。

## 4. 路由合同

```text
GET  /admin/login
POST /admin/login
POST /admin/logout
GET  /admin/
GET  /admin/accounts?page=&limit=&q=
GET  /admin/bindings
GET  /admin/customers
GET  /admin/diagnostics
GET  /admin/settings
GET  /admin/audit

POST /admin/accounts/sync
POST /admin/accounts/create
POST /admin/accounts/{id}/edit
POST /admin/accounts/{id}/delete
POST /admin/bindings/create
POST /admin/bindings/{id}/enable
POST /admin/bindings/{id}/disable
POST /admin/bindings/{id}/rebind
POST /admin/bindings/{id}/rotate
POST /admin/bindings/{id}/verify
POST /admin/bindings/{id}/export
POST /admin/customers/{id}/handover
POST /admin/customers/{id}/recover
```

如确有内部 JSON 调用需要，可将同一 handler 映射到 `/admin/api/v1/*`；这不是公共 API，不增加独立 SDK 或管理令牌体系。写操作使用 POST-Redirect-GET。

## 5. 鉴权、并发与幂等

- 单管理员角色；密码只保存 Argon2id/bcrypt 哈希的环境变量引用。
- 会话使用随机服务端 token，Cookie 设置 `HttpOnly`、`Secure`、`SameSite=Strict`、`Path=/admin` 和 TTL；重启后会话全部失效，会话数量有上限。
- 所有管理 GET 和 POST 都鉴权；POST 必须 CSRF token，并检查可信 Origin/Referer。
- 删除、凭证导出、改绑和轮换需要短时二次认证。
- 写操作提交当前 revision 和 `Idempotency-Key`。同键同参数返回原结果，同键不同参数返回 409，不重复调用微信。
- revision 冲突返回 409，要求刷新后重新确认；不能仅靠前端禁用按钮防重复。
- 管理查询必须带单企业范围和对象范围，路径中的 ID 不能直接改变查询边界。

## 6. 数据与实现分期

现有 JSON 配置作为首次 bootstrap；后台保存后的企业和绑定配置以 SQLite 为准。实现前应补充：客服账号资料、管理会话、管理操作/幂等结果和分页查询。真实 Secret 使用主密钥密文或外部引用，后台只展示掩码。

建议分期：

1. 先完成 state migration、单实例锁、不可变身份和 revision fencing。
2. 实现管理会话、CSRF、审计和只读概览/诊断。
3. 接入客服账号同步、创建、编辑、删除和官方链接。
4. 接入绑定启停、改绑、轮换、挑战验证和一次性凭证导出。
5. 接入客户接管/恢复和 UNKNOWN 人工处置。

真实账号 CRUD 必须经过 runtime 的 WeComAdapter/tokenCache 调用官方 `/cgi-bin/kf/*`，浏览器不能接触 real access_token，也不能复用 cc-connect 的兼容 `/cgi-bin` 接口代替官方管理接口。

## 7. 最小验收项

- 无企业列表、无租户切换；单企业所有页面可用。
- 未登录访问任何管理 GET/POST 均被拒绝，健康探针仍按设计可公开。
- CSRF、会话过期、重启失效、越权对象 ID、revision 冲突、重复提交均有测试。
- 客服账号分页失败不清空已有结果；外部成功响应丢失进入 UNKNOWN，不盲目重建或重发。
- 停用/解绑不删除微信账号，不抹除审计；删除有一次性危险确认。
- 凭证、token、完整客户正文不出现在 HTML、URL、日志、导出或错误响应中。
- callback 和头像/链接相关请求使用同一 SSRF allowlist，逐跳拒绝重定向。
- 真实测试企业完成账号 CRUD、链接、客服状态和接管/恢复联调后，才可更新为可发布状态。
