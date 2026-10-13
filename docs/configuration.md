# 配置参考

配置为 JSON 文件，路径由环境变量 `WECOM_KF_BRIDGE_CONFIG` 指定。完整示例见仓库根目录 [config.example.json](../config.example.json)。**密钥只出现在环境变量中，不写进配置文件。**

## 1. 首次启动必填

| 配置路径 | 说明 |
|---|---|
| `storage.database` | SQLite 文件路径 |
| `security.master_key_env` | 主密钥环境变量名（32 字节，用于密封落库字段）；默认名 `WECOM_KF_BRIDGE_MASTER_KEY` |
| `security.callback_targets[]` | 允许回调的 `host` + `port`（1–65535）+ 非空 `allowed_cidrs`（须可解析） |
| `wecom.api_base_url` | 微信 API 根地址，须为 **https**，禁止 userinfo / query / fragment |
| `wecom.customer_origins` | 接受的客户来源 origin 列表（不猜默认值） |
| `enterprises[]` | 至少一个企业：`id`、`tenant_key`、`corp_id`、以及 Secret/回调 Token/AES 的 `*_env` |
| `bindings[]`（可选） | 配置文件引导的绑定；也可启动后仅在管理后台创建 |

企业回调相关环境变量（`secret_env`、`callback_token_env`、`callback_aes_key_env`）在每次启动都需要。

绑定虚拟凭证相关环境变量（`virtual_secret_env`、`callback_token_env`、`callback_aes_key_env`）**仅首次初始化该绑定时需要**：入库后以数据库为准，重启可不再设置；修改请在管理后台轮换凭证。

## 2. 常用可选字段

| 配置路径 | 默认 / 说明 |
|---|---|
| `server.public_listen` | `127.0.0.1:8090`，对微信与 cc-connect 暴露的兼容 API |
| `server.public_base_url` | 可选；写入一次性导出里的 `api_base_url`（无 `/cgi-bin`） |
| `workers.*` | 并发、同步/投递间隔、最大投递次数等；`max_concurrency` 默认 2 |
| `send_policy.window_hours` / `max_sends` | 客服会话窗口与条数预算（默认 48h / 5） |
| `wecom.service_state_map` | 官方 `service_state` 数值 → 内部状态（`AI_ELIGIBLE` / `WAITING_HUMAN` / `HUMAN` / …）；管理后台转人工/恢复依赖此映射 |
| `wecom.allow_insecure_http` | 仅测试/本地 fake 允许 http API；启动会打安全告警 |
| `wecom.treat_48002_as_ai_eligible` | **仅联调**。默认 `false`。为 `true` 时，若 `kf/service_state/get` 返回官方 errcode **48002**（api forbidden），记 WARNING（无 PII）并返回可区分结果 `AI_ELIGIBLE_48002_BYPASS`：**仅发送路径**按可发送处理（不再 70007）；**入站 auto-recover 不得**据此清除自动栅栏。本地已是 HUMAN/WAITING 时发送仍拦截。其它 errcode / 网络错误不变。**严禁生产开启**。 |
| `wecom.handover_phrase` | 入站口令精确匹配（默认 `转人工`）；命中后走与后台相同的转人工 |
| `wecom.handover_welcome` | `send_msg_on_event` 转接话术（默认「正在为您转接人工客服，请稍候…」） |

## 2.1 管理后台在线保存（系统设置页）

`/admin/settings` 可编辑并持久化以下项（POST `/admin/settings/save`，需 CSRF + 会话 + **二次认证**）：

| 字段 | 说明 |
|---|---|
| `wecom.treat_48002_as_ai_eligible` | 联调旁路；保存强制审计并在 UI 显示危险提示；默认 `false`；**严禁生产开启** |
| `wecom.service_state_map` | 官方 state → 内部状态映射（表单为非空 JSON 对象；空串/`{}` 拒绝，避免误清空） |
| `server.public_base_url` | 导出给 cc-connect 的 `api_base_url` |

持久化：安全写回 `WECOM_KF_BRIDGE_CONFIG` 指向的 `config.json`，并**热加载**到运行中的适配器/控制台（无需重启）。Secret/Token/AES 仍只显示环境变量名，不可在此页改明文（轮换走绑定页）。

## 2.2 管理后台绑定配置在线保存（绑定页）

`/admin/bindings` 对**已出现在 `config.json` 的绑定**提供「配置绑定」表单（POST `/admin/bindings/{id}/config-save`，需 CSRF + 会话 + **二次认证**；内网免登会话跳过输密）。仅库中、未写入配置的控制台绑定不显示此表单。

