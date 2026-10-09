# 中文管理界面与网关管理 API

> v0.1。以下 `/admin/api/v1` 是本网关拟实现的管理合同，不是腾讯 API，也不是现有可调用接口。

## 1. 界面范围

采用同进程轻量中文管理页，不另建复杂运营平台。页面分为：概览、企业配置、客服账号、账号详情/cc-connect 绑定、消息诊断、操作审计。管理端默认仅允许本机或受控管理网络访问；公开回调与兼容 API 不自动公开管理权限。

概览显示连接状态、可见账号数、启用绑定数、拉取延迟、待处理/UNKNOWN 数量、失败趋势及数据目录健康。不要把“服务存活”显示成“所有消息已送达”。

## 2. 客服账号页面

列表支持同步、搜索、分页、创建、详情。每行显示微信侧名称/open_kfid、企业、网关启停、绑定项目标签、回调健康及最近错误。创建/编辑/删除表单要求和异常语义见 [账号管理](05-account-management.md)。

详情页提供：微信账号资料、客服接入链接、绑定配置、凭证轮换、网关转发开关、接管状态、诊断记录。将“停用 AI 转发”和“删除微信客服账号”放在不同区域；删除按钮位于危险操作区并二次确认。

绑定配置区按 cc-connect 现有字段名逐项显示，明确 API 基础地址在高级选项中；回调 URL 是网关访问的目标，与表单 port/callback_path 对照。不得提供虚假的“配置完成即自动打通内网”提示。

## 3. API 通用规范

JSON UTF-8；时间用 UTC RFC3339，界面按配置时区显示；不使用本地无时区时间做游标/重试。列表分页使用 opaque cursor+limit（默认 20，最大 100 为本地保护值，可配置），不代表账号上限。

统一成功体 `{"data":...,"request_id":"..."}`；错误体 `{"error":{"code":"...","message":"中文说明","retryable":false},"request_id":"..."}`。不在错误体暴露真实 token/secret、客户正文或完整堆栈。

后台修改需管理员会话；Cookie 使用 HttpOnly、Secure、SameSite，写操作校验 CSRF。需要 API 自动化时用独立管理令牌、最小权限、审计和有效期，不使用兼容 access_token 登录后台。

创建/副作用操作要求 Idempotency-Key；同键同参数返回原操作，不重复调用官方；同键不同参数 409。编辑支持 If-Match/revision，过期 409 或 412，全项目固定一种并测试。危险操作使用短期单次 confirmation_token 绑定操作/对象/revision，不把布尔 `confirmed:true` 当作充分确认。

## 4. 端点清单

所有端点前缀 `/admin/api/v1`，除状态读取外的操作都需要鉴权及 CSRF/令牌约束。

| 方法和路径 | 功能 | 主要结果 |
|---|---|---|
| GET /overview | 健康与汇总 | 分阶段状态，不含正文 |
| GET/POST /tenants | 企业列表/新增配置 | 企业 ID、凭证状态，密钥掩码 |
| PATCH /tenants/{id} | 更新企业配置 | revision、需重新核验项 |
| POST /tenants/{id}/test | 检查令牌及权限 | 不发送客户消息 |
| POST /tenants/{id}/accounts/sync | 同步真实账号 | operation_id、分页完成情况 |
| GET /accounts | 筛选/分页查看 | 同步时间、可见状态、绑定摘要 |
| POST /accounts | 创建真实客服 | 202 operation_id |
| GET /accounts/{id} | 详情 | 真实资料、本地状态分开 |
| PATCH /accounts/{id} | 更新真实资料 | operation_id、pending 状态 |
| POST /accounts/{id}/deletion-preview | 风险及确认票据 | 影响范围、确认要求 |
| DELETE /accounts/{id} | 删除真实客服 | operation_id；需确认票据 |
| POST /accounts/{id}/contact-ways | 获取/生成客服链接 | link、场景标签、目标账号 |
| POST /accounts/{id}/binding | 新建绑定 | 字段对照、一次性凭证结果 |
| PATCH /bindings/{id} | 改绑/回调修改 | revision，旧会话处置 |
| POST /bindings/{id}/verify | 加密 echostr 验证 | 分步骤结果 |
| POST /bindings/{id}/disable | 仅停用 AI 转发 | held 策略及时间 |
| POST /bindings/{id}/enable | 验证后恢复 | 新 revision/代际策略 |
| POST /bindings/{id}/unbind | 仅解绑 | 保留微信账号和审计 |
| POST /bindings/{id}/credentials/rotate | 轮换虚拟凭证 | 版本与过渡策略 |
| POST /bindings/{id}/config-export | 再认证后导出 | 现有表单字段/TOML；禁缓存 |
| GET /accounts/{id}/servicers | 接管目标只读列表 | 有权限时显示，不管理通讯录 |
| POST /conversations/{id}/handover | 显式转人工 | 本地 fence+官方操作结果 |
| POST /conversations/{id}/resume-ai | 明确恢复 AI | 官方状态+新代际提示 |
| GET /operations/{id} | 外部副作用进度 | PENDING/SUCCEEDED/FAILED/UNKNOWN |
| GET /deliveries | 消息诊断元数据 | Inbox/Outbox 分开 |
| POST /deliveries/{id}/retry-preview | 安全重试检查 | 是否可安全自动重试及风险 |
| POST /deliveries/{id}/retry | 经确认的重试 | 新 attempt，不改原业务身份 |
| GET /audit-events | 管理操作审计 | 可筛选、脱敏 |
| POST /assets | 上传头像/管理素材 | 受控 asset_id，不接受任意服务器路径 |

Account `id` 为网关对象 ID，不直接拿外部 open_kfid 当 URL 越权凭证。对每个对象操作均核验 tenant 范围，不能只在菜单中隐藏按钮。

## 5. 示例操作

创建真实账号（字段是本网关合同，转换到官方前须按外部契约验证）：

```json
{"tenant_id":"tenant_a","name":"设备咨询","avatar_asset_id":"asset_example"}
```

绑定：

```json
{"project_label":"设备资料问答","callback_url":"http://cc-host.internal:8081/wecom/callback","enabled":false,"expected_revision":0}
```

callback_url 只能由授权管理员设置并通过 SSRF allowlist。project_label 仅用于显示，不自动创建或编辑 cc-connect 项目。

响应 202 只说明操作已登记：

```json
{"data":{"operation_id":"op_example","status":"PENDING"},"request_id":"req_example"}
```

界面必须轮询/刷新到确定结果后才显示“微信账号创建成功”。UNKNOWN 显示“结果待核实，请先同步核查，勿重复创建”，不能自动连续重试。

## 6. 凭证展示与审计

首次生成可一次性显示完整虚拟凭证，之后默认掩码；复制/导出需再次确认并记审计。不得将密钥放 URL、浏览器 localStorage、前端构建产物或第三方分析工具。真实微信 Secret 不通过配置导出传给 cc-connect。

审计字段：管理员、动作、对象、企业、版本、时间、结果、operation_id、脱敏变更摘要。删除审计不跟随账号级联删除。

## 7. 前端验收

所有用户可见文案为中文；载入/空/失败/部分成功/UNKNOWN 状态分别展示；键盘可操作、字段有标签；窄屏可用不要求复杂移动端；双击创建只有一次 operation；后退刷新不重放危险操作；篡改对象 ID 越权失败；XSS 昵称不执行；停用与删除文案不可混淆；回调验证不触发真实客户消息。

前端含自行编写业务脚本时按测试文档单独统计覆盖率，不能因后台整体达标就跳过表单验证和危险操作流程测试。
