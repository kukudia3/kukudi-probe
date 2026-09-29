# 极简 VPS 探针 — 设计文档（Phase 1，待确认）

> 本文是唯一的设计基线。后续所有代码都必须能追溯到本文的某一节。
> 与本文件冲突的临时需求，先改本文件，再改代码。

状态：**待用户确认**。确认前不写业务代码。

---

## 1. 项目目标（复述）

做一个**自己长期用**的轻量服务器监控系统：打开网页，几秒钟内知道我的 VPS 是否正常。

它必须：

- 一个 Go 二进制当 Server，一个 Go 二进制当 Agent，**无中间件**（无 Redis / MQ / 外部 DB / 外部时序库）。
- 数据、配置、历史全部留在自己的服务器上：**不接第三方云、不埋点、不遥测、不广告**。
- 单管理员，无 RBAC、无多租户、无组织概念。
- 1 秒级实时刷新 + 可靠的流量统计 + 可靠的历史曲线。
- 在 50 台 VPS × 1 msg/s 下依然非常轻；数据库长期运行不无限膨胀。
- 安装、升级、卸载三条命令以内搞定。
- 代码量小到能一次性读完、能长期维护。

**它不是**：运维平台、服务器面板、远程管理工具、Grafana/Prometheus 替代品、Web SSH/文件管理器。

---

## 2. MVP 范围（v1 做）与明确延期（v1 不做）

### v1 做（只有这些）

| 模块 | 内容 |
|---|---|
| 认证 | 单管理员登录（Argon2id + 会话 Cookie + CSRF）、首次初始化 |
| 节点 | 增 / 删 / 改 / 停用；名称、分组（字符串）、国家地区（手填）、备注、月流量额度、流量重置日、到期日；Token 查看（仅首次）/ 重新生成 |
| 实时 | Agent 每 1s 采集上报；Server 内存保存最新值；前端 SSE 1s 级推送 |
| 首页 | 全部节点卡片 + 集群汇总条；6 个关键指标 + 状态 |
| 详情 | 基本信息表 + 5 张历史小图（CPU/内存/磁盘/网络/延迟）+ 6 档时间范围 |
| 流量 | 今日 / 本周期 / 累计（上、下、总）；周期额度与告警；Agent 本地 checkpoint 保证重启不乱 |
| 延迟 | Agent ↔ Server 应用层 RTT（同时充当 heartbeat）；min/avg/max 历史 |
| 在线 | 3 态状态机（online / stale / offline）+ 去抖 + 恢复通知 |
| 历史 | 两级存储（10s→12h、1m→8d）+ 读时降采样 + 定时清理 |
| 告警 | Notifier 接口；v1 只实现 Telegram；4 类规则（离线、恢复、流量阈值/超额、到期） |
| 运维 | systemd 单元（含安全加固）、安装/卸载/升级脚本、`--print-json` 自检、`go test ./...` 全绿 |

### v1 明确不做（写死在这里，防止范围蠕变）

Windows/macOS Agent（v1 只 Linux，非 Linux 直接报错退出）、Web SSH / 远程命令 / 文件管理 / Docker / 进程 / systemd 管理、多用户 / RBAC / OAuth / 2FA（2FA 记入 v1.1 候选）、多探测点互 Ping / 全球地图 / 拓扑 / traceroute、ICMP ping 探测（需要 root 权限，见 §17）、Prometheus 导出 / Grafana 对接、插件市场、公开状态页（v1.1 候选）、价格与成本核算、自定义仪表盘 / 自定义阈值规则引擎、审计导出、i18n（v1 只中文）、自动升级 Agent（风险高于收益）。

---

## 3. 技术架构

```
                        ┌──────────────────── 一台自己的服务器 ────────────────────┐
                        │                                                          │
  VPS A                 │   probe-server（单进程单二进制）                          │
  ┌────────────┐  wss   │   ┌──────────────────────────────────────────────┐       │
  │ probe-agent│────────┼──▶│ agentconn  50 conns, 1 msg/s/conn            │       │
  │ 1 Hz collect│       │   │   ↓ 严格校验 + 限流                          │       │
  └────────────┘        │   │ state     内存最新快照（map[int64]*NodeState）│       │
        ▲               │   │   ↓                          ↓               │       │
        │               │   │ hub(SSE)                aggregator           │       │
  VPS B ... 50 台       │   │   ↓ 1 Hz 变更推送           ↓ 10s/1m/1h 桶     │       │
                        │   │ 浏览器                     SQLite(WAL)        │       │
                        │   │                             ↑               │       │
                        │   │ alert(规则+去重+冷却) ──▶ Telegram            │       │
                        │   └──────────────────────────────────────────────┘       │
                        │   web/ 静态资源（go:embed，无 CDN、无外链）               │
                        └──────────────────────────────────────────────────────────┘
```

**三条独立通道，各用最适合的协议：**

| 通道 | 协议 | 理由 |
|---|---|---|
| Agent → Server | **WebSocket（JSON 帧，1 帧/秒）** | 长连接省掉每秒一次 HTTP 头开销；服务端可反向下发配置；断连即知；单连接便于限流与背压 |
| Server → 浏览器 | **SSE**（`text/event-stream`） | 只需服务端→客户端单向；浏览器原生 `EventSource` 自动重连；不用引入任何前端 WS 库；纯 stdlib 实现 |
| 浏览器 → Server | **普通 HTTPS JSON API** | 管理操作低频，够用 |

不做的选择：gRPC（重）、Protobuf（多一层代码生成）、消息队列（不需要）、Redis（不需要）、HTTP 轮询（浪费）。

### 技术选型与理由

| 层 | 选型 | 理由 |
|---|---|---|
| 语言 | Go 1.22+ | 单静态二进制、交叉编译容易、并发模型适合"每连接一个 goroutine"、内存可控、标准库 HTTP 可直接上生产 |
| DB | SQLite（WAL） | 单机 50 节点写入量极小；零运维、零中间件；备份=复制一个文件 |
| SQLite 驱动 | `modernc.org/sqlite`（纯 Go，无 CGO） | 交叉编译/静态编译零障碍（CGO 会让 `GOOS=linux` 交叉编译变麻烦）；性能对本项目**完全过剩**（我们每分钟只有个位数事务） |
| Agent 连接库 | `github.com/coder/websocket` | 小而现代、context 优先、支持 write deadline / read limit，依赖树干净 |
| 密码哈希 | `golang.org/x/crypto/argon2` | Argon2id 是当前推荐；`x/crypto` 是半标准库 |
| 日志 | 标准库 `log/slog`（JSON/Text） | 零依赖、结构化、级别可控 |
| 前端 | 原生 HTML/CSS/JS（无框架、无构建步骤） | 一个 SPA 约 1000 行 JS；没有 npm、没有打包器、没有 node_modules |
| 图表 | **自研 canvas 折线图（约 330 行，`web/chart.js`）** | 需要的只有"固定刻度 + 悬浮读数"，为此拉一个图表库进来不划算；零第三方代码、零下载、PC/手机同一套渲染 |
| 依赖总数 | 目标 **≤ 5 个直接依赖**（sqlite / websocket / x/crypto / 无其他） | 少依赖 = 少升级 = 少供应链风险 |

**为什么不用 Node/Python 后端**：要额外运行时、内存高、部署多一步。**为什么不用 Postgres/TimescaleDB**：单机 50 节点用量级完全不需要，违反"不引入不必要中间件"。

---

## 4. 目录结构

