package server

// 第三轮验证（D:\DEEPSEEK\_audit\ROUND3-VERIFY-2.md）⑤：**真服务端 + 真 SQLite**
// 量 ping_samples_1m 的增长速率。
//
// 与 internal/store 那条测量（round3_disk_growth_test.go，"字节/行"与"写入速率"）
// 的分工：那条量的是存储层常量，这条量的是**整条链路**（真 WebSocket Agent
// 每秒上报 → pingTracker 的配额闸门 → 真落盘），回答三个问题：
//
//  1. 修复后的上界"每节点每分钟 ≤32 行"在**真实帧**下成立吗；
//  2. 每节点每小时长多少字节（含索引与页开销）；
//  3. 8 天保留期下的稳态是多大（算术外推，假设写在下面）。
//
// ⚠️ 诚实边界（必须与数字一起读）：
//   - 观测**时刻**是注入的（`ping.observe(nodeID, results, ts)`）。真实链路里
//     这个时刻来自 time.Now()，不注入就得真等 60 分钟 —— 而落盘行数与时刻无关，
//     只与"这一分钟里这个节点有几个目标"有关（tracker 按 (node,target) 去重）。
//   - 真实 1 Hz 上报阶段是**真帧、真连接、真 tracker**，用它验证配额上界；
//     后面的 60 个"分钟"是同一套 tracker + 同一套 store API，只换了时钟。
//   - 8 天稳态 = 插入速率 × 保留期，**没有**跑 8 天，也没有算 purge 的删页
//     （SQLite 删行不缩文件；稳态大小是"水位"不是"实际使用"）。
//
// 命令：go test ./internal/server/ -run TestPingDiskGrowthPerNodeHour -count=1 -v

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/store"
)

// measureFileSize 返回文件大小（不存在记 0）。
func measureFileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// measureCheckpoint 把 WAL 折回主库并返回主库大小。
func measureCheckpoint(t *testing.T, s *Server, path string) int64 {
	t.Helper()
	if _, err := s.db.Writer().ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	return measureFileSize(t, path)
}

