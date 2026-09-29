package server

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
)

func TestHubBroadcastKeepsOnlyLatestForSlowClient(t *testing.T) {
	h := newHub(slog.New(slog.DiscardHandler))
	c := h.add("10.0.0.1")
	if c == nil {
		t.Fatal("第一条连接应当被接受")
	}

	h.broadcast([]byte("first"))
	h.broadcast([]byte("second"))
	h.broadcast([]byte("third"))

	select {
	case got := <-c.ch:
		if string(got) != "third" {
			t.Fatalf("慢客户端应当只保留最新值，实际 %q", got)
		}
	default:
		t.Fatal("槽里应当有值")
	}
	select {
	case got := <-c.ch:
		t.Fatalf("槽容量必须是 1，却读到了第二个值 %q", got)
	default:
	}

	if h.count() != 1 {
		t.Fatalf("客户端数 = %d", h.count())
	}
	h.remove(c)
	if h.count() != 0 {
		t.Fatalf("移除后客户端数 = %d", h.count())
	}
	// 移除后配额也要还回去，否则同一个 IP 会被永久卡住。
	if again := h.add("10.0.0.1"); again == nil {
		t.Fatal("移除后应当还能再连")
	}

	// 没有客户端时广播不应 panic。
	h.broadcast([]byte("nobody"))
}

func TestBroadcastDoesNotBlockOnStuckClient(t *testing.T) {
	h := newHub(slog.New(slog.DiscardHandler))
	stuck := h.add("10.0.0.1")
	fast := h.add("10.0.0.2")

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.broadcast([]byte("payload"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("广播被卡住的客户端阻塞了")
	}

	// 卡住的客户端槽里是最新值；正常客户端也能拿到值。
	select {
	case <-stuck.ch:
	default:
		t.Fatal("卡住的客户端槽里也应当有最新值")
	}
	select {
	case <-fast.ch:
	default:
		t.Fatal("正常客户端应当收到数据")
	}
}

// sseReader 读 SSE 流并把每个 data 负载推给 channel。
type sseReader struct {
	events chan []byte
	errors chan error
}

func startSSE(t *testing.T, h *authHarness) (*sseReader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.ts.URL+"/api/v1/stream", nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("连接 SSE 失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("SSE 状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("SSE Content-Type = %q", ct)
	}

	r := &sseReader{events: make(chan []byte, 16), errors: make(chan error, 1)}
	go func() {
		defer close(r.events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			r.events <- []byte(strings.TrimPrefix(line, "data: "))
		}
		if err := scanner.Err(); err != nil {
			r.errors <- err
		}
	}()

	stop := func() {
		cancel()
		_ = resp.Body.Close()
	}
	return r, stop
}

func (r *sseReader) next(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case payload, ok := <-r.events:
		if !ok {
			t.Fatal("SSE 流已关闭")
		}
		var out map[string]any
		if err := json.Unmarshal(payload, &out); err != nil {
			t.Fatalf("解析 SSE 数据失败: %v（%s）", err, payload)
		}
		return out
	case err := <-r.errors:
		t.Fatalf("SSE 读取失败: %v", err)
	case <-time.After(timeout):
		t.Fatal("等待 SSE 事件超时")
	}
	return nil
}

// nextMatching 持续读取事件直到满足条件：SSE 是异步的（1 Hz 广播），
// 用"等到出现目标状态"代替"读数第几个事件"才不会偶发失败。
func (r *sseReader) nextMatching(t *testing.T, timeout time.Duration, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		payload := r.next(t, time.Until(deadline))
		if match(payload) {
			return payload
		}
	}
	t.Fatalf("等待 SSE 事件超时: %s", what)
	return nil
}

// firstNode 取出负载里的第一个节点。
func firstNode(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	nodes, _ := payload["nodes"].([]any)
	if len(nodes) == 0 {
		return nil
	}
	node, _ := nodes[0].(map[string]any)
	return node
}

func TestStreamSendsSnapshotThenChanges(t *testing.T) {
	h := newAuthHarness(t)
	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "sse-01", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	reader, stop := startSSE(t, h)
	defer stop()

	// 1) 连上后先收到全量快照。
	first := reader.next(t, 5*time.Second)
	if first["type"] != "nodes" {
		t.Fatalf("首个事件类型 = %v", first["type"])
	}
	if nodes, _ := first["nodes"].([]any); len(nodes) != 1 {
		t.Fatalf("快照里节点数 = %d", len(nodes))
	}

	// 2) Agent 上报后，一秒内应当收到这个节点的变更。
	h.srv.State().Update(id, 1, protocol.Metrics{
		CPUPct: 42,
		Mem:    protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Net:    protocol.Net{Iface: "eth0", RxRate: 2048, TxRate: 1024},
		LatMS:  10, UptimeSec: 60,
	}, 0, time.Now())

	changed := reader.nextMatching(t, 5*time.Second, "带有 cpu_pct=42 的变更", func(p map[string]any) bool {
		n := firstNode(t, p)
		return n != nil && n["cpu_pct"] == 42.0
	})
	dto := firstNode(t, changed)
	if dto["rx_rate"] != 2048.0 || dto["status"] != "online" {
		t.Fatalf("变更集内容不对: %v", dto)
	}

	// 3) 汇总始终是全量的。
	summary, _ := changed["summary"].(map[string]any)
	if summary["total"] != float64(1) || summary["online"] != float64(1) {
		t.Fatalf("汇总不对: %v", summary)
	}
}

func TestStreamPushesStatusChangeWithoutNewMetrics(t *testing.T) {
	// 短阈值：让"时间流逝导致掉线"在测试里真的发生。
	cfg := config.Default()
	cfg.StaleAfter = 60 * time.Millisecond
	cfg.OfflineAfter = 120 * time.Millisecond
	h := newAuthHarnessWithConfig(t, cfg)

	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "sse-02", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	// 先让节点"在线"。
	h.srv.State().Update(id, 1, protocol.Metrics{Net: protocol.Net{Iface: "eth0"}}, 0, time.Now())

	reader, stop := startSSE(t, h)
	defer stop()
	_ = reader.next(t, 5*time.Second) // 快照

	// 之后不再上报，只让时间流逝：状态变化必须被推送出去。
	changed := reader.nextMatching(t, 6*time.Second, "状态变为 offline", func(p map[string]any) bool {
		n := firstNode(t, p)
		return n != nil && n["status"] == "offline"
	})
	dto := firstNode(t, changed)
	if dto["last_seen"] == float64(0) {
		t.Fatal("掉线推送里应当带最后通信时间")
	}
}
