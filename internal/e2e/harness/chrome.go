package harness

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// FindChrome 找本机的 Chrome：先看 PROBE_CHROME，再看常见安装位置，最后 PATH。
func FindChrome() string {
	if p := os.Getenv("PROBE_CHROME"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			filepath.Join(os.Getenv("LOCALAPPDATA"), `Google\Chrome\Application\chrome.exe`),
		}
	case "darwin":
		candidates = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	default:
		candidates = []string{"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"}
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"chrome", "google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// SyncBuffer 是并发安全的 bytes.Buffer。
//
// 为什么需要它：exec 在 Stdout/Stderr 不是 *os.File 时会起一条拷贝 goroutine；
// Chrome 还在写的时候去读 bytes.Buffer 就是**数据竞争**（e2e 被排除在 -race 之外，
// 所以这条竞争从来没被报过）。而超时分支要的正是"进程还在跑的时候读一眼尾部"。
//
// 语义一个字没变：还是那串字节、还是从尾部取 1500 —— 只是读写都过同一把锁。
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ResultSource 是 RunChromeForResult 要的两样东西：
//
//   - Results()：页面 POST /__result 回来的那一份（**一个事件**）；
//   - Live()：mock 侧的存活账本（请求 + 解析期心跳；返回 nil 表示没有存活信号）。
//
// 为什么把两者绑在一起传：超时的意义完全取决于"页面死在哪一步"。只有结果通道时，
// 外层对页面状态零观测，只能说一句"等回传超时" —— 历史上 ui 的 240s、fx 的 120s
// 两次偶发正是这个形态（连卡在哪一步都没有）。
type ResultSource interface {
	Results() <-chan []byte
	Live() *Liveness
}

const (
	// watchPoll 是看门狗看账本的间隔：250ms 足够快，又不占着测试 goroutine。
	watchPoll = 250 * time.Millisecond
	// waitGrace 是"等 Chrome 真的退出"的上限。旧写法在 defer 里裸调 cmd.Wait()：
	// Chrome 万一不吐 EOF，那就是一处**没有上限**的等待（铁律不许）。
	waitGrace = 15 * time.Second
)

// RunChromeForResult 打开 pageURL，等页面自己把结果 POST 回来（或超时）。
// windowSize 为空时用默认窗口；窄屏用例靠它落进 (max-width: 640px) 那一档。
//
// extraArgs 是给个别用例补的开关（必须排在 URL 前面）。目前只有一个调用方用它：
// 「跨标签页登录」那条用例要在页面里 window.open 出第二个标签，而无头 Chrome
// 默认会把它当弹窗拦掉（--disable-popup-blocking）。
//
// 与旧版的差别只有一个：等待的**判据**从"只等结果"变成"等结果，同时盯存活信号"。
// 外层上限、Chrome 参数、返回的结果字节全都没动；快的那条路上一个字节都没多等。
func RunChromeForResult(t *testing.T, chrome, pageURL string, src ResultSource, timeout time.Duration, windowSize string, extraArgs ...string) []byte {
	t.Helper()
	// 画像目录单独开在系统临时目录里：Chrome 被强杀时子进程可能还握着里面的文件，
	// 放在 t.TempDir() 里会让"清理失败"变成一条测试失败（与被测代码无关）。
	profile, err := os.MkdirTemp("", "probe-tz-chrome-")
	if err != nil {
		t.Fatalf("创建 Chrome 画像目录: %v", err)
	}
	defer func() { _ = os.RemoveAll(profile) }()
	if windowSize == "" {
		windowSize = "1500,1100"
	}
	args := []string{
		"--headless=new",
		"--no-proxy-server",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--user-data-dir=" + profile,
		"--window-size=" + windowSize,
	}
	args = append(args, extraArgs...)
	args = append(args, pageURL)
	cmd := exec.Command(chrome, args...)
	var out SyncBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 Chrome: %v", err)
	}
	// 这里**故意不用 --dump-dom + --virtual-time-budget**：页面连着一路 SSE，
	// 长连接会让虚拟时间一直暂停，预算可能永远耗不完（整套流程就卡死了）。
	// 结果通道改成"页面自己 POST 回 mock"（也就是本仓库惯用的那一套），
	// 拿到结果之后由我们结束 Chrome。
	//
	// Wait 只在这一条 goroutine 里调一次：它返回有两个用处 —— ① "Chrome 自己退了"
	// 变成一个可观测事件；② 它返回之后 out 才是静止的（SyncBuffer 已经保证读它
	// 不竞争，能等到静止更好，失败信息里的尾部才是完整的）。
	exit := watchExit(cmd)

	killed := false
	defer func() {
		if !killed {
			KillChrome(cmd)
		}
		exit.wait(t, cmd, &out)
	}()

	var res <-chan []byte
	var live *Liveness
	if src != nil {
		res = src.Results()
		live = src.Live()
	}
	start := time.Now()
	watch := WatchFor(timeout)
	chromeNote := "仍在运行"
	chromeGone := ""
	exitCh := exit.done
	ticker := time.NewTicker(watchPoll)
	defer ticker.Stop()
	hard := time.NewTimer(timeout)
	defer hard.Stop()

	for {
		select {
		case raw := <-res:
			killed = true
			KillChrome(cmd)
			exit.wait(t, cmd, &out)
			return raw

		case <-exitCh:
			// Chrome 退了但结果没到。**不立刻判死**：Windows 上 chrome.exe 有时
			// 会"起一个自己再退"（启动器交接），那时这一条立刻就到而页面一切正常。
			// 把它记成现场，交给看门狗按"没有新信号"的阈值判。
			// 这一支只处理一次（下面的 exitCh 置 nil，别让它把 select 占住）。
			exitCh = nil
			chromeNote = "已经退出"
			if err := exit.err0(); err != nil {
				chromeNote = "已经退出（" + err.Error() + "）"
			}
			chromeGone = chromeNote

		case <-ticker.C:
			if live == nil {
				continue
			}
			now := time.Now()
			snap := live.Snapshot(start)
			kind, why := snap.Diagnose(now, watch, chromeGone)
			if kind == "" {
				continue
			}
			killed = true
			KillChrome(cmd)
			exit.wait(t, cmd, &out)
			t.Fatalf("%s", failText("浏览器这一趟已经死了（只等了 "+shortDur(now.Sub(start))+
				"，外层上限 "+timeout.String()+"）", why, snap, now, chromeNote, out.String()))
			return nil

		case <-hard.C:
			now := time.Now()
			killed = true
			KillChrome(cmd)
			exit.wait(t, cmd, &out)
			var why string
			var snap Snapshot
			if live != nil {
				snap = live.Snapshot(start)
				kind, w := snap.Diagnose(now, watch, chromeGone)
				if kind == "" {
					// 没有"它死了"的证据 ⇒ 如实说"看不出来"，并把页面自报的
					// 最后一步贴出来（不再是一句"等回传超时"就完了）。
					why = "没有证据说明它死了（心跳一直在，页面看起来还活着）"
				} else {
					why = w
				}
			}
			t.Fatalf("%s", failText(fmt.Sprintf("等浏览器回传结果超时（%s）", timeout),
				why, snap, now, chromeNote, out.String()))
			return nil
		}
	}
}

