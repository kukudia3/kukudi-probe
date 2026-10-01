# Agent ↔ Server 通信协议 v1

状态：**待确认**（Phase 4 实现，Phase 1 只冻结设计）

设计目标：简单、安全、可版本升级、可扩展、低开销。
明确不做：RPC 框架、代码生成、双向流多路复用、二进制编码。

---

## 1. 传输

| 项 | 值 |
|---|---|
| URL | `wss://<host>/api/v1/agent/ws`（`ws://` 仅允许 `--allow-plaintext` 的本地调试场景） |
| 协议 | WebSocket，子协议 `probe.v1`（Upgrade 时带 `Sec-WebSocket-Protocol`） |
| 帧类型 | 文本帧（UTF-8 JSON） |
| 单帧上限 | Agent→Server 16 KB；Server→Agent 64 KB（下发配置留余量） |
| 压缩 | 关闭（帧本来就只有 ~0.5 KB；压缩增加 CPU 与攻击面） |
| 心跳 | 应用层 `ping`/`pong`（见 §5.4）；不用 WS 控制帧承载业务语义 |
| 保活 | TCP keepalive 关闭，由应用层 ping 每 5s 维持 |

反向代理（nginx/Caddy）必须：`proxy_read_timeout ≥ 120s`、`Upgrade`/`Connection` 头透传、关闭响应缓冲。文档给现成片段。

---

## 2. 鉴权

- 升级请求必须带 `Authorization: Bearer pba_<43 字符 base64url>`。
- **Token 不放在 URL**（URL 会进反代/浏览器日志）；服务端日志对 `Authorization` 一律脱敏。
- Server 侧：`sha256(token)` 查 `nodes.token_hash`（唯一索引），恒定时间比较；查不到 → 在**升级前**回 `401`。
- Token 生成：32 字节 CSPRNG，前缀 `pba_`；只在创建时显示一次；重新生成后旧 token **立即失效**（现存连接在下一个消息或 5s 内被 `4401` 关闭）。
- 失败限流：同一 IP 每分钟最多 10 次鉴权失败，超出直接 `429`，并计入审计日志（不含 token 内容）。

---

## 3. 消息信封

