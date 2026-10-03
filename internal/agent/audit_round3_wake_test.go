package agent

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"probe/internal/protocol"
)

// 03-A-7 的注入式验证：跨会话共享的 Prober，旧 Run 会不会吃掉新会话的 wake。
//
// 生产形态（client.go:339-348）：
//
//	sctx, cancel := context.WithCancel(ctx)
//	defer cancel()
//	...
//	go c.pings.Run(sctx)   // 每个会话起一个新的 Run，且**不等**上一个 Run 退出
//
// wake 是容量 1 的共享通道（ping.go:109-110），Run 的 select 同时等 ctx.Done()
// 与 p.wake（ping.go:305-309）。于是并存的两个 Run 都会去收同一个 wake。
//
// 这里能注入的是 p.measureTCP（ping.go:126-131）：换成脚本化的测量之后，
// "旧 Run 还停在 select 上、新 Run 已经应用了旧配置"这个时序可以精确摆出来。

// srTarget 造一个合法的 TCP 探测目标（端口只是占位，不会真的去连）。
func srTarget(id int64) protocol.PingTarget {
	return protocol.PingTarget{ID: id, Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: int(id)}
}

// srProber 造一个测量被脚本化的 Prober：握手恒成功，但**尊重 ctx** ——
// 已经取消的 ctx 不能留下结果（probeTCPMeasure 会 skip，见 ping.go:405-411），
// 因此"结果里出现了新目标"只可能来自那个 ctx 还活着的 Run。
func srProber() *Prober {
	p := NewProber(slog.New(slog.DiscardHandler))
	p.measureTCP = func(ctx context.Context, _ protocol.PingTarget) (float64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return 0.5, nil
	}
	return p
}

func srHasResult(p *Prober, id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.results[id]
	return ok
}

