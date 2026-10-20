# 本地 remotedialer 并发补丁

固定上游版本 `github.com/rancher/remotedialer v0.6.2-0.20261014155149-1e594259f9fc`，源码复制至 third_party/remotedialer，通过 go.mod 本地 replace 使用。原 LICENSE、README、模块元数据和 metrics 子包保留，修改仅限 wsconn.go 的写 deadline 同步及相应回归测试。

原问题是读侧心跳回调并发修改 Gorilla 的 writeDeadline。现在每次数据写入在同一个写锁内设置截止时间；调用方给定 deadline 应用于底层 socket，未给定时沿用 60 秒 ping wait 作为有界写期限。排队写入取得锁后先检查上下文，到期不再迟发。Ping/Pong 只更新读期限，不与帧写入争用写状态。协议、公开 API、DNS/ACL 与依赖版本不变。

`make test` 显式执行嵌套模块 race/vet，并验证同时心跳/数据流和超时写不遗留后台写锁。应用完整端到端仍验证 Agent HTTP、上传、WS/SSE、重连与撤销。后续升级依赖时应比较这几处改动，只有上游包含等价修复并通过全部验证后才移除 replace。

源码补丁说明见 [PATCHES.md](../third_party/remotedialer/PATCHES.md)。发布压缩包随附 [原 Apache-2.0 许可](licenses/remotedialer-LICENSE)。本次补丁不等于生产安全审计，也不提供控制面 HA。
