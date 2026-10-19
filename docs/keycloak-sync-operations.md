# Keycloak 禁用自动撤销操作

使用已有 OIDC 接入、显式 issuer/sub 绑定和本地恢复管理员。先确认该 realm 使用默认公开 subject，绑定的 sub 与 Admin Console 中用户 ID 相同；pairwise subject 或自定义 sub mapper 不适用此直接映射。

在同一 realm 创建独立 confidential OIDC client，开启 Service accounts、关闭浏览器标准登录和密码授权。只给其 service account 分配 realm-management 的 view-users / query-users，不授予 manage-users 或 realm 管理。桥接只 GET 已绑定活跃成员的用户信息，不在 IdP 写账号或调用 logout。

```json
{
  "oidc": {
    "issuer":"https://id.example.com/realms/company",
    "clientID":"tunnel-console",
    "sessionMinutes":60,
    "keycloakSync":{
      "enabled":true,
      "clientID":"tunnel-session-reader",
      "subjectIsUserID":true,
      "pollSeconds":15,
      "maxStaleSeconds":60
    }
  }
}
```

在受保护的服务环境中设置 TUNNEL_KEYCLOAK_SYNC_CLIENT_SECRET；原 TUNNEL_OIDC_CLIENT_SECRET 继续用于浏览器客户端，两者独立。Secret 不写 JSON、不放命令参数或管理页面。`server --check-config` 可查看非秘密的有效设置，客户端名、映射确认及间隔也有 `--keycloak-sync-*` / `TUNNEL_KEYCLOAK_SYNC_*` 覆盖。

上线前完成演练：OIDC+OTP 登录，打开应用及 WS/SSE；在 Keycloak 仅禁用该用户，不额外点击注销会话，观察 Tunnel OIDC 设备、应用请求和流被撤销；本地恢复管理员保持可用。查看 identity_sync_session_revoked 审计及 tunnel_identity_sync_enabled/checks_total/errors_total/revocations_total 指标，确认实际由桥接撤销。

禁用 IdP 用户只影响 OIDC 身份入口，本地密码是独立凭据。完全离职需同时禁用 Tunnel 成员；OIDC-only 成员没有本地密码入口。IdP 重新启用不会恢复旧 cookie，本地手动禁用也不会自动解除。

状态/token 请求失败时，新 OIDC 登录拒绝；现有 OIDC 状态超过 maxStale 后拒绝新请求，并由一秒失效扫描关闭已有连接。检查 service-client secret、用户查看权限、HTTPS/realm 路径及 IdP 可用性，不能通过给桥接增加写权限或无限延长 TTL 修复。现有 Server 的本地密码恢复入口保留；服务重启仍受原 OIDC discovery 启动要求约束，IdP 完全不可达时可按既有离线恢复手册暂时关闭 OIDC 配置启动本地管理。

轮询按四并发执行，一轮结束再等待间隔；大量活跃用户/高 RTT 会拉长周期，应在目标规模验收并设置 TTL，否则可能按预期失效关闭。这里只实测 Keycloak 26.8.0，不把其他 IdP 的账号禁用或任意 sub 映射视为已兼容。

设计：[Keycloak 状态同步](design/keycloak-session-sync.md)。原有标准退出：[设备与退出手册](session-operations.md)。