```json
{ "v": 1, "t": "metrics", "ts": 1712345678, "seq": 12345, "d": { } }
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `v` | int | 是 | 协议版本，v1 = 1 |
| `t` | string | 是 | 消息类型，见 §5 |
| `ts` | int64 | 否 | Unix 秒；Server 用它做参考，**判定在线只用本机到达时间** |
| `seq` | uint64 | 否 | Agent 自增序号，用于检测丢帧（Server 记录 `gap` 计数） |
| `d` | object | 否 | 各类型自己的负载 |

规则：
- 未知字段**忽略**（向前兼容）；已知字段类型/范围**严格校验**，不合法整帧丢弃并计数（不关连接，连续 30 次不合法才关闭）。
- 未知 `t` → 回 `error{code:"unknown_type"}`，不关连接（方便未来加类型时老 Server 优雅降级）。
- 数值：拒绝 `NaN`/`Inf`；比率字段 clamp 到 `[0,100]`；字节计数字段须 `≥0` 且 `< 2^53`。

---

## 4. 版本协商

- `hello.v` 为 Agent 协议的**主版本**；Server 配置 `min_proto=1`、`max_proto=1`。
- 超范围 → `error{code:"upgrade_required", d:{min,max,server_version}}` 然后以 `4426` 关闭；Agent 打印明确日志（当前版本/要求版本/升级链接），退避重试（最长 30 分钟一次）。
- 版本内演进规则：**只允许加可选字段**；删字段、改语义、改类型必须升 `v`。
- 独立于协议的 `agent_version`（如 `0.3.1`）随 `hello` 上报，只用于展示与兼容性排查。

---

## 5. 消息类型

### 5.1 `hello`（Agent→Server，连接后第一帧，超时 10s 未收到则 `4408` 关闭）

```json
{ "v":1, "t":"hello", "d":{
  "agent_version":"0.3.1",
  "hostname":"hk-01",
  "os":{"name":"Debian GNU/Linux 12","kernel":"6.1.0-13-amd64","arch":"amd64"},
  "cpu":{"model":"AMD EPYC 7542","cores":4},
  "boot_id":"9f1c...","uptime_sec":123456,
  "iface":{"name":"eth0","ifindex":2,"mac":"52:54:00:aa:bb:cc"},
  "interval_sec":1,
  "state":{"ckpt_age_s":4,"total_rx":8123456789,"total_tx":1234567890},
  "local_ip":"203.0.113.7",
  "local_ip6":"2001:db8::7"
}}
```

- `local_ip` / `local_ip6`：**Agent 自己采集的本机地址**（可选，两者都可以缺）。
  取法是"不发包的 UDP connect"：建一个 UDP 套接字 connect 到服务端地址，读
  `LocalAddr` —— 内核会告诉我们"去服务端会用哪个源地址"，**全程没有报文发出**，
  所以既不依赖任何第三方服务，也不受 NAT/代理影响（拿到的是机器自己的地址）。

  它与 `welcome.observed_ip` **互补，不是一回事**：

  | | 含义 | 什么时候会误导 |
  |---|---|---|
  | `observed_ip` | Server 从 TCP 连接上看到的来源地址 | Agent 在 NAT 后面、或与 Server 同机且走隧道/反代时 |
  | `local_ip` | Agent 自己报的机器地址 | 基本不会；它只说"我是谁" |

  采集只用 `AF_INET`/`AF_INET6`：**刻意不用 `net.Interfaces()`** —— Linux 上它走
  `AF_NETLINK`，而 Agent 的 systemd 单元把 `RestrictAddressFamilies` 限制成
  `AF_INET AF_INET6 AF_UNIX`，调用会直接失败。采集失败一律静默留空，
  绝不因为它影响连接。

### 5.2 `welcome`（Server→Agent）

```json
{ "v":1, "t":"welcome", "d":{
  "node_id":3,
  "name":"HK-01",
  "interval_sec":1,
  "server_time":1712345678,
  "observed_ip":"203.0.113.7",
  "config_version":7,
  "notes":""            // 可选：给管理员的提示（如"请升级 Agent"）
}}
```

- `observed_ip`：Server 从 TCP 连接上观察到的 Agent 源 IP（不涉及第三方）。
  它能反映"这台机器出站时的公网地址"，但 Agent 在 NAT 后面、或与 Server 同机且
  走隧道/反代时会失真（后者恒为回环地址）—— 所以详情页把它显示为「来源 IP」，
  与 Agent 自报的「本机地址」（`hello.local_ip` / `local_ip6`）并列，一眼能分辨。
- `interval_sec` 由服务端下发（取节点配置），Agent 之后按此节奏上报；变更通过 `config` 推送。
- `config_version` 与**紧随其后**那一帧 `config` 同号：welcome 之后 Server 一定会
  再发一帧完整的 `config`（见 §5.5），Agent 不需要区分这两帧的版本语义。

### 5.3 `metrics`（Agent→Server，每 `interval_sec` 一帧，默认 1s）

```json
{ "v":1, "t":"metrics", "ts":1712345679, "seq":1001, "d":{
  "cpu_pct":12.5, "cpu_cores":4,
  "mem":{"total":8589934592,"used":2147483648,"pct":25.0},
  "swap":{"total":2147483648,"used":1048576,"pct":0.05},
  "disk":[{"mount":"/","fs":"ext4","total":107374182400,"used":53687091200,"pct":50.0}],
  "load":{"l1":0.42,"l5":0.38,"l15":0.31},
  "net":{"iface":"eth0",
         "rx_total":8123456789,"tx_total":1234567890,   // 探针长期累计（权威）
         "rx_raw":9000000000,"tx_raw":1300000000,       // 内核 counter（诊断）
         "rx_rate":1048576.0,"tx_rate":524288.0,        // bytes/s
         "boot_id":"9f1c...","ckpt_age_s":6},
  "lat_ms":23.4,
  "uptime_sec":123457,
  "dropped":0,                                          // Agent 因发送阻塞跳过的采集拍数（累计）
  "gap":0,                                              // Server 观测到的序号缺口（由服务端填写，Agent 恒为 0）
  "pings":[                                             // 可选：各探测目标"最近一次"的结果
    {"target_id":1,"avg_ms":23.4,"min_ms":20.1,"max_ms":31.2,"loss_pct":0}
  ]
}}
```

要点：
- `rx_total/tx_total` 是**单调累计**（§9 of DESIGN），Server 用它做幂等增量；`rx_raw/tx_raw` 只作诊断。
- `disk` 最多 8 项（`/` 优先，其余按用量降序）；历史只存 `mount=="/"`（或 `--disk` 指定）的那一项，其余只在实时详情里显示。
- `dropped` 由 Agent 上报（本机来不及发的拍数），`gap` 由 Server 观测（收到的帧序号不连续）——两者互补，让"数据有洞"可见，而不是静默丢帧。
- `lat_ms` 是 Agent ↔ **面板自身**的 WebSocket 往返（走 Cloudflare 隧道时会包含绕行时间），
  它与"这台机器到公网的延迟"无关；后者由 `pings`（可配置的探测目标）提供，两者不要混用。
- `pings` 的语义是"**最近一次**探测的结果"：探测按 `ping_interval_sec` 进行（默认 60s），
  而 metrics 每秒一帧，所以同一个值会连续出现在多帧里 —— 这是有意的（曲线呈阶梯状，
  前端不必补点）。从未探到结果的目标（例如 ICMP 没有 `CAP_NET_RAW` 权限而被留空）
  不出现在数组里；一轮探测全丢时 `avg/min/max` 为 0、`loss_pct` 为 100。
- 条目上限 16，`min_ms <= avg_ms <= max_ms`，三个耗时都必须在 `[0, 600000]` 且为有限值。

### 5.4 `ping` / `pong`（双向，测 RTT + 保活）

```json
{ "v":1,"t":"ping","ts":1712345680,"d":{"ts_us":1712345680123456} }
{ "v":1,"t":"pong","ts":1712345680,"d":{"ts_us":1712345680123456} }
```

- Agent 每 5s 发一次 `ping`，Server 立即回 `pong`（原样回 `ts_us`）；Agent 计算 `lat_ms = (now - ts_us)/1000`，在下一帧 `metrics` 里上报。
- Server 也可以在 15s 没收到任何帧时主动发 `ping` 探测（用于区分"网络断了"与"Agent 卡了"）。
- 语义：`metrics` 到达也算心跳；`ping/pong` 只是让延迟指标有来源、并让"仅延迟"这种轻量场景保持连接活性。

### 5.5 `config`（Server→Agent，配置变更推送）

```json
{ "v":1,"t":"config","d":{
  "config_version":8,
  "interval_sec":5,
  "iface":"eth0",
  "reload":false,
  "ping_targets":[{"id":1,"type":"tcp","host":"1.1.1.1","port":443}],
  "ping_interval_sec":60
}}
```

- **什么时候发**：① 握手时紧跟 `welcome` 之后必发一帧（把该节点的完整配置交给 Agent）；
  ② 管理员在设置里改了探测目标/间隔、或改了节点的上报间隔时主动推一帧，不必等 Agent 重连。
- `config_version` 每次下发 +1（同一个进程内单调递增）；Agent 按版本号丢弃过期的配置，
  并且**每个连接**从 0 重新计（服务端重启后版本号从头开始，Agent 重连时也会重置）。
- Agent 应用后回 `{"t":"ack","d":{"config_version":8}}`；不合法（枚举/长度/数值越界）
  或版本过期的 config 整帧丢弃、不回 ack。
- `interval_sec` / `ping_interval_sec` 为 0 表示"本次不改"；`ping_targets` 里只会出现
  `enabled=true` 的目标（停用的目标不下发，Agent 不该为它花流量）。
- `ping_targets` 的 `id` 由服务端分配、**创建后永不变更**（前端靠它认曲线），
  `type` 只有 `icmp` / `tcp`，`port` 仅对 `tcp` 有意义。上限 16 个目标。
- `reload`（v1 恒 false）为未来"重读本地配置"预留。

### 5.6 `error`（双向）

```json
{ "v":1,"t":"error","d":{"code":"bad_frame","message":"cpu_pct out of range","fatal":false} }
```

错误码表（v1，固定集合）：

| code | 场景 | fatal |
|---|---|---|
| `bad_frame` | JSON 解析失败/字段非法 | false（累计超限则 true） |
| `unknown_type` | 未知 `t` | false |
| `rate_limited` | 超出 5 msg/s | false |
| `too_large` | 超过帧上限 | true |
| `upgrade_required` | 协议版本不兼容 | true |
| `unauthorized` | Token 无效/已轮换 | true |
| `node_disabled` | 节点被停用 | true |
| `interval_invalid` | 间隔超范围（1–300s） | false |
| `internal` | 服务端内部错误（不含细节） | false |

### 5.7 WebSocket 关闭码

| 码 | 含义 | Agent 行为 |
|---|---|---|
| 1000 | 正常关闭（服务端重启） | 立即退避重连 |
| 1009 | 帧超过上限（服务端 read limit） | 退避重连 |
| 4400 | 帧/信令错误（含二进制帧、连续非法帧） | 快速重连 3 次后转慢速退避 |
| 4401 | 鉴权失败 | 慢速重试（5 min），日志提示检查 token |
| 4403 | 节点停用 | 慢速重试（10 min），日志提示已被管理员停用 |
| 4408 | hello 或心跳超时（静默超过 max(30s, 3×间隔)） | 退避重连 |
| 4426 | 协议需升级 | 30 min 重试 + 明确日志 |
| 1011 | 服务端内部错误 | 退避重连 |

握手前的拒绝用 HTTP 状态码表达，Agent 据此选择退避：`401`（无/错 Token）、`403`（节点停用）、`429`（同来源连接过多）、`503`（服务端连接数已满）。

---

## 6. 速率、背压与资源保护

| 侧 | 限制 |
|---|---|
| Server 收 | 每连接 5 msg/s 令牌桶；单帧 ≤16 KB；连续 30 帧非法 → 关闭 |
| Server 收 | 每 IP 并发 Agent 连接 ≤ 20；总连接数上限可配（默认 500，超出回 `503`） |
| Server 推 | 写超时 5s；慢连接直接关闭（触发 Agent 重连，天然自愈） |
| Agent 发 | **同步发送，没有队列、没有缓存**：一次只发一帧，写完才继续下一拍；发送阻塞导致跳过的拍数计入 `dropped`。**绝不阻塞采集循环、绝不无限缓存** |
| Agent 缓存 | Server 不可达时**只保留**：本地流量 checkpoint、当前序号；**不缓存历史 metrics**（瞬时指标没有补传价值） |
| Server 内存 | 每节点状态固定大小（最新值 + 900 点环形缓冲 + 桶累加器），与断线时长无关 |

---

## 7. 重连状态机（Agent）

```
INIT ──dial──▶ TLS(证书校验失败→FATAL 日志并退出，除非 --insecure-skip-verify)
  │
  ├─ 401/4401 ──▶ AUTH_FAILED：5min 退避重试（不刷日志，每 10 次打一条）
  ├─ 4426     ──▶ VERSION_MISMATCH：30min 退避重试
  ├─ 其他失败 ──▶ BACKOFF：1,2,4,...,60s 上限，±20% 抖动
  └─ welcome  ──▶ RUNNING：定时 metrics/ping；任何写失败 → BACKOFF(重置为 1s 前先等 1 个间隔)
