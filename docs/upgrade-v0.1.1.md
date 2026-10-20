# v0.1.1 升级、验收与回退

适用源版本：已发布 v0.1.1（commit `020224911d5fdff6e007ca6811371d55939e0076`）。该版本只有 JSON，没有 `storage` 子命令、数据目录锁或组织模型。本文目标为包含 #1–#22 及本批收敛改动的单实例构建；不代表它已经发布。

## 1. 升级前记录

记录实际二进制的 `tunnel version`、校验和、服务启动命令、配置路径、dataDir、publicURL/accessURL、TLS/CA 和服务管理器重启策略。配置及凭据环境使用原有安全备份方式保存，不把秘密粘贴到终端记录或工单。

准备维护窗口。Server 重启后控制台/应用会话失效，HTTP/WS/SSE 中断；Agent 会重连，但正在进行的上传和 POST 不自动重放。新版本保留旧 ID、密码散列、站点令牌散列及整站授权，旧资源进入默认组织。

## 2. 停机并保留原件

先通过实际服务管理器停止旧 Server，关闭自动重启，确认进程已退出。v0.1.1 不遵守新文件锁，不能用新命令的锁检查证明旧 Server 已停止。

以下示例假定当前目录包含旧 `tunnel`、`server.json` 和 `data/`；路径必须按真实部署替换。目标目录必须不存在，备份保留在本地磁盘：

```bash
mkdir -m 700 backups/v0.1.1-before-upgrade
cp -a data backups/v0.1.1-before-upgrade/data
cp -p server.json backups/v0.1.1-before-upgrade/server.json
cp -p tunnel backups/v0.1.1-before-upgrade/tunnel
```

确认复制成功并单独保管凭据、反向代理/TLS/CA 配置。不要在 v0.1.1 上运行 storage export；也不要只复制运行中的 state.json。已使用中间批次 SQLite 的部署，应先用对应旧程序导出授权和运维配对备份，流程见 [存储](storage-operations.md)及[监控](monitoring-operations.md)。

## 3. 先验收 JSON，再选择迁移

使用已校验的新二进制（下例 `./tunnel-next`），先检查原配置并保持原公网入口：

```bash
./tunnel-next version
./tunnel-next server -c server.json --check-config
./tunnel-next storage inspect -c server.json --storage json
./tunnel-next server -c server.json --storage json
```

inspect 不是备份，也不能替代停机。新版本严格校验数据；损坏引用或错误格式会拒绝启动，不会初始化空库。先保留现场排查，不手改校验和、删除旧资源或重建目录。

依次确认：原管理员和成员密码可登录；原 Agent 以原站点令牌上线；资源 ID/域名不变；有整站授权者可访问，未授权成员仍被拒绝；旧控制台和应用 Cookie 不复活。用实际应用检查上传和 WS/SSE，再进行编辑、授权收回及健康探测。

如选择 SQLite，再次停止新 Server，并使用不存在的备份文件名：

```bash
./tunnel-next storage export -c server.json --storage json --output backups/current-json.backup.json
./tunnel-next storage validate --input backups/current-json.backup.json
./tunnel-next storage export-operations -c server.json --output backups/current-json.operations.tar.gz
./tunnel-next storage validate-operations --input backups/current-json.operations.tar.gz
./tunnel-next storage migrate -c server.json --from json --to sqlite
./tunnel-next storage inspect -c server.json --storage sqlite
./tunnel-next server -c server.json --storage sqlite --check-config
./tunnel-next server -c server.json --storage sqlite
```

同时将真实服务配置持久修改为 storage=sqlite，避免下次启动用错后端。迁移保留原 JSON 字节，但它不再更新；存在 state.db 时新 JSON 模式拒绝启动。重复 migrate 拒绝覆盖。再次执行上述权限和应用验收。

## 4. Agent 与身份逐步升级

先保留兼容站点令牌验证旧 Agent 重连，再逐台换新二进制和独立 instanceID/token，所有实例验证通过后才关闭兼容入口。详见 [Agent 操作](agent-operations.md)。新远程策略和签名升级均需主机显式配置，不会因升级 Server 自动开启。

先验证本地恢复管理员，再启用 OIDC、Keycloak 状态同步或角色委派；不要把身份系统变更与首次二进制升级同时进行。OIDC 实测步骤见 [接入手册](oidc-operations.md)。

## 5. 当前版本备份恢复

停机导出当前授权与运维归档，分别 validate，按 [监控归档](monitoring-operations.md)恢复到新的空目录。支持 JSON/SQLite 交叉恢复，但不是降级到 v0.1.1。恢复会清空 pending 邀请；账号、授权和凭据恢复到备份时刻，安全事件后的历史凭据须重新核对和轮换。

## 6. 回退到 v0.1.1

回退会丢失升级后的配置和权限变更，以升级前备份时刻为恢复点。若这些变更必须保留，保持新版本停机并先排查，不用旧程序尝试“兼容写入”。

1. 停止新 Server、关闭自动重启，确认没有任何新旧进程使用目录。
2. 保留失败现场。将当前 data 目录改名移走，完整复制升级前 data 到原路径；不要把新旧文件混合。
3. 恢复升级前二进制、配置和凭据环境。旧配置不能包含 storage/oidc 等新版字段。
4. 启动一个旧 Server，重新登录并检查 Agent、应用和未授权拒绝。

v0.1.1 的 JSON 读取较宽松，可能忽略新字段并在后续写入时丢失新版权限数据；因此“旧程序可以启动”不能证明安全回退。禁止让旧程序打开新版 state.json、state.db 或 v10 export。

## 7. 自动复现

先下载官方 v0.1.1 Linux amd64 包并对照同 Release checksums.txt；解出旧二进制到独立路径。构建新版本后运行：

```bash
python3 scripts/verify-upgrade.py \
  --old-binary /absolute/path/to/v0.1.1/tunnel \
  --new-binary /absolute/path/to/current/tunnel \
  --output /absolute/path/to/fresh-upgrade-report.json
```

脚本验证旧版本号及发布 commit；只使用临时目录/回环随机端口/一次性测试账号。真正运行旧 Server 和旧 Agent，经新 JSON/SQLite、跨后端配对恢复，最后恢复原目录并启动旧 Server。检查原 ID/散列/授权、旧会话失效、未授权拒绝和离线锁；临时秘密与日志在退出时删除，报告不含秘密。它验证升级链路，不替代公网 TLS、真实应用、Keycloak 或目标操作系统验收。
