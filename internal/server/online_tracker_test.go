package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

// newNodeForOnlineTest 造一个能通过存储层校验的最小节点。
func newNodeForOnlineTest(name string) store.NewNode {
	return store.NewNode{Name: name, IntervalSec: 1, ResetDay: 1, TrafficWarnPct: 80}
}

// 连续在线时长的语义：在线时持续累加、变成 stale/offline/unknown 就归零、
// 重新上线从那一刻重新起算（而不是接着上一段）。
//
// 判定沿用 buildNodeDTO 的那一套（last_seen + StaleAfter/OfflineAfter），
// 所以这里只喂"上报时刻"，状态由服务端自己算 —— 测试里不重写一份判定。
// 默认阈值：stale 10s / offline 30s（见 config.Default）。
func TestNodeOnlineSecSemantics(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	t0 := time.Now()

	node, _, err := s.db.CreateNode(ctx, newNodeForOnlineTest("online-1"), t0)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	onlineSecAt := func(at time.Time) uint64 {
		t.Helper()
		st, ok := s.state.Get(node.ID)
		if !ok {
			t.Fatalf("%s 时内存里没有节点状态", at)
		}
		return s.dtoFor(node, st, true, at).OnlineSec
	}

	// 第一次观测到在线：起点就是这一刻，时长从 0 开始。
	onlineState(t, s, node.ID, t0, protocol.Metrics{})
	if got := onlineSecAt(t0); got != 0 {
		t.Fatalf("刚上线时 online_sec = %d，期望 0", got)
	}
	// 持续在线：一路累加。
	if got := onlineSecAt(t0.Add(5 * time.Second)); got != 5 {
		t.Fatalf("在线 5 秒后 online_sec = %d，期望 5", got)
	}
	if got := onlineSecAt(t0.Add(9 * time.Second)); got != 9 {
		t.Fatalf("在线 9 秒后 online_sec = %d，期望 9", got)
	}

	// 10 秒没有通信 → stale（仍在 offline 阈值内）：清零。
	if got := onlineSecAt(t0.Add(20 * time.Second)); got != 0 {
		t.Fatalf("抖动时 online_sec = %d，期望 0（连续在线已经中断）", got)
	}
	// 超过 30 秒 → offline：同样清零。
	if got := onlineSecAt(t0.Add(45 * time.Second)); got != 0 {
		t.Fatalf("离线时 online_sec = %d，期望 0", got)
	}

	// 重新上线：从**这一次**的时刻重新起算。若实现里把旧起点留着了，
	// 这里会读到一个很大的数（把中间那段掉线也算了进去）。
	reconnect := t0.Add(46 * time.Second)
	onlineState(t, s, node.ID, reconnect, protocol.Metrics{})
	if got := onlineSecAt(reconnect); got != 0 {
		t.Fatalf("重新上线那一刻 online_sec = %d，期望 0", got)
	}
	if got := onlineSecAt(reconnect.Add(7 * time.Second)); got != 7 {
		t.Fatalf("重新上线 7 秒后 online_sec = %d，期望 7（不能接着上一段算）", got)
	}

	// 内存里没有这个节点的状态（从未连接）＝ unknown：也是 0，而不是报错。
	if got := s.dtoFor(node, state.Node{}, false, reconnect).OnlineSec; got != 0 {
		t.Fatalf("未知状态时 online_sec = %d，期望 0", got)
	}
}

