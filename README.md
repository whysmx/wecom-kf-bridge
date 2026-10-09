# WeCom KF Bridge

微信客服与 cc-connect 之间的独立协议转换网关。

> 文档版本：v0.1（首版详细设计） · 2026-10-09  
> 当前状态：**仅需求、设计与验收文档；没有业务实现、运行程序或已通过的测试报告。**

## 项目定位

```text
微信客户
  ↕ 微信客服
企业微信官方回调与客服 API
  ↕
WeCom KF Bridge（本项目，待开发）
  ↕ 企业微信自建应用兼容 HTTP API + 加密 XML 回调
cc-connect（不修改源码）
  ↕
Codex 与现有项目资料
```

本项目不调用大模型、不运行 Codex、不管理代码工作目录，也不是 cc-connect 插件。它在 cc-connect 一侧兼容**企业微信自建应用 HTTP 模式**，不是智能机器人 WebSocket 模式。

cc-connect 仍选择 **WeChat Work → 手动配置**。通过现有高级选项 `api_base_url` 指向网关，并填写网关生成的兼容凭证。反方向由网关 HTTP POST 到 cc-connect 的回调地址，因此网关必须能访问该地址；只修改 API 基础地址不会自动建立反向长连接。

以上配置与协议行为依据已检查的 [cc-connect 固定基线](https://github.com/whysmx/cc-connect/tree/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8)，不是对所有版本的无条件兼容承诺。

## 首版目标

- 不修改 cc-connect 源码或 Web 配置表单，复用项目、会话与 Codex 能力。
- 客服账号按需配置，不写死客服账号、项目或用户数量。
- 支持客服账号同步、创建、编辑、删除、客服链接、网关启停、项目回调绑定及凭证管理。
- 将客户昵称映射为 cc-connect 的 `UserName`，身份与昵称分离，不因同名串会话。
- 接入人工接管、消息去重、游标恢复、失败处理、媒体适配和必要审计。
- **实现后，项目整体与各业务模块自动化语句覆盖率必须严格大于 95%；关键场景矩阵全部通过。** 当前文档阶段覆盖率为未测，不得宣称达标。

客服账号管理和客户资料查询是两类不同能力。用户指定的 [95166：客户基础信息](https://kf.weixin.qq.com/api/doc/path/95166) 用于客户昵称等信息，不能代替客服账号增删改查接口。

## 文档导航

| 文档 | 内容 |
|---|---|
| [AGENTS.md](AGENTS.md) | 所有实现 Agent 必须遵守的边界、阅读顺序与停止条件 |
| [文档总览](docs/00-document-map.md) | 文档优先级、术语、状态与阅读路径 |
| [需求规格](docs/01-requirements.md) | 编号需求、首版范围与非目标 |
| [总体架构](docs/02-architecture.md) | 模块、网络方向、部署与实现基线 |
| [cc-connect 兼容契约](docs/03-cc-connect-contract.md) | 兼容接口、加密回调、身份和错误语义 |
| [微信客服适配契约](docs/04-wechat-kf-contract.md) | 官方接口清单、核验任务及平台限制 |
| [客服账号管理](docs/05-account-management.md) | 真实账号管理、项目绑定、权限与异常一致性 |
| [客户身份与昵称](docs/06-customer-identity.md) | 客户资料、稳定标识、缓存与名称透传 |
| [消息可靠性与人工接管](docs/07-delivery-and-handover.md) | 状态机、幂等、重试、迟到回复和已知边界 |
| [管理界面与管理 API](docs/08-admin-ui-and-api.md) | 中文界面、操作流程与内部接口 |
| [数据、配置与安全](docs/09-data-config-security.md) | 数据实体、事务、凭证、保留期与安全边界 |
| [测试与覆盖率门禁](docs/10-testing-and-coverage.md) | >95% 统计口径、契约测试、故障注入与发布门禁 |
| [实施任务与阶段](docs/11-implementation-plan.md) | 阶段依赖、任务交付与 Agent 协作 |
| [部署运维](docs/12-deployment-runbook.md) | 现有表单配置、Windows 联调、监控和恢复 |
| [需求—测试—验收矩阵](docs/13-acceptance-matrix.md) | 可追踪验收用例与完成定义 |
| [依据与待核实清单](docs/14-sources-and-verification.md) | 已读源码、官方入口、不可读内容与阻塞项 |
| [架构决策](docs/adr/0001-design-baseline.md) | HTTP 路线、最小管理后台、持久化与实现栈建议 |
| [任务模板](docs/templates/task-brief.md) / [测试报告模板](docs/templates/test-report.md) | 防止需求漂移与虚报测试结果 |

## 状态与使用方式

先读 `AGENTS.md`，再按实施计划逐项推进。需求和内部网关契约是本版设计约定；外部微信 API 的最新字段、权限、额度及状态转换必须完成官方资料核验和真实账号联调后才能标为已确认。

本次未成功读取微信客服官方页面正文。文档明确列出这些待验证项，不用社区文章、旧聊天结论或模型记忆冒充官方最新契约。无法核实的项目阻塞相应功能发布，但不阻止先完成不依赖该信息的内部设计和测试框架。

实现语言未由项目所有者指定。本版给出 Go、SQLite、内嵌轻量中文管理页的建议基线；在开始实现前用 ADR 固定工具链，不允许 Agent 在开发中自行换栈或扩展成微服务平台。

## 本次交付边界

仅建立中文文档基线，不包含业务代码、不创建真实微信客服账号、不修改现有 cc-connect 仓库、不配置生产凭证，也不声称 CI 或覆盖率门禁已经运行。