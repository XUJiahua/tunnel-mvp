# 管理目录分页与索引操作

不需要新增配置。现有 JSON/SQLite 数据目录、schema/backup v10 和旧备份恢复继续兼容；升级前按既有停机备份流程保存配对数据。SQLite 首次启动会根据已验证组数据重建派生成员索引，启动失败须排查磁盘/权限/数据库，不手工删除运行中表或取消目录锁。

## HTTP 客户端

以下 GET 支持 `?limit=1..200&cursor=...`：`/api/sites`、`/api/services`、`/api/users`、`/api/group-members`、`/api/role-members`、`/api/groups`、`/api/roles`、`/api/invitations`、`/api/organizations`。

- 返回 JSON 形状保持原样，下一页游标在 `X-Tunnel-Next-Cursor`，没有此头表示已读完。permissions 元数据每个角色页都有，虚拟默认组织只在第一页额外出现。
- 后续请求带相同接口、账号和当前权限作用域，URL 编码游标。每页都重新鉴权，游标不能授予权限。
- 409 表示配置写入或重启；丢弃整个部分集合，从不带 cursor 的第一页重新开始。频繁变更时应提示重试，不能拼接新旧集合。
- 400 表示非法大小/游标或跨作用域复用；正常鉴权拒绝仍是原接口的 401/403。目录读取失败返回 503，不返回部分集合或原始 SQL。数据库故障也可能在更早的当前用户校验时返回 401。
- 不传 limit 保留完整数组，适用于旧客户端。控制台自动使用每页 100 条并最多整体重试一次；页面搜索仍针对全部可见记录。

实时 `/api/agents`、站点 Agent 在线状态、`/api/status` 和监控是动态聚合，本批不为它们提供配置游标。旧版客户端或直接请求不分页时，仍可能得到大的响应；大目录应使用上述分页方式。

## 监控与恢复

`tunnel_state_query_errors_total` 现在同时统计资源、鉴权、登录和目录 SQL 读取错误；正常未找到、400/409 和 Close 后调用不增加。独立 `/metrics` 的 Bearer 令牌抓取不依赖用户数据库。计数随进程重启清零。

成员索引不会进入 JSON export；restore 或重新打开 SQLite 会重建。先停机，再 inspect/validate 和配对恢复，不用原始 SQLite 编辑作为修复手段。原子落盘失败不更改已提交授权、索引、内存或修订号；修复磁盘后可重试同一业务变更。

## 复现

```bash
go test -race ./internal/state -run 'TestLogin|TestMembership|TestDirectory|TestUserContext'
go test -race ./internal/control -run 'TestDirectory|TestUserQuery'
go test ./internal/state -run '^$' -bench BenchmarkStorageQueries -benchtime=500ms -count=3
cd web
pnpm run test:unit
pnpm exec playwright test tests/directory.spec.ts
TUNNEL_TEST_STORAGE=sqlite pnpm exec playwright test tests/directory.spec.ts
```

完整交付继续执行 make build/test、双后端浏览器、双后端原生 Keycloak、真实 v0.1.1 升级/回退及六平台包验证。原始基准与边界见 [设计](design/storage-queries.md)，本批实际 CI 结果记录在 PR 中。