```
probe/
├── cmd/
│   ├── probe-server/main.go        # 只做：解析参数、装依赖、启动、优雅退出
│   ├── probe-agent/main.go         # 同上（Agent 侧）
│   └── probe-fakeagent/main.go     # 压测/集成测试用的假 Agent（仅测试工具，不进 release）
├── internal/
│   ├── protocol/                   # 消息信封、载荷、严格校验（两个二进制共用）
│   ├── state/                      # 内存最新状态（1s 级更新，不落库）
│   ├── server/
│   │   ├── server.go               # HTTP 路由、健康检查、优雅退出
│   │   ├── middleware.go           # 日志、恢复、大小限制、安全头、来源 IP 解析
│   │   ├── webui.go                # go:embed 静态资源
│   │   ├── auth.go password.go     # 初始化码、登录、会话、CSRF、Argon2id
│   │   ├── api_nodes.go dto.go     # 节点列表/新增 + 前端视图结构
│   │   ├── api_stream.go hub.go    # SSE 变更推送 + 慢客户端保护
│   │   ├── api_series.go           # 历史查询（六档 range，Phase 6）
│   │   ├── api_settings.go         # 设置 + Telegram 测试（Phase 9）
│   │   └── agentconn.go            # Agent 接入、鉴权、限流
│   ├── state/                      # 内存最新状态（1s 级更新，不落库）
│   ├── store/                      # SQLite：store.go / schema.go / migrate.go / 查询 / rollup / retention
│   ├── alert/                      # 规则、去重、冷却、队列、Notifier 接口、telegram.go
│   ├── agent/                      # 采集与本地统计
│   │   ├── collect.go              # /proc 解析（纯函数，跨平台可测）
│   │   ├── collector.go            # 采样装配、网卡探测、磁盘统计
│   │   ├── traffic.go              # 单调流量累计 + 本地 checkpoint
│   │   ├── client.go               # WebSocket 客户端、退避重连
│   │   ├── disk_linux.go           # statfs（build tag: linux）
│   │   └── disk_other.go           # 非 Linux 明确不支持（build tag: !linux）
│   ├── config/                     # 启动参数解析（server.go / agent.go）
│   ├── units/                      # 字节/百分比/时间格式化（前后端只此一份）
│   ├── e2e/                        # 测试专用：真实 TCP 把 server 与 agent 拼起来跑
│   └── version/                    # 版本号、构建信息
├── web/
│   ├── embed.go index.html app.js style.css
│   └── chart.js                    # 自研 canvas 折线图（无第三方库）
├── deploy/
│   ├── install.sh                  # 一步式：下载 + SHA256 校验 + 调用下面的脚本
│   ├── install-server.sh           # 安装/升级/卸载（含 systemd 加固单元）
│   ├── install-agent.sh            # 同上，Token 只写文件不进命令行
│   ├── deploy_test.go              # 脚本护栏测试（LF、加固项、无 curl|sh、哈希校验）
│   └── README.md
├── scripts/
│   └── install-server.sh  install-agent.sh  uninstall.sh  build-release.sh
├── docs/
│   └── DESIGN.md  PROTOCOL.md  DEPLOY.md  SECURITY.md
├── go.mod  go.sum  Makefile  README.md  LICENSE
```

不拆 `pkg/`、不拆 `internal/service`/`repository`/`usecase` 三层：本项目**没有多个实现**，接口只在真正需要多实现的地方出现（`Notifier`、`store` 的窄接口）。

Agent 的 `/proc` 解析全部是"读文件 + 解析文本"的纯函数，只有磁盘容量需要 `statfs` 系统调用并单独用 build tag 隔离——因此采集逻辑可以在任何平台上对着**提交进仓库的 `/proc` 快照**做单元测试和自检（`--print-json --root <快照>`），不需要 Linux 机器。

---

## 5. 进程、端口、路径约定

| 项 | Server | Agent |
|---|---|---|
| 二进制 | `/usr/local/bin/probe-server` | `/usr/local/bin/probe-agent` |
| 用户 | 专用非 root 用户 `probe` | 专用非 root 用户 `probe-agent`（无需任何 capability） |
| 数据 | `/var/lib/probe-server/probe.db`（0600，目录 **0700**） | `/var/lib/probe-agent/state.json`（0600） |
| 配置 | **无配置文件**：命令行 + 环境变量；可变设置存 DB（后台可改） | 命令行 + `--token-file`（0600） |
| systemd | `probe-server.service` | `probe-agent.service` |
| 默认监听 | `127.0.0.1:25774`（默认只本地，公网必须经 TLS 反代或 `--tls-cert/--tls-key`） | — |
| 端口 | 25774/tcp | 无需监听任何端口（纯出站） |
| Agent 接口 | `GET /api/v1/agent/ws`（`Authorization: Bearer <token>`） | — |

Agent 默认**不监听任何端口**，只出站连接 Server —— 这一点对安全很重要。

---

## 6. 数据库设计（SQLite + WAL）

### 打开参数

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;   -- 崩溃最多丢最后几秒实时状态，流量/历史可重建（见 §9）
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA temp_store   = MEMORY;
```

两个 `*sql.DB` 句柄指向同一文件：**写句柄 `MaxOpenConns(1)`**，**读句柄 `MaxOpenConns(4)`**。WAL 允许读写并发，且我们的写入只有每分钟个位数事务，不会有写竞争。

迁移：`PRAGMA user_version` + 有序迁移函数数组，启动时在单个事务内执行，只前进不回退。

### 表（9 张，够用为止）

```sql
-- 1) 键值设置：schema 版本、管理员密码哈希、Telegram 配置、保留策略覆盖、时区
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 2) 节点（配置类数据；分组/地区是字符串列，不做关联表）
CREATE TABLE nodes (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  name             TEXT    NOT NULL,
  group_name       TEXT    NOT NULL DEFAULT '',
  region           TEXT    NOT NULL DEFAULT '',   -- 国家/地区，管理员手填
  note             TEXT    NOT NULL DEFAULT '',
  token_hash       BLOB    NOT NULL,              -- sha256(token) 32B，不存明文
  token_prefix     TEXT    NOT NULL DEFAULT '',   -- 展示用（前 8 字符）
  token_created_at INTEGER NOT NULL DEFAULT 0,
  iface            TEXT    NOT NULL DEFAULT '',   -- 空=Agent 自动探测
  interval_sec     INTEGER NOT NULL DEFAULT 1,
  traffic_limit    INTEGER NOT NULL DEFAULT 0,    -- bytes，0=不限
  traffic_warn_pct INTEGER NOT NULL DEFAULT 80,
  reset_day        INTEGER NOT NULL DEFAULT 1,    -- 1..31，计费周期起算日
  expires_at       INTEGER NOT NULL DEFAULT 0,    -- unix 秒，0=不提醒
  sort_order       INTEGER NOT NULL DEFAULT 0,
  enabled          INTEGER NOT NULL DEFAULT 1,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_nodes_name  ON nodes(name);
CREATE UNIQUE INDEX idx_nodes_token ON nodes(token_hash);

-- 3) 每节点一行的服务端运行态（重启后仍能看到最后状态；也是流量基线）
CREATE TABLE node_runtime (
  node_id       INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  last_seen     INTEGER NOT NULL DEFAULT 0,
  status        TEXT    NOT NULL DEFAULT 'unknown', -- online/stale/offline/unknown
  cpu_pct REAL, mem_pct REAL, swap_pct REAL, disk_pct REAL, load1 REAL,
  lat_ms REAL, uptime_sec INTEGER,
  boot_id       TEXT    NOT NULL DEFAULT '',
  iface         TEXT    NOT NULL DEFAULT '',
  rx_total      INTEGER NOT NULL DEFAULT 0,   -- Agent 生命周期累计（权威流量基线）
  tx_total      INTEGER NOT NULL DEFAULT 0,
  rx_raw        INTEGER NOT NULL DEFAULT 0,   -- 内核 counter 快照（诊断/审计用）
  tx_raw        INTEGER NOT NULL DEFAULT 0,
  agent_version TEXT, kernel TEXT, os_name TEXT, cpu_model TEXT,
  updated_at    INTEGER NOT NULL DEFAULT 0
);

