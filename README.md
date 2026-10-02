# 极简 VPS 探针

打开网页，几秒钟内知道自己的 VPS 现在是否正常。

- **Server**：一个 Go 二进制 + SQLite（WAL），无 Redis、无消息队列、无外部数据库。
- **Agent**：一个 Go 二进制，只读 `/proc` 与文件系统统计，不监听任何端口，不执行任何远程命令。
- **前端**：原生 HTML/CSS/JS，随二进制内嵌；无 CDN、无外链、无埋点、无遥测。

规模与依赖（可自行核对）：Go 约 1.67 万行 / 86 个文件（含 39 个测试文件），前端约 2000 行，
**直接依赖只有 3 个**（`modernc.org/sqlite`、`coder/websocket`、`golang.org/x/crypto/argon2`），
`CGO_ENABLED=0` 静态单文件。

设计基线与取舍见 [`docs/DESIGN.md`](docs/DESIGN.md)，Agent ↔ Server 协议见 [`docs/PROTOCOL.md`](docs/PROTOCOL.md)。

## 当前状态（诚实版）

| 阶段 | 内容 | 状态 |
|---|---|---|
| 1 | 需求 / 架构 / 数据库 / 协议设计 | ✅ 已冻结（`docs/`） |
| 2 | Server 骨架：参数、日志、DB 迁移、`/healthz`、内嵌静态页、Makefile | ✅ 已完成 |
| 3 | Agent 采集器（`/proc`、网卡探测、磁盘、流量 checkpoint）+ `--print-json` 自检 | ✅ 已完成 |
| 4 | 协议、Token 鉴权、1 秒实时上报链路、断线重连 | ✅ 已完成 |
| 5 | 登录/初始化 + 首页 + SSE 实时 + 新增节点 | ✅ 已完成 |
| 6 | 节点详情 + 六档历史图表（内存聚合 → 10s/1m 落库 → 读时降采样 → 自研 canvas 图） | ✅ 已完成 |
| 7 | 流量统计（Checkpoint + 幂等增量 + 原子落盘 + 月计费周期额度） | ✅ 已完成 |
| 8 | 告警（离线 / 恢复 / 流量 / 到期 → Telegram + 日志） | ✅ 已完成 |
| 9 | 后台管理（节点编辑 / 删除 / Token 轮换、设置页、操作记录） | ✅ 已完成 |
| 10 | 安全审计（越权、CSRF、XSS、暴力登录、注入、资源上限、依赖复核） | ✅ 已完成（见 [docs/SECURITY.md](docs/SECURITY.md)） |
| 11 | 性能验证（50 个 Agent、内存/CPU/写库频率、SSE 推送量） | ✅ 已完成（见 [docs/PERFORMANCE.md](docs/PERFORMANCE.md)） |
| 12 | 安装 / 升级 / 卸载脚本 + systemd 加固单元 | ✅ 已完成（见 [deploy/](deploy/)） |
| 13 | 端到端 + 长跑测试（含硬杀恢复） | ✅ 已完成 |

**尚未实现的功能不会出现在页面上，也不会用假数据填充。**

目前可用的是「端到端跑通」的这一条链路：初始化管理员 → 新增节点 → Agent 上报 → 首页每秒实时刷新 →
点卡片进详情页看六档历史曲线与近 7 天流量 → 离线/恢复/流量/到期告警推到 Telegram 或日志 →
编辑/删除/换 Token/告警参数/操作记录都在界面里 → 三条命令装到 VPS 上（systemd 加固单元）。
v1 明确不做的功能见 `docs/DESIGN.md` §2（Web SSH、远程命令、Docker 管理、多用户/RBAC、公开状态页等）。

## 安装到服务器（三条命令）

