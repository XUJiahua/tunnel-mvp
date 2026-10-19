# 上游应用 OIDC 设计

## 身份与访问边界

`examples/oidc-app` 是独立可构建模块，不把 Tunnel 用户头当成应用身份。浏览器先持有 Tunnel 的资源访问 cookie，再由应用独立发起 OIDC Authorization Code + PKCE。应用 client 与控制台 client、Keycloak 状态同步 client 分离，分别固定回调和密钥。IdP SSO 使两次身份认证可以共享交互状态，但不让应用跳过签名验证。

网关每次请求校验控制台会话、资源授权和可选 Keycloak 同步状态。业务应用按 `iss/sub` 识别用户，再自行实现业务对象授权。示例允许经其 IdP 认证的用户查看演示页，没有业务 ACL，不能以显示名匹配管理员权限。

## 协议与状态

- 固定可信公网 URL，保留 `/demo/` 路径，回调为 `auth/callback`；不使用请求 Host 或任意 return URL。
- 每次登录随机 state、nonce、浏览器绑定 cookie 和 S256 verifier；服务端最多保留 256 个待完成请求，5 分钟清理，回调一次消费，错误也不重试同一 state。
- token endpoint 使用 confidential client Basic 认证和 PKCE。库验证 ID token 签名、issuer、audience、expiry，应用另检查 nonce、subject、azp。
- 只把随机 opaque cookie 给浏览器，服务端存储其 SHA-256 键、subject、显示名、CSRF 和过期时间。最多 1024 会话，期限取 30 分钟和 token 期限较小者。没有 refresh/offline token。
- cookie 使用 HttpOnly、SameSite=Lax、挂载路径和公网 HTTPS 下的 Secure。登录 cookie 仅作用于 `auth/`。应用不读取或覆盖 Tunnel cookie。
- 模板自动转义显示名；响应 no-store、nosniff，CSP 禁止脚本/连接及嵌入。POST 本地退出验证会话内 CSRF，删除服务端会话。

发现、换码和 JWKS 请求有超时，拒绝重定向；IdP endpoint 要求 HTTPS（回环开发例外）。未记录原始 token 或密钥。配置的 IdP 和应用主机属于信任边界，示例不是多租户任意 IdP 代理。

## 生命周期与限制

应用本地退出不结束 Keycloak 或控制台会话。Keycloak 禁用经第十八批桥接使 Tunnel 撤销资源访问，应用内存会话可能仍存在；重新登录网关后，业务应用自身的全局退出/重新认证策略需另行实现。不能把网关关闭等同于删除所有应用服务端会话。

应用进程重启丢失会话。没有集群共享存储、应用 back-channel、角色映射或生产业务授权。健康端点仅检查进程路由。示例源码随六个平台发布包交付，不增加第二个默认发布二进制。

## 验证

独立签名 IdP 单元测试覆盖 PKCE、浏览器绑定、state 重放、nonce/issuer/audience 拒绝、显示名转义、过期与退出 CSRF。嵌套模块纳入 make test 的 race/vet。

真实 Keycloak 26.8.0 浏览器场景：OTP 控制台登录 → 一次性资源票据 → Agent → `/demo/` → 应用独立 OIDC 回调 → 已验身份页面 → 本地退出 → IdP SSO 再登录；禁用用户不调用 logout 后浏览器导航被网关拒绝。完整场景分别使用 JSON/SQLite，保留此前 back-channel/RP 退出和 HTTP/WS/SSE 验证。
