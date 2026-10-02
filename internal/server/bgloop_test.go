package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 这个文件是"用例启动的后台循环必须在它返回前停稳"的守卫。
//
// 为什么需要它：测试返回之后 Go 会去删 t.TempDir()，而只要还有一个循环在跑
// （它可能正卡在一次 SQLite 写入中间，或者刚走到"启动那几跳"的最后一段），
// SQLite 就会在删除的空隙里重建 -wal / -shm：Linux 的 RemoveAll 严格，于是 CI 上
//
//	TempDir RemoveAll cleanup: unlinkat ...: directory not empty
//
// 而 Windows 的 RemoveAll 宽容，同一份"漏等"的代码在本机**三轮全绿** ——
// 这正是这个 bug 能躲过本地验证的原因。所以守卫不去复现那条报错（那是平台的
// 脾气，本机复现不了），而是验证**根本性质**：用例返回时，它启动的循环必须
// 已经退出。这条性质与平台无关，在 Windows 上一样能红。
//
// 三层，缺一层都堵不住：
//
//  1. 账本：每一笔 start 都必须有人调用过 stop。**光有 defer cancel() 不算** ——
//     cancel 只是发信号，循环完全可能还在跑最后一段。这条判据是确定性的：
//     漏了就报，不依赖任何时序（见 bgGuard.checkLoops）。
//  2. 目录静默：登记过的数据目录在两次快照之间不许有任何变化（文件名、大小、
//     修改时间）。它就是"文件还在长"的那条检查，抓的是没记在账上的写手
//     （判据 1 只看得见走了账本的循环）。它是机会性的：写手正好在这段观察里
//     动一下才抓得到。
//  3. 静态规则：本包的测试源码里不许出现手写启动后台循环（见
//     TestBackgroundLoopsMustGoThroughGuard）。这一条补的正是判据 1 的盲区 ——
//     修复前那句 `go srv.pipelineLoop(ctx)` 在账本上根本不存在，
//     再确定性也报不出来，只能靠"不许这么写"。

// bgLoopStopTimeout 是"等后台循环退出"的上限。
//
// 10 秒远大于循环正常收尾需要的时间（它最多就是把手上那一次写库做完），
// 所以一旦撞上这个上限，基本可以断定它根本没在看 ctx.Done。
const bgLoopStopTimeout = 10 * time.Second

// bgDirQuietWindow 是"数据目录静默"两次快照之间的间隔。
//
// 100 毫秒足够让一次 SQLite 写入露出来（建 -wal/-shm、提交一页、动一次 mtime），
// 又不足以让整个包的测试时长明显变长。
const bgDirQuietWindow = 100 * time.Millisecond

// bgGuard 是"这个用例启动的后台循环"的账本。
//
// 用法：
//
//	guard := newBackgroundGuard(t)          // 尽量早，最好在 t.TempDir() 之前
//	guard.watchDir(filepath.Dir(dbPath))    // 可选：盯住数据目录
//	loop := guard.start("pipelineLoop", srv.pipelineLoop)
//	defer loop.stop()                       // 必须：等它真的退出
type bgGuard struct {
	t     *testing.T
	mu    sync.Mutex
	loops []*bgLoop
}

// bgLoop 是测试启动的一个后台循环的把手。
type bgLoop struct {
	guard  *bgGuard
	name   string
	origin string // 起它的那一行（"文件:行"），报错时直接指过去
	cancel context.CancelFunc
	done   chan struct{}

	// waited 表示有人（测试体或脚手架的清理）真的等过它。
	//
	// 它是判据 1 的全部依据，也是"确定性变红"的关键：光看 done 关没关是
	// **碰运气** —— defer cancel() 就在清理之前执行，循环可能刚巧已经退出，
	// 漏等的代码照样能蒙混过关。所以这里记的是"有没有人等过"，不是"它是不是
	// 刚好已经停了"。
	waited atomic.Bool
	// timedOut 表示等过但超时了（stop 已经报过一次，账本不再重复报）。
	timedOut atomic.Bool

	stopOnce sync.Once
}

