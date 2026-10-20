# Cloudflare 临时公网模拟

使用 cloudflared Quick Tunnel，为控制台与两个应用分别分配临时 HTTPS 域名；控制面使用现有自定义应用域名路由，不修改 Host、鉴权或 TLS 校验。四个独立 Agent 进程通过控制台公网 WSS 接入，分别模拟两个站点，每站点两个实例。

## 本地复现

在允许 DNS、Cloudflare API HTTPS 和 Cloudflare Tunnel HTTP/2 出站的测试主机，安装官方 cloudflared，先构建源码并安装 Chromium：

```bash
make build
pnpm --dir web exec playwright install --with-deps chromium
node scripts/verify-cloudflare.mjs \
  --cloudflared /absolute/path/to/cloudflared \
  --binary /absolute/path/to/tunnel \
  --output /absolute/path/to/new-cloudflare-report.json
```

脚本不读取真实 Server/Agent 配置；只暴露随机生成密码的临时控制面及合成应用。原站点、用户数据和凭据不参与。Cloudflare 入口在脚本结束时关闭，账号/令牌/进程日志随临时目录删除。报告保留入口域名、阶段结果和代码 SHA，不包含密码、Cookie、Agent Token 或一次性票据。输出文件必须不存在。

脚本使用三个独立 Quick Tunnel：控制台域名作为 publicURL，两个应用域名分别绑定 customDomain；accessURL 的保留 `.invalid` 域只作为未使用的默认命名空间。无需自有 DNS 或 Cloudflare 账号，不把一个随机域名误当作可控通配域。

## 检查范围

| 检查 | 路径 |
| --- | --- |
| 控制台与前端 | 真实公网 HTTPS 加载嵌入页面/脚本并完成浏览器登录 |
| Agent | 两站点四实例通过真实公网 WSS 上线 |
| 应用 | 公网独立域名兑换票据、上游登录及重定向，host-only/Secure/HttpOnly Cookie 隔离 |
| 上传 | 两个应用各上传 1 MiB，经 Cloudflare 与 Agent 返回计数 |
| 授权 | 只授权站点 A 的成员不能看见或 launch B，无应用 Cookie 请求被拒绝 |
| 故障 | 停一个 Agent 后余下实例承接访问，原实例通过公网 WSS 重连 |
| 长连接撤销 | 公网 WSS 双向 echo，控制台退出后关闭连接并拒绝旧访问，其他成员会话保留 |

Quick Tunnel 官方明确不支持 SSE，本脚本不执行或宣称通过 SSE。需要命名 Tunnel 和正式域名后另验；参考 [Cloudflare 文档](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/)。

这是单机多进程、真实公网边缘往返模拟，不是两处物理内网，不证明控制面 HA、跨机房故障、真实业务产品/IdP 或生产容量。此前本地 loopback 与本次公网结果分别记录，不互相替代。

## 当前环境与 CI

2026-10-20 的工作环境已安装 cloudflared 2026.10.1，官方 Linux amd64 SHA-256 为 `3bfc539f45d2620c71debb7a86be0efa737ed5ccbad9eb85c787116900e5c586`。申请入口时 DNS 连接返回 `network is unreachable`，故当前环境没有生成可访问的公网入口；普通 curl HTTPS 可用不代表原生隧道所需网络可用。

`.github/workflows/cloudflare-simulation.yml` 在独立 GitHub Actions runner 执行同样的原生进程模拟，固定并校验上述官方二进制，只有 contents:read 权限，不接入 Cloudflare 账号或生产 Secret。任务上传无秘密报告，执行结果以对应 Actions run 为准；失败不能被标为网络已打通。它不依赖开发环境的浏览器或网络限制。

## 公网实测记录

2026-10-20 的 [专项 CI 39358115397](https://github.com/XUJiahua/tunnel-mvp/actions/runs/39358115397) 已成功完成七个阶段。PR head 为 `7f7f1cded7b863ed32ff1d382b4ca7023a66a420`；Actions 实际检出的测试合并提交为 `5c687a7c25f969a8763824cac46a65ebc5b58e20`，原始报告记录该执行修订，不能混写为 head。

控制台浏览器 HTTPS 登录、四 Agent 公网 WSS、两个独立域名的应用登录/重定向/Cookie/上传、站点授权拒绝、单 Agent 故障与重连、公网双向 WebSocket 和退出撤销全部通过。报告的 cleanedUp=true，临时公网入口已经关闭，不能用其中的域名作为长期部署地址。

[原始无秘密报告](acceptance/2026-10-20-cloudflare.json) 从 Actions artifact 取回并验证 ZIP SHA-256/CRC；保留实际修订、临时 URL、阶段结果及清理状态。本次是公网链路补验，当前本地执行环境仍未通过原生出站条件。
