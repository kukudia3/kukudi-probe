package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// 这一份文件是"外层看门狗"的观测面：**mock 侧**能看见的东西，与页面回传的结果无关。
//
// 背景（为什么需要它）：RunChromeForResult 过去只等一个事件 —— 页面 POST /__result。
// 对"页面还活着吗 / 走到哪一步了"零观测，于是下面三种死法在日志里长得一模一样：
//
//	① 页面根本没跑起来（未决的样式表会让注入的内联脚本一行都不执行）；
//	② 页面跑起来了但 JS 卡住 / 渲染进程崩了；
//	③ 页面走到了最后一步、回传那一笔没到。
//
// 三种都是"干等到外层上限（90~300 秒），失败信息里连步骤都没有"。这里补上两个
// **页面之外**的信号，让上面三种能被分开：
//
//	· 请求账本：浏览器打到 mock 的每一个请求（到达时刻 / 路径 / 状态码 / 耗时）；
//	· 解析期心跳：注入脚本**在解析期就装好**的一个 2 秒定时器（POST /__alive），
//	  每次都把"这一趟走到哪一步、哪些请求还在飞"带回来。
//
// 心跳为什么能区分 ① 与 ②：它就是被注入的那一段内联脚本自己发的 ——
// 脚本压根没执行 ⇒ 心跳一帧都没有；脚本执行了但卡住 ⇒ 心跳一直在，只是停在
// 某一步上。**两个信号都只记事实、不参与任何断言**，也不改页面的行为。

// AlivePath 是心跳的落点。它由 mock 自己消费，**不转发**给真服务端
// （真服务端看不到这些请求，任何"请求计数 / 审计"类断言都不受影响）。
//
// 导出的原因：不走 Proxy、自己实现 ServeHTTP 的 mock 层（见 flows 包的
// longFlowProxy）也要认得它，否则心跳会被转发到真服务端上去。
const AlivePath = "/__alive"

const alivePath = AlivePath

// aliveMarker 是心跳前导脚本里的一个记号：Proxy 靠它在**自己发出去的那份 HTML**
// 里判断"这一页到底装没装心跳"。没装的页面不会被判"脚本没执行"（宁可少报，不误报）。
const aliveMarker = "__PROBE_LIVE"

