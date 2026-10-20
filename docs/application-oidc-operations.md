# 应用 OIDC 接入与故障演练

按 [示例 README](../examples/oidc-app/README.md) 构建和配置。生产 client 的精确 redirect URI 必须与应用 `public-url` 加 `auth/callback` 一致；同时匹配协议、域名、端口、路径与尾斜杠。Tunnel 目标保留相同路径前缀，不做通用 HTML/JS 内容改写。

## 操作顺序

1. 先确认 Agent 在线、ACL 允许目标、私网 GET `/demo/healthz` 返回 200；控制台业务探测使用相对 `healthz`。
2. 用有资源权限的 OIDC 成员从控制台打开应用；应显示 IdP 认证的应用用户。用无权限成员请求 launch 应被拒绝。
3. 在应用内退出，应进入 signed-out 页面，控制台仍已登录；再次点击登录，已有 IdP SSO 时不要求重输密码。若 realm 强制重新认证/MFA，以 IdP 策略为准。
4. 验证控制台退出/收回资源权限后，旧应用入口被网关拒绝。启用状态桥接时，在 Keycloak 禁用用户但不调用 logout，按配置的轮询/最大陈旧期限观察撤销；本地恢复管理员仍能管理。启用后旧 cookie 不自动复活。
5. 记录应用、Agent、控制面和 IdP 的时间及结果。不要把带授权码的地址、cookie、client secret 或浏览器 trace 上传到公开 issue。

## 故障定位

| 现象 | 检查 |
| --- | --- |
| 资源打开 401/403 | 控制台会话、成员授权、资源票据是否过期，先从控制台重新打开；随后查 Keycloak 同步指标/审计 |
| 应用回调 403 | 精确回调、client secret、PKCE S256、issuer、浏览器 auth 路径 cookie、时间同步、反复使用旧回调 |
| discovery 启动失败 | 应用主机到 issuer 的 DNS/TLS/CA；不允许重定向或非回环 HTTP |
| 探测成功但登录失败 | healthz 不访问 IdP；分别检查应用到 token/JWKS endpoint 的连接 |
| 退出后点击登录直接成功 | 应用本地退出保留了 IdP SSO，这是预期；全局退出需要业务应用自己接入 |
| 应用 429/503 | 待完成登录或内存会话达到上限；检查异常登录流量；重启会清会话，不能用扩副本共享此内存 |

## 本机回归

```bash
make test
make build
# 独立原生 Keycloak/Java、Chromium 环境，路径按安装调整。
GO_BINARY=/opt/go/bin/go KEYCLOAK_HOME=/path/to/keycloak \
JAVA_HOME=/path/to/java FONTCONFIG_PATH=/path/to/fonts \
PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/path/to/chromium \
pnpm --dir web run test:e2e
# 上述完整命令增加 TUNNEL_TEST_STORAGE=sqlite 再运行一次。
```

两次完整浏览器运行应串行执行，Keycloak fixture 使用固定端口和独立临时 realm。真实 IdP 测试内部构建并启动本示例，不是伪造身份头或仅检查 UI 截图。
