package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/protocol"
	"probe/internal/store"
)

// 第二轮安全审计（SECURITY-AUDIT-ROUND2.md）里属于 **Agent / 协议** 的
// confirmed 条目在此逐条钉住。每条都写了"撤掉修复会红在哪一处"。
//
// 本文件覆盖：
//   - 03-A-1（medium）：已认证 Agent 可用任意 target_id 无上限膨胀 ping_samples_1m；
//   - 03-A-8（low）：config 帧是一条指令面 —— 操作记录里必须留下"让全网去连了哪些地址"。
//
// 03-A-2 / A-3 / A-4 的用例在 security_round2_test.go（同一批的另外三条，已修）。
// 03-A-5（Agent 侧握手重定向）在 internal/agent/audit_round2_test.go。
// 03-A-6 是 refuted、03-A-7 是 unverified，都不在本批范围内。

// ---------------------------------------------------------------------------
// 03-A-1：target_id 必须落在"服务端真的下发过的那份配置"里
// ---------------------------------------------------------------------------

// pingRowsByTarget 返回某个节点在 ping_samples_1m 里的行（target_id → 行数）。
func pingRowsByTarget(t *testing.T, s *Server, nodeID int64) map[int64]int {
	t.Helper()
	ctx := context.Background()
	rows, err := s.db.Reader().QueryContext(ctx,
		`SELECT target_id, count(*) FROM ping_samples_1m WHERE node_id = ? GROUP BY target_id`, nodeID)
	if err != nil {
		t.Fatalf("统计探测行: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatalf("读取统计: %v", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历统计: %v", err)
	}
	return out
}

// pingResult 造一条数值合法的探测结果（能过 protocol.ValidateMetrics）。
func pingResult(id int64) protocol.PingResult {
	return protocol.PingResult{TargetID: id, AvgMS: 12.5, MinMS: 10, MaxMS: 20, LossPct: 0}
}

// sendPingsFrame 发一帧带探测结果的 metrics。seq 用来等"这一帧真的被处理了"，
// 而不是靠 sleep 猜（服务端处理是异步的，靠 Sleep 的断言在忙机器上会偶发变红）。
func sendPingsFrame(t *testing.T, conn *websocket.Conn, seq uint64, pings []protocol.PingResult) {
	t.Helper()
	m := testMetrics()
	m.Pings = pings
	env, err := protocol.New(protocol.TypeMetrics, m)
	if err != nil {
		t.Fatalf("构造 metrics: %v", err)
	}
	env.Seq = seq
	sendFrame(t, conn, env)
}

// waitAgentSeq 等到服务端把第 seq 帧处理完（state.Seq 由处理路径写入）。
func waitAgentSeq(t *testing.T, s *Server, nodeID int64, seq uint64) {
	t.Helper()
	waitFor(t, 5*time.Second, fmt.Sprintf("metrics seq=%d 被处理", seq), func() bool {
		st, ok := s.State().Get(nodeID)
		return ok && st.Seq >= seq
	})
}

