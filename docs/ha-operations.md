# HA 配置与故障验收

HA-3 构建支持显式 PostgreSQL 多控制面及跨节点 Agent 流。使用本阶段最终 CI 通过的构建；v0.1.1 及 HA-1/2 构建不能按此配置启动。HA-4 增加审计/指标来源汇聚、连接排空与滚动恢复自动化演练；使用 [PR #31](https://github.com/XUJiahua/tunnel-mvp/pull/31) 最终检查通过的构建。目标部署的外部 LB、数据库切换及 SLO 仍需单独验收。

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

同一 CA 下更新节点叶证书需先摘除并停止该节点，再部署符合原 nodeID 的新证书和启动；新 incarnation/指纹会写入数据库。其他节点继续服务。当前不支持在线重载证书；更换 CA 集、协议或公共认证策略需要全停和离线模式恢复，混用策略会拒绝加入。HA-4 的混合版本窗口保持基础 schema v3 和 cluster-v1；CA 集和认证策略必须一致。

## 验收与证据边界

隔离 CI PostgreSQL 15/18 强制运行 `TestPostgresCluster*`：数据库代际/旧释放保护、schema v2 升级、模式排斥；两个独立 Server、私有 TLS 监听、HTTP LB 和真实 Agent 的跨节点 HTTP/上传/WS/SSE、跨节点注销、心跳中断后的本地 fencing 和接管、直接杀掉独立 owner OS 进程后的真实 Agent 重连、POST 单次上游执行、伪造 generation/目标/成员 incarnation/数据库/授权及公网注入拒绝。没有测试 DSN 时本地跳过数据库测试，不能当作数据库通过；最终结果见 PR #30。

仍需目标部署验收：跨宿主机 kill -9/网络分区与数据库主备切换、生产负载及恢复 SLO、外部 LB/TLS 超时、证书轮换和完整恢复演练。macOS/Windows 实际服务管理器、AD/NTLM、额外 IdP 租户分别记录；交叉编译不能替代这些测试。HA-4 的跨节点观测与恢复边界见下节。

## HA-4：共享观测与节点排空

此节对应 PR #31；是否可交付须核对最终提交的 CI，而不能仅凭操作说明认定通过。[观测设计](design/cluster-observations.md)保留基础 schema v3 / 内部流 v1，独立增加 observation extension v1；旧节点在覆盖视图中显示未上报。

新配置 `cluster.observationBufferMB`（CLI `--cluster-observation-buffer-mb`，环境变量 `TUNNEL_CLUSTER_OBSERVATION_BUFFER_MB`）默认 256 MiB，允许 16–2048。每个节点的本地运维数据绑定 nodeID；不能复制同一份 monitor.db 给另一 nodeID。新数据库恢复需同时恢复配置和各节点运维归档，保持原 nodeID；会话、运行租约和 PKI 另按 HA-3 流程重建。运维归档中保留队列，启动重放以稳定事件 ID 去重。首次升级/重放超过容量会拒绝启动，应在摘除节点的维护窗口调整容量。

控制台“监控历史”对全局管理员显示节点状态；审计可按来源节点过滤，健康表展示来源。`GET /api/cluster/status` 提供当前响应 nodeID、各节点 incarnation、数据库存活、报告新鲜度、排空状态、版本、活动入口/中继及队列统计。超过 10 秒未上报或旧节点没有报告时，覆盖不完整。`/metrics` 的 `tunnel_cluster_node_*` 使用 node_id 与 node_epoch 标签；请求计数按入口统计，不能把入口与 owner 中继重复相加。每个端点返回相同的共享节点序列；抓取多个端点时，须按 node_id/node_epoch 对副本取最大值去重，不能直接跨抓取目标求和。共享查询失败返回 503，不回退到本地结果。

排空步骤：先把目标节点从 LB 新请求中摘除；保留其管理入口，使用有效全局管理员会话及公共 Origin，向该节点的实际监听器发送 `POST /api/cluster/local/drain`，JSON 为 `{"nodeID":"node-a","draining":true}`。请求必须保持公共控制台 Host。误投其他节点返回 409。数据库同事务验证管理员并设置路由不可选；心跳不能重新发布已排空路由。healthz 为 503，管理 API 可继续查询或以 `draining:false` 解除排空。

排空保留已建立的通道；已有 HTTP 持久连接仍可能承载后续请求，WS/SSE 不迁移。活动入口与中继、Agent 通道应降到业务认可的水平后再正常停止。长流或旧版本连接池可能不能在维护窗口自行归零，必须事先定义等待上限及可接受中断；不能把排空 API 成功等同于无损停机。

自动化验收入口：PG15/18 双节点共享查询与撤权、模糊提交重试及排空由 `TestPostgresCluster*` 验证；真实新旧节点混跑、逐节点升级/回滚、全新数据库配对恢复与浏览器来源视图由 `web/tests/cluster.spec.ts` 验证。文件归档兼容检查只证明归档可读，不能替代运行演练。最终提交的 CI 与实际发布包仍是交付门槛。


## 兼容升级与全新数据库恢复顺序

兼容窗口只包含已验证的 HA-3 基线与本阶段构建；基础 schema v3 / cluster-v1、公共入口、CA 和认证策略必须一致。旧节点不产生共享观测，覆盖视图应显示未上报。每次仅维护一个节点：摘除其新入口流量；新构建可使用定向排空 API，旧 HA-3 节点需由 LB 摘流；检查活动通道及预定等待上限；正常停止、更换二进制并以原 nodeID/独立目录启动；等待租约、Agent 和观测恢复再处理下一节点。回滚保留原 monitor.db，旧构建忽略附加表；再次升级补录旧格式日志。已有长流在停止入口/owner 时会中断，需业务客户端重连，不能保证流无损迁移。

恢复到新数据库需先停止所有 Server 和 Agent，确保源数据库持有者全部退出。用新 CLI 对源数据库执行 `storage recover-postgres-runtime --storage postgres --data-dir NODE_A_DIR --old-server-stopped` 后导出 portable 配置；分别对每个节点 `export-operations`。该确认仅适用于已真实停止或外部隔离的节点，不能用来抢占在线数据库。

创建独立空 PostgreSQL 数据库，为各节点准备全新目录。先以 `storage restore --storage postgres --data-dir NEW_NODE_A_DIR --input CONFIG_BACKUP` 恢复配置，再以 `restore-operations --data-dir NEW_NODE_DIR --input ITS_OPERATIONS_ARCHIVE` 恢复各自运维归档。保留原 nodeID 和正确 PKI，使用新 DSN 启动节点与 Agent；不要复制旧 `state.postgres` 绑定或把同一份运维归档用于另一 nodeID。会话/访问票据不会恢复，用户必须重新登录。启动将保留记录重新排队；检查所有节点报告新鲜、pending 清零、历史与审计来源、实际应用访问及再次重启的去重结果。此流程以独立新数据库配对恢复为目标，不用于把旧运维快照覆盖到正在使用的新数据库。