// newBackgroundGuard 建一个账本，并登记"用例结束时核账"。
//
// 建议在用例最开头（t.TempDir() 之前）就建：t.Cleanup 是后进先出，早登记
// ⇒ 核账跑在清理链的最后。顺序只影响"能不能看到临时目录"，
// 账本判据本身与顺序无关。
func newBackgroundGuard(t *testing.T) *bgGuard {
	t.Helper()
	g := &bgGuard{t: t}
	t.Cleanup(g.checkLoops)
	return g
}

// watchDir 把一个数据目录登记进"用例结束时它必须是静的"名单。
//
// 检查挂在**这里**注册的清理上（而不是账本那一份）：它在数据库关闭与
// t.TempDir() 删除**之前**执行 —— 那才是"还有没有人在写"能被看到的时刻。
// 挂在用例最末尾的话，目录早被删掉了，这条检查只会平凡成立。
func (g *bgGuard) watchDir(dir string) {
	g.t.Cleanup(func() { g.checkDir(dir) })
}

// start 启动一个后台循环，并把"必须等它退出"这件事记在账上。
//
// run 拿到的是一个可取消的 context。守卫另外注册了一个**兜底 cancel**：
// 用例提前 t.Fatalf 时循环也不会活过这个用例。但注意兜底 cancel 不算等待 ——
// 它不碰 waited，所以账本上的亏空不会因此蒙混过关。
func (g *bgGuard) start(name string, run func(context.Context)) *bgLoop {
	g.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &bgLoop{
		guard:  g,
		name:   name,
		origin: bgCallerSpot(2),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(l.done)
		run(ctx)
	}()

	g.mu.Lock()
	g.loops = append(g.loops, l)
	g.mu.Unlock()

	g.t.Cleanup(cancel)
	return l
}

// stop 取消循环并**等它真的退出**（幂等，可以放心 defer）。
//
// 这是整个守卫存在的理由：只 cancel 是"发了信号就走"，循环可能还在跑最后一段
// （比如正把这一分钟的桶写进 SQLite）—— 而用例一返回，t.TempDir() 的清理就会
// 去删那个目录。等 done 关上，才谈得上"它不会再碰任何东西了"。
func (l *bgLoop) stop() {
	l.stopOnce.Do(func() {
		l.cancel()
		select {
		case <-l.done:
		case <-time.After(bgLoopStopTimeout):
			l.timedOut.Store(true)
			l.guard.t.Errorf("后台循环 %q（起于 %s）在 %s 内没有退出：它多半没有在 select 里看 ctx.Done",
				l.name, l.origin, bgLoopStopTimeout)
		}
		l.waited.Store(true)
	})
}

// checkLoops 是判据 1：每一笔账都要有人等过（挂在用例清理链的最后，见 newBackgroundGuard）。
func (g *bgGuard) checkLoops() {
	g.mu.Lock()
	loops := append([]*bgLoop(nil), g.loops...)
	g.mu.Unlock()

	for _, l := range loops {
		switch {
		case !l.waited.Load():
			g.t.Errorf("后台循环 %q（起于 %s）在用例返回前没有被等停。"+
				"cancel 只是发信号，循环可能还在写库；而用例一返回，t.TempDir() 的清理就会去删数据目录 —— "+
				"Linux 上这就是 RemoveAll 报 \"directory not empty\" 的来源（Windows 上看不出来）。"+
				"请在用例里 defer loop.stop()（或显式 loop.stop()）等它真的退出",
				l.name, l.origin)
		case l.timedOut.Load():
			// stop 已经报过超时，这里不重复。
		default:
			select {
			case <-l.done:
			default:
				g.t.Errorf("后台循环 %q（起于 %s）的退出口还没关上：账上说等过了，它却还在跑",
					l.name, l.origin)
			}
		}
		// 就算漏等了，也别把它带进同一个包里的下一个用例。
		l.cancel()
	}
}