// TestAgentPingTargetsOutsideConfigAreBounded 钉住 03-A-1 的放大路径被封顶。
//
// 修复前：target_id 只校验"正整数、帧内不重复"，于是每帧 16 个新 ID、每秒 5 帧、
// 同一 Token 还能开 20 条连接（agentconn.go 的 per-IP 上限）—— 约 9.6 万行/分钟
// 灌进 ping_samples_1m，而这些行**永远不会被读到**（界面只遍历配置里的目标），
// 默认还留 8 天。修复后：配置内的 ID 一律接纳，配置外的 ID 每节点每个落盘周期
// 只接纳 pingExtraTargets 个。
//
// 反向验证（实测）：撤掉 observe 里的过滤（或让 admit 恒 true）后，本用例红在
// 两条 —— ①「配置外目标落了 31 行，超过每节点每周期上限 16」（发出去 56 个 ID；
// 31 而不是 56 是因为存储层的 32 目标上限又截了一刀，见 04-X-1）；
// ②「配置内目标 2 的行不该被丢」—— 存储层那 32 格被伪造 ID 占满之后，
// **连真实目标的行都会一起被丢掉**。这正说明上游这道闸不是重复劳动：
// 光靠存储层兜底，"封顶"会变成"随机丢真实数据"。
func TestAgentPingTargetsOutsideConfigAreBounded(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	ctx := context.Background()

	// 两个真实目标：它们的 ID 是"配置内"，一条都不该丢。
	saved, err := s.db.SetPingSettings(ctx, []store.PingTarget{
		{Label: "cf", Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
		{Label: "gw", Type: protocol.PingTypeICMP, Host: "10.0.0.1", Enabled: true},
	}, 60)
	if err != nil {
		t.Fatalf("配置探测目标: %v", err)
	}
	if len(saved.Targets) != 2 {
		t.Fatalf("目标数 = %d，期望 2", len(saved.Targets))
	}

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn) // welcome + config：白名单就是这一帧里的目标

	// 攻击形态：每帧 16 个**新** target_id（帧内不重复 → 能过 ValidateMetrics）。
	// 每帧同时带上两个配置内目标（真实 Agent 也是这么报的）。
	const frames = 4
	const bogusPerFrame = protocol.MaxPingTargets - 2
	bogus := make([]int64, 0, frames*bogusPerFrame)
	next := int64(1000)
	for f := 0; f < frames; f++ {
		pings := []protocol.PingResult{pingResult(saved.Targets[0].ID), pingResult(saved.Targets[1].ID)}
		for i := 0; i < bogusPerFrame; i++ {
			pings = append(pings, pingResult(next))
			bogus = append(bogus, next)
			next++
		}
		if len(pings) > protocol.MaxPingTargets {
			t.Fatalf("帧内条目 %d 超过上限", len(pings))
		}
		sendPingsFrame(t, conn, uint64(f+1), pings)
		// 上报限流是 5 帧/秒：四帧之间留够间隔，免得被限流丢掉（那会让断言变成空断言）。
		time.Sleep(150 * time.Millisecond)
	}
	waitAgentSeq(t, s, node.ID, frames)

	s.flushPings(ctx)
	rows := pingRowsByTarget(t, s, node.ID)

	for _, id := range []int64{saved.Targets[0].ID, saved.Targets[1].ID} {
		if rows[id] == 0 {
			t.Errorf("配置内目标 %d 的行不该被丢（实际 %v）", id, rows)
		}
	}
	unknown := 0
	for id, n := range rows {
		if id == saved.Targets[0].ID || id == saved.Targets[1].ID {
			continue
		}
		unknown += n
	}
	if unknown > pingExtraTargets {
		t.Errorf("配置外目标落了 %d 行，超过每节点每周期上限 %d（发出去的是 %d 个 ID）",
			unknown, pingExtraTargets, len(bogus))
	}
	if unknown == 0 {
		t.Error("配置外目标一行都没有：这条断言要证明的是「封顶」而不是「全丢」")
	}
}