-- 4) 日流量（按服务器本地日期聚合；月用量=周期内求和，不做"清零"动作）
CREATE TABLE traffic_daily (
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  day     TEXT    NOT NULL,                    -- 'YYYY-MM-DD'
  rx      INTEGER NOT NULL DEFAULT 0,
  tx      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node_id, day)
) WITHOUT ROWID;

-- 5/6) 两级历史桶（结构相同）
CREATE TABLE samples_10s (            -- 保留 12h
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL,      -- 桶起点（10 的倍数）
  cpu_avg REAL, cpu_max REAL, mem_avg REAL, mem_max REAL, swap_avg REAL,
  disk_avg REAL, disk_max REAL, load1_avg REAL,
  rx_rate REAL, rx_max REAL, tx_rate REAL, tx_max REAL,   -- bytes/s
  lat_avg REAL, lat_min REAL, lat_max REAL,
  up_cnt INTEGER NOT NULL DEFAULT 0, all_cnt INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node_id, ts)
) WITHOUT ROWID;
CREATE TABLE samples_1m ( /* 同列 */ PRIMARY KEY (node_id, ts) ) WITHOUT ROWID;   -- 保留 8d

-- 8) 告警状态（去重/冷却持久化，重启不重复轰炸）
CREATE TABLE alert_state (
  node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  rule        TEXT    NOT NULL,      -- offline / traffic_warn / traffic_exceeded / expiry_7 ...
  state       TEXT    NOT NULL,      -- firing / resolved
  since       INTEGER NOT NULL,
  last_notify INTEGER NOT NULL DEFAULT 0,
  notify_cnt  INTEGER NOT NULL DEFAULT 0,
  context     TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (node_id, rule)
) WITHOUT ROWID;

-- 9) 会话（管理员登录态；只存 token 的 sha256）
CREATE TABLE sessions (
  token_hash   BLOB PRIMARY KEY,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  ip TEXT NOT NULL DEFAULT '', ua TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sessions_exp ON sessions(expires_at);

-- 10) 极简审计（登录、改密、Token 重生成、节点删除），最多保留 2000 条
CREATE TABLE audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL, action TEXT NOT NULL,
  node_id INTEGER NOT NULL DEFAULT 0, ip TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
```

### 设计说明与写入节奏

- **表少、不高规范化**：分组/地区直接是列；没有 `node_group`、`metric`、`alert_rule` 表。
- **写入节奏**（50 节点满负载下）：
  - `samples_10s`：每 10s 一个事务，批量 N 行 → **6 事务/分**
  - `samples_1m`：每 60s `INSERT … SELECT … GROUP BY ts/60` **1 条 SQL/分**（在 DB 内聚合，不进 Go）
  - `traffic_daily` + `node_runtime`：每 60s 一个事务（流量增量 + 基线一起提交，见 §9；运行态的最后值也在同一周期写出）
  - 清理：每小时一次，按节点分批 `DELETE`
  - **合计 ≈ 4 个事务/分钟**，与"每秒一条历史"相比低 3 个数量级。
- **索引**：只保留每表主键 `(node_id, ts)`（范围查询天然走它）+ `nodes` 两个唯一索引 + `sessions.expires_at`。故意不给 `ts` 建独立索引：清理用"每节点精确区间删除"绕开全表扫描。
- **空间**（估算，实测会写进 README）：50 节点稳态约 **45–60 MB**；5 节点约 **5–6 MB**。行宽 ~60 B 级别。
- **备份**：直接 `sqlite3 probe.db ".backup"` 或停服复制；WAL 文件不需要单独备份（用 `.backup` 命令）。
- **不可放 NFS/SMB**：SQLite 锁依赖本地文件系统，文档里必须写明。

---

## 7. Agent ↔ Server 通信协议（摘要，全量见 `docs/PROTOCOL.md`）

- 传输：`wss://<host>/api/v1/agent/ws`（HTTPS 上 1 个 Upgrade）
- 鉴权：升级请求头 `Authorization: Bearer <token>`；**Token 不放 URL**（URL 会进反代日志）
- 消息信封：`{"v":1,"t":"metrics","ts":1712345678,"d":{…}}`，UTF-8 JSON，**单帧上限 16 KB**
- 消息类型只有 7 个：`hello` / `welcome` / `metrics` / `ping` / `pong` / `config` / `error`
- 版本协商：`hello.v` + `agent_version`；服务端只接受 `[min,max]` 区间，不匹配回 `error{code:"upgrade_required"}` 后关闭（Agent 慢速重试并打清晰日志）
- 心跳与延迟：Agent 每 5s 发 `ping{ts}`，Server 立即回 `pong{ts}`；Agent 算 RTT 作为延迟指标随 `metrics` 上报。**metrics 本身每 1s 到达也同时充当心跳**，两者结合判定在线。
- 背压：Agent **同步发送、无队列、无缓存**——网络慢时宁可跳过这一拍（计入 `dropped`），也不在内存里堆积；Server 侧每节点限 5 msg/s，超出直接丢弃；服务端写入带 5s 超时，慢连接直接关闭。
- 重连：指数退避 1s→2s→4s…→60s，带 ±20% 抖动；`welcome` 成功后重置退避。Server 重启、Agent 重启、VPS 重启、网络抖动都走这条路径。

---

## 8. 每秒实时状态处理方案

```
Agent (1 Hz)                          Server                               浏览器
─────────────                         ──────                               ──────
/proc 采样 (~50µs)                    收帧 → 严格校验 → 限流
  ↓                                    ↓
算速率/增量 (内存, 无分配)             state.Update()  内存最新值 + 1s 环形缓冲(900 点/节点)
  ↓                                    ↓                          ↘
入队(cap 5, 满丢旧)  ──ws──▶          桶累加器(10s/1m/1h)         hub: 1 Hz 汇总"变更节点"
  ↓                                    ↓                          ↓
写超时 5s 保护                        每 10s 批量落库              SSE 推送(每客户端 1 槽缓冲)
```

```
Agent 1s 上报 ──▶ 内存最新值（SSE 1 Hz 推送）
                └─▶ 内存 10 秒桶累加器 ──flush(10s)──▶ samples_10s
                                          ──rollup(1m)─▶ samples_1m
                        node_runtime（1m，重启后恢复界面）· 保留清理（1h）
```

关键点：

1. **每秒数据绝不直接写 SQLite**。1s 数据只进内存（最新值 + 10 秒桶累加器），落库只有 10s/1m 两种聚合，写库频率是个位数每分钟。
2. **前端刷新与 Agent 上报解耦**：N 个浏览器看同一节点，Agent 仍然只采集/上报一次（它根本不关心有没有人看）。这是结构上天然成立的，不是靠加锁。
3. **变更集推送**：每个节点带 `seq`（每次更新自增），hub 每秒只把"变了"的节点发给浏览器（状态随时间变化也算变化）；首页 50 节点全变约 10–15 KB/s/浏览器。
4. **慢客户端**：每个 SSE 客户端一个**容量 1 的"最新值槽"**（放不下就替换，不是排队），写超时 10s → 断开回收。内存有界，永不堆积。
5. **Server 侧无"无意义 goroutine"**：每个 Agent 连接只占 1 个 goroutine（写入是同步的、带 5s 超时）；全局只有 **2 个后台 goroutine**——一个每秒的循环（评估告警 + 有浏览器连接时推送变更集；两者共用同一份节点视图，所以没有浏览器时也照常评估告警），一个按 10s/1m/1h 周期落库与清理的流水线循环。
6. **崩溃可接受**：`synchronous=NORMAL` 下最多丢最后几秒内存态；退出时**刻意不落盘**最后一个未结束的 10 秒桶（宁可丢 10 秒，也不写一个"半个桶"进库）；rollup 幂等，启动时补算最近 2 小时窗口即可自愈。

