# 实施路线图

按已识别的十个方面持续推进。状态区分“本批完成”和“后续待实施”，文档中的后续方案不代表代码已支持。

| 方面 | 本批完成 | 后续待实施 |
| --- | --- | --- |
| 用户管理 | 自助改密、管理员重置成员密码、直接编辑授权、禁用/启用成员、OIDC 专用成员与身份绑定、离线管理员恢复、一次性邀请、多管理员全生命周期、组织内角色有界委派 | 多组织成员 |
| 资源编辑 | 修改站点/应用、同组织站点迁移、原子批量操作、自定义访问域名 | 跨组织移交需重新登记 |
| 精细权限 | 整站 + 单独应用授权，查询/探测/网关统一校验、用户组授权并集、组织隔离、自定义组织角色 | 多组织成员与组织级 IdP |
| 统一登录 | 控制面 OIDC、显式身份绑定、PKCE、Keycloak OTP 实测、back-channel/RP 退出、acr/amr、Keycloak 禁用自动撤销与 SSO 接入手册 | 更多 IdP/非默认 subject 的自动桥接实测 |
| 会话撤销 | 退出、改密、删除、禁用/身份修改、收回授权、到期主动关闭 HTTP/WS/SSE、设备列表与逐会话撤销 | 跨实例会话与设备风险识别 |
| Agent 管理 | 本地 ACL 监视、同站点多实例、独立凭据、故障切换、远程 ACL 收紧、签名升级/回滚 | macOS/Windows 服务实机验收 |
| 持久化/HA | Repository 接口、JSON/SQLite、版本及引用校验、进程锁、离线迁移与跨后端备份恢复、增量 SQL 写入、定向 SQL 查询、PostgreSQL 事务/共享认证/outbox/授权租约、节点代际/mTLS 路由、HA-4 共享观测/排空/升级恢复 | 独立主机、数据库主备切换及生产恢复指标 |
| 监控/运维 | 业务健康检查、持久化历史/图表、Prometheus、全部保留审计索引/分页、增量轮转与离线归档恢复 | 跨实例指标 |
| 性能验证 | 按应用复用池、连接上限、断线清池、真实隧道复用/撤销、会话隔离池与本地容量报告 | 生产负载/目标 SLO 验证、独立安全审计 |
| 应用兼容性 | 路径目标、域名/重定向/Cookie/CSRF、独立上游 OIDC 应用与浏览器上传/WS/SSE 实测 | 更多真实产品与平台实测；不承诺通用 HTML/JS 改写 |

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

## 第九批：监控历史与运维归档

- 业务健康路径/方法/预期状态、持久化曲线与样本上限、存储故障指标。
- 真实浏览器采样、成员隔离、停机归档/校验与跨授权后端恢复后继续采样。
- 设计：[监控历史](design/monitoring-history.md)。操作：[监控与归档](monitoring-operations.md)。

## 第十批：全部保留审计索引

- 当前和保留轮转文件完整索引、组合过滤、组织可见性与有界分页，重启/轮转游标代际校验。
- 修复延长运行下暴露的 WebSocket 写截止时间竞态，源码/许可和专门的 race/vet 验证随仓库保留。
- 设计：[审计索引](design/audit-index.md)。操作：[查询与恢复](audit-operations.md)。

## 第十一批：Agent 实例

- 同站点多个独立进程、独立凭据与全站点轮换、活动通道归属、故障后的新连接分配。
- 设计：[Agent 实例](design/agent-instances.md)。操作：[Agent 管理](agent-operations.md)。

## 第十二批：远程收紧策略

- 本地显式开启、逐解析 IP 的授权交集、变更重连和超时失效关闭、策略版本遥测。
- 设计：[远程策略](design/agent-remote-policy.md)。操作：[配置与故障](agent-policy-operations.md)。

## 第十三批：签名升级与回滚

- 本地固定公钥和源、单调序号、候选校验、安装日志/崩溃恢复、实际 Agent 就绪与自动回退。
- 设计：[签名升级](design/agent-updates.md)。操作：[发布与主机操作](agent-update-operations.md)。

## 第十四批：连接身份隔离与容量

- 独立登录会话的串行上游连接、独立健康池、撤销清池和有界 LRU；状态/备份 v9。
- 设计：[会话连接池](design/session-pools.md)。操作：[连接模式](connection-mode-operations.md)。验收：[容量报告](capacity-report.md)。

