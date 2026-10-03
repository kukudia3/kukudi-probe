package e2e

// 浏览器**长流程**验证：第二轮审计 §7 里那批"要在真浏览器里走完整条路"的缺口
// （逐条编号见 _audit/ROUND4-GAPS.md 的 §1 分类表）。本文件覆盖 8 条：
//
//	06-2  `F2` 实际暴露时长（延迟注入：让 /nodes 响应晚于登出）
//	06-3  `F1` 端到端复现：登出后明文密码是否留在 DOM（真登录 + 设置页 + 登出）
//	06-4  `F3` 端到端复现：详情页子请求迟到（延迟注入）
//	06-5  `F4` node-* 表单残留的浏览器实测
//	07-1  `发现 1` 修复后的副作用面：反代对 SSE 返 5xx 时前端行为
//	07-2  `发现 2` 的竞态窗口（与 06-2 同一装置）
//	07-3  附注的最终视觉：首页登出后是不是空白（另有一张 PROBE_SHOT_DIR 截图）
//	07-5  访客后台标签页 SSE 是否仍在收
//
// 只加测试：产品代码一行未改（`web/` 与 `internal/**` 的非 `_test.go` 文件逐字节未动）。
// 反向验证（把被测行为用**测试侧**手段改坏 → 用例必须红）逐条记在
// D:\DEEPSEEK\_audit\ROUND4-GAPS-BROWSER.md，变异全部发生在 %TEMP% 的仓库副本里。
//
// 装置与仓库既有那套完全一致（见 streamidentity_browser_test.go 的文件头）：
// 真服务端随机端口 + 反代夹在中间（注入自检脚本、改写响应）+ 结果 POST 回 mock
// + `--headless=new`，**不用** `--dump-dom --virtual-time-budget`（SSE 会让虚拟时间停住）。
//
// 本文件在既有装置上加的两样东西（都只存在于测试侧）：
//
//  1. **延迟注入**：反代的 ModifyResponse 能按"路径 + 方法 + 查询串"把**已经取到的**
//     响应扣住不放，等页面把登出走完再放行。必须是"先取到、后扣住"——先扣请求的话
//     响应回来时会话已经失效，服务端给的是 401，测的就不是"迟到响应写回 DOM"了。
//  2. **世代观测钩子**：包一层 `window.ProbeChart.create`，记下每一次 `setData` 的
//     点数与时刻（详情页迟到的那一半只能从"画布又被写了一次"看出来）。
//     ⚠️ 包装 `window.EventSource` 时三个常量（CONNECTING/OPEN/CLOSED）必须一起搬过去
//     —— app.js 用 `EventSource.CLOSED` 判永久失败，少一个就悄悄改掉被测分支。
//
// 每条用例都先自证"现场不空"（被扣住的响应体里真的有私有字段、登出前表单里真的有值、
// 登出前图表真的画过点），否则"登出后没有 X"这种断言在任何实现下都成立。

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// ---------------------------------------------------------------- 现场里的私有值

// longFlowValues 是"登出之后一个都不许留在页面上"的那批值。
//
// 为什么不用真实部署里必然出现的 127.0.0.1：回环地址与"服务端监听地址"是同一个串，
// 断言分不清是"节点地址漏了"还是"本来就该显示的监听地址"。这里特意用文档地址段
// （RFC 5737 的 203.0.113.0/24、198.51.100.0/24 与 ULA fd00::/8），漏没漏一目了然。
type longFlowValues struct {
	ObservedIP string // 服务端看到的来源地址（observed_ip）
	LocalIP    string // Agent 自报的本机地址（local_ip）
	LocalIP6   string
	Note       string // #node-note 的内容（服务端把 note 列为访客私有）
	NoteIP     string // 备注里那一小段（页面文本里搜这个）
	PingHost   string // 探测目标的地址（label 留空 ⇒ 存储层用 host 兜底成 label）
	NodeName   string
}

func defaultLongFlowValues() longFlowValues {
	return longFlowValues{
		ObservedIP: "203.0.113.9",
		LocalIP:    "10.0.0.5",
		LocalIP6:   "fd00::5",
		Note:       "root@203.0.113.7:22 备用入口",
		NoteIP:     "203.0.113.7",
		PingHost:   "198.51.100.77",
		NodeName:   "longflow-01",
	}
}

// all 是"页面上一个都不许出现"的全集（不含 pw 那几个：那是输入框的 value，
// textContent 看不见，由表单快照单独断言）。
func (v longFlowValues) all() []string {
	return []string{v.ObservedIP, v.LocalIP, v.LocalIP6, v.NoteIP, v.PingHost}
}

// ---------------------------------------------------------------- 现场

type longFlowFixture struct {
	h      *harness
	br     *browser
	nodeID int64
	token  string
	vals   longFlowValues
}

// startLongFlowFixture 起一个真服务端 + 一台带备注的节点 + 一份地址现场。
//
//	withGuest 打开「允许访客查看」（登出后浏览器留在只读面板上，正是审计描述的形态）
//	withAgent 起真 Agent（只有 07-5 需要"帧真的在推"；其余用例用 State().Attach 种的
//	          地址更可控 —— 真实 Agent 的 local_ip / observed_ip 都是 127.0.0.1）
//	withPing  配一个**名字留空**的探测目标（地址由 host 兜底成 label，见 guest.go）
func startLongFlowFixture(t *testing.T, withGuest, withAgent, withPing bool) *longFlowFixture {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	vals := defaultLongFlowValues()
	status, body := br.do(http.MethodPost, "/api/v1/nodes", map[string]any{
		"name": vals.NodeName, "group_name": "测试", "region": "HK", "interval_sec": 1,
		"note": vals.Note,
	}, true)
	if status != http.StatusCreated {
		t.Fatalf("建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id, _ := node["id"].(float64)
	token, _ := body["token"].(string)
	if id <= 0 || token == "" {
		t.Fatalf("建节点没有返回 id/token: %v", body)
	}
	f := &longFlowFixture{h: h, br: br, nodeID: int64(id), token: token, vals: vals}

	if withGuest {
		if status, body := br.do(http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": true}, true); status != http.StatusOK {
			t.Fatalf("打开访客开关失败: %d %v", status, body)
		}
	}
	if withPing {
		if status, body := br.do(http.MethodPut, "/api/v1/settings/ping", map[string]any{
			"interval_sec": 60,
			"targets": []map[string]any{
				// label 留空：存储层用 host 兜底 ⇒ label 的值就是地址（审计里那条路径）
				{"label": "", "type": "tcp", "host": vals.PingHost, "port": 443, "enabled": true},
			},
		}, true); status != http.StatusOK {
			t.Fatalf("配探测目标失败: %d %v", status, body)
		}
	}
	if withAgent {
		client, _ := newClient(t, "http://"+h.addr, token)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() { _ = client.Run(ctx) }()
		waitFor(t, 15*time.Second, "Agent 上报两拍", func() bool {
			n, ok := h.srv.State().Get(f.nodeID)
			return ok && n.Seq >= 2
		})
	} else {
		seedLongFlowState(t, h, f.nodeID, vals)
	}
	if !withAgent {
		seedLongFlowSamples(t, h, f.nodeID)
	}
	return f
}

// seedLongFlowState 把三个地址塞进内存状态（真部署里由 Agent 的 hello 带上来）。
//
// 为什么必须塞：不塞的话 /nodes 响应里根本没有那两个字段，"登出后页面上没有它们"
// 这条断言在任何实现下都成立 —— 那就成了一条空断言。
func seedLongFlowState(t *testing.T, h *harness, nodeID int64, vals longFlowValues) {
	t.Helper()
	h.srv.State().Attach(nodeID, 1, protocol.Info{
		OS:           protocol.OSInfo{Name: "Debian", Kernel: "6.1.0"},
		CPU:          protocol.CPUInfo{Model: "EPYC", Cores: 2},
		AgentVersion: "1.4.2",
	}, vals.ObservedIP, vals.LocalIP, vals.LocalIP6, time.Now())
	h.srv.State().Update(nodeID, 1, protocol.Metrics{
		CPUPct: 12.5, Mem: protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Net:       protocol.Net{Iface: "eth0", BootID: "boot-longflow", RxRate: 1024, TxRate: 2048},
		UptimeSec: 3600,
	}, 0, time.Now())
}

// seedLongFlowSamples 播一段 10 秒桶：06-4 要证明"图里真的有点"，
// 否则"登出之后没有再 setData 写点"就分不清是"被守卫挡住了"还是"本来就没数据"。
func seedLongFlowSamples(t *testing.T, h *harness, nodeID int64) {
	t.Helper()
	end := time.Now().Add(-30 * time.Second).Truncate(10 * time.Second)
	start := end.Add(-20 * time.Minute)
	var buckets []store.SampleBucket
	for ts := start; !ts.After(end); ts = ts.Add(10 * time.Second) {
		i := len(buckets)
		buckets = append(buckets, store.SampleBucket{
			NodeID: nodeID, TS: ts.Unix(),
			CPUAvg: float64(10 + i%20), CPUMax: float64(12 + i%20),
			MemAvg: 30, MemMax: 33, DiskAvg: 40, DiskMax: 41,
			RxRate: 1024 + float64(i), TxRate: 2048 + float64(i), Up: 10, All: 10,
		})
	}
	if len(buckets) < 30 {
		t.Fatalf("播撒的桶只有 %d 个，太少", len(buckets))
	}
	if err := h.db.InsertBuckets(context.Background(), store.TableSamples10s, buckets); err != nil {
		t.Fatalf("写入 10 秒桶: %v", err)
	}
	// /traffic 要的是日流量（7 天）：没有它，_traffic 那张图的 setData 会是空点数组。
	today := time.Now().UTC()
	for i := 6; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		if _, err := h.db.Writer().ExecContext(context.Background(),
			`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)`,
			nodeID, store.FormatDay(d), int64(7-i)*1_000_000_000, int64(14-i)*1_000_000_000); err != nil {
			t.Fatalf("写入日流量: %v", err)
		}
	}
}

// ---------------------------------------------------------------- 反代：延迟注入 + SSE 5xx

// holdRule 是一条"扣住不放"的规则。query 非空时还要匹配查询串（用来只扣 /series 里
// 的某一个指标 —— 全扣的话 Chrome 每个源 6 条连接会被占满，登出那一枪就发不出去了）。
type holdRule struct {
	method string
	path   string
	query  string
}

type longFlowMark struct {
	T        int            `json:"t"`
	Note     string         `json:"note"`
	OK       bool           `json:"ok"`
	Parked   map[string]int `json:"parked"`
	HadValue map[string]int `json:"hadValue"`
	// 下面三个是反代在**这一刻**的累计计数（Go 侧拿它算差值，比"页面自己数"更可信）。
	Session  int `json:"session"`
	Stream   int `json:"stream"`
	Settings int `json:"settings"`
}

type longFlowProxy struct {
	page   []byte
	result chan []byte
	rp     *httputil.ReverseProxy
	once   sync.Once

	mu        sync.Mutex
	armed     bool
	rules     []holdRule
	waits     map[string]map[string]int // mark 名 → （路径 → 需要几条）
	parked    map[string]int            // 路径 → 已扣下的条数
	hadValue  map[string]int            // 路径 → 扣下的响应体里带私有值的条数
	flags     map[string]map[string]int // 路径 → 响应体形态标记 → 条数
	markSeen  map[string]longFlowMark
	released  bool
	releaseCh chan struct{}

	streamAttempts atomic.Int32
	sessionHits    atomic.Int32
	settingsHits   atomic.Int32
	blockStream    atomic.Bool
}

func newLongFlowProxy(t *testing.T, base string, cfg any, harnessJS string) *longFlowProxy {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("解析服务端地址 %s: %v", base, err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = 50 * time.Millisecond
	rp.ErrorLog = log.New(io.Discard, "", 0)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("取首页: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读首页: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("首页状态码 = %d", resp.StatusCode)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化自检配置: %v", err)
	}
	p := &longFlowProxy{
		page:      []byte(injectHarness(string(body), string(raw), harnessJS)),
		result:    make(chan []byte, 1),
		rp:        rp,
		waits:     map[string]map[string]int{},
		parked:    map[string]int{},
		hadValue:  map[string]int{},
		flags:     map[string]map[string]int{},
		markSeen:  map[string]longFlowMark{},
		releaseCh: make(chan struct{}),
	}
	rp.ModifyResponse = p.modifyResponse
	return p
}

func (p *longFlowProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__mark":
		p.handleMark(w, r)
		return
	case "/api/v1/stream":
		p.streamAttempts.Add(1)
		if p.blockStream.Load() {
			// 反向代理对 SSE 返 5xx（不是 401）：会话仍然有效，是 07-1 要看的形态。
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "upstream unavailable")
			return
		}
	case "/api/v1/session":
		p.sessionHits.Add(1)
	case "/api/v1/settings":
		p.settingsHits.Add(1)
	}
	if r.URL.Path == "/__result" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		p.once.Do(func() { p.result <- body })
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(p.page)
		return
	}
	p.rp.ServeHTTP(w, r)
}

