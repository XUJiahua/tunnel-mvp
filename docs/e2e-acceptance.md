# 端到端验收矩阵

记录日期：2026-10-19。代码基线为 stacked PR #19 `feat/application-oidc-example`，该层代码 tree 为 `47f8264ec6d5b7c7a16bb7ae22e35a9e02cf7e66`，远端 commit 为 `9b3d7be9909efa83285969718fde3e0707f92920`。所有父批次在同一完整回归中执行；PR #20 补设计、验收记录及发布内容检查，不修改应用行为。不得把未执行的外部场景标为通过。

## 运行环境与命令

本地 Linux、Go 1.27.2、Node 24.19、pnpm 锁定版本、真实 Chromium、Keycloak 26.8.0 / Java 21；控制面、Agent、上游与 IdP 都是独立进程。两套授权存储分别为 JSON 和 SQLite。测试账号、密钥及数据目录临时生成，不使用生产凭据。完整浏览器套件串行执行，固定端口互不重叠。

```bash
PATH=/opt/go/bin:$PATH make test
PATH=/opt/go/bin:$PATH make release-snapshot GORELEASER=/path/to/goreleaser-2.17.1
# 快照 hook 已构建前端；浏览器运行使用当前源码本地二进制。
CGO_ENABLED=0 /opt/go/bin/go build -trimpath -o bin/tunnel ./cmd/tunnel
GO_BINARY=/opt/go/bin/go KEYCLOAK_HOME=/path/to/keycloak JAVA_HOME=/path/to/java \
FONTCONFIG_PATH=/path/to/fonts PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/path/to/chromium \
pnpm --dir web run test:e2e
# 同一完整命令加 TUNNEL_TEST_STORAGE=sqlite 再运行一次。
```

`make test` 包含根模块/控制面两种后端 race、remotedialer 和 OIDC 示例独立模块 race/vet、发布脚本验证、根模块 vet 和 TypeScript。前端完整 build 在发布快照 hook 执行。GoReleaser 固定 2.17.1，六个 archive 验证 checksum、gzip 完整页脚或 ZIP CRC，以及二进制/源码/操作设计文档。OIDC 示例还单独完成六平台 CGO=0 编译。

## 浏览器覆盖

每一行是实际测试文件；不能把一行中的多个断言换算成多个独立端到端测试数。

| 文件（web/tests） | 批次与真实覆盖 |
| --- | --- |
| console.spec.ts | 1：登记站点、真实 Agent、内网上游登录、成员权限、HTTP/上传/WS/SSE |
| oidc.spec.ts | 3：跨源签名 IdP 回调、显式绑定、禁用和本地恢复 |
| sessions.spec.ts | 4：设备所有权、管理员逐会话撤销、离线恢复 |
| keycloak.spec.ts | 4/18/19：真实 OTP、back-channel/RP 退出、Keycloak 禁用而不调用 logout、本地恢复；独立上游应用 PKCE、路径挂载、应用内退出/再次 SSO和禁用后访问拒绝 |
| groups.spec.ts | 5：一次邀请、组内授权边界、已有长连接撤销 |
| administrators.spec.ts | 6：当前密码、最后恢复管理员、降权撤销 |
| organizations.spec.ts | 7：组织作用域、真实 Agent 访问和角色撤销 |
| resources.spec.ts | 8：原子资源、自定义域名路由、路径登录/上传、活动迁移 |
| monitoring.spec.ts | 9：业务健康、离线配对归档/验证、跨后端恢复后采样 |
| audit.spec.ts | 10/17：保留文件搜索/分页、组织隐私、启动轮转、旧源事件、重启游标失效 |
| agents.spec.ts | 11：双 Agent、独立凭据、流归属、进程失效、逐实例撤销 |
| agent-policy.spec.ts | 12：初始拒绝、收紧已有流、本地 ACL 上限 |
| agent-updates.spec.ts | 13：实际编译版本升级、无效候选自动回退、重放拒绝和离线回滚 |
| connection-mode.spec.ts | 14：浏览器会话连接身份隔离和模式变更撤销 |
| delegated-roles.spec.ts | 15：有界委派、能力上限、角色成员实时撤销 |

存储迁移/备份版本、事务失败回滚、SQLite 稀疏写入和审计 v2→v3 迁移/保留行不重写/外键拒绝由针对数据库和文件的 Go 测试验证；浏览器同时覆盖持久化后的真实管理行为、重启/恢复及审计查询。不能用浏览器截图证明事务边界。

## 结果

- PR #17：make test、JSON/SQLite 各 15 项完整浏览器、六平台快照通过；远端 CI 全通过，含原生 Keycloak 两后端。
- PR #18：make test、JSON/SQLite 各 15 项完整浏览器、六平台快照通过；增加真实 Keycloak 禁用状态同步演练。
- PR #19：独立 OIDC 模块 race/vet、真实 Keycloak 扩展场景、六平台快照及示例六平台编译通过；JSON/SQLite 各 15 项完整浏览器通过（3.4/3.0 分钟）；全量 make test 通过（控制面 JSON/SQLite race 272.608/271.374 秒；嵌套模块、vet 和 TypeScript 通过）。
- 本地容量报告另见 [容量验收](capacity-report.md)：双后端共 5464 次请求零错误，并采样 RSS/FD。生产 SLO 和真实 WAN/TLS/落盘负载仍待验证。

## 未覆盖与继续条件

Linux 上执行的协议/进程测试和六平台编译没有验证 macOS/Windows 服务管理、真实 AD/NTLM、更多 IdP 或生产负载。资源自定义域名测试验证代理路由，不代表外部 DNS/TLS 自动管理已实现。控制面 HA 仍未实现，现有测试没有多节点数据库/集群故障保证。

待实现的多组织成员、组织级 IdP、设备风险识别和增量 SQL 查询继续列于路线图。HA 代码分解见 [设计](design/control-plane-ha.md)，隔离环境与故障验收要求见 [操作手册](ha-operations.md)。
