# 部署与运维（待实现）

> v0.2。仓库尚无程序，以下是实现后的配置要求和操作流程，不是已执行记录。

## 1. 网络和账号前置

确认实际企业微信客服权限、凭证来源、API 可管理账号、官方回调配置和可信 IP/出站要求。管理员身份不等于已启用全部 API 功能。真实凭证只放网关，不粘贴到仓库或 cc-connect 虚拟凭证栏。

网关公开 HTTPS 仅开放必要客服回调和兼容 API；管理端仅本机/受控管理网络，全部读写鉴权。网关到微信、cc-connect 到网关、网关到 cc-connect 回调分别测试。推荐同机/受控内网；跨机器利用已有私有隧道或受限 HTTPS 反代，不以 api_base_url 代替回调通路。

同机多个客户端 HTTP 平台实例分配不同空闲端口，不能只换 path 而共用监听端口。回调 URL 中 localhost 指网关自己，不指管理员浏览器或远程 Windows 主机。

## 2. 初始化

部署实际已测试的版本并配置低权限服务账户、数据目录、独立主密钥和备份。添加单企业真实配置、通过官方 URL 验证；同步或经明确授权创建测试客服，保持 AI 停用。

创建绑定、填写可达 callback_url、生成虚拟 corp_id/secret、数字 agent_id、回调 Token/AES Key。在 cc-connect 现有 WeChat Work HTTP 手动表单填写对应字段，高级 api_base_url 指向网关基础地址，不带 /cgi-bin 路径。

启动客户端后由网关做加密 echostr GET，必须得到正确挑战回显。这个检查验证网络和密钥，不会提交客户问题，也不证明 Codex 已运行、命令权限安全或历史问题可补答。完成下节问答保护及状态/预算检查后，才由授权测试客户发送新问题。

## 3. 现有表单

| 字段 | 来源 |
|---|---|
| 企业 ID / 企业密钥 | 网关绑定的虚拟 corp_id / corp_secret |
| 应用 ID | 网关数字 agent_id，如 1000002 |
| 回调 Token / AES Key | 网关对应绑定生成值；AES 编码为合法 43 字符 |
| 端口 | 客户端当前实例端口，如 8081 |
| 高级回调路径 | /wecom/callback 或实际配置 |
| 高级 API 基础地址 | 如 https://bridge.example.com |
| 高级允许用户 | 已授权兼容 UID；使用 * 必须配合受控回调和网关稳定客户授权 |

网关 callback_url 同机示例为 `http://127.0.0.1:8081/wecom/callback`。跨主机地址必须在回调 allowlist 中。不要填写 BotID、ws_url 或使用个人微信登录扫码。

## 4. 只回答的执行端配置