// TestAgentPingTargetsFromPreviousConfigAreStillAccepted 钉住**不误伤**：
// 服务端已经换了配置、Agent 还在按上一份配置上报时，那批旧目标必须照旧落库。
//
// 为什么这条与上一条同等重要：白名单如果按"当前配置"严格过滤，就会在
// "管理员刚删掉/换掉目标 → 推送还没到达（或推送写失败，要等 Agent 重连）"
// 这个必然存在的窗口里丢掉真实数据（图表上凭空少一段）。所以配额取的是一整套
// 目标的规模（16）：过期的一整套照单全收，多出来的第 17 个才丢。
//
// 反向验证：把 pingExtraTargets 改成 0（等价于"只认当前配置"），本用例红在
// "旧配置的目标必须照旧落库"那一段（16 个旧 ID 一行都没有）。
func TestAgentPingTargetsFromPreviousConfigAreStillAccepted(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	ctx := context.Background()

	oldTargets := make([]store.PingTarget, 0, protocol.MaxPingTargets)
	for i := 0; i < protocol.MaxPingTargets; i++ {
		oldTargets = append(oldTargets, store.PingTarget{
			Label: fmt.Sprintf("old-%d", i), Type: protocol.PingTypeICMP,
			Host: fmt.Sprintf("10.1.0.%d", i+1), Enabled: true,
		})
	}
	first, err := s.db.SetPingSettings(ctx, oldTargets, 60)
	if err != nil {
		t.Fatalf("配置第一套目标: %v", err)
	}

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	// 管理员换掉**整套**目标，服务端把新配置推给这条连接（写成功 → 白名单更新）。
	newTargets := make([]store.PingTarget, 0, protocol.MaxPingTargets)
	for i := 0; i < protocol.MaxPingTargets; i++ {
		newTargets = append(newTargets, store.PingTarget{
			Label: fmt.Sprintf("new-%d", i), Type: protocol.PingTypeICMP,
			Host: fmt.Sprintf("10.2.0.%d", i+1), Enabled: true,
		})
	}
	second, err := s.db.SetPingSettings(ctx, newTargets, 60)
	if err != nil {
		t.Fatalf("配置第二套目标: %v", err)
	}
	s.agents.PushConfig()
	// 等的是"白名单换成了新的那一套"，不是"规模一样"：两套都是 16 个，
	// 只比数量的话这个等待会立刻返回，后面的断言就变成空断言。
	waitFor(t, 5*time.Second, "新配置写出去（白名单更新）", func() bool {
		got := allowedIDs(s)
		if len(got) != len(second.Targets) {
			return false
		}
		for _, tg := range second.Targets {
			if !got[tg.ID] {
				return false
			}
		}
		return true
	})

	// Agent 还没读到新配置（或推送失败）：它报的仍是**旧**的一套，必须一条不丢。
	pings := make([]protocol.PingResult, 0, protocol.MaxPingTargets)
	for _, tg := range first.Targets {
		pings = append(pings, pingResult(tg.ID))
	}
	sendPingsFrame(t, conn, 1, pings)

	// 同一个窗口里的第 17 个"配置外"目标仍然要被丢掉（配额是**一整套**，不是无限）。
	// 注意顺序：这一帧必须在 flush 之前发 —— flush 会开新周期、重置配额。
	extra := make([]protocol.PingResult, 0, 4)
	for i := int64(0); i < 4; i++ {
		extra = append(extra, pingResult(9000+i))
	}
	sendPingsFrame(t, conn, 2, extra)
	waitAgentSeq(t, s, node.ID, 2)
	s.flushPings(ctx)

	rows := pingRowsByTarget(t, s, node.ID)
	for _, tg := range first.Targets {
		if rows[tg.ID] == 0 {
			t.Errorf("旧配置里的目标 %d 在错位窗口里被丢了（实际 %v）", tg.ID, rows)
		}
	}
	for i := int64(0); i < 4; i++ {
		if rows[9000+i] != 0 {
			t.Errorf("配额用完后的伪造 ID %d 不该落库（实际 %v）", 9000+i, rows)
		}
	}
}

// allowedIDs 读当前白名单（同包内直接读；只用于等待"推送写成功"）。
func allowedIDs(s *Server) map[int64]bool {
	s.ping.mu.Lock()
	defer s.ping.mu.Unlock()
	out := make(map[int64]bool, len(s.ping.allowed))
	for id := range s.ping.allowed {
		out[id] = true
	}
	return out
}