---

## 9. 流量统计详细方案（最关键的一节）

### 9.1 两个计数器必须分清

- `raw`（内核 `read /proc/net/dev` 的累计字节）：**每次开机从 0 开始，接口重建会清零或回绕，不可直接当统计用。**
- `probe_total`（探针自己的长期累计）：由 Agent 在本地 checkpoint 中维护的单调总量，**只增不减**（除非 checkpoint 丢失）。

### 9.2 Agent 本地算法

```
状态文件 /var/lib/probe-agent/state.json（0600，写临时文件+fsync+rename 原子替换）
{ iface, ifindex, mac, boot_id, total_rx, total_tx, raw_rx, raw_tx, saved_at }

每一拍：
  raw  = 读 /proc/net/dev[iface]              // 64 位累计
  boot = 读 /proc/sys/kernel/random/boot_id

  if iface 不存在 / ifindex 变了 / mac 变了:
        baseline = raw; delta = 0              // 接口被重建：只重设基线，不计流量
  else if boot != saved.boot_id:               // 机器重启过
        baseline = raw; delta = 0              // 计数器已归零，绝不能用 raw 差值
  else if raw >= saved.raw:
        delta = raw - saved.raw
  else if saved.raw > 2^31 and raw < 2^31:     // 32 位计数器回绕（老内核/32位）
        delta = (2^32 - saved.raw) + raw
  else:                                        // 其它"变小"= 计数清零
        delta = raw

  total_rx += delta;  raw_rx = raw
  上报 { rx_total: total_rx, tx_total: total_tx, rx_raw: raw, boot_id, ckpt_age_s }

持久化策略（防写放大）：
  每 10s 或累计未落盘增量 > 64MB 时写一次 checkpoint（每天约 8640 次小文件写，可接受；
  也可配置为 30s）
```

**关键取舍**：崩溃时丢失的是"未落盘的那几十秒增量"（少计），而不是重复计（多计）。宁可少算，不可多算——这是流量统计唯一正确的偏差方向。

### 9.3 Server 端算法（幂等，天然不会重复计）

- Server 保存**上次采纳的 `probe_total`**（`node_runtime.rx_total/tx_total`），并在启动时载入内存。
- 增量 = **本次观测 − 上次观测**（不是"观测 − 已落盘基线"）：重复帧差值为 0（不重复计），丢帧/跳帧差值自然补齐（不丢流量）。
- 异常分支（全部只重设基线、本次差值不计，即"宁可少算"）：
  - 首次看到该节点（或基线为 0）→ 只建基线。**绝不能把 Agent"安装以来的累计值"当成本月流量。**
  - 观测值变小 → Agent 重装 / checkpoint 丢失。
  - 单次差值 > `--traffic-delta-max`（默认 1 TiB）→ 可疑，重设并记 WARN。
- **每 60 秒一个事务**同时提交：`traffic_daily` 增量 UPSERT + `node_runtime` 新基线（`store.FlushTraffic`）。两者原子，所以**永远不会出现"增量加了但基线没动"（重复计）或"基线动了但增量没加"（丢流量）**。
- 内存状态只在**落盘成功后**才推进（snapshot → 写库 → commit）：写失败就保留 pending，下一分钟重试，绝不丢也不重。
- Server 崩溃/重启期间的流量也不会丢：重启后的第一帧差值天然包含那段区间（因为 Agent 的 `probe_total` 一直在涨）。

### 9.4 周期（月）用量与"重置日"

- **不做破坏性清零**。月用量 = `SUM(traffic_daily) WHERE day ∈ [周期起, 下周期起)`。
- 周期起算：`reset_day = 19` 时，今天 ≥19 号 → 周期起 = 本月 19 日；否则 = 上月 19 日。`reset_day` 大于当月天数时（如 31 在 2 月）钳到当月最后一天。
- 好处：历史永不丢、重算永远一致、跨月查询免费、改重置日只需改配置（立即生效且可解释）。
- UI 上就叫「本周期用量」，看起来就等于"重置了"。

### 9.5 故障矩阵（必须全部有测试覆盖）

| 场景 | 结果 |
|---|---|
| VPS 重启 | Agent 侧 `boot_id` 变化 → 基线重设、差值 0 → **本月流量不暴增**；服务端只看到"差值正常或变小（重设）" |
| Agent 重启 | checkpoint 读回 total → 继续累加，**不重复、不清零** |
| Agent 重装（checkpoint 丢失） | 观测值变小 → 服务端重设基线、本次差值不计 → **不暴增**（期间流量少计，可接受） |
| Server 重启（含 kill -9） | 基线来自 DB 并在启动时载入；第一帧差值含空窗 → **不漏计、不重复计** |
| 重复帧 / 乱序重发 | 差值 = 本次观测 − 上次观测 → 重复帧算 0 |
| DB 从旧备份恢复 | 基线回退 → 差值为正且是真实发生过的流量 → 最多一次"补记"（超过上限则重设为基线） |
| 网卡重建 / 换名 / ifindex 变 | Agent 重设基线 → 不计 |
| 32 位计数器回绕 | Agent 侧按回绕公式补偿 |
| 单次差值超上限 | 重设基线 + WARN（防"凭空多几 TB"） |
| 服务器长时间下线（>1 天） | 差值一次性补记到"补记当天"，日志标注（已知局限，v1.1 可让 Agent 上报本地按天累计来精确归属） |

---

## 10. 历史数据：分层、降采样、保留

### 10.1 两级存储

| 表 | 桶宽 | 保留 | 50 节点行数 | 用途 |
|---|---|---|---|---|
| `samples_10s` | 10s | 12h | 216k | 1h（10s 桶）、6h（30s 桶）曲线 |
| `samples_1m` | 1min | 8d | 576k | 12h / 1d / 3d / 7d 曲线 |

（**只有两层**：六档图表最长 7d，全部由这两层供数。10s 层保留 12h 而不是 6h，是为了让 6h 档永远有完整窗口、不被清理边界切掉。原设计的 `samples_1h` 长历史层已按用户决定删除——没有任何图表用它；将来若要加 30d/1y 图表，加一个迁移建表即可。保留期可用参数覆盖；行数只是估算，实测写入 README。）

### 10.2 降采样规则

- **每个桶都保存 avg / max**（CPU、内存、磁盘、网速、延迟），所以再长的范围也不会把"短时间飙高"抹平；UI 画 avg 线 + 一条更淡的 max 线。**接口只返回 `[ts, avg, max]`**：图表上多一个 min 只会更难读（延迟的 min 仍留在库里备用）。
- 聚合在 **SQL 内完成**（`GROUP BY (ts / 桶宽) * 桶宽`），不把原始行读进 Go。
- 聚合只处理**已封闭的桶**（`ts + 桶宽 ≤ 现在`），因此 rollup **幂等**，可以安全重跑（启动时补算最近 2 小时窗口，修复异常退出造成的空洞）。
- 每个桶同时存 `up_cnt` / `all_cnt`（实际收到的上报次数 / 按间隔应有的次数）→ 任意范围（≤7d）的可用率直接算出来，不需要额外表。
  - **两者都乘了同一个缩放因子 600**（`server/aggregate.go` 的 `uptimeScale`）：上报间隔可以是 1–300 秒，而桶宽只有 10 秒，间隔大于桶宽时"每桶应有的帧数"是分数（30 秒 → 1/3 帧），纯整数会变成 0、可用率就永远算不出来。乘 600 后 1–300 秒都能精确表示（最大误差 0.02%），比值语义不变。
  - 分母只累计**真实存在的桶**：这是"在探针观测到的这段时间里上报有多完整"，而不是"墙钟时间里节点在线多久"——服务端自己停机、或节点还没创建的那段时间没有桶，不该记到节点头上。

