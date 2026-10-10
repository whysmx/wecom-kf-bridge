# 维护说明

本仓库产品代码已可用作试验网关；介绍项目请读 [README.md](README.md)、[docs/architecture.md](docs/architecture.md)、[docs/configuration.md](docs/configuration.md) 与 [config.example.json](config.example.json)。

## 约定

1. **边界**：微信客服 → 本网关 → 未修改的 cc-connect → 现有 Agent。不改客户端源码、不新增平台插件、不把网关当成模型运行时。
2. **配置**：密钥只走环境变量；行为变更若影响配置字段，同步改 `config.example.json` 与 `docs/configuration.md`。
3. **版本**：用户可见变更写入 `CHANGELOG.md` 并更新 `VERSION`；Release 说明由 CHANGELOG 对应小节生成。

实现细节以代码与测试为准。