// TestPingTrackerQuotaIsPerNodeAndResetsEachFlush 钉住配额的两条性质：
// 按节点分别计数、按落盘周期重置。
//
// 反向验证：把 take 里的 `p.extra = make(...)` 删掉（配额永不重置），
// 本用例红在"下一个周期应当又能接纳一整套配置外目标"那一段。
func TestPingTrackerQuotaIsPerNodeAndResetsEachFlush(t *testing.T) {
	p := newPingTracker()
	now := time.Now()
	p.setAllowed([]int64{7})

	// 节点 1 用满配额（配置外的目标正好 16 个），第 17 个被丢。
	for i := 0; i < pingExtraTargets; i++ {
		if dropped := p.observe(1, []protocol.PingResult{pingResult(int64(100 + i))}, now); dropped != 0 {
			t.Fatalf("第 %d 个配置外目标不该被丢（dropped=%d）", i+1, dropped)
		}
	}
	if dropped := p.observe(1, []protocol.PingResult{pingResult(999)}, now); dropped != 1 {
		t.Fatalf("配额用完后应当丢弃，dropped=%d", dropped)
	}
	// 另一个节点有自己的配额。
	if dropped := p.observe(2, []protocol.PingResult{pingResult(999)}, now); dropped != 0 {
		t.Fatalf("配额应当按节点分别计（节点 2 不该被节点 1 用掉），dropped=%d", dropped)
	}
	// 配置内的目标不受配额影响。
	if dropped := p.observe(1, []protocol.PingResult{pingResult(7)}, now); dropped != 0 {
		t.Fatalf("配置内的目标永远不该被丢，dropped=%d", dropped)
	}

	if got := len(p.take()); got != pingExtraTargets+2 {
		t.Fatalf("本次落盘应当有 %d 个桶（16 配置外 + 配置内 1 个 + 节点 2 的 1 个），实际 %d",
			pingExtraTargets+2, got)
	}
	// 新周期：配额重置，又能接纳一整套配置外目标。
	if dropped := p.observe(1, []protocol.PingResult{pingResult(2000)}, now); dropped != 0 {
		t.Fatalf("落盘后配额应当重置，dropped=%d", dropped)
	}
}

// TestPingTrackerConfiguredTargetsSurviveSpam 钉住"伪造者挤不掉真实目标"。
//
// 如果只按"每节点每分钟最多 N 个目标、先到先得"封顶，攻击者只要先刷满 16 个随机
// ID，真实目标（同一周期的合法数据）就会被挤掉 —— 那是拿可用性换来的修复。
// 白名单优先保证了这一点：配置内的 ID 走的是另一条通道，与配额无关。
//
// 反向验证：把 admit 的第一行（allowed 命中即接纳）删掉，本用例红在
// "配置内的目标不该被伪造 ID 挤掉"那一段。
func TestPingTrackerConfiguredTargetsSurviveSpam(t *testing.T) {
	p := newPingTracker()
	now := time.Now()
	p.setAllowed([]int64{1, 2})

	// 先让伪造 ID 把配额吃光。
	for i := 0; i < pingExtraTargets; i++ {
		p.observe(1, []protocol.PingResult{pingResult(int64(500 + i))}, now)
	}
	if dropped := p.observe(1, []protocol.PingResult{pingResult(501)}, now); dropped != 0 {
		t.Fatal("同一个配置外 ID 第二次上报不该再吃配额")
	}
	if dropped := p.observe(1, []protocol.PingResult{pingResult(1), pingResult(2)}, now); dropped != 0 {
		t.Fatal("配置内的目标不该被伪造 ID 挤掉")
	}

	seen := map[int64]bool{}
	for _, b := range p.take() {
		seen[b.TargetID] = true
	}
	for _, id := range []int64{1, 2} {
		if !seen[id] {
			t.Errorf("配置内目标 %d 没进本次落盘（实际 %v）", id, seen)
		}
	}
}

