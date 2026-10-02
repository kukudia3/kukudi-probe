# 安全设计说明（Phase 10 审计结论）

本文记录**实际做了什么、用什么证明、还剩哪些已知风险**。
所有"证明"都是仓库里可复现的测试或用例名（`go test ./...` 就会跑）。

---

## 1. 威胁模型

**防护目标**

- 面板被暴露到公网时，未授权的人**不能**看到任何节点信息、不能改任何配置；
- 即使某个 Agent 的 Token 泄漏，影响范围也只限于**那一个节点**（不能读别人的数据、不能动服务端）；
- 浏览器侧不能因为节点名/分组名等**用户可控文本**而执行脚本（XSS）或伪造日志；
- 服务端**不主动连接**除 Telegram 之外的任何第三方（无遥测、无 CDN、无广告）；
- 单个恶意/失控的客户端不能让服务端内存或磁盘失控（限流、上限、超时）。

**明确不防（也不打算防）**

- 拿到 root 的攻击者：SQLite 文件是明文的，本地 root 能读库、能读 Token 文件；
- 中间人：默认监听 127.0.0.1，公网暴露**必须**由 Caddy/nginx 提供 TLS（见 §6）；
- 物理/主机层面的事故、宿主机被入侵、备份泄露；
- 针对管理员的钓鱼、密码复用等社工手段（v1 没有 2FA，这是有意的取舍，见 §5）。

---

## 2. 控制点与证明

