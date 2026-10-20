# PostgreSQL 操作手册（HA-1）

适用于包含本阶段代码的构建；未发布的新能力不能由 v0.1.1 二进制执行。支持 PostgreSQL 15 以上，CI 验证 15 和 18。当前仍部署一个活跃 Server，数据库服务需独立维护，运行目录不能共享。会话、OIDC、隧道、审计/监控尚未组成集群。

## 新部署

预先创建独立空数据库及专用角色，角色需拥有该库内 `tunnel_state` schema、建表/约束及读写权限；不要求 superuser。不要与其他应用复用数据库。连接使用直连或保持会话的池；transaction/statement pooling 不适用于生命周期 advisory lock。每个打开的 Repository 最多两个数据库连接。

通过 secret manager/服务私有环境提供 `TUNNEL_POSTGRES_DSN`，不要提交到配置、命令行或日志；生产连接显式使用 `sslmode=verify-full` 和受信 CA。以下主机、库、角色和证书路径需按部署替换：

```bash
export TUNNEL_POSTGRES_DSN='postgres://tunnel@db.internal/tunnel?sslmode=verify-full&sslrootcert=/run/secrets/db-ca.pem'
# 密码同样由部署的 secret 环境/凭据机制提供，不写进 shell 历史。
./tunnel server --storage postgres --data-dir /srv/tunnel-pg --check-config
./tunnel server --storage postgres --data-dir /srv/tunnel-pg
```

首次空库初始化需要 `TUNNEL_ADMIN_PASSWORD`（12–72 字节）。配置文件仅保存 `"storage":"postgres"` 和运行 `dataDir`。`--check-config` 校验配置并隐藏 DSN，**不代表已成功连接或验证数据库**。已有数据库不会因设置初始管理员密码而重置。

`state.postgres` 把运行目录绑定到数据库实例 ID；保留它，不能手工修改/删除以切换后端。目录含 JSON/SQLite 状态或 sidecar 时启动拒绝，换新的运行目录。audit.jsonl 与 monitor.db 仍在本地，按 [监控归档](monitoring-operations.md) 单独备份。

## 从 JSON/SQLite 迁移

先停止 Server 并禁用自动拉起，按原后端导出/校验授权备份和运维归档，另留整目录副本。保持 DSN 指向新的空库：

```bash
./tunnel storage migrate --data-dir /srv/tunnel-old \
  --from sqlite --to postgres --target-data-dir /srv/tunnel-pg
# JSON 源改为 --from json；源文件保持不变。
./tunnel storage inspect --storage postgres --data-dir /srv/tunnel-pg
./tunnel storage restore-operations --data-dir /srv/tunnel-pg --input operations.tar.gz
./tunnel server --storage postgres --data-dir /srv/tunnel-pg
```

迁移保留未消费邀请、用户/组织/角色/组、应用、ID 与凭据散列；切换后重新登录，Agent 重连，旧活动流不能迁移。不要同时启动旧配置。SQLite 源有 sidecar 时先按原后端完成恢复再重试；不删除 sidecar 绕过检查。数据库已经初始化即拒绝覆写。PostgreSQL 凭据注册表要求站点 token 散列全局唯一；若历史 JSON/SQLite 曾复用站点 token，迁移明确拒绝，先在原系统轮换重复凭据，不会静默改写源凭据。

## 离线备份、恢复与回退

关闭 Server 后操作；运行中的数据库互斥锁也阻止另一个目录执行维护：

```bash
./tunnel storage export --storage postgres --data-dir /srv/tunnel-pg --output state-backup.json
./tunnel storage validate --input state-backup.json
./tunnel storage export-operations --data-dir /srv/tunnel-pg --output operations.tar.gz
```

授权备份包含凭据散列，默认 0600，不能覆盖已有输出。恢复 PostgreSQL 需新数据库、新运行目录，并把 DSN 切到新库：

```bash
./tunnel storage restore --storage postgres --data-dir /srv/tunnel-restored --input state-backup.json
./tunnel storage restore-operations --data-dir /srv/tunnel-restored --input operations.tar.gz
```

同一授权备份可 `--storage json` / `sqlite` 恢复到新目录。所有 portable restore 清空 pending 邀请；正常重启和 migrate 保留邀请。回退时停止当前 Server，只在新目标启动一个 Server，保持入口/TLS/IdP 配置一致。旧二进制不能直接读新 PostgreSQL 数据库。

离线改密使用 `storage reset-admin-password --storage postgres --data-dir ... --username admin`，密码由 `TUNNEL_RECOVERY_PASSWORD` 提供；仅恢复已有管理员，不创建账号。

## 异常运行标记

固定连接丢失、进程崩溃后会保留 runtime_owner，控制面拒绝新访问，旧流结束；`healthz` 返回 503。数据库重新连通不代表旧进程已停止。必须先通过服务管理器停止并确认旧 Server 不会自动复活，或在宿主机/网络层外部隔离它，再执行：

```bash
./tunnel storage recover-postgres-runtime --storage postgres \
  --data-dir /srv/tunnel-pg --old-server-stopped
./tunnel storage inspect --storage postgres --data-dir /srv/tunnel-pg
./tunnel server --storage postgres --data-dir /srv/tunnel-pg
```

`--old-server-stopped` 是操作者对外部停机/隔离的明确声明，命令不能自行证明远端进程已停。活跃连接仍持锁时恢复拒绝。不要定时自动清标记或直接改表；自动故障接管属于后续 HA-3。

## 开发验证

在可创建临时数据库的隔离测试集群设置 `TUNNEL_TEST_POSTGRES_DSN`（URL 格式）；凭据需要 CREATEDB，仅用于测试，不授予生产运行角色：

```bash
TUNNEL_REQUIRE_POSTGRES_TESTS=1 go test -race -count=1 ./internal/state ./cmd/tunnel -run Postgres
TUNNEL_REQUIRE_POSTGRES_TESTS=1 TUNNEL_TEST_STORAGE=postgres go test -race -count=1 ./internal/control
```

测试创建随机数据库并在清理时删除它们。缺测试 DSN 时普通本地回归跳过数据库测试；显式要求 PostgreSQL 时缺 DSN 会失败。工作流 postgres-storage 在 15/18 上强制执行，不以本地跳过当作数据库验收。存储格式和性能边界见 [设计](design/postgres-repository.md)。
