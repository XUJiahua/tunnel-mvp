# 上游连接模式与容量操作

在应用列表选择“编辑”，把“上游连接模式”改为“每个登录会话独立串行连接”，保存后从控制台重新打开应用。模式变更会关闭旧连接并撤销应用访问票据。默认“按应用共享连接池”保留现有行为。

使用隔离模式前，确认上游确实依赖连接缓存身份；普通 Cookie 登录不需要它。一个控制面登录会话的同一应用一次只使用一个 TCP 通道，长下载/WS/SSE 会阻塞该会话的其他请求。连接丢失/空闲 30 秒后应用需能重新认证。同一账号在不同设备或重新登录使用不同池。

健康检查独立于用户池，不带应用 Cookie。Prometheus `tunnel_http_pools{mode="shared|session|probe"}` 展示三类池数量。隔离池全局最多 1024 条，超出采用 LRU 清理空闲连接；登出/撤销立即清理，过期会话后台每分钟清理。池数量不是并发浏览器人数，活动请求仍看 `tunnel_active_requests`。

API 创建/批量创建可指定 `"connectionMode":"session"`；PATCH 应同时提交 name/url 和 connectionMode。`""` 表示共享，省略字段保留现状。状态/备份 v9 包含该字段；升级前做好授权状态和运维归档备份，旧版本不能打开新 SQLite schema。

## 重跑容量验收

先完成 `make build` 和 `cd web && pnpm exec playwright install chromium`。选择空闲测试主机，避免同时运行编译、race 或其他压测；脚本使用 31880/31882 端口并启动独立 Server/两个 Agent/浏览器进程，不连接生产服务。

```sh
GO_BINARY=/absolute/path/to/go node scripts/capacity.mjs \
  --storage json --output /absolute/new-json-report.json
GO_BINARY=/absolute/path/to/go node scripts/capacity.mjs \
  --storage sqlite --output /absolute/new-sqlite-report.json
```

如果使用已有 Chromium，设置 `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH`；字体路径按环境设置。脚本拒绝覆盖输出。报告包括环境/源提交/工作区修改状态、HTTP p50/p95/p99、吞吐、上传、16 WS + 16 SSE、单 Agent 生存、优雅停机/重连，以及 Linux `/proc` 每 100ms 的各进程 RSS/FD 峰值；输出不包含凭据。其他平台缺少 `/proc` 时需相应原生采样实现。

具体原始数据与解释见 [容量报告](capacity-report.md)。本地回环合成结果用于回归基线，不能直接据此配置生产人数或带宽。
