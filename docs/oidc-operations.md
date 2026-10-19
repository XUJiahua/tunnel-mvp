# OIDC 接入、成员禁用与恢复手册

适用于包含第三批代码的构建。统一登录只保护 tunnel 控制台及应用入口，内网应用仍保留自身认证。

## 1. 升级现有部署

1. 停止 Server，确认服务管理器不会再次拉起旧进程。
2. 用上一批二进制执行 `storage export` 并 `storage validate`，另行备份整个停机数据目录、配置、凭据环境和审计日志。
3. 替换为本批二进制。JSON 旧数据直接兼容；SQLite v1 首次被 Server 或 inspect/export 打开时会事务升级到 v2。升级失败保留 v1，排查错误后重试。
4. 保持原本地管理员密码，先不启用 OIDC 启动并验收密码登录、原 Agent 令牌重连和应用访问，再安排 IdP 接入。

不要用旧程序打开升级后的目录。回滚必须停机并恢复升级前的二进制、配置及状态副本；不要删除 schema 标记或身份字段。新 export 输出 backup v2，本程序也支持旧 backup v1。

## 2. 在 IdP 注册客户端

使用标准 OIDC confidential client：

| 设置 | 值 |
| --- | --- |
| Client ID | 例如 `tunnel-console` |
| Client authentication | 开启，客户端密钥认证 `client_secret_basic` |
| Standard/Authorization Code Flow | 开启 |
| Implicit / Direct Access Grant / Service Accounts | 无需启用 |
| PKCE | S256，可在 IdP 要求客户端必须使用 S256 |
| Redirect URI | 精确的 `https://console.example.com/auth/oidc/callback`，不使用通配符 |
| Scope | `openid`；无需 email/profile/offline_access |

IdP、浏览器和 Server 必须使用一致的 Issuer，例如 `https://sso.example.com/realms/tunnel`。不要把容器内部 URL 配成 Issuer、浏览器却访问另一个公网 Issuer。Server 需要能够访问 Discovery、Token 和 JWKS 端点；使用系统信任的 TLS 证书/CA。

## 3. 配置 Server

参考 [生产配置示例](../examples/oidc/server.production.json)：

```json
"oidc": {
  "issuer": "https://sso.example.com/realms/tunnel",
  "clientID": "tunnel-console",
  "sessionMinutes": 60
}
```

`oidc` 是 Server JSON/YAML 顶层字段。也支持以下覆盖：

| 配置 | 环境变量 | CLI |
| --- | --- | --- |
| `oidc.issuer` | `TUNNEL_OIDC_ISSUER` | `--oidc-issuer` |
| `oidc.clientID` | `TUNNEL_OIDC_CLIENT_ID` | `--oidc-client-id` |
| `oidc.sessionMinutes` | `TUNNEL_OIDC_SESSION_MINUTES` | `--oidc-session-minutes` |
| client secret | `TUNNEL_OIDC_CLIENT_SECRET` | 仅环境变量，不写配置/CLI |

优先级为 CLI > 环境变量 > 文件 > 默认。未设置 Issuer/Client ID 则关闭 OIDC，保留原密码登录；配置不完整会报错。配置未设置或值为 0 时会话上限默认 60 分钟，显式非零值限 1–720。客户端 secret 由服务管理器或 secret 注入机制设置；不要提交仓库，也不要把真实值写进 shell 命令历史。

```bash
./tunnel server -c server.json --check-config
./tunnel server -c server.json
```

`--check-config` 不连接 IdP、不检查 secret 是否存在。实际启动会进行 Discovery；缺少 secret、Issuer 不匹配、证书或网络错误会阻止启动并释放数据目录锁。生产使用 HTTPS publicURL/Issuer/端点；HTTP 只允许本机 loopback 开发。

Compose 已透传上述四个可选环境变量。使用仓库 `compose.yaml` 时，可在服务环境中设置它们，继续使用原 production config；`TUNNEL_OIDC_SESSION_MINUTES` 留空即可使用默认值。修改配置/secret 后重启 Server；会话不会跨重启保留。

