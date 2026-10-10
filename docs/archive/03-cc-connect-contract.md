# cc-connect 兼容契约

> v0.2。本网关只实现固定客户端所用的 HTTP 自建应用协议子集，不模拟腾讯全部 API。

## 1. 固定基线

上游 `chenhg5/cc-connect@dfad19415a38b00b2c5c288610784d1a7eef337f`：

- [platform/wecom/wecom.go](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/platform/wecom/wecom.go)：配置、回调、发送、令牌、昵称、媒体、加解密。
- [core/dedup.go](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/core/dedup.go)：旧消息过滤。
- [config.example.toml](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/config.example.toml)：显示与会话配置。

原 Fork `whysmx/cc-connect@848eb24d89bbd83d03c2afd0dbd94768fd86d5a8` 保留历史对照。上游已有 api_base_url；这次是源码核对，不是二进制、GUI 或 Codex 实测。固定版本运行前仍做互操作测试。

## 2. 配置与网络

| 字段 | 网关约定 |
|---|---|
| corp_id | 每绑定独立虚拟企业标识 |
| corp_secret | 每绑定独立网关接入密钥，绝非真实微信 Secret |
| agent_id | 正整数十进制字符串 |
| callback_token | 当前绑定签名 Token |
| callback_aes_key | 32 字节随机密钥的 Base64 去尾等号，43 字符 |
| port | 客户端监听端口，同机不同平台实例不得冲突 |
| callback_path | 如 /wecom/callback |
| api_base_url | 网关基础地址，不带 /cgi-bin/... |
| allow_from | 兼容 UID 白名单；使用 * 必须另有网关客户授权及受保护回调 |

不设置 `mode=websocket`。现有 WeChat Work 手动配置沿用上述字段；网关另保存完整 callback_url，并且必须能从网关访问。只设置 api_base_url 不建立反向长连接。挑战验证使用加密 echostr GET，必须匹配回显，不能拿 TCP 连通或 HTTP 200 代替。

```toml
# 仅字段示例，必须换成实际生成凭证，并放入既有项目配置。
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
allow_from = "REPLACE_WITH_ALLOWED_UIDS"
```

## 3. 通用兼容响应

不透明转发未知 /cgi-bin 路径，不允许跨绑定查询、群发或任意 userid。所有 JSON 成功响应显式含整数 `errcode:0`；错误显式含非零 errcode 与脱敏 errmsg。客户端部分函数不检查 HTTP 状态而只解析 errcode，不能回空对象造成假成功。协议/鉴权错误同时给适当 HTTP 状态；媒体二进制例外见第 7 节。

网关自定义码：70001 认证，70002 越权，70003 参数，70004 停用/人工/旧代际，70005 不支持，70006 平台拒绝，70007 暂不可用，70008 结果未知。不是微信官方错误码，原官方码单独保留于脱敏诊断。

查询串 token/secret 在两端和反向代理日志均脱敏。外部微信 token 不得返回给 cc-connect。

## 4. Token 与用户名

`GET /cgi-bin/gettoken?corpid=...&corpsecret=...`：验证绑定、启用/撤销状态及凭证版本，返回只属于该绑定的高熵虚拟 access_token。示例寿命 3600 秒为网关设计值，不是微信规定。

```json
{"errcode":0,"errmsg":"ok","access_token":"opaque_gateway_token","expires_in":3600}
```

客户端对正常 expires_in 缓存时减 60 秒；非正值回退 7200 秒。网关必须返回合法正值，不能依赖回退。轮换/停用拒绝旧 token，运维需处理客户端缓存，并明确协调配置更新/重启或有限过渡期。

`GET /cgi-bin/user/get?access_token=...&userid=...`：先校验 token 与 UID 的 binding，再解析当前客户资料。

```json
{"errcode":0,"errmsg":"ok","userid":"bcu_example_g1","name":"客户昵称"}
```

昵称不可得时合法 UID 返回空 name，客户端回退 UID 且不缓存虚假昵称；未知/越权 UID 返回错误。非空 name 被客户端进程内缓存，没有网关主动失效接口。头像不属于该函数消费字段，不能声称头像透传。身份与代际规则见[身份文档](06-customer-identity.md)。

## 5. message/send

`POST /cgi-bin/message/send?access_token=...`：

```json
{"touser":"bcu_example_g1","msgtype":"text","agentid":"1000002","text":{"content":"回答内容"},"safe":0}
```

agentid 接受可无损解析的十进制字符串或整数，并与 token 绑定核对；禁止浮点截断。touser 仅允许一个已登记 UID，不允许 @all、分隔列表或未知客户。text/markdown 作为普通文本适配到微信客服，不承诺其具备自建应用 Markdown 展示语义。

固定 `Reply` 先按配置移除 Markdown，再按 **2000 UTF-8 字节**分块，逐次 HTTP 调用；任一块出错即返回错误。客户端出站 HTTP timeout **30 秒**。网关建议在 **15 秒**内完成状态查询和发送等总预算，15 秒属于本地设计值。

每个请求先登记 outbox，再检查绑定 revision、UID generation、客户授权、官方人工状态及窗口/预算。明确官方成功才回 errcode 0，含义仅为官方接受；结果不明回 70008 并保存 UNKNOWN。不能先回成功再异步发送，不能返回失败后自动继续排队发送。

