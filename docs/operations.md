# tunnel 操作手册

本手册随代码更新。状态后端选择、JSON → SQLite 迁移及可移植备份见 [存储操作手册](storage-operations.md)。快速部署仍见 [README](../README.md)，设计语义见 [管理与运行设计](design/operations-and-access.md)，后续功能见 [路线图](roadmap.md)。

## 1. 从 v0.1.1 升级

1. 安排维护窗口，告知用户隧道和应用长连接会中断。
2. 停止 Server，备份整个 `dataDir`（默认 `data/`），包含选用的 `state.json` / `state.db`、`audit.jsonl` 及轮转文件。配置文件另行备份；环境变量中的凭据继续由原密钥管理方式保管。
3. 获取包含本次变更的提交并运行 `make build`，替换二进制。未打版本标签时，GitHub Releases 中旧产物不含新功能。
4. 运行 `./tunnel server -c server.json --check-config`，确认域名、数据目录和探测间隔，启动 Server。运行时无需 Node.js。
5. 逐站点验证在线状态、应用手动探测、登录和 WebSocket/SSE。
6. 为需要自动加载 ACL 的 Agent 更新二进制并加上 `--watch-config`。未更新的 Agent 仍能使用原有隧道协议。

旧 `state.json` 会直接读取；新增 `serviceIDs` 缺省为空，不会改变整站授权。Server 重启后所有人重新登录。回滚时停止 Server，恢复升级前数据备份和二进制；不要用旧版本覆盖写入已包含新授权的数据文件。配置新增字段应与旧二进制同时回滚，旧版本严格拒绝未知字段。

当前仍为**单进程、单数据目录**。新版本用文件锁阻止两个 Server 或离线维护命令同时打开同一目录；旧版程序不认识此锁。不要对运行中的状态文件手工修改。尚无忘记管理员密码的离线恢复命令；请妥善保存管理员凭据及备份。

## 2. 新建站点与应用

1. 管理员在概览点击“接入新站点”，保存站点 ID 和一次性展示的令牌。
2. 在可访问内网目标的机器准备 Agent 配置，设置准确的 CIDR、端口和可选主机名。
3. 注入 `TUNNEL_AGENT_TOKEN` 后启动 Agent；非回环控制面必须使用 HTTPS。
4. 创建应用，填写 HTTP/HTTPS 源地址，例如 `http://grafana.internal:3000`。
5. 检测连接，通过后打开应用；应用自身仍使用自己的账号登录。

Agent 在容器里时，`127.0.0.1` 是容器本身。内网 HTTPS 的 CA 必须被 Server 信任，Agent 连接控制面的 CA 必须被 Agent 信任。

## 3. 编辑站点与应用

- 站点卡片点击“编辑”：修改名称，ID、令牌和现有连接保持不变。
- 应用行点击“编辑”：修改名称或内网地址；所属站点不能修改。
- 只改名称不会撤销访问。修改地址会关闭该应用当前连接，撤销已签发票据和访问会话；用户从控制台重新打开即可。
- 更换地址后仍须匹配 Agent 白名单；不匹配时先调整 Agent 配置。
- 跨站点迁移或替换为另一套完全无关的应用，建议新建应用，重新授权后删除旧应用，避免复用浏览器里的第三方 Cookie。

## 4. 授权与撤销

“访问权限”中创建成员或点击“编辑权限”，可选：

| 操作 | 效果 |
| --- | --- |
| 勾选整站授权 | 访问该站点全部应用，包括以后新增的应用 |
| 只勾选单独应用 | 只能访问指定应用 |
| 同时勾选 | 两种权限的并集 |
| 清空全部选项并保存 | 保留账号和控制台登录，收回全部应用访问 |

例如只让成员访问 Grafana：取消 Grafana 所在站点的整站授权，再勾选 Grafana 应用。仅取消应用选项、却保留整站授权，不会收回权限。

保存后，失去授权的 WebSocket、SSE 和代理请求会被关闭，其他获授权应用不受影响。成员下次刷新控制台或最多 10 秒后看到更新后的资源列表。用户无法直接输入任意内网地址，应用仍由管理员登记。

删除成员会使该成员全部控制台和应用会话失效，并关闭现有连接。轮换站点令牌会断开整个站点，请同步更新 Agent 的 `TUNNEL_AGENT_TOKEN` 并重启。

## 5. 密码维护

- 所有人：右上角“修改密码”，输入当前密码及两次新密码。
- 管理员：在成员行点击“重置密码”，设置新密码并通过安全渠道交付成员。
- 密码限制为 12–72 字节；中文字符占多个 UTF-8 字节。
- 修改或重置成功后，该账号的全部控制台会话和应用长连接失效，需要重新登录。
- 管理员不能通过成员重置接口修改管理员密码；管理员使用自助改密入口。
- `TUNNEL_ADMIN_PASSWORD` 仍仅用于首次初始化，修改它不会重置已有账号。

## 6. Agent ACL 自动重载

```bash
./tunnel agent -c agent.local.json --watch-config
```

建议流程：

1. 编辑新的 JSON/YAML 文件，运行 `./tunnel agent -c agent.next.json --check-config`。
2. 将验证后的文件替换为 Agent 当前读取的配置文件，推荐同目录原子替换。
3. 最多约 2 秒后，日志出现 `agent ACL reloaded; replacing tunnel`；Agent 断开并重连，现有流会中断。
4. 在控制台重新探测目标，核对允许和拒绝结果。