```

- 退避期间**不采集上报**（省资源），但流量 checkpoint 仍按 10s 落盘（保证连续性）。
- `RUNNING` 期间收到 `config` 立即生效，无需重连。
- Server 重启：所有 Agent 在 1–60s 内自动回归（抖动避免惊群）。

---

## 8. 一次典型会话

```
Agent                                          Server
  │  GET /api/v1/agent/ws  (Upgrade, Bearer)      │
  │ ─────────────────────────────────────────────▶│ 校验 token → node=3
  │ ◀──────────────────────────────────────────── │ 101 Switching Protocols
  │  hello {agent_version, os, iface, totals}     │
  │ ─────────────────────────────────────────────▶│ 校验版本 → 绑定节点 → 写内存状态
  │ ◀──────────────────────────────────────────── │ welcome {node_id:3, interval:1, observed_ip}
  │ ◀──────────────────────────────────────────── │ config  {config_version:8, ping_targets, ping_interval_sec}
  │  ack {config_version:8}                        │
  │ ─────────────────────────────────────────────▶│
  │  metrics #1 (1s 后，含 pings 最近一次结果)      │
  │ ─────────────────────────────────────────────▶│ 内存更新 + 桶累加 + 流量增量 + 探测结果每分钟落一行
  │  metrics #2 ...                                │
  │  ping (每 5s)                                  │
  │ ─────────────────────────────────────────────▶│
  │ ◀──────────────────────────────────────────── │ pong
  │ ◀──────────────────────────────────────────── │ config（管理员改了探测设置时主动推）
  │        … 网络抖动 3s（无帧）…                    │ 状态短暂 STALE，不告警
  │  metrics #N 恢复                                │ 状态回 ONLINE
```

---

## 9. 兼容性与演进

- 加字段：直接加（老端忽略）。改字段语义：升 `v`。
- 保留的扩展位：`d.caps`（能力位图，v1 空闲）、`d.meta`（键值对，v1 空闲）。
- 未来可能需要的新类型（**现在不实现**）：`traffic_daily_report`（Agent 本地按天累计，用于精确归属）、`probe_result`（多探测点），都可在 v1 加可选字段/新 `t` 而不破坏兼容。
