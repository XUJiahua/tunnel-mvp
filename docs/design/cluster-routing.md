# 跨控制面 Agent 路由（HA-3）

本实现引入显式 PostgreSQL 集群模式、节点 incarnation、递增 Agent generation 和独立 mTLS 流监听。HA-3 的测试及结果以 PR #30 的最终提交为准；HA-4 的跨节点审计/指标聚合与完整滚动恢复演练仍待实施。

## 运行模式与数据库

PostgreSQL schema v3 在启动事务中从 v1/v2 升级，授权模型及 portable backup 保持 v10。新增 `cluster_nodes` 和 `cluster_agents`；运行记录不进入 portable backup。每个节点保留私有运行目录、审计文件和 monitor.db。

单活模式持原固定连接的独占 advisory lock。显式集群模式在同一 key 持共享锁，并把持久 `runtime_owner` 设为 `cluster-v1`。普通存储客户端、单活 Server 和离线维护不能进入运行中的集群；集群也不能绕过异常单活标记。切换模式需要停掉全部节点并执行现有离线恢复声明，不提供在线清标记入口。

每个节点最多四个数据库连接，包括原固定锁连接。请求 SQL 取消不会释放生命周期锁。事务同时检查运行模式、固定连接 PID/锁、节点 ID/incarnation 及数据库租约；节点一旦过期即不能续租、提交或读出权威路由。配置/认证变更仍由既有数据库事务排序。

## 所有权与截止时间

节点 ID 是配置的稳定 DNS 风格标识，incarnation 是每次启动的随机值。节点记录同时绑定私有 HTTPS 地址、叶证书 SHA-256 和数据库时间。最多 128 个活节点，每个节点最多 4096 个 Agent claim。

数据库节点租约 10 秒，每秒心跳。每次成功注册/心跳仅给本地证明续到“查询开始时间 + min(5 秒, 数据库剩余租期)”。独立定时器取消节点和旧流，新请求还直接检查截止时间；排队定时器或迟到 SQL 回复不能复活过期节点。调度/网络恢复不会自动重新启用已被 fencing 的进程，应重新启动取得新 incarnation。

Agent claim 在数据库事务内校验最新凭据和站点/实例状态，以 `site~instance` 为键；兼容 Agent 使用 site ID。活节点已持有 claim 时其他节点拒绝重复连接。每次成功取得 claim 都递增 generation；释放比较 owner、incarnation 和 generation，旧释放不能移除接管者。claim 先保留为未就绪，实际 WebSocket 建立后才由心跳公开路由。

运行表不使用指向可替换配置实体的外键。路由查询连接当前站点/Agent 凭据及禁用状态；删除、轮换或禁用会立刻使旧 claim 不再具备路由权威，owner 心跳再关闭对应本地 Agent。节点失联时，其他节点在数据库租期过后才能接管；Agent 的重试间隔、LB 检测和调度另加到实际恢复时间，10 秒不是端到端 SLO。

## 私有传输与授权

独立监听只接受 TLS 1.3、HTTP/1.1、私有 CA 双向认证；不加载系统 CA。节点证书必须同时具有 serverAuth/clientAuth 用途及唯一 URI `spiffe://tunnel-mvp/node/<nodeID>`，广告地址还需通过正常 DNS/IP SAN 校验。实际连接再次比对数据库里的叶指纹和 incarnation，只有 CA 签名不足以成为活节点。

内部 CONNECT 路径 `/_tunnel/internal/v1/stream` 的有界信封绑定协议、数据库实例、源 incarnation、目的 owner/incarnation、Agent generation、应用、TCP 目标及授权索引。接收端独立查询当前成员/所有权/应用，重新证明会话、访问 grant、角色、身份新鲜度和目标，不接受公网头赋予内部权限。只拨本地精确 generation，拒绝第二次路由，不能构成任意目标代理。Agent 仍逐连接执行本地 ACL，HTTPS 上游证书仍由请求节点的应用 transport 校验。

用户连接池在集群模式始终按父会话和应用 grant 隔离，避免一个已验证内部连接被另一授权复用；保留原 LRU 和连接上限。成员探测携带自己的会话，后台健康探测仅允许受信活节点访问当前配置应用目标。内部流具有独立的来源节点证明与授权证明，两者都由每秒查询及独立 5 秒定时器维持；数据库失联、注销、资源/权限变更会结束旧流。

转发仅在建立 TCP、尚未发送应用请求时尝试另一个 Agent。已经发送的 POST/上传没有新增自动重放；已有 TCP/WS/SSE 不能迁移，客户端需建立新连接。

## 停机与剩余边界

正常停机先关闭接入和本地节点上下文，停止后台任务，取消流和 Agent，等内部转发退出后再使数据库节点租约到期，最后释放原连接锁。崩溃后租约自然到期；旧进程不能续回已过期 incarnation。

本阶段 UI 站点/Agent 在线归属读取共享路由；审计、历史、即时请求计数和部分 Agent 诊断仍在处理节点本地。HA-4 负责聚合、来源去重和版本/恢复演练。部署与证书生成见 [HA 操作手册](../ha-operations.md)。
