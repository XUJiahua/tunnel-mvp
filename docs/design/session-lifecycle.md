# 会话设备、身份退出与管理员恢复

控制面仍为单实例。JSON/SQLite 的会话与退出重放窗口在内存；PostgreSQL 使用 [HA-2 共享认证](shared-authentication.md)，事务撤销与授权租约保证跨客户端一致性。

## 会话模型和权限

每次登录生成独立公开 session ID；Cookie 原值及其 SHA-256 索引不出现在会话列表中。会话记录用户 ID、认证方式、创建/到期时间、最多每分钟更新的最近控制台活动时间、512 字节内的 User-Agent，以及 TCP 直连 IP。转发头不能伪造设备地址；反向代理后的 IP 因此可能是代理地址。设备名和地址仅供识别，不是额外认证因子。

OIDC 会话还在服务端认证存储保留 Issuer、Subject 和 IdP sid，用于匹配退出通知；不保留原始 ID Token、Access Token 或客户端密钥副本以外的业务令牌。认证运行记录不进入 portable backup。

| API | 权限和语义 |
| --- | --- |
| `GET /api/me/sessions` | 仅自己的未到期会话，标记当前会话 |
| `DELETE /api/me/sessions/{id}` | 只能撤销自己的会话；他人 ID 返回 404 |
| `GET /api/sessions?userID=...` | 仅本地管理员，可选按用户筛选 |
| `DELETE /api/sessions/{id}` | 仅本地管理员，可撤销指定设备 |

撤销在访问写锁下重新检查调用者身份，然后删除对应会话及其票据/应用凭据，并取消该会话的 HTTP/WS/SSE。其他设备的会话继续有效。撤销自身返回 `reauthenticate=true` 并清除控制台 Cookie。会话事件包含公开 session ID，禁止记录 Cookie 或其索引。

## OIDC back-channel logout

`POST /auth/oidc/backchannel-logout` 接收 `application/x-www-form-urlencoded` 的单个 `logout_token`，请求体上限 64 KiB。它不要求浏览器 Origin 或 Cookie；其授权依据是 IdP 签名及完整声明验证。

1. 使用原 Discovery/JWKS 验证签名算法、Issuer、Audience 和 Expiry，不接受无签名 JWT。
2. 要求 iat 在过去 5 分钟至未来 30 秒范围，jti 非空，sid/sub 至少一个存在。
3. `events` 必须包含标准 backchannel-logout 事件且事件值为 JSON 对象；nonce 即使为空/null 也拒绝。
4. sid 存在时只匹配同 Issuer 的对应 OIDC 会话，sub 也存在则同时匹配；仅 sub 时撤销该 Subject 的全部 OIDC 设备。本地密码会话不受 IdP 退出影响。
5. jti 重放缓存有 4096 条上限，窗口内重复有效通知返回幂等成功，不再影响后续登录。容量用尽返回 503，避免静默丢失撤销。
6. 对 sid/sub 保留 6 分钟的退出时间界线；注销前已发起、但尚未创建本地会话的授权流不能在通知之后重新创建会话。新发起的认证可恢复登录。

会话签发、代理请求注册及退出处理使用同一访问锁排序。所有验证失败返回通用 400；有效退出包括目标已不存在时返回 200/no-store。审计只记录事件及撤销数量，不写入完整退出令牌。

该能力依赖 IdP 能访问控制台固定公网回调地址，以及 IdP 实际发送通知。Keycloak 的用户禁用与会话注销不是同一个操作：需要同时结束其 IdP 会话，或在 tunnel 本地禁用成员，不能把“配置了 back-channel”当成所有禁用事件都已自动同步。

## RP 主动退出与 MFA 声明

OIDC 会话可显式调用 `POST /api/logout/identity`。先撤销当前 tunnel 会话，再返回 Discovery 的 end_session_endpoint URL，参数固定为配置中的 client_id 和 publicURL 根路径。客户端密钥和原始 ID Token 不进入 URL；IdP 可以要求用户确认退出。该按钮仅在当前为 OIDC 会话且提供方声明退出端点时显示。原“退出登录”仍只结束当前 tunnel 会话。

配置 `oidc.requiredACR` 后要求令牌 acr 精确命中任一配置值，并在授权请求携带 acr_values。配置 `oidc.requiredAMR` 后要求所有指定认证方法都出现在令牌 amr 中。未配置时保持兼容。不能从显示名、email、role 或单纯登录成功推断 MFA；必须在 IdP 正确配置认证上下文映射和流程。

## 离线管理员密码恢复

`storage reset-admin-password --username ...` 只重置已存在的 admin，密码从 `TUNNEL_RECOVERY_PASSWORD` 获取，限 12–72 字节。不接受命令行/配置文件密码，不创建账号，不提升成员角色，不修改资源或授权。恢复沿用数据目录独占锁，Server 未停止时拒绝执行。不存在的状态/管理员、损坏数据及非法密码仍拒绝。物理运维权限等价于对该目录的管理权限，不能用此工具恢复不可信备份。

恢复成功后须启动 Server 并用新密码登录，旧密码失败；已有进程不能与离线修改并行。离线改密与配置变更同事务撤销该管理员的旧会话；JSON/SQLite 重启也会清空内存会话。该操作与其他离线存储命令一样不依赖在线审计进程，执行结果输出不含凭据。

## 验证

- 双后端 Go race 覆盖设备所有权、管理员逐会话撤销、自己退出、其他设备保留、真实 Agent 的 WS/SSE 关闭，以及恢复锁/引用保持。
- 签名协议测试覆盖退出令牌签名/Issuer/Audience/iat/exp/jti/events/nonce、sid/sub 范围、重放和回调并发；MFA 声明拒绝降级。
- Playwright 从控制台完成设备撤销和停机 CLI 恢复，并重新登录验收。
- 独立 Keycloak 26.8.0 + Java 21 进程完成真实 OTP 注册、密码 + OTP 二次登录、单应用授权、Agent 访问、IdP 管理员注销、RP 主动退出，以及 HTTP/WS/SSE 立即撤销；JSON 与 SQLite 各跑一遍。无需 Docker。

参考：[OIDC Back-Channel Logout 1.0](https://openid.net/specs/openid-connect-backchannel-1_0.html)、[Keycloak 管理指南](https://www.keycloak.org/docs/26.8.0/server_admin/)。

PostgreSQL 的设备管理、注销和配置写入在同一数据库事务重新鉴权，跨节点已有流由最长 5 秒授权租约约束；正常重启保留未到期设备和待回调，改变认证策略或 portable restore 会清空它们。详见共享认证设计。