### 10.3 六档时间范围的取数与图表参数（范围：1h / 6h / 12h / 1d / 3d / 7d）

**规格来源**：用户给定（后端 ≤1000 点动态返回 / 手机端二次聚合 / X 轴标签间隔）。三条规则就是代码里的全部逻辑：

1. **后端**：源层取「**能覆盖该范围的最细一层**」（分辨率最高）；`bucket_sec` 从整齐梯级 `{10s,20s,30s,1m,2m,5m,10m,15m,30m,1h,2h,4h,6h,12h,1d}` 里取「使 `range/bucket_sec ≤ 1000` 的最小值」。**任何一档都绝不返回超过 1000 点**；`bucket_sec` 在响应的 `meta` 里回带，前端不猜。（实现细节：若最终桶宽 ≥ 更粗一层的间距且该层也覆盖该范围，就直接从更粗层取数——结果相同但少扫 10 倍行数。）
2. **手机端二次聚合**：按**目标间隔的绝对时间网格**分桶（`bucket = floor(ts/target)*target`），对落在桶内的服务端点取 avg/min/max。**不做插值造点**，也**不要求**目标间隔是服务端桶宽的整数倍（服务端 2m + 目标 5m 时，每桶 2–3 个点，min/max 精确、avg 权重略有差异——可接受）。兜底：`range/agg ≤ 1000`。
3. **X 轴**：标签位置固定在**绝对时间网格**上（≥1d 的间隔按服务器本地零点对齐，其余按 UTC 秒网格），窗口滑动时标签数只会有 ±1 的差别，布局不跳。桌面标签 >8 个、移动 >5 个时按**整数倍跳过**标签（网格线跟随实际标签，不画多余竖线）。

| 范围 | 后端：源层 → 桶宽 → 点数 | 手机端聚合目标 → 实际点数 | X 轴基础刻度 | 标签间隔（桌面 / 移动） |
|---|---|---|---|---|
| 1h | samples_10s → 10s → 360 | 不聚合 → 360 | 10 min | 10m（6 个）/ 10m（6 个） |
| 6h | samples_10s → 30s → 720 | 2 min → 180 | 2 h | 2h（3 个）/ 2h（3 个） |
| 12h | samples_1m → 1m → 720 | 3 min → 240 | 3 h | 3h（4 个）/ 3h（4 个） |
| 1d | samples_1m → 2m → 720 | 5 min → 288 | 6 h | 6h（4 个）/ 6h（4 个） |
| 3d | samples_1m → 5m → 864 | 15 min → 288 | 1 d | 1d（3 个）/ 1d（3 个） |
| 7d | samples_1m → 15m → 672 | 30 min → 336 | 2 d | 2d（4 个）/ 4d（2 个，每 2 个跳 1 个） |

- 后端点数 360–864，手机端点数 180–360 —— 两侧都在 1000 点以内，手机端不 overplotting。
- 7d 的 2d 刻度对齐到**偶数日**（避免 7÷2=3.5 造成两端不齐）；窗口滑动时标签数为 3 或 4，语义稳定。
- 桶宽都是源层间距的整数倍 → 无残缺首尾桶，聚合在 SQL 内一次 `GROUP BY (ts/桶宽)*桶宽` 完成。
- **查询窗口两端都对齐到桶网格**（`Range.window`）：起点对齐保证第一个桶是完整的（否则 7d 档的第一个 15 分钟点可能只包含 1 分钟数据却被画成完整桶）；代价是最新点最多滞后一个桶（1h 档 10 秒，7d 档 15 分钟），对历史曲线无影响。
- 图表容器高度固定（移动 150px / 桌面 190px），Y 轴用"好看刻度"算法；切换范围时同尺寸 + 骨架占位，不重排页面。
- 每档的 `bucket_sec`、`points`、`tick_base_sec`、`tick_label_sec`、`mobile_agg_sec` 由后端在响应 `meta` 里给出（PC/移动用同一份数据，手机端只做二次聚合，不做两套 API）。

### 10.4 保留清理

- 每小时一次，**按节点**执行 `DELETE FROM samples_10s WHERE node_id=? AND ts<?`（走主键索引，单条极快），50 节点 50 条语句，分批提交。
- 不建额外 `ts` 索引（避免拖慢每 10s 的写入）。
- **清理与 rollup 的先后关系**：`rollup 覆盖写只增不减`（`WHERE excluded.up_cnt >= 目标.up_cnt`）。当 `--retention-10s` 短于 rollup 回补窗口时，部分 10 秒源行可能已被清理，此时残缺的重算结果**不会**覆盖掉已经算好的 1 分钟行。
- 不主动 `VACUUM`；删掉的页会被复用，文件大小在 1–2 个保留周期内稳定。提供 `probe-server compact`（可选，离线执行 `VACUUM`）不需要 —— 用一行文档说明手动 `VACUUM` 即可。

---

## 11. 在线 / 离线判定

```
t = now - last_seen
t < 10s          → ONLINE   （绿）
10s ≤ t < 30s    → STALE    （黄，"抖动/延迟"）
t ≥ 30s          → OFFLINE  （红，进入告警候选）
```

- 阈值可配（`--stale-after 10s`、`--offline-after 30s`）。
- **判定源是"最后一次有效通信"**：任何一个合法 `metrics` 或 `ping` 帧都刷新 `last_seen`；用一个统一的 1s ticker 对所有节点做状态迁移（不在收包路径上判断，避免抖动放大）。
- **去抖（hysteresis）**：
  - 变 OFFLINE：条件必须**连续**成立，且需要 ticker 连续两次判定（等价 2s 确认）；
  - 变 ONLINE：收到合法帧**立即**恢复显示（数据即证据），但**通知**要等在线稳定 30s 才发"已恢复"，避免"离线→恢复"成对轰炸。
- **通知抑制**：Server 启动后 60s 内只算状态、不发通知（避免重启引发全节点"恢复"风暴）；同节点同规则冷却 30 min；状态未变化不重复发。
- 结果：网络抖 3 秒、Agent 卡 5 秒，界面上最多闪一下黄色，不会红/绿反复横跳。

---

## 12. 告警架构

```go
type Notifier interface {
    Name() string
    Send(ctx context.Context, n Notification) error
}
type Notification struct {
    NodeID   int64
    NodeName string
    Rule     string    // offline / recovered / traffic_warn / traffic_exceeded / expiry
    Severity string    // info / warn / critical
    Title    string
    Body     string
    At       time.Time
}
```

- v1 实现：`telegram`（唯一）+ `log`（永远开启，写服务端日志，便于排查"为什么没收到"）。
- 扩展点：`webhook` / `email` / `bark`（v1.1 起，各约 40 行，不需要改架构）。
- 规则与去重：

| 规则 | 触发 | 去抖/去重 | 冷却 | 恢复 |
|---|---|---|---|---|
| `offline` | 持续 ≥30s 无有效通信 | 连续 2 次 tick 确认 | 30 min 重发（仍离线时） | — |
| `recovered` | 从 offline 恢复且稳定 30s | 与 offline 配对 | 6h | 发一次 |
| `traffic_warn` | 本周期用量 ≥ warn%（默认 80%） | 每周期一次 | — | 周期重置时静默解除 |
| `traffic_exceeded` | ≥100% | 每周期一次 | 每 24h 重发（可关） | 周期重置 |
| `expiry` | 到期前 7/3/1 天 | 每档一次 | — | 过期后再发一次"已到期" |

