# 上游 OIDC 应用示例

独立 Go 模块，演示真实应用通过 Tunnel 接入同一个 Keycloak。控制台授权决定能否到达应用，应用自己的 OIDC client 验证用户身份。两者不能相互替代。示例只有已登录页面，没有业务角色/对象授权，不能直接作为生产应用的权限框架。

## 构建与接入

需要 Go 1.27+。仓库根目录执行 `make example-oidc GO=/opt/go/bin/go`；发布包只包含示例源码，可进入本目录运行 `go build -trimpath -o oidc-app .`。Tunnel 主程序运行不需要 Go。

1. 在 Tunnel 创建应用，目标例如 `http://127.0.0.1:9090/demo/`，为 Agent 本地 ACL 开放目标 IP 和 9090 端口。应用成员授权仍由 Tunnel 管理。Agent 与应用不在同一主机时使用 Agent 能访问的私网地址，应用监听相应私网接口。
2. 记录最终的公网应用地址，例如 `https://APP_ID.apps.example.com/demo/`。自定义域名也必须使用实际入口。DNS/TLS 配置见资源操作手册。
3. 在同一个 Keycloak realm 创建**独立的 confidential OIDC client** `tunnel-example-app`，启用 Standard Flow 和 client authentication，要求 PKCE S256，关闭 Direct Access Grants 和 Service Accounts。精确登记回调 `https://APP_ID.apps.example.com/demo/auth/callback`，不要使用通配符。无需 Web Origins：换码在服务端完成。
4. 只把该 client 的 secret 放到应用主机环境中；不要复用控制台 client 或只读状态同步 service client 的密钥。

```bash
read -rs OIDC_APP_CLIENT_SECRET
export OIDC_APP_CLIENT_SECRET
./bin/oidc-app --listen 127.0.0.1:9090 \
  --public-url https://APP_ID.apps.example.com/demo/ \
  --issuer https://id.example.com/realms/tunnel \
  --client-id tunnel-example-app
```

从本目录构建时使用 `./oidc-app`。也可设置 `OIDC_APP_PUBLIC_URL`、`OIDC_APP_ISSUER`、`OIDC_APP_CLIENT_ID`；命令行显式参数优先。密钥只读环境变量。issuer 及 IdP 端点要求 HTTPS，localhost/回环 HTTP 用于开发；公网应用地址仅对 localhost/回环允许 HTTP。不提供跳过证书验证选项。

健康检查选 GET、相对路径 `healthz`、预期 200，对应 `/demo/healthz`。它表示应用进程正常，不代表 IdP 可用或用户拥有业务权限。

## 验收与退出

先在控制台使用 Keycloak + OTP 登录，再打开被授权的示例。已有 IdP SSO 会话时，应用完成自己的授权码/PKCE 跳转，显示 Keycloak 的 `preferred_username`；账户关联仍使用签名 `iss/sub`，显示名不能作为业务授权键。控制台本地账号不会自动变成同名 IdP 账号。

应用按钮仅退出应用自己的会话，控制台和 Keycloak SSO 会话独立保留；再次登录可能无需输入密码。控制台退出、用户禁用或收回资源授权时，Tunnel 会阻止旧网关凭据并关闭已有隧道流量。示例未实现应用自身 back-channel/RP 全局退出，私网直达应用的访问不受 Tunnel 会话撤销影响。生产环境应限制应用入口为可信 Agent 网络，并按业务要求接入 IdP 退出事件。

应用会话保存在内存，最长 30 分钟且不超过 ID token 到期；重启丢失。不刷新 token，不请求 offline_access，不向浏览器存储 access/ID token。会话上限 1024，待完成登录上限 256，达到上限拒绝新请求。`public-url` 是固定可信配置，不从请求 Host 拼接回调。

完整设计见 [应用 OIDC](../../docs/design/application-oidc.md)，故障检查见 [操作手册](../../docs/application-oidc-operations.md)。
