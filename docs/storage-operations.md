# 存储迁移与备份恢复手册

本手册适用于包含存储批次代码的构建。默认后端仍是 JSON；当前 Release 中的旧二进制不一定包含这些命令。先执行 `tunnel storage --help` 确认。

当前资源批次使用 SQLite schema v5、backup v5，并继续读取旧 backup v1–v4。已有 SQLite v1–v4 首次被 Server、inspect 或 export 打开时会事务升级；**先用旧二进制导出并备份停机目录，再换程序**。失败不会留下部分 schema。回滚须使用升级前备份。详见 [OIDC 升级手册](oidc-operations.md)。

## 1. 选择后端

| 场景 | 操作 |
| --- | --- |
| 保持旧部署 | 不设置 storage，继续读取 `state.json` |
| 新 SQLite 部署 | 设置 `storage: sqlite`，使用没有状态文件的新目录 |
| 旧 JSON 切换 SQLite | 停止 Server，执行 migrate，再修改配置 |
| SQLite 切回 JSON | 导出当前状态，恢复到新的 JSON 目录，切换配置 |

配置可设置 `storage: json` / `storage: sqlite`，也支持 `TUNNEL_STORAGE` 或 `--storage`，优先级仍为 CLI > 环境变量 > 文件 > 默认值。数据目录使用原有 `dataDir` / `TUNNEL_DATA_DIR` / `--data-dir`。

例如：

```bash
./tunnel server -c examples/server.sqlite.json --check-config
./tunnel server -c examples/server.sqlite.json
```

新目录首次启动仍需要 `TUNNEL_ADMIN_PASSWORD`。已有数据的管理员密码不会被该变量重置。所有 SQLite 构建保持无 CGO、单二进制，运行时不需要系统 SQLite CLI 或 Node.js。

## 2. 从 JSON 迁移到 SQLite

维护窗口内操作；涉及站点隧道和应用长连接的短暂中断。

```bash
# 先通过服务管理器/Compose 停止 Server；不要停止后又让自动重启拉起旧进程。
mkdir -p backups

# 旧目录检查、导出，并验证备份
./tunnel storage inspect --data-dir data --storage json
./tunnel storage export --data-dir data --storage json --output backups/before-sqlite.backup.json
./tunnel storage validate --input backups/before-sqlite.backup.json

# 原 state.json 保留；目标 state.db 必须不存在
./tunnel storage migrate --data-dir data --from json --to sqlite
./tunnel storage inspect --data-dir data --storage sqlite
```

随后把实际 Server 配置改为 `"storage": "sqlite"`，保持原来的 `dataDir`、publicURL 和 accessURL，运行 `--check-config` 后启动。也可用 `--storage sqlite` 或环境变量覆盖。Compose 已支持可选的 `TUNNEL_STORAGE` 环境变量。

验收：管理员及成员均能用原密码登录；原站点 Agent 用原令牌自动重连；应用 ID/公网访问域名不变；单独应用授权仍无法访问兄弟应用；重新签发应用访问票据后打开服务。Server 重启必然清空内存会话，需要重新登录。

迁移后不要再启动 JSON 模式，程序会因存在 `state.db` 拒绝。保留的 `state.json` 是迁移时的旧副本，不会继续更新。migrate 重跑会拒绝覆盖，不能作为同步工具。

## 3. 日常导出

停止 Server 后：

```bash
./tunnel storage export -c server.json --output backups/current.backup.json
./tunnel storage validate --input backups/current.backup.json
```

`-c` 读取普通 Server JSON/YAML，storage 与 dataDir 参数仍可覆盖。命令成功后输出后端、schema 版本和资源数量，不把完整状态打印到终端。输出文件必须是新文件；同名文件已存在会失败。

备份文件权限 0600，包含密码散列、Agent 令牌散列、OIDC 身份/禁用状态和内部应用地址；不加密，也不包含明文 Agent 令牌。请单独安全保存 Server/Agent 配置、凭据环境、TLS/CA 和审计历史。仓库默认忽略 `backups/`，不要改名后提交到公开仓库。

JSON/SQLite 状态文件均可在停机后随整个数据目录备份。可移植 export 的优势是跨后端恢复及格式/校验和检查；不能替代审计日志归档。

## 4. 恢复到新目录

```bash
./tunnel storage validate --input backups/current.backup.json

# 选择要恢复成的后端；不需要初始管理员密码
./tunnel storage restore --input backups/current.backup.json --data-dir data-restored --storage sqlite
./tunnel storage inspect --data-dir data-restored --storage sqlite
```

将 Server 配置的 `dataDir` 改成恢复目录，storage 与恢复后端一致，检查配置后启动并完成上面的业务验收。原目录保留，以便排查。恢复拒绝含有 `state.json`、`state.db` 或 SQLite sidecar 的目标，不提供强制覆盖选项。

需要恢复为 JSON 时，把 restore 的 `--storage` 改为 json 即可。向旧二进制回滚时，还必须恢复匹配的旧版配置；旧版本可能拒绝新配置字段，也不具备应用授权等新能力。优先使用同一批次的二进制和备份。

会话、票据、探测快照不在备份中。Agent 注册数据保留，但 Agent 机器仍需拥有原令牌；令牌遗失时通过控制台轮换。

## 5. 错误处理

| 错误 | 含义与处理 |
| --- | --- |
| `data directory is in use` | 有新版本 Server 或维护命令持锁；先正常停止，勿删除锁文件 |
| `state.db exists; select --storage sqlite` | 已迁移或选错后端，核对配置；不要用旧 JSON 恢复运行 |
| `legacy state.json exists; run storage migrate` | 只改了后端选项，尚未迁移；按维护流程处理 |
| `already exists` / `refusing to overwrite` | 目标已存在；核对是否上次已成功，或使用新的备份文件/恢复目录 |
| `unsupported SQLite application/schema` | 错误数据库或当前程序不支持其版本；使用对应程序或有效备份恢复 |
| `backup checksum mismatch` | 备份被修改或损坏；选择完整可信副本，不手改校验和绕过 |
| `unknown ... grant` / `invalid ...` | 引用或数据格式无效；保持源文件，排查原始数据后再迁移 |
| `SQLite sidecar ... exists` | 发现旧 journal/WAL/SHM；停机确认旧库恢复状态，不要直接删掉重试 |
| `publish ... without overwrite` | 文件已存在、权限或硬链接支持问题；使用支持硬链接的本地磁盘 |

SQLite 正常采用 DELETE journal，事务完成后不应依赖 WAL 文件。发生异常停机后，优先用相同版本在原目录打开现有数据库，让 SQLite 按自身机制恢复；不要只拷贝一个仍在写入的数据库文件。

程序的文件锁只保护遵守协议的新版本命令，旧版 Server 不认识它。仍需确保旧服务已停机，也不要让多个主机共享该目录。SQLite、导出命令和目录锁都不构成控制面 HA。

第五批起 restore 会清空未使用邀请，避免旧备份复活已消费 token；已有组成员和授权保留，详见 [用户组手册](groups-operations.md)。

组织批次新增组织/角色与归属列（schema v4 / backup v4），v1–v3 事务升级；默认组织保持旧授权与校验和兼容。归属/角色业务约束见 [组织与角色设计](design/organizations-roles.md)。
