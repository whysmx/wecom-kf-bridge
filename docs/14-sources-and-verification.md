# 依据、核验状态与发布前置

> v0.2 · 核查日期 2026-10-09。源码事实、SDK 注释和真实环境结果分别记录。本次没有运行测试或访问用户企业后台。

## 1. cc-connect 固定源码

上游 main 本次解析为 `dfad19415a38b00b2c5c288610784d1a7eef337f`。使用该提交永久链接，不把未来变化的 main 当稳定合同。原 Fork `whysmx/cc-connect@848eb24d89bbd83d03c2afd0dbd94768fd86d5a8` 为历史对照；在 GitHub Fork 网络中可访问某 SHA 不代表它是上游发行版。

### S-001：HTTP 适配器

[platform/wecom/wecom.go](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/platform/wecom/wecom.go)

本次核对 New、handleMessage、Reply、resolveUserName、downloadMedia 等关键路径：上游支持 api_base_url；客户端 HTTP timeout 30 秒；回调 200 在后续业务解析/过滤前写入；文本和媒体 handler 异步；Reply 按 2000 UTF-8 字节分块并在某块失败时返回错误；昵称非空缓存；图片标记固定 image/jpeg；媒体下载不校验 HTTP 状态/JSON 错误；短期 MsgId 去重仅在进程内。

原 Fork 同文件亦核对 gettoken、SendImage、上传、加密结构及字段。[历史文件](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/platform/wecom/wecom.go)。尚未运行跨版本全部函数的自动差异测试，不能声称两个提交逐字相同或所有能力全等。

### S-002：现有表单（沿用初版证据）

