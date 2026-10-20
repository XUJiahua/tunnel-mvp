# OIDC 身份与账号生命周期设计

本批基于 PR #2 的 `feat/sqlite-storage`，形成第三层 stacked PR。实现控制台统一登录、显式身份绑定和成员禁用；资源授权仍由本地控制面管理。

## 身份模型

用户新增可选 `identity: {issuer, subject}` 与 `disabled` 字段。`issuer + subject` 全局唯一，按原始字符串精确匹配，区分大小写，不修剪或把邮箱、用户名、groups、roles 当作绑定依据。不自动创建账号，也不自动授予站点、应用或管理员角色。一个成员最多绑定一个外部身份；当前只有一个配置中的 IdP。

| 用户类型 | 登录方法 | 管理约束 |
| --- | --- | --- |
| 本地成员 | bcrypt 密码 | 可绑定 OIDC，成为双登录成员；可解除绑定 |
| OIDC 专用成员 | 已绑定的 OIDC 身份 | 创建时无本地密码，禁止重置/自助增加本地密码，不能解除唯一登录方法 |
| 双登录成员 | 本地密码或已绑定 OIDC | 两种入口共享本地角色、授权、禁用与撤销 |
| 管理员 | 本地密码恢复入口 | 本批 API 不提供管理员禁用、删除、身份绑定或 IdP 提权 |

持久化校验要求至少保留一个启用、拥有有效本地密码的管理员。OIDC 专用成员可由管理员禁用或删除。身份重绑定必须由管理员显式输入当前配置 Issuer 下的新 Subject；切换 IdP 不会自动迁移已有绑定。Subject 是敏感标识，管理界面只对管理员展示成员列表，普通用户可读取自身资料。

## 登录协议

使用 `github.com/coreos/go-oidc/v3 v3.19.0` 和 `golang.org/x/oauth2 v0.36.0`。Server 启动时执行 Discovery，要求精确 Issuer、一组有效端点和环境变量中的 confidential client secret；失败时关闭存储并释放目录锁。`--check-config` 只验证本地配置，不访问 IdP，也不输出 secret。

- Authorization Code Flow，`client_secret_basic`，scope 仅 `openid`。不请求 refresh token、offline_access 或用户资料接口。
- 回调固定为配置的 `publicURL + /auth/oidc/callback`；不从请求 Host、Forwarded 或 return URL 推导目的地。
- 每次请求生成独立 state、nonce、PKCE S256 verifier，以及浏览器绑定 Cookie。Cookie 是 host-only、HttpOnly、SameSite=Lax，HTTPS 时启用 Secure，路径限定 `/auth/oidc`。
- pending flow 在 JSON/SQLite 内存或 PostgreSQL 私有认证表保存，5 分钟有效，最多 256 条；清理过期项后仍满则返回 429。浏览器中的单个 flow Cookie 意味着并行发起登录时，以最新一次为准。
- 回调同时匹配 state 和浏览器 Cookie，在交换代码之前原子消费 flow。重复回调、跨浏览器回调、过期 state、IdP error 都不签发会话。
- 校验 PKCE、JWKS 签名、Issuer、Audience、Expiry、nonce、非空 Subject，以及多 Audience/已声明的 authorized party (`azp`)。不信任 email/roles/groups。
- 网络请求超时 10 秒，校验证书；配置、Discovery 和 HTTP 重定向只接受 HTTPS，HTTP 仅供 localhost/loopback 开发。Issuer 可包含 realm 路径，但不允许查询、fragment 或 URL 凭据。
- 成功只签发原有控制台 Cookie，然后跳到固定控制台根路径。令牌不返回前端、不落盘、不转发到内网应用。失败跳到固定 `/?login_error=oidc`，审计只记录通用失败事件，不记录 code、state、nonce 或原始令牌。

浏览器授权导航不受脚本 `connect-src` 限制；换码和 JWKS 请求由 Server 发起，无需给 IdP 配置浏览器跨域 API 权限。

## 本地会话与即时撤销

OIDC 会话到期时间为 `min(ID Token exp, 登录完成时间 + oidc.sessionMinutes)`，默认 60 分钟，可配置 1–720 分钟。网关票据和访问 Cookie 继续依赖这条本地会话；已注册代理请求的截止时间也受它约束。本地密码会话仍为 12 小时。

外部请求期间不持有访问锁。验证令牌完成后，在 `accessMu` 写锁下读取最新绑定和禁用标志，再创建会话；禁用与绑定修改也使用同一把锁，防止并发回调绕过已提交的账号状态。

