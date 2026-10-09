# 依据、核验记录与发布阻塞项

> v0.1 · 核查日期 2026-10-09。区分源码检查与实际运行；记录未知不是允许 Agent 随意填空。

## 1. 已核对的一手资料

### S-001：cc-connect HTTP 适配器

[固定源码](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/platform/wecom/wecom.go)

已检查 New、Start、handleVerify、handleMessage、Reply、SendImage、getAccessToken、resolveUserName、downloadMedia、decrypt。由此确认：支持 api_base_url；HTTP 服务独立监听；兼容 API 路径；加密 XML；UserName 读取 name 并缓存；HTTP 200 早于后续处理；client HTTP timeout 为 30 秒；发送请求不携带原问题 ID；媒体下载函数对响应字节的错误辨识有限。

这不是对主程序、所有 GUI 显示、真实 Codex 或官方客服 API 的运行验证。具体客户端旧消息阈值及所有上游版本行为仍须 P0 实验。

### S-002：现有表单

[platformMeta.ts 固定源码](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/web/src/lib/platformMeta.ts)

确认 WeChat Work 基础字段为 corp_id、corp_secret、agent_id、callback_token、callback_aes_key、port；高级字段为 callback_path、api_base_url、allow_from。本项目因此选择 HTTP 自建应用兼容，而不是要求修改表单添加 ws_url。

### S-003：客户端消息/附件

[core/message.go 固定源码](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/core/message.go)

确认已有消息和附件处理能力，运行时可能保存文件；项目资料只读与运行时目录可写需区别。不能由这些函数推断 Codex 能理解所有文件或允许任何写操作。

### S-004：测试统计工具

[Go coverage 文档](https://go.dev/doc/build-cover)、[Go test 文档](https://pkg.go.dev/cmd/go#hdr-Test_packages)。已读取官方正文，用于确认语句覆盖口径、插桩范围和进程集成报告机制。>95%、每模块/diff 门禁是本产品要求，不是 Go 官方规定。

### S-005：备份与网络安全

[SQLite Backup API](https://www.sqlite.org/backup.html)、[OWASP SSRF](https://owasp.org/www-community/attacks/Server_Side_Request_Forgery)。已读取一手正文。本网关的具体备份策略、回调 allowlist、保留时间属于设计约定，不能宣称已通过认证或满足某项法律合规。

## 2. 官方微信资料访问状态

用户指定 [95166](https://kf.weixin.qq.com/api/doc/path/95166) 为客户资料相关入口。本次尝试该页及企业微信官方同编号入口，浏览工具未取得正文；尝试其他官方概述/接口入口也未取得可验证正文。容器直连亦出现域名解析失败。

因此没有将搜索摘要、社区 SDK、博客或旧聊天中的字段、数字、权限当成最新官方事实。没有缓存官方完整文档，更没有执行真实微信账号/消息 API。

开发时应通过可访问的官方控制台/官方页面补齐。仅把相同候选路径放进 mock 并让测试通过，不算官方核验。

## 3. 必须闭环的外部核验任务

| 编号 | 核验内容 | 影响/发布条件 |
|---|---|---|
| V-001 | 实际企业接入方式、Secret 来源、调用权限/API 管理范围、可信 IP/网络条件 | 未明确不得启用真实企业写操作 |
| V-002 | 通知事件、签名/加解密、Token 作用域/有效期、官方回调响应格式/重试条件 | 阻塞官方回调发布 |
| V-003 | sync_msg 请求/响应、游标作用域、分页/时间范围、origin/type、过期恢复 | 阻塞消息同步发布 |
| V-004 | send_msg 文本/媒体、发送窗口/条数/频控、字符单位、可选 msgid 是否有幂等承诺、错误分类 | 阻塞真实发送及自动重试发布 |
| V-005 | 95166 客户昵称字段、批量上限、资料可见性/时效、部分失败语义 | 阻塞“客户昵称透传已验证”声明，不应阻塞内部空名回退测试 |
| V-006 | 客服账号列表/创建/编辑/删除、头像要求、账号配额、接入链接及权限/分页 | 阻塞真实账号管理发布 |
| V-007 | 人工状态枚举/事件/合法转换/接待人员权限、恢复条件 | 阻塞人工接管功能发布；未知状态必须安全暂停 |
| V-008 | 官方媒体上传/下载、类型/大小/有效期、与头像及客服发送的令牌关联 | 阻塞相应媒体能力发布 |
| V-009 | 实际未改 cc-connect 版本及上游对照、旧消息阈值、权限与媒体限制、200/昵称缓存实验 | 阻塞兼容性发布 |
| V-010 | 推荐实现栈、工具链/依赖/许可证、Windows 运行条件、模块覆盖分母 | P0 架构评审后才能固定实现 |

初始状态全部为待完成；S-001～S-003 的源码事实仅作为 V-009 的输入，不等于运行验证完成。

## 4. 核验记录格式

每项记录：编号、官方页面标题/URL、读取日期、适用接入方式、权限范围、方法/路径、字段/枚举及限制、脱敏成功/错误样本、测试环境标签、核验人、结果、相关测试编号和代码提交。不要记录真实 Secret 或完整客户个人资料。

允许保存人工整理的最小字段表与合成 fixture；不要把受版权保护官方页面全文复制进仓库。来源发生变化时保留前后差异和兼容处理，不简单覆盖旧结论。

## 5. 风险与设计处理

| 风险 | 处理 |
|---|---|
| 默认误用 WSS 导致表单无法接入 | 主路线在需求/AGENTS 锁定为 HTTP，测试原表单 |
| 网关只模拟消息发送，没实现反向回调 | 双方向单独契约与网络测试 |
| 客户信息与客服账号混淆 | 资料与账号管理单独文档、API、测试 |
| 无原问题 ID 却宣称 exactly-once | UNKNOWN 状态及发送关联边界明确 |
| 人工恢复后迟到 AI 结果 | generation UID 栅栏；明确会丢跨代际上下文自动接续 |
| 官方发送和人工状态存在竞态 | 前置检查+本地栅栏；在途请求风险不虚报消除 |
| 95% 被 round/缺包/排除刷过 | 原始语句严格比较、分模块、检查器负向测试 |
| 账号列表缺失就被删/重复创建 | 可见性状态、operation 幂等、UNKNOWN 对账 |
| 公开仓库出现真实配置 | 脱敏示例、secret 扫描、实际密钥只在运行环境 |

## 6. 状态维护

功能实现、假服务测试、源码互操作、真实账号联调分栏记录。当前只有文档基线；没有通过覆盖率门禁、没有创建测试账号、没有部署服务。后续 Agent 必须实际运行并附报告后才能更改相应状态。
