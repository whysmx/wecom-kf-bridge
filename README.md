# WeCom KF Bridge

微信客服与未修改的 cc-connect 之间的独立协议转换网关。

> 文档版本 v0.2 · 2026-10-09。当前只有需求、设计和评审文档，没有业务实现、运行程序或已通过的测试报告。

## 定位与评审结论

```text
个人微信客户
    ↕ 微信客服回调 / 客服 API
WeCom KF Bridge
    ↕ 自建应用兼容 HTTP API / 加密 XML 回调
cc-connect（不改源码，现有 WeChat Work 配置）
    ↕
Codex 与只读项目资料
```

**主路线有固定源码支持，可按小项目实施；不等于已经完成真实微信联调。** 上游 `chenhg5/cc-connect@dfad19415a38b00b2c5c288610784d1a7eef337f` 已有 `api_base_url`，不必新增平台插件或修改客户端。微信接口另以 WxJava 固定源码交叉核对；证据和剩余核验项见[评审](docs/15-feasibility-review.md)与[依据登记](docs/14-sources-and-verification.md)。

在 cc-connect 选择 WeChat Work 的 HTTP 自建应用配置，通过高级 `api_base_url` 指向网关并填入虚拟凭证。**网关还必须能访问 cc-connect 的回调地址**；API 基础地址不是反向隧道。不要改成智能机器人 WebSocket、个人微信扫码或 OneBot。

网关不调用模型、不运行 Codex、不管理项目目录或大模型凭证。现有模型及中转站配置保持在执行端。

## 小项目实现范围

推荐单实例、单进程、SQLite、同进程中文页面，首版按单企业部署。不引入 Redis、独立消息队列、微服务、租户运营平台或另一个客服工作台；不把逻辑模块强制拆成服务或大量数据库表。

保留真实客服账号同步、创建、编辑、删除、客服链接、项目绑定、启停和凭证管理；保留客户昵称、文本及既定媒体目标、人工接管、持久化去重、故障恢复和必要审计。账号、客户和绑定数量不写死，资源限额及平台配额仍须控制。

原 R-001～R-024 及整体/每业务模块严格 >95% 的覆盖率要求保留。简化的是实现形态，不是删除功能、安全或测试。具体见 [ADR-0002](docs/adr/0002-small-project-baseline.md)。

## 需要接受的边界

HTTP 回调 200 不代表 AI 已处理；客户端会过滤早于本次进程启动时间减 2 秒的消息，重启后不保证补答历史问题。未知投递/发送默认不盲重试，不承诺端到端 exactly-once。

长回答会拆成多次发送，必须考虑客服窗口与条数限制。人工接管抑制后续发送，但不能撤回已在途消息或停止正在运行的 Codex；恢复采用新代际 UID，会新建客户端上下文。

只读 Codex 不等于禁止 cc-connect 的管理命令，也不等于客户间文件权限隔离。昵称缓存、媒体 MIME 和下载错误的客户端限制同样需要验证，不能宣称“完全继承且没有差异”。

## 文档导航

| 文档 | 用途 |
|---|---|
| [AGENTS](AGENTS.md) / [文档地图](docs/00-document-map.md) | 实现约束、阅读顺序与版本优先级 |
| [需求规格](docs/01-requirements.md) | R-001～R-024，保留原功能范围 |
| [总体架构](docs/02-architecture.md) | 单进程、网络方向与职责 |
| [cc-connect 契约](docs/03-cc-connect-contract.md) | 虚拟接口、XML、分块及客户端限制 |
| [微信客服契约](docs/04-wechat-kf-contract.md) | SDK 交叉证据、接口、权限与待实测项 |
| [客服账号管理](docs/05-account-management.md) | 真实 CRUD、链接和绑定 |
| [身份与昵称](docs/06-customer-identity.md) | 稳定身份、缓存和代际 |
| [可靠性与人工接管](docs/07-delivery-and-handover.md) | 游标、重试、UNKNOWN 和人工栅栏 |
| [中文管理](docs/08-admin-ui-and-api.md) | 简化页面及内部操作合同 |
| [数据配置安全](docs/09-data-config-security.md) | 最小持久化、访问控制与恢复 |
| [测试门禁](docs/10-testing-and-coverage.md) | >95% 统计口径与实际证据要求 |
| [实施计划](docs/11-implementation-plan.md) | 先验证文本及安全，再补管理、媒体和发布 |
| [部署运维](docs/12-deployment-runbook.md) | 网络、问答配置、Windows 与故障处理 |
| [验收矩阵](docs/13-acceptance-matrix.md) / [追加验收](docs/15-feasibility-review.md) | AT-001～AT-069，当前均待执行 |
| [依据登记](docs/14-sources-and-verification.md) / [技术评审](docs/15-feasibility-review.md) | 固定来源、发现、结论及未完成项 |
| [ADR-0001](docs/adr/0001-design-baseline.md) / [ADR-0002](docs/adr/0002-small-project-baseline.md) | 原始决策和小项目修订 |
| [任务模板](docs/templates/task-brief.md) / [测试报告](docs/templates/test-report.md) | 可按实际任务简短填写，不编造结果 |
| [修改记录](CHANGELOG.md) | 文档修订历史 |

## 当前交付状态

本次完成设计评审和文档修订，未修改其他仓库、未操作真实客服账号、未部署或配置生产凭证。腾讯官方页面本次未取得可验证正文；不能把 SDK 注释中的数字或测试 fake 当作最新官方政策。外部契约可按当前能力切片核验，真实功能发布前必须完成相应企业权限及接口联调。