| 字段 | 可编辑？ | 说明 |
|---|---|---|
| `project_id` / `virtual_corp_id` / `agent_id` / `callback_url` | 是 | 写回配置并热加载；`callback_url` 须落在 `callback_targets` |
| `disabled` | 是 | 写入配置后 bootstrap 对齐 SQLite `Active`（`disabled:true` → inactive，升 revision）；与页上「启用/停用」并存时，**保存配置后以 `config.disabled` 为准** |
| `id` / `enterprise_id` / `open_kfid` | 否 | 提交若与现网配置不一致则拒绝 |
| `virtual_secret_env` / `callback_token_env` / `callback_aes_key_env` / `credential_revision` | 否 | 只读显示 env 名；密钥明文不可见、不可改（轮换仍走「轮换虚拟凭证」） |

热加载：更新 `liveCfg.Bindings` → `bootstrapBinding`（project/callback/active）→ `register` / `ApplyBinding`（投递目标与虚拟 API）。**边界**：已密封入库的虚拟 `corp_id` 仍以数据库为准，改 `virtual_corp_id` 只更新配置/liveCfg；要换运行时虚拟企业请走轮换。配置保存**不会**像「改绑」那样轮换客户代际。写盘沿用 `WriteConfigFile` / `replaceFile`（Windows 安全覆盖）。

## 3. 管理后台（`admin`）

未配置 `admin` 则不启动后台。启用时：

| 字段 | 说明 |
|---|---|
| `listen` | 独立地址，须与 `server.public_listen` 不同；可绑 `127.0.0.1` 或 `0.0.0.0`（局域网）；默认示例 `127.0.0.1:8091` |
| `password_hash_env` | bcrypt 密码哈希所在环境变量（可用 `htpasswd -bnBC 12 "" '<密码>' \| tr -d ':\\n'` 生成）；外网/非私网客户端仍走登录页 |
| `origin` | 浏览器可访问的 Origin（如 `http://192.168.1.10:8091`）。`listen` 为 `0.0.0.0`/`::` 时**必须**显式配置；回环监听时默认为 `http://`+listen |
| `insecure_cookie` | 明文 http 时须为 `true`（回环或局域网均可）；https Origin 时保持 `false`（Secure Cookie） |
| `lan_no_login` | 默认 `true`。为 `true` 时，TCP 对端为回环/RFC1918 私网/链路本地的客户端免密进入控制台（自动建会话）；非私网仍须密码。写操作仍校验 CSRF/同源；免登会话跳过二次输密（step-up），审计 Actor 为 `admin-lan@…`、登录摘要 `lan_no_login`。不信任 `X-Forwarded-For` |
| `company_name` / `session_ttl_minutes` / `max_sessions` | 显示名与会话策略 |

启用后台时配置中须恰好一个企业。管理 listener 仅本机或受控局域网；不要暴露到公网。

## 4. 绑定字段（配置或后台）

| 字段 | 说明 |
|---|---|
| `id` / `enterprise_id` / `open_kfid` / `project_id` | 绑定标识与关联 |
| `virtual_corp_id` / `agent_id` | 给 cc-connect 的虚拟企业与应用 ID |
| `virtual_secret_env` 等 | 见上文「首次初始化」 |
| `callback_url` | 网关访问 cc-connect 的完整回调 URL；host:port 必须落在 `callback_targets` |
| `disabled` | 默认 `false`。为 `true` 时该配置绑定启动后为 inactive（停发令牌、栅栏在途发送）。对**已存在**行同样生效（升 revision）。配置里列出的绑定每次启动会把 `active`/project/callback 与 JSON 对齐；仅库中的控制台绑定不受此字段影响。也可在绑定页在线保存并热加载（见 §2.2） |

一次性导出（管理后台）按 cc-connect WeChat Work 表单给出：`corp_id`、`corp_secret`、`agent_id`、`callback_token`、`callback_aes_key`、`port`、`callback_path`、`api_base_url`、`allow_from`。

## 5. 启动方式（概要）

```bash
export WECOM_KF_BRIDGE_CONFIG=/path/to/config.json
export WECOM_KF_BRIDGE_MASTER_KEY="$(openssl rand -base64 32)"
# 以及企业 Secret、回调 Token/AES、（首次）绑定虚拟凭证、（若启用后台）管理员哈希
./wecom-kf-bridge
```

- 公开 listener：兼容 API + `/webhooks/wechat-kf/{tenant_key}` + 健康检查。
- 管理 listener：仅本机/受控网络；不要暴露到公网。
- 发布产物与校验和见 GitHub Releases；版本说明见 [CHANGELOG.md](../CHANGELOG.md)。

字段级约束以进程启动时的 `Validate` 为准：非法 CIDR、空企业、http API（未显式 insecure）、http Origin 却未设 `insecure_cookie`、`0.0.0.0` 监听却未设可访问 `origin` 等都会导致进程拒绝启动。