| 面向 | 做法 | 证明（用例名） |
|---|---|---|
| 密码存储 | Argon2id `m=64MiB, t=3, p=1`，PHC 字符串；重哈希并发限制为 2（防内存打爆） | `TestPasswordHashRoundTrip` |
| 密码强度 | 至少 10 位、不能等于用户名；改密必须验证当前密码且新旧不能相同 | `TestSecurityPasswordChangeValidation`、`TestSetupRejectsWrongCodeAndWeakPassword` |
| 登录 | 用户名与密码比较都走恒定时间；失败与用户名不存在返回同一条消息 | `TestLoginFlowAndLogout`、`TestSecurityPasswordChangeValidation` |
| 暴力破解 | 登录：每 IP 5 次/分钟，累计失败 10 次锁 15 分钟；初始化码：每 IP 5 次/10 分钟，累计 10 次锁 30 分钟 | `TestLoginRateLimitAndLockout`、`TestSetupIsRateLimited`、`TestAttemptLimiterWindowAndLock`、`TestAttemptLimiterBoundsEntries` |
| 会话 | 32 字节随机 Token，数据库只存 SHA-256；7 天滑动过期；Cookie `HttpOnly`+`SameSite=Lax`+`Secure`(TLS)+`Path=/` | `TestSecuritySessionsAreHashedAtRest`、`TestSessionCookieAttributes`、`TestSessionExpiryIsEnforced`、`TestSecurityExpiredSessionIsRejected` |
| 会话注销 | 登出即删；**改密会注销其它所有会话**（只留当前这条） | `TestSecurityPasswordChangeRevokesOtherSessions`、`TestLoginFlowAndLogout` |
| 初始化 | 12 位十六进制一次性码，只在日志出现一次、30 分钟有效（`TestSetupCodeExpiry`）、**任何接口都不返回** | `TestSecuritySetupCodeNeverReturnedByAPI`、`TestSetupCreatesAdminAndSession` |
| CSRF | 所有写操作要求 `X-CSRF-Token = sha256(session‖"probe-csrf")`，恒定时间比较 | `TestSecurityAllMutatingEndpointsRequireCSRF`、`TestCSRFAndOriginChecks`、`TestCSRFDerivationIsStableAndUnpredictable` |
| 跨源 | `/api/` 下**所有**请求（含 GET）都校验 `Origin` 与 `Host` 一致；无 Origin 的非浏览器客户端放行 | `TestSecurityCrossOriginRequestsAreRejected` |
| XSS | 前端只使用 `textContent`/DOM API，禁止 `innerHTML`；CSP 无 `unsafe-inline`/`unsafe-eval`；响应 `nosniff` | `TestFrontendAvoidsInnerHTML`、`TestSecurityHeaders`、`TestSecurityHostileNodeNameIsStoredVerbatimAndNotExecuted` |
| 点击劫持 | `X-Frame-Options: DENY` + CSP `frame-ancestors 'none'` | `TestSecurityHeaders` |
| 日志伪造 | 节点名等用户可控值经过 slog 转义，换行不会伪造出新的日志行 | `TestSecurityLogInjectionIsEscaped` |
| 敏感值不外泄 | 日志里不出现密码、Agent Token、会话 Cookie；Telegram Token 不进错误信息；接口只回 `has_token` | `TestSecuritySensitiveValuesNeverReachLogs`、`TestTelegramErrorsNeverLeakToken`、`TestTelegramSettingsAPI` |
| SQL 注入 | 全部参数化；表名/指标名来自白名单常量，绝不拼接用户输入 | `TestInsertBucketsRejectsUnknownTable`、`TestQuerySeriesRejectsUnknownMetric`、`TestQuerySeriesBucketsAndLimits` |
| 命令注入 | 服务端与 Agent**都不执行任何外部命令**（无 `os/exec`） | 代码扫描：`grep -r "os/exec" internal cmd` 无结果；Agent 只读 `/proc`、`/etc` 与 `statfs` |
| 路径遍历 | 前端资源来自 `go:embed`；数据目录来自启动参数；Agent 的 `--root` 用斜杠语义 `path.Clean` 归一化 | `TestSecurityPathTraversalOnStaticFiles`、`TestStaticAssetsAreServed` |
| DoS（HTTP） | 请求体上限、读写超时、panic 恢复、结构化请求日志 | `TestSecurityRequestBodyLimit`、`TestPanicIsRecovered`、`TestUnknownAPIPathReturnsJSONError` |
| DoS（SSE） | 全局 32 条、每 IP 8 条；每客户端"容量 1 的最新值槽"；15 秒 ping、10 秒写超时 | `TestSecuritySSEConnectionCap`、`TestHubBroadcastKeepsOnlyLatestForSlowClient`、`TestBroadcastDoesNotBlockOnStuckClient` |
| DoS（Agent） | 帧上限 16 KB、每 IP 20 条连接、每连接每秒 5 帧、hello 超时、空闲超时 | `TestAgentOversizeFrameIsRejected`、`TestAgentPerIPConnectionLimit`、`TestAgentRateLimitDropsExcess`、`TestAgentHelloTimeout`、`TestAgentTooManyBadFramesClosesConnection` |
| Agent 凭据 | Token 32 字节随机（`pba_` 前缀），只存 SHA-256；**只走 Authorization 头**（URL 里不接受，避免进日志/代理记录）；停用的节点拒绝连接 | `TestSecurityAgentTokenNotAcceptedInQueryString`、`TestAgentRejectsBadToken`、`TestAgentRejectsDisabledNode`、`TestCreateNodeReturnsTokenOnce` |
| 传输安全 | Agent 拒绝向远端使用明文 http（回环例外，便于自测）；服务端默认只监听 127.0.0.1 | `TestSecurityAgentRejectsPlainHTTPToRemoteHost`、`TestClientRejectsPlaintextForNonLoopback`、`TestSecurityServerDefaultsToLoopback`、`TestLoopbackListen` |
| Agent 权限 | 非 root 用户运行，**只**持有 `CAP_NET_RAW`（ICMP 原始套接字所需），其余加固项不变 | `TestUnitsGrantOnlyCapNetRawToAgent`、`TestUnitsCarrySecurityHardening` |
| 供应链 | 只有 3 个直接依赖，全部是纯 Go；零前端第三方代码（图表自研） | `go.mod`、`TestFrontendHasNoExternalResources` |
| 访客只读（1.1.0） | 默认**关**的总开关。打开后未登录的人只能读 7 条读接口，响应经**服务端白名单**脱敏（本机/来源 IP、备注、boot_id、探测目标地址**根本不存在**）；所有写接口、`/audit`、`/settings` 与开关无关，永远需要会话；SSE 走同一个脱敏函数 | `TestGuestSwitchDefaultsToOff`、`TestEveryRouteIsProtected`、`TestAllRoutesGoThroughRouteTable`、`TestGuestReadsAreRedacted`、`TestGuestStreamSnapshotIsRedacted`、`TestGuestCannotWriteAnything`、`TestNodeDTOFieldsAreClassified`、`TestGuestReadOnlyInRealBrowser` |
| 访客限流（1.1.0） | 未登录的读请求按来源 IP 300 次/分钟（管理员不受影响），超限 429 + `Retry-After` | `TestGuestReadsAreRateLimited`、`TestGuestRateLimitedResponseIsJSONEnvelope` |
| 不被搜索引擎收录 | `/robots.txt`（`Disallow: /`）+ `<meta name="robots">` + 每个响应的 `X-Robots-Tag: noindex, nofollow, noarchive` | `TestRobotsAndNoIndexHeader` |

