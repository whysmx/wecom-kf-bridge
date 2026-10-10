# 评审问题修复记录（#1–#29）

> 按用户要求维护（覆盖 AGENTS.md “不另维护修改记录”的一般规则）。哈希为本地提交。
> 门禁：`go build ./...`、`go vet ./...`、`go test -race ./...` 通过；严格覆盖率门禁（>95%）通过，总计约 96.5%，见文末“覆盖率”条目。

| # | 问题 | 修复 | 主要文件 | 测试 | 提交 | 遗留 |
|---|---|---|---|---|---|---|
| 1 | 入口只挂 Health | JSON 配置加载与校验（密钥走环境变量）、打开 SQLite（主密钥加密）、挂载兼容 API + 租户回调 + 健康、启动 SyncWorker/DeliveryWorker、启动恢复在途记录；配置无效即退出 | runtime/config.go, gateway.go, adapter.go, workers.go, app.go, cmd/.../main.go, config.example.json | runtime/gateway_test.go（端到端：回调→sync→投递 cc-connect→gettoken/user/get/send→微信 send_msg）、units_test.go、app_workers_test.go、main_test.go | 7a3acd5, ef0b7b6 | 无管理后台；对账任务未实现；DELIVERY_UNKNOWN 不阻塞同客户后续消息 |
| 2 | send 不写 outbox、不查栅栏/状态/窗口 | 先 CreateOutbox（违规直接记 BLOCKED），再查官方接待状态，再 MarkOutboxSending 后发送 | protocol.go, state/store.go | protocol_test.go TestSendRegistersOutbox…, TestSendEnforcesGeneration…, state/send_rules_test.go | dab885d, f179dd2 | — |
| 3 | 一律 70008；分块失败不记录 | SendError 分类映射 70006/70007/70008；每块成功即 RecordOutboxChunk，未发块释放预算 | protocol.go, runtime/adapter.go | TestSendErrorCodesAndPartialChunks | f179dd2 | 错误码分类表需真实环境核验 |
| 4 | 内存 map，旧 UID 仍可发 | 客户状态全部走 SQLite；UID 历史表判定 stale | protocol.go, state/store.go | TestSendEnforcesGeneration…, state/send_rules_test.go | dab885d, f179dd2 | — |
| 5 | 非常量时间比较 | secret 用 SHA-256+ConstantTimeCompare；签名用 ConstantTimeCompare | protocol.go, wecom/crypto.go | wecom/crypto_unified_test.go, TestGetTokenCredentials | bfef95e, f179dd2 | — |
| 6 | 令牌只增不删 | 过期清理 + 每绑定/全局上限，淘汰最旧 | protocol.go | TestTokenTableBoundedAndExpiring | f179dd2 | — |
| 7 | 未授权可查昵称 | 未授权直接拒绝；昵称缓存回退 | protocol.go | TestUserGetAuthorizationAndNickname | f179dd2 | — |
| 8 | 回调逐绑定试解密、路径不符 | 公开回调改为 /webhooks/wechat-kf/{tenant_key} 按租户路由；兼容服务不再处理回调 | wecom/router.go, protocol.go | wecom 路由测试, TestNoCallbackRouteOnCompatServer, 端到端测试 | f6b1799, f179dd2 | — |
| 9 | receiver 为空跳过校验 | 空 receiver 返回 ErrReceiver | wecom/crypto.go | crypto_unified_test.go | bfef95e | — |
| 10 | CDATA 手工拼接可注入 | encoding/xml 序列化信封 | wecom/webhook.go, wecom/outbound.go | wecom/webhook_safety_test.go, TestBuildCallbackEncryptsEscapedXML | 07bec3f | — |
| 11 | token/RawXML 未脱敏未加密 | 删除 RawXML；Token 不进 JSON/String/日志；落库密封 | wecom/webhook.go, state/seal.go | webhook_safety_test.go, state/synctoken_test.go | 07bec3f | — |
| 12 | 正文明文落库 | AES-256-GCM 主密钥加密 inbox payload / outbox body | state/seal.go, state/store.go | state/seal_test.go | 1e4f2b9 | 主密钥轮换未实现 |
| 13 | FNV-64 生成 MsgId | 事务内持久递增序列，重复消息保留原 ID | state/store.go, wecom/sync.go | state/compat_test.go, wecom/sync_test.go | 4d36d3e | — |
| 14 | 缺 msgid 编兜底 ID | 返回 ErrMissingMsgID，暂停该 scope 不提交 | wecom/sync.go | wecom/sync_test.go | 4d36d3e | 需人工处理暂停 scope |
| 15 | 无 origin 过滤/客户关联 | 必须显式配置客户 origin；非客户消息记 IGNORED；按 external_userid 关联客户 | wecom/sync.go | wecom/sync_test.go, 端到端测试 | 4d36d3e | origin 取值需官方核验 |
| 16 | has_more 时清 pending | 仅 has_more=0 时清 pending | state/store.go | state/sync_pending_test.go | 6312339 | — |
| 17 | 游标检查不全、页上限报错 | 游标不前进→STALLED 告警；页上限→PAGE_LIMIT 并可续拉 | wecom/sync.go | wecom/sync_test.go | 4d36d3e | — |
| 18 | 未去 Markdown | StripMarkdown 在分块前执行 | markdown.go, protocol.go | markdown_test.go, TestMarkdownIsStrippedBeforeSending | f179dd2 | 正则实现，复杂嵌套仅尽力而为 |
| 19 | 两套加解密 | 根包委托 wecom 包唯一实现 | crypto.go, wecom/crypto.go | crypto_unified_test.go, util_test.go | bfef95e | — |
| 20 | 锁从不回收 | 引用计数 keyedLocks，空闲即回收 | state/locks.go | state 锁测试 | 15a9c09 | — |
| 21 | MemoryAdapter 永远成功 | 移出生产代码；默认 unavailableAdapter 失败关闭；测试用 fakes_test.go | protocol.go, fakes_test.go | TestUnavailableAdapter… | f179dd2 | — |
| 22 | 每次新建 http.Client、无限流重试 | 复用 Client；令牌桶限流；仅对无副作用请求或未连接错误重试 | wecom/client.go | wecom/client_retry_test.go | d78a0a4 | — |
| 23 | 文档完成状态矛盾 | README 新增“当前实现状态”并如实写覆盖率未达标；AGENTS、docs/09 同步 | README.md, AGENTS.md, docs/09 | — | 587372d | 其余专题文档仍描述目标设计 |
| 24 | 同 ID 的绑定/企业配置更新可改身份字段，存量客户被路由到别的企业/客服账号 | 数据库触发器禁止 UPDATE 身份列（enterprises.tenant_id/corp_id、bindings.enterprise_id/open_kfid、customers.enterprise_id/binding_id/external_user_id）；PutEnterprise/PutBinding 先比对并返回 ErrIdentityChanged；启动时配置改了身份即拒绝启动 | state/store.go, runtime/gateway.go | state/identity_cas_test.go TestIdentityIsImmutable, TestIdentityTriggersInstalledOnUpgrade; runtime/review2_test.go TestConfigIdentityChangeRefusedAtStartup | b196fb9, 5563cd6 | 未提供“显式迁移+客户换代”工具；需改身份请用新 ID |
| 25 | outbox 不记 BindingRevision，绑定轮换/停用并发时旧请求仍可发出 | CreateOutbox 写入 binding_revision；MarkOutboxSending 条件更新含 active+revision；每块发送前 SendGuarded 在绑定锁（PutBinding 同锁）下再做条件检查，失败则停发、记 REJECTED `precondition_lost:i/n`、释放未发预算、回 70006 | state/store.go, protocol.go | TestMarkOutboxSendingIsConditional, TestSendGuardedSerialisesWithBindingRotation（并发）, protocol_revision_test.go TestSendStopsWhenPreconditionChangesMidMessage | b196fb9, 0acbc34 | 绑定锁为进程内锁，配合 #26 单实例 |
| 26 | 多进程共用同一 SQLite 会重复跑 worker | 数据库旁 `.lock` 独占 OS 文件锁（unix flock / Windows LockFileEx），进程退出自动释放；第二实例 ErrAlreadyRunning 拒绝启动 | runtime/instance_lock*.go, runtime/gateway.go | TestSingleInstanceLock | 5563cd6 | Windows 实现仅交叉编译验证 |
| 27 | MarkOutboxSending 先读后无条件 UPDATE，接管/恢复可在检查后插入 | 改为单条 SQL CAS（state、generation、uid、AI_ELIGIBLE、fence、binding active/revision），按影响行数判定并给出精确原因；TransitionOutbox/MarkOutboxUnknown/TransitionInbox 加 `AND state=<已校验状态>`，冲突返回 ErrConflict | state/store.go | TestMarkSendingRacesTakeover, TestTransitionsAreCompareAndSet（并发）, TestCompareAndSetLostRace | b196fb9 | — |
| 28 | 回调 SSRF 白名单只看端口和任意 CIDR，主机名未绑定其 CIDR | 按 host:port 建立每目标策略，拨号时解析主机名，仅连接该目标自身 CIDR 内地址；链路本地/组播/未指定地址一律拒绝；无代理、不跟随重定向 | runtime/workers.go | TestCallbackDialPolicyIsPerTarget（同端口 A 不能到 B 的网段） | 5563cd6 | 共用一个 Transport，但策略按目标隔离 |
| 29 | DeliveryWorker 状态迁移失败仍当成功；部分 HELD 分支忽略错误 | step 失败返回 false、不更新本地状态；所有 HELD 分支返回迁移结果，失败即停止该客户后续消息 | runtime/workers.go | TestDeliveryTransitionFailuresStopAndKeepState（每个分支） | 5563cd6 | POSTING 后迁移失败的行由启动恢复处理 |

