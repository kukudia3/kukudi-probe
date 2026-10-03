package server

// 第四轮「缺口补齐」用例：第二轮安全审计 SECURITY-AUDIT-ROUND2.md §7 里被判
// 「本机可补、但当时没做」的三条（逐条编号见 _audit/ROUND3-VERIFY-1.md §3.2/§3.3/§3.5）：
//
//	02-3 访客全局 20 条 SSE 名额打满后的表现（当时卡在「需 ≥10 个不同来源 IP」）
//	03-4 `hello.state` 是未校验字段 —— 用例要钉住「它不产生任何副作用」
//	05-4 「两个并发 PUT 丢更新」那一半（`-race` 那半本机做不到）
//
// 本文件只加测试：产品代码一行未改（internal/server 下的非 _test.go 文件逐字节未动）。
// 结论与反向验证记录在 D:\DEEPSEEK\_audit\ROUND4-GAPS.md。

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
)

// ---------------------------------------------------------------------------
// 02-3：伪造的 X-Forwarded-For 打满访客 20 条 SSE 名额
// ---------------------------------------------------------------------------

// openStreamWithForwardedFor 用指定的 X-Forwarded-For 建一条**不带会话 Cookie** 的
// SSE 连接。返回状态码、非 200 时的响应体、以及关闭函数。
//
// 为什么不复用 startSSE：它走 h.client（带会话 Cookie），而这条用例要的正是
// "匿名 + 不同来源 IP"这一组条件；而且它把非 200 当致命错误，这里是**要断言 503**。
func openStreamWithForwardedFor(t *testing.T, base, xff string) (int, string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("构造 SSE 请求: %v", err)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	// http.DefaultClient 没有 Cookie jar、也没有 Timeout（SSE 是长连接）。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("连接 SSE（XFF=%q）: %v", xff, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		cancel()
		return resp.StatusCode, string(raw), func() {}
	}
	return http.StatusOK, "", func() {
		cancel()
		_ = resp.Body.Close()
	}
}