// pingRowCountsByNode 数每个节点在 ping_samples_1m 里的行数。
func pingRowCountsByNode(t *testing.T, s *Server) map[int64]int64 {
	t.Helper()
	rows, err := s.db.Reader().QueryContext(context.Background(),
		`SELECT node_id, count(*) FROM ping_samples_1m GROUP BY node_id`)
	if err != nil {
		t.Fatalf("统计探测行: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int64{}
	for rows.Next() {
		var id, n int64
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

// pingMeasurement 是一次测量的环境。
type pingMeasurement struct {
	ts      *httptest.Server
	s       *Server
	path    string
	nodes   []store.Node
	targets []int64
	conns   []*websocket.Conn
}

// newPingMeasurement 起一个真服务端：真 SQLite 文件、16 个配置内探测目标、
// n 个节点、n 条真 WebSocket Agent 连接（都完成 hello/welcome/config）。
func newPingMeasurement(t *testing.T, n int) *pingMeasurement {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	// 50 台机器都从 127.0.0.1 连过来（测试环境），把每 IP / 总连接上限让开，
	// 否则第 21 条连接会被"同一来源的 Agent 连接过多"挡掉 —— 那是另一条防线，
	// 与本用例要量的磁盘增长无关。
	cfg.AgentMaxPerIP = 4 * n
	cfg.AgentMaxConns = 4 * n

	s := New(cfg, db, slog.New(slog.DiscardHandler), time.UTC)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// 16 个配置内目标：这是"正常部署"的上界（protocol.MaxPingTargets = 16）。
	saved, err := s.db.SetPingSettings(ctx, makeTargets(protocol.MaxPingTargets), 60)
	if err != nil {
		t.Fatalf("配置探测目标: %v", err)
	}
	if len(saved.Targets) != protocol.MaxPingTargets {
		t.Fatalf("目标数 = %d，期望 %d", len(saved.Targets), protocol.MaxPingTargets)
	}

	m := &pingMeasurement{ts: ts, s: s, path: path}
	for _, tg := range saved.Targets {
		m.targets = append(m.targets, tg.ID)
	}
	for i := 0; i < n; i++ {
		node, token, err := s.db.CreateNode(ctx, store.NewNode{
			Name: fmt.Sprintf("g-%02d", i+1), IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
		}, time.Now())
		if err != nil {
			t.Fatalf("创建节点 %d: %v", i+1, err)
		}
		m.nodes = append(m.nodes, node)
		conn := mustDialAgent(t, ts, token)
		sendFrame(t, conn, helloFrame(t, testHello()))
		readHandshake(t, conn) // welcome + config（白名单就是这一帧里的目标）
		m.conns = append(m.conns, conn)
	}
	// 等白名单真的落地：sendConfig 写成功之后才会 setAllowed。
	waitFor(t, 5*time.Second, "16 个目标进入白名单", func() bool {
		return len(allowedIDs(s)) == protocol.MaxPingTargets
	})
	return m
}

func makeTargets(n int) []store.PingTarget {
	out := make([]store.PingTarget, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.PingTarget{
			Label: fmt.Sprintf("t-%02d", i), Type: protocol.PingTypeICMP,
			Host: fmt.Sprintf("10.9.%d.%d", i/250, i%250+1), Enabled: true,
		})
	}
	return out
}

// TestPingDiskGrowthPerNodeHour 对 1 / 10 / 50 个节点各量一次。
func TestPingDiskGrowthPerNodeHour(t *testing.T) {
	for _, n := range []int{1, 10, 50} {
		t.Run(fmt.Sprintf("%d节点", n), func(t *testing.T) {
			m := newPingMeasurement(t, n)
			ctx := context.Background()

			// ---------- 阶段 1：真实 1 Hz 上报（真帧、真连接、真 tracker）----------
			// 一帧最多带 protocol.MaxPingTargets(16) 个结果（ValidateMetrics 硬上限），
			// 所以"16 个配置内 + 尽量多的伪造"要拆成多帧 —— 这正是真实放大路径的形状
			// （报告 03-A-1：每帧 16 个新 ID，每秒 5 帧）。
			//
			// 帧 1 = 16 个配置内目标；帧 2..4 = 各 16 个全新伪造 ID（共 48 个）。
			// 期望落库：16（配置内，永远接纳）+ 16（配置外配额）= 32 行。
			const forgedFrames = 3
			configured := map[int64]bool{}
			for _, id := range m.targets {
				configured[id] = true
			}
			// 按"波"发：每一波给所有连接各发一帧，波与波之间睡 150ms。
			// 这样 50 条连接也只等 3 个 150ms（而不是 50×3 个），用例不至于跑 20 秒。
			for f := 0; f <= forgedFrames; f++ {
				for i, conn := range m.conns {
					pings := make([]protocol.PingResult, 0, protocol.MaxPingTargets)
					if f == 0 {
						for _, id := range m.targets {
							pings = append(pings, pingResult(id))
						}
					} else {
						for j := 0; j < protocol.MaxPingTargets; j++ {
							pings = append(pings, pingResult(int64(900000+i*1000+(f-1)*16+j)))
						}
					}
					sendPingsFrame(t, conn, uint64(f+1), pings)
				}
				// 上报限流 5 帧/秒：帧之间留够间隔，别让"被限流丢掉"混进结论。
				time.Sleep(150 * time.Millisecond)
			}
			for _, node := range m.nodes {
				waitAgentSeq(t, m.s, node.ID, forgedFrames+1)
			}
			m.s.flushPings(ctx)

			realRows := pingRowCountsByNode(t, m.s)
			byTarget := pingRowsByTarget(t, m.s, m.nodes[0].ID)
			forgedLanded := 0
			for id, cnt := range byTarget {
				if !configured[id] {
					forgedLanded += cnt
				}
			}
			for _, node := range m.nodes {
				got := realRows[node.ID]
				// 上界：16 个配置内 + 16 个配置外配额 = 32。
				if got > 32 {
					t.Fatalf("节点 %d 一分钟落了 %d 行，超过上界 32", node.ID, got)
				}
				// 下界用来防"空断言"：连配置内的目标都没写全，说明上面根本没落盘。
				if got < protocol.MaxPingTargets {
					t.Fatalf("节点 %d 只落了 %d 行，连配置内的 %d 个目标都没写全",
						node.ID, got, protocol.MaxPingTargets)
				}
			}
			if forgedLanded > protocol.MaxPingTargets {
				t.Fatalf("发出去 %d 个伪造 ID，落了 %d 行，超过每周期配额 %d",
					forgedFrames*protocol.MaxPingTargets, forgedLanded, protocol.MaxPingTargets)
			}
			if forgedLanded == 0 {
				t.Fatal("伪造 ID 一行都没落：这条断言要证明的是「封顶」而不是「全丢」")
			}
			t.Logf("阶段1（真实帧，%d 节点 × 1 分钟）：每节点落 %d 行（上界 32）；"+
				"发出去 %d 个伪造 ID，落库 %d 行",
				n, realRows[m.nodes[0].ID], forgedFrames*protocol.MaxPingTargets, forgedLanded)

			// ---------- 阶段 2：每个"分钟"每节点 32 行（只把时钟注入）----------
			// 180 分钟：1 个节点也有 5760 行，能把 4 KiB 页量化的噪音压下去；
			// 50 个节点 = 288,000 行，约 3 秒。
			const minutes = 180
			before := measureFileSize(t, m.path)
			base := time.Now().Add(-time.Duration(minutes) * time.Minute)
			start := time.Now()
			for mi := 0; mi < minutes; mi++ {
				at := base.Add(time.Duration(mi) * time.Minute)
				for _, node := range m.nodes {
					results := make([]protocol.PingResult, 0, 32)
					for _, id := range m.targets {
						results = append(results, pingResult(id))
					}
					for j := 0; j < protocol.MaxPingTargets; j++ {
						// 每个"分钟"换一批新伪造 ID（与真实放大路径同形）。
						results = append(results, pingResult(int64(1_000_000+mi*16+j)))
					}
					m.s.ping.observe(node.ID, results, at)
				}
				m.s.flushPings(ctx)
			}
			elapsed := time.Since(start)

			wal := measureFileSize(t, m.path+"-wal") // checkpoint 之前看 WAL 高水位
			after := measureCheckpoint(t, m.s, m.path)

			totalRows := int64(0)
			for _, cnt := range pingRowCountsByNode(t, m.s) {
				totalRows += cnt
			}
			grew := after - before
			// 折算成"每节点每小时"（本用例跑的是 minutes 分钟）。
			perNodeHour := grew * 60 / int64(minutes) / int64(n)
			bytesPerRow := float64(grew) / float64(minutes*32*n)
			// 8 天稳态（算术外推，假设：保留期 8 天、每节点每分钟稳定 32 行、
			// 期间不 purge 也不删页；SQLite 删行不缩文件，所以这是"水位"）。
			eightDaysRows := int64(11520) * 32
			eightDaysPerNode := float64(eightDaysRows) * bytesPerRow
			t.Logf("阶段2（%d 节点 × %d 分钟 × 32 行）：总行数 %d，耗时 %s；"+
				"主库 +%d B ⇒ **%d B/节点/小时**、%.1f B/行；-wal 高水位 %d B",
				n, minutes, totalRows, elapsed.Round(time.Millisecond), grew,
				perNodeHour, bytesPerRow, wal)
			t.Logf("    外推 8 天稳态（%d 行/节点）：约 %.1f MB/节点（%.2f GB/50 节点）",
				eightDaysRows, eightDaysPerNode/(1<<20), eightDaysPerNode*50/(1<<30))

			// WAL 是**回收**的，不是只增不减：折回主库、再写一个"分钟"，
			// WAL 应当回到几十 KB 量级（auto-checkpoint 水位是 1000 页 = 4 MiB）。
			walAfterIdle := measureFileSize(t, m.path+"-wal")
			oneMore := time.Now()
			for _, node := range m.nodes {
				results := make([]protocol.PingResult, 0, 32)
				for _, id := range m.targets {
					results = append(results, pingResult(id))
				}
				for j := 0; j < protocol.MaxPingTargets; j++ {
					results = append(results, pingResult(int64(2_000_000+j)))
				}
				m.s.ping.observe(node.ID, results, oneMore)
			}
			m.s.flushPings(ctx)
			walSteady := measureFileSize(t, m.path+"-wal")
			t.Logf("    -wal：写完 180 分钟后(未 checkpoint) %d B；checkpoint 后 %d B；"+
				"再写 1 分钟后 %d B（稳态水位，auto-checkpoint=1000 页/4 MiB）",
				wal, walAfterIdle, walSteady)
			if walSteady > 8*(1<<20) {
				t.Fatalf("-wal 稳态水位 %d B 超过 8 MiB：WAL 没有被回收", walSteady)
			}

			if totalRows < int64(minutes*32*n) {
				t.Fatalf("总行数 = %d，少于预期的 %d（有行没落盘）", totalRows, minutes*32*n)
			}
			if grew <= 0 {
				t.Fatal("主库一个字节都没长：行没有真的落盘")
			}
			// 一条防止"测量方法整体跑偏"的宽区间：一行 8~512 B（store 层单独量到 ~66 B/行）。
			if bytesPerRow < 8 || bytesPerRow > 512 {
				t.Fatalf("字节/行 = %.1f，超出合理区间 [8, 512]", bytesPerRow)
			}
		})
	}
}
