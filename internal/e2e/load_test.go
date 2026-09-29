package e2e

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/config"
	"probe/internal/protocol"
)

// TestFiftyAgentsLoad 用 50 条真实 WebSocket 连接按 4 Hz 上报（约 200 帧/秒，
// 是设计负载 50×1Hz 的 4 倍），验证服务端跟得上、不丢帧、结束时没有 goroutine 泄漏。
//
// 这条用例是 Phase 11 的"回归护栏"：以后任何改动把服务端拖慢到跟不上，它都会失败。
func TestFiftyAgentsLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过负载用例")
	}

	const (
		agents      = 50
		rateHz      = 4
		seconds     = 6
		totalFrames = agents * rateHz * seconds
		allowedLoss = 0.05 // 允许 5% 的调度抖动，超出即认为"跟不上"
	)

	logs := &captureHandler{}
	h := startServerFull(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"),
		slog.New(logs), func(c *config.Server) {
			c.FlushInterval = time.Second
			// 负载用例的 50 条连接都来自 127.0.0.1：把每 IP 上限放宽到不成为瓶颈
			// （生产默认 20 是防滥用的，这里要测的是"跟不跟得上"）。
			c.AgentMaxPerIP = agents + 10
		})
	// 建 50 个节点（先完成初始化）。
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	type target struct {
		id    int64
		token string
	}
	targets := make([]target, 0, agents)
	for i := 0; i < agents; i++ {
		id, token := createNodeViaAPI(t, br, "load-"+strconv.Itoa(i+1))
		targets = append(targets, target{id: id, token: token})
	}

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var sendErr error
	var mu sync.Mutex
	sent := 0

	for _, tr := range targets {
		wg.Add(1)
		go func(tr target) {
			defer wg.Done()
			conn, _, err := dialAgentWS(ctx, "http://"+h.addr, tr.token)
			if err != nil {
				mu.Lock()
				sendErr = err
				mu.Unlock()
				return
			}
			defer func() { _ = conn.CloseNow() }()

			hello, err := protocol.New(protocol.TypeHello, protocol.Hello{
				AgentVersion: "load-test",
				Hostname:     "load",
				IntervalSec:  1,
				OS:           protocol.OSInfo{Name: "test", Kernel: "test", Arch: "amd64"},
				CPU:          protocol.CPUInfo{Model: "test", Cores: 2},
				BootID:       "boot-" + strconv.FormatInt(tr.id, 10),
			})
			if err != nil {
				return
			}
			if err := writeEnvelope(ctx, conn, hello); err != nil {
				return
			}
			if _, err := readEnvelope(ctx, conn); err != nil {
				return
			}

			ticker := time.NewTicker(time.Second / rateHz)
			defer ticker.Stop()
			seq := uint64(0)
			deadline := time.Now().Add(seconds * time.Second)
			for time.Now().Before(deadline) {
				<-ticker.C
				seq++
				frame, err := protocol.New(protocol.TypeMetrics, protocol.Metrics{
					CPUPct:   float64(seq % 100),
					CPUCores: 2,
					Mem:      protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
					Disk:     []protocol.Disk{{Mount: "/", FS: "ext4", Total: 1 << 40, Used: 1 << 39, Pct: 50}},
					Load:     protocol.Load{L1: 0.5},
					Net:      protocol.Net{Iface: "eth0", RxRate: 1024, TxRate: 512, RxTotal: seq << 20, TxTotal: seq << 19},
					LatMS:    10,
				})
				if err != nil {
					return
				}
				frame.Seq = seq
				if err := writeEnvelope(ctx, conn, frame); err != nil {
					return
				}
				mu.Lock()
				sent++
				mu.Unlock()
			}
		}(tr)
	}

	wg.Wait()
	if sendErr != nil {
		t.Fatalf("连接失败: %v", sendErr)
	}

	// 服务端处理了多少帧：把所有节点的 seq 加起来比对。
	state := h.srv.State()
	processed := uint64(0)
	gaps := uint64(0)
	for _, tr := range targets {
		node, ok := state.Get(tr.id)
		if !ok {
			t.Fatalf("节点 %d 没有状态", tr.id)
		}
		processed += node.Seq
		gaps += node.Gap
	}

	if gaps != 0 {
		t.Fatalf("服务端报告了 %d 次序号跳变（说明丢帧）", gaps)
	}
	loss := 1 - float64(processed)/float64(totalFrames)
	if loss > allowedLoss {
		t.Fatalf("服务端只处理了 %d/%d 帧（丢失 %.1f%%），超出 %.0f%% 的容差",
			processed, totalFrames, loss*100, allowedLoss*100)
	}
	t.Logf("处理 %d/%d 帧（%.1f%%），无序号跳变", processed, totalFrames, float64(processed)/float64(totalFrames)*100)

	// API 仍然要快：50 节点列表在 100ms 内返回。
	start := time.Now()
	if status, _ := br.do(http.MethodGet, "/api/v1/nodes", nil, false); status != http.StatusOK {
		t.Fatalf("列表接口状态码 = %d", status)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("50 节点列表用时 %s，超过 100ms", elapsed)
	} else {
		t.Logf("50 节点列表用时 %s", elapsed)
	}

	// 连接全部关闭后，goroutine 应当回落到接近开始时的数量。
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+10 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	if after > before+10 {
		t.Fatalf("goroutine 从 %d 涨到 %d，疑似泄漏", before, after)
	}
	t.Logf("goroutine：%d → %d", before, after)
}

// dialAgentWS 建立一条 Agent WebSocket 连接（带 Bearer Token）。
func dialAgentWS(ctx context.Context, base, token string) (*websocket.Conn, *http.Response, error) {
	url := "ws" + base[len("http"):] + "/api/v1/agent/ws"
	return websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{"probe.v1"},
	})
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	data, err := env.Encode()
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (protocol.Envelope, error) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	return protocol.Decode(data)
}
