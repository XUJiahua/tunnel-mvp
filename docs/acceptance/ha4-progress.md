# HA-4 开发检查点（未完成验收）

基线：main `29fcd9399507da008385fcd3136279c2d781a168`，分支 `feat/cluster-observability`。本记录描述本地候选实现，不代表已有远端提交、合并或可发布版本。

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

## 阻塞及后续

自动审批两次拒绝将源码树上传到用户指定的公开 GitHub 仓库。已确认当前连接账号与仓库所有者一致且有写权限，审批仍要求用户明确批准这批未发布源码的公开上传。未通过其他渠道重试写入。

待发布草稿分支后实际运行：PG15/18 幂等/冲突/租户隔离/撤权测试、真实双节点来源聚合与定向排空、真实提交后丢失响应的队列重试、既有 HA-3 分区/进程故障回归。CI 已准备固定 HA-3 基线源码构建及真实旧 CLI 兼容测试。

后续仍需实施和验收：真实新旧集群节点并行运行、逐节点升级/回滚、配置加各节点运维归档恢复到全新数据库、浏览器集群视图、最终六平台发布包验证及目标环境验收。外部 LB、Named Tunnel、跨宿主机数据库主备切换未在本次本地检查中验证。
