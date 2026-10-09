# 数据模型、配置与安全设计

> v0.1。字段是逻辑模型，不是已创建的数据库。建议 SQLite 单实例持久化；具体 DDL、驱动版本与迁移脚本在 P0/P1 冻结。

## 1. 最小持久化实体

| 实体 | 关键字段/约束 |
|---|---|
| tenants | id、真实 corp_id、凭证引用、回调密钥引用、状态、revision |
| kf_accounts | id、tenant_id、open_kfid、name/avatar 元数据、visibility_status、last_sync_at；唯一(tenant_id,open_kfid) |
| bindings | id、account_id、project_label、callback_url、虚拟 corp_id/agent_id、凭证引用/版本、enabled、revision；每账号最多一个 active |
| credentials/tokens | binding_id、版本、哈希/密文或引用、到期/撤销时间；虚拟令牌与官方令牌分表或明确类型 |
| customers | id、tenant_id、external_userid 受保护值、查询索引、昵称、缓存状态/时间；企业范围唯一 |
| conversations | id、binding_id、customer_id、generation、官方状态、fence_version、状态来源/时间 |
| bridge_user_aliases | 不透明 uid、conversation_id、generation、active/blocked、created_at；旧 UID 永不再分配 |
| sync_cursors | sync_scope_key、cursor、revision、last_success_at；作用域由外部核验确定 |
| sync_jobs | scope、通知/Token 受保护引用、状态、attempt、lease、next_attempt_at |
| inbox | id、官方唯一键、消息类型/来源/时间、binding revision、UID generation、正 int64 compat_msg_id、状态、正文受保护引用 |
| delivery_attempts | inbox_id、目标 revision、尝试编号、时间、HTTP/网络结果、错误类别 |
| outbox | id、binding/customer/generation、消息类型、内容/媒体引用、官方请求标识、状态/错误、created/updated |
| media | 虚拟 media_id、tenant/binding、真实 media_id、用途、文件路径引用、hash/type/size、状态、expires_at |
| operations | 管理员、对象、动作、幂等键哈希、参数摘要、PENDING/SUCCEEDED/FAILED/UNKNOWN、外部结果 |
| audit_events | 时间、操作者、动作、对象、revision、脱敏差异、结果、关联 ID |

审计和必要 UNKNOWN 证据不因账号删除级联删除。存量旧 UID 保留 tombstone 或等价不可重用证据，防止恢复后错误发送；不需要永远保留客户全文。

## 2. 主键与隔离

禁止全局仅按 open_kfid、external_userid、nickname 查询。API 服务先解析授权范围，再查实体；SQL 层查询条件包含 tenant/binding 范围。用户提供的 agentid/touser/media_id 都是不可信数据，不能推翻 token 作用域。

兼容 MsgId 使用稳定正 int64 序列或经验证的无冲突方案，存储唯一约束。外部字符串 msgid 完整保存，不转换成浮点数。管理 API 向 JavaScript 输出超安全整数范围的标识时用字符串；时间和原消息 ID 类型分别建模。

## 3. 事务边界

必须原子完成：一页 Inbox+next_cursor；UID 创建+对应会话/代际登记；binding 修改+旧 revision/UID 封锁；outbox 创建+授权版本快照；管理操作幂等登记+状态写入。

网络 I/O 不在数据库写锁内等待。涉及外部副作用时使用持久化 operation 与补偿/对账，而不是声称分布式回滚。并发 cursor/账号编辑使用 revision 比较；失败需刷新，不静默覆盖。

启动时执行经测试的 schema 迁移；迁移失败不以新版本开始收消息。降级不盲目套旧二进制到新 schema，先维护模式和备份恢复演练。

## 4. 配置分层

建议 `bridge.yaml` 仅包含进程、存储、安全和任务参数；真实企业、账号、绑定由数据库管理并可导出脱敏清单，避免同一字段同时由 YAML 和后台竞争控制。

```yaml
# 目标配置示例，尚无可执行程序或加载器
server:
  public_listen: "127.0.0.1:8090"
  admin_listen: "127.0.0.1:8091"
  public_base_url: "https://bridge.example.com"
storage:
  data_dir: "./data"
  database: "./data/bridge.db"
security:
  master_key_env: "WECOM_KF_BRIDGE_MASTER_KEY"
  callback_targets:
    - host: "cc-host.internal"
      port: 8081
      allowed_cidrs: ["10.20.0.0/16"]
workers:
  max_concurrency: 8
  shutdown_grace_seconds: 20
retention:
  terminal_message_days: 7
  audit_days: 90
```

