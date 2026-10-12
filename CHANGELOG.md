# 更新日志

本文件记录项目各版本的主要变更，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循语义化版本。每个发布版本必须在此有对应小节，发布流程会将该小节作为 GitHub Release 说明（见 README“发布流程”）。

## [未发布]

### 修复

- 依赖：`modernc.org/sqlite` v1.29.8 → **v1.60.1**（配套 `modernc.org/libc` v1.77.1）；含 WAL `-shm` 读故障不再进程崩溃、context 取消后 Rows 泄漏导致 checkpoint 饿死等修复。Go 工具链升至 **1.26**（驱动要求）。本地 `go test -race -count=20 ./admin ./state ./runtime` 压测通过（#6）。
- 系统设置写回 `config.json`：Windows 上目标已存在时 `os.Rename` 会失败；改为 Remove+Rename 回退（`replaceFile`），并拒绝空/`{}` 的 `service_state_map` 以免误清空；热加载只调用一次 `ApplyEditableSettings`（#25 / #26）。

### 新增

- 管理后台绑定页：对 `config.json` 中已有绑定可在线编辑 `project_id` / `virtual_corp_id` / `agent_id` / `callback_url` / `disabled`，写回配置并热加载（CSRF + 二次认证）；身份字段与密钥 env 只读；审计 `config_save`（#29）。
- 管理后台支持 `admin.listen` 绑定 `0.0.0.0`（须显式 `admin.origin` + `insecure_cookie` 方可明文 http）；新增 `admin.lan_no_login`（默认 `true`）：私网/回环客户端免密进控制台，CSRF/同源保护保留，免登会话跳过 step-up 输密，审计标明 `admin-lan@` / `lan_no_login`；非私网仍须密码登录。
- 管理后台「系统设置」可在线保存：`wecom.treat_48002_as_ai_eligible`（二次认证 + 危险提示 + 强制审计）、`wecom.service_state_map`、`server.public_base_url`；写回 `config.json` 并热加载，无需重启；密钥类字段仍只显示环境变量名（#25）。

## v0.1.7 - 2026-10-12

### 新增

- 入站文件/图片/语音：解析 `kf/sync_msg` 嵌套 `media_id`（`image`/`file`/`voice`），投递给 cc-connect 的加密 XML 带真实 `MsgType` + `MediaId`（`file` 另带 basename 安全的 `FileName`）；非法类型仍标 `UNSUPPORTED`（#17）。
- 兼容接口 `GET /cgi-bin/media/get`：校验虚拟 `access_token` 后，用真实 corp token 代理企业微信临时素材下载；透传 body 与 `Content-Disposition`/`Content-Type`；单文件上限 25MB；日志只记 `media_id` 前缀与长度，不落 token/完整 media_id/二进制（#17）。

### 修复

- 配置 `bindings[].disabled: true` 对**已存在**的 SQLite 绑定生效：启动 bootstrap 会把 `active`/project/callback 与配置对齐并升 revision；`disabled→inactive` 记 `binding_config_disabled`。配置中的绑定以 JSON 为准（控制台对该绑定的停用/改绑在下次启动会被配置覆盖）；仅存在于库中的控制台绑定不受影响（#18 / #19）。
- SQLite WAL 下 `BEGIN DEFERRED` 读后写不再返回 `SQLITE_BUSY_SNAPSHOT`（517）：`OpenWithOptions` DSN 增加 `_txlock=immediate`，写锁冲突由 `busy_timeout` 等待；连接钩子（foreign_keys / busy_timeout）不变（#11 / #20）。
- 后台转人工：官方已成功但本地 `BeginHandover`/`SetHandoverStatus` 失败（含 SQLITE_BUSY/517）时，对 BUSY 有限重试；仍失败返回 **503**（不再误导性 409），flash 明确「官方已转人工，本地状态保存失败…」，并写审计 `official_ok_local_failed`（#10 / #21）。
- 空 `sync_msg` 页（count=0 且 inserted=0）不再打 `wecom_sync_page_ok` INFO，避免空闲轮询刷盘；有消息或有入库的页仍记（#13 / #23）。

### 已知不足

- 部分企业应用对 `kf/service_state/get` 仍返回 48002（无权限）；文本联调依赖 `treat_48002_as_ai_eligible`，**严禁生产开启**。
- 一对一会话能力在上游（cc-connect / Codex），本网关不实现。
- 管理后台 Web 配置页列入下一阶段，本版本未包含。

## v0.1.6 - 2026-10-12

### 诊断

