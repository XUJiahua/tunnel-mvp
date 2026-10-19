# Keycloak 用户状态到 OIDC 会话的自动撤销

此桥接补充标准 back-channel logout：Keycloak 用户被禁用/删除但未发送 logout_token 时，已有控制面 OIDC 会话也必须失效。仅在本机明确开启，使用独立只读 service-account client；不授予写用户、注销其他系统或修改 realm 的能力。

## 映射与信任

必须显式配置 subjectIsUserID=true，确认此 realm 的 OIDC sub 就是 Keycloak User ID。pairwise subject、自定义 sub mapper 等不同映射不能直接启用。不会按邮箱、用户名或显示名合并账号，也不自动注册/更改本地用户。Keycloak Admin URL 从同一 issuer 的 /realms/{realm} 路径派生，token endpoint 必须与该 realm/源一致；HTTPS（回环测试例外）、禁止重定向、响应有界，service secret 只读环境变量。

桥接只撤销 OIDC 方法的会话、其应用票据和正在进行的 HTTP/WS/SSE。独立本地密码凭据保留，包括恢复管理员；如需整个账号离职停用，仍应禁用 Tunnel 成员，不能把 IdP 禁用当作本地密码注销。重新启用 IdP 用户只允许新的认证，旧 cookie 不复活；本地禁用也不会被自动反向启用。

## 检查与一致性

OIDC 回调在绑定成员匹配后同步读取用户 enabled；未知、删除（404）、禁用、网络/权限/解析错误均不新发 OIDC 会话。网络检查不持有访问锁，返回后在写锁内重新检查本地绑定/禁用，防止更改绑定期间的旧响应授权。相同身份并发请求合并，外部请求最多四个并发，service token 单独缓存并串行刷新。

后台只检查活跃的 OIDC 成员；每轮完成后等待 pollSeconds，再开始下一轮。用户量/IdP 延迟会增加周期，不能把配置间隔当作所有用户的硬截止时间。成功响应记录本机校验时刻；失败保留最后成功状态直到 maxStaleSeconds，超过后新请求直接拒绝，独立一秒扫描关闭已有流并撤销会话。扫描先取消流，再在访问写锁内删除会话，减少慢写事务对失效关闭的影响。

禁用/删除成功响应在同一写锁内更新状态并撤销 OIDC 会话。Cookie/设备、网关授权与长期连接共同受控；不是仅隐藏控制台按钮。撤销缓存不持久化，进程重启本来就不保留会话，新 OIDC 登录必须重新检查。短暂错误不自动放宽验证，也不无限延长旧状态。

## 可观测性与恢复

提供 enabled、checks/errors/revocations 计数和 identity_sync_session_revoked 审计（原因、会话数与本地用户 ID，无 service token/secret）。后台非活跃缓存定期清理。Secret 轮换通过本机配置管理和重启应用；不接受管理 API 传入任意 URL 或凭据。

默认 poll=15 秒、maxStale=60 秒；最小配置要求 maxStale >= 2*poll+5。应按实际活跃成员数/IdP RTT 设定并验收，四并发不足时会失效关闭，不能绕过检查保持可用。此桥接是轮询的一致性窗口，不是 IdP 管理事务与本地撤销的跨系统原子提交。

## 验收

race 测试覆盖禁用/删除、错误用户映射、状态接口故障与 TTL、重启旧 cookie 不复活的会话语义、独立密码恢复、服务 token 编码/缓存、并发合并、绑定更新期间的旧结果拒绝、禁止跨源重定向。真实 Keycloak 26.8.0 用单独只读 service client：OTP 登录、Agent HTTP/WS/SSE、标准 back-channel/RP 退出仍验证；另外只 PUT enabled=false、不调用 logout，证明桥接撤销控制台/应用 cookie 和 WS/SSE，恢复管理员仍可用，重新启用后旧 cookie 仍无效。