// checkDir 是判据 2：登记过的目录里不许还有人在写（挂在 watchDir 注册的清理上）。
func (g *bgGuard) checkDir(dir string) {
	before := bgDirSnapshot(dir)
	if before == nil {
		// 目录已经不在了（t.TempDir() 的清理成功）＝ 没有残留，也没有东西还在写。
		return
	}
	time.Sleep(bgDirQuietWindow)
	after := bgDirSnapshot(dir)
	if after == nil {
		// 就是在这段观察里被删掉的：干净。
		return
	}
	if moved := bgDirDiff(before, after); moved != "" {
		g.t.Errorf("数据目录 %s 在用例返回后仍在变化（%s）：还有东西在写它，"+
			"这正是 Linux 上 RemoveAll 报 \"directory not empty\" 的来源", dir, moved)
	}
}

// bgDirSnapshot 记录目录里每个文件的大小与修改时间（目录不存在时返回 nil）。
func bgDirSnapshot(dir string) map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			// 刚被删掉/换掉：按"非空且未知"记，变化时能报出来。
			out[e.Name()] = "?"
			continue
		}
		out[e.Name()] = fmt.Sprintf("%d/%s", info.Size(), info.ModTime().Format(time.RFC3339Nano))
	}
	return out
}

// bgDirDiff 找出两份快照之间动过的地方（按名字排序，空串表示没动）。
func bgDirDiff(before, after map[string]string) string {
	var moved []string
	for name, state := range after {
		prev, ok := before[name]
		switch {
		case !ok:
			moved = append(moved, name+"（新出现）")
		case prev != state:
			moved = append(moved, name+"（大小或时间变了）")
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			moved = append(moved, name+"（被删掉）")
		}
	}
	sort.Strings(moved)
	return strings.Join(moved, "、")
}

// bgCallerSpot 返回"文件:行"，报错信息里直接指到起循环的那一行。
func bgCallerSpot(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "未知位置"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

// ---------------------------------------------------------------- 静态规则

// rawLoopStart 是"手写启动后台循环"的形态，例如把 pipelineLoop / realtimeLoop /
// dispatch.Start / Run 直接用 go 丢到后台。注意 `go func(){ ... }()` 不算：
// 那种写法至少还有一个显式的闭包，而这里的几种都是"把一个长期运行的执行体
// 启动完就走"。
var rawLoopStart = regexp.MustCompile(`^\s*go\s+[\w.]*\.(?:[A-Za-z]*Loop|Start|Run)\s*\(`)

// TestBackgroundLoopsMustGoThroughGuard 钉住"起后台循环必须走账本"。
//
// 为什么需要这条静态规则：账本只能看住**记在账上**的循环，一句手写的
// `go srv.pipelineLoop(ctx)`（修复前的原始写法）它完全看不见 —— 反向验证里
// 就是这么漏过去的：把等待去掉、只留一个 defer cancel()，账本无话可说，
// 因为压根没人告诉它有这么个循环。
//
// 所以这里把"绕过账本"这件事本身钉死：扫本包的测试源码，出现手写启动后台
// 循环就失败。于是只剩一条路 —— guard.start(...) 记账 + 等停，
// 而那条路上有确定性的账本判据（见 checkLoops）。
func TestBackgroundLoopsMustGoThroughGuard(t *testing.T) {
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("扫描本包测试源码: %v", err)
	}
	if len(paths) < 10 {
		// 扫不到文件时这条用例会"平凡通过"，那比没有它还糟。
		t.Fatalf("只扫到 %d 个测试文件，多半是工作目录不对", len(paths))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := line
			// 注释里的反例不算数（本文件上面就写着几个例子）。
			if at := strings.Index(code, "//"); at >= 0 {
				code = code[:at]
			}
			if rawLoopStart.MatchString(code) {
				t.Errorf("%s:%d 手写启动了后台循环：%s\n"+
					"两条路选一条：① guard := newBackgroundGuard(t); loop := guard.start(...); defer loop.stop()；"+
					"② 用 go func(){ ... }() 起、并在用例里自己等它（像 TestRunServesAndShutsDownGracefully 等 done 那样）。"+
					"只 cancel 不算等停，守卫也看不见手写的循环",
					path, i+1, strings.TrimSpace(line))
			}
		}
	}
}
