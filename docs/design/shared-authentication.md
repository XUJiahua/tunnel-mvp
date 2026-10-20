# 共享认证生命周期（HA-2）

PostgreSQL 后端把会话、应用票据、应用 Cookie 索引、登录限流、OIDC state/nonce/PKCE、注销重放记录和 Keycloak 状态放入私有认证表。JSON/SQLite 继续使用进程内认证状态。本阶段保持生产 Server 单活；双节点测试通过私有构造器注入两个独立 Repository，不提供跳过运行锁的部署参数。跨节点 Agent owner、generation、内部 mTLS 与数据转发属于 HA-3。

## 数据与原子性

数据库 schema v2 在启动事务中升级 v1；配置与 portable backup 仍为 v10。`auth_records` 使用种类/索引联合主键，session/user、grant/session 外键延迟到提交检查；会话公开 ID 唯一。浏览器 bearer、OIDC state 和浏览器绑定 Cookie 仅保存 SHA-256 索引；短期 nonce/PKCE verifier 供跨节点回调使用，ID/access token、client secret 不落库。认证表不进入 portable backup 或运维归档，恢复到新库要求重新登录。

配置和认证写入都先锁 singleton meta 行，再读取、验证和提交。配置写入比较变更前后用户，禁用、改密、身份绑定及角色变更在同一事务删除旧会话和子凭据；应用路由变更清除相关票据/凭据。直接授权和组变更保留设备会话，下一次请求使用最新权限，已有流通过周期证明撤销。控制面的配置变更在数据库锁内再次验证调用会话和当前有效用户/权限，旧调用者不能等待其他事务撤销后继续提交。目录查询绑定鉴权时的配置修订，期间变更返回 409 重试。

一次性票据、OIDC state 消费以及注销 JTI 标记与撤销在同一事务完成。注销 cutoff 阻止注销前发起的登录回调重新生成会话；JTI 重放不再次撤销之后的新会话。时间与到期判断使用数据库时钟。会话最多 4096、票据和应用凭据各 8192、待回调和限流各 256、注销索引和身份状态各 4096；到期记录清理后仍满时拒绝创建。

热路径按索引读取会话/应用凭据，并在同一 REPEATABLE READ 快照读取对应用户、角色、组和应用。普通请求不加载全部认证记录。低频认证写入仍加载有界认证集合；登录、票据创建/兑换和设备管理当前读取完整配置，配置写入仍全量校验/替换。这是正确性实现，不是容量优化或吞吐承诺。

## 已有流与通知丢失

配置/撤销/策略事件与修改在同一事务写 `auth_outbox`，事件 ID 由 meta 写锁排序。consumer 持久保存已证明的水位；只确认权威快照覆盖的 ID。`auth_policy.last_event_id` 独立保存高水位，清空旧事件不会把水位归零。保留最多约 100000 条或已确认且超过 24 小时的事件；消费断层要求重新证明，24 小时不活跃 consumer 清理。

每个 HTTP/WS/SSE 应用流有独立的 5 秒授权租约。后台每秒批量读取当前活跃流对应的会话、凭据、权限、应用路由及身份状态；新证明只能续到“查询开始时间 + 5 秒”，并受真实会话/凭据/身份到期限制。超时定时器独立于数据库和后台 worker，过期后不能被迟到回复复活。通知仅为提示，即使 poll 丢失，每轮仍重新证明；查询阻塞或失败时原定时器到期关闭流。新请求每次读取数据库，不使用旧权限放行。

5 秒是进程正常调度时的授权有效期，不保证暂停的进程或断网客户端能在精确墙钟时刻观察 TCP 关闭。撤销不重放 POST，也不迁移既有连接。

## Keycloak 与运行策略

共享身份检查以 check ID 和短期 pending 状态合并跨节点请求，旧完成结果不能覆盖新检查。成功 enabled 证明有有效期和 epoch；证明过期或禁用后旧 OIDC 会话删除/拒绝，后续 enabled 结果不能复活旧 Cookie。请求失败不延长旧证明，不把先前成功冒充新的登录检查。密码恢复会话不依赖该证明。

所有节点必须使用相同 publicURL、accessURL、OIDC 和桥接策略指纹。共享客户端不匹配即拒绝；生产单活 Server 在重启时改变策略会清空旧认证记录并发布策略事件。每个本地目录的私有 `node.id` 是 consumer 标识，不是节点证书，不能用于 HA-3 身份认证。

生命周期 advisory lock 使用专用固定连接；请求查询使用独立连接池，最多四个连接（含固定连接）。请求取消可能由驱动丢弃查询连接，但不能释放运行锁。固定连接每秒探测，5 秒超时；锁/owner 丢失隔离整个 Server，保持离线恢复要求。

## 验证入口

`TUNNEL_REQUIRE_POSTGRES_TESTS=1 go test -race -count=1 ./internal/state ./cmd/tunnel -run Postgres` 验证事务、到期、策略、身份 epoch、持久水位和请求取消后的运行锁。

`TUNNEL_REQUIRE_POSTGRES_TESTS=1 TUNNEL_TEST_STORAGE=postgres go test -race -count=1 ./internal/control` 包含真实双 HTTP 节点/负载均衡器、跨节点 OIDC 回调与注销、一次性票据、通知丢失/证明阻塞下的 SSE/WS 撤销，以及已撤销调用者的事务拒绝。CI 使用隔离 PostgreSQL 15、18 服务执行；本地缺 DSN 的跳过不能当作数据库通过。
