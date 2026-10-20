# PostgreSQL Repository（HA-1）

本阶段增加 PostgreSQL 配置存储和两个独立存储客户端的事务一致性。服务仍只允许一个活跃控制面：会话、OIDC 交互、隧道路由、审计和监控尚未共享。HA-2/3/4 继续按 [HA 设计](control-plane-ha.md) 实施。

## 数据与事务

使用纯 Go `github.com/lib/pq v1.12.3`。`tunnel_state` schema v1 保存授权模型 v10；portable backup 继续 v10，JSON/SQLite 格式不变。各实体以 JSONB payload 为规范数据，生成列提供用户名、OIDC issuer/subject、域名、资源和组织归属的数据库约束。grant、group member 与 credential 关系在同一事务维护。默认组织使用内部占位行，导出不包含它。数据库管理员仍是可信边界，不支持绕过 Repository 直接改表。

每次变更先以 READ COMMITTED 锁定 singleton 修订行，再读取最新完整状态、调用变更函数、执行既有全量语义校验、写数据并递增修订号。等待锁的客户端读取上一事务提交后的状态。拒绝、校验错误、数据库写入/约束错误全部回滚；回调不会自动重放。唯一值交换使用延迟唯一约束，组织与资源引用由延迟外键约束。最后本地管理员保护在最新锁定状态上校验。

`UpdateAtRevision` 在调用回调前比较预期修订号，冲突返回 `ErrRevisionConflict`。`RetryUpdate` 最多 8 次显式从新快照重建纯变更意图；调用方须在事务内重新验证操作者权限，不能捕获旧授权或在重试中发出外部副作用。既有 opaque `Update` 回调保持一次执行。若网络在 COMMIT 附近中断，调用方不能推断事务一定未提交，需重读并核对意图；这类错误不会自动重试。

读取使用 REPEATABLE READ / READ ONLY，元数据和多条权限查询来自同一数据库快照。登录、资源、成员权限与目录使用定向 SQL；目录在 SQL 内完成组织/授权过滤、位置 seek 和 limit+1。游标绑定数据库实例 ID、持久修订号和操作人/作用域，可跨存储客户端及正常重启接续；任何成功写入使旧游标返回 409。恢复到新数据库生成新实例 ID，旧游标无效。

无持久授权缓存。错误感知调用用 `SnapshotContext`；旧接口 `Snapshot` 失败返回空状态并计数，绝不返回上次成功的权限。查询失败通过既有 `tunnel_state_query_errors_total` 暴露，正常分页冲突不计数据库故障。驱动错误只保留 SQLSTATE，不输出 DSN、认证材料或 offending payload。

当前写入仍为完整状态替换，使用 COPY 降低传输次数；全局修订行串行化所有配置写入，完整快照/校验为 O(N)。这不是容量优化完成或生产吞吐保证，后续可在保留语义与约束的前提下做增量写入。

## 单活与故障边界

每个 Repository 固定一个连接并在其生命周期持有数据库 advisory lock。独立存储客户端取共享锁；Server 和离线维护取互斥锁。运行期间不能另外启动 Server、shared repository 或维护命令。固定连接失效不能在新连接续租。

单独连接每秒检查固定 backend PID 是否仍持锁，探测超时 5 秒。探测/网络异常使 Repository 不可用并通知 Server；控制面拒绝新请求、取消已有 HTTP/WS/SSE、断开 Agent 并清理会话和连接池。流程不等待配置访问锁，也不在故障通知中查询数据库，避免与正在失败的事务互锁。探测周期是失效检测机制，不是跨节点 HA SLA。

Server 打开时还写持久 runtime_owner。正常关闭先取消应用流并等待 Agent 连接退出，再由原固定连接清除 owner。异常退出或固定连接断开不能清除标记，即使 advisory lock 已释放，新 Server 仍拒绝启动。运维必须先停止/外部隔离旧 Server，再明确执行恢复命令；不能因数据库恢复而自动接管。HA-3 将以租约/generation 及内部流 fencing 实现可验收的自动接管。

运行目录继续有本地生命周期锁，保存 audit/monitor；`state.postgres` 仅包含数据库实例 ID、没有 DSN。目录不能同时含本地 state.json/state.db/sidecar，也不能改绑另一数据库。JSON/SQLite 拒绝打开 PostgreSQL 标记目录。不得共享运行目录。

## 迁移与验证

停机 JSON/SQLite→新 PostgreSQL 数据库保留 ID、密码/令牌散列、授权、Agent 和未使用邀请。SQLite 在临时副本上读取/升级/重建派生索引，原文件字节不变；出现 journal/WAL/SHM 必须先完成原数据库恢复。目标库已初始化即拒绝覆盖。PostgreSQL 的全局 credential 唯一约束比历史本地存储更严格：多个站点复用 legacy token 时明确拒绝导入，要求先轮换重复凭据；不静默转换。portable restore 校验后清空 pending 邀请，防止历史备份复活已消费凭据；跨三后端保持同一规则。

CI 使用 PostgreSQL 15、18 隔离服务，每个测试创建独立随机数据库，测试 DSN 所指库只用于创建/销毁测试库，不删其 schema。覆盖并发写无丢失、回调一次执行、CAS/重建重试、并发最后管理员、唯一值交换、跨组织 FK、注入写失败、跨语句一致读、跨客户端游标、固定连接终止、持久运行标记、三后端备份恢复及真实 Agent HTTP/WS/SSE。完整控制面回归另以 PostgreSQL 后端运行。

参考：[PostgreSQL 事务隔离](https://www.postgresql.org/docs/current/transaction-iso.html)、[显式锁](https://www.postgresql.org/docs/current/explicit-locking.html)、[数据库约束](https://www.postgresql.org/docs/current/ddl-constraints.html)。操作见 [PostgreSQL 手册](../postgres-operations.md)。
