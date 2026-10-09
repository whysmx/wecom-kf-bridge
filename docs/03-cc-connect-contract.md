# cc-connect 兼容契约

> v0.1。本文规定本网关必须兼容的客户端行为及本项目自定义响应。不是腾讯完整自建应用 API 实现。

## 1. 基线与依据

固定提交：`whysmx/cc-connect@848eb24d89bbd83d03c2afd0dbd94768fd86d5a8`。

- [platform/wecom/wecom.go](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/platform/wecom/wecom.go)：New、Start、handleMessage、Reply、SendImage、getAccessToken、resolveUserName、decrypt。
- [web/src/lib/platformMeta.ts](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/web/src/lib/platformMeta.ts)：现有 WeChat Work 表单和高级字段。
- [core/message.go](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/core/message.go)：用户名及附件相关数据处理。

本次只做源码检查，未运行客户端。P0 要记录实际二进制版本、SHA、未修改上游对照和契约测试报告；不能把此 Fork 提交叫作官方最新发布版。

## 2. 配置字段

| 字段 | 本网关约定 |
|---|---|
| corp_id | 网关分配的虚拟企业标识；不是实际企业 Secret 的伴随字段 |
| corp_secret | 每个绑定独立的网关接入密钥 |
| agent_id | 正整数十进制字符串，表示绑定；兼容 JSON 字符串和 XML 整数 |
| callback_token | 该绑定的签名 Token |
| callback_aes_key | 32 字节密钥编码出的 43 字符 Base64（去掉末尾等号） |
| port | cc-connect 本机 HTTP 监听端口，多个同机实例不得冲突 |
| callback_path | cc-connect 本地路由，如 /wecom/callback |
| api_base_url | 网关兼容 API 基础地址，不含末尾 /cgi-bin/message/send |
| allow_from | 客户兼容 UID 白名单；见身份及运维文档 |

不要设置 `mode=websocket`。当前代码只在该值精确匹配时进入另一套协议；表单不要求填写 mode。

示例仅使用占位符，不能直接运行：

```toml
[[projects.platforms]]
type = "wecom"
[projects.platforms.options]
corp_id = "bridge_example_a"
corp_secret = "REPLACE_WITH_GATEWAY_SECRET"
agent_id = "1000002"
callback_token = "REPLACE_WITH_CALLBACK_TOKEN"
callback_aes_key = "REPLACE_WITH_VALID_43_CHARACTER_KEY"
port = "8081"
callback_path = "/wecom/callback"
api_base_url = "https://bridge.example.com"
allow_from = "*"
```

`*` 只用于明确允许所有映射客户的部署，不是通用安全默认。修改 TOML 是配置行为，不是改源码；本项目首选支持现有表单完成操作。

## 3. HTTP API 通用约定

所有 /cgi-bin 路径由网关实现。仅兼容规定的方法和字段，未知路径不透明转发到腾讯。不允许令牌跨绑定、任意 userid 查询或群发。查询串中的密钥/令牌在反向代理、日志、异常追踪中均须脱敏。

成功 JSON 必须含整数 `errcode:0`；错误 JSON 必须含非零 `errcode` 和不含密钥的 errmsg。不能只返回 HTTP 500 加空对象：当前部分解析只读取 errcode，缺省值可能被理解为成功。业务错误可返回 HTTP 200 加非零 errcode，协议/鉴权失败也须有正确 HTTP 状态和非零体。`media/get` 的二进制行为另见第 7 节。

内部网关错误码预留 `70001` 认证、`70002` 越权、`70003` 参数、`70004` 停用/人工/旧代际、`70005` 不支持类型、`70006` 平台拒绝、`70007` 暂时不可用、`70008` 结果未知。这些是本网关错误码，不是微信官方错误码。原官方 errcode 仅在脱敏诊断记录中保留。

## 4. 令牌与用户接口

### GET /cgi-bin/gettoken

查询 `corpid`、`corpsecret`。验证有效绑定及凭证版本，返回独立的高熵随机兼容令牌：

```json
{"errcode":0,"errmsg":"ok","access_token":"opaque_gateway_token","expires_in":3600}
```

3600 秒是本版建议的网关令牌寿命，不是微信寿命。客户端会减去 60 秒做缓存；必须返回合法正值。令牌只作用于一个 binding，不能返回真实微信 access_token。停用/撤销后拒绝旧令牌，且操作指南须说明该客户端可能缓存旧令牌至到期，需要协调重启或过渡轮换。

### GET /cgi-bin/user/get

查询 `access_token`、`userid`。校验 userid 属于令牌对应绑定，再由身份映射找到真实客户。成功：

```json
{"errcode":0,"errmsg":"ok","userid":"bcu_example_g1","name":"客户昵称"}
```

昵称暂时不可得时返回 `errcode:0`、空 `name`，让客户端显示 UID 且不缓存一个虚假的非空昵称；不影响正常回复。跨绑定或不存在 UID 返回非零错误。源码对非空 name 做进程内缓存，网关无法无修改强制刷新。头像不是该兼容函数消费的字段，不承诺头像透传到客户端界面。

## 5. 发送接口

### POST /cgi-bin/message/send?access_token=...

文本示例：

```json
{"touser":"bcu_example_g1","msgtype":"text","agentid":"1000002","text":{"content":"回答内容"},"safe":0}
```

