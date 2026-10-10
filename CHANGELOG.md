# 更新日志

本文件记录项目各版本的主要变更，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循语义化版本。每个发布版本必须在此有对应小节，发布流程会将该小节作为 GitHub Release 说明（见 README“发布流程”）。

## [未发布]

### 文档

- 精简对外文档：README 改为项目介绍；保留架构概览与配置参考；移除设计期专题与评审明细归档（版本说明以本文件为准）。

## v0.1.5 - 2026-10-10

### 修复

- 后台诊断查询限定本企业，标记诊断项前校验归属，其他企业的对象返回 404（#39）。
- 凭证轮换：新密钥与 revision 同一事务提交，提交后才应用到运行时；应用失败恢复旧密钥，旧凭证继续可用（#40）。
- 绑定虚拟凭证入库后，重启不再需要旧的环境变量；新库缺少环境变量时给出明确错误（#41）。
- 官方列表中已不存在的客服账号标记 UNKNOWN，禁止新建绑定和发送（#43）。

### 安全

- `wecom.api_base_url` 拒绝 userinfo、query、fragment；日志、设置页和 HTTP 传输错误中的 URL 已脱敏，不再带出 access_token（#42）。

### 新增

- 一次性导出包含 cc-connect 所需全部字段（agent_id、port、callback_path、api_base_url、allow_from 等）；新增可选配置 `server.public_base_url`（#45）。

### 构建

- 发布工作流在汇总任务中一次生成覆盖全部 6 个安装包的 `SHA256SUMS`（#44）。

### 兼容性

- 配置绑定的虚拟凭证首次启动后保存在数据库，之后以数据库为准；修改配置中的凭证/env 不再生效，请在后台轮换。
- 仍然读取旧版 `SHA256SUMS` 的脚本：现在只有一个汇总文件，包含所有包。

### 已知不足

- 尚未真实联调，非生产就绪；限制摘要见 README。

## v0.1.4 - 2026-10-10

### 修复

- 绑定内客户换代持有与发送相同的绑定锁，换代不会插在发送检查与出站调用之间（#30）。
- `BlockOutbox` 改为 CAS，不会覆盖 SENDING/UPSTREAM_ACCEPTED，预算仅在真实转移时释放一次（#31）。
- 删除客服账号（含结果未知）时停用关联绑定、吊销 cc-connect 令牌并冻结旧客户 UID（#32）。
- 转人工/恢复 AI 先调用官方 `service_state/trans` 并经 `service_state/get` 回读确认后才改本地状态；结果不确定标记 UNKNOWN 并显示（#33）。
- SQLite 改用连接钩子初始化每个连接（`foreign_keys`、`busy_timeout`），并设置连接池上限（#38）。

### 安全

- 后台 Cookie 默认 Secure；新增 `admin.insecure_cookie`，仅回环监听可用（修复示例配置登录循环）（#34）。
- `wecom.api_base_url` 强制 https，`wecom.allow_insecure_http` 仅供测试并在启动时告警（#35）。
- `security.callback_targets` 的 host/端口/CIDR 启动时严格校验，非法即启动失败（#36）。
- `runtime.Build` 自行应用默认值并校验配置，空企业列表不再 panic（#37）。

### 变更（配置）

- 用 http 访问后台需显式设置 `admin.insecure_cookie: true`（且仅限回环）；否则 `admin.origin` 必须为 https。
- 指向 http 的 WeCom API（如本地 fake）需设置 `wecom.allow_insecure_http: true`。
- 后台转人工/恢复依赖 `wecom.service_state_map` 中 WAITING_HUMAN / AI_ELIGIBLE 对应的官方值。

### 已知不足

- 与 v0.1.3 相同：尚未完成真实企业微信联调，非生产就绪。

## v0.1.3 - 2026-10-10

### 修复

- SQLite 的 `foreign_keys` 与 `busy_timeout` 改为在 DSN 中设置，连接池中每个连接都生效（此前只作用于首个连接，并发写入可能出现 SQLITE_BUSY，其他连接不执行外键约束）。

### 已知不足

- 与 v0.1.2 相同：尚未完成真实企业微信联调，非生产就绪。

## v0.1.2 - 2026-10-10

### 新增