```bash
# 服务端（VPS 上，root）
curl -fsSLO https://你的发布地址/probe-server-linux-amd64
curl -fsSLO https://你的发布地址/SHA256SUMS
sh install.sh server --file ./probe-server-linux-amd64 \
   --sha256 "$(grep probe-server-linux-amd64 SHA256SUMS | awk '{print $1}')"

# Agent（被监控的 VPS 上，root）：Token 从面板「新增节点」里复制（只显示一次）
sh install.sh agent --file ./probe-agent-linux-amd64 --sha256 <哈希> \
   --server https://monitor.example.com --token pba_xxx
```

脚本做的事很少且可重复执行（**升级就是再跑一次**）：建专用非 root 用户、放二进制到 `/usr/local/bin`、
准备数据目录（0750）、写 systemd 单元（`ProtectSystem=strict`、`NoNewPrivileges`、空 `CapabilityBoundingSet` 等一整套加固）、
启动服务。卸载：`sh install-server.sh --uninstall`（默认保留数据，加 `--purge` 才删）。

参数与安全说明见 [`deploy/README.md`](deploy/README.md)；**从零到跑起来的完整步骤（含反代与避坑清单）见 [`docs/DEPLOY.md`](docs/DEPLOY.md)**。

## 快速开始（现在就能用）

**服务端**（本机或一台 VPS，默认只监听 127.0.0.1）：

```bash
go run ./cmd/probe-server --data-dir ./data
# 启动日志里会打印一次性初始化码：
#   level=WARN msg=尚未初始化管理员... setup_code=xxxxxxxxxxxx
```

浏览器打开 `http://127.0.0.1:25774/`：

1. 输入日志里的初始化码 + 用户名 + 密码（至少 10 位）→ 完成初始化并登录；
2. 点右上角「新增节点」→ 填名称 → 创建后**立刻保存页面上的 Token**（只显示这一次）；
3. 首页会每秒刷新，节点卡片显示状态、CPU/内存/磁盘、实时上下行、本周期流量、延迟、Uptime；
4. 点卡片进详情页：左边是完整信息表（含 24h/7d 可用率、今日/本周期/累计流量与计费周期），右边是 CPU / 内存 / 磁盘 / 网络 / 延迟五张图（右上角切换 `1h 6h 12h 1d 3d 7d`）外加一张近 7 天流量图；
5. 点右上角「设置」填 Telegram 的 Bot Token 与 Chat ID → 「发送测试」确认能收到 → 之后离线、恢复、流量超限、到期都会推送（没配 Telegram 时告警仍然会写进服务端日志）；同一个对话框里还能改告警参数、查看只读的服务器信息；
6. 详情页右上角可以「编辑 / 换 Token / 删除」（删除会连历史数据一起删，有二次确认）；「设置 → 操作记录」能看到谁在什么时候做了什么（登录失败也会记一笔）。

**Agent**（Linux）：

```bash
# Token 放文件里，避免出现在 ps 输出中
install -m 600 /dev/null /etc/probe-agent/token
printf '%s\n' 'pba_你的Token' > /etc/probe-agent/token

probe-agent --server https://monitor.example.com --token-file /etc/probe-agent/token
# 先用 --once 验证 Token 与网络是否通：
probe-agent --server https://monitor.example.com --token-file /etc/probe-agent/token --once
```

## 验证前端 JS（没有浏览器时的自动检查）

前端没有构建步骤，随二进制发布。`go test ./internal/server/` 里有几条护栏：
引用的元素 id 必须在 `index.html` 里存在、禁止使用 `innerHTML`/`eval`、页面里不允许出现任何外链。
此外可以用 Node 做语法检查：

```bash
node --check web/app.js
```

## 构建与运行

需要 Go 1.22 以上（开发时使用的是 Go 1.27）。

```bash
go build ./...                       # 编译全部
go test ./...                        # 单元测试
go vet ./...                         # 静态检查

go run ./cmd/probe-server --data-dir ./data
# 默认监听 127.0.0.1:25774，浏览器打开 http://127.0.0.1:25774/
curl -s http://127.0.0.1:25774/healthz    # {"ok":true,"db":"ok"}（免鉴权，但匿名拿不到版本）

# Agent 自检：不需要 Linux、不需要 Server，对着仓库里的 /proc 快照跑一遍
go run ./cmd/probe-agent --print-json --root internal/agent/testdata/root
go run ./cmd/probe-agent --print-json --root internal/agent/testdata/root --samples 3 --interval 1s
```