---

## 3. 本次审计发现并修复的问题

| # | 问题 | 风险 | 修复 |
|---|---|---|---|
| 1 | **跨站 GET 未被拒绝**：`/api/` 的同源校验只作用在写操作上 | 跨站脚本无法读到响应（没有 CORS 头），但"受理"本身就是不必要的暴露面；将来一旦有人加了宽松 CORS 头就会变成真漏洞 | 同源校验改为覆盖 `/api/` 下**所有**请求（`internal/server/auth.go`） |
| 2 | **缺少改密入口**：密码一旦泄漏，管理员无法在界面里轮换 | 只能删库重建，或者接受"密码永远是初始那个" | 新增 `POST /api/v1/auth/password`（验证当前密码 + CSRF + 改完注销其它会话）与设置页入口 |
| 3 | **SSE 连接没有上限** | 一条泄漏的会话就能开几千条连接，把内存与文件描述符吃光 | `hub` 增加全局 32 / 每 IP 8 的上限，超限直接 503 `too_many_streams` |

### 3.1 第二轮全量走查（11 个单元并行审查 + 对抗式复核）发现并修复

| # | 问题 | 风险 | 修复 | 守住的用例 |
|---|---|---|---|---|
| 4 | **反代终止 TLS 时 Cookie 丢 `Secure`**：只看 `r.TLS`，而 Caddy/nginx 后面它恒为 nil | 会话 Cookie 会被浏览器附到同主机的 `http://` 请求上，中间人可直接读走会话 | `cookieSecure()`：仅当直连对端在 `--trusted-proxy` 内**且** `X-Forwarded-Proto: https` 时才加 `Secure`，其余情况保守不加 | `TestCookieSecureBehindProxy` |
| 5 | **转发头可伪造限流键**：从右往左找"第一个不可信地址"，客户端自己写的值可能被采信 | 攻击者换个假 IP 就是一个新的登录限流桶，等于绕过暴力破解防护 | 改为**只采信最近一跳**追加的那条（最右）；最右一条本身在可信网段时返回空、退回直连地址 | `TestForwardedIPOnlyTrustsTheLastHop`、`TestClientIPFallsBackToRemoteWhenAllForwardedTrusted` |
| 6 | **数据目录 0750**：同组用户可穿越进入，而 SQLite 自己创建的 `-wal`/`-shm` 从未收紧 | WAL 里含最近的提交页（密码哈希、会话、审计），同组用户可直接读走 | 数据目录改为 **0700**（启动时 chmod 收紧），主库仍 0600 | `TestOpenCreatesSchemaAndIsIdempotent` + systemd 单元 `UMask=0077` |
| 7 | **限流表满时整体清空** | 攻击者用 IP 池灌满 1024 条即可顺手清掉所有人的锁定期（含 15 分钟登录锁、30 分钟初始化锁） | 改为每次淘汰一条：优先最久未出现且未锁定，其次最早解锁 | `TestLimiterKeepsLockoutsWhenTableIsFull` |
| 8 | **并发初始化可覆盖**：检查"无管理员"与写入之间隔着约 100ms 的 Argon2 | 双击/并发提交时后写者会静默覆盖先建好的管理员密码 | `CreateAdminIfAbsent` 用设置表的唯一键做哨兵，只有一个请求能建成，另一个拿 409 | `TestSetupIsNotRaceable` |
| 9 | **等 Argon2 时不可取消** | 客户端断开后请求仍在排队，占着 64MiB 的哈希名额 | 信号量等待改为 `select` + `ctx.Done()` | `TestVerifySemaphoreRespectsContext` |
| 10 | **长度校验按字节、提示按"位"** | 4 个汉字的密码能骗过"至少 10 位"；22 个汉字的用户名被误判超长，提示自相矛盾 | 用户名/密码/节点字段统一按 rune 计数 | `TestUnicodeLengthLimits` |
| 11 | **改密策略与初始化不一致**：改密不检查"密码≠用户名"；注销其它会话失败仍回 200 | 管理员以为入侵者已被踢出，实际其它设备仍处于登录状态 | 改密复用 `validateCredentials`；注销失败回 500 `revoke_failed` 并明确告知 | `TestChangePasswordRejectsUsernameAsPassword` |

