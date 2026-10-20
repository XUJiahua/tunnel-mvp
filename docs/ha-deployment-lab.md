# 容器部署与故障验收

本演练使用源码中的真实 Dockerfile：Next.js 构建并 embed 到 Go 二进制，两个独立 Server 及目录、PostgreSQL、HAProxy 3.2 TLS 入口和真实 Agent。应用只加入 Agent 所在网络，Server 无法直接到达应用。它补充 HA-3/4 的进程及浏览器验收，不代表跨宿主机、WAN、Named Tunnel 或数据库主备切换已经通过。

## 一键演练

在 Linux 源码检出目录准备 Docker Engine、Compose v2、OpenSSL、Python 3.11+ 和 Git。需要下载镜像和构建依赖，28443–28445 三个 loopback 端口必须空闲。正式发布压缩包不是完整源码，须检出对应提交的仓库运行此演练。

```bash
python3 scripts/verify-ha-deployment.py \
  --runtime /tmp/tunnel-ha-lab-new \
  --report /tmp/ha-deployment-report.json \
  --postgres 18
```

runtime 必须不存在。脚本生成独立项目名、新数据库卷、随机密码和两套不同的 CA；目录 0700、文件默认 0600。节点只挂载自己的证书、私钥和公共 CA；CA 私钥不挂载到容器。Agent 信任实验入口 CA，验收客户端校验 TLS/SNI/主机名，并验证不信任该 CA 的连接失败，不跳过证书校验。

脚本仅操作自己新建的 Compose 项目，不接收外部 DSN 或现有项目名。默认结束时清除本项目容器、网络和临时数据库卷；私有 runtime 保留节点数据、实验密钥及诊断，检查后可删除。`--keep` 保留运行现场，须手动停止。不要把 runtime 上传或提交到 Git；JSON 报告必须位于 runtime 之外。CI 只保存 JSON 结果及经随机密码/Agent token 脱敏的诊断日志。

隔离网络中数据库使用 `sslmode=disable`，只用于此单机实验；生产跨主机数据库应使用 `verify-full`。PG15/18 均设置实验专用 PGDATA 子目录，各版本使用独立新项目/卷，不能直接替换旧卷的数据库大版本。

## 网络与入口

| 入口 | 用途 |
| --- | --- |
| `127.0.0.1:28443` | TLS 公共入口；控制台轮询健康节点，应用优先 A、Agent 优先 B，以验证跨节点流 |
| `127.0.0.1:28444` | 实验用定向 A 管理入口，保持公共 Host/Origin/SNI |
| `127.0.0.1:28445` | 实验用定向 B 管理入口，保持公共 Host/Origin/SNI |
| `cluster` 内部网络 | PostgreSQL 与节点私有 mTLS，没有宿主机端口映射 |
| `site` 内部网络 | Agent 与应用，Server/HAProxy 不加入 |

验收客户端将实验域名解析到 loopback，无需修改 hosts；Agent 使用 Docker DNS。HAProxy 保留公共 Host，主动检查 `/healthz`，只把 200 视为就绪；关闭请求重试，不重复执行 POST。定向管理入口绕过健康检查以允许解除排空，必须保持 loopback；此配置不能直接用作公网生产配置。

## 自动验证内容

1. 安全控制台 cookie 跨节点有效；经过 Agent 的 HTTP、应用 WebSocket 和 TLS 校验通过。
2. 错投排空返回 409；A 排空后 healthz=503，真实 HAProxy 将新入口送往 B，已有 SSE 继续；新 POST 上游执行一次。
3. 报告确认 Agent owner 为 B 后 SIGKILL B 容器；Agent 重连到 A，应用恢复；重启 B 后观测恢复。
4. 暂停实验数据库，两个节点 fencing，新应用请求返回 503；恢复数据库并显式重启节点后 Agent/观测/应用恢复。报告记录实际拒绝访问耗时，不承诺生产 SLO。
5. 摘除并停止 B，同 CA 重签同 nodeID/DNS 的叶证书；重启 B 和 Agent，确认 B 再次持有 Agent，应用经 A→B 的新 mTLS 通道及 WebSocket 恢复。

这是有界功能与故障演练，不是压力测试。停止 owner/入口会中断已有连接，新请求恢复不代表长流无损迁移。数据库 pause 是无响应演练，没有备库或主备切换，也没有测试数据库数据丢失。新库配对恢复仍由 `web/tests/cluster.spec.ts` 的 PG15/18 验收证明。

## 仅生成配置

```bash
python3 scripts/verify-ha-deployment.py --prepare-only --runtime /tmp/tunnel-ha-lab-new
docker compose --env-file /tmp/tunnel-ha-lab-new/environment \
  --project-name tunnel-lab-manual -f examples/ha-lab/compose.yaml config --quiet
docker compose --env-file /tmp/tunnel-ha-lab-new/environment \
  --project-name tunnel-lab-manual -f examples/ha-lab/compose.yaml up --build -d
docker compose --env-file /tmp/tunnel-ha-lab-new/environment \
  --project-name tunnel-lab-manual -f examples/ha-lab/compose.yaml down --volumes
```

仅生成配置不注册或启动 Agent，不能记作应用验收通过。完整脚本注册 Agent 后启动 `agents` profile。不要把普通 `compose config` 或环境变量打印到共享日志，其中包含实验凭据。

CI 入口 `.github/workflows/deployment.yml` 分别使用新 PG15/18 环境。以对应 PR 最终提交的 workflow/JSON 报告为准，语法和配置检查不能代替 Docker 演练。镜像标签可能更新，报告记录实际镜像 ID；生产部署应按已验收 digest 固定并重新验证升级。

后续真实环境仍需独立主机、DNS/TLS 或 Named Tunnel、内网应用、数据库高可用拓扑和预期负载/恢复指标，采集网络分区、DB 切换、备份 RPO/RTO 及长时间运行证据。