[原固定 platformMeta.ts](https://github.com/whysmx/cc-connect/blob/848eb24d89bbd83d03c2afd0dbd94768fd86d5a8/web/src/lib/platformMeta.ts)。初版核对了 WeChat Work 的基础字段与高级 api_base_url；本次补上上游后端能力证据，但未启动网页验证渲染/保存/生效。GUI E2E 仍未完成。

### S-003：旧消息过滤

[core/dedup.go](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/core/dedup.go)

StartTime 在进程启动时赋值；IsOldMessage 为 `msgTime.Before(StartTime.Add(-2*time.Second))`。这里不是“当前时间减两秒”。HTTP 挑战不提供 StartTime；持久化历史不能因此保证客户端重启后补答。

### S-004：会话和显示配置

[config.example.toml](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/config.example.toml)

核对 per-project display、thinking_messages、tool_messages、reply_footer、show_context_indicator；默认显示可能额外发送中间消息。reset_on_idle_mins 的未设置默认值为 30 分钟，0 禁用。data_dir 需要独立持久化。run_as_user 有 OS/Agent 限制，不能泛化为 Windows Codex 隔离能力。

### S-005：执行模式和命令权限

[agent/codex/codex.go](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/agent/codex/codex.go)：exec 的 suggest 映射只读 sandbox/非交互审批，auto-edit/full-auto 可写，yolo 绕过保护。[core/privileged_test.go](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/core/privileged_test.go)：测试预期明确普通 `/cron add/list` 及部分 `/commands` 子命令不需要管理员；不能用空 admin_from 代替全部命令限制。这次是阅读测试源码，不是执行这些测试。

[上游 usage 文档](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/docs/usage.md)的项目级 admin_from 说明，以及配置/管理接口中的 disabled_commands 是部署输入，最终命令表和规范化绕过路径仍须测试。

## 2. 微信客服相关项目源码

固定 `binarywang/WxJava@6b4a3d3e78f1795200bb1b303a025384d2b4b0e5`。SDK 的代码是其自身实现证据，不是腾讯当前行为保证。Java 依赖不纳入本项目，只参考必要合同。

### S-006：方法和请求参数

[WxCpKfServiceImpl.java](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/api/impl/WxCpKfServiceImpl.java)

确认客服 add/update/del/list、链接、service_state get/trans、sync/send、customer batchget 使用 POST；**listServicer 使用 GET**。listAccount 使用 offset/limit；sync 重载含 open_kfid/voice_format；customerBatchGet 发送 external_userid_list。修正原接待人员列表 POST 的候选写法。

### S-007：SDK 注释中的限制与重要边界

[WxCpKfService.java](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/api/WxCpKfService.java)

注释记录拉取 token 约 10 分钟有效、可省略但严格限频，limit/has_more 的分页含义；发送记录 48 小时/最多 5 条以及客户继续发送后的后续回复条件。注释还说明 sync_msg 可读客户/人工消息及发送失败事件，但不读通过发送接口发出的成功消息。

这些数字和状态条件只作为核验/测试输入，不是本次确认的最新腾讯政策。旧账号数量和不同接待人员上限注释不能直接变成代码常量。文本精确上限、频控、状态数字和事件连续性没有由本次证据完整确认。

### S-008：消息与客户返回类型

- [WxCpKfMsgSendRequest.java](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/bean/kf/WxCpKfMsgSendRequest.java)：touser/open_kfid/msgtype、可选字符串 msgid；字段存在不证明重试幂等。
- [WxCpKfMsgListResp.java](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/bean/kf/WxCpKfMsgListResp.java)：next_cursor、整数 has_more、msg_list、字符串 msgid、来源/类型/时间及事件等。
- [WxCpKfServiceStateResp.java](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/bean/kf/WxCpKfServiceStateResp.java)：service_state、servicer_userid；此类型本身没有证明完整合法转换图。
- [WxCpKfCustomerBatchGetResp.java](https://github.com/binarywang/WxJava/blob/6b4a3d3e78f1795200bb1b303a025384d2b4b0e5/weixin-java-cp/src/main/java/me/chanjar/weixin/cp/bean/kf/WxCpKfCustomerBatchGetResp.java)：customer_list、invalid_external_userid。昵称具体字段/可见性用真实企业回包确认，不由集合名推断。

## 3. 官方及通用参考

腾讯官方尝试：[微信客服 95166](https://kf.weixin.qq.com/api/doc/path/95166)、[客服 API 入口](https://kf.weixin.qq.com/api/doc/)、[企业微信文档](https://developer.work.weixin.qq.com/document/)，及消息相关路径。本次浏览未取得可验证正文，不能将搜索摘要/博客当已读取官方内容；也没有操作真实账号或发送消息。

[Codex approvals/security](https://developers.openai.com/codex/agent-approvals-security)及[非交互模式](https://developers.openai.com/codex/non-interactive-mode)为只读与非交互执行的一般官方参考，实际固定 CLI 与 cc-connect 参数仍需本机验证。只读不是无读取命令/无外部工具/无跨客户文件风险。

原测试与安全通用参考继续保留：[Go coverage](https://go.dev/doc/build-cover)、[Go test](https://pkg.go.dev/cmd/go#hdr-Test_packages)、[SQLite Backup API](https://www.sqlite.org/backup.html)、[OWASP SSRF](https://owasp.org/www-community/attacks/Server_Side_Request_Forgery)。>95%、模块划分、保留期、超时和 worker 数是项目设计，不是这些来源强制规定。

## 4. 剩余核验（均未完成真实测试）

| 编号 | 已有输入 | 对应发布前还需完成 |
|---|---|---|
| V-001 | 接口与凭证分层设计 | 实际 Secret/应用授权、可管理账号、可信 IP/网络/回调权限 |
| V-002 | 标准加密方向及通知设计 | 官方事件结构、接收方、响应体、重试/时间、有效 token 材料 |
| V-003 | SDK sync 字段及分页类型 | 两客服游标作用域/过滤、origin 数字、期限与断点恢复、实际限频 |
| V-004 | SDK send 方法/字段及窗口注释 | 当前窗口/条数/文本单位/额度恢复/错误码/幂等承诺及失败事件关联 |
| V-005 | 客户批量/无效列表字段 | nickname 实际字段、可见性、时效、批量上限及部分失败回包 |
| V-006 | 账号 CRUD/list/链接方法 | 当前字段、头像、数量/权限、分页与受控真实 CRUD/链接 |
| V-007 | 状态 get/trans、接待人员 GET | 数字枚举、合法转换、工作台事件/延迟/人工恢复规则 |
| V-008 | 客户端媒体路径/限制 | 官方类型/大小/期限、真实 JPEG/语音/文件解码及异常下载 |
| V-009 | 上游固定源码支持主路线 | 未修改二进制/GUI/Windows/Codex E2E、旧消息与权限/预算实验 |
| V-010 | Go/SQLite/HTML 推荐与小项目 ADR | 实际工具版本/驱动/许可证、运行及覆盖统计映射 |

V 项可以按能力切片推进，不是必须全完成才允许写第一行内部测试。相应真实功能发布前，关键未知须闭环；源码支持不自动把 V 项标完成。

## 5. 后续证据维护

每项记录来源/提交/日期、适用账号范围、字段/枚举/限制、合成 fixture、真实测试环境标签与脱敏结果、AT 编号。不得复制客户对话/密钥到夹具，不能整页复制第三方文档替代自己的合同。来源变化保留差异，升级不自动追随 main。

本次仅完成文档评审与修订，新增测试见[AT-059～AT-069](15-feasibility-review.md)。没有业务实现/覆盖报告、没有真实账号管理、没有上线；后续必须有实际证据才能改这些状态。
