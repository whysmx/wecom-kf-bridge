# 评审问题修复记录（#1–#23）

> 按用户要求维护（覆盖 AGENTS.md “不另维护修改记录”的一般规则）。哈希为本地提交。
> 门禁：`go build ./...`、`go vet ./...`、`go test -race ./...` 通过；CI 严格覆盖率（>95%）当前约 92.9%，未通过。

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

附带修复：inbox `attempt` 原先每次状态迁移都 +1，导致重试次数被流水线迁移耗尽；改为仅进入 POSTING 时计数（ef0b7b6）。