- 发送流水线：单 worker + 有界队列（128）+ 3s 内的事件合并成一条消息（最多 10 个节点一行一条）+ 重试 3 次（2s/8s/30s 退避）+ Telegram 429 按 `retry_after` 等待。令牌桶限 20 条/分。
- 状态持久化在 `alert_state`，重启不会重复轰炸。
- Telegram 消息**不启用 Markdown 解析**（避免节点名里的字符导致发送失败或注入）。

### 12.3 实现说明（Phase 8 落地）

- `internal/alert` 三个文件：`engine.go`（规则与去重，纯内存、可单测）、`dispatcher.go`（合并/重试/限流/队列）、`telegram.go`（唯一的网络出口）。
- **评估每秒一次**，与实时推送共用同一份 `currentNodes()` 结果（一次读取同时喂告警和 SSE，见 §8）。
- **只有状态变化才写库**：`alert_state` 的行数 = 节点数 × 已触发的规则数，稳态几乎不写库。
- **启动静默期** `--alert-startup-grace`（默认 60s）：只更新状态、不发通知，避免"服务端重启 → 所有离线节点齐刷刷轰炸一次"。
- **去抖与恢复确认**：`--alert-debounce`（默认 2s，抗网络抖动）、`--alert-recover-stable`（默认 30s，避免"掉线 3 秒又回来"刷屏）。
- 到期提醒按 **1 天 → 3 天 → 7 天** 递进提醒（同一档位只提醒一次，续期后规则自动 resolve）。
- 通知器**不持有用户可控的 URL**：Telegram 地址硬编码，Token 出现在 URL 路径里，因此错误信息与日志一律脱敏（有专门的用例守着）。
- 设置入口：`GET/PUT /api/v1/settings/telegram` + `POST /api/v1/settings/telegram/test`（前端「设置」对话框）；Token 永不回显，只回 `has_token`。

---

## 13. 安全设计