// failText 把一次失败排成"一句话结论 + mock 侧现场 + Chrome 尾部"。
//
// 失败信息是这一整套存活信号的**唯一**出口：判据、断言、用例名都不受影响，
// 变的只是"红的时候日志里有什么"。
func failText(head, why string, snap Snapshot, now time.Time, chromeNote, chromeOut string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s。\n页面死在哪一步：%s\n", head, why)
	if snap.Start.IsZero() {
		b.WriteString("（这一趟没有存活信号：mock 层没接账本）\n")
	} else {
		fmt.Fprintf(&b, "页面最后一步：%s\n", snap.LastStep())
		b.WriteString(snap.Scene(now))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Chrome 进程：%s\n", chromeNote)
	fmt.Fprintf(&b, "Chrome 输出尾部：\n%s", Tail(chromeOut, 1500))
	return b.String()
}

// exitWatch 记 Chrome 的退出。Wait **只调一次**，结果缓起来：成功分支与 defer
// 都会来收，谁先到谁等，第二个立刻拿到（旧写法里那第二次收会一直等到超时 ——
// 一次 tz 用例因此白等 15 秒）。
type exitWatch struct {
	done chan struct{}
	err  error
}

func watchExit(cmd *exec.Cmd) *exitWatch {
	w := &exitWatch{done: make(chan struct{})}
	go func() {
		w.err = cmd.Wait() // 写在 close 之前：收的一方靠 close 建立先后关系
		close(w.done)
	}()
	return w
}

// err0 只在 done 关闭之后读（那时 err 的写对读方可见）。
func (w *exitWatch) err0() error { return w.err }

// wait 有上限地等 Wait 返回：等不到也往下走（进程还没吐 EOF 不影响判据，
// SyncBuffer 也保证读尾部不竞争）。
func (w *exitWatch) wait(t *testing.T, cmd *exec.Cmd, out *SyncBuffer) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(waitGrace):
		t.Logf("Chrome（pid %d）退出后 %s 内 Wait 没返回，继续往下走。输出尾部：\n%s",
			cmd.Process.Pid, waitGrace, Tail(out.String(), 400))
	}
}

// KillChrome 结束 Chrome。Windows 上只杀主进程会留下渲染/GPU 子进程攥着画像目录，
// 所以优先用 taskkill /T 整棵树一起收。
func KillChrome(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if cmd.Process.Pid > 0 {
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}

func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("…%s", s[len(s)-n:])
}