// TestAgentPingTargetsQuotaIsSharedAcrossConnections 钉住"多开连接不是多份额度"。
//
// 报告里"约 9.6 万行/分钟"的来源正是"同一枚 Token 可以并发 20 条连接"
// （每连接 4800 行/分钟，见 agentconn.go 的每 IP 上限）。配额按**节点**计，
// 所以多开连接只会一起抢同一份 16 个目标，不会线性放大。
//
// 反向验证：把 admit 里的 p.extra[nodeID] 换成按连接计（现在没有 connID 参数，
// 等价于"每条连接各自一份配额"），本用例红在"两条连接合计只该落 16 个目标"那一条。
func TestAgentPingTargetsQuotaIsSharedAcrossConnections(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	ctx := context.Background()

	// 一个目标都不配：白名单为空，全部走"配置外配额"这条通道。
	conns := make([]*websocket.Conn, 0, 2)
	for i := 0; i < 2; i++ {
		conn := mustDialAgent(t, ts, token)
		sendFrame(t, conn, helloFrame(t, testHello()))
		readHandshake(t, conn)
		conns = append(conns, conn)
	}

	next := int64(5000)
	for i, conn := range conns {
		pings := make([]protocol.PingResult, 0, protocol.MaxPingTargets)
		for j := 0; j < protocol.MaxPingTargets; j++ {
			pings = append(pings, pingResult(next))
			next++
		}
		sendPingsFrame(t, conn, uint64(i+1), pings)
		time.Sleep(200 * time.Millisecond)
	}
	waitAgentSeq(t, s, node.ID, 2)

	s.flushPings(ctx)
	rows := pingRowsByTarget(t, s, node.ID)
	// 上界写 protocol.MaxPingTargets（一帧的条目上限）而不是 pingExtraTargets：
	// 断言的性质是"多开连接不等于多份额度"，与实现里那个常数无关 ——
	// 常数一旦被改成"每条连接一份"（2 × 16），这里就会红。
	if len(rows) > protocol.MaxPingTargets {
		t.Errorf("两条连接合计落了 %d 个目标，超过「一帧的量」%d（各报了 %d 个伪造 ID）",
			len(rows), protocol.MaxPingTargets, protocol.MaxPingTargets)
	}
	if len(rows) == 0 {
		t.Error("一个目标都没落：这条断言要证明的是「共享配额」而不是「全丢」")
	}
}

// ---------------------------------------------------------------------------
// 03-A-8：config 帧是一条指令面 —— 操作记录里要留下地址
// ---------------------------------------------------------------------------

// TestPingSettingsAuditRecordsTargetAddresses 钉住"审计里看得见改成了哪些地址"。
//
// 探测目标列表就是**全体 Agent 的出站目的地**：能写它的人可以让每台被监控机从
// 各自内网去连任意 host:port。只记"改了 3 个目标"，事后翻操作记录完全看不出改了
// 哪些地址，取证时等于没记。
//
// 反向验证：把 api_ping.go 的 audit detail 换回
// `fmt.Sprintf("修改延迟探测目标（%d 个，间隔 %d 秒）", …)`，本用例红在
// "审计 detail 应当带上目标地址"那一条 t.Errorf 上。
func TestPingSettingsAuditRecordsTargetAddresses(t *testing.T) {
	h := newAuthHarness(t)

	status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "DNS", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "NAS", "type": "icmp", "host": "10.0.0.5", "port": 0, "enabled": true},
			{"label": "旧的", "type": "icmp", "host": "10.0.0.9", "port": 0, "enabled": false},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("保存探测设置失败: %d %v", status, body)
	}

	status, body = h.get(t, "/api/v1/audit?limit=5")
	if status != http.StatusOK {
		t.Fatalf("读取操作记录失败: %d", status)
	}
	entries, _ := body["entries"].([]any)
	detail := ""
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["action"] != "settings_update" {
			continue
		}
		if d, _ := entry["detail"].(string); strings.Contains(d, "修改延迟探测目标") {
			detail = d
		}
	}
	if detail == "" {
		t.Fatalf("操作记录里没有 settings_update/修改延迟探测目标：%v", entries)
	}
	for _, want := range []string{"1.1.1.1:443", "10.0.0.5", "10.0.0.9", "停用"} {
		if !strings.Contains(detail, want) {
			t.Errorf("审计 detail 应当带上目标地址 %q，实际 %q", want, detail)
		}
	}
}
