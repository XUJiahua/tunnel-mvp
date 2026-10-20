# Agent 升级与回滚操作

## 发布准备

在离线签名环境创建密钥（不在 Agent 主机生成/分发私钥）：

```sh
tunnel generate-update-key --private-output release.key --public-output release.pub
```

文件均为 base64 Ed25519；私钥 64 字节、公钥 32 字节。命令不打印私钥且拒绝覆盖。通过可信配置管理把 release.pub 固定到主机；发布私钥保留离线。此工作流不自动更改任何 GitHub Secret 或发布标签。

使用现有 GoReleaser 流程构建并校验六平台发布包，然后从其构建目录准备原始可执行文件。升级下载使用 raw binary，不读取 tar/zip 内任意路径：

```sh
python3 scripts/prepare-agent-update.py \
  --version 0.2.0 --sequence 10 --expires-hours 24 \
  --base-url https://updates.example.com/agent/0.2.0 \
  --directory agent-update-0.2.0 \
  --artifact linux/amd64=dist/tunnel_linux_amd64_v1/tunnel \
  --artifact linux/arm64=dist/tunnel_linux_arm64_v8.0/tunnel \
  --artifact darwin/amd64=dist/tunnel_darwin_amd64_v1/tunnel \
  --artifact darwin/arm64=dist/tunnel_darwin_arm64_v8.0/tunnel \
  --artifact windows/amd64=dist/tunnel_windows_amd64_v1/tunnel.exe \
  --artifact windows/arm64=dist/tunnel_windows_arm64_v8.0/tunnel.exe

tunnel sign-update-manifest --input agent-update-0.2.0/manifest.json \
  --private-key release.key --output agent-update-0.2.0/signed.json
```

版本必须与 `tunnel version` 中的实际 main.version 完全相同（是否带 v 以构建值为准）。序号必须高于此前发布，不因撤回而复用。检查 manifest 的域名、平台、大小、摘要、失效时间后签名。仅上传六个 raw binary 与 signed.json；不上传 release.key。更新站点必须返回直接 HTTPS 200 文件，不依赖重定向或带令牌查询串。到期前刷新签名有效期；同序号更新有效期不会重复安装已运行版本。

## 主机启用

先把独立实例的 config 和 `TUNNEL_AGENT_TOKEN` 按 [登记手册](agent-operations.md) 配好，确认直接 Agent 可连接。用主机服务管理器运行：

```sh
tunnel agent supervise --config /etc/tunnel/agent.json \
  --update-dir /var/lib/tunnel-agent/update \
  --bootstrap-binary /opt/tunnel/tunnel \
  --release-public-key /etc/tunnel/release.pub \
  --manifest-url https://updates.example.com/agent/stable/signed.json \
  --minimum-sequence 9 --update-interval 1m --ready-timeout 30s
```

update-dir 必须为本账号的私有 0700 目录。bootstrap 只在初次初始化使用，后续摘要以 journal 为准；不是后续下载的信任来源。minimum-sequence 是当前引导版本的发布下限，禁止首次装机接受更旧序号。父进程不替换自身；服务管理器应启动固定的 supervisor 二进制，子 Agent 由其管理。

站点、实例和 allow 使用文件或环境变量；supervise 会拒绝这些 CLI 覆盖，避免以为它们被转发。需要本地 ACL 热更新时增加 `--watch-config`。所有子进程日志以 JSON 转发；敏感环境变量不写日志。服务管理器应管理整个进程组（systemd 使用 KillMode=control-group）；如强制杀死父进程，重启前确认旧子 Agent 已停止，避免重复实例连接。

观察日志 `Agent update healthy`、实例在线清单中的实际版本，以及业务 HTTP/上传/WS/SSE。每个站点先升级一个实例并验收，再推进其他主机。升级会关闭该实例的原连接，不能自动续传上传或迁移 WS/SSE。

## 失败和回滚

签名/过期/摘要错误：当前 Agent 继续运行；检查发布源，不能用关闭签名验证解决。候选无法启动、无法在 ready-timeout 内连接或上报版本不匹配：自动恢复上一已验证版本。已接受的失败序号不会重复安装，修复发布需新序号。

如需主动回退，先停止 supervisor，再执行：

```sh
tunnel agent rollback --update-dir /var/lib/tunnel-agent/update
```

重启同一 supervisor 命令，确认实例实际版本和应用正常。回滚保持最高序号，新发布须大于该值。目录锁冲突表示旧 supervisor 仍在运行。摘要不匹配或缺少经校验 previous 时命令会失败；保留目录用于排查，从可信离线备份修复，不能手改 journal 使未知二进制通过验证。

控制台不接收升级私钥，也不能远程修改主机的公钥和更新源。Linux 已有真实进程端到端验收；macOS/Windows 服务启停、文件替换权限和断电恢复需在目标主机单独验收后部署。