有 `make` 的环境可用：

```bash
make check          # vet + test
make build          # dist/probe-server
make build-linux    # 交叉编译 linux/amd64 与 linux/arm64（CGO_ENABLED=0）
make release        # 交叉编译 + SHA256SUMS
```

`go test -race ./...` 需要可用的 CGO 工具链（gcc）。Windows 上需要 64 位 MinGW；本机当前只有 32 位 gcc，因此竞态检测在 Linux（部署机或 CI）上跑。`go vet`、`gofmt`、普通单元测试在本机全绿。

日志消息是 UTF-8 中文；在 `LANG=C` 的容器里请用 `journalctl`/`docker logs` 查看（它们按 UTF-8 渲染），不要用只看字节的老终端。

## 已验证的内容

| 验证项 | 方式 | 结果 |
|---|---|---|
| 构建 | `go build ./...` | ✅ |
| 静态检查 | `gofmt -l .`、`go vet ./...` | ✅ 无输出 |
| 单元测试 | `go test ./...` | ✅ 全绿（config / store / server / agent） |
| Server 真实启动 + 接口 | 启动进程后请求 `/healthz`、`/`、`/style.css`、`/api/nodes` | ✅ 200/200/200/404(JSON) |
| DB 迁移可重入 | 硬杀进程后重启，`schema_version` 仍为 1，无报错 | ✅ |
| 优雅退出 | 真实监听端口 → 收到取消 → 干净退出 → 端口释放 | ✅ |
| Agent 采集正确性 | 对着提交进仓库的 `/proc` 快照实跑，逐项核对内存/swap/负载/uptime/网卡计数 | ✅ |
| Agent 速率与增量 | 两份快照差 10s：CPU 25%、rx 1 MiB/s、tx 0.5 MiB/s、累计值精确 | ✅ |
| 流量 checkpoint | 落盘 → 重新加载 → 累计值延续、不重复计；权限 0600；坏文件按新基线处理 | ✅ |
| 重启不清零 | `boot_id` 变化 / ifindex 变化 / MAC 变化 / 计数器清零 / 32 位回绕 | ✅ 五种情况各有用例 |
| 非 Linux 拒绝运行 | Windows 上直接运行 `probe-agent` 报错退出，提示用 `--print-json` | ✅ |
| 协议与鉴权（真实 TCP） | hello/welcome 握手、错误 Token 401、停用节点 403、版本不兼容 4426、非 hello 首帧 4400、hello 超时、二进制帧拒绝 | ✅ 全部有用例 |
| 实时链路（真实 TCP） | 1 秒上报进入内存状态、ping/pong 原样回传、未知类型回错误帧但不断连、超大帧 1009、连续非法帧 4400 | ✅ |
| 限流与背压 | 每连接 5 msg/s，超出丢弃但不断连；下一个窗口恢复正常 | ✅ |
| 端到端（两个二进制代码路径） | 真实 socket + 真实 SQLite：Agent 上报 → 服务端内存状态；**服务端整体重启后 Agent 自动重连**（同地址同数据库）；10 个 Agent 并发同时在线 | ✅ `internal/e2e` |
| 真实二进制路由 | `probe-server.exe` 启动后：`/healthz` 200、Agent 接口无 Token 401（带 `WWW-Authenticate`）、伪造 Token 401、未知 API 路径 JSON 404 | ✅ |
| 登录与初始化（真实 socket） | 初始化码错误 403、过期失效、重复初始化 409、弱密码 400、登录错误 401、限流 429（带 `Retry-After`）、登出后 401、会话过期 401 | ✅ 全部有用例 |
| CSRF / 同源（真实二进制 + curl） | 无 CSRF 403、错误 CSRF 403、跨站 Origin 403、正确 CSRF 201；Cookie 属性 `HttpOnly; SameSite=Lax; Path=/` | ✅ |
| 首页实时链路（真实 socket） | 从日志读初始化码 → 初始化 → 建节点拿 Token → 起真 Agent → SSE 收到该节点的实时数据（内存 51.77%、`eth0`、uptime 全部核对） | ✅ `internal/e2e` |
| 变更集推送 | 只推变化的节点；**没有新数据但状态随时间变化（在线→离线）也会推**；汇总始终是全量 | ✅ |
| 慢客户端保护 | 每个 SSE 客户端只有 1 个"最新值槽"，卡住的浏览器不影响别人、内存不增长（1000 次广播不被阻塞） | ✅ |
| 敏感字段 | 节点列表响应里不出现 `token` / `token_hash` / `pba_` 前缀 | ✅ |
| 前端护栏 | `app.js` 引用的元素 id 必须存在；禁止 `innerHTML`/`eval`；页面零外链；`node --check` 语法通过（app.js + chart.js） | ✅ |
| 六档参数 | 桶宽/源表/点数由规则推导，与 `docs/DESIGN.md` §10.3 定稿表**逐项一致**；任何档位 ≤1000 点 | ✅ |
| 聚合与 rollup | 10 秒桶 avg/max/up/all 正确；rollup 幂等、只聚合已封闭的桶；重复执行行数不变 | ✅ |
| 保留清理 | 按节点逐条删除（走主键索引），只删过期数据；`--retention-*` 生效 | ✅ |
| 重启恢复 | `node_runtime` 每分钟落盘；服务端重启后恢复最后状态，且**一律显示为未连接**（离线就是离线） | ✅ |
| 历史链路端到端 | 真 Agent → 内存聚合 → 落盘 → `series` 接口返回 `[ts,avg,max]`，数值与 `/proc` 快照一致；六档全部可查 | ✅ `internal/e2e` |
| 流量记账端到端 | 真实 WebSocket 帧（累计值递增）→ 内存增量 → 原子落盘 → 首页/详情接口显示今日/本周期/累计与额度百分比 | ✅ |
| 流量幂等性 | 重复帧算 0；跳帧按差值补齐；**首帧只建基线**（绝不把"安装以来的累计值"算成本月流量） | ✅ |
| 流量故障矩阵 | Agent 重装（值变小）→ 重设基线加 0；单次差值超上限 → 重设 + WARN；服务端重启 → 基线来自 DB，空窗流量补齐且不重复 | ✅ |
| 计费周期 | 重置日 19 号 / 1 号 / 31 号（2 月钳到 28、闰年 29）；跨年；时区（UTC 时刻落在上海的下一天） | ✅ 9 个子用例 |
| 额度使用率 | 周期用量 / 额度，写进节点视图（首页进度条与详情页都用它） | ✅ |
| 告警规则 | 离线（去抖 2s + 冷却重发）、恢复（需稳定 30s）、流量预警/超额（每周期一次）、到期（1/3/7 天递进） | ✅ 12 个用例 |
| 告警不该发的时候 | 启动静默期内不发、冷却期内不重发、刚上线不算恢复、没配到期日不提醒、同周期不重复提醒 | ✅ |
| 告警流水线 | 3 秒内多节点事件合并成一条；失败重试 3 次（退避）；队列满丢弃并计数（不阻塞采集链路）；尊重 Telegram `retry_after` | ✅ |
| 告警端到端 | 真实断开 → 状态机判离线 → 规则引擎 → 通知流水线 → 通知器收到；重连稳定后收到「已恢复」，且 `alert_state` 落盘为 firing/resolved | ✅ |
| 通知安全 | Telegram 地址硬编码（无 SSRF）；Token 只存库、永不回显；错误信息脱敏（有用例守着） | ✅ |
| 节点编辑 | 整体替换语义（缺字段明确 400，不会悄悄改成默认值）；重名 409；停用后 Agent 连不上；改配置会重置该节点的告警状态 | ✅ |
| 删除节点 | 历史桶、日流量、告警状态、内存状态、流量基线全部清理；旧 Token 立刻失效；重复删除 404 | ✅ |
| 换 Token | 新 Token 只回一次；旧连接被服务端断开；旧 Token 连不上、新 Token 可用 | ✅ |
| 操作记录 | 新增/修改/删除/换 Token/通知设置/告警参数/初始化/登录/登录失败/退出全部落库；时间倒序 + `before_id` 翻页；最多 2000 条 | ✅ |
| 告警参数热更新 | 改冷却/静默期/去抖/恢复确认立刻生效，且**不会让已触发的告警重新通知一遍** | ✅ |
| 前端护栏（补充） | `api()` 第二个参数必须是 options 对象（写成字符串会静默退化成 GET，有用例禁止） | ✅ |
| 第二轮全量走查 | 11 个单元并行审查 + 对抗式复核 → 确认 38 条、修掉 11 类真问题（数据正确性 6 条、安全 5 条），每条都有回归用例 | ✅ 见 `docs/SECURITY.md` §3.1、`docs/PERFORMANCE.md` |
| 安全（Phase 10） | 跨源一律拒绝（含 GET）、改密会踢掉其它设备、SSE 有连接上限；日志/接口不泄露密码与 Token；无外部命令、无 SQL 拼接 | ✅ 19 个用例 + `govulncheck` 零命中（见 `docs/SECURITY.md`） |
| 性能（Phase 11） | 50 节点 @1Hz 真实连接：**稳态 RSS 37–39 MB**、CPU 低于计数器分辨率、写库 6 次/分钟、零丢帧；Agent 采样 108 µs/次 | ✅ 见 `docs/PERFORMANCE.md` |
| 负载护栏（Phase 11） | 50 条连接 × 4 Hz（约 200 帧/秒）跑 6 秒：处理 99.9%、无序号跳变、goroutine 不增长、列表接口 1 ms | ✅ `TestFiftyAgentsLoad` |
| 部署脚本（Phase 12） | LF 换行、`set -eu`、systemd 全套加固项、Token 不进命令行、下载必须校验 SHA256、卸载默认保留数据 | ✅ `deploy/deploy_test.go` |
| 长跑与恢复（Phase 13） | 50 节点 10 分钟：RSS/库增长平稳、无 ERROR；**硬杀后重启**：节点/历史/流量/审计全部完好 | ✅ 见下方"长跑结论" |
| 交叉编译 | `CGO_ENABLED=0 GOOS=linux` amd64 / arm64（Server 12.9 MB、Agent 6.9 MB 单文件，附 SHA256SUMS） | ✅ |
| 竞态检测 | `go test -race` | ⚠️ 本机缺 64 位 gcc，留到 Linux 跑（`make race`） |