固定上游 [Codex 适配器](https://github.com/chenhg5/cc-connect/blob/dfad19415a38b00b2c5c288610784d1a7eef337f/agent/codex/codex.go)把 exec 后端的 suggest 映射为只读 sandbox 和非交互审批；auto-edit/full-auto 可写，yolo 绕过沙箱，不适用于本项目。

以下是**合并到既有项目的配置片段**，不是包含所有平台凭证的完整配置；模型与第三方中转站设置保持原位置。实际 CLI 版本及参数效果仍需运行验证。

```toml
[[projects]]
name = "kf-qa"
admin_from = ""
reset_on_idle_mins = 0
# 纵深保护示例，不是全部命令的穷尽列表；结合固定版本命令清单回归。
disabled_commands = ["mode", "dir", "shell", "restart", "upgrade", "cron", "commands", "alias", "list", "switch", "history", "del"]

[projects.display]
mode = "compact"
thinking_messages = false
tool_messages = false
show_context_indicator = false
reply_footer = false

[projects.agent]
type = "codex"
[projects.agent.options]
backend = "exec"
mode = "suggest"
work_dir = "REPLACE_WITH_READ_ONLY_PROJECT_DIRECTORY"
```

admin_from 在 `[[projects]]`，不能放到平台 options；不要用 `*` 给客户管理员权限。空 admin_from 只禁部分特权命令：固定上游权限测试显示 `/cron add` 和部分 `/commands` 操作不需要管理员。因此网关客户入口不转发控制命令，客户端还要禁用对应命令、配置别名、快捷执行、hooks/webhooks 等不需要的控制路径；网关自身的转人工/恢复动作不转成客户端命令。

检查客户端规范化后的输入，包括前置空白、机器人提及和别名映射，防止原文看似普通文本却被客户端解释为命令。上述 disabled_commands 是示例纵深保护，不是完整安全证明；AT-064 未通过不得对外开放。

关闭思考/工具消息可以减少发送和泄露内部信息；compact/quiet 都不能保证 HTTP 只发送一次，长回答仍按 2000 字节分块。生成结果应尽量适合客服窗口，但提示词不是硬性额度控制，网关仍执行预算。

固定配置示例的 reset_on_idle_mins 未设置时默认 30 分钟。连续问答采用 0 禁用自动闲置新会话；人工恢复的新代际仍会主动建立新会话，这不是配置故障。

## 5. Windows、目录和真实安全边界

使用独立低权限 Windows 服务账户运行 cc-connect/Codex，项目资料只读，运行时会话/缓存/日志/附件放受保护目录；网关无权访问 Codex 凭证和资料。不要为了附件可写把整个项目或用户目录开放管理员写权限。

只读不等于不能执行读取命令，也不保护同一账户可读的其他文件。用真实权限测试验证模型无法读取他人附件、会话文件、密钥或越界目录；公共客服仅接入可共享资料，敏感客户附件未经隔离验证不得启用。MCP、联网、钩子与外部工具按实际用途收紧，不能靠提示词保证只读或保密。

上游配置中的 run_as_user 有平台/Agent 限制，不能假定可直接为 Windows Codex 提供隔离。以实际服务账户和文件权限为准。Windows 原生启动/停止、路径、回调端口、媒体和 Codex CLI 兼容必须实际测试；交叉编译不是 Windows 运行验证。

## 6. 诊断与恢复

| 现象 | 检查 |
|---|---|
| 官方回调验证失败 | HTTPS/反代、回调密钥/接收方、真实权限 |
| 通知有但没有正文 | sync scope、cursor、token 有效期、分页及权限；不把通知当正文 |
| 回调 200 但无回答 | 客户端业务解析/授权/旧消息过滤/Agent 错误；200 不代表处理完成 |
| 客户端重启后历史问题不答 | 原始时间可能早于 StartTime-2秒；暂停历史记录，让客户重新发送，不改 CreateTime |
| 发送部分成功 | 2000 字节分块、窗口/额度、状态提示、图片；不要直接重发整份回答 |
| UNKNOWN | 保留记录核查，不以没读到成功回显当作失败，不连续重试 |
| 人工结束仍暂停 | 本地人工暂停/事件缺口/旧代际；核查后明确新代际恢复，不直接放行旧 UID |
| 昵称或会话异常 | 客户端非空 name 缓存、reset_on_idle_mins、是否换代际 |
| 图片/文件失败 | 投递前缓存、真实字节/MIME、下载错误/空体、磁盘权限和大小限制 |

分别展示进程存活、数据库就绪、官方接口、客户端回调及消息状态。健康探针只返回最少信息，第三方偶发超时不应直接引起进程无限重启。

网关或客户端计划重启前暂停领取，保存版本和在途记录。客户端重启后重新做挑战/新消息验证，旧历史默认暂停；网关单独重启不等于客户端重启，确定未发送的任务按可靠性规则恢复。

## 7. 备份、升级和发布

数据库使用一致性备份，连同媒体索引/schema/程序版本保存；主密钥单独安全备份。恢复旧备份默认维护模式，核查已经发生的发送/账号操作及人工状态；禁止自动从旧游标重放所有副作用。

升级 cc-connect/Codex 固定版本并先跑适配器契约和实际 CLI/Windows 测试，不自动追随 latest。日志两端及反代均脱敏 query secret/token，不常态记录客户正文，临时 debug 受限并及时清理。

完整发布需原 R-001～R-024、AT-001～AT-069、真实账号管理/文本/媒体/人工接管、Windows/恢复及 >95% 原始报告齐全。来源未核验或真实用例未执行时明确未完成，不因源码已读就勾选通过。
