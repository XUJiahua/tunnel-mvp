# 应用迁移、批量操作、访问域名与路径目标

第八层依赖组织/角色批次，并补端到端测试发现的 Agent 关闭竞态与浏览器 CSRF 兼容问题。

## 资源变更

`PATCH /api/services/{id}` 支持可选 siteID 和 customDomain，名称/目标 URL 继续按原接口提供。迁移目标站点必须与应用同组织，调用者须有 applications:manage。组织归属不随迁移变化。保留应用 ID 及单应用授权；整站授权按新站点重新计算。URL、站点或访问域名变化都即时撤销应用票据/Cookie，并关闭 HTTP/WS/SSE、清理连接池和当前探测结果。仅改名称不撤销访问。

| 批量 API | 输入/原子性 |
| --- | --- |
| `POST /api/services/bulk` | services 列表，每项 name/siteID/url，可选 customDomain；一次创建 1–100 个 |
| `POST /api/services/bulk-move` | 唯一 ids 列表与 siteID；同组织迁移 |
| `POST /api/services/bulk-delete` | 唯一 ids 列表；删除并清理用户/组/邀请引用 |

每项先验证权限/引用/名称/目标/域名，任意项失败则整个状态事务回滚，不产生部分资源或部分迁移。成功提交后逐应用撤销和记录带 bulk 标记的审计。批量能力不能绕过组织边界。限制仍包括 JSON 请求体 64 KiB。

## 自定义域名

仅全局管理员可绑定或修改，组织应用管理员可继续编辑其应用和目标，但不能变更域名。状态验证要求小写 DNS 主机名且所有活动应用唯一，不允许 IP、协议、端口或路径。拒绝与控制台及内置访问域命名空间重叠；回环 HTTP 开发环境允许独立 `.localhost` 域名。

控制面生成的 launch URL 优先使用自定义域名，端口和 scheme 沿用 accessURL。网关先识别配置的主机名再进行原有应用 ID/票据/实时权限校验，不能由 Host 提交任意内网地址。内置 ID 域名仍代表同一应用；变更域名时先撤销全部旧票据/访问。应用 Cookie 保持 host-only。

DNS、证书和边缘反向代理由运维显式配置，不自动注册 DNS 或证明域名所有权；绑定只允许全局运维身份。更换域名后浏览器中的第三方 Cookie 不会被服务器远程清除。不要把旧域名重新分配给无关安全域的应用；先清理浏览器数据或使用新域名。同一应用迁移站点则保留应用自身 Cookie，网关会话必须重新签发。

启动时也校验已有映射，防止修改 console/access 配置后引入保留命名空间冲突。

## 路径模型

目标可以是 HTTP/HTTPS origin，或纯 ASCII 路径前缀，例如 `http://grafana.internal:3000/grafana/`。前缀补齐尾斜杠，仅支持字母、数字和 `-._~` 段，拒绝空段、点段、反斜杠、转义路径、凭据、query/fragment、保留 `/_tunnel` 前缀。控制台/Agent 的 server URL 仍只接受 origin。

公网路径与上游路径相同：`https://<app>/grafana/...` 转发为 `http://<target>/grafana/...`，不会重复拼接前缀。入口票据直接跳转至前缀；已登录应用根地址 GET/HEAD 可跳转至前缀，前缀外请求和规范化后越界路径拒绝，不把同源其他应用暴露给当前应用授权。双重编码点/分隔符等歧义路径也拒绝。

应用必须配置自己的子路径/外部 URL；绝对 `/assets`、`/login` 等跳出前缀的地址不会通用改写。保留上游 Cookie Path 与相同路径的 Location；同上游 host 的绝对重定向只替换公网 scheme/host。不重写 HTML/JS。

对已识别的同应用 Origin/Referer，将 scheme/host 替换为上游 origin，保留 Referer 路径；CORS 同源 origin 反向替换，origin 不包含挂载路径。跨源 Origin 不伪装为可信来源。应用响应使用 `Referrer-Policy: same-origin`，允许同应用表单正常提供 Origin/Referer，跨源仍不发送 Referer；控制台、一次性票据入口及错误响应保持 no-referrer，避免把入口票据转发给应用。所有代理仍剥离网关/控制台 Cookie。

## 状态与 Agent 关闭顺序

SQLite schema v5 增加 custom_domain 列，并提高 reader 版本以拒绝旧程序回写路径目标。backup v5 读取 v1–v5，旧省略字段维持 checksum 兼容；所有已知旧布局事务升级。域名唯一与资源组织引用由业务层校验。

真实迁移 race 测试发现 remotedialer 的便捷客户端在接收循环初始化 pingCancel/WaitGroup 时可能并发 Session.Close。Agent 改为显式拥有 WebSocket/Session：取消上下文、关闭底层 WebSocket、等待 Serve 退出，再关闭 Session。仍使用同一 remotedialer 协议和本地 DNS/ACL dialer，不修改依赖源码。100 次握手后立即取消的 race 测试覆盖该时序，不能用 sleep 或忽略 race 规避。

## 验证

双后端 URL/域名持久化、v4 升级、旧版本升级与恢复；真实 Agent 路径、重定向、Cookie/Origin/Referer/CORS、目录穿越拒绝；站点迁移授权语义、批量失败回滚、跨组织/成员/自定义域名越权拒绝。浏览器通过两个 Agent 验证批量创建、自定义 `.localhost`、挂载应用登录/静态脚本/1 MiB 上传/WS/SSE、批量迁移时主动关闭、重新进入与批量删除。
