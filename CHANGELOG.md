# 更新记录

## v0.1.0

- Go 控制面、内网 Agent、自动重连和 Agent 令牌轮换。
- 内置 Next.js 控制台，单二进制部署，运行时无需 Node.js。
- 多站点与多应用管理，管理员 / 成员权限与 Agent 目标白名单。
- 独立应用域名、一次性访问票据、Cookie 与跳转重写。
- HTTP / HTTPS、WebSocket、SSE 和经 Agent 执行的链路探测。
- 持久化配置、审计日志和真实隧道端到端测试。
- GoReleaser + GitHub Actions 发布六个平台的二进制包与 SHA-256 校验文件。

MVP 限制：单实例控制面，不含 SSO / HA；内网应用保留自身登录。
