# Worktree 预览运行手册

## 生命周期

在任务仓库根目录运行：

| 命令 | 行为 |
| --- | --- |
| `python3 scripts/preview.py up` | 分配空闲槽位，构建并启动；活 worker 复用，不自动更新源码 |
| `up --slot 0` | 指定槽位，支持 0–3；已占用时拒绝 |
| `up --local --storage json` | 首次分配时选择本地 HTTP 与 JSON；默认公开 HTTPS、SQLite |
| `refresh` | 停止、重新构建、启动，保留槽位、合成账号和业务数据 |
| `publish` | 将当前 worktree 本地预览切换为公网 HTTPS，停止并重建，保留槽位、账号和应用 ID；已是公网时重新构建 |
| `status` / `status --all` | 检查源码、构建、进程、本地及公网身份 |
| `smoke` | Chromium 实测登录、两应用票据/HTTP/上传/WS/SSE，以及退出撤销；报告保存在实例私有目录 |
| `url` | 只输出地址，不探测可用性 |
| `credentials` | 明确请求后在本机输出管理员账号；不要公开该输出 |
| `stop --if-exists` | 停止进程，保留绑定、数据和构建 |
| `destroy --if-exists` | 停止、删除该 UUID 实例目录并释放绑定 |
| `list` | 列出主机所有绑定；丢失源码的实例也保留记录 |
| `routes` | 输出 10 条 ingress 与最后的 404 规则 |
| `dns --tunnel UUID` | 输出 10 条代理 CNAME 配置 |
| `certificates` | 输出所需证书覆盖范围 |

可用 `--root /absolute/checkout` 管理其他或已不存在的源码路径的绑定。
`--main` 明确管理独立主版本；普通归档不加此标志。
存储类型在分配时确定，切换需先导出要保留的测试数据再 destroy、重新 up。
本地转公网使用 `publish`；先准备好该槽位的 DNS、Tunnel 和边缘证书。操作会重启 Server，原会话需要重新登录。
发布失败会保留公网绑定和数据，修复后 `refresh`；恢复本地模式仍需导出数据再 destroy、重新 up。
此命令只公开合成数据预览，不提交代码、不管理 main，也不代表正式发布。
destroy 删除整个预览的合成数据、凭据、日志和构建；不会访问源码、其他实例或生产目录。

默认等待 300 秒；`--wait 0` 只发起后台工作并返回当前状态，未 ready 时退出码为 1。
超时不停止 worker；继续用 status 查看。Ctrl-C、结束 Agent 会话也不自动停止独立 worker。
构建错误、stop、待配置的公网入口都保留槽位；确认不再使用才 destroy。
旧版 dev4–dev9 绑定不会自动删除；收敛后不能再 up/refresh，仍可 status/stop/destroy。
重新 provision 会收敛 Tunnel ingress；旧 DNS 记录保留，需单独清理。

## 构建与身份

构建由全主机 build.lock 串行执行。一个 worktree 的 worker 从构建到停止持有自己的租约，
CLI 操作另有绑定锁，避免两个 refresh、stop 或 destroy 交错。
源码摘要包含 Git 跟踪文件与未忽略的新文件、删除、symlink 和可执行位；commit、branch、dirty 单独保留。
构建前后再次核对源码，变化则失败，不启动混合版本。准备交付时先暂停编辑。

前端 export 后由现有 embed 脚本复制；管理器向 embed 目录加入不含秘密的 `/__preview.json`，
再将身份嵌入一个带版本元数据的 Go 二进制。运行二进制放在该实例独立 build ID 目录，
后续 `make build` 不会替换已运行实例的二进制。只读身份文件含 build ID、实例 ID 和源码信息，不含主机路径或账号。