| 面向 | 措施 |
|---|---|
| 密码 | Argon2id（m=64MiB, t=3, p=1），PHC 字符串存储；登录恒定时间比较；改密后注销全部会话 |
| 会话 | 32 字节随机 token，DB 只存 SHA-256；Cookie `HttpOnly` + `SameSite=Lax` + `Secure`（TLS 时）+ `Path=/`；TTL 7 天滑动；登出/改密即失效 |
| CSRF | 同源校验（`Origin` 与 `Host` 一致）+ 会话**派生**的 CSRF Token（`sha256(会话token + 固定后缀)`，无需额外存储），所有写操作校验 |
| 暴力破解 | 登录/初始化限流（每 IP 5 次/窗口，连续 10 次失败锁 15 分钟）；`--trusted-proxy` 未配置时不信任 `X-Forwarded-For`；Argon2id 校验并发数限制为 2（防 64MiB×N 的内存放大） |
| 首次初始化 | 服务启动时在**日志**里打印一次性初始化码，`/api/v1/setup` 必须提交该码（30 分钟有效，重启重新生成）——避免"服务刚上线被人抢注管理员" |
| 传输 | 默认只监听 `127.0.0.1`；提供 `--tls-cert/--tls-key`；强烈建议 Caddy/nginx 自动 HTTPS；非 TLS 且监听公网时每次启动打 WARN |
| Agent Token | 32 字节 CSPRNG（`pba_` 前缀）；DB 存 SHA-256；只在创建时显示一次；随时可重新生成（旧的立即失效）；**绝不写日志**；支持 `--token-file`，避免出现在 `ps` 里 |
| SQL | 全部参数化查询；排序列名走白名单映射，绝不拼接用户输入 |
| XSS | 前端只用 `textContent` / DOM API，禁止 `innerHTML` 渲染数据；CSP：`default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`；无内联脚本/样式 |
| 其它响应头 | `X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、TLS 时 `Strict-Transport-Security` |
| SSRF | 服务端**不请求任何用户提供的 URL**；Telegram 只连硬编码的 `api.telegram.org` |
| 路径穿越 / 任意文件读写 | 服务端没有任何"按路径提供文件"的接口；前端资源用 `go:embed` + 固定路由；Agent 不接受任何来自网络的路径/命令 |
| 命令注入 / 远程执行 | 全项目**不调用 `os/exec`**（Server 完全不 import）。探针就是探针 |
| 输入校验 | 单帧 ≤16 KB；字段白名单 + 类型/范围/长度校验；数值 clamp（CPU 0–100、无 NaN/Inf、时间戳 ±5min）；`interval` 限制在 1–300s |
| 日志泄漏 | Token / 密码 / Cookie / Authorization 一律脱敏；不记录查询串；日志只到 stdout（交给 journald） |
| 文件权限 | DB 0600、数据目录 **0700**（WAL/SHM 由 SQLite 创建，只能靠目录权限兜住）、Agent token 文件 0600；systemd 下用 `StateDirectory=` + `UMask=0077` 且非 root 运行 |
| 越权 | 所有 `/api/v1/*`（除 `/healthz`、登录、Agent WS）强制会话校验；节点级接口校验 id 存在性并返回 404 而非 500 |
| 依赖 | 直接依赖 ≤5 个；`go mod verify` + `govulncheck`（Phase 10 跑一次，之后每次升级依赖跑） |

**明确不做**：Web SSH、远程命令、文件管理、Docker/systemd 管理、插件、公开注册（节点由管理员预先创建；自动注册 enrollment 留到 v1.1 且默认关闭）。

---

## 14. 性能预算（设计目标，Phase 11 用 `probe-fakeagent` 实测并写入 README）

| 项 | 目标 | 说明 |
|---|---|---|
| Agent CPU | < 0.3%（1 核平均） | 每秒 5 次小文件读（/proc 都在内存里，约 50µs） |
| Agent 内存 | **RSS ≤ 20 MB**（实测预期 8–15 MB） | Go 运行时基线约 6–10 MB。**"内存接近 0"做不到**，见 §17 |
| Agent 网络 | 约 0.4 KB/s（≈10 MB/月/节点） | JSON 帧 + 少量 ping |
| Server CPU（50 节点） | < 3%（1 核） | 50 msg/s 的 JSON 解码 + 内存更新，成本极低 |
| Server 内存 | RSS ≤ 80 MB | 状态 + 环形缓冲（50×900 点 ≈ 2.5 MB）+ SQLite page cache |
| Server DB 写 | ≈10 事务/分钟 | 见 §6 |
| DB 稳态大小 | 50 节点 45–60 MB；5 节点 ~5–6 MB | 两级保留策略 |
| SSE 带宽 | 单浏览器 10–15 KB/s（首页全量变化） | 变更集推送 + 只推变化节点 |
| 规模上限 | 设计目标 ≤200 节点单机；实测 50 | 再往上需要分片，v1 不做 |

---

## 15. 前端设计要点

- **视觉**：白/深两套 CSS 变量，跟随系统 + 手动切换（localStorage）。1px 边框、无阴影、无渐变、无装饰图标；颜色只用于状态和阈值；数字用等宽字体 + `tabular-nums`。
- **首页卡片**：名称 + 分组/地区、状态点、CPU/内存/磁盘三条细进度条、实时 ↑↓、本周期流量（设了额度时多一条额度进度条）、延迟、Uptime、最后通信时间。
- **三个视图 + 详情页**（同一个页面内按会话状态与 hash 路由切换）：首次初始化（输入日志里的一次性初始化码）、登录、首页、`#/n/<id>` 详情。
- **新增节点**：一个对话框表单；创建成功后弹窗显示 Token（只显示一次，带复制按钮）与 Agent 启动命令。
- **详情页**：左边信息表（状态、最后通信、实时/累计流量、今日/本周期/累计流量与计费周期、延迟、Uptime、24h/7d 可用率、CPU、内存、Swap、磁盘列表、负载、系统、内核、CPU 型号、网卡、出口地址、Agent 版本），右边 5 张跟随范围的小图（CPU、内存、磁盘、网络↑↓、延迟）+ 一张固定"近 7 天流量"图 + 右上角六档范围按钮。
- **两次降采样**：后端按 §10.3 给 ≤1000 点的桶；**手机端再做一次二次聚合**（目标间隔来自后端 `mobile_agg_sec`，只能变粗），用 `matchMedia('(max-width: 640px)')` 切换，PC 端不聚合。
- **图表**：自研 canvas 渲染（`web/chart.js`），固定高度、Y 轴"好看刻度"、X 轴标签钉在绝对时间网格上，悬浮/触摸显示该点读数与峰值；主题切换后重画。
- **局部更新**：SSE 每秒只推变化的节点，前端按 id 更新对应卡片（不整页重绘，输入框不会被打断）；详情页的实时字段跟着 SSE 走，图表每 30 秒整段刷新一次。
- **后台管理**：详情页右上角「编辑 / 换 Token / 删除」；编辑复用新增对话框（同一套字段与校验）；换 Token 与删除都走二次确认对话框；设置对话框里有通知、告警参数与只读的服务器信息；`#/audit` 是操作记录页（每页 50 条，"加载更多"按 `before_id` 翻页）。
- **hash 路由**：`#/`（首页）、`#/n/<id>`（详情）、`#/audit`（操作记录）；未登录时任何路由都会回到登录/初始化视图。
- **详情页**：上方信息表（CPU/内存/swap/磁盘/负载/实时↑↓/累计↑↓/本月/延迟/可用率 24h·7d/系统/内核/CPU 型号/IPv4/IPv6/Agent 版本）；下方 5 张固定高度小图（CPU、内存、磁盘、网络上下行、延迟），共享同一范围；点击某图放大。
- **范围控件**：固定分段控件 `1h 6h 12h 1d 3d 7d`，等宽、不换行、移动端字号 12px，位置固定不跳动。
- **两次降采样**：后端按 §10.3 给 ≤1000 点的桶；**手机端再做一次二次聚合**（目标间隔：不聚合 / 2m / 3m / 5m / 15m / 30m），用 `matchMedia('(max-width: 640px)')` 切换，PC 端不聚合。同一份接口数据同时服务 PC 与手机（含 `meta`），不做两套 API。
- **路由**：hash 路由（`#/`、`#/n/3`、`#/admin`），无需服务端 fallback 配置。
- **实时**：一条 SSE 连接；断线由 `EventSource` 自动重连；页面顶部显示"实时/已断开"小圆点。
- **无外链、无 CDN、无字体下载**（用系统字体栈），保证离线可用且不产生第三方请求。

---

## 16. 开发顺序与阶段门禁

每个阶段结束必须：`gofmt` + `go vet ./...` + `go test ./...`（必要时 `-race`）+ 手工跑一遍验证，全绿才进入下一阶段。**不做 TODO / 空实现 / 假数据**；测试里允许 fixture（如 `/proc` 采样文本），生产代码里不允许。

| 阶段 | 交付 | 门禁 |
|---|---|---|
| 1 | 本文档 + `PROTOCOL.md` | **你确认** |
| 2 | 骨架：参数、slog、DB 迁移、`/healthz`、embed 静态页、Makefile | 启动/退出干净；迁移可重入；`curl /healthz` |
| 3 | Agent 采集器 + `--print-json`（不需要 Server 就能自检） | /proc fixture 单测全绿；对着提交进仓库的 /proc 快照实跑，数值逐项核对 |
| 4 | 协议 + Agent WS 接入 + Token 鉴权 + hello/welcome + 1s metrics + ping/pong | 集成测试：正常/错 token/断线重连/Server 重启/限流/超大帧 |
| 5 | 登录与初始化 + 首页 + SSE 实时 + 新增节点（鉴权是首页的前置条件，因此提前） | 初始化/登录/限流/CSRF/同源各有用例；SSE 变更集与慢客户端不阻塞；真 Agent → SSE 端到端 |
| 6 | 详情页 + 6 档历史 + rollup + retention | 六档全部返回；桶宽/点数推导与定稿表逐项一致；可用率、保留清理、重启恢复各有用例 |
| 7 | 流量统计全链路 | §9.5 故障矩阵逐条测试（重启、清零、回绕、重装、Server 崩溃） |
| 8 | 告警全链路 + Telegram + 设置接口 | 规则/去抖/冷却/合并/重试/脱敏用例齐全；真实"断开→离线通知→重连→恢复通知"端到端验证 |
| 9 | 后台管理（节点编辑/删除/换 Token、设置页、操作记录） | 全部写操作都有鉴权/CSRF/审计用例；删除会清理历史与内存态；换 Token 立刻断开旧连接 |
| 10 | 安全审计 | 逐条核对 §13（结论见 `docs/SECURITY.md`）；`govulncheck` 零命中；发现并修掉 3 个问题：跨源 GET、缺少改密入口、SSE 无连接上限 |
| 11 | 性能 | 50 节点真实连接压测 + Go 基准；实测数字与结论见 `docs/PERFORMANCE.md`；顺带把"每 IP 连接上限"改成可配置（`--agent-max-per-ip`） |
| 12 | 安装/升级/卸载脚本 + systemd | `deploy/` 三个幂等脚本 + 加固单元；`deploy/deploy_test.go` 守住 LF、全部加固项、Token 不进命令行、下载必须校验哈希（脚本只能在 Linux 上实跑，见 `deploy/README.md`） |
| 13 | 端到端与长跑 | 50 节点 10 分钟长跑（RSS/CPU/库增长稳定）+ **硬杀（kill -9）后重启数据完好**；全量测试矩阵一遍过 |

---

## 17. 我认为不合理 / 有隐患 / 需要你拍板的地方

1. **"Agent 内存尽量低于 20MB、CPU 接近 0%"**：CPU 目标可达（实测预期 0.1–0.3%）；**内存 20 MB 是 Go 的合理下限区间，想稳到 5 MB 以内必须换 Rust/C**。我按"RSS ≤20 MB，稳态实际 8–15 MB"承诺，并在 README 写实测值。
2. **每秒上报全部指标，其实超出需求 10 倍**：历史曲线最终都来自聚合数据，1s 只在看"最近 1 小时"时有意义。我按你要求默认 1s，但做成可配（1–300s），并建议实际部署用 1s（50 节点总成本也就 30 KB/s）。
3. **"公网 IP" 与"不连接任何第三方"直接冲突**：不请求外部服务就拿不到公网 IPv4/IPv6。我的方案：① 用 Server 观察到的 Agent 源地址（这是你自己的服务器，不算第三方）回传；② 用本机接口的全球单播地址。**如果你要的是"外网看到的出口 IP"，那必须允许一次外部 HTTP 请求，请明确表态。**
4. **"国家/地区"同样无法自动获取**（IP 库要么联网要么带几十 MB 数据）。方案：管理员手填 + 首次注册时用 Agent 的时区给个建议值。首页用文字显示，不做国旗库。
5. **"延迟"定义要定死**：v1 = Agent↔Server 的应用层 RTT（含 ping 帧，无需 root、无需额外目标）。真实 ICMP ping 需要 root/`CAP_NET_RAW`，且默认目标会变成"第三方"，我**建议不做**；若你要"到公网某地址的 TCP 握手延迟"（无需 root、目标由你指定、默认关闭），我可以在 v1.1 加，约 40 行。
6. **月流量"重置日"我改成了会计周期而不是清零**（§9.4）：体验上一样，但历史永不丢、不会因为改配置而对不上账。若你坚持物理清零，请说，我会明确写出它会破坏历史一致性。
7. **最大的真实安全风险不是 SQL 注入，而是明文 HTTP 暴露后台**：单管理员 + 强密码在 HTTP 上等于没有安全。**建议 TLS 是默认路径**（安装脚本引导用 Caddy 自动 HTTPS），`--listen 0.0.0.0` 仅在你显式指定时生效并每次 WARN。这条我需要你确认。
8. **`curl | bash` 安装方式本身有供应链风险**：脚本会做校验和验证（sha256）、固定版本、支持 `--dry-run` 与手动安装文档，并**绝不**自动改防火墙、绝不 `set -e` 后 rm 大目录。端口需要你自己放行（我不会代你改防火墙）。
9. **`--token TOKEN` 会让 token 出现在 `ps`/`/proc/*/cmdline`**：我按你的接口保留它，但同时提供 `--token-file`（推荐给安装脚本用，权限 0600）。
10. **"完全不写 placeholder"与分阶段开发有张力**：我承诺功能只有"实现"或"不存在"两种状态——接口不通就不出现在 UI 上；绝不用假数据糊页面。测试 fixture 不算假数据。
11. **SQLite 必须放本地盘**：NFS/SMB 上锁语义不完整会导致损坏，文档会写死这条。
12. **50 节点是合理目标，但别指望它变成 5000 节点**：单进程内存态 + 每浏览器一条 SSE，实测到几百节点没问题；再大就要分片，v1 明确不做。
13. **"不做复杂权限系统"我理解为**：单管理员、无角色、无多租户——但**必须有**：强密码、会话、CSRF、登录限流、setup code。这不是"复杂权限系统"，是最低安全线，我不会砍。
14. **12 个阶段里 Phase 13 才做完整测试太晚**：我把测试拆到每个阶段的门禁里（§16），Phase 13 只做"端到端 + 长跑"，不是第一次跑测试。
15. **你列的 `users/nodes/metrics/notifications` 表设计我没有照抄**：`metrics` 拆成 10s / 1m 两级桶（否则长期膨胀或查询爆炸），`notifications` 拆成 `alert_state`（去重状态）+ 日志（可选），`users` 不需要表（单管理员放 `settings`）。理由见 §6。
16. **图表规格已按你的表落地**（§10.3，范围 1h/6h/12h/1d/3d/7d）：后端每档 ≤1000 点（实际 360–864），手机端二次聚合到 180–360 点，X 轴标签 3–6 个。三条硬约束：**不插值造点、不超 1000 点、标签不重叠**。原表里的"15m/2h/3h/6h/1d/2d"和"不聚合/2m/3m/5m/15m/30m"全部原样保留，只有 1h 档的刻度按你的选择从 15m 改成 10m。
17. **「≤1000 点」对 PC 是浪费，对手机是灾难**：1000 点在 ~350px 宽的手机上等于每像素约 3 个点（overplotting），所以手机端二次聚合必需；PC 宽屏保留后端点数，不做额外抽稀。
18. **`samples_1h` 长历史层已按你的决定删除**：六档最长只有 7d（由 1m 层供数），所以不留这张表、不留 rollup 任务、不留开关——将来要加 30d/1y 图表时，加一个迁移建表即可。副作用：「30 天可用率」不存在了，详情页的可用率只显示 24h / 7d（这两个由现有数据直接算）。

---

## 18. 附录 A：Server 命令行参数（初稿）

```
--listen 127.0.0.1:25774      # 默认只本地
--data-dir /var/lib/probe-server
--tls-cert / --tls-key        # 可选，直接提供 TLS
--trusted-proxy 127.0.0.1     # 只有配置了才信任 X-Forwarded-For
--log-level info              # debug/info/warn/error
--timezone Local              # 日流量/周期归属时区，默认服务器本地
--stale-after 10s  --offline-after 30s
--retention-10s 12h --retention-1m 192h
--flush-interval 10s          # 内存聚合落盘周期（1s-60s）
--traffic-delta-max 1TiB
--alert-cooldown 30m --alert-startup-grace 60s
--alert-debounce 2s --alert-recover-stable 30s
--setup-code-ttl 30m
```

## 19. 附录 B：Agent 命令行参数（初稿）

```
--server https://monitor.example.com     # 必填，默认要求 https
--token-file /etc/probe-agent/token      # 推荐（0600）
--token TOKEN                            # 兼容你要求的用法（会出现在 ps 中，不推荐）
--name "HK-01"                           # 可选，仅首次注册显示名建议
--interval 1s                            # 采集/上报间隔
--iface auto                             # auto=默认路由网卡
--disk /                                 # 主文件系统
--state-dir /var/lib/probe-agent
--root /                                 # 根目录，其下应有 proc/ 与 etc/（诊断时可指向一份根文件系统快照）
--insecure-skip-verify                   # 关闭 TLS 校验（不推荐，文档标注风险）
--log-level info
--print-json                             # 打印一次采集结果后退出（自检/排障）
--samples 1                              # 配合 --print-json：连续打印几份采样
--once                                   # 上报一次后退出（排障）
```

## 20. 附录 C：HTTP API 一览（全部 JSON，错误统一 `{"error":{"code","message"}}`）

```
GET    /healthz                          公开，仅 {"ok":true,"version":...}
GET    /api/v1/session                   公开：{needs_setup, authenticated, username, csrf_token}
POST   /api/v1/setup                     首次初始化（需日志里的一次性初始化码）
POST   /api/v1/auth/login  /logout
GET    /api/v1/nodes                     列表 + 最新状态（内存）+ 集群汇总；★ 不返回 Token
POST   /api/v1/nodes                     新增节点；★ 响应里的 token 只出现这一次
GET    /api/v1/nodes/{id}                详情：视图 + 24h/7d 可用率 + 六档参数
GET    /api/v1/nodes/{id}/series?metric=cpu&range=1h
       → {"meta":{"key":"1h","bucket_sec":10,"points":360,"tick_base_sec":600,
                  "tick_label_sec":600,"mobile_agg_sec":0,"source":"samples_10s"},
          "points":[[ts,avg,max],…]}      // 任意档位 points ≤ 1000，桶宽/刻度/手机聚合目标均由后端给定