## 参数（probe-server）

| 参数 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `--listen` | `PROBE_LISTEN` | `127.0.0.1:25774` | 监听地址；**默认只对本机开放** |
| `--data-dir` | `PROBE_DATA_DIR` | `data` | 数据目录（SQLite 文件放这里） |
| `--tls-cert` / `--tls-key` | `PROBE_TLS_CERT` / `PROBE_TLS_KEY` | 空 | 直接提供 TLS；两者必须同时给出 |
| `--log-level` | `PROBE_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `--log-format` | `PROBE_LOG_FORMAT` | `text` | `text`/`json` |
| `--timezone` | `PROBE_TIMEZONE` | `Local` | 日流量与日期归属的时区 |
| `--trusted-proxy` | `PROBE_TRUSTED_PROXY` | 空 | 可信反向代理 CIDR（逗号分隔）；只有来自这些地址的请求才采信 `X-Forwarded-For` |
| `--setup-code-ttl` | — | `30m` | 首次初始化码的有效期（1 分钟 – 24 小时） |
| `--retention-10s` | — | `12h` | 10 秒桶保留时长（0 = 不清理） |
| `--retention-1m` | — | `192h` | 1 分钟桶保留时长（0 = 不清理） |
| `--flush-interval` | — | `10s` | 内存聚合落盘周期（1s–60s）；调大能减少写库次数，但历史出现得更晚 |
| `--traffic-delta-max` | — | `1TiB` | 单次接受的流量增量上限（超过即重设基线，防"凭空多几 TB"） |
| `--alert-cooldown` | — | `30m` | 同一告警重复通知的最短间隔（节点仍异常时） |
| `--alert-startup-grace` | — | `60s` | 服务端启动后的静默期（只记状态不发通知，防重启轰炸） |
| `--alert-debounce` | — | `2s` | 连续离线多久才判定为真离线（抗抖动） |
| `--alert-recover-stable` | — | `30s` | 恢复后需稳定在线多久才发「已恢复」 |
| `--stale-after` | — | `10s` | 超过该时长无通信显示为"抖动" |
| `--offline-after` | — | `30s` | 超过该时长无通信判定离线并告警 |
| `--shutdown-grace` | — | `10s` | 收到退出信号后的最长等待时间 |
| `--version` | — | — | 打印版本后退出 |

**没有配置文件**：只有命令行 + 环境变量；运行期可变的设置存在数据库里，由后台页面修改。

## 参数（probe-agent）

| 参数 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `--server` | `PROBE_SERVER` | 空 | 服务端地址，如 `https://monitor.example.com`；**必须是 https**（本机回环可用 http 调试） |
| `--token-file` | `PROBE_TOKEN_FILE` | 空 | Token 文件路径（推荐：0600，避免出现在 `ps` 里） |
| `--token` | `PROBE_TOKEN` | 空 | Token 明文（会出现在 `ps` 输出中，不推荐） |
| `--name` | `PROBE_NAME` | 空 | 节点显示名建议（仅首次注册时用） |
| `--state-dir` | `PROBE_STATE_DIR` | `/var/lib/probe-agent` | 状态目录（流量 checkpoint 落在这里） |
| `--root` | `PROBE_ROOT` | `/` | 根目录，其下应有 `proc/` 与 `etc/`；诊断时可指向一份根文件系统快照 |
| `--iface` | `PROBE_IFACE` | 空 | 要监控的网卡；留空 = 默认路由网卡 → IPv6 默认路由 → 排除虚拟网卡后流量最大者 |
| `--disk` | `PROBE_DISK` | `/` | 主文件系统路径（详情页第一块磁盘） |
| `--log-level` | `PROBE_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `--log-format` | `PROBE_LOG_FORMAT` | `text` | `text`/`json`（日志走 stderr） |
| `--interval` | — | `1s` | 采集/上报间隔，1s–300s；**实际以服务端下发为准** |
| `--samples` | — | `1` | 配合 `--print-json`：连续打印几份采样 |
| `--print-json` | — | — | 打印一次采集结果后退出（自检/排障，任何平台可用） |
| `--once` | — | — | 连上服务端上报一次后退出（验证 Token 与网络） |
| `--insecure-skip-verify` | — | — | 跳过 TLS 证书校验（不推荐） |
| `--allow-plaintext` | — | — | 允许明文 `ws://` 连接非本机地址（不推荐） |
| `--version` | — | — | 打印版本后退出 |