- 单企业管理后台（docs/17）：独立监听（默认 `127.0.0.1:8091`，配置 `admin`，未配置不启动），包含概览、客服账号、转发绑定、客户与接管、消息诊断、系统设置、操作审计七个页面。
- 后台安全：bcrypt 密码哈希（环境变量）、服务端会话（HttpOnly/Secure/SameSite=Strict/Path=/admin，重启失效、数量上限）、登录限流、CSRF + Origin/Referer 校验、危险操作二次认证、Idempotency-Key 重放/冲突 409、revision 冲突 409、操作审计、no-store/CSP 响应头。
- 客服账号经服务端 token 调用官方 `/cgi-bin/kf/*` 同步、新建、改名、生成链接、一次性票据删除；响应丢失记为 UNKNOWN，不自动重建。
- 转发绑定新建、启停、改绑（全部客户换代际）、虚拟凭证轮换（加密存储、一次性导出）、回调挑战验证；客户转人工/恢复 AI；诊断默认不显示正文，查看正文需二次认证并审计，只能标记不能重发。
- 单实例文件锁，防止多个进程共用同一 SQLite（#26）。
- 发布说明从 CHANGELOG 自动提取（`scripts/release_notes.sh`），缺少版本小节时发布失败。

### 变更

- 架构接通：主程序加载配置、打开 SQLite、挂载兼容 API、租户回调 `/webhooks/wechat-kf/{tenant_key}`、健康检查和 sync/delivery worker（#1）。
- JSON 配置只做首次导入，之后以 SQLite（后台修改）为准；身份字段变化拒绝启动。
- 微信客服 HTTP 客户端复用连接，增加令牌桶限流和安全重试（#22）。

### 修复

- 评审问题 #1–#29 全部修复，主要包括：
  - 发送链路先写 outbox，检查代际、接管栅栏、接待状态、窗口与预算，区分 70006/70007/70008，分块逐块记录（#2–#4）。
  - 安全：常量时间比较、令牌上限、未授权客户不可查昵称、接收方必校验、XML 安全编码、拉取 token 与客户正文加密落库（#5–#12）。
  - 协议正确性：MsgId 持久序列、缺 msgid 不伪造、origin 过滤、has_more 不丢 pending、游标停滞告警、去 Markdown、统一加解密（#13–#19）。
  - 并发与工程：锁回收、移除生产中的 MemoryAdapter、文档状态一致（#20、#21、#23）。
  - 身份字段不可变（数据库触发器）、outbox 记录 BindingRevision 并逐块复核、SQL 条件更新（CAS）、回调 SSRF 按目标主机绑定 CIDR、DeliveryWorker 迁移失败不再当成功（#24、#25、#27–#29）。

### 质量

- CI 严格覆盖率门禁：总计及每个包语句覆盖率均 >95%（当前总计约 96.0%），`go vet`、`gofmt`、`go test -race` 全部通过。

### 已知不足（非生产就绪）

- 尚未在真实企业微信测试企业完成全链路联调（账号增删改、链接、接管/恢复、真实发送），不可视为可发布到生产。
- 系统设置只读，不支持在线保存；头像需填写已上传的 media_id；接管/恢复只改本地状态，不调用官方 `service_state/trans`。
- 无对账任务，DELIVERY_UNKNOWN/UNKNOWN 只停留在待核查；`service_state_map` 需按官方取值配置；Windows 文件锁仅交叉编译验证，未做 Windows 服务。

## v0.1.1 - 2026-10-10

### 文档与配置

- 完成单企业管理后台设计文档，明确管理界面、路由、鉴权、审计、诊断和分期实施边界。
- 补充文档导航和 README 入口，便于从项目首页查阅设计资料。
- 当前版本为文档与配置基线版本，未引入新的业务代码变更。

### 发布包

打 v0.1.1 标签后，GitHub Actions 会自动构建并发布以下基础平台包：

- Linux amd64
- Linux arm64
- Windows amd64
- Windows arm64
- macOS amd64
- macOS arm64

每个压缩包包含对应可执行文件、配置示例、README、完整 docs 和 SHA256SUMS 校验文件。Windows 使用 ZIP，Linux/macOS 使用 tar.gz。

### 发布说明

- GitHub Release 会同时提供源码压缩包和上述平台发布包。
- 配置示例、架构说明、测试与部署文档均随源码保留。
- 后续版本将根据管理后台实施进度继续更新本文件。