## 第十五批：组织角色有界委派

- 同组织、原有及新权限双重上限；最小角色目录与自我降权即时撤销。
- 设计：[有界委派](design/delegated-roles.md)。操作：[角色委派](delegated-role-operations.md)。

## 第十六批：SQLite 增量写入

- 稀疏实体/授权写入、原子唯一值互换/位置重排、提交后差异缓存及失败重试。
- 设计：[增量写入](design/incremental-sqlite.md)。操作/基准：[SQLite 增量持久化](sqlite-incremental-operations.md)。

## 第十七批：审计增量轮转

- 稳定来源 ID、健康轮转不重写保留事件、过期源清理，故障/不一致时原文件重建。
- 设计：[增量轮转](design/audit-rotation.md)。操作：[轮转与升级](audit-rotation-operations.md)。

## 第十八批：Keycloak 用户状态桥接

- 独立只读 service client、登录校验、活跃 OIDC 轮询、过期失效关闭和本地恢复；真实禁用不调用 logout 的验收。
- 设计：[状态桥接](design/keycloak-session-sync.md)。操作：[配置与演练](keycloak-sync-operations.md)。

## 第十九批：上游应用 OIDC 示例

- 独立应用 client、Authorization Code/PKCE、服务端 token 验证、路径挂载与应用本地退出。
- 真实 Keycloak OTP 控制台 → Agent → 应用独立 OIDC → 再次 SSO；禁用后应用入口被拒绝。
- 设计：[应用 OIDC](design/application-oidc.md)。操作：[接入与故障演练](application-oidc-operations.md)。

## 第二十批：验收记录与 HA 设计提案

- 对照前十九批补齐端到端矩阵、实际验证与未验证范围，保留尚未实现的代码扩展。
- 设计：[控制面 HA](design/control-plane-ha.md)；操作：[HA/目标环境验收](ha-operations.md)；证据：[端到端矩阵](e2e-acceptance.md)。
- 本批没有实现或宣称支持 HA，后续代码工作按 HA-1 至 HA-4 分阶段推进。

## 第二十一批：资源定向查询

- 站点/应用 ID、自定义域名点查，提交排序、数据库错误不回退旧快照、非空域名索引。
- 设计：[定向查询](design/resource-queries.md)；操作/基准：[查询索引](resource-query-operations.md)。
- 本批仅覆盖资源；第二十二批补当前用户鉴权投影，管理列表仍继续拆解。

## 第二十二批：用户鉴权投影

- 当前用户、直接授权及关联角色/组的一致读取；失败拒绝鉴权、返回值隔离及查询失败指标。
- 设计：[用户投影](design/user-queries.md)；操作/基准：[鉴权查询](user-query-operations.md)。
- 组成员筛选仍扫描 JSON membership，管理列表等剩余查询尚未改造。

## 第二十三批：单实例交付收敛

- 基于 #22，统一 README/存储/身份/审计/监控操作文档，明确已发布 v0.1.1 与开发能力的区别。
- 更新 #22 验收/CI 基线；使用真实发布旧版 Server/Agent 验证两种后端升级、跨后端配对恢复及原版本回退，并加入 CI/发布门禁。
- [交付阶段与门槛](release-readiness.md)、[v0.1.1 升级与恢复](upgrade-v0.1.1.md)。下一阶段准备最终整合提交和单实例发布候选；HA 及外部环境验收仍未完成。

## 后续边界

- JSON/SQLite 保持单实例；PostgreSQL 的共享认证、隧道所有权路由及观测/排空已由 HA-1～HA-4 实现并验收。跨宿主机和数据库主备切换仍需目标环境证据，不能用共享 SQLite 文件或去掉进程锁代替。
- macOS/Windows 实机服务、更多 IdP、真实 AD/NTLM 应用及生产负载尚未完成，交叉编译不能替代目标环境验收。
- 多组织成员、组织级 IdP、设备风险识别与其余 SQL 查询仍是待实施扩展，不因前述批次完成而标记支持。

## 可选方向

SSH/RDP/数据库浏览器网关、UDP、P2P、QUIC、DNS/证书自动管理不属于当前 HTTP 多站点访问的前置条件。它们各自需要产品入口、授权和运维设计，独立立项实现。

## 本批交付与验收