另外复核确认（未发现问题）：无 SQL 拼接、无外部命令执行、无 `innerHTML`、CSP 无 `unsafe-*`、日志无敏感值、
初始化码只出现在日志、Agent Token 不进 URL、静态资源无路径遍历、请求体与连接数都有上限。

### 3.2 权限变化：Agent 单元从"零能力"放宽到 `CAP_NET_RAW`（延迟探测 Phase）

**改了什么**：`deploy/install-agent.sh` 写出的 systemd 单元里，

```ini
CapabilityBoundingSet=CAP_NET_RAW
AmbientCapabilities=CAP_NET_RAW
```

（原来是空值，即零能力。）

**为什么必须改**：延迟探测支持两种方式，其中 ICMP（`type=icmp` 的探测目标）要用原始套接字
自己组 echo 请求并解析回包（`net.Dial("ip4:icmp", …)` / `"ip6:ipv6-icmp"`），
内核要求 `CAP_NET_RAW`。零能力时 `socket(AF_INET, SOCK_RAW, IPPROTO_ICMP)` 直接返回 `EPERM`：

- Agent 不会掉线，只会记**一条**警告（同类问题只记一次，避免刷日志），并把该目标留空；
- 但用户看到的现象是"这条曲线永远没有数据"，日志里的那一行也很容易被忽略 ——
  属于"配置看起来没问题、功能就是不工作"的那类最难排查的故障。

**边界（只有这一条被放宽）**：

- 仍然以非 root 的 `probe-agent` 用户运行（`User=${USER_NAME}`，不是 root）；
- `NoNewPrivileges=yes`、`ProtectSystem=strict`、`SystemCallFilter=@system-service`、
  `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`、`UMask=0077` 等全部保持不变；
- 只给 `CAP_NET_RAW`，**不给** `CAP_NET_ADMIN`（改路由/防火墙/接口）、不给任何 `CAP_SYS_*`；
  能力在进程启动时由 systemd 授予，进程内无法再获得新的能力；
- 服务端单元（`install-server.sh`）保持零能力 —— 它不需要任何原始套接字，
  `TestUnitsGrantOnlyCapNetRawToAgent` 会把这条也钉死。

**风险与取舍**：`CAP_NET_RAW` 允许构造任意原始报文（例如伪造源地址的包）。
接受它的理由：Agent 本来就部署在**用户自己的**被监控机器上，且服务端代码是同一份、
不执行任何外部命令；相对"ICMP 探测完全不可用"，这个代价更小。
不需要 ICMP 的部署可以把单元里那两行改回空值，只使用 `type=tcp` 的目标 ——
TCP 探测只做普通的 `connect()`（高延迟时会重试几次握手，见 `internal/agent/ping.go`），
不需要任何特权。

> 说明：本文引用的用例名都可以用 `go test ./... -run <名字>` 单独复现。

---

## 4. 依赖复核（Phase 10）

```
$ go run golang.org/x/vuln/cmd/govulncheck@latest ./...
No vulnerabilities found.
Your code is affected by 0 vulnerabilities.
This scan also found 0 vulnerabilities in packages you import and 1
vulnerability in modules you require, but your code doesn't appear to call these
vulnerabilities.
```