Agent 的行为约定：

- **同步发送，没有队列、没有缓存**：一次只发一帧，写完才继续下一拍；网络慢导致跳过的拍数计入 `dropped` 上报给服务端。
- 重连退避：正常断开 1s→2s→…→60s（±20% 抖动，稳定运行 60s 后重置为 1s）；`401` 5 分钟、`403` 10 分钟、协议不兼容 30 分钟。**服务端重启、Agent 重启、VPS 重启、网络抖动都走这条路径。**
- 退出前会把流量 checkpoint 落盘，不丢最后一段增量。
- 采集内容的其它约定：

- CPU 使用率取 `/proc/stat` 前 8 个字段（`guest` 已含在 `user` 里，不重复计数），**iowait 不算忙**；第一次采样没有基线，`cpu_pct` 为 0。
- 内存已用 = `MemTotal - MemAvailable`（老内核没有该字段时退回 `MemFree+Buffers+Cached`）。
- 磁盘占用口径与 `df` 一致：`used/(used+avail)`。
- 流量是**探针自己维护的单调累计**（存在 `--state-dir` 的 checkpoint 里），不是内核计数器；机器重启、Agent 重启、Agent 重装、网卡重建、计数器清零、32 位回绕各有明确处理，原则是**宁可少算，绝不多算**（`docs/DESIGN.md` §9）。
- `--print-json` 使用仅内存的流量统计，**不会在磁盘上留下任何状态文件**；快照模式下跳过需要 `statfs` 的磁盘统计并在 `warnings` 里说明。

