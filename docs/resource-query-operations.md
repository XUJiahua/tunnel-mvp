# 资源查询索引操作

JSON/SQLite 无需新增配置。升级后仍只启动一个控制面，SQLite 启动自动补 `services_custom_domain` 派生索引；较大库第一次启动需要完成索引构建后才接流量。授权 schema/backup v10 保持兼容，按原配对备份/恢复流程操作。

修改自定义域名时，先按 [资源操作](resource-operations.md)准备新 DNS/TLS，再修改资源并从控制台重新打开。旧域名不会保留别名；已有应用 cookie 不保证跨域迁移，用户需要重新完成上游登录。没有索引缓存刷新命令或手动清缓存步骤。

SQLite 查询失败时，网关不会用旧快照继续代理，当前表现可能是 404。检查进程是否正常、state.db 及目录权限、磁盘/文件完整性；不要在运行时直接编辑数据库或删除文件重建。停机后按 [存储手册](storage-operations.md) inspect/validate 已验证的备份，恢复到空目录。不要为了临时恢复路由而去掉目录锁或多进程共享文件。

复现局部性能基准：

```bash
go test ./internal/state -run '^$' -bench BenchmarkResourceQueries -benchtime=100x -count=3
```

完整验证仍用 make test 和双后端 Playwright。用户/角色/组与管理列表尚未迁移到定向 SQL 查询；这些路径的内存/CPU 开销不由此基准覆盖。