// 「连续在线时长」必须扛得住服务端重启：起点存在 node_runtime.online_since，
// 重启后如果那个节点按同一套判定仍然在线，就接着原来的起点累加，而不是从零开始。
//
// 用真实的库 + 两次 Open 模拟重启（第二次 Open 会跑一遍迁移，走的正是线上路径）。
func TestNodeOnlineSecSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")
	logger := slog.New(slog.DiscardHandler)
	now := time.Now()

	// ---- 第一次启动 ----
	db1, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("首次打开数据库: %v", err)
	}
	srv1 := New(config.Default(), db1, logger, time.UTC)
	// 先把"当前有哪些节点"记账到内存里：真实路径上它由 1 Hz 的实时循环推到
	// （见 realtimeLoop → currentNodes → dtoFor），这里直接读一次视图。
	if _, err := srv1.currentNodes(ctx); err != nil {
		t.Fatalf("首次启动的节点视图: %v", err)
	}

	// 节点 A：已经连续在线一小时（起点在一小时前），刚刚还上报过。
	onlineNode, _, err := db1.CreateNode(ctx, newNodeForOnlineTest("keep-online"), now)
	if err != nil {
		t.Fatalf("创建在线节点: %v", err)
	}
	// 节点 B：库里同样存着起点，但它在上次退出前就掉线了（last_seen 很旧）。
	offlineNode, _, err := db1.CreateNode(ctx, newNodeForOnlineTest("went-offline"), now)
	if err != nil {
		t.Fatalf("创建离线节点: %v", err)
	}
	if err := db1.UpsertRuntime(ctx, []store.RuntimeRow{
		{NodeID: onlineNode.ID, LastSeen: now.Unix(), Status: "online",
			OnlineSince: now.Add(-time.Hour).Unix()},
		{NodeID: offlineNode.ID, LastSeen: now.Add(-time.Hour).Unix(), Status: "offline",
			OnlineSince: now.Add(-2 * time.Hour).Unix()},
	}, now); err != nil {
		t.Fatalf("写入运行态: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}

	// ---- 第二次启动（同一个库）----
	db2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("重启后打开数据库: %v", err)
	}
	defer func() { _ = db2.Close() }()
	srv2 := New(config.Default(), db2, logger, time.UTC)
	if n, err := srv2.seedFromRuntime(ctx); err != nil || n != 2 {
		t.Fatalf("恢复运行态: n=%d err=%v", n, err)
	}

	nodes, err := srv2.currentNodes(ctx)
	if err != nil {
		t.Fatalf("读取节点视图: %v", err)
	}
	got := map[string]nodeDTO{}
	for _, dto := range nodes {
		got[dto.Name] = dto
	}

	online := got["keep-online"]
	if online.Status != string(state.StatusOnline) {
		t.Fatalf("重启后这个节点应当仍然在线（last_seen 是刚刚），实际 %q", online.Status)
	}
	// 起点在一小时前：重启后应当接着算，而不是从 0 开始。
	if online.OnlineSec < 3600 {
		t.Fatalf("重启后 online_sec = %d，期望 ≥ 3600（继续累加，而不是从零开始）", online.OnlineSec)
	}
	// 也不该凭空多算：允许多跑几秒。
	if online.OnlineSec > 3600+60 {
		t.Fatalf("重启后 online_sec = %d，比一小时多出太多（把停机也算进去了？）", online.OnlineSec)
	}

	// 反面：库里存着起点、但节点已经掉线 → 不继承（停机那段时间不是"在线"）。
	offline := got["went-offline"]
	if offline.Status != string(state.StatusOffline) {
		t.Fatalf("这个节点的 last_seen 是一小时前，应当判定为离线，实际 %q", offline.Status)
	}
	if offline.OnlineSec != 0 {
		t.Fatalf("已经掉线的节点重启后 online_sec = %d，期望 0", offline.OnlineSec)
	}

	// 运行态落盘也要把在线起点写出去（下一次重启读的就是它）。
	srv2.flushRuntime(ctx)
	rows, err := db2.LoadRuntime(ctx)
	if err != nil {
		t.Fatalf("读取运行态: %v", err)
	}
	for _, row := range rows {
		if row.NodeID == onlineNode.ID && row.OnlineSince != now.Add(-time.Hour).Unix() {
			t.Fatalf("重启后写回的 online_since = %d，期望 %d（起点不该被重置）",
				row.OnlineSince, now.Add(-time.Hour).Unix())
		}
		if row.NodeID == offlineNode.ID && row.OnlineSince != 0 {
			t.Fatalf("掉线节点的 online_since = %d，期望 0", row.OnlineSince)
		}
	}
}