- 真机验收 INFO：回调验签解密并受理记 `wecom_callback_accepted`（method/msg_type/wecom_event/open_kfid/has_token/create_time，不含 Token 与客户标识）；`sync_msg` 每页提交成功记 `wecom_sync_page_ok`（binding_id/scope/count/has_more/page/inserted/errcode）；`send_msg` 上游接受并落库 chunk 记 `wecom_send_msg_ok`（binding_id/msgid/outbox_id/chunk/chunks/errcode）。均不记录正文、昵称或 external_userid。字段名用 `wecom_event`（不用 `event`），避免覆盖结构化日志的 event 名（#12、#15）。
- 配置项 `wecom.treat_48002_as_ai_eligible`（默认 `false`）：**仅联调**。开启后，`kf/service_state/get` 返回 48002 时记 `WARNING_service_state_48002_bypassed` 并返回可区分结果 `AI_ELIGIBLE_48002_BYPASS`：**仅发送路径**视为可发送（不再 70007）；入站 OfficialState / auto-recover **不会**据此清除自动栅栏。本地 HUMAN/WAITING 栅栏下发送仍拦截。其它 errcode（含 48001）/ 网络错误不变。启动记 `WARNING_treat_48002_as_ai_eligible`。**严禁生产开启**（#9）。
- `kf/service_state/get` 各类失败分别记录日志：`service_state_upstream_errcode`（含官方 errcode/errmsg、open_kfid；48002 附权限排查提示）、`service_state_call_error`（网络/解析）、`service_state_route_error`、`service_state_unmapped`（含官方 service_state 原始整数）、`service_state_not_ai`；日志不含 token、external_userid 或正文。
- 启动时若 `wecom.service_state_map` 为空或缺少 AI_ELIGIBLE / WAITING_HUMAN / HUMAN，记录 `WARNING_service_state_map_incomplete`（未映射的官方状态一律视为 UNKNOWN，回复会被拦截）。

### 修复

- 回调受理 INFO 的结构化 event 名不再被 LogFields 的企微 `Event` 覆盖：LogFields 改为 `wecom_event`，StructuredLogger 在合并字段后强制写回 `event`/`ts`/`level`（#15 / #16）。
- SQLite 连接被取消的 ctx「污染」（#5 / #7）：modernc.org/sqlite v1.29.8 在语句执行中 ctx 被取消时会对连接调用 sqlite3_interrupt，中断可能泄漏到同一池化连接的下一条语句，导致无关请求（含关停收尾、其他 HTTP 请求）偶发 `interrupted (9)`；复现 3000 次中约 2700 次失败。现在 Store 通过包装驱动不把取消传给 SQLite（取消仍在语句开始前和行之间由 database/sql 生效；语句很短，锁等待由 busy_timeout 限制）。
- 投递 worker 在 cc-connect 已返回 200 后若恰逢关停取消 ctx，`HTTP_ACCEPTED` 写入失败，消息停在 POSTING、重启后可能重复投递（#5 / #7）。状态迁移现在使用脱离取消的 ctx 加 5 秒超时。
- 启动迁移超时由 10s 调整为 60s，避免高负载下 `New()` 中 PRAGMA/迁移偶发超时（#8）。
- 真实 `kf/sync_msg` 中 `msgtype=event` 的 `event` 是对象（如 `{"event_type":"enter_session",...}`），此前按字符串解析导致整页同步失败（`sync_failed ... cannot unmarshal object`）。现在 `event` 按原始 JSON 保存（事件只记录、不投递），并可读取 `event_type`（#1）。
- `msg_list` 改为逐条解码：单条无法解析的消息不再拖垮整页，按其 msgid 记为 IGNORED（`unparseable`）并告警（不记录正文），其余消息照常入库投递，游标照常推进；无 msgid 的坏条目只告警跳过，避免同一页无限重试（#1）。
- 发送前官方 `service_state` 非 AI 时写入的本地栅栏此前永不自动解除（`SetHandoverStatus` 拒绝写回 AI_ELIGIBLE），会话结束后客户再次来信也一直 HELD，只能后台手工恢复。现在：官方 4（CLOSED）只拦截本次发送、不写持久栅栏；2/3/未知写入「自动栅栏」（`customers.fence_source='auto'`）；客户再次来信时投递前查询官方状态，若为 0/1 则轮换代际清除栅栏并以新 UID 投递该条（旧 UID/旧 outbox 失效，接管期间已 HELD 的消息保持 HELD）。后台手工转人工为 `manual`，永不自动解除，只能后台恢复。旧库迁移：最近一次接管事件来自 `service_state_precheck` 或状态为 CLOSED 的视为 auto，其余视为 manual（#3）。

### 文档

- 精简对外文档：README 改为项目介绍；保留架构概览与配置参考；移除设计期专题与评审明细归档（版本说明以本文件为准）。

### 已知不足

- 部分企业应用对 `kf/service_state/get` 仍返回 48002（无权限）；文本联调依赖 `treat_48002_as_ai_eligible`，**严禁生产开启**。
- 一对一会话能力在上游（cc-connect / Codex），本网关不实现。
- 管理后台 Web 配置页、入站文件/图片投递（透传 MediaId / 代理 `media/get`）列入下一阶段，本版本未包含。
- 非文本入站消息仍会入库，但当前标为不支持、不投递给上游。

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
