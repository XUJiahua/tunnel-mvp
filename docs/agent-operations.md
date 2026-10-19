# Agent 实例与故障切换操作

1. 在“Agent 实例”选择站点，填写名称并登记。保存仅本次显示的 instanceID、siteID 和 token，随后隐藏凭据。
2. 为每台 Agent 分别创建配置文件（不要共享 instanceID/token）：

```json
{
  "serverURL": "https://console.example.com",
  "siteID": "从控制台复制",
  "instanceID": "从控制台复制",
  "allow": [{"cidr":"10.10.0.0/16","ports":[3000]}]
}
```

3. 使用受保护的服务环境文件设置 `TUNNEL_AGENT_TOKEN`，启动 `tunnel agent --config agent.json`。也可用 `--instance-id` / `TUNNEL_INSTANCE_ID` 指定实例 ID。`--check-config` 不显示令牌；更换 instanceID、siteID 或 serverURL 需重启进程。ACL 的有效修改仍支持 `--watch-config`。
4. 确认所有实例在线、能访问相同应用。控制台展示的“连接”是活动 TCP 通道，包含池中的空闲连接，不是浏览器人数或请求数。版本是客户端自报值。通过 `GET /api/sites/{siteID}/agents` 可取得相同清单。
5. 完成迁移后点击“禁用站点令牌兼容连接”。旧 site token 将不能建立隧道，已连接的兼容实例会被关闭；登记实例不受影响。保留兼容模式时，旧 Agent 也参与新通道分配。

## 故障演练

在两个实例均在线时打开应用、上传文件并建立 WS/SSE；记录实例连接数，停止其中一个进程。确认该实例离线、其已有流断开，新访问继续通过另一个实例。重启原进程后应恢复在线。不要把正在上传的业务请求自动重放，是否重试由应用决定。

如果仅一个实例出现 ACL 拒绝或网络错误，临时禁用该实例，修复本地配置和网络再启用。当前策略按隧道在线选取，不按各上游应用健康选取，监控探测结果不能证明每一个实例都可达。

## 凭据事件与恢复

- 单台泄漏：禁用或轮换该实例，只影响其连接；轮换后以新 token 重启该 Agent。删除登记会永久拒绝旧 token。
- 全站点事件：在站点执行原有令牌轮换。全部实例断开并进入“必须轮换凭据”；逐实例轮换、更新秘密并重启，不能直接启用绕过轮换。兼容入口是否禁用保持原设置。
- 恢复授权状态备份会恢复历史凭据和禁用状态。事件恢复后逐实例复查并轮换；运维归档不包含授权凭据，不能替代状态备份。

`GET /api/agents` 返回当前操作员可管理的组织实例；PATCH `/api/sites/{site}/agents/{instance}` 更新名称/disabled，POST `.../{instance}/rotate` 返回一次性新 token，DELETE 删除。站点 PATCH 支持 `legacyDisabled`。审计记录登记/更新/轮换/删除/连接/断开和通道归属，均不含 token。

Prometheus `tunnel_site_agents{site_id}` 为在线实例数，`tunnel_site_online` 表示至少一个在线。对期望冗余站点将实例数低于 2 作为降级告警，不能仅监控 online。

控制面重启、网络出口故障或单个上游停机仍可能影响全部实例；本功能只解决 Agent 进程/所在主机冗余。