附带修复：inbox `attempt` 原先每次状态迁移都 +1，导致重试次数被流水线迁移耗尽；改为仅进入 POSTING 时计数（ef0b7b6）。

## 覆盖率（门禁补齐）

- 问题：#1–#23 修复后覆盖率约 92.9%，CI 严格门禁（总计及各包均 >95%）不通过。
- 修复：补充有行为断言的错误路径测试，未排除文件、未改统计口径：
  - state：SQLite 触发器注入故障，验证事务原子性（客户创建/轮换、同步页、outbox 预算预留/释放、分块记录）；旧库迁移与只读库拒绝启动；篡改密文被检出；无主密钥拒绝写入正文；损坏行报错而非跳过；启动恢复失败上报。
  - root（protocol.go）：store 失败绝不回成功；已发出未记录的分块记为 UNKNOWN；未授权/绑定停用/未知官方状态的错误码；随机源失败时不构造回调。
  - runtime：每个密钥缺失/格式错误都拒绝启动；不可打开的数据库、非法 tenant_key；回调落库失败要求企微重试；worker 关停超时上报；sync/delivery 各错误分支（传输中断→DELIVERY_UNKNOWN、URL 非法→RETRY_WAIT、状态迁移失败停止后续）；拨号地址校验；令牌刷新失败。
  - wecom：限流/退避期间 ctx 取消立即停止；Decrypt 拒绝坏填充、过短、长度越界、非 UTF-8。