// alivePreambleJS 由 InjectHarness 插在自检脚本**之前**（自检脚本里那句
// `window.fetch.bind(window)` 因此拿到的是下面这层计数器，计数不漏）。
//
// 它只做三件事，任何一件都不改变页面的取数行为：
//  1. 包一层 window.fetch：**只计数**在飞的请求，返回值 / 异常原样透出去；
//  2. 立刻发一次心跳（证明"被注入的这段内联脚本确实执行到了"）；
//  3. 之后每 2 秒再发一次（带步骤名、在飞请求、readyState、可见性）。
//
// 截图模式（CFG.shot）只发第 2 步那一次：那一路带 --virtual-time-budget，
// 虚拟时间会把定时器快进，周期性心跳会变成空转（见 chrome.go 顶部那段说明）。
const alivePreambleJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var origFetch = window.fetch.bind(window);
  var t0 = Date.now();
  var inflight = [];
  window.` + aliveMarker + ` = { beats: 0, inflight: inflight };
  window.fetch = function (input, init) {
    var rec = { u: urlOf(input), at: Date.now(), done: false };
    inflight.push(rec);
    trim();
    var p;
    try {
      p = origFetch(input, init);
    } catch (e) {
      rec.done = true;
      throw e;
    }
    if (p && typeof p.then === 'function') {
      p.then(function () { rec.done = true; }, function () { rec.done = true; });
    }
    return p;
  };
  function urlOf(input) {
    if (typeof input === 'string') { return input; }
    if (input && input.url) { return String(input.url); }
    return String(input);
  }
  // 在飞的请求最多留 12 条：先把已经收尾的挤掉，一条都没收尾时挤最老的。
  // 卡住的那一笔因此不会被后来的请求挤出视野（它正是要看的东西）。
  function trim() {
    while (inflight.length > 12) {
      var idx = -1;
      for (var i = 0; i < inflight.length; i++) {
        if (inflight[i].done) { idx = i; break; }
      }
      inflight.splice(idx < 0 ? 0 : idx, 1);
    }
  }
  function beat() {
    var R = window.__TZRESULT || {};
    var steps = R.steps || [];
    var body = {
      t: Date.now() - t0,
      ready: document.readyState,
      vis: document.visibilityState,
      shot: !!CFG.shot,
      steps: steps.length,
      step: steps.length ? String(steps[steps.length - 1]).slice(0, 140) : '',
      fatal: String(R.fatal || '').slice(0, 200),
      errs: (R.errs || []).length,
      inf: []
    };
    for (var i = 0; i < inflight.length; i++) {
      if (inflight[i].done) { continue; }
      body.inf.push({ u: String(inflight[i].u).slice(0, 90), ms: Date.now() - inflight[i].at });
    }
    window.` + aliveMarker + `.beats++;
    try {
      var p = origFetch('` + alivePath + `', {
        method: 'POST', cache: 'no-store', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
      });
      if (p && p.catch) { p.catch(function () {}); }
    } catch (e) {}
  }
  beat();
  if (!CFG.shot) { setInterval(beat, 2000); }
})();`

// InflightReq 是页面心跳自报的一笔"还在飞"的请求。
type InflightReq struct {
	URL string `json:"u"`
	Ms  int64  `json:"ms"`
}

// Beat 是注入脚本发回来的一帧现场（原样解析，不做任何判断）。
type Beat struct {
	At        time.Time
	ElapsedMs int64         `json:"t"`
	Ready     string        `json:"ready"`
	Vis       string        `json:"vis"`
	Shot      bool          `json:"shot"`
	Steps     int           `json:"steps"`
	Step      string        `json:"step"`
	Fatal     string        `json:"fatal"`
	Errs      int           `json:"errs"`
	Inflight  []InflightReq `json:"inf"`
}

// LiveNote 是账本里的一笔请求。
//
// Status 为 0 表示"还没收尾"：要么服务端 handler 还在写（正常情况下毫秒级），
// 要么它是一条 SSE 长连接（会一直挂着 —— 这本身是有用的现场）。
type LiveNote struct {
	Method string
	Path   string
	At     time.Time
	Status int
	Dur    time.Duration
	Kind   string // page（首页）/ beat（心跳）/ result（回传结果）/ api（其余）
}

// String 把这一笔排成"这一趟的第几秒、什么结果"。
func (n LiveNote) String(start time.Time) string {
	status := "还没收尾"
	if n.Status != 0 {
		status = fmt.Sprintf("%d", n.Status)
	}
	return fmt.Sprintf("%s %s → %s（%.1fs 时到达，耗时 %s）", n.Method, n.Path, status,
		n.At.Sub(start).Seconds(), durText(n.Dur))
}

// Liveness 是"这一趟浏览器干了什么"的账本。全部字段由互斥量保护：HTTP handler
// 在自己的 goroutine 里写，看门狗在测试 goroutine 里读（-race 下就是靠这里过的）。
type Liveness struct {
	mu       sync.Mutex
	notes    []*LiveNote
	dropped  int
	first    time.Time
	last     *LiveNote
	lastReal *LiveNote
	resultAt time.Time
	beats    int
	beatSeen bool
	lastBeat Beat
	counts   map[string]int
	// expectBeats：这一页装没装心跳脚本。只有装了，"一帧心跳都没有"才能被
	// 解释成"注入脚本没执行"（宁可少报，不误报）。
	expectBeats bool
}

// NewLiveness 建一份账本。不走 Proxy 的 mock handler（例如 flows 包自己那层）
// 也能用它接上存活信号。
func NewLiveness() *Liveness {
	return &Liveness{counts: map[string]int{}}
}

// ExpectBeats 声明"这一页装了心跳脚本"（Proxy 从自己发出去的 HTML 里认出来）。
func (l *Liveness) ExpectBeats(v bool) {
	l.mu.Lock()
	l.expectBeats = v
	l.mu.Unlock()
}

const liveNoteCap = 600

// Note 记下"浏览器打过来一个请求"，返回的收尾函数用来补上状态码（不调也能出报告）。
func (l *Liveness) Note(method, path string) func(status int) {
	now := time.Now()
	n := &LiveNote{Method: method, Path: path, At: now, Kind: kindOf(path)}
	l.mu.Lock()
	if l.first.IsZero() {
		l.first = now
	}
	l.last = n
	if n.Kind != "beat" {
		l.lastReal = n
	}
	l.counts[method+" "+path]++
	if path == "/__result" {
		l.resultAt = now
	}
	l.notes = append(l.notes, n)
	if len(l.notes) > liveNoteCap {
		l.notes = l.notes[1:]
		l.dropped++
	}
	l.mu.Unlock()
	return func(status int) {
		l.mu.Lock()
		n.Status = status
		n.Dur = time.Since(n.At)
		l.mu.Unlock()
	}
}

func kindOf(path string) string {
	switch path {
	case alivePath:
		return "beat"
	case "/__result":
		return "result"
	case "/", "/index.html":
		return "page"
	}
	return "api"
}

// ServeAlive 是心跳的落点：解析页面自报的那一帧，记进账本，立刻回 204。
//
// 它**绝不转发**（真服务端不该看见这些请求），也**绝不因为解析失败而报错**
// （心跳只是诊断，不能反过来影响页面）。
func (l *Liveness) ServeAlive(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	b := Beat{At: time.Now()}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &b); err != nil {
			// 解析不了也留一帧：至少"心跳到了"这件事被记下来。
			b = Beat{At: time.Now(), Step: strings.TrimSpace(Tail(string(body), 120))}
		}
	}
	l.mu.Lock()
	l.beats++
	l.beatSeen = true
	l.lastBeat = b
	l.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// Observe 记一笔"浏览器打过来的请求"，返回一个原样透传的 ResponseWriter
// （只为在收尾时补上状态码）。Flush 与 Unwrap 都透传出去 —— SSE 的逐帧下灌
// 与 ReverseProxy 拿 Flusher 的路子都不能被这层壳挡住。
func Observe(l *Liveness, w http.ResponseWriter, r *http.Request) *NoteWriter {
	if l == nil {
		return &NoteWriter{ResponseWriter: w}
	}
	return &NoteWriter{ResponseWriter: w, finish: l.Note(r.Method, r.URL.Path)}
}

// NoteWriter 是 Observe 的产物：记一笔请求 + 原样透传。handler 里
// `nw := Observe(...); defer nw.Finish()` 就行。
type NoteWriter struct {
	http.ResponseWriter
	status int
	finish func(int)
	done   bool
}

func (w *NoteWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *NoteWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(p)
}

func (w *NoteWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.NewResponseController 还能找到底下那个 ResponseWriter
// （ReverseProxy 靠它拿 Flusher；少了这个，SSE 会被缓冲成一坨）。
func (w *NoteWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Finish 把状态码补进账本。handler 里 defer 一下就行。
func (w *NoteWriter) Finish() {
	if w.done || w.finish == nil {
		return
	}
	w.done = true
	if w.status == 0 {
		w.status = 200
	}
	w.finish(w.status)
}

// Snapshot 是账本的只读快照（看门狗读它，不做任何阻塞操作）。
type Snapshot struct {
	// Start 是"这一趟等待"的起点（看门狗开始等的时刻），报告里的秒数都以它为基准。
	Start       time.Time
	ReqCount    int
	FirstReqAt  time.Time
	LastReq     *LiveNote
	LastReal    *LiveNote
	ResultSeen  bool
	Beats       int
	BeatSeen    bool
	LastBeat    Beat
	Paths       []PathStat
	Dropped     int
	ExpectBeats bool
}

// PathStat 是按路径聚合的一行（报告里最多列十几行）。
type PathStat struct {
	Key   string
	Count int
}

// Snapshot 取一份快照。start 是"这一趟等待"的起点。ExpectBeats 取自账本自己
// （Proxy 建账本时从它发出去的那份 HTML 里认出来的）。
func (l *Liveness) Snapshot(start time.Time) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Snapshot{
		Start:       start,
		ReqCount:    len(l.notes) + l.dropped,
		FirstReqAt:  l.first,
		LastReq:     l.last,
		LastReal:    l.lastReal,
		ResultSeen:  !l.resultAt.IsZero(),
		Beats:       l.beats,
		BeatSeen:    l.beatSeen,
		LastBeat:    l.lastBeat,
		Dropped:     l.dropped,
		ExpectBeats: l.expectBeats,
	}
	for k, v := range l.counts {
		s.Paths = append(s.Paths, PathStat{Key: k, Count: v})
	}
	sort.Slice(s.Paths, func(i, j int) bool {
		if s.Paths[i].Count != s.Paths[j].Count {
			return s.Paths[i].Count > s.Paths[j].Count
		}
		return s.Paths[i].Key < s.Paths[j].Key
	})
	if len(s.Paths) > 14 {
		s.Paths = s.Paths[:14]
	}
	return s
}

// Watch 是看门狗的判定阈值。前三个都是"**没有**信号"的容忍时长，不是"等了多久"：
// 只有拿到"页面已经死了"的正证据才会提前判死，看不出证据就继续等（到外层上限）。
type Watch struct {
	// FirstReq 内一个请求都没到 ⇒ 页面没起来（Chrome 连不上 mock / 根本没导航）。
	FirstReq time.Duration
	// ScriptSilent：首个请求之后这么久还没有第一帧心跳 ⇒ 注入脚本没执行
	// （未决的样式表会把后面的内联脚本全部挡住，这是页面侧修不了的那一支）。
	ScriptSilent time.Duration
	// BeatGap：心跳断了这么久 ⇒ 页面 JS 不再运行（渲染进程崩了 / 卡死）。
	BeatGap time.Duration
	// BeatGapHidden 是页面自报"在后台"时用的阈值：后台标签的定时器会被 Chrome
	// 节流（跨标签页那条用例真的会把 A 标签放到后台），同一个阈值会把
	// "活着但被节流"误判成死。
	BeatGapHidden time.Duration
	// ChromeExitSilence：Chrome 进程退出之后，还要这么久没有任何请求/心跳，
	// 才算"它崩了"。为什么要这一条：Windows 上 chrome.exe 有时会"起一个自己
	// 再退"（启动器交接），只看"退了"会把正常的页面判死。
	ChromeExitSilence time.Duration
}

// WatchFor 按外层上限推阈值：上限越小阈值越小（但都有下限：Chrome 在满载的
// CI 上起得慢，阈值太小会把"慢"当成"死"）。
func WatchFor(timeout time.Duration) Watch {
	grace := timeout / 3
	if grace < 20*time.Second {
		grace = 20 * time.Second
	}
	if grace > 45*time.Second {
		grace = 45 * time.Second
	}
	return Watch{
		FirstReq:          grace,
		ScriptSilent:      grace,
		BeatGap:           25 * time.Second,
		BeatGapHidden:     75 * time.Second,
		ChromeExitSilence: 10 * time.Second,
	}
}

// 死法（判定结果）。空串表示"没有证据说明页面死了" ⇒ 继续等。
const (
	DeadNoPage     = "no-page"     // 一个请求都没到
	DeadNoScript   = "no-script"   // 请求到了，但注入脚本一行都没执行
	DeadJS         = "js-dead"     // 心跳断了：页面 JS 不再运行
	DeadChromeExit = "chrome-exit" // Chrome 进程自己退了
)

// Diagnose 是看门狗的核心：拿账本快照判"页面死在哪一步"。
//
// 只有拿到正证据才算死（见 Watch 的注释）；看不出证据时返回空串 ⇒ 继续等。
// chromeGone 非空表示 Chrome 进程已经退出（内容是退出原因）。
func (s Snapshot) Diagnose(now time.Time, w Watch, chromeGone string) (kind, why string) {
	if s.ResultSeen {
		return "", "" // 结果那一笔已经到了 mock（只是还没交给测试）：页面不是死
	}
	// 最后一次"页面还活着"的信号：请求或心跳，谁晚算谁。
	lastSignal := s.Start
	if s.LastReq != nil && s.LastReq.At.After(lastSignal) {
		lastSignal = s.LastReq.At
	}
	if s.BeatSeen && s.LastBeat.At.After(lastSignal) {
		lastSignal = s.LastBeat.At
	}
	switch {
	case s.ReqCount == 0:
		// 一个请求都没到：要么 Chrome 根本没起来，要么起来就退了。
		if chromeGone != "" && now.Sub(s.Start) >= w.ChromeExitSilence {
			return DeadNoPage, fmt.Sprintf("Chrome 进程%s，页面一个请求都没打过来", chromeGone)
		}
		if now.Sub(s.Start) >= w.FirstReq {
			return DeadNoPage, fmt.Sprintf("页面一个请求都没打过来（等了 %s，Chrome 起来了但没导航 / 连不上 mock）",
				shortDur(now.Sub(s.Start)))
		}
	case chromeGone != "" && now.Sub(lastSignal) >= w.ChromeExitSilence:
		// Chrome 退出**并且**之后再没有任何信号，才算"崩了"。少了后半条，
		// Windows 上 chrome.exe"起一个自己再退"（启动器交接）会被误判成崩溃 ——
		// 那种情况下页面照样在发请求/心跳。
		return DeadChromeExit, fmt.Sprintf("Chrome 进程%s，之后 %s 再没有任何请求或心跳（页面已经不可能再回传结果）",
			chromeGone, shortDur(now.Sub(lastSignal)))
	case s.ExpectBeats && !s.BeatSeen:
		if now.Sub(s.FirstReqAt) >= w.ScriptSilent {
			return DeadNoScript, fmt.Sprintf(
				"页面加载了（首个请求在 %.1fs 时到达）但注入脚本的解析期心跳一帧都没到（等了 %s）—— 脚本没有执行：未决的样式表会挡住它后面的全部内联脚本",
				s.FirstReqAt.Sub(s.Start).Seconds(), shortDur(now.Sub(s.FirstReqAt)))
		}
	case s.BeatSeen && !s.LastBeat.Shot:
		gap := now.Sub(s.LastBeat.At)
		limit, why2 := w.BeatGap, ""
		if s.LastBeat.Vis == "hidden" {
			limit, why2 = w.BeatGapHidden, "（页面自报在后台，用的是放宽后的阈值）"
		}
		if gap >= limit {
			return DeadJS, fmt.Sprintf("页面心跳断了 %s：页面 JS 不再运行（渲染进程崩了或卡死）%s；最后一帧现场见下",
				shortDur(gap), why2)
		}
	}
	return "", ""
}

// LastStep 说"页面最后走到哪一步"：优先用页面自报的心跳，其次用"回传那一笔有没有到"。
func (s Snapshot) LastStep() string {
	if s.BeatSeen {
		for _, in := range s.LastBeat.Inflight {
			if strings.Contains(in.URL, "/__result") {
				return fmt.Sprintf("已经发出「回传结果」那一笔（在飞 %.1fs）—— 卡在最后一步回传",
					float64(in.Ms)/1000)
			}
		}
		if s.LastBeat.Step != "" {
			return fmt.Sprintf("停在步骤「%s」", s.LastBeat.Step)
		}
		if s.LastBeat.Steps == 0 {
			return "还没走到第一步（页面刚起跑）"
		}
	}
	if s.LastReal != nil && s.LastReal.Kind == "result" {
		return "回传结果那一笔到了 mock（结果没被取走）"
	}
	return "页面侧没有报出步骤"
}

// LiveScene 给"自己起 Chrome、自己等结果"的用例用（例如 ui 包那条走 CDP 的窄屏探测）：
// 把这一趟 mock 侧的现场排成几行，塞进它们自己的超时信息里。
//
// 它**不判死、也不改上限**：那些用例没有外层的终止逻辑，这里只把"页面死在哪一步"
// 写出来。返回空串表示这一趟没有存活信号（那就什么也不加，保持原样）。
func LiveScene(src ResultSource, start time.Time) string {
	if src == nil {
		return ""
	}
	live := src.Live()
	if live == nil {
		return ""
	}
	now := time.Now()
	snap := live.Snapshot(start)
	return fmt.Sprintf("页面死在哪一步：%s\n页面最后一步：%s\n%s",
		verdictOrUnknown(snap, now), snap.LastStep(), snap.Scene(now))
}

// verdictOrUnknown 是"没有判死证据"时的那句话（与 chrome.go 到点分支同一口径）。
func verdictOrUnknown(s Snapshot, now time.Time) string {
	if kind, why := s.Diagnose(now, WatchFor(24*time.Hour), ""); kind != "" {
		return why
	}
	return "没有证据说明它死了（心跳一直在，页面看起来还活着）"
}

// Scene 把"这一趟 mock 侧看到的东西"排成人读的几行。它只在失败信息里出现。
func (s Snapshot) Scene(now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "这一趟 mock 侧看到：请求 %d 笔", s.ReqCount)
	if !s.FirstReqAt.IsZero() {
		fmt.Fprintf(&b, "（首个 %.1fs 时到达", s.FirstReqAt.Sub(s.Start).Seconds())
		if s.LastReq != nil {
			fmt.Fprintf(&b, "，最后一笔 %.1fs 前", now.Sub(s.LastReq.At).Seconds())
		}
		b.WriteString("）")
	}
	fmt.Fprintf(&b, "；心跳 %d 次", s.Beats)
	if s.BeatSeen {
		fmt.Fprintf(&b, "（最后一帧 %.1fs 前）", now.Sub(s.LastBeat.At).Seconds())
	}
	b.WriteString("\n")
	if s.BeatSeen {
		beat := s.LastBeat
		fmt.Fprintf(&b, "最后一帧心跳：ready=%s vis=%s 步骤数=%d fatal=%q errs=%d%s\n",
			orQ(beat.Ready), orQ(beat.Vis), beat.Steps, beat.Fatal, beat.Errs, shotNote(beat))
		for _, in := range beat.Inflight {
			fmt.Fprintf(&b, "  · 页面自报还在飞：%s（已 %.1fs）\n", in.URL, float64(in.Ms)/1000)
		}
	} else if s.ExpectBeats {
		b.WriteString("心跳一帧都没到：注入脚本在解析期就没有执行（未决的样式表会挡住它）\n")
	}
	if s.LastReal != nil {
		fmt.Fprintf(&b, "最后一笔实际请求：%s\n", s.LastReal.String(s.Start))
	}
	if len(s.Paths) > 0 {
		var parts []string
		for _, p := range s.Paths {
			parts = append(parts, fmt.Sprintf("%s×%d", p.Key, p.Count))
		}
		fmt.Fprintf(&b, "请求明细（按路径聚合）：%s\n", strings.Join(parts, "、"))
	}
	if s.Dropped > 0 {
		fmt.Fprintf(&b, "（更早的 %d 笔请求已省略）\n", s.Dropped)
	}
	return strings.TrimRight(b.String(), "\n")
}

func shotNote(b Beat) string {
	if b.Shot {
		return "（截图模式：只发一帧心跳）"
	}
	return ""
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%.1fm", d.Minutes())
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
}

func durText(d time.Duration) string {
	if d <= 0 {
		return "还在飞"
	}
	return shortDur(d)
}
