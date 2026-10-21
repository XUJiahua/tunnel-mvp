# AI 开发工作流

以一个可验收任务为单位：一个任务对应一个分支与 worktree，一个主要写入者负责实施。
人负责目标、产品取舍和体验验收；Agent 负责理解调用链、修改、验证、预览和整理交付。

## 开始任务

1. 阅读根 README、AGENTS.md，再读目标模块的设计与操作手册。
2. 检查 `git status --short`、当前分支和已有修改。记录相关基线失败，不覆盖其他任务。
3. 明确用户场景、期望行为、范围、触发步骤、失败恢复及验收平台。
4. 小修改直接实施；多模块、协议或存储变更先写阶段方案与验收边界。
5. 新主机按[环境准备](development-environment.md)配置；已有主机运行 `make doctor`。

根目录运行命令；项目由 Go 后端和 Next.js 静态前端构成。前端必须经过 export、embed、Go 编译，
才能成为可验收的完整产物。修改静态输出或仅访问 Next 开发服务器不能替代它。

## 日常实施与检查

先沿目标功能追踪界面、API、鉴权、Agent 和存储调用链，再修改相关实现。
复用已有接口；涉及鉴权、组织作用域、ACL、失败关闭、撤销已有流、事务和迁移时补必要行为测试。
低风险文档或样式调整不机械增加测试。

| 场景 | 入口与覆盖 |
| --- | --- |
| 环境检查 | `make doctor`，不代表服务已启动 |
| 安装 | `make install`，准备前端、Go 依赖、Chromium 和 Git hook |
| 日常完整工程检查 | `make verify`，基础设施测试、完整 build 和现有 make test |
| 管理器修改 | `python3 scripts/dev.py verify --profile infrastructure` |
| 浏览器回归 | `python3 scripts/dev.py verify --profile browser`，JSON/SQLite；明确记录环境相关 skip |
| PostgreSQL Go 专项 | `python3 scripts/dev.py verify --profile postgres`，要求显式隔离测试 DSN |
| 容器 HA 演练 | `python3 scripts/dev.py verify --profile ha --postgres 15` 或 `18` |
| 旧版升级 | `python3 scripts/dev.py verify --profile upgrade` |
| 六平台快照 | `python3 scripts/dev.py verify --profile release`，需要固定 GoReleaser |

`core` 不包含浏览器、真实 Keycloak、PostgreSQL 全矩阵、HA 或正式发布。
PostgreSQL 专项使用 `TUNNEL_TEST_POSTGRES_DSN`；它只运行名字匹配 Postgres 的 Go 测试，
不能代替 CI 的完整控制面、浏览器及 HA-4 兼容验收。既有 CI 门禁保留，最终结论以本次 head 的实际结果为准。
新预览环境暂不准备 Keycloak；已有 Keycloak CI 仍负责其专项回归。
普通 CI 另外用独立登记目录执行本地预览首次启动、stop/up、refresh 和每轮 Chromium smoke，结束时清理。

`dev.py verify` 用主机锁串行执行占用固定端口的现有验收，并与预览构建互斥。
直接执行旧命令会绕开该锁；多 worktree 日常使用统一入口。Git pre-commit 只检查暂存区空白错误，
涉及开发脚本时增加基础设施测试；它不替代完整 CI，不设置每次推送都启动完整环境的重型 hook。

## 预览与验收

首次 `make preview`；需要无公网依赖的环境时先选择 `make preview-local`。
DNS、Tunnel 和证书准备完成后，可用 `python3 scripts/preview.py publish` 将本地预览切换为 HTTPS，保留合成数据。
每个预览使用独立 SQLite（可显式选择 JSON）、随机管理员密码、两个合成站点、两个独立 Agent 和两个示例应用。
源码、node_modules、构建目录按 worktree 隔离；主机仅共享依赖缓存、预览登记和可选外部服务。
PG、Keycloak 和 HA 不作为每次预览的前置条件，按任务走专项环境。

编辑完成后暂停修改，运行 `make preview-refresh`；查看 `make preview-status`。
交付需要 ready=true：worker、Server、Agent 存活，二进制摘要和源码匹配，本地身份/健康/页面通过；
公网模式还需 HTTPS 身份探测通过，并在启动时验证两个应用的真实 Agent 路径。
可用 `python3 scripts/preview.py smoke` 对当前版本补充真实 Chromium 的双应用、上传、WS/SSE 和退出撤销检查。
这不代表任意业务、故障恢复或浏览器验收全部通过。按任务执行真实浏览器的成功、失败、撤销、重试和恢复路径。

预览的密码和 Agent token 留在主机私有目录；通过 `python3 scripts/preview.py credentials` 在本机查看管理员账号。
不要把该命令输出、私有日志或 Playwright trace 直接作为公开交付附件。
完整生命周期、域名与证书见[预览运行手册](worktree-preview.md)。

## 交付、合并与归档

交付记录至少包含：

```text
目标与结果：哪些验收项完成，哪些仍未完成。
代码：分支、commit、未提交修改及相关源码摘要。
预览：实际入口、build ID、sourceChanged、是否本地模式。
验证：命令、结果、跳过与未执行项，浏览器及设备覆盖。
数据影响：迁移、共享配置、恢复边界。
下一步：阻塞条件与后续动作。
```

代码实现、工程检查、体验验收和正式发布各自记录。提交仅含本任务文件；按用户授权执行推送、PR、合并和发布。
合并后同步基础分支和依赖 worktree；预览脚本、规则和 paseo.json 必须进入创建新 worktree 的基础分支才能继承。

暂停使用 `preview-stop`，保留数据和槽位；任务完成后 destroy，释放槽位并删除该预览的合成数据和构建。
Paseo teardown 调用 `destroy --if-exists`，不管理独立 main 环境。清理失败保留绑定并排查，不按 URL 是否可达判断资源是否已释放。
