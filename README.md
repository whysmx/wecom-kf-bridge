# WeCom KF Bridge

微信客服与未修改的 cc-connect 之间的独立协议转换网关。

> 当前实现基线 · 2026-10-09。各专题文档共同组成一套现行设计，不另维护修改记录或并行旧方案。代码已实现可运行的网关主链路（见下文“当前实现状态”）；文档描述目标设计，未实现部分在该节明确列出。

## 定位与可行性

```text
个人微信客户（多个客户可同时对话）
    ↕ 微信客服回调 / 客服 API
WeCom KF Bridge（一个网关进程，并发处理不同客户）
    ↕ 自建应用兼容 HTTP API / 加密 XML 回调
cc-connect（不改源码，现有 WeChat Work 配置）
    ↕
Codex 与只读项目资料
```

**单进程指网关只部署一份运行中的程序，不是只允许一位客户，也不是所有客户排队轮流等 AI 回答。** 网关在同一进程内并发处理不同客户；客户 A 等待回答时，仍可接收和处理客户 B、C 的请求。cc-connect、Codex 是独立运行的执行端，不受“网关单进程”数量约束。实际同时生成能力还受执行端配置、服务器资源及平台额度限制，必须联调验证。

主路线有固定源码支持，可按小项目实施；不等于已经完成真实微信联调。上游 `chenhg5/cc-connect@dfad19415a38b00b2c5c288610784d1a7eef337f` 已有 `api_base_url`，不必新增平台插件或修改客户端。微信接口以 WxJava 固定源码交叉核对；证据和剩余核验项见[技术评审](docs/15-feasibility-review.md)与[依据登记](docs/14-sources-and-verification.md)。

在 cc-connect 选择 WeChat Work 的 HTTP 自建应用配置，通过高级 `api_base_url` 指向网关并填入虚拟凭证。**网关还必须能访问 cc-connect 的回调地址**；API 基础地址不是反向隧道。不要改成智能机器人 WebSocket、个人微信扫码或 OneBot。

网关不调用模型、不运行 Codex、不管理项目目录或大模型凭证。现有模型及中转站配置保持在执行端。

## 小项目实现范围

推荐单企业、单实例、单网关进程、SQLite、同进程中文页面。通过进程内并发任务处理多个客户，不引入 Redis、独立消息队列、微服务、租户运营平台或另一个客服工作台；不把逻辑模块强制拆成服务或大量数据库表。

保留真实客服账号同步、创建、编辑、删除、客服链接、项目绑定、启停和凭证管理；保留客户昵称、文本及既定媒体目标、人工接管、持久化去重、故障恢复和必要审计。账号、客户和绑定数量不写死，资源限额及平台配额仍须控制。

不同客户使用独立会话身份；同一客户只在需要保持顺序或更新状态时局部串行，不用全局锁包住整轮 AI 对话。worker 数限制的是网关某类后台任务的并发量，不是客户数或整个系统的 AI 会话数。容量达到上限时采用有界排队和明确限流，不承诺无限并发。

R-001～R-024 及整体/每业务模块严格 >95% 的覆盖率要求保留。具体实现基线见[架构决策](docs/adr/0001-design-baseline.md)。

## 需要接受的边界

HTTP 回调 200 不代表 AI 已处理；客户端会过滤早于本次进程启动时间减 2 秒的消息，重启后不保证补答历史问题。未知投递/发送默认不盲重试，不承诺端到端 exactly-once。

长回答会拆成多次发送，必须考虑客服窗口与条数限制。人工接管抑制后续发送，但不能撤回已在途消息或停止正在运行的 Codex；恢复采用新代际 UID，会新建客户端上下文。

只读 Codex 不等于禁止 cc-connect 的管理命令，也不等于客户间文件权限隔离。昵称缓存、媒体 MIME 和下载错误的客户端限制同样需要验证，不能宣称“完全继承且没有差异”。

## 文档导航

| 文档 | 用途 |
|---|---|
| [AGENTS](AGENTS.md) / [文档地图](docs/00-document-map.md) | 实现约束、阅读顺序与现行设计 |
| [需求规格](docs/01-requirements.md) | R-001～R-024，包括多客户并发对话 |
| [总体架构](docs/02-architecture.md) | 单进程并发、网络方向与职责 |
| [cc-connect 契约](docs/03-cc-connect-contract.md) | 虚拟接口、XML、分块及客户端限制 |
| [微信客服契约](docs/04-wechat-kf-contract.md) | SDK 交叉证据、接口、权限与待实测项 |
| [客服账号管理](docs/05-account-management.md) | 真实 CRUD、链接和绑定 |
| [身份与昵称](docs/06-customer-identity.md) | 稳定身份、缓存和代际 |
| [可靠性与人工接管](docs/07-delivery-and-handover.md) | 游标、重试、UNKNOWN 和人工栅栏 |
| [中文管理](docs/08-admin-ui-and-api.md) | 简单页面及内部操作合同 |
| [数据配置安全](docs/09-data-config-security.md) | 最小持久化、访问控制与恢复 |
| [测试门禁](docs/10-testing-and-coverage.md) | >95% 统计口径与实际证据要求 |
| [实施计划](docs/11-implementation-plan.md) | 文本并发及安全、管理、媒体和发布 |
| [验收矩阵](docs/13-acceptance-matrix.md) / [兼容边界验收](docs/15-feasibility-review.md) | AT-001～AT-070，当前均待执行 |
| [部署运维](docs/12-deployment-runbook.md) | 网络、问答配置、Windows 与故障处理 |
| [依据登记](docs/14-sources-and-verification.md) / [技术评审](docs/15-feasibility-review.md) | 固定来源、结论及未完成项 |
| [架构决策](docs/adr/0001-design-baseline.md) | 唯一现行实现基线与取舍 |
| [Agent 实现手册](docs/16-agent-implementation-playbook.md) | 防止实现偏移、日志和测试门禁 |
| [任务模板](docs/templates/task-brief.md) / [测试报告](docs/templates/test-report.md) | 可按实际任务简短填写，不编造结果 |

## 当前交付状态

当前实现状态（2026-10-10，以代码和测试为准）：

- 已接通：`cmd/wecom-kf-bridge` 读取 JSON 配置（`WECOM_KF_BRIDGE_CONFIG`，示例 `config.example.json`，密钥只从环境变量读取），打开 SQLite，挂载 cc-connect 兼容 API（`/cgi-bin/gettoken`、`/cgi-bin/user/get`、`/cgi-bin/message/send`）、按租户路由的微信客服回调 `/webhooks/wechat-kf/{tenant_key}`、健康检查，并启动 sync/delivery worker；启动时回收中断的投递和发送。配置缺失或无效时进程直接退出，不会只起健康检查。
- 已实现：先写 outbox 再发送、代际/接管栅栏、官方接待状态、48h/5 条窗口与预算、70006/70007/70008 区分、分块逐块记录；客户正文与拉取 token 落库加密；CompatMsgId 持久序列；origin 过滤；常量时间比较；令牌数量上限。
- 未实现/未验证：管理后台、Windows 服务、真实企业微信全链路联调、对账任务（DELIVERY_UNKNOWN/UNKNOWN 只停在待核查）、`service_state_map` 需在测试企业中按官方取值配置。
- 质量门禁：本地 `go build ./...`、`go vet ./...`、`go test -race ./...` 通过；CI 的严格覆盖率门禁（>95%）当前为约 92.9%，**未通过**，需补测试。不得据此宣称已完成生产联调。

