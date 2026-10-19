# 更新记录

## 未发布

- 全部保留审计文件索引、按组织/操作者/资源/时间过滤、有界分页与游标代际校验；monitor.db schema v2。
- 固定源码本地修复 remotedialer 心跳/帧写 deadline 竞态，补嵌套模块 race/vet 与真实 socket 超时测试并保留许可。

- 自定义业务健康路径/方法/预期状态、持久化监控历史和图表、健康存储失败指标；授权 schema/backup v6。
- 停机审计/监控归档、完整 checksum/gzip/SQLite 校验和无覆盖/可续传恢复，pending 标记阻止不完整启动。

- 应用同组织跨站点迁移、原子批量创建/迁移/删除、自定义访问域名、保留前缀的路径目标；SQLite/backup v5。
- 修正真实浏览器表单的 Origin/Referer 兼容和 Agent 快速取消时的接收/心跳关闭竞态。

- 组织所有权与自定义角色、按组织过滤管理目录/审计、跨组织拒绝、角色变更撤销全部设备与应用连接；SQLite/backup v4 兼容升级。

- 多管理员创建、提升、停用、降权、删除和当前密码再次验证；保护最后一个本地恢复管理员，全部管理写入与会话撤销统一排序。

- 用户组权限并集、实时组授权撤销，一次性成员邀请与自主设密。
- SQLite schema v3 / backup v3；恢复历史备份清空 pending 邀请，防止已用链接重获效力。

- 会话设备列表、成员/管理员逐会话撤销、离线管理员密码恢复。
- OIDC back-channel/RP 退出、可选 acr/amr 策略；补真实 Keycloak + OTP + Agent HTTP/WS/SSE 双后端端到端验证与 CI。

- 发布工具固定 GoReleaser v2.17.1 并串行执行；v2.18.3 在本地串行模式也出现 gzip 截断，v2.17.1 连续两套六平台快照通过。保留完整解压/CRC/SHA-256 校验。

- 新增可选 OIDC 控制台登录、PKCE/state/nonce 与签名验证、显式身份绑定、OIDC 专用成员及账号禁用/启用。
- 禁用或身份变更即时撤销所有本地会话和 HTTP/WS/SSE；保留本地恢复管理员，不自动信任 IdP 角色。
- SQLite schema v2 事务升级、backup v2 与旧 v1 读取兼容；新增 OIDC 设计、Keycloak 操作示例和双后端浏览器验收。

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

- 同站点多 Agent：独立凭据、在线清单、TCP 通道轮询、单实例撤销和兼容入口停用；状态/备份 v7。

- Agent 本地显式开启远程策略，CIDR/主机/端口与本地 ACL 逐 IP 取交集，轮询变更关闭旧流，策略失效关闭隧道；状态/备份 v8。

- Agent supervisor：本机公钥固定、Ed25519 manifest、下载完整性、高水位防重放、候选就绪检查、自动及离线回滚。

- 应用可选登录会话独立串行连接池、探测隔离、会话撤销清池与有界 LRU；状态/备份 v9。新增可复现双 Agent HTTP/上传/WS/SSE/RSS/FD 容量报告。

- 组织 roles:manage 有界委派：创建/修改/分配均受原有及新权限上限约束，最小角色成员目录、即时撤销；状态/备份 v10。

- SQLite 授权状态只更新变化实体/关系，外键事务末尾校验、唯一值/位置互换、提交后缓存发布；新增实际写行与稀疏写入基准。