唯一命中的模块级条目与用法无关：

- `GO-2026-5932`（`golang.org/x/crypto/openpgp` 已停止维护、设计上不安全）——本项目只用 `golang.org/x/crypto/argon2`，不涉及 openpgp（govulncheck 也确认"未被调用"）。

| 直接依赖 | 版本 | 用途 | 复核结论 |
|---|---|---|---|
| `modernc.org/sqlite` | v1.60.0 | 纯 Go SQLite 驱动（无 CGO） | 内嵌 SQLite **3.53.4**，高于 CVE-2025-6965 的修复版本 3.50.2；且本项目的 SQL 全部参数化，不接受用户 SQL |
| `github.com/coder/websocket` | v1.8.15 | 服务端/Agent 的 WebSocket | 历史"恶意 pong 导致内存膨胀"问题在 v1.8.11 前已修；本版本更高，且我们**主动设置了 16 KB 读上限与读超时**（不依赖库默认值） |
| `golang.org/x/crypto` | v0.57.0 | 仅 `argon2` | 未使用 ssh/openpgp 等受影响包 |

参考：[CVE-2025-6965（SQLite 整数截断）](https://www.incibe.es/incibe-cert/alerta-temprana/vulnerabilidades/CVE-2025-6965)、
[coder/websocket 依赖与安全公告页](https://socket.dev/go/package/github.com/coder/websocket)、
[govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck)。

复现方式（需要网络）：

```bash
govulncheck ./...          # 或 go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

---

## 5. 已知接受的风险

| 风险 | 说明 | 为什么接受 |
|---|---|---|
| 没有 2FA / 没有 IP 白名单 | 只有用户名 + 密码 + 登录限流 | 保持极简；建议配合反代的 Basic Auth 或仅内网/VPN 访问 |
| 初始化码出现在日志里 | 唯一的分发途径（12 位十六进制、30 分钟、一次性） | 比"默认密码"安全得多；能读日志的人本来就能读数据库 |
| 数据库明文 | SQLite 文件不加密 | 本地 root 能读进程内存，加密的收益有限；靠文件权限与主机安全 |
| 告警只支持 Telegram | v1 只有这一个渠道 | 满足需求且只引入一个外部目标；扩展点是一个 6 行的接口 |
| Agent 侧无 mTLS | 认证靠 Token | 公网必须走 TLS；Token 泄漏可单独轮换 |
| 单管理员 | 没有多用户与权限分级 | 自用场景；多用户会带来权限模型与审计的复杂度 |
| 访客模式能看到价格与用量 | 「允许访客查看」打开时，节点名称、用量、曲线与**价格**对任何能访问到地址的人都可见（IP、备注、审计与设置仍然不可见） | 这是用户明确要的口径（"价格我能看到"）；而且开关**默认关**，打开是一次有意识的动作，设置页里也写清了会公开什么 |

---

## 6. 运维建议

1. **不要直接暴露端口**：默认只监听 `127.0.0.1:25774`，公网访问请用 Caddy/nginx 反代并启用 TLS；反代要关闭响应缓冲（SSE 需要），并把真实 IP 传给 `--trusted-proxy`。
2. **数据目录权限**：`chmod 700` 数据目录，SQLite 文件与 `-wal` 只允许服务账号读写。
3. **Agent Token 文件**：`/etc/probe-agent/token` 用 `0600`（Agent 会在权限过宽时打印警告）。
4. **定期备份**：停服后复制 `probe.db`（或 `VACUUM INTO`），验证能恢复；`alert_state`/`traffic_daily` 都在同一个库里。
5. **升级前看变更**：本项目遵循"每个阶段可验证"，升级请跑一遍 `go test ./...` 与 `docs/DESIGN.md` 的验收清单。
6. **开了「允许访客查看」之后**（设置 →「访客访问」）：面板的首页与详情页对任何能访问到该地址的人可见（看不到 IP、备注、审计与设置，也不能做任何修改）。此时更要确认反代上的 TLS 与访问控制 —— 面板公开不等于应该裸奔在公网上。