IdP 故障导致启动失败时，可使用移除 `oidc` 的恢复配置，并清除服务环境中全部 `TUNNEL_OIDC_*` 覆盖后重启，以本地管理员恢复运维；OIDC 专用成员在恢复期间无法登录。环境变量留空不会覆盖配置文件中的非空 Issuer，需修改实际配置来源。

## 4. 创建或绑定成员

先用本地管理员登录，进入“访问权限”：

- 新成员：点击“添加成员”，登录方式选择“OIDC 统一登录”，输入本地用户名、IdP Subject，再分配整站/应用权限。此账号没有本地密码。
- 已有密码成员：点击“身份绑定”，输入当前 Issuer 下的 Subject。该操作保留本地密码，形成双登录入口；只想使用 OIDC 时应新建 OIDC 专用成员并迁移授权，当前没有删除本地密码的 API。
- 重新绑定：在“身份绑定”修改 Subject 后保存，原来的所有会话和应用连接立即失效，新身份须重新登录。
- 解除绑定：有本地密码的成员可清空 Subject；OIDC 专用成员不能清空唯一登录方法，应禁用或删除。

Subject 必须是该客户端 ID Token 中的 `sub`，精确区分大小写，不是邮箱/显示名。Keycloak 普通本地用户通常使用用户 ID；若配置了 pairwise subject 或自定义映射，以实际客户端 `sub` 为准。通过可信 IdP 管理界面/管理员取得标识，不把真实令牌粘贴到公开解码网站。

切换 Issuer 后，旧绑定仍保留但无法通过新 IdP 登录。由本地管理员逐个核对并重新绑定；同名邮箱不会触发迁移或角色提升。当前不提供 OIDC 管理员绑定、自动注册或组/角色同步。

成员从登录页选择“使用统一身份登录”，完成 IdP 的密码/MFA，再回到控制台。本地 username 只是控制台显示名。通过 IdP 登录后仍只能看到本地授予的站点/应用。

## 5. 禁用、启用与紧急回收

在成员行点击“禁用账号”并确认：持久化后立即撤销该成员全部控制台会话、应用凭据和未使用票据，主动关闭现有 HTTP/WS/SSE。两个登录入口都会拒绝该账号。已分配权限和身份绑定保留，方便审计和恢复。

“启用账号”不会恢复旧会话，成员须重新登录。绑定保存也会撤销该成员全部会话。管理员账号不提供禁用操作，确保本地恢复入口存在。

仅在 IdP 禁用用户，不会立即同步到 tunnel。紧急离职回收需同时在 tunnel 禁用成员；单纯 IdP 禁用后，已建立的会话可继续到其原到期时间。OIDC 会话上限为配置时间与 ID Token 剩余有效期的较短者。

控制台“退出登录”只退出当前 tunnel 会话。再次统一登录可能直接复用 IdP 的 SSO 状态。需离开共享电脑时，同时退出 IdP/浏览器会话。MFA 在 IdP 配置和执行；第四批增加可选 acr/amr 策略、back-channel/RP 退出与设备列表，见 [会话与退出手册](session-operations.md)。

## 6. Keycloak 本地接入演练

本节是容器操作示例。第四批已增加无需 Docker 的原生 Keycloak + OTP 端到端验证，见 [会话与退出手册](session-operations.md)；下列 Compose 部署仍需按实际环境演练。

1. 设置 `KC_BOOTSTRAP_ADMIN_PASSWORD` 为本地演练管理员密码，执行：

   ```bash
   docker compose -f examples/oidc/keycloak.compose.yaml up -d
   ```

