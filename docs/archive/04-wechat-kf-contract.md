# 微信客服适配契约与核验

> v0.2 · 2026-10-09。接口方法/类型已有相关项目源码交叉依据；腾讯页面本次未取得正文，真实企业权限和最新平台限制仍待核验。不得把所有字段标为官方实测通过。

## 1. 来源与凭证

官方入口：[微信客服](https://kf.weixin.qq.com/api/doc/)、[客户基础信息 95166](https://kf.weixin.qq.com/api/doc/path/95166)。95166 用于客户昵称等，不是客服账号管理。

本次参考固定 [WxJava 实现](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/api/impl/WxCpKfServiceImpl.java)及[接口说明](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/api/WxCpKfService.java)。方法实现、数据类型和注释分开记证据；不因此引入 Java 依赖。完整定位见[依据](14-sources-and-verification.md)。

网关保管真实企业凭证，cc-connect 使用虚拟凭证，绝不互换。企业初始化必须确认实际接入方式、微信客服 Secret/应用授权来源、可 API 管理的账号范围、回调配置及可信 IP/出站网络要求；本次未访问用户企业后台，不推断管理员身份自动满足全部条件。不做第三方套件/OAuth 运营平台。

## 2. 所需接口

下表 KF 方法和请求字段依据固定 SDK；未列出的精确限制、必填条件及错误码需官方/真实企业冻结。

| 能力 | 方法/路径 | 关键请求或返回 |
|---|---|---|
| 真实 access_token | GET /cgi-bin/gettoken | corpid、corpsecret；独立缓存真实 token |
| 拉消息/事件 | POST /cgi-bin/kf/sync_msg | cursor、token、limit、voice_format、open_kfid；next_cursor、整数 has_more、msg_list |
| 发客服消息 | POST /cgi-bin/kf/send_msg | touser、open_kfid、msgtype、对应内容、可选 msgid |
| 客户资料 | POST /cgi-bin/kf/customer/batchget | external_userid_list；customer_list、invalid_external_userid |
| 创建客服 | POST /cgi-bin/kf/account/add | 名称/头像字段；返回 open_kfid |
| 编辑客服 | POST /cgi-bin/kf/account/update | open_kfid 与允许修改字段 |
| 删除客服 | POST /cgi-bin/kf/account/del | open_kfid |
| 客服列表 | POST /cgi-bin/kf/account/list | offset、limit；不得误改成 GET |
| 客服链接 | POST /cgi-bin/kf/add_contact_way | open_kfid 及支持的场景参数 |
| 查询接待状态 | POST /cgi-bin/kf/service_state/get | open_kfid、external_userid；service_state、servicer_userid |
| 转换接待状态 | POST /cgi-bin/kf/service_state/trans | 上述身份、目标 service_state、适用时 servicer_userid |
| 接待人员列表 | **GET /cgi-bin/kf/servicer/list** | **open_kfid 放查询参数**；修正 v0.1 的 POST |
| 临时媒体 | POST /cgi-bin/media/upload；GET /cgi-bin/media/get | 类型、文件、media_id；用途与权限需分别核验 |

不把 `/cgi-bin/message/send` 当客服发送，也不把企业成员 userid 当客户 external_userid。欢迎语 `send_msg_on_event` 不是首版必要能力，不用于绕过普通发送额度。

## 3. 通知接收

公开路径沿用设计 `/webhooks/wechat-kf/{tenant_key}`；首版只有一个企业配置也可保留该路由键。GET 验证、POST 通知的签名、密文接收方、成功响应体和重试条件按实际官方合同确认。

大小/方法检查 → 定位真实回调密钥 → 验签、解密和接收方检查 → 解析合法事件 → 短事务保存待拉取标记及必要 Token → 尽快响应。不要等 Codex 或全部分页完成。校验/落库失败不得承诺已接受。

通知不是客户正文；回调签名 Token、通知中的拉取 token、接口 access_token 不是同一个值。重复通知只用于唤醒同步，不能由通知次数推断消息数量。拉取 token 加密短期保存，不进访问日志。事件时间与到达时间分开保留，不靠过短重放窗口拒绝合法重投。

## 4. 同步、游标与恢复

SDK 的 syncMsg 重载支持 open_kfid；旧无此参数重载已标 deprecated。请求有 token 时按已验证合同使用；注释记录 token 约 10 分钟有效、可省略但会严格限频。该数字及无 token 频率仍须真实企业核验，不能据此做高频全量轮询。

启动阶段用两个客服账号验证过滤与 cursor 的作用域，将企业/凭证范围/过滤方式作为固定 sync_scope_key。每条游标只用于自己的固定作用域，不能将某账号返回的 cursor 直接用于另一个账号，不能运行中随意切换带/不带 open_kfid。作用域尚未确认不发布真实同步，但可先写分页/事务测试。

单进程内对同 scope 串行。一个页面的消息/事件去重入库与 next_cursor **同事务**提交；不持锁等网络。has_more 为整数，按其值翻页，不能用返回条数少于 limit 判断结束。处理重复页、空页、无进展及未知枚举；未知类型可记录隔离后前进，无法确定消息边界则暂停。

持久化原 msgid、open_kfid、external_userid、send_time、origin、msgtype 和事件。明确区分客户内容、人工回复、系统/状态/发送失败事件，不把后几类再次发给 Codex。origin 数字与人工状态数字本次没有充分一手核验，不在业务代码中猜测默认值。

重启从原 cursor 恢复。通知 token 已过期时，在合同确认后以受限无 token 拉取补偿或等待有效通知，并显示同步延迟；不能认为持久化 token 就永远有效。无效 cursor 不自动清空重置。人工状态事件存在缺口时暂停 AI，见[可靠性](07-delivery-and-handover.md)。

## 5. 发送窗口与结果

固定 SDK 接口注释记录：新接入待处理/智能助手状态可发；客户主动发送后的 48 小时内最多发送 5 条，客户继续发消息后可再次下发。这是 **SDK 注释支持的待实测约束**，不是本次已读取的腾讯最新政策，也不是“一个问题可无限分段”。

在真实企业确认 policy 的窗口、计数/重置条件、文本单位与长度、媒体限制、频控和错误码；SDK 旧注释中的客服账号数量不得复制成业务上限。平台约束保守执行，不靠连续刷新 token、换客服或欢迎语接口绕过。

每次发送查授权、当前代际/人工状态及预算。思考提示、状态提示、分段和图片都纳入发送计数，不自动向客户发送不受预算控制的错误通知。出现额度不足保留未送部分并报告管理员，不静默截断、跨账号补发或延迟到以后主动重发。

msgid 是可选字符串字段；其存在不能证明微信保证重试去重。没有明确幂等承诺与验证之前，可能已执行的写请求超时标 UNKNOWN，不自动重发。固定 SDK 注释还明确 sync_msg 不读取由发送接口发出的成功消息，因此不能以“同步没看到”认定没发送，也不承诺通用自动对账。

官方接受与客户收到/已读不同；可出现后续发送失败事件。按已核验事件字段关联 outbox 并标记 DELIVERY_FAILED，事件不是新客户问题；缺失事件亦不证明已送达。

## 6. 账号、客户与媒体

真实客服 CRUD/链接保留[账号文档](05-account-management.md)的防重复、UNKNOWN、危险操作和部分列表规则。账号名称、头像字段/格式、分页及实际可见范围要有受控回包。不能按同名合并账号；不能将列表缺失直接当删除。

客户资料按 customer_list 与 invalid_external_userid 逐条处理，nickname 字段及权限/时效以该企业实际回包核实。只依赖可用昵称，不扩展收集 unionid 等无关个人信息。资料失败走空名/缓存回退，不阻塞正常问答，不挪用别人的资料。

媒体逐用途核验头像上传、客户下载、客服发送的格式/大小/有效期；固定客户端 JPEG/MIME 和错误响应限制见[兼容契约](03-cc-connect-contract.md)。未通过真实解码与异常用例的类型不标已支持。

## 7. 人工状态与冻结要求

状态 get/trans 有源码路径；具体数字枚举、合法转换图、人工工作台触发行为仍须核验后单点映射为 AI_ELIGIBLE、WAITING_HUMAN、HUMAN、CLOSED、UNKNOWN。转到人工的适用请求中 servicer_userid 要求与接待人员状态按合同校验，不能凭空编配接待人。

默认发送前查状态；查询失败不发送。当前 AI 状态不证明此前没有发生人工接管，必须结合持久化人工暂停、事件和代际规则；事件缺口或恢复旧备份不自动恢复 AI。

按能力切片补充：固定来源/日期、权限、字段/枚举/限制、合成成功/失败样本、真实测试结果、相关 AT 编号。可先用源码合同开发内部测试；真实能力发布前补齐适用官方/企业契约和联调，不把全部外部未知都强制变成开发骨架的全局阻塞。