无效文件保持旧配置，日志出现 `agent config reload rejected`。不要据此误认为新的收紧策略已经生效。`siteID/serverURL` 变更会拒绝自动加载，需重启进程；令牌和日志配置也需重启。

CLI `--allow` 和 `TUNNEL_ALLOW` 的优先级高于文件。使用它们时，修改文件里的 `allow` 不会覆盖这些值。自动重载只加载本地策略，控制面不下发新白名单。

## 7. 自动探测和诊断

Server 配置示例：

```json
{
  "listen": "127.0.0.1:8080",
  "publicURL": "https://console.example.com",
  "accessURL": "https://access.example.net",
  "dataDir": "data",
  "probeIntervalSeconds": 60,
  "auditMaxMB": 20,
  "auditBackups": 5
}
```

`probeIntervalSeconds` 可用 `--probe-interval` 或 `TUNNEL_PROBE_INTERVAL_SECONDS` 覆盖，0 禁用，否则为 10–86400。启动后第一批在一个间隔后执行，手动“检测连接”可立即得到结果。最多 4 个探测同时运行，大批慢目标可能使实际刷新周期变长。

控制台会加载最近探测结果，悬停结果可查看时间。401/403/405/5xx 表示 HTTP **可达**，但不是 2xx/3xx 的状态码健康结果。HTTP 探测不验证业务流程；可达与应用正常工作应分别判断。

失败时打开“诊断详情”，查看阶段、IP、ACL 和请求 ID。也可在内网机器独立执行：

```bash
./tunnel agent diagnose -c agent.local.json --target http://127.0.0.1:3000
```

本机诊断成功但 Server 代理失败时，检查隧道、Server 的 CA 信任和应用公网 Base URL。日志使用 `--log-level debug --log-format json`，按请求 ID 关联。

## 8. Prometheus 与告警

1. 生成一个至少 32 字节的随机监控令牌，通过 `TUNNEL_METRICS_TOKEN` 注入 Server 环境；不要写到命令行、仓库或镜像。
2. 在 Prometheus 使用相同令牌的只读凭据文件，配置见 [prometheus.yml](../examples/monitoring/prometheus.yml)。
3. 安装 [告警规则](../examples/monitoring/tunnel-alerts.yml)，在现有 Alertmanager 配置接收人和渠道。
4. 确认 `up{job="tunnel"}=1`，手动停一个测试 Agent，验证离线告警，再恢复。

令牌未设置时 `/metrics` 返回 404，错误令牌返回 401。路径属于控制台域名；访问应用子域名不会获得此端点。管理员也可在已登录会话访问 `/api/metrics` 临时检查。不要混用 Agent Token。

| 指标 | 用途 |
| --- | --- |
| `tunnel_site_online` | Agent 控制隧道在线，0/1 |
| `tunnel_service_reachable` | 最近一次 HEAD 是否收到 HTTP 响应 |
| `tunnel_service_healthy` | 最近一次 HEAD 是否返回 200–399 |
| `tunnel_service_probe_timestamp_seconds` | 探测新鲜度；没有快照时不输出应用样本 |
| `tunnel_service_probe_duration_seconds` | 最近一次探测耗时 |
| `tunnel_proxy_requests_total` | 已鉴权代理请求累计值 |
| `tunnel_proxy_errors_total` | 上游传输错误累计值，不含正常收到的 HTTP 5xx |
| `tunnel_active_requests` | 活跃代理请求和手动探测 |
| `tunnel_probes_total` | 已完成手动/自动探测累计值 |

告警样例关注离线、不可达、探测过期和代理错误比率。401 为正常认证行为的应用，不宜直接针对 `healthy=0` 报警。关闭自动探测时应同时调整过期告警。程序只提供指标和状态变化记录，不直接发送邮件或消息。

## 9. 审计查询、轮转和备份

管理员打开“审计日志”，可按事件类型、站点 ID 精确过滤。常用事件：`login`、`logout`、`password_changed`、`password_reset`、`user_grants_updated`、`site_updated`、`service_updated`、`health_changed`、`proxy_error`。

界面显示当前文件尾部 2 MiB 内最近 100 条匹配记录，不是完整历史检索。API 的 `limit` 最大 500。完整历史请采集 `audit.jsonl` 及 `.1` 至 `.N`。

默认活跃文件约 20 MiB、5 个备份，可通过 `auditMaxMB` / `auditBackups`（或 `--audit-max-mb` / `--audit-backups`、对应环境变量）调整。新文件权限 0600。轮转错误会输出 `audit rotation failed`，检查目录权限和磁盘空间。

备份前停止 Server，以取得一致状态。恢复时保持目录权限，恢复配置/状态后启动，按站点检查重新连接和用户授权。会话和监控快照不在备份中，重启后重建。

## 10. 性能与运行边界

连接池按应用隔离，默认每目标 64 个并发连接、16 个空闲连接，空闲 30 秒回收。慢响应和长连接会占用资源，达到限制后排队。目标修改、Agent 断开和令牌轮换会清理相关池。

仓库验证了真实隧道上的连接复用、并发隔离和撤销行为，尚未给出生产并发容量承诺。上线前应使用自身应用、大文件上传、长连接及峰值流量压测，并监测文件描述符、CPU、内存、代理错误率和探测延迟。

本阶段已提供单机 SQLite，仍没有控制面 HA、多租户、SSO/MFA、同站点多 Agent、远程配置下发、自动升级和非 Web 协议网关。详细进度及依赖见路线图。