// modifyResponse 是"先取到、后扣住"的地方：响应已经从真服务端拿回来了，
// 在这里等放行 —— 放行时浏览器拿到的还是一份**会话有效时**生成的完整响应。
func (p *longFlowProxy) modifyResponse(resp *http.Response) error {
	if resp.Request == nil {
		return nil
	}
	path := resp.Request.URL.Path
	p.mu.Lock()
	hit := p.armed && matchHold(p.rules, resp.Request)
	p.mu.Unlock()
	if !hit {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	hasValue := false
	var shape []string
	if raw, err := decodeBody(body, resp.Header.Get("Content-Encoding")); err == nil {
		hasValue = longFlowBodyHasPrivate(string(raw))
		shape = longFlowBodyFlags(string(raw))
	}
	p.mu.Lock()
	p.parked[path]++
	if hasValue {
		p.hadValue[path]++
	}
	if p.flags[path] == nil {
		p.flags[path] = map[string]int{}
	}
	for _, flag := range shape {
		p.flags[path][flag]++
	}
	p.mu.Unlock()

	<-p.releaseCh // 等页面把登出走完

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func matchHold(rules []holdRule, r *http.Request) bool {
	for _, rule := range rules {
		if rule.method != "" && rule.method != r.Method {
			continue
		}
		if rule.path != r.URL.Path {
			continue
		}
		if rule.query != "" && !strings.Contains(r.URL.RawQuery, rule.query) {
			continue
		}
		return true
	}
	return false
}

func decodeBody(body []byte, enc string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "identity":
		return body, nil
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		return io.ReadAll(r)
	default:
		return nil, fmt.Errorf("不认识的编码 %q", enc)
	}
}

// longFlowBodyHasPrivate 判断被扣下的响应体里有没有私有字段 —— 这是"现场不空"的证据
// （服务端确实为这条请求生成了带 local_ip / observed_ip 的完整响应）。
func longFlowBodyHasPrivate(s string) bool {
	for _, needle := range []string{"local_ip", "observed_ip", "198.51.100.77", "203.0.113", "10.0.0.5"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// longFlowBodyFlags 记下被扣下的响应体的"形态"：详情页那三条子请求里，
// /series 与 /traffic 的响应体不带地址，只能靠"点数组非空"证明它真的能画出来
// （否则"登出后没有再把点挂回画布"就分不清是守卫挡住的还是本来就没数据）。
func longFlowBodyFlags(s string) []string {
	var flags []string
	if strings.Contains(s, `"points":[[`) || strings.Contains(s, `"points": [[`) {
		flags = append(flags, "points-nonempty")
	}
	if strings.Contains(s, `"points":[]`) || strings.Contains(s, `"points": []`) {
		flags = append(flags, "points-empty")
	}
	if strings.Contains(s, "198.51.100.77") {
		flags = append(flags, "has-ping-host")
	}
	if strings.Contains(s, "local_ip") || strings.Contains(s, "observed_ip") {
		flags = append(flags, "has-node-address")
	}
	if strings.Contains(s, `"ip":"`) || strings.Contains(s, `"ip": "`) {
		flags = append(flags, "has-audit-ip")
	}
	return flags
}

func (p *longFlowProxy) handleMark(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 256))
	name := strings.TrimSpace(string(body))
	reply := longFlowMark{OK: true, Parked: map[string]int{}, HadValue: map[string]int{}}

	switch {
	case name == "arm":
		p.mu.Lock()
		p.armed = true
		p.mu.Unlock()
		reply.Note = "已武装延迟注入"
	case name == "arm-detail":
		p.mu.Lock()
		p.armed = true
		p.mu.Unlock()
		reply.Note = "已武装详情页延迟注入"
	case name == "arm-stream":
		p.blockStream.Store(true)
		reply.Note = "反代开始对 /api/v1/stream 返 503"
	case name == "logout-done":
		p.mu.Lock()
		if !p.released {
			p.released = true
			close(p.releaseCh)
		}
		p.mu.Unlock()
		reply.Note = "已放行被扣住的响应"
	case name == "logout-now":
		// 登出那一枪之前：先等服务端确认"该扣的都扣住了"，然后**解除武装** ——
		// 之后新发的请求（登出后以访客身份重新拉的那一次 /nodes）原样放过去。
		// 不解除的话，"登出后出现的卡片"就分不清是访客自己该看的，还是上一位登录者的
		// 迟到响应写回来的 —— 那正是这条用例要分辨的事。
		got, okAll := p.waitParked(p.waits[name], 15*time.Second)
		p.mu.Lock()
		p.armed = false
		p.mu.Unlock()
		reply.OK = okAll
		reply.Parked = got
		if !okAll {
			reply.Note = "超时：被扣住的请求没有凑齐（已解除武装）"
		} else {
			reply.Note = "被扣住的请求已凑齐；已解除武装"
		}
	default:
		if want, ok := p.waits[name]; ok {
			got, okAll := p.waitParked(want, 15*time.Second)
			reply.OK = okAll
			reply.Parked = got
			if !okAll {
				reply.Note = "超时：被扣住的请求没有凑齐"
			} else {
				reply.Note = "被扣住的请求已凑齐"
			}
		} else {
			reply.Note = "未知的信号 " + name
		}
	}

	p.mu.Lock()
	reply.HadValue = map[string]int{}
	for k, v := range p.hadValue {
		reply.HadValue[k] = v
	}
	for k, v := range p.parked {
		reply.Parked[k] = v
	}
	p.mu.Unlock()
	reply.Session = int(p.sessionHits.Load())
	reply.Stream = int(p.streamAttempts.Load())
	reply.Settings = int(p.settingsHits.Load())
	reply.T = int(time.Now().UnixMilli())

	p.mu.Lock()
	p.markSeen[name] = reply
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// waitParked 等到每条规则要求的条数都凑齐（或超时）。返回当前计数与是否凑齐。
func (p *longFlowProxy) waitParked(want map[string]int, timeout time.Duration) (map[string]int, bool) {
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		got := map[string]int{}
		ok := true
		for path, n := range want {
			got[path] = p.parked[path]
			if p.parked[path] < n {
				ok = false
			}
		}
		p.mu.Unlock()
		if ok || time.Now().After(deadline) {
			return got, ok
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// counts 取某一刻的累计计数（用例在浏览器跑完之后读，用来核对页面自己报的时刻）。
func (p *longFlowProxy) counts() (session, stream, settings int) {
	return int(p.sessionHits.Load()), int(p.streamAttempts.Load()), int(p.settingsHits.Load())
}

// ---------------------------------------------------------------- 浏览器回传的结果

type setDataCall struct {
	T      int `json:"t"`
	Points int `json:"points"`
	Series int `json:"series"`
}

type longFlowStream struct {
	Created       int            `json:"created"`
	Opens         int            `json:"opens"`
	Errors        int            `json:"errors"`
	Closes        int            `json:"closes"`
	Frames        int            `json:"frames"`
	CloseHooked   bool           `json:"closeHooked"`
	FramesByPhase map[string]int `json:"framesByPhase"`
}

type domSnapshot struct {
	Views        []string `json:"views"`
	Cards        int      `json:"cards"`
	CardTitles   []string `json:"cardTitles"`
	EmptyHidden  bool     `json:"emptyHidden"`
	GuestBar     bool     `json:"guestBar"`
	AdminEntries []string `json:"adminEntries"`
	LiveText     string   `json:"liveText"`
	SummaryText  string   `json:"summaryText"`
	GridText     string   `json:"gridText"`
	PageHas      []string `json:"pageHas"`
}

type formSnapshot struct {
	PwCurrent  string `json:"pwCurrent"`
	PwNew      string `json:"pwNew"`
	PwNew2     string `json:"pwNew2"`
	NodeName   string `json:"nodeName"`
	NodeGroup  string `json:"nodeGroup"`
	NodeRegion string `json:"nodeRegion"`
	NodePrice  string `json:"nodePrice"`
	NodeExpire string `json:"nodeExpires"`
	NodeTags   string `json:"nodeTags"`
	NodeNote   string `json:"nodeNote"`
	LoginUser  string `json:"loginUser"`
}

type raceSample struct {
	T         int      `json:"t"`
	Cards     int      `json:"cards"`
	Titles    []string `json:"titles"`
	NodesRows int      `json:"nodesRows"`
	AuditRows int      `json:"auditRows"`
}

type longFlowResult struct {
	Errs          []string                `json:"errs"`
	Fatal         string                  `json:"fatal"`
	Steps         []string                `json:"steps"`
	Scenario      string                  `json:"scenario"`
	RealHidden    bool                    `json:"realHidden"`
	UsedSynthetic bool                    `json:"usedSynthetic"`
	ChartHooked   bool                    `json:"chartHooked"`
	SetData       []setDataCall           `json:"setData"`
	Stream        longFlowStream          `json:"stream"`
	Marks         map[string]longFlowMark `json:"marks"`

	Home struct {
		HashBefore   string      `json:"hashBefore"`
		Settled      bool        `json:"settled"`
		Before       domSnapshot `json:"before"`
		After        domSnapshot `json:"after"`
		EmptyText    string      `json:"emptyText"`
		SummaryAfter string      `json:"summaryAfter"`
	} `json:"home"`

	Forms struct {
		Before formSnapshot `json:"before"`
		After  formSnapshot `json:"after"`
	} `json:"forms"`

	Race struct {
		LogoutAt        int               `json:"logoutAt"`
		ReleaseAt       int               `json:"releaseAt"`
		LogoutSettled   bool              `json:"logoutSettled"`
		NodesRowsBefore int               `json:"nodesRowsBefore"`
		AuditRowsBefore int               `json:"auditRowsBefore"`
		NodesTextBefore string            `json:"nodesTextBefore"`
		Samples         []raceSample      `json:"samples"`
		MaxCards        int               `json:"maxCards"`
		MaxNodesRows    int               `json:"maxNodesRows"`
		MaxAuditRows    int               `json:"maxAuditRows"`
		CardsAfter      int               `json:"cardsAfter"`
		CardTitlesAfter []string          `json:"cardTitlesAfter"`
		NodesRowsAfter  int               `json:"nodesRowsAfter"`
		AuditRowsAfter  int               `json:"auditRowsAfter"`
		PanelsAfter     map[string]string `json:"panelsAfter"`
		PageHasAfter    []string          `json:"pageHasAfter"`
		ViewsAfter      []string          `json:"viewsAfter"`
	} `json:"race"`

	Detail struct {
		Round1Targets    []string      `json:"round1Targets"`
		Round1SetData    []setDataCall `json:"round1SetData"`
		LogoutAt         int           `json:"logoutAt"`
		LogoutSettled    bool          `json:"logoutSettled"`
		TargetsAfter     []string      `json:"targetsAfter"`
		TargetsHidden    bool          `json:"targetsHidden"`
		SetDataAfter     []setDataCall `json:"setDataAfter"`
		DetailNameAfter  string        `json:"detailNameAfter"`
		LiveTextAfter    string        `json:"liveTextAfter"`
		PageHasAfter     []string      `json:"pageHasAfter"`
		ViewsAfter       []string      `json:"viewsAfter"`
		FramesBeforeStop int           `json:"framesBeforeStop"`
	} `json:"detail"`

	Stream5xx struct {
		AttemptsBefore int      `json:"attemptsBefore"`
		AttemptsAfter  int      `json:"attemptsAfter"`
		ErrorsAfter    int      `json:"errorsAfter"`
		LiveText       string   `json:"liveText"`
		CardsAfter     int      `json:"cardsAfter"`
		AdminEntries   []string `json:"adminEntries"`
		PageHasAfter   []string `json:"pageHasAfter"`
		ViewsAfter     []string `json:"viewsAfter"`
		WaitedMs       int      `json:"waitedMs"`
	} `json:"stream5xx"`

	GuestBG struct {
		GuestFramesVisible     int    `json:"guestFramesVisible"`
		GuestFramesWhileHidden int    `json:"guestFramesWhileHidden"`
		GuestStreamsBeforeHide int    `json:"guestStreamsBeforeHide"`
		GuestClosesWhileHidden int    `json:"guestClosesWhileHidden"`
		AdminFramesVisible     int    `json:"adminFramesVisible"`
		AdminFramesWhileHidden int    `json:"adminFramesWhileHidden"`
		AdminClosesWhileHide   int    `json:"adminClosesWhileHidden"`
		AdminFramesAfterShow   int    `json:"adminFramesAfterShow"`
		StreamsAfterLogin      int    `json:"streamsAfterLogin"`
		HiddenFlag             bool   `json:"hiddenFlag"`
		LiveTextAfter          string `json:"liveTextAfter"`
		CardsAtEnd             int    `json:"cardsAtEnd"`
	} `json:"guestbg"`
}

// ---------------------------------------------------------------- 自检脚本

const longFlowHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var SCEN = CFG.scenario || '';
  var PRIVATE = (CFG.private || []).filter(function (s) { return !!s; });
  var rawFetch = window.fetch.bind(window);
  var T0 = Date.now();
  var R = {
    errs: [], steps: [], fatal: '', scenario: SCEN, marks: {}, setData: [],
    home: {}, forms: {}, race: {}, detail: {}, stream5xx: {}, guestbg: {}
  };
  window.__LONGRESULT = R;

  function now() { return Date.now() - T0; }
  function el(id) { return document.getElementById(id); }
  function shown(id) { var n = el(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function textOf(id) { var n = el(id); return n ? n.textContent : ''; }
  function step(s) { R.steps.push(s + ' @+' + now() + 'ms'); }
  function waitFor(what, cond, ms) {
    var deadline = Date.now() + (ms || 20000);
    return new Promise(function (resolve, reject) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { reject(new Error(what + ' 判定抛错: ' + e.message)); return; }
        if (ok) { step(what); resolve(true); return; }
        if (Date.now() > deadline) {
          // 超时消息里带上"当时页面上是什么样"：不然只剩一句"等待超时"，
          // 排查时还得重跑一遍才知道卡在哪（这是本仓库既有用例踩过的坑）。
          reject(new Error('等待超时: ' + what + '（hash=' + window.location.hash +
            ' 视图=' + visibleViews().join(',') + ' JS 错误=' + JSON.stringify(R.errs) +
            ' 步骤=' + JSON.stringify(R.steps) + '）'));
          return;
        }
        setTimeout(poll, 50);
      })();
    });
  }
  // waitForSoft 与 waitFor 的区别：超时不抛，只把结果记下来。
  // 用在"卡住也不该让整条用例挂死"的地方（比如登出那一枪可能被连接池挡住）。
  function waitForSoft(what, cond, ms) {
    var deadline = Date.now() + (ms || 10000);
    return new Promise(function (resolve) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { ok = false; }
        if (ok) { step(what + '（成功）'); resolve(true); return; }
        if (Date.now() > deadline) { step(what + '（超时）'); resolve(false); return; }
        setTimeout(poll, 50);
      })();
    });
  }

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // ---- 观测工具 -----------------------------------------------------------

  function pageText() {
    var clone = document.body.cloneNode(true);
    Array.prototype.forEach.call(clone.querySelectorAll('script'), function (s) {
      if (s.parentNode) s.parentNode.removeChild(s);
    });
    return clone.textContent;
  }
  function hits() {
    var t = pageText();
    return PRIVATE.filter(function (s) { return t.indexOf(s) >= 0; });
  }
  function visibleViews() {
    return ['view-home', 'view-detail', 'view-settings', 'view-login', 'view-setup'].filter(shown);
  }
  function adminEntries() {
    return ['btn-add', 'btn-settings', 'btn-logout'].filter(function (id) { return !!el(id); });
  }
  function cardTitles() {
    var grid = el('grid');
    if (!grid) return [];
    return Array.prototype.map.call(grid.children, function (c) { return c.title || ''; });
  }
  function rowCount(id) { var n = el(id); return n ? n.children.length : -1; }
  // panelText：容器的正文 + 里面所有输入框的值（输入框的值 textContent 看不见，
  // 而"登出后设置页里还留着上一次的地址"这条正是要看它）。
  function panelText(id) {
    var box = el(id);
    if (!box) return '';
    var out = box.textContent || '';
    Array.prototype.forEach.call(box.querySelectorAll('input,textarea,select'), function (c) {
      out += ' | value=' + (c.value === undefined ? '' : String(c.value));
    });
    return out;
  }
  function targetTexts() {
    var box = el('lat-targets');
    if (!box) return [];
    return Array.prototype.map.call(box.querySelectorAll('.lat-card-name'), function (n) { return n.textContent; });
  }
  function snapshot() {
    return {
      views: visibleViews(),
      cards: el('grid') ? el('grid').children.length : -1,
      cardTitles: cardTitles(),
      emptyHidden: el('empty') ? el('empty').hidden : null,
      guestBar: shown('guest-bar'),
      adminEntries: adminEntries(),
      liveText: textOf('live-text'),
      summaryText: textOf('summary'),
      gridText: el('grid') ? el('grid').textContent : '',
      pageHas: hits()
    };
  }
  function formSnapshot() {
    function v(id) { var n = el(id); return n ? String(n.value) : ''; }
    return {
      pwCurrent: v('pw-current'), pwNew: v('pw-new'), pwNew2: v('pw-new2'),
      nodeName: v('node-name'), nodeGroup: v('node-group'), nodeRegion: v('node-region'),
      nodePrice: v('node-price'), nodeExpires: v('node-expires'), nodeTags: v('node-tags'),
      nodeNote: v('node-note'), loginUser: v('login-user')
    };
  }

  // ---- 实时通道仪表 -------------------------------------------------------
  //
  // ⚠️ CONNECTING / OPEN / CLOSED 三个常量必须一起搬过去（本仓库踩过的坑）：
  // app.js 用 EventSource.CLOSED 判"永久失败"，少一个就悄悄改掉被测分支。
  var framesByPhase = {};
  var phase = 'boot';
  framesByPhase[phase] = 0;
  var stream = { created: 0, opens: 0, errors: 0, closes: 0, frames: 0, closeHooked: false, framesByPhase: framesByPhase };
  R.stream = stream;
  function setPhase(p) { phase = p; if (framesByPhase[p] === undefined) framesByPhase[p] = 0; }
  function framesIn(p) { return framesByPhase[p] || 0; }
  if (window.EventSource) {
    var RealES = window.EventSource;
    var wrapped = function (url, opts) {
      var es = new RealES(url, opts);
      stream.created++;
      es.addEventListener('open', function () { stream.opens++; });
      es.addEventListener('error', function () { stream.errors++; });
      es.addEventListener('nodes', function () {
        stream.frames++;
        framesByPhase[phase] = (framesByPhase[phase] || 0) + 1;
      });
      try {
        var realClose = es.close.bind(es);
        es.close = function () { stream.closes++; return realClose(); };
        stream.closeHooked = true;
      } catch (e) { R.errs.push('包 EventSource.close 失败: ' + e.message); }
      return es;
    };
    wrapped.CONNECTING = RealES.CONNECTING;
    wrapped.OPEN = RealES.OPEN;
    wrapped.CLOSED = RealES.CLOSED;
    wrapped.prototype = RealES.prototype;
    window.EventSource = wrapped;
  }

  // ---- 画布写入仪表 -------------------------------------------------------
  //
  // 详情页那三条迟到的子请求里，/series 与 /traffic 只写画布（DOM 上看不出来），
  // 所以只能从"setData 又被调了一次、而且带着点"看出来。包的是 app.js 自己用的
  // window.ProbeChart.create（chart.js 的公开出口），不碰产品代码。
  // 返回值 points 是这一批 series 的点数总和；clearDetailCharts 的清洗是 setData([], {})，
  // 所以"points > 0"就是"真的画了一条曲线"。
  function hookProbeChart() {
    if (!window.ProbeChart || typeof window.ProbeChart.create !== 'function') return false;
    if (window.ProbeChart.__longflowHooked) return true;
    var realCreate = window.ProbeChart.create;
    window.ProbeChart.create = function (canvas, opts) {
      var chart = realCreate(canvas, opts);
      var realSetData = chart.setData;
      chart.setData = function (series, extra) {
        var points = 0;
        (series || []).forEach(function (s) { points += ((s && s.points) || []).length; });
        R.setData.push({ t: now(), points: points, series: (series || []).length });
        return realSetData.apply(chart, arguments);
      };
      return chart;
    };
    window.ProbeChart.__longflowHooked = true;
    return true;
  }
  R.chartHooked = false;

  // ---- 可见性（合成）------------------------------------------------------
  //
  // 无头 Chrome 里没法可靠地构造"真后台标签页"（另开一个窗口抢焦点那条路在无头下
  // 不保证生效，见 streamidentity_browser_test.go 的说明）。这里退化成"把
  // document.hidden 改成常量 + 派发一个真的 visibilitychange 事件"—— app.js 的
  // 处理器读的就是 document.hidden，事件本身也是真的。用没用合成如实记在结果里。
  function setHiddenFlag(hidden) {
    try {
      Object.defineProperty(document, 'hidden', { configurable: true, get: function () { return hidden; } });
      Object.defineProperty(document, 'visibilityState', {
        configurable: true, get: function () { return hidden ? 'hidden' : 'visible'; }
      });
    } catch (e) { R.errs.push('改 document.hidden 失败: ' + e.message); }
    document.dispatchEvent(new Event('visibilitychange'));
  }
  function hide() { R.usedSynthetic = true; setHiddenFlag(true); }
  function show() { R.usedSynthetic = true; setHiddenFlag(false); }
  R.realHidden = false;

  // ---- 通用动作 -----------------------------------------------------------

  function settle() {
    return waitFor('页面脚本就绪', function () {
      return shown('view-home') || shown('view-login') || shown('view-setup') || shown('view-detail');
    }, 30000).then(function () {
      R.chartHooked = hookProbeChart();
      return sleep(500);
    });
  }
  function mark(name) {
    return rawFetch('/__mark', { method: 'POST', body: name }).then(function (r) {
      return r.text().then(function (txt) {
        var info = {};
        try { info = JSON.parse(txt); } catch (e) { info = { note: 'parse: ' + txt }; }
        info.t = now();
        R.marks[name] = info;
        step('信号 ' + name + ' → ' + (info.note || ''));
        return info;
      });
    });
  }
  function login() {
    step('login() 起点：视图=' + visibleViews().join(',') + ' hash=' + window.location.hash +
      ' 登录入口=' + !!el('btn-login-entry'));
    if (shown('view-home') && el('btn-login-entry') && !el('btn-logout')) el('btn-login-entry').click();
    else if (!shown('view-login')) window.location.hash = '#/login';
    return waitFor('登录页出现', function () { return shown('view-login'); }, 20000)
      .then(function () {
        el('login-user').value = CFG.user;
        el('login-pass').value = CFG.pass;
        el('login-submit').click();
        return waitFor('登录成功（顶栏出现「退出」）', function () { return !!el('btn-logout') && shown('view-home'); }, 30000);
      });
  }
  function logoutSoft() {
    var p = waitForSoft('登出结算（管理员入口被摘掉）', function () { return !el('btn-logout'); }, 15000);
    el('btn-logout').click();
    return p;
  }

  // ---- ① 首页登出（07-3）+ 表单残留（06-3 / 06-5）+ 迟到响应（06-2 / 07-2）----
  //
  // 四条共用一次页面生命周期：两次登出、一次登录，都在同一个页面里。
  function logoutRacePass() {
    var H = R.home, F = R.forms, C = R.race;
    var samples = [];
    var sampling = false;
    // tick 用定时器一直采到 sampling 关掉：采样必须从**放行之前**开始，
    // 否则分不清"卡片是访客自己那一次响应画的"还是"迟到响应写回来的"。
    function tick() {
      if (!sampling) return;
      samples.push({
        t: now(),
        cards: el('grid') ? el('grid').children.length : -1,
        titles: cardTitles(),
        nodesRows: rowCount('nodes-list'),
        auditRows: rowCount('audit-body')
      });
      setTimeout(tick, 100);
    }
    return login()
      .then(function () { return waitFor('首页卡片出现', function () { return shown('view-home') && el('grid').children.length >= 1; }, 20000); })
      // 登录是从 #/login 进来的，地址栏停在 #/login。附注那条的前提是"hash 已是 #/"
      // ——先把它归零，并确认这一步真的落到了首页（否则下面的断言与附注说的不是一回事）。
      .then(function () {
        window.location.hash = '#/';
        return waitFor('停在首页且 hash = #/', function () {
          return shown('view-home') && window.location.hash === '#/';
        }, 10000);
      })
      .then(function () { return sleep(500); })
      // ---- 07-3：首页（hash 已是 #/）点登出，看最终视觉 ----
      .then(function () {
        H.hashBefore = window.location.hash;
        H.before = snapshot();
        step('登出前：hash=' + H.hashBefore + ' 卡片=' + H.before.cards);
        return logoutSoft();
      })
      .then(function (settled) {
        H.settled = settled;
        return sleep(1500);
      })
      .then(function () {
        H.after = snapshot();
        H.emptyText = textOf('empty');
        H.summaryAfter = textOf('summary');
        step('首页登出后的最终视觉：视图=' + H.after.views.join(',') + ' 卡片=' + H.after.cards +
          ' 空态隐藏=' + H.after.emptyHidden + ' 只读条=' + H.after.guestBar);
        return true;
      })
      // ---- 第二次登录：06-3 / 06-5 ----
      .then(function () { return login(); })
      .then(function () { return waitFor('首页卡片出现（第二轮）', function () { return shown('view-home') && el('grid').children.length >= 1; }, 20000); })
      .then(function () { return sleep(300); })
      .then(function () {
        window.location.hash = '#/settings/security';
        return waitFor('设置页安全栏出现', function () { return shown('view-settings') && !!el('pw-current'); }, 20000);
      })
      .then(function () { return sleep(800); })
      .then(function () {
        // 06-3：三个明文密码框填上标记（不提交）
        var fill = CFG.pwFill || ['', '', ''];
        el('pw-current').value = fill[0];
        el('pw-new').value = fill[1];
        el('pw-new2').value = fill[2];
        // 06-5：从服务器列表打开「编辑节点」——服务端会把 note 回填进 #node-note
        var rows = el('nodes-list') ? el('nodes-list').querySelectorAll('button') : [];
        var edit = null;
        Array.prototype.forEach.call(rows, function (b) { if (b.textContent === '编辑节点') edit = b; });
        if (!edit) { R.fatal = '设置页的服务器列表里没有「编辑节点」按钮：现场不对'; return false; }
        edit.click();
        return waitFor('节点对话框打开', function () { return el('dlg-node') && el('dlg-node').open; }, 10000);
      })
      .then(function () { return sleep(400); })
      .then(function () {
        F.before = formSnapshot();
        C.nodesRowsBefore = rowCount('nodes-list');
        C.auditRowsBefore = rowCount('audit-body');
        C.nodesTextBefore = panelText('nodes-list').slice(0, 400);
        step('表单已填：pw=' + (F.before.pwCurrent ? '有' : '无') + ' note=' + F.before.nodeNote +
          ' 服务器列表行=' + C.nodesRowsBefore + ' 操作记录行=' + C.auditRowsBefore);
        // 等操作记录那一栏真的渲染出行（不空才有判别力）
        return waitFor('操作记录出现来源地址', function () { return panelText('audit-body').indexOf('127.0.0.1') >= 0; }, 15000);
      })
      // ---- 延迟注入：先武装，再用"保存节点"触一次首页 loadNodes ----
      .then(function () { return mark('arm'); })
      .then(function () {
        window.location.hash = '#/';
        return waitFor('回到首页', function () { return shown('view-home'); }, 10000);
      })
      .then(function () { return sleep(300); })
      .then(function () {
        el('node-submit').click();   // 对话框还开着（编辑模式）：保存 ⇒ refreshNodeViews ⇒ loadNodes
        return sleep(200);
      })
      .then(function () { return mark('await-home-nodes'); })
      .then(function (info) {
        if (!info.ok) R.errs.push('首页 /nodes 没有被扣住：' + (info.note || ''));
        // 再进设置页：这一轮的四条链里 /nodes 与 /audit 会被扣住
        window.location.hash = '#/settings/security';
        return sleep(200);
      })
      .then(function () { return mark('logout-now'); })
      .then(function (info) {
        if (!info.ok) R.errs.push('设置页那几条链没有被扣齐：' + (info.note || ''));
        C.logoutAt = now();
        return logoutSoft();
      })
      .then(function (settled) {
        C.logoutSettled = settled;
        return sleep(600);
      })
      .then(function () {
        // 采样从**放行之前**就开始：登出后以访客身份重新拉的那一次 /nodes 会立刻
        // 画出一张卡片（那是访客自己该看的），而"上一位登录者的迟到响应"只可能在
        // 放行之后落地 —— 两者必须分得开，所以这里记的是每一点的卡片 title 与时刻。
        sampling = true;
        tick();
        return mark('logout-done');
      })
      .then(function () {
        C.releaseAt = now();
        return sleep(1600);
      })
      .then(function () {
        sampling = false;
        C.samples = samples;
        C.maxCards = 0;
        C.maxNodesRows = 0;
        C.maxAuditRows = 0;
        samples.forEach(function (s) {
          if (s.cards > C.maxCards) C.maxCards = s.cards;
          if (s.nodesRows > C.maxNodesRows) C.maxNodesRows = s.nodesRows;
          if (s.auditRows > C.maxAuditRows) C.maxAuditRows = s.auditRows;
        });
        F.after = formSnapshot();
        C.cardsAfter = el('grid') ? el('grid').children.length : -1;
        C.cardTitlesAfter = cardTitles();
        C.nodesRowsAfter = rowCount('nodes-list');
        C.auditRowsAfter = rowCount('audit-body');
        C.panelsAfter = {
          nodesList: panelText('nodes-list'), auditBody: panelText('audit-body'),
          serverInfo: panelText('server-info'), pingList: panelText('ping-list')
        };
        C.pageHasAfter = hits();
        C.viewsAfter = visibleViews();
        step('放行后采样 ' + samples.length + ' 个点：卡片最多 ' + C.maxCards + ' 个、服务器列表最多 ' +
          C.maxNodesRows + ' 行、操作记录最多 ' + C.maxAuditRows + ' 行');
        return true;
      });
  }

  // ---- ② 详情页子请求迟到（06-4）-----------------------------------------
  function detailPass() {
    var D = R.detail;
    return login()
      .then(function () { return waitFor('首页卡片出现', function () { return shown('view-home') && el('grid').children.length >= 1; }, 20000); })
      .then(function () {
        R.chartHooked = hookProbeChart();
        if (!R.chartHooked) { R.fatal = 'window.ProbeChart 没装上：画布写入观察不到'; return Promise.reject(new Error(R.fatal)); }
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitFor('详情页打开', function () { return shown('view-detail') && textOf('detail-name') === CFG.nodeName; }, 20000);
      })
      .then(function () { return sleep(1500); })
      // 第一轮（不扣）：证明管理员的详情页真的会把探测目标地址写进卡片、图表真的画过点
      .then(function () {
        D.round1Targets = targetTexts();
        D.round1SetData = R.setData.slice();
        step('第一轮详情页：探测目标卡片=' + JSON.stringify(D.round1Targets) +
          ' setData 调用=' + D.round1SetData.length);
        return mark('arm-detail');
      })
      .then(function () {
        window.location.hash = '#/';
        return waitFor('回到首页', function () { return shown('view-home'); }, 10000);
      })
      .then(function () { return sleep(400); })
      .then(function () {
        R.setData = [];   // 只看第二轮之后的写入
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitFor('详情页第二轮打开', function () { return shown('view-detail') && textOf('detail-name') === CFG.nodeName; }, 20000);
      })
      .then(function () { return mark('await-detail-parked'); })
      .then(function (info) {
        if (!info.ok) R.errs.push('详情页子请求没有被扣齐：' + (info.note || ''));
        D.logoutAt = now();
        D.framesBeforeStop = R.stream.frames;
        return logoutSoft();
      })
      .then(function (settled) {
        D.logoutSettled = settled;
        return sleep(600);
      })
      .then(function () { return mark('logout-done'); })
      .then(function () { return sleep(1800); })
      .then(function () {
        D.targetsAfter = targetTexts();
        D.targetsHidden = el('lat-targets') ? el('lat-targets').hidden : null;
        D.setDataAfter = R.setData.slice();
        D.detailNameAfter = textOf('detail-name');
        D.liveTextAfter = textOf('live-text');
        D.pageHasAfter = hits();
        D.viewsAfter = visibleViews();
        var points = 0;
        D.setDataAfter.forEach(function (c) { points += c.points; });
        step('setData 时间线（第二轮起）：' + D.setDataAfter.map(function (c) {
          return c.t + ':' + c.points;
        }).join(','));
        step('登出并放行之后：setData 调用 ' + D.setDataAfter.length + ' 次（点数 ' + points +
          '）、探测目标卡片=' + JSON.stringify(D.targetsAfter));
        return true;
      });
  }

  // ---- ③ 反代对 SSE 返 5xx（07-1）----------------------------------------
  function stream5xxPass() {
    var S = R.stream5xx;
    return login()
      .then(function () { return waitFor('实时流建立', function () { return R.stream.opens >= 1; }, 20000); })
      .then(function () { return sleep(500); })
      .then(function () {
        S.attemptsBefore = R.stream.created;
        step('5xx 之前：建流 ' + S.attemptsBefore + ' 条、状态栏=' + textOf('live-text'));
        return mark('arm-stream');
      })
      .then(function () { hide(); return sleep(500); })
      .then(function () {
        show();   // 回前台 ⇒ connectStream() ⇒ 这次反代返 503
        var t0 = Date.now();
        return sleep(3500).then(function () { S.waitedMs = Date.now() - t0; });
      })
      .then(function () {
        S.attemptsAfter = R.stream.created;
        S.errorsAfter = R.stream.errors;
        S.liveText = textOf('live-text');
        S.cardsAfter = el('grid') ? el('grid').children.length : -1;
        S.adminEntries = adminEntries();
        S.pageHasAfter = hits();
        S.viewsAfter = visibleViews();
        step('5xx 之后：建流 ' + S.attemptsAfter + ' 条、error ' + S.errorsAfter + ' 次、状态栏=' + S.liveText);
        return true;
      });
  }

  // ---- ④ 访客后台标签页（07-5）-------------------------------------------
  function guestBGPass() {
    var G = R.guestbg;
    return settle()
      .then(function () { return waitFor('访客首页卡片出现', function () { return shown('view-home') && el('grid').children.length >= 1; }, 25000); })
      .then(function () {
        setPhase('guestVisible');
        return waitFor('访客流开始推帧', function () { return framesIn('guestVisible') >= 2; }, 25000);
      })
      .then(function () {
        G.guestFramesVisible = framesIn('guestVisible');
        G.guestStreamsBeforeHide = R.stream.created;
        hide();
        return sleep(500);            // 先让"切换那一瞬"过去，再开始数
      })
      .then(function () {
        setPhase('guestHidden');
        G.hiddenFlag = document.hidden === true;
        return sleep(3000);
      })
      .then(function () {
        G.guestFramesWhileHidden = framesIn('guestHidden');
        G.guestClosesWhileHidden = R.stream.closes;
        show();
        return sleep(500);
      })
      .then(function () { return login(); })
      .then(function () {
        setPhase('adminVisible');
        return waitFor('管理员流开始推帧', function () { return framesIn('adminVisible') >= 2; }, 25000);
      })
      .then(function () {
        G.adminFramesVisible = framesIn('adminVisible');
        G.streamsAfterLogin = R.stream.created;
        R.closesAtAdminHide = R.stream.closes;
        hide();
        return sleep(500);
      })
      .then(function () {
        setPhase('adminHidden');
        return sleep(3000);
      })
      .then(function () {
        G.adminFramesWhileHidden = framesIn('adminHidden');
        G.adminClosesWhileHide = R.stream.closes - R.closesAtAdminHide;
        show();
        setPhase('adminShownAgain');
        return waitFor('实时流自己接回来', function () { return framesIn('adminShownAgain') >= 1; }, 20000);
      })
      .then(function () {
        G.adminFramesAfterShow = framesIn('adminShownAgain');
        G.liveTextAfter = textOf('live-text');
        G.cardsAtEnd = el('grid') ? el('grid').children.length : -1;
        step('访客后台收帧 ' + G.guestFramesWhileHidden + ' 帧；管理员后台收帧 ' + G.adminFramesWhileHidden + ' 帧');
        return true;
      });
  }

  // ---- 截图模式（07-3 的人工核对图）--------------------------------------
  function shot() {
    // 截图那条路带 --virtual-time-budget：挂着的 SSE 长连接会让虚拟时间停住，
    // 所以（与仓库其它截图用例一样）把 EventSource 换成一个不联网的替身。
    if (CFG.shot === 'home-logout') {
      if (!window.EventSource) return Promise.reject(new Error('没有 EventSource'));
      window.EventSource = function () {
        var listeners = {};
        this.addEventListener = function (name, fn) { (listeners[name] = listeners[name] || []).push(fn); };
        this.close = function () {};
        setTimeout(function () {
          (listeners['open'] || []).forEach(function (fn) { fn({ type: 'open' }); });
        }, 0);
      };
    }
    return login()
      .then(function () { return waitFor('首页卡片出现', function () { return shown('view-home') && el('grid').children.length >= 1; }, 20000); })
      .then(function () {
        window.location.hash = '#/';
        return waitFor('停在首页且 hash = #/', function () {
          return shown('view-home') && window.location.hash === '#/';
        }, 10000);
      })
      .then(function () { return sleep(800); })
      .then(function () {
        el('btn-logout').click();
        return waitForSoft('登出结算', function () { return !el('btn-logout'); }, 10000);
      })
      .then(function () { return sleep(1500); })
      .then(function () {
        R.home.after = snapshot();
        R.home.emptyText = textOf('empty');
        R.home.summaryAfter = textOf('summary');
        return true;
      });
  }

  function run() {
    if (SCEN === 'logoutrace') return logoutRacePass();
    if (SCEN === 'detail') return detailPass();
    if (SCEN === 'stream5xx') return stream5xxPass();
    if (SCEN === 'guestbg') return guestBGPass();
    if (SCEN === 'shot') return shot();
    return Promise.reject(new Error('未知场景 ' + SCEN));
  }

  run().then(function () {
    if (CFG.shot) { document.title = 'SHOT-DONE'; return null; }
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:longflow'; });
  }).catch(function (err) {
    R.fatal = String(err && err.message ? err.message : err);
    step('失败：' + R.fatal);
    if (CFG.shot) { document.title = 'SHOT-FAIL'; return null; }
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:longflow-fail'; });
  });
})();`

// ---------------------------------------------------------------- 跑一条场景

type longFlowConfig struct {
	User     string   `json:"user"`
	Pass     string   `json:"pass"`
	Scenario string   `json:"scenario"`
	NodeID   int64    `json:"nodeID"`
	NodeName string   `json:"nodeName"`
	Private  []string `json:"private"`
	PWFill   []string `json:"pwFill"`
	Shot     string   `json:"shot"`
}

const (
	longFlowUser = "admin"
	longFlowPass = "a-very-good-password"
)

// runLongFlowScenario 起反代 + mock + 真 Chrome，把页面回传的观测值解回来。
func runLongFlowScenario(t *testing.T, f *longFlowFixture, scenario string, timeout time.Duration) (*longFlowResult, *longFlowProxy) {
	t.Helper()
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	cfg := longFlowConfig{
		User: longFlowUser, Pass: longFlowPass, Scenario: scenario,
		NodeID: f.nodeID, NodeName: f.vals.NodeName,
		Private: f.vals.all(),
		PWFill:  []string{"LEAK-pw-current-1", "LEAK-pw-new-2", "LEAK-pw-new2-3"},
	}
	proxy := newLongFlowProxy(t, "http://"+f.h.addr, cfg, longFlowHarnessJS)
	switch scenario {
	case "logoutrace":
		// 只扣三条：首页 /nodes（F2 的落点）、设置页 /nodes（发现 2 里写 IP 文本的那条）
		// 与 /audit（写来源 IP）。Chrome 每个源只有 6 条连接，扣满会把登出那一枪堵死。
		proxy.rules = []holdRule{
			{method: http.MethodGet, path: "/api/v1/nodes"},
			{method: http.MethodGet, path: "/api/v1/audit"},
		}
		proxy.waits = map[string]map[string]int{
			"await-home-nodes": {"/api/v1/nodes": 1},
			"logout-now":       {"/api/v1/nodes": 2, "/api/v1/audit": 1},
		}
	case "detail":
		proxy.rules = []holdRule{
			{method: http.MethodGet, path: fmt.Sprintf("/api/v1/nodes/%d/series", f.nodeID), query: "metric=cpu"},
			{method: http.MethodGet, path: fmt.Sprintf("/api/v1/nodes/%d/traffic", f.nodeID)},
			{method: http.MethodGet, path: fmt.Sprintf("/api/v1/nodes/%d/ping", f.nodeID)},
		}
		proxy.waits = map[string]map[string]int{
			"await-detail-parked": {
				fmt.Sprintf("/api/v1/nodes/%d/series", f.nodeID):  1,
				fmt.Sprintf("/api/v1/nodes/%d/traffic", f.nodeID): 1,
				fmt.Sprintf("/api/v1/nodes/%d/ping", f.nodeID):    1,
			},
		}
	}
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, timeout, "1500,1100")

	var res longFlowResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 800))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v；页面里的 JS 问题：%v）",
			res.Fatal, res.Steps, res.Errs)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条问题：%v", len(res.Errs), res.Errs)
	}
	if res.Stream.CloseHooked != true {
		t.Errorf("EventSource.close 没包上：后台标签页那几条断言会失去一半证据")
	}
	t.Logf("自检脚本走过的步骤：\n  %s", strings.Join(res.Steps, "\n  "))
	return &res, proxy
}

// longFlowEcho 只把"实际值"带进报错信息里，方便一眼看出发生了什么。
func longFlowEcho(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

// ---------------------------------------------------------------- 06-2 / 07-2 / 06-3 / 06-5 / 07-3

// TestLongFlowLogoutRaceAndFormResidue 覆盖 06-2、06-3、06-5、07-2，外加 07-3 的自动化那一半。
//
// 一次页面生命周期里做三件事（分开跑要多花两次 Chrome 启动 + 两次登录，整包预算不允许）：
//
//	① 07-3：管理员在**首页**（hash 已是 `#/`）点「退出登录」，看最终视觉；
//	② 06-3 / 06-5：第二次登录 → 设置页填三个明文密码框、从服务器列表打开「编辑节点」
//	   （服务端把 note 回填进 #node-note）→ 关掉对话框（值留在 DOM 里，这是非空证据）；
//	③ 06-2 / 07-2：反代把**已经取到**的 `/api/v1/nodes`（首页那条 + 设置页那条）与
//	   `/api/v1/audit` 扣住 → 保存节点让首页 loadNodes 在飞 → 登出 → 放行 → 采样 1.5 秒。
//
// 断言：
//   - 06-3：登出后 `#pw-current/#pw-new/#pw-new2` 必须为空（登出前必须有值）；
//   - 06-5：登出后 `#node-name/.../#node-note` 必须为空（登出前 note 必须是服务端那份）；
//     顺带 `#login-user` 也必须为空（它从来只在登出清理里被清）；
//   - 06-2：放行之后 `#grid` 里**一个卡片都不许再出现**、任何卡片 title 里不许有地址；
//   - 07-2：放行之后设置页的服务器列表与操作记录必须仍是空的、页面文本里不许有地址；
//   - 07-3：把"首页登出的最终视觉"如实记下来（特征化断言，见下面的注释）。
func TestLongFlowLogoutRaceAndFormResidue(t *testing.T) {
	f := startLongFlowFixture(t, true, false, false)
	res, proxy := runLongFlowScenario(t, f, "logoutrace", 180*time.Second)

	// ---- 现场自检：被扣住的响应真的是"会话有效时生成的完整响应" ----
	nodesParked := proxy.parked["/api/v1/nodes"]
	auditParked := proxy.parked["/api/v1/audit"]
	if nodesParked < 2 || auditParked < 1 {
		t.Fatalf("延迟注入没有扣齐请求（/nodes %d 条、/audit %d 条）：现场没搭起来，"+
			"下面的断言会变成空断言", nodesParked, auditParked)
	}
	if got := proxy.flags["/api/v1/nodes"]["has-node-address"]; got < 2 {
		t.Fatalf("被扣住的 /nodes 响应里只有 %d 条带节点地址（期望 ≥2）：扣住的不是管理员响应，"+
			"断言失去判别力（响应体形态 %v）", got, proxy.flags)
	}
	if got := proxy.flags["/api/v1/audit"]["has-audit-ip"]; got < 1 {
		t.Fatalf("被扣住的 /audit 响应里没有来源地址（响应体形态 %v）：扣住的不是管理员响应",
			proxy.flags)
	}
	// 被扣住的每一条 /nodes 都必须是**管理员**响应（带节点地址）：登出之后以访客身份
	// 重新拉的那一次会被"解除武装"放过去（见 logout-now 的说明），不能混进这里的计数。
	if got, all := proxy.flags["/api/v1/nodes"]["has-node-address"], proxy.parked["/api/v1/nodes"]; got != all {
		t.Fatalf("被扣住的 %d 条 /nodes 里只有 %d 条带节点地址：混进了访客响应，"+
			"下面的断言分不清是谁写的（形态 %v）", all, got, proxy.flags)
	}
	// 登出之前设置页那两栏必须有内容，否则"登出后是空的"没有判别力。
	if res.Race.NodesRowsBefore < 1 || res.Race.AuditRowsBefore < 1 {
		t.Fatalf("登出前设置页是空的（服务器列表 %d 行、操作记录 %d 行）：这条断言会是空的",
			res.Race.NodesRowsBefore, res.Race.AuditRowsBefore)
	}
	t.Logf("延迟注入：扣住 /nodes %d 条、/audit %d 条；响应体形态 %v；登出时刻 +%dms、放行时刻 +%dms",
		nodesParked, auditParked, proxy.flags, res.Race.LogoutAt, res.Race.ReleaseAt)
	if !res.Race.LogoutSettled {
		t.Fatalf("登出没有结算（管理员入口还在）：这一轮什么都没测到")
	}

	// ---- 06-3：明文密码跨登出边界 ----
	before := res.Forms.Before
	if before.PwCurrent == "" || before.PwNew == "" || before.PwNew2 == "" {
		t.Fatalf("登出前三个密码框里没有值（%s）：这条断言会是空的", longFlowEcho(before))
	}
	if before.PwCurrent != "LEAK-pw-current-1" || before.PwNew != "LEAK-pw-new-2" || before.PwNew2 != "LEAK-pw-new2-3" {
		t.Fatalf("登出前密码框里的值与填进去的不一样：%s", longFlowEcho(before))
	}
	after := res.Forms.After
	if after.PwCurrent != "" || after.PwNew != "" || after.PwNew2 != "" {
		t.Errorf("06-3：登出之后改密表单里仍然留着明文密码（当前密码=%q 新密码=%q 再输一次=%q）—— "+
			"共享终端上按 F12 就能读到上一位管理员的凭据", after.PwCurrent, after.PwNew, after.PwNew2)
	}

	// ---- 06-5：node-* 表单与登录名残留 ----
	if before.NodeNote != f.vals.Note {
		t.Fatalf("登出前 #node-note 里不是服务端那份备注（拿到 %q，期望 %q）：打开「编辑节点」"+
			"没有把 note 回填进来，这条断言失去判别力", before.NodeNote, f.vals.Note)
	}
	if before.NodeName != f.vals.NodeName {
		t.Fatalf("登出前 #node-name = %q，期望 %q", before.NodeName, f.vals.NodeName)
	}
	if before.LoginUser != longFlowUser {
		t.Fatalf("登出前 #login-user = %q，期望 %q（登录页填过之后从不清）", before.LoginUser, longFlowUser)
	}
	residue := map[string]string{
		"node-name": after.NodeName, "node-group": after.NodeGroup, "node-region": after.NodeRegion,
		"node-price": after.NodePrice, "node-expires": after.NodeExpire, "node-tags": after.NodeTags,
		"node-note": after.NodeNote, "login-user": after.LoginUser,
	}
	for id, val := range residue {
		if val != "" {
			t.Errorf("06-5：登出之后 #%s 里仍然留着 %q（登出前是服务端回填的那份）—— "+
				"#node-note 是服务端明确列为访客私有的自由文本（可能反推出主机）", id, val)
		}
	}

	// ---- 06-2：迟到的 /nodes 不许把上一位登录者的卡片写回 #grid ----
	//
	// 判据是"卡片 title 里有没有私有字段"，不是"有没有卡片"：登出之后页面会以**访客**
	// 身份重新拉一次 `/nodes`（那是访客自己该看的一屏，卡片 title 里只有名字），
	// 而迟到的**管理员**响应写回来的卡片 title 上带着 observed_ip
	// （`dto.name + ' · ' + dto.observed_ip`，见 app.js 的 updateCard）。
	// 两者的区别就是这条用例要抓的东西 —— 放了行之后才出现的、带地址的那一张。
	leakedTitles := 0
	firstLeak := ""
	for _, s := range res.Race.Samples {
		for _, title := range s.Titles {
			for _, v := range f.vals.all() {
				if strings.Contains(title, v) {
					if leakedTitles == 0 {
						firstLeak = fmt.Sprintf("登出后第 %dms（放行时刻 +%dms）的卡片 title = %q（私有值 %q）",
							s.T, res.Race.ReleaseAt, title, v)
					}
					leakedTitles++
				}
			}
		}
	}
	if leakedTitles > 0 {
		t.Errorf("06-2：%s —— 上一位登录者的 /nodes 响应被写回了首页（%d 个采样点都带着它）",
			firstLeak, leakedTitles)
	}
	for _, title := range res.Race.CardTitlesAfter {
		for _, v := range f.vals.all() {
			if strings.Contains(title, v) {
				t.Errorf("06-2：结束时卡片 title 里还有私有值 %q：%q", v, title)
			}
		}
	}
	t.Logf("06-2：采样 %d 个点（放行时刻 +%dms）、卡片峰值 %d、带私有值的 title 命中 %d 次；"+
		"放行后卡片 title 时间线 = %s", len(res.Race.Samples), res.Race.ReleaseAt,
		res.Race.MaxCards, leakedTitles, longFlowTitleTimeline(res.Race.Samples, res.Race.ReleaseAt))

	// ---- 07-2：迟到的设置页响应不许把私有一栏写回来 ----
	if res.Race.MaxNodesRows != 0 || res.Race.NodesRowsAfter != 0 {
		t.Errorf("07-2：放行之后设置页的服务器列表又出现了 %d 行（结束时 %d 行）：%s",
			res.Race.MaxNodesRows, res.Race.NodesRowsAfter, res.Race.PanelsAfter["nodesList"])
	}
	if res.Race.MaxAuditRows != 0 || res.Race.AuditRowsAfter != 0 {
		t.Errorf("07-2：放行之后操作记录表又出现了 %d 行（结束时 %d 行）：%s",
			res.Race.MaxAuditRows, res.Race.AuditRowsAfter, res.Race.PanelsAfter["auditBody"])
	}
	if strings.Contains(res.Race.PanelsAfter["auditBody"], "127.0.0.1") {
		t.Errorf("07-2：登出后的操作记录栏里留着来源地址：%q", res.Race.PanelsAfter["auditBody"])
	}
	for name, text := range res.Race.PanelsAfter {
		for _, v := range f.vals.all() {
			if strings.Contains(text, v) {
				t.Errorf("07-2：登出后 #%s 里出现了私有值 %q：%q", name, v, text)
			}
		}
	}
	if len(res.Race.PageHasAfter) != 0 {
		t.Errorf("登出后页面文本里还有这些私有值：%v", res.Race.PageHasAfter)
	}
	t.Logf("放行后采样 %d 个点：卡片峰值 %d、服务器列表峰值 %d 行、操作记录峰值 %d 行；"+
		"登出后停在视图 %v", len(res.Race.Samples), res.Race.MaxCards, res.Race.MaxNodesRows,
		res.Race.MaxAuditRows, res.Race.ViewsAfter)

	// ---- 07-3：首页登出的最终视觉（特征化断言，请连着注释一起读）----
	//
	// 下面这两条锁住的是**当前（未修复）**的行为：管理员在首页（hash 已是 #/）点退出，
	// 页面停在可见的首页上，但既没有卡片、也没有空态文案 —— 也就是审计附注说的"一块空白"。
	// 产品该做的是"登出后按身份重新渲染（访客首页 / 登录页）"（见 ROUND4-GAPS.md 的 C9）。
	// 谁修了它，这里会红：那时应当把断言改成"卡片或空态文案至少有一个"，而不是删掉。
	if res.Home.HashBefore != "#/" {
		t.Fatalf("首页登出这条的前提没成立：登出前 hash = %q，期望 #/", res.Home.HashBefore)
	}
	if res.Home.Before.Cards < 1 {
		t.Fatalf("登出前首页没有卡片：这条断言会是空的")
	}
	if !res.Home.Settled {
		t.Fatalf("首页那次登出没有结算")
	}
	if res.Home.After.Cards != 0 {
		t.Errorf("首页登出后卡片数 = %d，期望 0（resetHome 会摘掉全部卡片）", res.Home.After.Cards)
	}
	if !containsString(res.Home.After.Views, "view-home") {
		t.Errorf("首页登出后可见视图 = %v，期望仍然停在首页（hash 没变、没有 hashchange）",
			res.Home.After.Views)
	}
	if !res.Home.After.EmptyHidden {
		t.Errorf("首页登出后空态文案 `#empty` 出现了（文本 %q）—— 那就不是空白面板了，"+
			"审计附注的说法需要改写", res.Home.EmptyText)
	}
	if len(res.Home.After.AdminEntries) != 0 {
		t.Errorf("首页登出后管理员入口还在文档里：%v", res.Home.After.AdminEntries)
	}
	if !res.Home.After.GuestBar {
		t.Errorf("服务端开着访客查看，登出后应当显示只读提示条")
	}
	if len(res.Home.After.PageHas) != 0 {
		t.Errorf("首页登出后页面文本里还有私有值：%v", res.Home.After.PageHas)
	}
	t.Logf("07-3 首页登出的最终视觉：视图=%v 卡片=%d 空态隐藏=%v 汇总条=%q 实时状态=%q 只读条=%v",
		res.Home.After.Views, res.Home.After.Cards, res.Home.After.EmptyHidden,
		res.Home.SummaryAfter, res.Home.After.LiveText, res.Home.After.GuestBar)
}

