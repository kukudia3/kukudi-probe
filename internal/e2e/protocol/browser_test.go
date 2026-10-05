package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"probe/internal/e2e/harness"
	"testing"
	"time"
)

// TestBrowserSeesLiveData 走完整条"用户在浏览器里能看到什么"的链路：
// 日志里读初始化码 → 初始化管理员 → 登录态 → 建节点拿 Token →
// 起真 Agent → SSE 收到这个节点的实时数据。
func TestBrowserSeesLiveData(t *testing.T) {
	logs := &harness.CaptureHandler{}
	h := harness.StartServerWithLogger(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), slog.New(logs))

	br := harness.NewBrowser(t, "http://"+h.Addr)

	// 初始化前，前端会看到 needs_setup=true。
	status, body := br.Do(http.MethodGet, "/api/v1/session", nil, false)
	if status != http.StatusOK || body["needs_setup"] != true {
		t.Fatalf("初始化前会话状态不对: %d %v", status, body)
	}

	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))
	nodeID, token := harness.CreateNodeViaAPI(t, br, "e2e-browser")

	client, _ := harness.NewClient(t, "http://"+h.Addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	events, stop := br.SSE("/api/v1/stream")
	defer stop()

	// Agent 的 hello 一落地（state.Attach）节点状态就已经是 online，而实时指标要等它
	// 紧接着发出的**第一拍 metrics** 才进内存：Attach 只填 LastSeen 与静态信息，
	// Metrics 仍是零值 ⇒ 那一刻的视图是"status=online、mem_pct=0、mem_total=0"。
	// 1 Hz 的推送循环完全可能正好落在两者之间，推出一条这样的帧 —— 那是握手期间的
	// 正常中间态，不是"与 /proc 快照不符"（实测 red：内存使用率 = 0；
	// 本地确定性复现见 _audit/ROUND3-VERIFY-4.md §2.2）。所以先等**带着上报快照**的
	// 那一帧：mem_total 只在真上报里才有值（hello 不带内存总量，见 dto.go 的
	// buildNodeDTO：MemTotal 取自 st.Metrics.Mem.Total）。等不到就如实报超时，
	// 绝不把"一直没上报"放过去。
	deadline := time.Now().Add(20 * time.Second)
	var lastOnline map[string]any
	for time.Now().Before(deadline) {
		select {
		case payload, ok := <-events:
			if !ok {
				t.Fatal("SSE 流已关闭")
			}
			nodes, _ := payload["nodes"].([]any)
			for _, raw := range nodes {
				n, _ := raw.(map[string]any)
				if id, _ := n["id"].(float64); int64(id) != nodeID {
					continue
				}
				if n["status"] != "online" {
					continue
				}
				lastOnline = n
				if memTotal, _ := n["mem_total"].(float64); memTotal <= 0 {
					// 还没收到第一拍上报：继续等下一帧（这里不下任何断言）。
					continue
				}
				memPct, _ := n["mem_pct"].(float64)
				if memPct < 51 || memPct > 52 {
					t.Fatalf("内存使用率 = %v，与 /proc 快照不符", memPct)
				}
				if n["iface"] != "eth0" || n["uptime_sec"] != float64(1234567) {
					t.Fatalf("节点视图内容不对: %v", n)
				}
				summary, _ := payload["summary"].(map[string]any)
				if summary["online"] != float64(1) || summary["total"] != float64(1) {
					t.Fatalf("汇总不对: %v", summary)
				}
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastOnline != nil {
		t.Fatalf("20 秒内只等到「在线但还没有上报快照」的帧（mem_pct=%v mem_total=%v）：Agent 的实时数据始终没有到浏览器",
			lastOnline["mem_pct"], lastOnline["mem_total"])
	}
	t.Fatal("20 秒内没有等到节点的实时数据")
}

// TestNodeListHidesTokenFromBrowser 验证列表接口不会把 Token 之类的敏感字段漏给前端。
func TestNodeListHidesTokenFromBrowser(t *testing.T) {
	logs := &harness.CaptureHandler{}
	h := harness.StartServerWithLogger(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), slog.New(logs))

	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))
	harness.CreateNodeViaAPI(t, br, "hide-01")

	status, body := br.Do(http.MethodGet, "/api/v1/nodes", nil, false)
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化响应: %v", err)
	}
	for _, needle := range []string{"pba_", "token_hash", "token"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("列表响应里出现了敏感内容 %q: %s", needle, raw)
		}
	}
}