GET    /api/v1/nodes/{id}/traffic?days=7  按天流量 + 今日/本周期/累计
       → {"days":7,"points":[[本地零点ts,rx,tx],…],"today":{rx,tx},
          "cycle":{rx,tx,start,end},"total":{rx,tx},"limit":…}
PATCH  /api/v1/nodes/{id}                编辑节点（**整体替换**语义：要带全部字段，
                                         缺字段返回 400 而不是悄悄改成默认值）
DELETE /api/v1/nodes/{id}                删除节点（连带历史、流量、告警状态一起删）
POST   /api/v1/nodes/{id}/token          重新生成 Token；★ 新 Token 只返回这一次，旧连接立刻断开
GET    /api/v1/audit?limit=100&before_id=  操作记录（时间倒序，最多保留 2000 条）
GET    /api/v1/settings                  服务器信息（只读）+ 告警参数
PUT    /api/v1/settings/alert            改告警参数（立刻生效，不用重启）
GET    /api/v1/stream                    SSE，1 Hz 推变更集（Cookie 鉴权）
GET    /api/v1/agent/ws                  Agent WebSocket（Bearer Token 鉴权，不参与会话/CSRF）
GET    /                                 前端 SPA（go:embed）

GET/PUT /api/v1/settings/telegram       通知设置（GET 不返回 Token，只回 has_token）
POST   /api/v1/settings/telegram/test   立刻发一条测试通知
```

写操作（POST/PUT/PATCH/DELETE）必须带 `X-CSRF-Token` 头（值取自 `/api/v1/session` 或登录/初始化响应），
并且 `Origin`（若存在）必须与 `Host` 同源。CSRF Token 由会话 Token 派生（`sha256(token + "\x00probe-csrf")`），
不额外存储、不额外查询。
