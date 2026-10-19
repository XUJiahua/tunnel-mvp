# 实施路线图

按已识别的十个方面持续推进。状态区分“本批完成”和“后续待实施”，文档中的后续方案不代表代码已支持。

| 方面 | 本批完成 | 后续待实施 |
| --- | --- | --- |
| 用户管理 | 自助改密、管理员重置成员密码、直接编辑授权、禁用/启用成员、OIDC 专用成员与身份绑定、离线管理员恢复、一次性邀请、多管理员全生命周期 | 组织内角色管理 |
| 资源编辑 | 修改站点/应用、同组织站点迁移、原子批量操作、自定义访问域名 | 跨组织移交需重新登记 |
| 精细权限 | 整站 + 单独应用授权，查询/探测/网关统一校验、用户组授权并集、组织隔离、自定义组织角色 | 多组织成员与组织级 IdP |
| 统一登录 | 控制面 OIDC、显式身份绑定、PKCE、Keycloak OTP 实测、back-channel/RP 退出、acr/amr 策略与应用 SSO 接入手册 | 更多 IdP 实测、禁用事件到会话注销的可靠自动桥接 |
| 会话撤销 | 退出、改密、删除、禁用/身份修改、收回授权、到期主动关闭 HTTP/WS/SSE、设备列表与逐会话撤销 | 跨实例会话与设备风险识别 |
| Agent 管理 | 本地 ACL 配置监视、有效性校验后重连应用 | 远程配置下发的信任模型、自动升级与回滚、同站点多 Agent 容灾 |
| 持久化/HA | Repository 接口、JSON/SQLite、版本及引用校验、进程锁、离线迁移与跨后端备份恢复 | 增量 SQL 读写、共享数据库/会话、跨实例隧道路由 |
| 监控/运维 | 周期 HEAD、状态快照、Prometheus、告警样例、审计 UI 和大小轮转 | 自定义业务健康路径/预期状态、监控历史和图表、完整审计索引、审计历史备份工具 |
| 性能验证 | 按应用复用池、连接上限、断线清池、真实隧道复用/撤销测试 | 工作负载压测、容量报告、生产安全审计、连接绑定身份的专用模式 |
| 应用兼容性 | 路径目标、域名/重定向/Cookie/CSRF 与浏览器上传/WS/SSE 实测 | 更多真实产品与平台实测、OAuth 示例；不承诺通用 HTML/JS 改写 |

## 第二批：存储（stacked PR）

- 基于 `feat/operations-and-access`，以 PR #1 分支为 base，保持前一层可独立审阅。
- 完成存储接口、可选 SQLite、JSON → SQLite 迁移、带版本/校验和的 export/restore、目录锁与双后端验收。
- 设计：[存储设计](design/storage.md)。操作：[迁移与备份恢复](storage-operations.md)。
- 不合并父 PR 或 main，不创建发布标签。

## 第三批：OIDC 身份（stacked PR）

- 基于 PR #2 的 `feat/sqlite-storage`，不合并父 PR，不创建发布标签。
- OIDC 授权码/PKCE、显式 issuer/sub 绑定、专用成员、本地恢复管理员和禁用/绑定变更即时撤销。
- SQLite v1 → v2 事务升级与 backup v1/v2 读取；JSON/SQLite 的签名 IdP 协议和跨源浏览器验收。
- 设计：[OIDC 身份与生命周期](design/oidc.md)。操作：[接入、禁用、Keycloak 与恢复](oidc-operations.md)。第三批协议测试使用独立签名 IdP；第四批补原生 Keycloak + OTP 端到端验证。

## 第四批：会话生命周期与真实 IdP 验收

- 依赖 PR #3：设备会话列表、成员自助/管理员逐会话撤销、离线恢复管理员密码。
- OIDC back-channel logout、RP 主动退出、acr/amr 策略，含回调并发和退出重放防护。
- 使用 Keycloak 26.8.0 + Java 21 原生进程补真实 OTP 与 Agent HTTP/WS/SSE 端到端验证，CI 分别运行 JSON/SQLite。
- 设计：[会话生命周期](design/session-lifecycle.md)。操作：[设备、退出与恢复手册](session-operations.md)。

## 第五批：用户组与成员邀请

- 依赖第四批：用户组授权并集、成员自助设密的一次性邀请、全路径即时权限检查与长连接收回。
- SQLite schema v3 / backup v3，兼容旧数据，恢复备份主动使 pending 邀请失效。
- 设计：[用户组与邀请](design/groups-invitations.md)。操作：[分组、邀请与恢复](groups-operations.md)。

## 第六批：多管理员与管理变更排序

- 依赖第五批：独立管理员、成员提升、管理员启停/降权/删除，当前密码再次验证和最后本地恢复账号保护。
- 所有管理员写入在统一访问写锁内重新鉴权，与角色/会话变更排序；旧会话和应用连接即时撤销。
- 修正发布工具缓解措施：固定 v2.17.1 并验证完整快照，不依赖串行执行规避 v2.18.3 截断。
- 设计：[多管理员](design/administrators.md)。操作：[管理员生命周期](administrator-operations.md)。

## 第七批：组织与自定义角色

- 依赖第六批：资源所有权、组织作用域、自定义能力、组织成员/组/邀请与审计隔离、角色变更即时撤销。
- 默认组织保留旧数据，SQLite schema v4 / backup v4，实际旧布局事务升级与失败回滚。
- 设计：[组织与角色](design/organizations-roles.md)。操作：[组织管理](organization-operations.md)。

## 第八批：资源与路径目标

- 同组织应用迁移、原子批量创建/迁移/删除、自定义访问域名、保留路径前缀。
- 真实浏览器登录/上传/WS/SSE/站点迁移，修复表单 Origin/Referer 与 Agent 快速关闭竞态。
- 设计：[资源与目标](design/resources-targets.md)。操作：[迁移/域名/批量](resource-operations.md)。

## 下一批：监控与审计归档

- 自定义业务健康检查、持久化历史和图表、审计离线归档。
- 继续扩展真实应用接入和容量测试。

## 后续批次：Agent 容灾与容量

- 同站点多 Agent 需要实例 ID、健康选择、流量归属及连接故障处理；不能直接去掉当前重复连接 409 限制。
- 远程策略只允许在本地授权上进一步收紧，或采用显式签名/本地批准模型；控制面不应自行扩大站点网络访问范围。
- 数据库/共享会话、跨实例 Session Registry 和隧道所有权路由共同完成后，再声称支持控制面 HA。
- 在典型 HTTP、上传、WebSocket、SSE 和重连流量下取得延迟、吞吐、内存与 FD 数据，再调整复用参数。

## 可选方向

SSH/RDP/数据库浏览器网关、UDP、P2P、QUIC、DNS/证书自动管理不属于当前 HTTP 多站点访问的前置条件。它们各自需要产品入口、授权和运维设计，独立立项实现。

## 本批交付与验收

- 设计：[管理、授权与运行能力](design/operations-and-access.md)。
- 操作：[升级、日常管理、ACL 重载、监控与审计](operations.md)。
- 自动验证：Go race/vet、Next.js build/TypeScript、真实 Agent 集成与 Playwright 浏览器流程。
- 本批不创建新版本标签；后续按既有 GoReleaser CI 流程发布。