## 安全要点

- 默认只监听 `127.0.0.1`。**公网部署必须提供 TLS**（推荐 Caddy/nginx 反代，或用 `--tls-cert/--tls-key`）；以明文监听非本机地址时会在每次启动打印警告。
- SQLite 文件 0600，数据目录 0750；**数据库必须放在本地盘**（NFS/SMB 的锁语义不可靠）。
- 前端不加载任何外部资源；服务端不请求任何用户提供的地址。
- Agent 不监听端口、不需要 root、不接受任何远程命令。项目内没有任何"远程 Shell / 文件管理"接口。

## 长跑结论（Phase 13）

50 个节点、1 秒间隔、连续 10 分钟（真实二进制 + 50 条 WebSocket 连接）：

- **稳态 RSS 37–39 MB**（Windows；前两分钟的 108–128 MB 是启动爬坡），CPU 低于计量分辨率，零 ERROR；
- 数据库 10 分钟只增长几百 KB，写库 6 次/分钟；
- **硬杀（kill -9）后重启**：节点、六档历史、今日/本月流量、操作记录全部完好，
  只丢最后 10 秒还在内存里的样本桶（设计里明确接受的取舍）；
- 细节与复现步骤见 [`docs/PERFORMANCE.md`](docs/PERFORMANCE.md)。

