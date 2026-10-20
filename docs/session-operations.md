# 登录设备、统一身份退出与管理员恢复手册

## 管理登录设备

打开“登录设备”查看账号、登录方式、User-Agent、直连地址、创建/最近控制台活动/到期时间。普通成员只能看自己的设备；管理员可把“会话范围”切到“全部成员会话”。地址可能是反向代理的地址；不采信浏览器提交的 Forwarded/X-Forwarded-For。

点击“撤销会话”并确认，会立即撤销该设备控制台和应用访问并关闭其长连接。其他设备继续有效。撤销当前会话会回到登录页。成员禁用/改密/身份重新绑定仍撤销该账号的全部会话。

JSON/SQLite 列表来自当前 Server 内存，重启清空；PostgreSQL 列表来自共享认证表，正常重启保留未到期会话。恢复到新库或改变认证策略要求重新登录。最近活动时间最多每分钟更新，仅反映控制台鉴权，不是完整应用访问轨迹。

## 配置 Keycloak 退出通知

在对应 confidential client 设置：

| 设置 | 值 |
| --- | --- |
| Backchannel Logout URL | `https://console.example.com/auth/oidc/backchannel-logout` |
| Backchannel Logout Session Required | 开启 |
| Front Channel Logout | 本批不依赖，关闭 |
| Valid Post Logout Redirect URIs | 精确的 `https://console.example.com/` |

Keycloak 必须能访问该回调；不能仅保证用户浏览器可达。控制台按签名和声明验证通知，不要求浏览器 Cookie。IdP 主动注销某个 sid 时，只有该 OIDC 会话被撤销；sub-only 通知会撤销此身份的全部 OIDC 设备，本地密码会话保留。

紧急离职回收：在 tunnel 禁用成员，同时在 Keycloak 禁用用户并结束其现有会话。单纯把 Keycloak enabled 改为 false 不保证发送所有退出事件。本地“禁用账号”是立即收回 tunnel 全部入口的明确操作。

OIDC 登录后的顶部“退出统一身份”先退出当前 tunnel 会话，再打开 IdP 的退出页，可能需要确认。原侧栏“退出登录”仍只退出 tunnel。IdP 回调无法送达时，其他本地会话可继续到原到期时间；检查 IdP 日志及控制台 `oidc_backchannel_logout` 审计。

## 强制认证上下文

Server JSON/YAML 可选：

```json
"oidc": {
  "issuer": "https://sso.example.com/realms/tunnel",
  "clientID": "tunnel-console",
  "sessionMinutes": 60,
  "requiredACR": ["2"],
  "requiredAMR": ["otp"]
}
```

ACR 命中任一项即可，AMR 要求全部指定项。也支持 `--oidc-required-acr` / `--oidc-required-amr` 及 `TUNNEL_OIDC_REQUIRED_ACR` / `TUNNEL_OIDC_REQUIRED_AMR`，使用逗号分隔多值。实际值由 IdP 的认证流程和声明映射决定，示例中的 2/otp 不能直接视为所有 IdP 的统一配置。先在演练环境核对签名 ID Token 的可信声明，不把令牌上传公开解码工具。未配置/缺失声明时，已配置的策略会拒绝登录，不降级接受。

## 忘记管理员密码

1. 停止 Server，并禁止服务管理器自动拉起；用原有备份流程保留停机数据目录。
2. 在服务环境或临时安全输入中设置 `TUNNEL_RECOVERY_PASSWORD`，值为 12–72 字节。不要把真实密码写入命令行历史或仓库文件。
3. 执行：

   ```bash
   ./tunnel storage reset-admin-password -c server.json --username admin
   ```

4. 清除临时恢复密码环境，启动 Server；用新密码登录，确认旧密码失败，并验收原资源与 Agent 令牌。

命令要求已存在的 admin；不创建账号或提升成员。运行中返回 `data directory is in use` 时先正常停止，不能删除锁文件。恢复不改变其他成员、应用、授权与站点令牌。此工具需要可信目录的物理运维权限；不提供在线绕过管理员认证的入口。

## 运行真实 Keycloak 端到端验证

宿主机安装 Java 21，准备官方发行包（脚本固定版本及 SHA-256）：

```bash
bash scripts/prepare-keycloak-e2e.sh /tmp/tunnel-keycloak-e2e
make build
cd web
KEYCLOAK_HOME=/tmp/tunnel-keycloak-e2e/server pnpm exec playwright test tests/keycloak.spec.ts
TUNNEL_TEST_STORAGE=sqlite KEYCLOAK_HOME=/tmp/tunnel-keycloak-e2e/server pnpm exec playwright test tests/keycloak.spec.ts
```

设置 JAVA_HOME 指向 Java 21。脚本使用官方 Keycloak 26.8.0；临时 realm、客户端密钥、用户密码和 H2 数据库每次独立生成，成功后清理；失败保留诊断目录。仅用 loopback 开发端口 19880–19882，不接触现有 IdP。OTP 注册后会等待下一个时间步再二次登录，以符合提供方的一次性 OTP 规则。

测试覆盖真实 Discovery/JWKS、密码 + OTP、未绑定拒绝、本地应用授权、Agent HTTP/WS/SSE、IdP 管理员注销及通知撤销、RP 主动退出和 IdP SSO 会话终止。普通 `test:e2e` 未设置 KEYCLOAK_HOME 时该用例明确跳过；CI 另设 JSON/SQLite 两个真实 Keycloak 作业，不以跳过代替验收。

CI 失败先看对应 job 的 test-results 和 Keycloak 诊断目录。不要并行运行多个 Playwright 进程共用默认 test-results；如需独立并发，给每个进程设置不同 `--output` 并避开固定端口。

PostgreSQL 撤销事务提交后，新请求直接读取最新认证；已有应用流每秒重新证明，并受独立 5 秒授权租约限制。生产仍是单活 Server，跨节点 Agent 路由属于后续阶段。见 [共享认证设计](design/shared-authentication.md)。
