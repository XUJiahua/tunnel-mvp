# 鉴权投影与查询故障

无需新增配置或迁移步骤，授权 schema/backup v10 保持不变。JSON/SQLite 自动启用对应查询路径；用户的直接授权、组授权和角色能力语义不变。管理目录支持分页；实时聚合及写入前验证仍有快照成本。

## 监控与排查

- 监控 `tunnel_state_query_errors_total` 的增长。它包含失败的用户、资源、登录和目录 SQL 投影；正常未知 ID/域名不会增长，没有按用户或租户分配标签。
- 数据库读取失败时，已有控制台 cookie 也不能获得新的已授权请求；当前 HTTP 接口可能返回 401 或资源 404，不会通过旧快照继续放行。已有活动流仍受原有会话到期/管理撤销机制管理，不把磁盘读错误等同于一次持久化的会话撤销。
- 控制台会话入口 `/api/metrics` 需要读取当前用户，数据库故障期间可能返回 401；独立 `/metrics` 使用 TUNNEL_METRICS_TOKEN Bearer 认证，不依赖当前用户投影，仍可用于抓取累计计数。令牌未配置时 `/metrics` 返回 404，错误令牌返回 401；计数重启清零。结合独立进程/HTTP 告警，不放开 metrics 或应用鉴权。
- 先确认服务进程、数据目录权限、磁盘和数据库完整性。停止控制面后使用存储手册的 inspect/验证及配对恢复流程，不在运行时直接改表或删除索引/文件。JSON 与 SQLite 仍不能多进程共享目录。

## 复现验证

```bash
go test -race ./internal/state -run TestUserContext
go test -race ./internal/control -run TestUserQuery
go test ./internal/state -run '^$' -bench BenchmarkUserContext -benchtime=100x -count=3
make test
```

完整浏览器流程参见 [验收矩阵](e2e-acceptance.md)，JSON/SQLite 分别运行并保留真实 Keycloak 环境。权限变更之后同时观察新 HTTP 拒绝与已有 WS/SSE 关闭。用户/组/角色等列表的分页与 SQL 投影见 [目录操作](storage-query-operations.md)。