## 目录结构

```
cmd/probe-server/     Server 入口（只做装配与优雅退出）
cmd/probe-agent/      Agent 入口（自检模式 / 正式运行）
internal/config/      启动参数解析（server.go / agent.go / bytes.go）
internal/store/       SQLite：打开、迁移、节点/样本/流量/告警/审计查询
internal/state/       内存实时状态（最新值 + 连接归属）
internal/server/      HTTP 路由、中间件、SSE、聚合流水线、告警接入、API
internal/alert/       告警规则引擎、发送流水线、Telegram 通知器
internal/protocol/    Agent ↔ Server 消息结构（两个二进制共用）
internal/agent/       采集与本地统计（collect / collector / traffic / disk_*）
internal/version/     版本信息（ldflags 注入）
web/                  内嵌前端资源（go:embed）+ 自研 canvas 图表
deploy/               安装/升级/卸载脚本 + systemd 单元 + 脚本护栏测试
docs/                 设计基线、协议、安全结论、性能结论
```

## 文档

| 文档 | 内容 |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | 冻结的设计基线：数据模型、协议、算法、范围表、分阶段验收 |
| [`docs/PROTOCOL.md`](docs/PROTOCOL.md) | Server ↔ Agent 的完整线协议（帧、错误码、关闭码） |
| [`docs/SECURITY.md`](docs/SECURITY.md) | 威胁模型、逐项控制点与证明用例、依赖复核、已知接受的风险 |
| [`docs/PERFORMANCE.md`](docs/PERFORMANCE.md) | 50 节点实测（长跑 / 延迟 / 内存 / 写库 / 推送量）与复现步骤 |
| [`deploy/README.md`](deploy/README.md) | 安装脚本、systemd 加固说明、反代配置 |
| [`docs/DEPLOY.md`](docs/DEPLOY.md) | **部署手册**：从交叉编译、上传、装服务端、反代 TLS、加节点到装 Agent 的完整流程（含避坑清单） |

## 许可

MIT，见 [`LICENSE`](LICENSE)。