// TestGuestStreamGlobalQuotaFilledByForwardedForIPs 覆盖 02-3。
//
// 审计写的是「访客全局 20 条 SSE 名额被打满后的表现：需 ≥10 个不同来源 IP（或一个 /64
// IPv6 段）；"管理员不受影响"这半只做了代码核对」。既有用例
// （TestGuestStreamFloodDoesNotLockOutAdmin）自己写了"httptest 里来源 IP 都是 127.0.0.1，
// 造不出 20 个不同 IP，所以剩下的名额直接打 hub"——**本用例补的就是那一步**：
// 配 `--trusted-proxy 127.0.0.1/32`（推荐部署下反代就是这个角色），每个请求带一个不同的
// `X-Forwarded-For`，于是**真实 HTTP 栈**上就有了 20 个不同的来源 IP。
//
// 断言四件事：
//  1. 同一个 XFF 的第 3 条访客流被拒（per-IP 上限 2）——先证明"XFF 真的决定了限流键"，
//     否则下面 20 个不同 IP 的结论没有意义；
//  2. 20 个不同 XFF 的 20 条访客流全部 200，且 hub 里访客数恰好是 20；
//  3. 第 21 个**新** XFF 的访客流 503 + `too_many_streams`（全局上限生效）；
//  4. 此刻**管理员自己的会话**仍然能建流并收到数据（两份配额互不挤占）。
//
// 反向验证（已实测，见报告）：把 hub.add 里 `h.guests >= maxGuestSSEClients` 这个判据
// 删掉 → 第 3 段红在"第 21 个不同来源 IP 应当 503"。
func TestGuestStreamGlobalQuotaFilledByForwardedForIPs(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxy = "127.0.0.1/32" // 直连对端（127.0.0.1）是可信反代 ⇒ 采信最右一条 XFF
	h := newAuthHarnessWithConfig(t, cfg)
	guestOn(t, h)

	// ---- 1. XFF 真的决定限流键：同一个 IP 的第 3 条被拒 ----
	var perIPStops []func()
	defer func() {
		for _, stop := range perIPStops {
			stop()
		}
	}()
	for i := 0; i < maxGuestSSEClientsPerIP; i++ {
		code, body, stop := openStreamWithForwardedFor(t, h.ts.URL, "203.0.113.9")
		if code != http.StatusOK {
			t.Fatalf("同一个来源 IP 的前 %d 条访客流应当 200，第 %d 条得到 %d %s",
				maxGuestSSEClientsPerIP, i+1, code, body)
		}
		perIPStops = append(perIPStops, stop)
	}
	if code, body, stop := openStreamWithForwardedFor(t, h.ts.URL, "203.0.113.9"); code != http.StatusServiceUnavailable {
		stop() // 万一被放行（变异版本），也要把这条流关掉，别让 httptest.Close 卡住
		t.Fatalf("同一个来源 IP 的第 %d 条访客流应当 503（per-IP 上限 %d），实际 %d %s",
			maxGuestSSEClientsPerIP+1, maxGuestSSEClientsPerIP, code, body)
	}
	for _, stop := range perIPStops {
		stop() // 释放名额，别把全局配额也占了（下面要精确地占 20 条）
	}

	// ---- 2. 20 个不同 XFF：全部 200 ----
	stops := make([]func(), 0, maxGuestSSEClients)
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	for i := 0; i < maxGuestSSEClients; i++ {
		xff := fmt.Sprintf("198.51.100.%d", i+1)
		code, body, stop := openStreamWithForwardedFor(t, h.ts.URL, xff)
		if code != http.StatusOK {
			t.Fatalf("第 %d 个不同来源 IP（%s）的访客流应当 200（全局上限 %d），实际 %d %s",
				i+1, xff, maxGuestSSEClients, code, body)
		}
		stops = append(stops, stop)
	}
	if _, guests := h.srv.hub.countByRole(); guests != maxGuestSSEClients {
		t.Fatalf("hub 里访客连接数 = %d，期望 %d", guests, maxGuestSSEClients)
	}

	// ---- 3. 第 21 个**新** XFF 被全局配额拒掉 ----
	code, body, stopExtra := openStreamWithForwardedFor(t, h.ts.URL, "203.0.113.201")
	defer stopExtra() // 同上：变异版本下这条会被放行，必须关掉
	if code != http.StatusServiceUnavailable {
		t.Fatalf("访客全局配额（%d）占满后，新来源 IP 应当 503，实际 %d %s",
			maxGuestSSEClients, code, body)
	}
	if !strings.Contains(body, "too_many_streams") {
		t.Errorf("503 的错误码应当是 too_many_streams，实际响应体 %s", body)
	}

	// ---- 4. 管理员不受影响：自己的流照样建得起来，而且真的在推数据 ----
	r, stopAdmin := startSSE(t, h)
	defer stopAdmin()
	payload := r.next(t, 5*time.Second)
	if _, ok := payload["nodes"]; !ok {
		t.Errorf("管理员的首帧应当带 nodes 快照，实际 %v", payload)
	}
	if _, guests := h.srv.hub.countByRole(); guests != maxGuestSSEClients {
		t.Errorf("管理员建流之后访客连接数 = %d，期望不变（%d）", guests, maxGuestSSEClients)
	}
}

// ---------------------------------------------------------------------------
// 03-4：hello.state 是未校验字段 —— 钉住"它不产生任何副作用"
// ---------------------------------------------------------------------------