`sourceChanged=true`、`binaryMatches=false`、身份不匹配或任一进程退出都会使 ready=false。
up 发现活 worker 但源码已变时返回未就绪，需 refresh；它不默默重建。
worker 保存 Linux boot ID、PID 和启动时间，清理前核对身份，避免误杀复用的 PID。
worker 意外终止后，stop/refresh/destroy 根据私有子进程记录清理残留，再处理绑定。
清理等待整个受管进程组退出，超时后发送 SIGKILL；构建命令退出或被中断时也先清理后代，再释放构建锁。
新启动的进程通过继承的随机归属标记支持组长退出后的身份核验；若旧记录缺少标记且无法核验残留归属，
清理会报错并保留绑定，需在本机排查，不能仅凭进程组编号终止进程。

旧构建保留在实例目录直到 destroy，便于定位；主机需定期检查磁盘并释放不用的绑定。
不要直接删除登记目录来释放一个槽位，这会丢失进程归属。

## 初始化与 Ready 边界

实例只接受合成数据：随机 preview-admin、两个站点、每站点一个独立 Agent、两个上游示例应用。
上游绑定动态 loopback 端口，Agent 仅允许各自的 127.0.0.1/32 与对应端口。
刷新保留站点/应用 ID 和凭据，更新示例目标端口。手动删除或撤销受管示例会导致后续初始化失败；
需要恢复配置或明确销毁并重建，不能通过清空其他数据来修复。

启动按顺序验证：本地嵌入身份、healthz、页面；公开模式再验证真实 HTTPS 入口身份；
管理员登录、两 Agent 上线、应用一次性票据兑换与真实 Agent 返回的构建标记；然后撤销初始化会话。
status 动态重验 worker/子进程、源码摘要、二进制摘要、本地与公网控制台身份，应用 URL 列于 applications。
状态不会持续重放应用票据、浏览器或故障测试，相关业务验收仍需执行。
smoke 在退出前确认两应用的 SSE 和 WebSocket 仍然打开，退出后要求原连接在 10 秒内终止，并验证新请求被拒绝。

本地模式使用 `localhost:3050N` 和 `<ID>.apps.localhost:3050N`，公开模式使用已约定的 cubbber.dev 域名。
本地直连探测保留公开 Host/Origin；公开模式的 Agent 实际使用对应 HTTPS 控制台，通过公网 WSS 接入。
不能只靠本地 Host 直连声称公网链路已经通过。
公网探测在 TCP 或 TLS 连接超时时尝试 DNS 返回的其他地址，始终使用原域名做 SNI 与系统 CA 校验。
证书验证失败立即报错；票据兑换和业务写入不重放。公网身份/健康/页面三个只读 GET 遇网络错误时重试一次；
公开 HTTP 响应超时为 15 秒。Chromium 的身份及退出后拒绝访问 GET 遇 ECONNRESET 最多重试两次。
主机设置 `https_proxy` / `HTTPS_PROXY` 时，Python 公网探测与 Chromium smoke 使用 HTTP CONNECT 代理，并遵循 `no_proxy`；
TLS 仍由客户端校验目标域名。代理地址需使用 `http://`，不要把含认证信息的代理配置写入仓库或报告。

## 故障处理

| 现象 | 检查 |
| --- | --- |
| queued/building 很久 | build.lock 队列、frontend.log/build.log、工具链及依赖网络 |
| checking_public 失败 | DNS、连接器、证书、真实响应实例身份；失败保留绑定 |
| 应用 TLS 失败 | 证书必须覆盖 `<ID>.devN.app.cubbber.dev`，父级通配证书不够 |
| 请求进入主版本 | 把具体 `*.devN.app` ingress 放到 `*.app` 前面，保留 Host |
| 有端口占用 | 检查对应服务；管理器拒绝覆盖或终止无关进程 |
| 槽位耗尽 | list/status --all；停止或失败仍占位，不用的实例 destroy |
| 源码已变、页面仍旧 | 停止编辑，refresh，核对 build ID |
| worker 已死但端口仍在 | stop/refresh 会检查并回收自己记录的子进程；不能杀无关 PID |

日志、fixtures.json、credentials.json 可能含凭据或业务诊断，仅在本机检查；分享前脱敏。