2. 打开 `http://localhost:8081`，以 `admin` 和上一步密码登录。创建 realm `tunnel`，按第 2 节创建 client `tunnel-console`，redirect URI 改成 `http://localhost:8080/auth/oidc/callback`。复制客户端密钥到本地 Server 的 `TUNNEL_OIDC_CLIENT_SECRET`。
3. 创建普通测试用户、设置密码并记录用户 ID。使用 [本地 Server 示例](../examples/oidc/server.development.json)，首次启动另设 `TUNNEL_ADMIN_PASSWORD`；Server 在宿主机运行，访问同一 `http://localhost:8081/realms/tunnel` Issuer。
4. 本地管理员创建 OIDC 成员并绑定测试用户实际 Subject。为其只授权一项测试应用，确认不能访问兄弟应用；退出后从统一入口登录。
5. 在 Keycloak 设置 MFA 之后重复登录；再在 tunnel 禁用成员，确认旧应用连接关闭、重新登录失败，启用后旧会话仍无效。
6. 检查审计中的 `oidc_login`、`user_disabled`、`user_enabled`、`user_identity_updated`，不应包含原始令牌。结束演练用同一 Compose 文件执行 `down`；命名卷保留 IdP 数据。

Keycloak 采用固定示例版本 `26.8.0`，遵循[官方容器入门](https://www.keycloak.org/getting-started/getting-started-docker)；`start-dev`、loopback HTTP 与内置数据库仅用于此演练，生产按官方 HTTPS/数据库/备份方案部署。

## 7. 内网应用自身的 OIDC/SSO

控制台 OIDC 不会把 Token/Header 注入上游，也不会替内网应用建立会话。如果应用本身支持 OIDC，可把它注册成同一 IdP 的独立客户端，redirect URI 使用该应用的固定公网访问子域名与其真实回调路径，并为其分别配置 client ID/secret 和允许的外部根地址。IdP 在浏览器端复用 SSO 状态，应用仍独立验证令牌和建立自身会话。

先验证应用的外部 URL、回调路径、Cookie、WebSocket Origin 和绝对链接；目前仅支持 origin 目标，不承诺通用 HTML/JS 改写或自动修复所有产品。网关权限与应用自身权限分别配置，tunnel 退出不等于应用或 IdP 全局退出。应用服务器若需出站访问 IdP，必须自行具备网络可达性，不能假定反向隧道提供任意出站代理。

## 8. 排障与验收

| 现象 | 检查 |
| --- | --- |
| 启动缺少 secret | 核对运行服务实际环境，`--check-config` 不验证该项 |
| Discovery 失败 | Issuer 精确值、realm 路径、网络、CA、端点 HTTPS 与 IdP 可用性；错误日志不会输出 secret |
| 统一登录失败 | 本地成员是否存在/启用、Issuer/sub 是否精确匹配、client secret/redirect URI、ID Token audience/有效期和两端时钟 |
| 多标签页登录失败 | 重新从登录页发起；同一浏览器以最新 flow Cookie 为准 |
| Server 重启后回调失败 | pending flow 在内存中，重新发起登录 |
| 登录拥塞返回 429 | 最多 256 条 pending，5 分钟到期清理；检查 IdP 故障和重复发起请求 |
| 专用成员无法重置密码 | 在 IdP 管理密码；本地 API 不为其创建备用密码 |
| IdP 注销后仍能访问 | 本地会话独立，使用控制台禁用即时收回，核对到期上限 |

完成一次绑定成功、未绑定拒绝、同名/邮箱不提权、单应用授权、退出、禁用/启用、重新绑定和 Server 重启后的重新登录验收。自动检查命令为 `make test`，以及构建后在 `web` 中分别运行普通和 `TUNNEL_TEST_STORAGE=sqlite` 的 `pnpm run test:e2e`。

发布前本地执行 `make release-snapshot`，检查六平台 archive/checksum/文档完整性。v2.18.3 在本环境串行模式仍出现 gzip 截断，因此固定 GoReleaser v2.17.1，连续两次独立检出快照全部通过。尚未证明上游根因，不能把串行模式视为修复；保留全文件解压、CRC 与 SHA-256 检查，不以重试代替校验。该措施不改变 Server 请求并发。