// TestHelloStateExtremesAreUnvalidatedButInert 覆盖 03-4。
//
// 审计的原文：「`hello.state`（`protocol.AgentStat`）被 `json.Unmarshal` 解析，
// 但 `ValidateHello` 不校验、服务端无任何地方使用（Agent 侧会发）⇒ 当前无影响，
// 属"未校验字段留在协议里"的隐患（将来谁要用必须先补校验：`CkptAgeS` 是 int64、
// `TotalRx/Tx` 是 uint64，都没有范围检查）」。
//
// 审计给的做法是「写一条"未校验字段不产生副作用"的回归用例」。本用例把这条钉死成三段：
//  1. **未校验是事实**：极端值（MinInt64 / MaxUint64）的 hello 仍然通过 ValidateHello
//     —— 这一条**故意钉住现状**；将来谁给 State 补了校验，这条断言会红，那时应当
//     把它改成"拒绝"，而不是删掉它（红是提醒，不是障碍）；
//  2. **不影响握手**：带这种 hello 的真实 WebSocket 握手照常完成（welcome + config）；
//  3. **不落库、不改变基线**：握完手之后 `node_runtime` 的流量基线仍然是 0
//     —— 也就是说 state.total_rx 没有被当成真实上报写进去。
//
// 反向验证（已实测，见报告）：在 agentconn.go 的握手成功后加一行
// `store` 基线写入（把 hello.State.TotalRx 写进 node_runtime.rx_total）→ 第 3 段红在
// "hello.state 不该改变流量基线"。
func TestHelloStateExtremesAreUnvalidatedButInert(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	ctx := context.Background()

	hello := testHello()
	hello.State = &protocol.AgentStat{
		CkptAgeS: math.MinInt64,
		TotalRx:  math.MaxUint64,
		TotalTx:  math.MaxUint64,
	}

	// 1. 未校验：极端值照样过（这是现状，见函数注释）。
	if err := protocol.ValidateHello(hello); err != nil {
		t.Fatalf("现状是 hello.state 不被校验；这里却报了错 %v —— "+
			"如果这是刚补上的校验，请把本用例改成断言拒绝，并同步 §7 的 03-4", err)
	}

	// 2. 真实握手照常完成。
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, mustEnvelope(t, protocol.TypeHello, hello))
	readHandshake(t, conn)

	// 握手之后这个节点必须在册（证明 hello 真的被处理了，而不是连接被丢掉）。
	if _, ok := s.State().Get(node.ID); !ok {
		t.Fatal("握完手之后节点不在内存状态里：hello 没被处理，本用例失去判别力")
	}

	// 3. 不落库：流量基线仍然是最初的 0。
	var rxTotal, txTotal uint64
	if err := s.db.Reader().QueryRowContext(ctx,
		`SELECT rx_total, tx_total FROM node_runtime WHERE node_id = ?`, node.ID).
		Scan(&rxTotal, &txTotal); err != nil {
		t.Fatalf("读 node_runtime: %v", err)
	}
	if rxTotal != 0 || txTotal != 0 {
		t.Errorf("hello.state（total_rx=%d / total_tx=%d）不该改变流量基线：读回 rx=%d tx=%d",
			uint64(hello.State.TotalRx), uint64(hello.State.TotalTx), rxTotal, txTotal)
	}

	// 而且这条连接仍然是**活的**：紧接着发一拍正常 metrics 必须被接受。
	sendFrame(t, conn, mustEnvelope(t, protocol.TypeMetrics, testMetrics()))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := s.State().Get(node.ID); ok && st.Seq >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hello.state 用了极端值之后，正常的 metrics 拍没有落地：连接被这条 hello 影响了")
}

// ---------------------------------------------------------------------------
// 05-4：两个并发 PUT —— 最终状态必须是一整代，不许是两代的混搭
// ---------------------------------------------------------------------------

