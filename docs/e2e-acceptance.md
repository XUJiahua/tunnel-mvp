# 端到端验收矩阵

记录更新：2026-10-20。最新应用行为基线为 stacked PR #22 `70023e9edba70d751f04682aeca02f85c18ba99d`。#19 历史基线为 commit `9b3d7be9909efa83285969718fde3e0707f92920`、tree `47f8264ec6d5b7c7a16bb7ae22e35a9e02cf7e66`；#20 为 HA 设计/验收文档，#21/#22 改造资源及用户查询。本批收敛文档并增加真实旧版升级门禁，不修改应用行为。

本文件区分历史通过、最新 CI 和本次本地结果。不能把历史测试、交叉编译或设计提案记成新环境/HA 已验收。

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

Linux 上执行的协议/进程测试和六平台编译没有验证 macOS/Windows 服务管理、真实 AD/NTLM、更多 IdP 或生产负载。资源自定义域名测试包含运行中更换域名、旧入口拒绝和新入口登录，但只验证代理路由，不代表外部 DNS/TLS 自动管理已实现。控制面 HA 仍未实现，现有测试没有多节点数据库/集群故障保证。

待实现的多组织成员、组织级 IdP、设备风险识别和剩余管理列表/成员索引查询继续列于路线图。HA 代码分解见 [设计](design/control-plane-ha.md)，隔离环境与故障验收要求见 [操作手册](ha-operations.md)。

## #21/#22 最新提交证据

- #21：资源 ID/域名定向 SQL、提交排序、查询故障拒绝、索引计划和 durable 回滚；浏览器增加运行中域名切换。
- #22：用户/直接授权/角色/组同一版本投影、返回值隔离、部分查询故障拒绝和错误计数；管理列表和 membership JSON 扫描仍存在。
- #22 完整 make test、双后端各 15 项 Playwright（含原生 Keycloak/OTP/上游 SSO）与六平台快照已记录通过。
- 最新 commit 的 [PR CI](https://github.com/XUJiahua/tunnel-mvp/actions/runs/39308731234) 与 [push CI](https://github.com/XUJiahua/tunnel-mvp/actions/runs/39308663203) 均 completed/success；PR CI 的 verify、六个 cross-build、JSON/SQLite 两个 keycloak-e2e 共九项通过。普通 verify 不设置 KEYCLOAK_HOME，真实 Keycloak 由独立任务补验，不能把普通套件的 skip 算作通过。
- PR #22 描述另记录最终干净提交上 5464 次混合负载零错误；仍是回环明文短测，不替代生产容量。

## 本批新增：真实 v0.1.1 升级门禁

使用官方 v0.1.1 Linux amd64 发布包，archive SHA-256 固定为 `47de51ee874c2dd0f7f9782d1eb02705ca283d680f07ef669f92188576c0d69b`。`scripts/verify-upgrade.sh` 校验下载内容后调用 Python 演练；旧版号及 commit 也校验，不以手写 JSON 夹具冒充旧版升级。

本次本地应用二进制来自 #22 行为基线，Go 1.27.2 / Linux amd64，旧 Server/旧 Agent/新 Server/上游均为真实进程。JSON、SQLite、双向跨后端配对恢复、原 ID/散列/授权、未授权拒绝、旧会话失效、活动目录锁拒绝、升级后真实写入及原目录回退通过；原始无秘密报告见 [升级记录](acceptance/2026-10-20-upgrade-v011.json)。报告中的 dev/none 是 make build 默认版本元数据，并非正式发布包。候选阶段必须对最终整合提交重新构建、注入版本并重跑。

新演练已进入普通 CI 与发布工作流，输出 JSON 报告作为 Actions artifact；六平台包必须包含升级手册、交付基线和复现脚本。本次本地 Chromium 启动遇到 socket Operation not permitted，浏览器回归交由本批远端 CI；不得将本地受限运行记为通过。
