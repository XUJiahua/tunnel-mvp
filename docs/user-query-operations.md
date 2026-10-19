# 鉴权投影与查询故障

无需新增配置或迁移步骤，授权 schema/backup v10 保持不变。JSON/SQLite 自动启用对应查询路径；用户的直接授权、组授权和角色能力语义不变。全量目录及写入前验证仍有快照成本。

## 监控与排查

- 监控 `tunnel_state_query_errors_total` 的增长。它包含失败的用户和资源 SQL 投影；正常未知 ID/域名不会增长，没有按用户或租户分配标签。
- 数据库读取失败时，已有控制台 cookie 也不能获得新的已授权请求；当前 HTTP 接口可能返回 401 或资源 404，不会通过旧快照继续放行。已有活动流仍受原有会话到期/管理撤销机制管理，不把磁盘读错误等同于一次持久化的会话撤销。
- 受保护的 metrics 请求同样需要读取当前用户，故障期间可能无法读取计数。结合独立进程检查和 HTTP 可用性告警；恢复后查看计数累计，重启会清零。不要通过放开 metrics/应用鉴权来恢复可用性。
- 先确认服务进程、数据目录权限、磁盘和数据库完整性。停止控制面后使用存储手册的 inspect/验证及配对恢复流程，不在运行时直接改表或删除索引/文件。JSON 与 SQLite 仍不能多进程共享目录。

## 复现验证

```bash
go test -race ./internal/state -run TestUserContext
go test -race ./internal/control -run TestUserQuery
go test ./internal/state -run '^$' -bench BenchmarkUserContext -benchtime=100x -count=3
make test
```

完整浏览器流程参见 [验收矩阵](e2e-acceptance.md)，JSON/SQLite 分别运行并保留真实 Keycloak 环境。权限变更之后同时观察新 HTTP 拒绝与已有 WS/SSE 关闭。用户/组/角色列表的分页/SQL 化仍是后续代码工作。