- 文件：protocol_fault_test.go, runtime/errors_test.go, state/fault_test.go, state/migrate_fault_test.go, wecom/edge_errors_test.go
- 结果：总计约 96.5%；root 96.8%、runtime 97.5%、state 95.1%、wecom 96.3%、cmd 100%。
- 遗留：state 余量小；剩余未覆盖多为事务 Commit 失败等难以确定性注入的分支。

- #24–#29 后复测：总计 96.46%；root 96.9%、runtime 97.6%、state 95.3%、wecom 96.3%、cmd 100%。

## 管理后台（docs/17-single-enterprise-admin-design.md）

范围决策（按文档取最安全的最小选项）：

- 仅在配置 `admin.listen` 时启动，且要求配置中恰好一个企业；未配置时不暴露任何管理面。
- 单管理员，只用密码（bcrypt 哈希来自环境变量）；不做用户名、RBAC。
- 系统设置只读展示 + callback 连接测试；在线保存配置（带 revision）未做，仍改配置文件重启。原因：在线改 API 地址/窗口/worker 参数需要热重载全部组件，范围大且风险高。
- 二次认证 = 在 5 分钟内重新输入密码（`POST /admin/stepup`）；删除、导出、改绑、轮换、查看正文需要。
- 虚拟凭证：新建/轮换时生成并以主密钥加密存入 SQLite（`binding_secrets`），页面上不显示；“一次性导出”经二次认证后通过会话显示一次（不进 URL），再导出需先轮换。配置文件中的绑定凭证仍来自环境变量，直到在后台轮换。
- JSON 配置只做首次 bootstrap：绑定已在 SQLite 中时，可变字段以 SQLite 为准；身份字段变化仍拒绝启动（#24）。后台新建的绑定在重启后从 SQLite 及其加密凭证恢复。