并发 8、保留 7/90 天都是可调整的本地保护建议，不是客服/用户数量上限，也不是法规或腾讯规定。启动校验 unknown 字段、端口、URL、目录权限、密钥长度与保留策略；不能拼错字段还静默启动。

租户启用前必须装载已核验的官方 policy（发送窗口、媒体限制、频控等）；关键未知约束不能默认无穷大。测试环境可显式使用 fake policy，界面明显标识不能用于生产。

## 5. 凭证与信任边界

真实微信 secret、官方 access_token、回调 AES key 用外置秘密引用或主密钥加密保存；主密钥不放数据库同目录/同备份，不提交 Git。需要恢复导出的虚拟密钥应加密存储；仅用于认证比较的随机密钥/令牌可使用强哈希/HMAC 摘要并进行常量时间比较。

真实官方 Token 与兼容 Token 采用不同命名空间、生命周期和缓存，绝不互换。刷新并发 single-flight，每租户/绑定范围独立。停用/轮换撤销旧版本，并提供客户端缓存影响说明。

公网流量经 HTTPS；内网 HTTP 只允许明确受信任的同机/受控网络，跨公网的 cc-connect 回调必须用反向代理 TLS 或私有隧道。管理员端不因公开微信回调而开放匿名访问。

## 6. SSRF 与回调安全

callback_url、头像 URL、媒体位置都可能成为 SSRF 入口。网关确实需要访问指定内网 cc-connect，所以不能一刀切禁止所有内网，也不能允许任意内网。

设计要求：仅授权管理员可设置；明确 host+port+允许 CIDR；拒绝用户名密码 URL、非 HTTP(S) 协议、metadata/link-local 地址及管理端口；DNS 解析结果校验并将连接固定到已校验目标，防重绑定；默认禁跳转，确需跳转逐跳重新检查。诊断测试也须使用同一防护。

不得让客户端 `media_id` 或上传文件名成为服务器文件路径；虚拟媒体 ID 只能查询授权映射。禁止通用 URL 代理、`file://`、任意磁盘读取和未授权下载。

相关风险定义参考 [OWASP SSRF](https://owasp.org/www-community/attacks/Server_Side_Request_Forgery)；本文 allowlist 和内网部署取舍是本产品设计。

## 7. 回调、输入与内容保护

分别管理官方回调密钥与每个 cc-connect 回调密钥。验证签名、接收者、时间/重放策略；严格校验 Base64、密钥长、密文块长度、PKCS#7 补位每字节、长度前缀，使用成熟密码库，不能自己发明算法。

XML/JSON 请求限大小、限制深度/元素、拒绝异常外部实体用途；编码时转义正文/昵称/文件名。SQL 使用参数；HTML 默认转义。媒体按魔数/类型/大小检查，不自动执行、解压或发给 shell。必要临时文件最小权限，过期清理不跟随符号链接越界。

客户内容和昵称都视为不可信数据，不作为网关管理指令。需要转人工的命令须精确定义和白名单匹配，不允许运行任意管理 API。是否能修改项目由执行端权限控制，网关不能靠过滤几个关键词保证只读。

## 8. 保留、清理与备份

仅保存恢复/诊断所需正文，单独加密或受权限保护。默认不在管理列表展示完整聊天；查看必要内容需权限并审计。过期客户资料可删除/匿名化，旧 UID tombstone 和去重证据保留至安全期，不能清理后导致重复投递。

媒体保留期须覆盖有效待处理任务；有活跃引用/UNKNOWN 操作的记录不能被盲目定时清理。达到存储限制时拒绝新大文件并报警，不删除尚未处理数据腾空间。

使用一致性备份机制；SQLite 可参考 [官方 Backup API](https://www.sqlite.org/backup.html)，不要只复制运行中的主 db 文件而忽略事务状态。备份、主密钥、媒体索引的恢复必须联合演练。测试用故障注入验证磁盘满、权限丢失、损坏及迁移失败。

## 9. 安全验收

跨绑定 gettoken/user/get/send/media/管理 API 越权全部拒绝；重复/错误签名不触发同步；非法补位/超长 XML 不崩溃；SSRF 不探测 metadata 或管理端；XSS 不执行；日志扫描无密钥/客户正文；危险删除有确认；每次轮换可解释恢复；外部调用失败不使凭证落盘到公开错误文件。