客户端 agentid 可能为字符串；网关仅接受可无损解析的十进制字符串或整数，统一校验到令牌绑定，禁止浮点截断。touser 首版只允许单一已登记 UID，禁止 `@all`、分隔列表及未知用户。text 和 markdown 均按客户可见的普通文本转换；不得假定微信客服支持自建应用 Markdown 语义。

客户端会按 UTF-8 字节分块发送长回复；网关不能假定一次 HTTP 调用等于一个完整答案，也不能靠内容相同把两条合法回复合并。按官方已核验的发送约束进一步校验，不得偷偷删除超限内容。

先持久化操作记录，再检查账号、binding revision、客户代际、人工状态和媒体权限，随后调用真实微信发送。只有拿到官方成功响应才返回 `errcode:0`（表示官方接受，不表示客户已读）。若超时且结果无法证明，标 UNKNOWN，返回 70008，不盲目重复发送。详见可靠性文档。

图片示例：

```json
{"touser":"bcu_example_g1","msgtype":"image","agentid":"1000002","image":{"media_id":"bm_example"}}
```

媒体 ID 必须属于当前绑定且未过期。msgtype 未实现时明确报错，不假装成功。

## 6. 网关投递到 cc-connect 的加密 XML

方向是网关 → `binding.callback_url`。目标不一定公开，但必须能从网关访问。

明文示例：

```xml
<xml>
  <ToUserName>bridge_example_a</ToUserName>
  <FromUserName>bcu_example_g1</FromUserName>
  <CreateTime>由原消息时间转换的Unix秒</CreateTime>
  <MsgType>text</MsgType>
  <Content>经XML安全转义的原问题</Content>
  <MsgId>1000000001</MsgId>
  <AgentID>1000002</AgentID>
</xml>
```

这段包含说明性占位符，测试夹具须替换为实际整数。禁止字符串拼接未转义昵称、正文和文件名；使用 XML 编码器。消息正文与昵称是数据，不得把网关指令或模型提示词拼入客户原文。

MsgId 由网关分配稳定正 int64，保证持久化唯一；微信客服原始消息 ID 即使是字符串也必须单独保存，不能直接塞入 int64 字段或使用不校验冲突的截断哈希。重投同一条消息使用同一 MsgId。不要把时间戳单独当 ID。

按源码兼容结构加密：16 字节安全随机前缀 + 4 字节大端的明文 UTF-8 字节长度 + 明文 XML + 虚拟 corp_id；采用 32 字节分组补位约定，再 AES-256-CBC，IV 为密钥前 16 字节；Base64 密文放入外层 Encrypt。签名是 callback_token、timestamp、nonce、Encrypt 排序拼接后的 SHA-1 小写十六进制。该结构源自固定客户端，不能混用 WSS 帧或客服事件的原始密文。

```text
POST {callback_url}?msg_signature=...&timestamp=...&nonce=...
Content-Type: application/xml; charset=utf-8
```

外层为 `<xml><ToUserName>...</ToUserName><AgentID>...</AgentID><Encrypt>...</Encrypt></xml>`。重试可重新生成 nonce/加密前缀，但原始 MsgId 与业务正文保持不变。要用独立的已知向量和固定客户端互操作测试，而不只是网关自己的 encrypt/decrypt 自证。

目标验证用 GET 加密 echostr：客户端返回解密后的挑战原文才算通过；不能仅用 TCP 通或 HTTP 200 判定凭证正确。不得发送伪造客户问题做“连接测试”，避免无意触发 Codex。

## 7. 媒体契约

客户端入站 HTTP 支持 image、voice、file，随后调用 GET `/cgi-bin/media/get?access_token=...&media_id=...`。网关返回经授权的原始媒体字节；文件名通过 XML FileName 提供，voice 带 Format。媒体文件必须在投递前准备好，所有虚拟 media_id 与绑定关联。

客户端该下载函数仅读取响应字节，并未完整校验 HTTP/JSON 错误。因此不能用 JSON 错误体冒充媒体成功。对已知准备失败的文件不要先投递 XML；到期/越权下载应返回非成功状态、空响应体并记录事件，兼容测试证明不会泄露或产生伪文件，无法处理的情况明确作为已知限制。

出站图片接口为 POST `/cgi-bin/media/upload?access_token=...&type=image`，multipart 字段名 media。网关校验文件大小、魔数、类型，保存映射/必要上传，返回 `{"errcode":0,"errmsg":"ok","media_id":"bm_example"}`。不能承诺固定客户端拥有通用出站文件发送接口；其他能力需新证据和范围评审。

## 8. 必须显式保留的客户端限制

- 回调 handleMessage 在进一步解析、旧消息过滤、权限过滤及异步处理前已写 HTTP 200；这只表示 HTTP 接受，绝不是 AI 完成回执。
- MsgId 去重在客户端只是短期进程内缓存；网关必须做独立持久化去重。
- 客户端对旧消息可能过滤。P0 检查具体阈值；禁止偷偷修改 CreateTime 绕过。超过投递有效期的记录进入 expired/not-dispatched。
- 回复包含 touser/agentid，没有原问题 MsgId、代际编号或幂等请求头；网关通过兼容 UID 编码/映射代际，不能凭空精确关联某条原问题。
- 每个 HTTP 平台实例单独 ListenAndServe；同机不同项目应使用不同端口。只填不同 path 不解决端口竞争。
- 只读问答不能禁止全部运行时写入：cc-connect 可能存附件、日志、会话；项目资料只读与运行时目录可写要分开。

这些是验收前提，不允许 Agent 修改客户端来掩盖。