// srWaitResult 在规定时间内等某个目标的结果出现；出现返回 true。
func srWaitResult(p *Prober, id int64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if srHasResult(p, id) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// TestProberWakeCanBeEatenByCancelledRun 构造"旧 Run 与新会话并存"的时序，
// 数一数有多少轮新 Run 收不到本该属于它的唤醒。
//
// 每一轮的时序：
//
//	旧 Run 起（ctx 活着）→ 应用目标 A → 停在 select
//	新 Run 起（ctx 活着）→ 首轮 snapshot 拿到的还是 A（p.targets 跨会话不清空）→ 停在 select
//	取消旧 Run 的 ctx（= 旧会话结束），**紧接着**下发目标 B（= 新会话的 config 帧）
//	→ 这一次 wake 只有一个：要么新 Run 拿到（应用 B），要么旧 Run 拿走（新 Run 永远看不到 B）
//
// 每一轮末尾都有一次对照 update(C)：新 Run 若连 C 都收不到，说明它自己出了问题，
// 这一轮判为"不可判定"而不是"被吃"。这样"没有 B 但有 C"才真的是 wake 被吃。
func TestProberWakeCanBeEatenByCancelledRun(t *testing.T) {
	// 两种调度形态都跑一遍，因为它们的证据强度不同：
	//
	//  默认 GOMAXPROCS：cancel 之后旧 Run 可能立刻在别的 P 上跑完 select 清理，
	//                   "0 次被吃"可能只是因为旧 Run 已经退出了（弱证据）。
	//  GOMAXPROCS=1：  cancel 只把旧 Run 置为可运行，测试 goroutine 不让出 P，
	//                   紧接着的 update(B) 一定发生在旧 Run 做 select 清理**之前**
	//                   —— 也就是说唤醒是"当着旧 Run 还在 select 上登记的 sudog"
	//                   发出去的。这一档 0 次被吃才是硬证据。
	for _, procs := range []int{0, 1} {
		name := "默认GOMAXPROCS"
		if procs == 1 {
			name = "GOMAXPROCS=1"
		}
		t.Run(name, func(t *testing.T) {
			if procs > 0 {
				prev := runtime.GOMAXPROCS(procs)
				defer runtime.GOMAXPROCS(prev)
			}
			runWakeRounds(t)
		})
	}
}

func runWakeRounds(t *testing.T) {
	const (
		rounds  = 50
		settle  = 15 * time.Millisecond
		window  = 120 * time.Millisecond
		control = 2 * time.Second
	)

	var stolen, delivered, undecided int
	for i := 0; i < rounds; i++ {
		p := srProber()
		a, b, c := srTarget(101), srTarget(102), srTarget(103)

		// —— 旧会话：Run 应用 A 之后停在 select 上 ——
		zctx, zcancel := context.WithCancel(context.Background())
		var zwg sync.WaitGroup
		zwg.Add(1)
		go func() {
			defer zwg.Done()
			p.Run(zctx)
		}()
		p.update([]protocol.PingTarget{a}, time.Hour)
		if !srWaitResult(p, a.ID, control) {
			zcancel()
			zwg.Wait()
			t.Fatalf("第 %d 轮：旧 Run 没有应用初始目标 A", i)
		}

		// —— 新会话：新 Run 起，首轮拿到的仍是 A ——
		vctx, vcancel := context.WithCancel(context.Background())
		var vwg sync.WaitGroup
		vwg.Add(1)
		go func() {
			defer vwg.Done()
			p.Run(vctx)
		}()
		time.Sleep(settle)

		// —— 旧会话结束（cancel），紧接着新会话的 config 帧到达 ——
		zcancel()
		p.update([]protocol.PingTarget{b}, time.Hour)

		gotB := srWaitResult(p, b.ID, window)

		// —— 对照：新 Run 必须还能接住后面的配置变更 ——
		p.update([]protocol.PingTarget{c}, time.Hour)
		gotC := srWaitResult(p, c.ID, control)

		vcancel()
		zwg.Wait()
		vwg.Wait()

		switch {
		case !gotC:
			undecided++
		case gotB:
			delivered++
		default:
			stolen++
		}
	}

	t.Logf("轮数=%d 新 Run 收到=%d 被旧 Run 吃掉=%d 不可判定=%d", rounds, delivered, stolen, undecided)
	if delivered+stolen == 0 {
		t.Fatalf("全部 %d 轮都不可判定，用例本身失效", rounds)
	}
	if stolen > 0 {
		t.Errorf("确认 wake 会被已取消的旧 Run 吃掉：%d/%d 轮新 Run 永远没应用新目标", stolen, delivered+stolen)
	}
}

// srOriginKey 用来把"这个 worker 是哪个 Run 起的"标出来：两个 Run 的 ctx 各自
// 挂一个不同的值，worker 拿到的 cctx 派生自哪个 ctx 就带哪个值。
type srOriginKey struct{}

func srProberWithOrigin(seen chan string) *Prober {
	p := NewProber(slog.New(slog.DiscardHandler))
	p.measureTCP = func(ctx context.Context, target protocol.PingTarget) (float64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if target.ID == 302 {
			if who, _ := ctx.Value(srOriginKey{}).(string); who != "" {
				select {
				case seen <- who:
				default:
				}
			}
		}
		return 0.5, nil
	}
	return p
}

// TestProberWakeGoesToTheRunThatParkedFirst 是上面那条的对照：
// 把两个 Run 的 ctx 都**保持存活**，再下发新目标 —— 这一次没有"ctx 已取消"这层保护，
// 共享 wake 只递给了先停在 select 上的那个 Run（旧 Run），后起的越来越拿不到配置。
//
// 这条用例本身**不是**在报一个生产可达的缺陷（生产上旧会话的 sctx 一定先被取消，
// 见 client.go:339-358）：它是要把"上面那条为什么不会出事"讲清楚 ——
// 安全性来自"旧 Run 的 ctx 先取消"这个次序，而不是共享 wake 这个设计本身。
func TestProberWakeGoesToTheRunThatParkedFirst(t *testing.T) {
	seen := make(chan string, 16)
	p := srProberWithOrigin(seen)
	a, b := srTarget(301), srTarget(302)

	oldCtx, oldCancel := context.WithCancel(context.WithValue(context.Background(), srOriginKey{}, "旧Run"))
	defer oldCancel()
	var oldWG sync.WaitGroup
	oldWG.Add(1)
	go func() {
		defer oldWG.Done()
		p.Run(oldCtx)
	}()

	p.update([]protocol.PingTarget{a}, time.Hour)
	if !srWaitResult(p, a.ID, 2*time.Second) {
		t.Fatal("旧 Run 没有应用初始目标")
	}

	newCtx, newCancel := context.WithCancel(context.WithValue(context.Background(), srOriginKey{}, "新Run"))
	defer newCancel()
	var newWG sync.WaitGroup
	newWG.Add(1)
	go func() {
		defer newWG.Done()
		p.Run(newCtx)
	}()
	time.Sleep(15 * time.Millisecond)

	p.update([]protocol.PingTarget{b}, time.Hour)

	var got string
	select {
	case got = <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("没有任何 Run 应用新目标 B")
	}
	t.Logf("新目标 B 的 worker 来自：%s", got)
	if got != "旧Run" {
		t.Fatalf("预期先停到 select 的旧 Run 拿走唤醒，实际是 %q —— 说明唤醒的归属会变，结论需要重写", got)
	}
}

// TestProberOldRunExitsWellBeforeReconnect 量一量"旧 Run 从 ctx 取消到真正退出"要多久，
// 用来给上面那条的可达性定界：生产上旧会话结束到新会话的 config 帧之间，
// 至少隔着一次重连退避（initialBackoff=1s，jitter 后 ≥0.8s，client.go:29/230）
// 加一次 TCP+WebSocket 握手，而旧 Run 只需要在这段时间里被调度到一次。
func TestProberOldRunExitsWellBeforeReconnect(t *testing.T) {
	const rounds = 30

	var worst time.Duration
	for i := 0; i < rounds; i++ {
		p := srProber()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			p.Run(ctx)
			close(done)
		}()
		p.update([]protocol.PingTarget{srTarget(201)}, time.Hour)
		if !srWaitResult(p, 201, 2*time.Second) {
			cancel()
			<-done
			t.Fatalf("第 %d 轮：Run 没有应用初始目标", i)
		}

		start := time.Now()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 轮：Run 在 ctx 取消后 5 秒仍未退出", i)
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
	}

	// 重连退避的下界：jitter(1s) ∈ [0.8s, 1.2s]（client_test.go 里 jitter(10s) 的界推得）。
	const reconnectFloor = 800 * time.Millisecond
	// 纳秒分辨率下 worst 常常不足 1µs，除法的分母取 1µs 避免报出天文数字。
	const resolution = time.Microsecond
	divisor := worst
	if divisor < resolution {
		divisor = resolution
	}
	t.Logf("旧 Run 退出耗时上界=%v，重连间隔下界=%v（相差约 %d 倍）",
		worst, reconnectFloor, int64(reconnectFloor/divisor))
	if worst >= reconnectFloor {
		t.Errorf("旧 Run 退出耗时 %v 已经不小于重连间隔下界 %v，可达性论证失效", worst, reconnectFloor)
	}
}