// TestConcurrentAlertSettingsSavesNeverMixGenerations 覆盖 05-4 的"丢更新那一半"。
//
// 审计的原文：「'两个并发 PUT 丢更新'亦未实测（需要并发 HTTP + 时序）」。
// 这一条在**修复前**是真实的数据竞争（`05-X-1`：运行期写 `s.cfg.Alert*` 无锁），
// 修复后由 `Server.cfgMu` 把"解析 + 赋值"整组圈住（api_admin.go 的注释）。
//
// 既有用例（audit_round2_test.go 的 TestAlertSettingsConcurrentSaveAndReadStayConsistent）
// 测的是"并发读**过程中**不许读到混搭"，用的是 tight loop 直接调取值函数。
// 本用例补的是另一半：**每一轮并发保存结束后，落下来的最终状态必须完整来自某一次保存**。
//
// 为什么"最终状态"也能抓到丢更新：不带锁时四个字段是四次独立赋值，两个写者的
// 赋值完全可以交错（A 写 cooldown → B 写四个 → A 写剩下三个），于是最终状态是两代的
// 混搭 —— 这正是用户看到的"保存了但没生效"。
//
// 反向验证（已实测，见报告）：把 api_admin.go 里 `s.cfgMu.Lock()/Unlock()` 那一对
// 去掉（其余不动）→ 30 轮里出现混搭，红在"第 N 轮保存之后四个参数来自不同代次"。
func TestConcurrentAlertSettingsSavesNeverMixGenerations(t *testing.T) {
	h := newAuthHarness(t)

	units := map[string]time.Duration{
		"cooldown":       time.Second,
		"startup_grace":  time.Millisecond,
		"debounce":       time.Microsecond,
		"recover_stable": 10 * time.Millisecond,
	}
	// 每代的值都不同且可逆推出代次；四个字段必须是同一代。
	payload := func(gen int) map[string]any {
		return map[string]any{
			"cooldown":       (time.Duration(gen) * time.Second).String(),
			"startup_grace":  (time.Duration(gen) * time.Millisecond).String(),
			"debounce":       (time.Duration(gen) * time.Microsecond).String(),
			"recover_stable": (time.Duration(gen) * 10 * time.Millisecond).String(),
		}
	}
	genOf := func(v string, unit time.Duration) (int, bool) {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, false
		}
		g := int(d / unit)
		return g, g > 0
	}

	// 轮数 × 写者数是**判别力**的来源：不带锁时四个字段是四次独立赋值，只有两个
	// 写者的赋值真的交错才会留下混搭。这里用 100 轮 × 6 个并发写者把这个窗口撞满
	// （实测：去掉 cfgMu 之后，本用例在多轮里稳定抓到混搭）。
	const rounds = 100
	const writers = 6
	for round := 0; round < rounds; round++ {
		base := (round + 1) * 100 // 保证各轮代次不重叠，混搭一定跨写者
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(gen int) {
				defer wg.Done()
				if err := h.putNoFatal("/api/v1/settings/alert", payload(gen)); err != nil {
					t.Errorf("第 %d 轮并发保存失败: %v", round, err)
				}
			}(base + w)
		}
		wg.Wait()

		status, body := h.get(t, "/api/v1/settings")
		if status != http.StatusOK {
			t.Fatalf("第 %d 轮读取设置: HTTP %d %v", round, status, body)
		}
		alert, _ := body["alert"].(map[string]any)
		if alert == nil {
			t.Fatalf("第 %d 轮响应里没有 alert：%v", round, body)
		}
		gen := -1
		mixed := false
		for field, unit := range units {
			raw, _ := alert[field].(string)
			g, ok := genOf(raw, unit)
			if !ok {
				t.Fatalf("第 %d 轮 %s = %q，推不出代次", round, field, raw)
			}
			if gen == -1 {
				gen = g
			} else if g != gen {
				mixed = true
			}
		}
		if mixed {
			t.Errorf("第 %d 轮并发保存之后，四个告警参数来自不同代次（丢更新/部分更新）：%v", round, alert)
		}
		if gen < base || gen > base+writers-1 {
			t.Errorf("第 %d 轮最终代次 = %d，不属于本轮任何一个写者（%d…%d）",
				round, gen, base, base+writers-1)
		}
	}
}

// mustEnvelope 用协议自己的编码器造一帧（字段名漂移时用例跟着漂移，不会各写一份 JSON）。
func mustEnvelope(t *testing.T, typ string, payload any) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(typ, payload)
	if err != nil {
		t.Fatalf("编码 %s 帧: %v", typ, err)
	}
	return env
}