| 事件 | 结果 |
| --- | --- |
| 退出控制台 | 删除当前本地会话，撤销其票据/访问凭据，关闭相关 HTTP/WS/SSE |
| 禁用、启用或修改身份绑定 | 持久化成功后删除该成员全部本地会话与票据，关闭现有代理连接；启用后须重新登录 |
| 禁用成员重新进行 OIDC/密码登录 | 拒绝，不创建新会话 |
| 收回应用权限 | 沿用前批逻辑，关闭失去权限的应用请求 |
| Server 重启 | JSON/SQLite 清空会话和 pending flow；PostgreSQL 正常重启保留未到期记录 |

本地退出不会清理 IdP SSO Cookie。再次点击统一登录时，IdP 可能无需输入密码。第四批增加 RP 主动退出、back-channel logout 和可选 acr/amr 策略，见 [会话生命周期设计](session-lifecycle.md)。仍未提供令牌 introspection、SCIM 或完整 IdP 禁用事件同步。因此仅在 IdP 禁用用户，不会立即撤销已建立的本地会话；紧急回收必须同时在控制台禁用成员。MFA 由 IdP 执行；第四批可配置可信 ID Token 的 acr/amr 要求。

## API

所有管理写入沿用 exact Origin 校验和本地管理员检查。

| 方法/路径 | 输入/结果 |
| --- | --- |
| `GET /api/auth/options` | 未登录可读：OIDC 开关与 Issuer，不包含 secret |
| `GET /auth/oidc/start` | 创建有界登录事务并跳转授权端点 |
| `GET /auth/oidc/callback` | 一次性回调，成功/失败均跳到固定控制台地址 |
| `POST /api/users` | 原密码成员格式；或 `username, oidcSubject, siteIDs, serviceIDs` 创建 OIDC 专用成员，不能同时提供密码 |
| `PATCH /api/users/{id}/identity` | `{subject}`：绑定当前 Issuer，空字符串仅用于解除有本地密码成员的绑定 |
| `PATCH /api/users/{id}/disabled` | `{disabled: true/false}`，仅允许成员 |
| 用户资料响应 | 增加 `passwordLogin`、可选 `identity/disabled`，从不返回 PasswordHash |

管理事件为 `user_identity_updated`、`user_disabled`、`user_enabled`，登录事件为 `oidc_login` / `oidc_login_failed`。继续沿用 JSONL 审计与轮转。

## 存储兼容与回滚

SQLite schema v2 给 users 增加 disabled、oidc_issuer、oidc_subject，以及非空 Issuer 的身份唯一索引。首次打开 v1 数据库时，在独占目录锁保护下，以单事务添加字段/索引、读取完整模型、完成业务校验后提交版本；任何失败都回滚到 v1。Server、inspect 和 export 都可能触发该升级，升级前应先用旧程序导出并备份整个停机目录。未知版本仍拒绝读取。

JSON 字段均可选，旧数据无需改写即可读取。新导出使用 `tunnel-state` backup v2，继续读取 v1，并在规范化之前验证 checksum。新字段缺省时不改变旧 v1 的规范 JSON 字节；OIDC/disabled 可跨 JSON/SQLite 备份恢复。旧程序不支持 v2 schema/backup，回滚应恢复匹配的旧二进制、配置及升级前备份，不能删除身份字段来绕过旧程序检查。

## 验证边界

- Go 集成测试用独立 HTTP IdP 进行真实 Discovery、授权跳转、PKCE 换码、RS256 签名与 JWKS 获取，测试错误签名/nonce/issuer/audience/azp/expiry/subject、未绑定与大小写错配、state/浏览器绑定、重放、容量和并发禁用。
- 真实 Agent 隧道测试扩展禁用与身份修改后的 WebSocket/SSE 关闭；协议登录测试覆盖 OIDC 用户应用票据和撤销。
- Playwright 跨源走完整页面登录，创建 OIDC 成员、验证无管理员提权、无本地重置入口、禁用/启用与身份修改；同一套验收分别运行 JSON/SQLite。
- 状态测试覆盖 v1 schema 事务升级与失败回滚、旧 v1 backup 读取、身份唯一性、恢复管理员约束和双后端恢复。
- 第三批提供 Keycloak 接入步骤及测试 IdP；第四批补真实 Keycloak/OTP/Agent/退出通知端到端验收。该验收不等于生产安全审计。

参考：[go-oidc](https://github.com/coreos/go-oidc/tree/v3.19.0)、[oauth2 PKCE](https://pkg.go.dev/golang.org/x/oauth2@v0.36.0)、[OpenID Connect Core](https://openid.net/specs/openid-connect-core-1_0.html)、[Keycloak OIDC](https://www.keycloak.org/securing-apps/oidc-layers)。

PostgreSQL 的 state 消费、会话签发和注销 cutoff 在数据库锁内重新检查身份与配置，能够跨两个独立客户端完成。身份状态桥接使用共享 epoch，详细一致性和失效边界见 [HA-2](shared-authentication.md)。
