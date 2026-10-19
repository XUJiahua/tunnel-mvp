# 更新记录

## 未发布

- 新增 Repository 存储接口、可选纯 Go SQLite、schema/引用校验和数据目录独占进程锁。
- 新增离线 `storage inspect/migrate/export/validate/restore`，保留 JSON 源文件，支持带校验和的跨后端恢复，拒绝覆盖和未知版本。
- 真实控制面及浏览器验收覆盖 JSON/SQLite，CI 验证六个平台的无 CGO 编译，发布包包含完整设计和操作手册。


- 直接编辑站点与应用，支持整站和单独应用授权以及成员权限修改。
- 自助改密、管理员重置成员密码；退出、改密、授权撤销和到期关闭已有 HTTP / WebSocket / SSE 连接。
- 应用隔离连接池，目标更新和 Agent 断线时淘汰旧池。
- 自动 HEAD 探测、健康快照、Prometheus 指标与告警示例。
- 审计界面、过滤查询和大小轮转。
- `agent --watch-config` 验证并重载本地 ACL，通过隧道重连撤销旧连接。
- 增加设计说明、操作手册、实施路线图和真实隧道/浏览器验证。

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