| 设计要求 | 实现 | 测试 | 提交 |
|---|---|---|---|
| §2 独立监听、不进 public listener | `AppConfig.AdminAddr/AdminHandler`、`App.ServeAdmin`；public mux 不挂 `/admin` | runtime `TestAppServesAdminSeparately`、`TestAdminConsoleEndToEnd`（public 上不可访问）；admin `TestUnauthenticatedAccessIsRejected`（后台不提供 /cgi-bin） | db0cb0b |
| §2 不信任 X-Forwarded-For | 审计操作者取 TCP 对端地址 | `TestLoginSessionCookieAndThrottle`（actor） | 2b07aa5 |
| §3 七个页面、无企业选择器、顶部企业名/CorpID 掩码/状态/最近同步 | `admin/templates.go`、`pages.go`；`consoleRuntime.Status` | `TestPagesRenderAndSettingsTest`（全部页面 200、无完整 CorpID、无脚本） | 2b07aa5, db0cb0b |
| §3.1 概览计数与组件状态，不显示密钥/正文 | `Store.Overview`、`Status` | `TestDiagnosticsListsAndOverview`、`TestAdminRuntimeEdges` | 8e852b5, db0cb0b |
| §3.2 账号分页/搜索、同步、新建、改名、备注、官方链接、删除 | `kf_accounts` 表；`consoleKF` 走 runtime tokenCache 调官方 `/cgi-bin/kf/*`；全部页成功后才写入 | `TestAccountsSyncPagingAndFailureKeepsList`（分页失败不清空）、`TestAccountCreateOutcomes`、`TestAccountEditLinkAndRevision`、runtime E2E（真实 wecom.Client 对 fake 官方接口） | 8e852b5, 2b07aa5, db0cb0b |
| §3.2 删除：危险区、绑定账号/动作/revision/2 分钟、一次性票据 | `issueTicket/useTicket` + 二次认证 + 输入 open_kfid 确认 | `TestAccountDeleteTicket`（错误确认、复用、过期、成功、删除不动绑定） | 2b07aa5 |
| §3.2 响应丢失 → UNKNOWN，不按同名合并、不自动重建 | 非 `wecom.APIError` 即视为结果未知，记 UNKNOWN 占位/状态 | `TestAccountCreateOutcomes`、`TestAccountEditLinkAndRevision`、`TestAccountDeleteTicket` | 2b07aa5 |
| §3.3 绑定新建/启用/停用/改绑/轮换/挑战验证/一次性导出 | `UpdateBinding`（revision CAS 并 +1）、`RotateBindingCustomers`、`bridge.Server.ApplyBinding`（吊销该绑定全部令牌）、`VerifyChallenge` | `TestBindingLifecycle`、`TestRebindRotatesCustomerGenerations`、`TestApplyBindingRevokesTokensAndReplacesCredentials`、`TestVerifyChallenge`、runtime E2E（轮换后旧凭证立即失效、停用不发令牌、对 fake cc-connect 挑战通过） | 8e852b5, 2b07aa5, db0cb0b |
| §3.3 改绑/轮换/停用使旧 revision、旧 UID、未发送任务失效 | revision +1 使已有 outbox 的 MarkOutboxSending/SendGuarded 失败（#25/#27）；改绑全部客户换代际 | `TestUpdateBindingAndRotateCustomers`（旧 revision outbox 被拒、旧 UID 过期、换代原子） | 8e852b5 |
| §3.4 客户查询、转人工、恢复 AI（新代际）、最近 inbox/outbox；区分官方/本地状态 | `Store.Customers`、`BeginHandover`+`SetHandoverStatus`、`RecoverCustomer` | `TestCustomersHandoverRecover`（external_userid 掩码、跨企业 404、revision 409） | 2b07aa5 |
| §3.5 诊断三视图，默认不显示正文；查看正文二次认证+审计；只能标记不能重发 | `InboxByStates/OutboxByStates`、`diag_marks`、`viewBody` | `TestDiagnosticsHideBodiesAndMark`、`TestWriteFailuresAreReported` | 8e852b5, 2b07aa5 |
| §3.6 设置分组展示（只显示环境变量名）、callback 连接测试用同一 SSRF 校验 | `buildAdmin` 生成设置；`TestURL` 用投递同一按目标 SSRF 客户端、拒绝重定向 | `TestAdminRuntimeEdges`（重定向被拒、白名单外拒绝）、E2E | db0cb0b |
| §3.7 审计：操作者、动作、对象、revision、operation_id、结果、脱敏摘要；覆盖登录/同步/CRUD/绑定/接管/导出/连接测试 | `AddActorAudit`、`Audits` 分页；摘要不含凭证 | `TestActorAuditAndPaging`、`TestBindingLifecycle`（审计中无密钥） | 8e852b5, 2b07aa5 |
| §4 路由合同 | 全部列出路由已实现；额外 `/admin/stepup`、`/admin/accounts/{id}/link`、`/admin/diagnostics/{kind}/{id}/mark|view`、`/admin/settings/test` | 各测试 | 2b07aa5 |
| §5 会话：随机 token、HttpOnly/Secure/SameSite=Strict/Path=/admin/TTL、重启失效、数量上限 | 内存会话（32 字节随机）、上限淘汰最旧 | `TestLoginSessionCookieAndThrottle`、`TestRestartInvalidatesSessions`、`TestLogout` | 2b07aa5 |
| §5 所有 GET/POST 鉴权；POST CSRF + 可信 Origin/Referer | `authed`、`post`（常量时间比较 CSRF） | `TestUnauthenticatedAccessIsRejected`、`TestCSRFAndOrigin` | 2b07aa5 |
| §5 密码校验常量时间、防爆破 | bcrypt 比较；15 分钟内 5 次失败锁定 | `TestLoginSessionCookieAndThrottle` | 2b07aa5 |
| §5 Idempotency-Key：同键同参返回原结果、同键异参 409、不重复调微信；revision 冲突 409 | `admin_ops` 表先预留再执行，存 PRG 结果（含 409）重放 | `TestAdminOperationIdempotency`、`TestIdempotencyReplayAndMismatch` | 8e852b5, 2b07aa5 |
| §5 路径 ID 不扩大查询边界 | 绑定/客户/正文查看校验 enterprise_id | `TestBindingLifecycle`、`TestCustomersHandoverRecover`、`TestWriteFailuresAreReported` | 2b07aa5 |
| §6 SQLite 为准、真实 Secret 不展示 | `bootstrapBinding`、`withStoredCredentials` | E2E 重启后控制台修改保留、后台建的绑定带轮换后凭证恢复 | db0cb0b |
| §7 凭证/token/正文不出现在 HTML/URL/日志/错误 | 导出只经会话显示一次；`Cache-Control: no-store`、CSP、X-Frame-Options | `TestBindingLifecycle`、E2E（页面无 AES/真实 token/完整 CorpID） | 2b07aa5, db0cb0b |