// longFlowTitleTimeline 把采样序列压成一行"时刻:卡片数:是不是放行之后"，供报告引用。
func longFlowTitleTimeline(samples []raceSample, releaseAt int) string {
	var parts []string
	for i, s := range samples {
		if i > 0 && s.Cards == samples[i-1].Cards && (i < len(samples)-1) && s.Cards == samples[i+1].Cards {
			continue // 只留变化点，别把三十几个一样的点全打出来
		}
		mark := "放行前"
		if s.T >= releaseAt {
			mark = "放行后"
		}
		parts = append(parts, fmt.Sprintf("%dms(%s):%d:%v", s.T, mark, s.Cards, s.Titles))
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------- 06-4

// TestLongFlowDetailSubRequestsArriveLate 覆盖 06-4（`F3` 的端到端复现）。
//
// 走法：登录 → 进详情页（第一轮不拦，证明地址真会画进卡片、图真会画点）→ 回首页 →
// 反代武装 → 再进详情页（这一轮的 `/series?metric=cpu`、`/traffic`、`/ping` 被扣住）
// → 在详情页点「退出登录」→ 放行 → 等 1.8 秒。
//
// 断言（三条迟到写入，各自会红）：
//   - `/ping` 的迟到响应不许重建探测目标卡片（管理员的响应里 host 是有值的 ⇒
//     卡片上写的就是目标地址）；
//   - `/series` 与 `/traffic` 的迟到响应不许再 `setData` 把点挂回画布；
//   - 详情页 DOM 里不许留下任何私有值。
func TestLongFlowDetailSubRequestsArriveLate(t *testing.T) {
	f := startLongFlowFixture(t, true, false, true)
	res, proxy := runLongFlowScenario(t, f, "detail", 180*time.Second)

	seriesPath := fmt.Sprintf("/api/v1/nodes/%d/series", f.nodeID)
	trafficPath := fmt.Sprintf("/api/v1/nodes/%d/traffic", f.nodeID)
	pingPath := fmt.Sprintf("/api/v1/nodes/%d/ping", f.nodeID)
	if proxy.parked[seriesPath] < 1 || proxy.parked[trafficPath] < 1 || proxy.parked[pingPath] < 1 {
		t.Fatalf("详情页子请求没有扣齐（series=%d traffic=%d ping=%d）：现场没搭起来",
			proxy.parked[seriesPath], proxy.parked[trafficPath], proxy.parked[pingPath])
	}
	if !res.ChartHooked {
		t.Fatalf("window.ProbeChart.create 没包上：画布写入观察不到，这条用例说明不了问题")
	}
	if !res.Detail.LogoutSettled {
		t.Fatalf("详情页那次登出没有结算")
	}
	// 被扣住的响应必须"真的能画"：/series 与 /traffic 的体里点数组非空，
	// /ping 的体里有目标地址。少一条，"登出后没有写回"就成了空断言。
	if proxy.flags[seriesPath]["points-nonempty"] < 1 {
		t.Fatalf("被扣住的 /series 响应里没有非空的点数组（形态 %v）：它即使落地也画不出曲线，"+
			"下面的断言失去判别力", proxy.flags[seriesPath])
	}
	if proxy.flags[trafficPath]["points-nonempty"] < 1 {
		t.Fatalf("被扣住的 /traffic 响应里没有非空的点数组（形态 %v）", proxy.flags[trafficPath])
	}
	if proxy.flags[pingPath]["has-ping-host"] < 1 {
		t.Fatalf("被扣住的 /ping 响应里没有目标地址 %q（形态 %v）", f.vals.PingHost, proxy.flags[pingPath])
	}

	// ---- 现场自检：第一轮真的把地址写进了卡片、真的画过带点的曲线 ----
	if len(res.Detail.Round1Targets) == 0 {
		t.Fatalf("第一轮详情页的探测目标卡片是空的：现场不对（下面的断言会变成空断言）")
	}
	joined := strings.Join(res.Detail.Round1Targets, " ")
	if !strings.Contains(joined, f.vals.PingHost) {
		t.Fatalf("第一轮详情页的探测目标卡片里没有地址 %q（拿到 %q）："+
			"这条用例要验的就是「迟到响应把地址写回来」，第一轮都没写出来就没有判别力",
			f.vals.PingHost, joined)
	}
	pointsBefore := 0
	for _, c := range res.Detail.Round1SetData {
		pointsBefore += c.Points
	}
	if pointsBefore == 0 {
		t.Fatalf("第一轮详情页一次带点的 setData 都没有（调用 %d 次）：图表没画过，"+
			"下面「登出后没有再画」就分不清是被守卫挡住还是本来就没数据", len(res.Detail.Round1SetData))
	}
	t.Logf("第一轮详情页：探测目标卡片=%v；setData %d 次、共 %d 个点；登出 +%dms",
		res.Detail.Round1Targets, len(res.Detail.Round1SetData), pointsBefore, res.Detail.LogoutAt)
	t.Logf("延迟注入实况：扣住 %v；响应体形态 %v", proxy.parked, proxy.flags)

	// ---- 迟到响应不许重建探测目标卡片 ----
	afterJoined := strings.Join(res.Detail.TargetsAfter, " ")
	if strings.Contains(afterJoined, f.vals.PingHost) {
		t.Errorf("06-4：登出之后探测目标的**地址**被迟到的 /ping 响应重新写进了（隐藏的）详情页："+
			"卡片=%v（登出前是 %v）", res.Detail.TargetsAfter, res.Detail.Round1Targets)
	}
	if len(res.Detail.TargetsAfter) != 0 {
		t.Errorf("06-4：登出之后探测目标卡片又被重建了 %d 张：%v（closeDetail 应当把它清空）",
			len(res.Detail.TargetsAfter), res.Detail.TargetsAfter)
	}

	// ---- 迟到响应不许再 setData ----
	latePoints := 0
	for _, c := range res.Detail.SetDataAfter {
		if c.T < res.Detail.LogoutAt {
			continue
		}
		latePoints += c.Points
		if c.Points > 0 {
			t.Errorf("06-4：登出之后（+%dms，登出时刻 +%dms）还有人把 %d 个点挂回画布（%d 条曲线）—— "+
				"迟到的 /series 或 /traffic 响应绕过了 detail.seq 守卫", c.T, res.Detail.LogoutAt,
				c.Points, c.Series)
		}
	}
	t.Logf("登出之后 setData 调用 %d 次、其中带点的共 %d 个点", len(res.Detail.SetDataAfter), latePoints)

	if len(res.Detail.PageHasAfter) != 0 {
		t.Errorf("06-4：登出后页面文本里还有私有值：%v", res.Detail.PageHasAfter)
	}
}

// ---------------------------------------------------------------- 07-1

// TestLongFlowStreamFivedxxFromProxy 覆盖 07-1（`发现 1` 修复后的副作用面）。
//
// 形态：反代对 `/api/v1/stream` 返 **503**（不是 401）——会话仍然有效，响应的不是身份问题。
// 页面先在后台/前台之间切一次（管理员这一档会 stopStream，回前台再 connectStream），
// 于是新连接撞上 503。
//
// 断言：
//   - 5xx 之后**恰好发生一次**会话复查（`GET /api/v1/session` 计数 +1）——这正是
//     "CLOSED 即复查"那条修复的作用面；
//   - 复查发现会话仍然有效 ⇒ 页面**不许**被清空/登出（卡片与管理员入口都还在）：
//     5xx 没有被当成身份失效；
//   - 不会再出现新的 `/stream` 尝试（EventSource 对非 200 是 fail the connection，
//     浏览器自己不重连 —— 这是现状，如实钉住）。
func TestLongFlowStreamFivedxxFromProxy(t *testing.T) {
	f := startLongFlowFixture(t, false, false, false)
	res, proxy := runLongFlowScenario(t, f, "stream5xx", 180*time.Second)

	mark, ok := res.Marks["arm-stream"]
	if !ok || !strings.Contains(mark.Note, "503") {
		t.Fatalf("没有武装「反代对 SSE 返 503」（marks=%s）", longFlowEcho(res.Marks))
	}
	if res.Stream5xx.AttemptsAfter != res.Stream5xx.AttemptsBefore+1 {
		t.Fatalf("建流次数从 %d 变成 %d，期望恰好 +1（回到前台时应当只重连一次）",
			res.Stream5xx.AttemptsBefore, res.Stream5xx.AttemptsAfter)
	}
	finalSession, finalStream, _ := proxy.counts()
	if delta := finalSession - mark.Session; delta != 1 {
		t.Errorf("07-1：反代对 SSE 返 503 之后，会话复查次数 = %d，期望恰好 1 次"+
			"（app.js 在 `es.readyState === EventSource.CLOSED` 时立刻复查一次）—— "+
			"修复前这里恒为 0", delta)
	}
	if delta := finalStream - mark.Stream; delta != 1 {
		t.Errorf("07-1：503 之后一共发了 %d 次 /stream 请求，期望恰好 1 次（非 200 ⇒ 浏览器不再重连；"+
			"但也不该自己循环重连打服务端）", delta)
	}
	if res.Stream5xx.CardsAfter < 1 {
		t.Errorf("07-1：5xx 之后首页卡片数 = %d —— 会话明明还有效，页面不该被清空",
			res.Stream5xx.CardsAfter)
	}
	if !containsString(res.Stream5xx.AdminEntries, "btn-logout") {
		t.Errorf("07-1：5xx 之后管理员入口 = %v —— 会话有效却被摘掉了管理员的入口",
			res.Stream5xx.AdminEntries)
	}
	if res.Stream5xx.LiveText != "已断开，重连中" {
		t.Errorf("07-1：5xx 之后状态栏 = %q，期望 %q", res.Stream5xx.LiveText, "已断开，重连中")
	}
	t.Logf("07-1：503 之后 /session +%d 次、/stream +%d 次、状态栏=%q、卡片=%d（会话仍然有效，页面没有被清）",
		finalSession-mark.Session, finalStream-mark.Stream, res.Stream5xx.LiveText, res.Stream5xx.CardsAfter)
}

// ---------------------------------------------------------------- 07-5

// TestLongFlowGuestBackgroundTabKeepsStreaming 覆盖 07-5。
//
// 走法：访客只读面板（有真 Agent 每秒上报）→ 合成"切到后台"→ 数 3 秒里的帧 →
// 回前台 → 登录管理员（实时流按新身份重建）→ 再切后台 → 数 3 秒里的帧 → 回前台。
//
// 断言（对比式，两条都会红）：
//   - **访客**切后台之后 SSE **仍然在收帧**（当前行为：visibilitychange 的第一行是
//     `if (!session.authenticated) return;`，访客走不到 stopStream）——这是特征化断言，
//     锁住的是现状；谁改了它，这里会红并应当把断言反过来写；
//   - **管理员**切后台之后帧数必须为 0（stopStream 真的执行了）——这一条不空，靠的是
//     前一条证明"仪表真的在数帧"；回前台之后必须自己接回来。
func TestLongFlowGuestBackgroundTabKeepsStreaming(t *testing.T) {
	f := startLongFlowFixture(t, true, true, false)
	res, _ := runLongFlowScenario(t, f, "guestbg", 180*time.Second)
	g := res.GuestBG

	if !res.UsedSynthetic || !g.HiddenFlag {
		t.Fatalf("后台状态没有构造出来（synthetic=%v hiddenFlag=%v）：这条用例没测到东西",
			res.UsedSynthetic, g.HiddenFlag)
	}
	if g.GuestFramesVisible < 2 {
		t.Fatalf("访客在前台只收到 %d 帧：Agent 没在推，这条用例说明不了问题", g.GuestFramesVisible)
	}
	if g.AdminFramesVisible < 2 {
		t.Fatalf("登录管理员之后只收到 %d 帧：身份切换后的那条流没在推", g.AdminFramesVisible)
	}
	if g.HiddenFlag != true {
		t.Fatalf("数帧的时候 document.hidden 不是 true")
	}
	// 访客：后台仍在收（特征化）
	if g.GuestFramesWhileHidden == 0 {
		t.Errorf("07-5：访客标签页切到后台之后 3 秒里一帧都没收到 —— 与当前代码不符" +
			"（visibilitychange 处理器第一行 `if (!session.authenticated) return;` 会让访客跳过 stopStream）。" +
			"若这是**有意改的**（让访客也省流量），请把这条断言反过来写成「后台不再收帧」")
	}
	// 管理员：后台必须停（对照组，证明仪表不空）
	if g.AdminFramesWhileHidden != 0 {
		t.Errorf("07-5 对照组：管理员切到后台之后仍然收到 %d 帧 —— stopStream 没有生效"+
			"（这一条同时说明上面的「访客仍在收」不是仪表失灵）", g.AdminFramesWhileHidden)
	}
	if g.AdminFramesAfterShow < 1 {
		t.Errorf("07-5：回前台之后实时流没有自己接回来（帧数 %d）", g.AdminFramesAfterShow)
	}
	if g.StreamsAfterLogin < 2 {
		t.Errorf("07-5：登录之后一共只建过 %d 条流，期望 ≥2（访客一条 + 管理员一条：身份变了必须重建）",
			g.StreamsAfterLogin)
	}
	t.Logf("07-5：访客前台 %d 帧 / 后台 %d 帧；管理员前台 %d 帧 / 后台 %d 帧 / 回前台 %d 帧；"+
		"建流共 %d 条",
		g.GuestFramesVisible, g.GuestFramesWhileHidden, g.AdminFramesVisible,
		g.AdminFramesWhileHidden, g.AdminFramesAfterShow, g.StreamsAfterLogin)
}

// ---------------------------------------------------------------- 07-3 截图

// TestShotHomeAfterLogout 只产出一张人工核对的 PNG：管理员在首页（hash 已是 #/）
// 点「退出登录」之后的那一屏。默认**跳过**（CI 上不该往磁盘里写 PNG）：
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestShotHomeAfterLogout -v
//
// 自动化那一半的断言在 TestLongFlowLogoutRaceAndFormResidue 里（同一形态，只是那里
// 把观测值带回了 Go）。这张图是给"是不是一块空白"一个可以直接看的答案。
func TestShotHomeAfterLogout(t *testing.T) {
	outDir := os.Getenv("PROBE_SHOT_DIR")
	if outDir == "" {
		t.Skip("没设 PROBE_SHOT_DIR：截图用例默认不跑（它只产出人工核对的 PNG）")
	}
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("创建截图目录: %v", err)
	}

	f := startLongFlowFixture(t, true, false, false)
	cfg := longFlowConfig{
		User: longFlowUser, Pass: longFlowPass, Scenario: "shot", Shot: "home-logout",
		NodeID: f.nodeID, NodeName: f.vals.NodeName, Private: f.vals.all(),
	}
	proxy := newLongFlowProxy(t, "http://"+f.h.addr, cfg, longFlowHarnessJS)
	mock := newMockServer(t, proxy)
	out := filepath.Join(outDir, "home-after-logout.png")

	args := []string{
		"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
		"--hide-scrollbars",
		"--user-data-dir=" + t.TempDir(),
		"--window-size=1500,1100",
		"--virtual-time-budget=60000",
		"--screenshot=" + out,
		mock.URL + "/",
	}
	cmd := exec.Command(chrome, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 Chrome: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		killChrome(cmd)
		t.Fatalf("Chrome 超时。输出尾部：\n%s", tail(buf.String(), 800))
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("截图没有生成: %v\nChrome 输出尾部：\n%s", err, tail(buf.String(), 800))
	}
	t.Logf("首页登出后的那一屏 → %s（%d 字节）", out, st.Size())
}