没有原问题 ID、完整回答结束标识或请求幂等键。相同文本可以是两次合法回答，不能按内容去重；也不能推断后续片段属于哪一个完整回答或自动聚合。每段、图片、思考/工具/状态提示均可能消耗平台额度；关闭非必要提示，详见[运维](12-deployment-runbook.md)和[可靠性](07-delivery-and-handover.md)。

出站图片请求：

```json
{"touser":"bcu_example_g1","msgtype":"image","agentid":"1000002","image":{"media_id":"bm_example"}}
```

仅接受当前绑定、用途正确且未过期的虚拟媒体 ID。未实现类型明确拒绝，不伪成功。

## 6. 网关到客户端的 XML

明文结构使用 XML 编码器生成，正文/昵称/文件名安全转义，不拼入模型指令：

```xml
<xml>
  <ToUserName>bridge_example_a</ToUserName>
  <FromUserName>bcu_example_g1</FromUserName>
  <CreateTime>1791500000</CreateTime>
  <MsgType>text</MsgType>
  <Content>客户问题</Content>
  <MsgId>1000000001</MsgId>
  <AgentID>1000002</AgentID>
</xml>
```

示例时间/ID 仅示意。实际 CreateTime 必须来自原始 send_time，不改当前时间或 0 绕过过滤。原始客服 msgid 为字符串，单独保存；兼容 MsgId 分配稳定唯一正 int64，重试沿用同一个值，不用时间戳或截断哈希冒充唯一。

加密载荷：安全随机 16 字节 + 4 字节大端 XML UTF-8 长度 + XML + 虚拟 corp_id；按 32 字节分组补位规则填充，再 AES-256-CBC，IV 为 key 前 16 字节，结果 Base64。签名为 callback_token、timestamp、nonce、Encrypt 排序拼接的 SHA-1 小写十六进制。

```text
POST {callback_url}?msg_signature=...&timestamp=...&nonce=...
Content-Type: application/xml; charset=utf-8
```

外层 `<xml><ToUserName>...</ToUserName><AgentID>...</AgentID><Encrypt>...</Encrypt></xml>`。每绑定使用独立加密密钥和签名 Token。以独立测试向量及未改适配器验证，不只自加密/自解密。网关接收官方密文时使用严格补位校验，不照抄客户端宽松解密逻辑。

客户端 SessionKey 是 `wecom:{兼容UID}`；文本 handler 以 goroutine 调用，媒体另异步下载。网关只能控制投递顺序，不能把 ACK 当成上一轮已执行完成。

## 7. 媒体能力及明确限制

入站 HTTP 支持 image、voice、file，随后 GET `/cgi-bin/media/get?access_token=...&media_id=...`。文件名用 XML FileName，语音用 Format；格式与实际字节一致。媒体在 XML 投递前先下载、验证并固定本地引用，不在下载请求时才临时抓一个可能过期的官方 URL。

固定图片 handler 一律标记 `image/jpeg`。非 JPEG 图片必须在有大小/像素上限的条件下规范化为 JPEG，或以实际下游测试证明可正确解码后再放开该格式；不能仅改 HTTP Content-Type 就认为已修复。规范化可能损失透明度/动画，应明确边界，不建设通用转码服务。

固定 downloadMedia 只读响应体，**不检查 HTTP 状态或 JSON 错误**。返回非 200 空体可以避免把 JSON 错误当正文泄露，但仍可能生成空附件；原来“这样即可保证无伪文件”的要求不成立。已知失败不投递 XML；已投递后的过期/磁盘/网络异常记录可见故障，不能无修改客户端保证所有失败均优雅处理。未通过异常用例的媒体能力不标完成。

出站图片上传：POST `/cgi-bin/media/upload?access_token=...&type=image`，multipart 字段 `media`，校验大小/魔数/类型，保存虚拟映射后返回：

```json
{"errcode":0,"errmsg":"ok","media_id":"bm_example"}
```

固定适配器实现 ImageSender，不承诺通用出站文件/语音能力。媒体不执行、不任意解压；下载仅按授权 ID 映射，禁止任意 URL/路径代理。

## 8. 重启、去重和安全配置

- `handleMessage` 在业务解析、旧消息及 allow_from 过滤前写 HTTP 200；最多证明 HTTP 接受。
- 去重缓存约 60 秒且只在进程内；重启后不存在，不能作为持久可靠投递承诺。
- 旧消息条件是 `msgTime.Before(StartTime.Add(-2*time.Second))`。不是“当前时间前两秒的消息都过期”。网关挑战接口不能查询 StartTime；不知道实际启动边界时不得凭 200 宣称已处理或凭推测宣称已过滤。
- 客户端重启/不可达恢复后，历史消息默认暂停而非篡改时间重放；客户可重新发送问题。网关自己的重启与客户端重启不是同一事件。
- 只读、控制命令、别名、闲置重置及提示消息配置见运维。稳定 UID 不能阻止客户端默认闲置新会话，也不能充当客户文件保密边界。

以上边界进入原验收矩阵及 AT-059～AT-069，不通过修改客户端来隐瞒。
