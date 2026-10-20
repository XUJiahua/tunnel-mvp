# HA 配置与故障验收

HA-3 构建支持显式 PostgreSQL 多控制面及跨节点 Agent 流。使用本阶段最终 CI 通过的构建；v0.1.1 及 HA-1/2 构建不能按此配置启动。HA-4 的审计/指标聚合与滚动恢复验收尚未完成，不将本阶段当作完整生产 HA 验收。

## 双节点部署

准备独立 PostgreSQL 数据库、两个独立运行目录、共同公网控制台/应用入口及支持 WebSocket/SSE 的负载均衡。公网入口无需粘性会话；`/healthz` 失去节点/数据库权威后返回 503。节点之间只开放私有 mTLS 端口，不能把该端口转发到公网。JSON/SQLite 保持单进程。

为广告地址生成专用 CA 和不同节点证书，例如：

```bash
bash scripts/create-cluster-pki.sh ./cluster-pki \
  node-a=10.20.0.11 node-b=10.20.0.12
```

命令要求 OpenSSL、Python 3，拒绝已有输出目录。CA 私钥留在线下签发机；每台节点只部署 ca.crt 和自己的 node.crt/node.key，目录 0700、私钥 0600。叶证书有效 90 天，节点 URI 分别为 `spiffe://tunnel-mvp/node/node-a` / `node-b`，同时有 serverAuth/clientAuth 和广告地址 SAN。可接入自有 PKI，只要满足这些校验。

使用 [节点 A](../examples/cluster/server.node-a.json) 和 [节点 B](../examples/cluster/server.node-b.json) 配置，替换地址及证书路径。两个节点的 publicURL、accessURL、OIDC、身份桥接、协议与 CA 集必须一致；不同策略拒绝加入。通过私有环境向两节点提供同一 `TUNNEL_POSTGRES_DSN`，使用 verify-full 等生产数据库 TLS 设置，见 [PostgreSQL 手册](postgres-operations.md)。新空库的首次初始化还需 `TUNNEL_ADMIN_PASSWORD`。

```bash
./tunnel server --config examples/cluster/server.node-a.json --check-config
./tunnel server --config examples/cluster/server.node-a.json
# 另一主机使用 node-b 配置、独立 dataDir 和自己的证书。
```

也可使用 `--cluster-enabled`、`--cluster-node-id`、`--cluster-listen`、`--cluster-advertise-url`、`--cluster-ca-file`、`--cluster-cert-file`、`--cluster-key-file`；环境变量对应 `TUNNEL_CLUSTER_*`。`--check-config` 仅检查配置，不证明数据库连接、证书加载或节点互通成功。`node.id` 绑定配置的稳定 nodeID，保留在该节点私有目录，不能复制给其他节点。不要共享 dataDir 或 monitor.db，也不能删除目录锁绕过限制。

## 从单活迁入及离线维护

1. 备份授权及原节点运维归档，停止单活 Server 并禁用自动拉起，确认旧进程不会继续服务。
2. 正常停机会释放原运行标记；异常退出时按 PostgreSQL 手册执行 `storage recover-postgres-runtime --old-server-stopped`。
3. 用同一数据库和独立新目录启动 cluster node-a，再启动 node-b。新目录与数据库绑定；如原目录已有随机 consumer node.id，使用新目录。首次切入集群协议会使不匹配的共享认证策略失效，重新登录。
4. 让 Agent 通过共同公网入口连接，在另一节点进入应用验证跨节点流。切换期间旧连接会结束。

集群正常全停后仍保留 `cluster-v1` 模式标记，防止旧单活构建意外进入。切回单活、离线改密、portable 导出/其他维护前，必须停止并隔离**全部**节点，再明确执行：

```bash
./tunnel storage recover-postgres-runtime --storage postgres \
  --data-dir /var/lib/tunnel/node-a --old-server-stopped
```

任何活节点持有共享锁时此操作拒绝。该声明不能代替操作者确认远端停机。随后按 [存储手册](storage-operations.md) 和 PostgreSQL 手册导出、校验及恢复；授权 portable backup 不带会话、票据、节点或 Agent 运行 claim，恢复到新库后重新登录并让 Agent 重连。每个节点的审计/监控分别归档，尚无自动聚合恢复。

## 故障与证书更新

节点每秒续租；数据库租期 10 秒，本地准入及现有流证明最长 5 秒，自查询开始计时。失去有效证明的节点保持关闭状态，数据库恢复后应重启。接管还需 Agent 重连及 LB 检测，不能承诺固定的用户恢复秒数。现有连接不迁移，不重放已发送 POST。

同一 CA 下更新节点叶证书需先摘除并停止该节点，再部署符合原 nodeID 的新证书和启动；新 incarnation/指纹会写入数据库。其他节点继续服务。当前不支持在线重载证书；更换 CA 集、协议或公共认证策略需要全停和离线模式恢复，混用策略会拒绝加入。版本兼容窗口及完整滚动恢复属于 HA-4。

## 验收与证据边界

隔离 CI PostgreSQL 15/18 强制运行 `TestPostgresCluster*`：数据库代际/旧释放保护、schema v2 升级、模式排斥；两个独立 Server、私有 TLS 监听、HTTP LB 和真实 Agent 的跨节点 HTTP/上传/WS/SSE、跨节点注销、心跳中断后的本地 fencing 和接管、POST 单次上游执行、伪造 generation/目标/成员 incarnation/数据库/授权及公网注入拒绝。没有测试 DSN 时本地跳过数据库测试，不能当作数据库通过；最终结果见 PR #30。

仍需目标部署验收：独立宿主机 kill -9/网络分区与数据库主备切换、生产负载及恢复 SLO、外部 LB/TLS 超时、证书轮换和完整恢复演练。macOS/Windows 实际服务管理器、AD/NTLM、额外 IdP 租户分别记录；交叉编译不能替代这些测试。HA-4 继续补跨节点监控/审计来源、聚合与去重。
