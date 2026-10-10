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

## 3. 管理后台（`admin`）

未配置 `admin` 则不启动后台。启用时：

| 字段 | 说明 |
|---|---|
| `listen` | 独立地址，须与 `server.public_listen` 不同；默认示例 `127.0.0.1:8091` |
| `password_hash_env` | bcrypt 密码哈希所在环境变量（可用 `htpasswd -bnBC 12 "" '<密码>' \| tr -d ':\\n'` 生成） |
| `origin` | 浏览器 Origin；默认 Secure Cookie 时须为 `https://…` |
| `insecure_cookie` | 仅当 `listen` 为回环时可设 `true`，以支持明文 http 本地访问 |
| `company_name` / `session_ttl_minutes` / `max_sessions` | 显示名与会话策略 |

启用后台时配置中须恰好一个企业。

## 4. 绑定字段（配置或后台）

| 字段 | 说明 |
|---|---|
| `id` / `enterprise_id` / `open_kfid` / `project_id` | 绑定标识与关联 |
| `virtual_corp_id` / `agent_id` | 给 cc-connect 的虚拟企业与应用 ID |
| `virtual_secret_env` 等 | 见上文「首次初始化」 |
| `callback_url` | 网关访问 cc-connect 的完整回调 URL；host:port 必须落在 `callback_targets` |

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

字段级约束以进程启动时的 `Validate` 为准：非法 CIDR、空企业、http API（未显式 insecure）、非回环却 `insecure_cookie` 等都会导致进程拒绝启动。
