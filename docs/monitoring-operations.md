# 业务健康、历史图表与运维归档手册

## 配置业务健康

1. 编辑应用，在“业务健康检查”填写相对路径，例如应用目标 `/business/` 下的 `ready`，实际检查 `/business/ready`。留空检查入口。
2. 默认 HEAD；需要响应状态的业务端点可选 GET。默认预期状态 0 接受 200–399，或填写一个 200–599 状态，例如 204。
3. 进入“监控历史”，选择应用并“检测并刷新”。验证健康/HTTP 可达不同含义，并检查期望状态；检查不跟随登录重定向或携带用户 Cookie。
4. 自动采集仍由 `--probe-interval` 配置，0 关闭，其他取 10–86400 秒。健康配置变化不关闭正常应用连接。

历史显示当前检查配置的曲线；更改目标、站点或检查策略后新配置重新采样。重启保留历史/最新状态，设备会话仍需重新登录。每应用最多 1440 条，全局 100000 条，写入时清理七天以前的数据；高频检查会更早达到条数上限。健康比例是检查次数比例。

检查 `tunnel_monitor_storage_errors_total` 和 Server 日志中的历史写失败；当前诊断成功不代表历史持久化成功。现有 `tunnel_service_healthy`、`tunnel_service_reachable`、探测时间和延迟指标继续提供。

## 配对停机备份

先停止 Server 和需要同时停机的 Agent。以下例子使用相同 Server 配置的 dataDir；文件名必须是新文件：

```bash
./tunnel storage export -c server.json --output state-backup.json
./tunnel storage export-operations -c server.json --output operations.tar.gz
./tunnel storage validate --input state-backup.json
./tunnel storage validate-operations --input operations.tar.gz
```

运行中的 Server 占有目录锁，归档命令会拒绝。普通 state 备份不包含审计/监控，运维归档不包含账号/授权状态或配置/环境密钥；两份一起保管，配置和密钥继续使用已有备份方式。运维归档包含完整保留的审计文件和监控数据库，权限为 0600，最大解压数据 2 GiB。

若发现 monitor.db 的 journal/WAL/SHM 文件，不手工删除；先用当前二进制正常恢复并停机，再重新归档。监控库损坏时先保留整个停机目录，排查或隔离损坏库后重新采样；不要删除授权状态文件来修复监控。

## 恢复并验证

将恢复配置的 dataDir 指向新的目录，storage 可以选择另一后端：

```bash
./tunnel storage restore -c restored.json --input state-backup.json
./tunnel storage restore-operations -c restored.json --input operations.tar.gz
./tunnel server -c restored.json
```

恢复不会覆盖已有运维文件。若中断，保持 Server 停止，用同一个 operations.tar.gz 重跑 restore-operations；标记 `operations.restore.pending` 会阻止 Server 启动，已完成文件须匹配 checksum 才能续传。不要删除标记后启动或换另一个归档覆盖，无法续传时保留现场并换新目录重新恢复。

启动后重新登录，检查历史曲线、审计记录和新采样，再逐站点验证 Agent 与实际应用。归档不恢复旧登录设备、网关 Cookie 或隧道连接。JSON/SQLite 两种授权后端使用同一个运维归档格式。

## 升级与边界

本层授权 SQLite/backup v6，兼容 v1–v5 事务升级。新增 monitor.db 在所有授权后端都使用纯 Go SQLite。组织/应用授权同样限制历史读取，组织操作者不能查询别的组织图表。

第十批在线 UI 已索引全部保留轮转文件，详见 [审计查询手册](audit-operations.md)。长期保留需要在轮转删除前归档或使用日志采集系统。