未做/偏差：

- 系统设置在线保存（带 revision）未实现，见上。
- 客服账号头像只能填写已上传的 `media_id`，未做头像上传；新建账号必须提供 media_id。
- “新建代际”通过 转人工 → 恢复 AI 完成，没有单独按钮。
- 转人工/恢复已先调官方 `service_state/trans` 并 `get` 回读再改本地（拒绝则本地不变；结果未知则本地 UNKNOWN 并暂停 AI）；删除客服账号成功或 UNKNOWN 时停用关联绑定、吊销令牌并冻结旧客户 UID（#32/#33）。
- 诊断“查看正文”在 POST 响应中直接显示，不走 PRG（避免正文进入 URL 或会话）。
- §7 最后一项（真实测试企业联调账号 CRUD、链接、客服状态、接管/恢复）未执行，不能据此标为可发布。

## 第三轮评审（#30-#38，v0.1.4）

| # | 问题 | 修复 | 文件 | 测试 | 提交 |
|---|------|------|------|------|------|
| 30 | `RotateBindingCustomers` 不持绑定锁，可在 `SendGuarded` 检查通过后完成换代，旧代际仍可发出 | 换代持有与 `SendGuarded` 相同的 `binding:<id>` 锁，与每个出站分块互斥 | state/admin.go | `TestRotateBindingCustomersSerialisesWithSendGuarded`（发送进行中换代被阻塞，之后旧代际发送返回 `ErrStaleGeneration`） | 8ca267b |
| 31 | `BlockOutbox` 先读后无条件 UPDATE，可能把 SENDING/UPSTREAM_ACCEPTED 改成 BLOCKED 并错误释放预算 | 单写事务内两条语句都以 `state IN ('CREATED','VALIDATED')` 为条件（CAS），预算仅在本次转移真实发生且 `budget_reserved=1` 时释放；失败路径在事务内读状态（避免单连接池死锁） | state/store.go | `TestBlockOutboxIsCompareAndSet`（SENDING/UPSTREAM_ACCEPTED 不被覆盖、并发 block vs mark-sending 预算至多释放一次、重复 block 不重复释放） | 8ca267b |
| 32 | 删除客服账号只标记 DELETED，关联绑定仍可转发 | 删除成功或结果未知（UNKNOWN）时：停用该账号的全部绑定（revision+1 栅栏在途发送）、`Runtime.ApplyBinding` 吊销 cc-connect 令牌、`RotateBindingCustomers` 冻结旧 UID；失败在提示中显示警告 | admin/actions.go | 修正原 `admin_test.go` 断言（绑定须停用、revision=2、旧 UID stale、其他企业绑定不变）；`TestAccountDeleteFreezesBindings`；runtime E2E：删除后旧 token 调 `user/get` 被拒、gettoken 被拒 | 1487115 |
| 33 | 转人工/恢复只改 SQLite | 先调官方 `service_state/trans`（目标值取自 `wecom.service_state_map` 中 WAITING_HUMAN / AI_ELIGIBLE 对应的官方值，不猜），再 `service_state/get` 回读确认，确认后才改本地；官方明确拒绝则本地不变；结果不确定（网络错误/回读失败/回读不符）时转人工按失败安全暂停 AI 并标记 UNKNOWN，恢复保持暂停并标记 UNKNOWN，页面显示 | admin/admin.go、admin/actions.go、runtime/admin.go | `TestHandoverRequiresOfficialConfirmation`、`TestHandoverNeedsMappedOfficialState`（fake WeCom）；runtime E2E 经 fake 官方 API 转人工/恢复 | 1487115 |
| 34 | Cookie 恒为 Secure，但示例用 `http://127.0.0.1:8091` 导致登录循环 | 新增 `admin.insecure_cookie`：仅当 `admin.listen` 为回环地址时允许；未开启时 `admin.origin` 必须为 https；默认 Secure；`config.example.json` 显式开启（回环明文） | runtime/config.go、admin/admin.go、config.example.json | `TestReview3ConfigHardening`、`TestInsecureCookieOption`、`TestExampleConfigIsValid` | 6dfc566 |
| 35 | WeCom API base 允许 http:// | `Validate` 强制 https；`wecom.allow_insecure_http` 显式放行（测试/fake），启动时记录 `SECURITY_WARNING_insecure_wecom_api` | runtime/config.go、runtime/gateway.go | `TestReview3ConfigHardening`、`TestInsecureAPIWarns` | 6dfc566 |
| 36 | `callback_targets` 端口和 CIDR 未在启动时校验，非法 CIDR 被静默忽略 | 启动严格校验：host 非空、端口 1-65535、`allowed_cidrs` 非空且每项可解析，否则启动失败 | runtime/config.go | `TestReview3ConfigHardening` | 6dfc566 |
| 37 | 导出的 `runtime.Build` 跳过默认值和校验，`Enterprises` 为空时 `cfg.Enterprises[0]` panic | `Build` 自行执行 `defaults()` + `Validate()`（admin 要求恰好一个企业，因此不会越界） | runtime/gateway.go | `TestReview3ConfigHardening`（每个非法配置 Build 返回错误不 panic；手写配置自动补默认值） | 6dfc566 |
| 38 | PRAGMA 只作用于取得的连接 | 改用 modernc 驱动的连接钩子 `RegisterConnectionHook`：经 `sqlite` 驱动打开的每个连接（`OpenWithOptions` 及调用方传给 `New` 的 `*sql.DB`）都执行 `foreign_keys=ON; busy_timeout=5000`；`OpenWithOptions` 设连接上限 8（`New` 保留调用方设置） | state/store.go | `TestEveryPooledConnectionIsInitialised`（同时持有 8 个连接逐一检查 foreign_keys=1、busy_timeout=5000，覆盖两种入口） | 8ca267b |

**此前一次性 admin 包测试失败**：在 CPU 负载下（并行运行 state/runtime race 测试）以 `-race -count=10 -cpu=1,2,8` 运行 admin 包 30 次未复现。该失败发生在 v0.1.3 连接级 PRAGMA 修复之前，最可能的原因是非首个连接缺失 busy_timeout 导致的 SQLITE_BUSY；#38 进一步把初始化改为连接钩子覆盖所有入口。
