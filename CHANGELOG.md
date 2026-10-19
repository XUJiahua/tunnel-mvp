# 更新记录

## v0.1.1

- 使用 Cobra / Viper 管理命令、JSON / YAML 配置、环境变量、参数覆盖和日志选项，兼容 `-config`。
- 增加 `agent diagnose`、`--check-config` 和 shell 补全。
- 探测与代理错误保留 DNS / ACL / TCP / TLS / HTTP 阶段、目标、解析 IP 与请求 ID，并在界面显示完整诊断。
- Agent 鉴权诊断上报绕过 remotedialer 的 100 字节错误限制，兼容旧隧道协议。
- 等待站点连接退出后关闭审计文件，避免正常停机产生审计写入错误。
- 前端统一使用 pnpm，固定包管理器版本，并更新本地构建、Docker 和 CI / 发布流程。

## v0.1.0

- Go 控制面、内网 Agent、自动重连和 Agent 令牌轮换。
- 内置 Next.js 控制台，单二进制部署，运行时无需 Node.js。
- 多站点与多应用管理，管理员 / 成员权限与 Agent 目标白名单。
- 独立应用域名、一次性访问票据、Cookie 与跳转重写。
- HTTP / HTTPS、WebSocket、SSE 和经 Agent 执行的链路探测。
- 持久化配置、审计日志和真实隧道端到端测试。
- GoReleaser + GitHub Actions 发布六个平台的二进制包与 SHA-256 校验文件。

MVP 限制：单实例控制面，不含 SSO / HA；内网应用保留自身登录。
