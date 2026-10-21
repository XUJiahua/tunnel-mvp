# 开发环境准备

开发管理器面向 Linux，使用 Python 标准库、`fcntl.flock`、`/proc` 进程启动身份和用户 systemd。
跨平台二进制仍由既有 CI/GoReleaser 构建；macOS/Windows 的 Agent 服务和实机验收另行执行。

## 工具与首次安装

- Go 1.27.2；go.mod 的 toolchain 与 CI 对齐，首次命令可能下载工具链。
- Node.js 24+、Corepack、pnpm 12.14.0；包管理器版本以 web/package.json 为准。
- Python 3.11+、Git、GNU make；Chromium 与 Playwright 所需系统库。
- 公网入口需要 cloudflared；HA 演练另需 Docker Engine、Compose v2、OpenSSL。
- 发布快照需要 GoReleaser 2.17.1。Keycloak 暂不列入本开发环境。

```bash
corepack enable
make doctor
make install
```

Corepack 在 web 目录按 packageManager 选择 pnpm；不依赖主机恰好安装了同版本全局 pnpm。
`make install` 安装 Chromium；系统库不足时按 Playwright 提示由主机管理员准备。
Go 工具链、npm 和容器镜像下载失败须保留实际错误，不降低项目版本或跳过锁文件检查。

## 主机登记与隔离

默认主机目录是 `~/.local/state/tunnel-preview`，可用 `TUNNEL_PREVIEW_HOME` 覆盖为绝对路径。
config.json、登记表、进程身份、私有凭据、日志和实例目录都在此处，目录 0700、JSON 状态文件 0600。
不要把该目录放进 Git，也不要指向生产 data 或已有数据库目录。

可选配置：

```json
{
  "domain": "cubbber.dev",
  "basePort": 30500,
  "mainPort": 30510
}
```

dev0–dev3 使用 30500–30503；独立 main 使用 30510。该范围避开本机 Reader 的 30100 系列和其他项目的 30200 系列。
可按新机占用情况调整，但先停止对应实例、更新 Tunnel 转发；已有绑定保存原端口，须先销毁再重新分配。

共享 pnpm/Go 下载缓存，依赖目录和构建产物按 worktree 分开。预览用自有数据目录，默认 SQLite，
不读取主 checkout 的业务 .env、Server 配置或生产凭据；启动子进程时清除继承的 TUNNEL_/CLOUDFLARE_/CF_ 变量。
PostgreSQL 测试服务可共享，但数据库按测试隔离；禁止对生产 DSN 运行测试。

## 公网 DNS、Tunnel 与证书

主版本：console.cubbber.dev、应用 `<ID>.app.cubbber.dev`。
worktree：console-devN.cubbber.dev、应用 `<ID>.devN.app.cubbber.dev`，N 为 0–3，共四个槽位。
DNS 共 10 条代理 CNAME：5 个控制台记录、5 个应用通配记录；应用基址无需单独页面或 DNS。

先生成可审阅计划，再在主机上准备独立 Tunnel：

```bash
python3 scripts/preview-host.py plan
python3 scripts/preview.py certificates
# 需要本机具有 cubbber.dev 的 cloudflared 登录证书及用户 systemd。
python3 scripts/preview-host.py provision --origincert ~/.cloudflared/cert.cubbber-dev.pem
python3 scripts/preview-host.py status
python3 scripts/preview-host.py check
```

provision 使用名为 tunnel-mvp-preview 的独立 Tunnel，不修改其他项目的 config.yml 或重启其连接器。
完整计划先保存在 host-plan.json；逐条创建 CNAME，不覆盖已有冲突记录，失败后保留已创建资源并可重试。
服务写入 `~/.config/systemd/user/tunnel-mvp-preview.service`。用户退出后是否继续运行取决于主机的 linger 配置。
新机需自己的 Tunnel 和域名；不要把两个使用不同本地端口配置的连接器挂到同一个 Tunnel。

需要覆盖的边缘证书：`*.cubbber.dev`、`*.app.cubbber.dev`、以及 `*.dev0.app.cubbber.dev` 至 `*.dev3.app.cubbber.dev`。
常规 Cloudflare Universal SSL 只覆盖一级子域；应用域要额外准备边缘证书，例如 Advanced Certificate Manager。
一张证书可包含多个通配项；具体签发约束以 Cloudflare 为准。
DNS 通配、证书覆盖和 ingress 转发必须分别验证。仅在源站安装证书无法替代 Cloudflare 对浏览器提供的证书。

参考：[证书覆盖](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/)、
[通配证书](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/)、
[ingress 配置](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/configuration-file/)。

provision 不购买附加服务或自动订购证书。证书未配置完成时公网应用验收会失败；不能用跳过 TLS 校验来通过。
可先用显式本地模式完成开发，并准确记录公网阻塞条件。

## 主版本

main 使用独立实例和数据，不自动随任意 worktree 启动。只有干净的 main checkout 可以：

```bash
python3 scripts/preview.py up --main
python3 scripts/preview.py status --main
python3 scripts/preview.py refresh --main
```

该入口是主分支的独立开发集成环境，初始化合成站点；不是生产数据部署或发布流程。
新基建尚未合入 main 时，不能把任务分支冒充主版本运行。主版本也不会被普通 worktree teardown 销毁。