- 设计：[管理、授权与运行能力](design/operations-and-access.md)。
- 操作：[升级、日常管理、ACL 重载、监控与审计](operations.md)。
- 自动验证：Go race/vet、Next.js build/TypeScript、真实 Agent 集成与 Playwright 浏览器流程。
- 本批不创建新版本标签；后续按既有 GoReleaser CI 流程发布。

## 后续阶段四：存储查询完善

- 登录名和 OIDC 身份点查、规范组数据的同事务成员索引及启动/恢复重建。
- 组织/资源/用户/组/角色/邀请管理目录的权限过滤、按位置分页和修订冲突；旧 API 形状兼容，控制台自动拉全并有界重试。
- 显式记录 Update 的验证/落盘/缓存/修订发布边界，真实 SQL 失败注入、并发读取、恢复矩阵和局部基准。
- 设计：[存储查询](design/storage-queries.md)。操作：[分页与索引](storage-query-operations.md)。实时 Agent/监控聚合与全量写校验保留原路径；HA 按 HA-1–HA-4 另行实现。

## HA-1：PostgreSQL 事务存储

阶段四 PR #26 已合入 main（4eb4a66）。本阶段增加 PostgreSQL schema v1 / 授权模型 v10、修订锁/CAS/显式意图重建、定向查询、离线迁移与配对恢复、固定连接单活锁及异常运行标记。真实数据库 CI 使用 15/18；HA-2 已接入共享会话/OIDC/撤销证明，HA-3 增加显式多节点与 owner/generation/mTLS。见 [实现设计](design/postgres-repository.md)及[操作手册](postgres-operations.md)。

## HA-2：共享认证生命周期

PostgreSQL schema v2 保存会话、票据、OIDC 交互、注销与身份证明；配置/撤销/outbox 同事务，调用者在写锁内重新鉴权。应用 HTTP/WS/SSE 每秒重新证明，独立 5 秒租约限制失联时继续访问。真实双 HTTP 节点测试通过私有注入构造器运行，后续 HA-3 加入显式集群启动与 Agent owner/内部路由。认证运行记录不进入 portable backup v10，JSON/SQLite 认证生命周期保持本地。完整验收结果以本阶段 PR 与 CI 为准；见 [设计](design/shared-authentication.md)。

## HA-3：跨节点 Agent 路由

PR #30 引入 schema v3 节点 incarnation/10 秒数据库租约、5 秒本地截止时间、递增 Agent generation 与就绪路由、独立 mTLS CONNECT 流及接收端授权重证。包括真实双 Server/LB/Agent 的跨节点流、注销与心跳中断/接管/POST 不重放测试；验收结论以最终 CI 为准。部署见 [HA 手册](ha-operations.md)，实现见 [路由设计](design/cluster-routing.md)。HA-4 继续聚合/来源去重、滚动版本和完整恢复演练。

## HA-4：共享观测与维护恢复

开发分支 `feat/cluster-observability` 实现独立版本的共享审计/健康表、本地持久队列、事件 ID 幂等重放、来源节点标记、事务内权限重证、节点报告及定向排空 API。控制台增加节点覆盖视图和来源过滤。基础 schema v3、运维归档格式及内部流协议保持兼容，边界见[观测设计](design/cluster-observations.md)。

[PR #31](https://github.com/XUJiahua/tunnel-mvp/pull/31) 提供 PG15/18 双节点验证及真实 CLI/Agent/浏览器演练：HA-3/HA-4 混跑、排空保留已有 SSE、逐节点升级与回滚、配置加各节点运维归档恢复到全新数据库、会话失效及重放不重复。基础存储与内部流协议不变。本地队列故障、JSON/SQLite 回归和真实旧版本文件兼容已验证；远端验收与发布包以最终提交的 CI/产物为准。目标宿主机、数据库切换、外部 LB/TLS 与生产 SLO 仍需部署验收。

PR #31 已合入 main（dce12a9）；最终 head 040a7a4 的完整 CI、真实 PG15/18 升级/恢复与六平台快照验证通过。

## 后续：容器部署与故障演练

源码提供双节点/独立目录、PostgreSQL、真实 HAProxy TLS 和独立 Agent/应用网络的[部署实验](ha-deployment-lab.md)。自动脚本覆盖主动健康摘流、排空保留 SSE、应用 WebSocket、owner SIGKILL、数据库无响应 fencing/显式恢复和同 CA 叶证书轮换。PG15/18 CI 及报告为执行证据；单机容器不代表跨宿主机、Named Tunnel 或数据库主备切换已通过。
