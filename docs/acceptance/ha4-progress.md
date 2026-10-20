# HA-4 本地证据与远端验收入口

基线：main `29fcd9399507da008385fcd3136279c2d781a168`，分支 `feat/cluster-observability`。代码提交于 [PR #31](https://github.com/XUJiahua/tunnel-mvp/pull/31)。本记录保留本地证据；远端测试状态及最终提交 SHA 以 PR 检查为准，不将开发二进制等同于正式发布版本。

## 已实现

- PostgreSQL observation extension v1：共享审计、健康样本、来源绑定、内容冲突回滚、保留清理和游标失效；基础 schema v3 / cluster-v1 不变。
- 本地 monitor extension v1：持久 outbox、稳定事件身份、旧日志完整快照、队列确认、按页修复及容量错误计数；运维归档仍使用原格式。
- 同事务重新校验查询者会话、身份、角色、组织范围和当前健康 profile；共享查询错误不回退本地数据。
- 节点状态/来源指标、控制台覆盖视图、审计来源筛选、健康来源展示；定向排空与解除排空，心跳不能恢复已撤销的 ready。
- 运维归档校验附加版本、队列事件身份和字节计数，恢复保留未确认事实。

## 本地已执行

| 检查 | 结果及边界 |
|---|---|
| `go test -race ./internal/control`（JSON） | 两次完整回归通过，294.315s / 297.622s；后续索引/查询校正另由定向及 SQLite 回归验证。PG 测试均因无 DSN 跳过 |
| `go test -race ./internal/state ./internal/config ./cmd/tunnel` | 通过；包含新配置约束，PostgreSQL 测试跳过 |
| `go test -race ./internal/control -run '^TestObservationJournal'` | 通过：真实 SQLite 触发器故障、索引/队列原子回滚、JSONL 修复、捕获序号确认、重启身份、相同旧日志行不合并、缓冲上限与分页修复、归档恢复 |
| 真实 HA-3 二进制 `280e813` | 运维归档导出/校验通过；实际旧 Server 读取含新表的 monitor.db、登录并写入旧格式审计后，新代码再次升级并补录通过。此项使用本地 JSON Server，未证明集群混跑 |
| 前端 `pnpm typecheck` / `pnpm build` / `pnpm test:unit` | 通过；真实浏览器未执行 |
| `go vet`（control/state/config/CLI） | 通过 |
| 真实 v0.1.1 → 当前本地开发二进制 | 官方旧 Server/Agent 实测升级、旧 Agent 重连、JSON/SQLite 与跨后端恢复、凭据和权限保留、会话失效、离线锁及回滚通过；该开发二进制不是最终发布包 |
| SQLite 完整竞态回归 | 通过，301.649s；包含真实 HA-3 旧 Server 归档/回滚文件兼容测试。PG 测试跳过 |

旧 HA-3 二进制 SHA-256：`818d7098f3995a310fcd7b8f9ace0da124e1353d1a2091393a3007c86d2c40ef`。旧 v0.1.1 二进制 SHA-256：`c5bdb0e1bf9e0b791640f568a701b1d0bd34901dc0ae0b515a9824aa0896416d`。

## 远端自动化验收

公开上传的审批阻塞已在用户继续授权后解除，代码已提交 PR #31。工作流固定构建 main `29fcd9399507da008385fcd3136279c2d781a168` 的旧 HA-3 CLI，避免用新代码伪造旧版本。

- PG15/18：事件重放与冲突原子性、组织隔离与撤权、当前健康 profile、旧日志快照屏障、节点覆盖与心跳排空；真实提交后丢失响应、本地队列保留及幂等重试。
- 双 Server/LB/Agent：跨节点状态/历史/审计、来源过滤、定向排空、既有 WS/SSE 保留、分区截止时间、kill -9 接管及 POST 不重放。
- `web/tests/cluster.spec.ts`：真实旧/新 CLI 混跑、真实 Agent 与浏览器、排空时已有 SSE 继续、第二 Agent 接入、逐节点升级与回滚、控制台来源/覆盖视图、配置加每节点运维归档恢复到独立新数据库、旧会话失效、凭据与 Agent 重连、同一事件/样本只出现一次、再次重启不重复。
- 既有 JSON/SQLite/PG 浏览器及真实 Keycloak 回归；最终快照包与实际提取二进制升级验证在交付时记录。

上述入口已经实现，执行结论须查看 PR #31 对应提交的检查结果，不能仅凭测试定义认定通过。外部 LB、Named Tunnel、跨宿主机数据库主备切换、macOS/Windows 服务管理器及生产恢复 SLO 不包含在本次 Linux 容器演练内。
