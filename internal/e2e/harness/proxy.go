package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestZone 是跨时区用例用的服务端时区。
//
// 故意选一个与本机浏览器（Asia/Shanghai，UTC+8）不同的：两边一样时，页面上的时间
// 无论按哪一边渲染都长得一样，测了等于没测。它以前叫 tzTestZone 放在 tz 那个文件里，
// 但 axis / cardstate / 轴标签截图那几条用例也拿它当"服务端时区 ≠ 浏览器时区"的
// 前提，现在分开成不同包了，所以放到这里共用。
const TestZone = "Asia/Tokyo" // UTC+9

// HarnessConfig 是注入到页面里的那份自检配置（脚本从 window.__TZCFG 读它）。
//
// 字段全是导出的：它在各包的用例里以复合字面量的形式现场拼出来
// （`harness.HarnessConfig{NodeID: …, Scenario: …}`），只写用得上的那几个。
type HarnessConfig struct {
	NodeID   int64  `json:"nodeID"`
	NodeName string `json:"nodeName"`
	Date     string `json:"date"`
	// Private 是"页面上一个都不许出现"的私有值（访客脱敏用例用）：
	// 自检脚本拿它去搜页面文本，Go 那边再核对搜到的结果。
	//
	// 为什么把现场的真值交给脚本而不是在脚本里写死：同一条用例既要证明
	// "访客页面上没有它"，也要证明"管理员页面上**有**它"（否则前一条是空断言），
	// 两边用的必须是同一批字符串。
	Private []string `json:"private,omitempty"`
	User    string   `json:"user"`
	Pass    string   `json:"pass"`
	// Shot 非空时脚本只把界面开到指定状态就停住（给人截图核对用），
	// 不跑完整流程、也不回传结果。取值：audit / chart / dialog。
	Shot string `json:"shot"`
	// Scenario 是自检脚本要跑哪一套流程（不同用例观测的东西不同）：
	// 延迟图那条用 agg / interval，跨时区那条不用。
	Scenario string `json:"scenario"`
	// Nodes 是"节点名字 → id"（汇率那条用例要逐台机器看三处显示口径，
	// 名字用来在首页卡片与服务器列表里找到那一行，id 用来直接开它的详情页）。
	Nodes map[string]int64 `json:"nodes,omitempty"`
	// Focus 是"这条用例主要看哪台机器"（汇率那条用例的期望值是 Go 侧按它算的，
	// 详情页路由也必须走同一台；map 序列化之后键是字典序，不能靠"第一个"猜）。
	Focus string `json:"focus,omitempty"`
	// Theme 只在截图模式下用：取 "dark" 时页面进来就切到深色
	// （深色是 [data-theme="dark"] 而不是系统偏好 —— 无头浏览器没法可靠地
	// 模拟 prefers-color-scheme，走手动切换那条分支同时也验了它）。
	Theme string `json:"theme,omitempty"`
	// TwoFACode / RecoveryCodes 是两步验证那组用例的现场凭据
	// （见 twofa_browser_test.go）：
	//
	// 截图那条路带 --virtual-time-budget，页面里的 Date.now() 是**虚拟时间**，
	// 脚本自己算出来的 6 位码对不上服务端的钟。所以现场由 Go 侧预置好
	// （用一份独立的 Go 实现算码），再经这两个字段交给页面。
	TwoFACode     string   `json:"twofaCode,omitempty"`
	RecoveryCodes []string `json:"recoveryCodes,omitempty"`
	// TwoFASecret 是两步验证的**密钥**（不是某个时刻的 6 位码）。
	//
	// 为什么给密钥而不是给算好的码：断言那条路（不带 --virtual-time-budget）里
	// 6 位码是**提交那一刻**在页面里用 WebCrypto 现算的（见 twofa_browser_test.go
	// 的说明）。先在 Go 侧算好再传进去的话，用例走几步之后可能已经跨了 30 秒窗口，
	// 变成一个与时钟有关的偶发红。只有截图那条路（虚拟时间）才需要预置好的码。
	TwoFASecret string `json:"twofaSecret,omitempty"`
}

// NewHarnessProxy 起一个"反代 + 只改首页"的层，首页里注入调用方给的脚本。
func NewHarnessProxy(t *testing.T, base string, cfg HarnessConfig, harnessJS string) *Proxy {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("解析服务端地址 %s: %v", base, err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	// SSE 要立刻往下灌：总览条与卡片都靠它（Go 对 text/event-stream 本来就会
	// 立即 flush，这里显式写一遍是不依赖那个隐式行为）。
	rp.FlushInterval = 50 * time.Millisecond
	// 用例收尾时 Chrome 被强杀，长连接会带着 context canceled 报错回来 ——
	// 那是我们自己杀的，不该混进测试输出里。
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
		t.Fatalf("取首页状态码 = %d", resp.StatusCode)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化自检配置: %v", err)
	}
	return &Proxy{
		Upstream: rp,
		Page:     []byte(InjectHarness(string(body), string(raw), harnessJS)),
		Result:   make(chan []byte, 1),
	}
}

// Proxy 是"反向代理 + 只改首页"的一层。
//
//   - "/" → 从**真服务端**取 index.html，注入自检脚本后返回（注入必须发生在
//     index.html 上，所以需要这一层）；
//   - "/__result" → 浏览器把观测结果 POST 回这里，测试从 channel 里取走；
//   - "/__alive" → 注入脚本的解析期心跳（存活信号），记进账本、**不转发**；
//   - 其它一切 → 原样转发给真服务端（Cookie / CSRF / SSE 一个都不碰）。
//
// 为什么不把 Chrome 直接指向服务端：注入不进去。为什么不干脆 mock 接口：
// 那等于把"后端按 --timezone 切天"这一半也 mock 掉了，而这正是要验证的东西。
type Proxy struct {
	Upstream *httputil.ReverseProxy
	Page     []byte
	Result   chan []byte
	once     sync.Once
	// live 是 mock 侧的存活账本（请求 + 心跳）：RunChromeForResult 靠它把
	// "等回传超时"变成"页面死在某某一步"。惰性建 —— Proxy 允许被跨包用
	// 字面量构造（见 chart/tz_shot_test.go），那种情况下也得能用。
	live     *Liveness
	liveOnce sync.Once
}

// Results 与 Live 一起满足 ResultSource（见 chrome.go）：外层要等的是
// "结果"，但判"页面死没死"靠的是账本。两者必须成对传，才不会退化成
// "只有一个事件、对页面状态零观测"的老形态。
func (p *Proxy) Results() <-chan []byte { return p.Result }

// Live 返回存活账本。ExpectBeats 由**自己发出去的那份 HTML**决定：
// 页里有心跳前导脚本，才拿"一帧心跳都没有"当"脚本没执行"的证据。
func (p *Proxy) Live() *Liveness {
	p.liveOnce.Do(func() {
		l := NewLiveness()
		l.ExpectBeats(bytes.Contains(p.Page, []byte(aliveMarker)))
		p.live = l
	})
	return p.live
}

// ServeHTTP 是 Proxy 的入口：/__alive 收心跳、/__result 收结果、首页换成注入过
// 脚本的那份、其余一切原样转发。每一笔都先记进存活账本（到达时刻 + 状态码）。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	live := p.Live()
	nw := Observe(live, w, r)
	defer nw.Finish()
	switch {
	case r.URL.Path == alivePath:
		live.ServeAlive(nw, r)
	case r.URL.Path == "/__result":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		p.once.Do(func() { p.Result <- body })
		nw.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		nw.Header().Set("Content-Type", "text/html; charset=utf-8")
		nw.Header().Set("Cache-Control", "no-store")
		_, _ = nw.Write(p.Page)
	default:
		p.Upstream.ServeHTTP(nw, r)
	}
}

func NewTZProxy(t *testing.T, base string, cfg HarnessConfig) *Proxy {
	t.Helper()
	return NewHarnessProxy(t, base, cfg, HarnessJS)
}

// InjectHarness 把自检脚本插在 </body> 之前：脚本是普通（非 defer）的内联脚本，
// 会在 app.js/chart.js 这些 defer 脚本**之前**执行，钩子因此一定装得上。
//
// 插进去的是两段：先是**存活心跳**的前导脚本（live.go），再是调用方给的自检脚本。
// 前导脚本在解析期就发第一帧心跳 —— 它是"注入脚本到底执行了没有"的唯一证据：
// 未决的样式表会把它后面的内联脚本全部挡住（页面侧修不了），那时心跳一帧都没有。
//
// harnessJS 由调用方给：跨时区那条用例与延迟图那条用例要观测的东西完全不同，
// 各带一份脚本比在一个脚本里塞两套分支清楚。
func InjectHarness(html, cfgJSON, harnessJS string) string {
	const marker = "</body>"
	at := strings.LastIndex(html, marker)
	if at < 0 {
		panic("index.html 里没有 </body>：注入点找不到")
	}
	script := "<script>window.__TZCFG = " + cfgJSON + ";</script>\n" +
		"<script>\n" + alivePreambleJS + "\n</script>\n" +
		"<script>\n" + harnessJS + "\n</script>\n"
	return html[:at] + script + html[at:]
}

// NewMockServer 起一个只服务注入过自检脚本的首页 + 反代 + 结果回收的 httptest 服务。
func NewMockServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// NewShotProxyWith 与 internal/e2e/chart 的 newShotProxy 同一套（反代 + 只改首页），
// 只是注入哪一份自检脚本由调用方给：X 轴那几张与首页分组那三张观测的东西完全不同。
func NewShotProxyWith(t *testing.T, base string, cfg HarnessConfig, harnessJS string) *Proxy {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("解析服务端地址 %s: %v", base, err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = 50 * time.Millisecond
	// 注意：测试二进制的 CWD 是【调用方那个包的目录】（internal/e2e/<pkg>/），
	// 不是这个包自己的目录 —— 所以这里比原来多一层 ".."。
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("读取 index.html: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化自检配置: %v", err)
	}
	return &Proxy{
		Upstream: rp,
		Page:     []byte(InjectHarness(string(body), string(raw), harnessJS)),
		Result:   make(chan []byte, 1),
	}
}

// HarnessJS 是注入到页面里的自检脚本。
//
// 它只做三件事：**装钩子**（收集 JS 报错、把 canvas 上真正画出来的字记下来、
// 记下 SSE 给的原始 ts）、**按用户的真实操作驱动界面**（登录 → 进详情页 →
// 开编辑框 → 改到期日 → 保存 → 重新打开，连着来三轮）、**把观测值 POST 回去**。
// 一句断言都不在这里做：期望值要用 Go 的 time 包算（见文件顶部说明）。
const HarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], steps: [], browserTZ: '', fatal: '',
    nodesServerTime: '', updatedAfterNodesFetch: null, updatedLog: [], updatedLogAtSSE: [],
    sseTSList: [], serverInfoTZ: '', auditHeader: '', auditRendered: [], auditRawTS: [],
    traffic: {}, cpu: {}, hoverTraffic: {}, hoverCpu: {}, dialogRounds: []
  };
  window.__TZRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });
  try { R.browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch (e) { R.browserTZ = '?'; }

  // ---- 钩 canvas：把真正 fillText 出来的字按画布 id 记下来 ------------------
  // 只静态看代码是证明不了"轴上写的是哪一天"的，必须看它画了什么。
  var drawn = {};
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    var orig = proto.fillText;
    proto.fillText = function (text, x, y) {
      var id = this.canvas ? this.canvas.id : '';
      if (id === 'chart-traffic' || id === 'chart-cpu') {
        var list = drawn[id] || (drawn[id] = []);
        list.push({ text: String(text), x: x, y: y });
      }
      return orig.apply(this, arguments);
    };
  })();
  function markOf(id) { return (drawn[id] || []).length; }
  function since(id, mark) { return (drawn[id] || []).slice(mark); }
  function uniqSince(id, mark, minY) {
    var out = [], seen = {};
    since(id, mark).forEach(function (r) {
      if (minY !== undefined && r.y < minY) return;
      if (seen[r.text]) return;
      seen[r.text] = 1;
      out.push({ text: r.text, x: r.x, y: r.y });
    });
    return out;
  }

  // ---- 钩 fetch：记下 /api/v1/nodes 那一帧渲染完之后「更新于」是什么 --------
  // 首帧必须显示占位 —（而不是浏览器时钟算出来的时间），这一条只有在这里取样
  // 才抓得到：它一瞬间就被随后到达的 SSE 覆盖了。
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var method = (init && init.method) || 'GET';
    return rawFetch(input, init).then(function (res) {
      if (method === 'GET' && /\/api\/v1\/nodes$/.test(url)) {
        var origJSON = res.json.bind(res);
        res.json = function () {
          return origJSON().then(function (data) {
            R.nodesServerTime = (data.server && data.server.time) || '';
            if (R.updatedAfterNodesFetch === null) {
              // 微任务跑完之后才轮到定时器：这时 app.js 那一帧已经渲染完了。
              setTimeout(function () {
                if (R.updatedAfterNodesFetch === null) {
                  R.updatedAfterNodesFetch = textOf('updated');
                }
              }, 0);
            }
            return data;
          });
        };
      }
      return res;
    });
  };

  // ---- 钩 EventSource：记下服务端给的原始 ts -------------------------------
  var OrigES = window.EventSource;
  window.EventSource = function (url, cfg) {
    var es = new OrigES(url, cfg);
    es.addEventListener('nodes', function (ev) {
      try {
        var p = JSON.parse(ev.data);
        if (p && typeof p.ts === 'number') R.sseTSList.push(p.ts);
      } catch (e) { R.errs.push('SSE 解析失败: ' + e.message); }
    });
    return es;
  };
  window.EventSource.prototype = OrigES.prototype;

  // ---- 「更新于」的变化轨迹 -------------------------------------------------
  var updatedEl = null;
  function watchUpdated() {
    updatedEl = document.getElementById('updated');
    if (!updatedEl) return;
    if (window.MutationObserver) {
      new MutationObserver(function () {
        var v = updatedEl.textContent;
        if (R.updatedLog[R.updatedLog.length - 1] !== v) R.updatedLog.push(v);
      }).observe(updatedEl, { childList: true, characterData: true, subtree: true });
    }
  }

  // ---- 小工具 --------------------------------------------------------------
  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  // ---- 自检自己的取数：**单次 20 秒上限 + 失败换一条请求重发一次** ----------
  //
  // 这里原来是裸 fetch，**没有任何上限**：只要有一条 /api/v1/... 的响应永远不回
  // 来（半开连接、keep-alive 竞态、中转层把这一笔丢了 —— 都不是页面能控制的），
  // 自检就永远停在那个 then 上：不 reject（于是不写 R.fatal）、不报错、也不回传。
  // 而 Go 侧等的是"页面 POST 回结果"这**一个**事件，它对"页面还活着吗 / 走到哪
  // 一步了"零观测 ⇒ 只能干等到外层上限（这条用例是 90 秒），失败信息里连卡在
  // 哪一步都没有（历史偶发正是这个形态：Chrome 只有 GCM 噪声、没有 JS 报错、
  // 没有步骤列表）。
  //
  // 现在单次给 20 秒上限，超时就**换一条请求重发一次**：
  //   · 单笔响应丢了（最常见的那种）⇒ 重发就过去了，用例照常绿；
  //   · 真的持续卡死 ⇒ 抛错，run() 的 catch 写进 R.fatal，页面照常 finish() 回传
  //     —— Go 侧拿到的是"取数失败 <url>"这种能定位的失败，而不是静默超时。
  // 判据与观测值一个都没动：拿到的仍然是同一个接口的同一份 JSON。
  function boundedFetch(url, init) {
    var left = 2;
    function once() {
      var ctl = (typeof AbortController === 'function') ? new AbortController() : null;
      var timedOut = false;
      var timer = ctl ? setTimeout(function () { timedOut = true; ctl.abort(); }, 20000) : null;
      var opts = {};
      for (var k in (init || {})) {
        if (Object.prototype.hasOwnProperty.call(init, k)) opts[k] = init[k];
      }
      if (ctl) opts.signal = ctl.signal;
      return rawFetch(url, opts).then(function (res) {
        if (timer) clearTimeout(timer);
        return res;
      }, function (err) {
        if (timer) clearTimeout(timer);
        if (--left > 0) {
          R.steps.push('取数没回来（20 秒上限），换一条请求重发一次: ' + url);
          return sleep(200).then(once);
        }
        var why = timedOut ? '20 秒没有响应（已 abort）' : (err && err.message ? err.message : String(err));
        throw new Error('取数失败（单次 20 秒上限、已重发一次）' + url + '：' + why);
      });
    }
    return once();
  }
  function getJSON(url) {
    return boundedFetch(url, { headers: { 'Accept': 'application/json' }, credentials: 'same-origin', cache: 'no-store' })
      .then(function (res) { return res.json(); });
  }
  function waitFor(what, cond, ms) {
    var deadline = Date.now() + (ms || 20000);
    return new Promise(function (resolve, reject) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { reject(new Error(what + ' 判定抛错: ' + e.message)); return; }
        if (ok) { R.steps.push(what); resolve(true); return; }
        if (Date.now() > deadline) { reject(new Error('等待超时: ' + what)); return; }
        setTimeout(poll, 50);
      })();
    });
  }
  // 「服务器信息」栏里的只读网格：每一项是一格 .kv-cell（小标签 .kv-k + 值 .kv-v）。
  // 改版前是 <dl> 里成对的 dt/dd，所以这里按"相邻两个子元素"读；
  // 现在一对值包在同一格里（.kv-grid 是 grid，dt/dd 会被拆到相邻两格）。
  function serverInfoValue(label) {
    var box = node('server-info');
    if (!box) return '';
    var cells = box.querySelectorAll('.kv-cell');
    for (var i = 0; i < cells.length; i++) {
      var k = cells[i].querySelector('.kv-k');
      if (k && k.textContent === label) {
        var v = cells[i].querySelector('.kv-v');
        return v ? v.textContent : '';
      }
    }
    return '';
  }

  // ---- 0. 登录（走真实表单：不注入 Cookie，整条链路与用户手点一样）--------
  // 登录成功的判据不能只看首页：带 #/n/<id> 或 #/settings/audit 进来时，
  // enterApp() 会直接路由到详情页/设置页，首页自始至终是 hidden 的。
  function loggedIn() {
    return shown('view-home') || shown('view-detail') || shown('view-settings');
  }
  function login() {
    return waitFor('出现登录视图', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', loggedIn, 30000);
      });
  }

  // ---- 1. 首页：等第一帧 SSE，并把「更新于」的轨迹快照下来 -------------------
  function home() {
    return waitFor('收到第一帧 SSE', function () { return R.sseTSList.length > 0; }, 30000)
      .then(function () {
        return waitFor('「更新于」已按服务端 ts 渲染', function () {
          return /^更新于 /.test(textOf('updated'));
        }, 15000);
      })
      .then(function () {
        R.updatedLogAtSSE = R.updatedLog.slice();
        if (R.updatedLogAtSSE.length === 0) R.updatedLogAtSSE = [textOf('updated')];
        return true;
      });
  }

  // ---- 2. 详情页：等两张图都画出来 -----------------------------------------
  function openDetail() {
    window.location.hash = '#/n/' + CFG.nodeID;
    return waitFor('详情页打开', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000).then(function () {
      return waitFor('流量图已绘制', function () { return markOf('chart-traffic') > 20; }, 30000);
    }).then(function () {
      return waitFor('CPU 图已绘制', function () { return markOf('chart-cpu') > 20; }, 30000);
    });
  }

  function canvasRect(id) { return node(id).getBoundingClientRect(); }

  // 强制重画一次（图表引擎监听 window 的 resize，80ms 防抖之后整张重画）：
  // 这样"要分析的那一帧"是我们自己触发的，几何尺寸也已经稳定。
  function forceRedraw(id) {
    var mark = markOf(id);
    window.dispatchEvent(new Event('resize'));
    return sleep(500).then(function () { return mark; });
  }

  // 把一张图的 X 轴标签（画在绘图区下方那一行）连同像素坐标一起收下来。
  function captureAxis(id, slot, t0, t1, points) {
    return forceRedraw(id).then(function (mark) {
      var rect = canvasRect(id);
      var h = Math.round(rect.height);
      slot.drawn = markOf(id);
      slot.width = Math.round(rect.width);
      slot.height = h;
      slot.plotLeft = 64;   // 与 chart.js 的 layout() 一致
      slot.plotRight = 8;
      slot.t0 = t0;
      slot.t1 = t1;
      slot.points = points;
      slot.labels = uniqSince(id, mark, h - 30).filter(function (r) {
        return /^[0-9]{2}[-:][0-9]{2}/.test(r.text);
      });
      return true;
    });
  }

  // 悬浮浮层那几行是**这一帧最后画的**东西（chart.js 的 drawHover 排在 draw() 末尾），
  // 而且行距恒为 14px（见 chart.js 里 lineH）。从末尾按行距往回走，就能把浮层的行
  // 原样取出来 —— 不能按文字去重：浮层里那个日期完全可能和 X 轴上某个标签同字
  // （"09-30" 既是轴标签、又是某一天的读数），按文字筛会把它吞掉。
  function tooltipRows(id, mark) {
    var list = since(id, mark);
    var tail = [];
    for (var i = list.length - 1; i >= 0; i--) {
      if (tail.length === 0) { tail.unshift(list[i]); continue; }
      if (Math.abs((tail[0].y - list[i].y) - 14) <= 2) { tail.unshift(list[i]); continue; }
      break;
    }
    return tail.map(function (r) { return r.text; });
  }

  // 悬浮：按图表引擎的线性映射算出"目标点"的像素位置，把鼠标停上去，
  // 再把浮层里新画出来的那几行字收下来。
  function hoverPoint(id, slot, t0, t1, ts) {
    var canvas = node(id);
    var rect = canvasRect(id);
    var w = Math.round(rect.width);
    var plotW = w - 64 - 8;
    var px = 64 + (ts - t0) / Math.max(t1 - t0, 1) * plotW;
    if (px > w - 10) px = w - 10;
    if (px < 70) px = 70;
    var mark = markOf(id);
    canvas.dispatchEvent(new MouseEvent('mousemove', {
      clientX: rect.left + px, clientY: rect.top + 60, bubbles: true
    }));
    slot.targetTS = ts;
    slot.rows = tooltipRows(id, mark);
    slot.time = slot.rows.filter(function (t) { return /^[0-9]{2}[-:][0-9]{2}/.test(t); })[0] || '';
    canvas.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }));
    return true;
  }

  function charts() {
    return getJSON('/api/v1/nodes/' + CFG.nodeID + '/series?range=1h&metric=cpu')
      .then(function (data) {
        var pts = (data.points || []).map(function (p) { return p[0]; });
        if (pts.length < 5) throw new Error('CPU 曲线点太少：' + pts.length);
        var mid = pts[Math.floor(pts.length / 2)];
        return captureAxis('chart-cpu', R.cpu, pts[0], pts[pts.length - 1], pts)
          .then(function () { return hoverPoint('chart-cpu', R.hoverCpu, pts[0], pts[pts.length - 1], mid); });
      })
      .then(function () {
        return getJSON('/api/v1/nodes/' + CFG.nodeID + '/traffic?days=7');
      })
      .then(function (data) {
        var pts = (data.points || []).map(function (p) { return p[0]; });
        if (pts.length < 7) throw new Error('日流量点太少：' + pts.length);
        var last = pts[pts.length - 1];
        return captureAxis('chart-traffic', R.traffic, pts[0], last, pts)
          .then(function () { return hoverPoint('chart-traffic', R.hoverTraffic, pts[0], last, last); });
      });
  }

  // ---- 3. 设置页 · 操作记录 ------------------------------------------------
  function audit() {
    window.location.hash = '#/settings/audit';
    return waitFor('操作记录已渲染', function () {
      return shown('view-settings') && node('audit-body').childNodes.length > 0 && serverInfoValue('时区') !== '';
    }, 30000).then(function () {
      R.auditHeader = textOf('audit-time-head');
      R.serverInfoTZ = serverInfoValue('时区');
      var rows = node('audit-body').childNodes;
      for (var i = 0; i < rows.length && i < 6; i++) {
        R.auditRendered.push(rows[i].childNodes[0].textContent);
      }
      return getJSON('/api/v1/audit?limit=50');
    }).then(function (data) {
      (data.entries || []).slice(0, 6).forEach(function (e) { R.auditRawTS.push(e.ts); });
      return true;
    });
  }

  // ---- 4. 到期日往返：打开编辑 → 填日期 → 保存 → 重新打开，连着三轮 --------
  function openEdit() {
    return waitFor('详情页就绪', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000).then(function () {
      return waitFor('编辑按钮可用', function () { return node('detail-edit') && !node('detail-edit').disabled; }, 10000);
    }).then(function () {
      node('detail-edit').click();
      return waitFor('节点对话框打开', function () { return node('dlg-node').open === true; }, 15000);
    });
  }

  function roundTrip(n) {
    var rec = { round: n, before: '', readback: '', stored: 0, remaining: 0 };
    window.location.hash = '#/n/' + CFG.nodeID;
    return openEdit().then(function () {
      rec.before = node('node-expires').value;
      node('node-expires').value = CFG.date;
      return waitFor('提交按钮可用', function () { return !node('node-submit').disabled; }, 15000);
    }).then(function () {
      node('node-submit').click();
      return waitFor('保存返回（对话框关闭）', function () { return node('dlg-node').open === false; }, 30000);
    }).then(function () {
      return getJSON('/api/v1/nodes/' + CFG.nodeID);
    }).then(function (data) {
      rec.stored = data.node.expires_at;
      rec.remaining = data.node.remaining_days;
      return openEdit();
    }).then(function () {
      rec.readback = node('node-expires').value;
      node('node-cancel').click();
      R.dialogRounds.push(rec);
      return true;
    });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(function () { watchUpdated(); return true; })
      .then(login)
      .then(home)
      .then(openDetail)
      .then(charts)
      .then(audit)
      .then(function () { return roundTrip(1); })
      .then(function () { return roundTrip(2); })
      .then(function () { return roundTrip(3); });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () {
      document.title = 'DUMPMARK:' + JSON.stringify({ errs: R.errs.length, fatal: R.fatal });
    });
  }

  // 起跑**不再等 load 事件**：load 要等所有子资源（style.css / chart.js / app.js…）
  // 全部就绪，而自检真正需要的只是 DOM —— 它第一步本来就是轮询等 view-login
  // 出现（waitFor，30 秒上限）。把整条自检挂在一个与它无关的事件上，等于白白多
  // 一处"事件不来就整条静默"的窗口：某个 defer 脚本慢/挂住时 load 永远不触发，
  // 页面既不自检也不回传，Go 侧只能干等到外层上限。起跑提前只改变"从哪一刻开始
  // 轮询 DOM"，步骤名与顺序、判据、观测值一个都不变。
  if (CFG.shot) {
    shot();   // 截图模式：只把界面开到指定状态，不回传结果
  } else {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  }

  // shot 模式：只把界面开到指定状态（给人工截图核对用），不跑完整流程。
  function shot() {
    var mark = function (what) { document.title = 'SHOT-READY:' + what; };
    login().then(function () {
      if (CFG.shot === 'audit') {
        window.location.hash = '#/settings/audit';
        return waitFor('操作记录已渲染', function () {
          return shown('view-settings') && node('audit-body').childNodes.length > 0;
        }, 30000).then(function () { mark('audit'); });
      }
      window.location.hash = '#/n/' + CFG.nodeID;
      return waitFor('详情页打开', function () {
        return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
      }, 30000).then(function () {
        if (CFG.shot === 'dialog') {
          return openEdit().then(function () { mark('dialog'); });
        }
        return waitFor('流量图已绘制', function () { return markOf('chart-traffic') > 20; }, 30000)
          .then(function () { mark('chart'); });
      });
    }).catch(function (err) { R.fatal = String(err && err.message ? err.message : err); });
  }
})();`

// GroupHarnessJS 是注入到页面里的自检脚本（与其它浏览器用例同一套做法）。
//
// 它按用户的真实操作驱动界面：等首页 → 读 chip → 点分组 → 数还看得见的卡片 →
// 刷新一次验证选择留下来了 → 进设置页**合成**一次拖动（Pointer Events）→
// 切回「全部」再拖一次。断言全在 Go 那边。
//
// 刷新之后脚本会再跑一遍：用 sessionStorage 记一个阶段标记，第二遍接着往下走
// （localStorage 里的选择正好是这一遍要验证的东西）。
const GroupHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], fatal: '', steps: [], chips: [], total: 0,
    summaryBefore: '', summaryAfter: '', afterPickChip: '', afterPickVisible: [],
    ungroupedVisible: [], stored: '', afterReloadChip: '', afterReloadVisible: [],
    keyboardChipFocus: '',
    lockedHandle: {}, unlockedHandle: {}, orderPutsLocked: 0, orderPutsUnlocked: 0,
    narrow: {}
  };
  window.__GROUPRESULT = R;

  // 截图模式：把 EventSource 换成一个不联网的替身（理由见 axis_browser_test.go：
  // 挂着的 SSE 请求会让 --virtual-time-budget 永远耗不完，Chrome 就不截图）。
  if (CFG.shot) {
    window.EventSource = function () {
      var listeners = {};
      this.addEventListener = function (name, fn) { (listeners[name] = listeners[name] || []).push(fn); };
      this.close = function () {};
      setTimeout(function () {
        (listeners['open'] || []).forEach(function (fn) { fn({ type: 'open' }); });
      }, 0);
    };
  }

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // ---- 记 PUT /api/v1/nodes/order 发了几次（拖动排序的唯一出口）-------------
  var orderPuts = 0;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var method = ((init && init.method) || 'GET').toUpperCase();
    return rawFetch(input, init).then(function (res) {
      if (method === 'PUT' && url.indexOf('/nodes/order') >= 0) orderPuts++;
      return res;
    });
  };

  // ---- 小工具 -------------------------------------------------------------
  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function waitFor(what, cond, ms) {
    var deadline = Date.now() + (ms || 20000);
    return new Promise(function (resolve, reject) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { reject(new Error(what + ' 判定抛错: ' + e.message)); return; }
        if (ok) { R.steps.push(what); resolve(true); return; }
        if (Date.now() > deadline) { reject(new Error('等待超时: ' + what)); return; }
        setTimeout(poll, 50);
      })();
    });
  }

  function chips() {
    var box = node('group-filter');
    if (!box) return [];
    var out = [];
    Array.prototype.forEach.call(box.querySelectorAll('button'), function (b) {
      out.push({
        text: b.textContent,
        pressed: b.getAttribute('aria-pressed'),
        active: b.classList.contains('active')
      });
    });
    return out;
  }

  function activeChipText() {
    var box = node('group-filter');
    if (!box) return '';
    var btn = box.querySelector('button.active');
    return btn ? btn.textContent : '';
  }

  // 现在**看得见**的卡片名字（hidden 的卡片不占位置，等于被筛掉了）。
  function cardNames() {
    var grid = node('grid');
    var out = [];
    if (!grid) return out;
    Array.prototype.forEach.call(grid.children, function (card) {
      if (card.hidden) return;
      var name = card.querySelector('.card-name');
      out.push(name ? name.textContent : '?');
    });
    return out;
  }

  function clickChip(prefix) {
    var box = node('group-filter');
    if (!box) throw new Error('找不到分组筛选那一排 #group-filter');
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent.indexOf(prefix) === 0) { btns[i].click(); return true; }
    }
    throw new Error('chip 列表里没有以「' + prefix + '」开头的分组');
  }

  // 键盘路径：把焦点放到某一枚 chip 上再激活它（真实键盘按回车走的就是这条路），
  // 返回"激活之后焦点落在哪儿"的描述 —— 那一排是整块重画的，很容易把焦点丢掉。
  function activateChipByKeyboard(prefix) {
    var box = node('group-filter');
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent.indexOf(prefix) !== 0) continue;
      btns[i].focus();
      btns[i].click();
      var active = document.activeElement;
      if (!active) return '（没有 activeElement）';
      var pressed = active.getAttribute ? active.getAttribute('aria-pressed') : null;
      // 只取前 40 个字符：焦点掉到 <body> 上时 textContent 是整个页面。
      return active.tagName + '[' + (active.textContent || '').slice(0, 40) + '] pressed=' + pressed;
    }
    throw new Error('chip 列表里没有以「' + prefix + '」开头的分组');
  }

  function home() {
    return waitFor('首页四张卡片都画出来', function () {
      return shown('view-home') && node('grid') && node('grid').children.length === 4;
    }, 30000)
      .then(function () {
        return waitFor('分组 chip 就绪', function () { return chips().length >= 4; }, 15000);
      })
      .then(function () { return sleep(300); });
  }

  function login() {
    function loggedIn() {
      return shown('view-home') || shown('view-detail') || shown('view-settings');
    }
    return waitFor('出现登录视图', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', loggedIn, 30000);
      })
      .then(function () {
        // 刷新之后 hash 可能还停在 #/settings/nodes：拉回首页再继续。
        if (!shown('view-home')) {
          window.location.hash = '#/';
          return waitFor('回到首页', function () { return shown('view-home'); }, 15000);
        }
        return true;
      });
  }

  function goSettings() {
    window.location.hash = '#/settings/nodes';
    return waitFor('服务器列表就绪', function () {
      var list = node('nodes-list');
      return shown('view-settings') && list && list.children.length === 4;
    }, 30000).then(function () { return sleep(300); });
  }

  function handleInfo() {
    var handle = node('nodes-list').querySelector('.node-drag');
    if (!handle) throw new Error('服务器列表里找不到拖动把手');
    var cs = getComputedStyle(handle);
    return {
      cls: handle.className,
      title: handle.title,
      disabled: handle.getAttribute('aria-disabled'),
      cursor: cs.cursor,
      opacity: cs.opacity
    };
  }

  // synthDrag 合成一次真实的拖动：把最后一行拖到最上面（落点会变 → 顺序会变）。
  // 用 Pointer Events（与页面自己监听的是同一套），pointerId 固定 1。
  function synthDrag() {
    var rows = node('nodes-list').children;
    if (rows.length < 2) throw new Error('服务器列表里没有两行');
    var last = rows[rows.length - 1];
    var handle = last.querySelector('.node-drag');
    var hr = handle.getBoundingClientRect();
    var first = rows[0].getBoundingClientRect();
    var x = hr.left + hr.width / 2;
    var y = hr.top + hr.height / 2;
    var endY = first.top + 1;
    var common = { bubbles: true, cancelable: true, pointerId: 1, pointerType: 'mouse', isPrimary: true, clientX: x };
    handle.dispatchEvent(new PointerEvent('pointerdown', Object.assign({}, common, { button: 0, buttons: 1, clientY: y })));
    document.dispatchEvent(new PointerEvent('pointermove', Object.assign({}, common, { buttons: 1, clientY: endY })));
    document.dispatchEvent(new PointerEvent('pointerup', Object.assign({}, common, { button: 0, buttons: 0, clientY: endY })));
  }

  // restore 把刷新前那一遍的观测值搬回来：重载会新建一份 R，而第一遍量的东西
  // （chip 名单、总览条前后、被筛掉的卡片…）在第二遍里拿不到了。
  // 只搬"第一遍才有的"那些键，第二遍自己会重新赋值的不受影响。
  function restore() {
    var prev = null;
    try { prev = JSON.parse(sessionStorage.getItem('probe-group-result') || 'null'); } catch (e) { prev = null; }
    if (!prev) return;
    ['chips', 'total', 'summaryBefore', 'summaryAfter', 'afterPickChip', 'afterPickVisible',
      'ungroupedVisible', 'stored', 'keyboardChipFocus'].forEach(function (k) {
      if (prev[k] !== undefined) R[k] = prev[k];
    });
    // errs / steps 是两遍各自的记录，接在一起（第二遍的报错同样要报出来）。
    if (prev.errs && prev.errs.length) R.errs = prev.errs.concat(R.errs);
    if (prev.steps && prev.steps.length) R.steps = prev.steps.concat(R.steps);
    if (prev.fatal) R.fatal = prev.fatal;
  }

  // ---- 场景一：名单、台数、筛选、总览条、刷新保留 -------------------------
  function firstPass() {
    R.total = node('grid').children.length;
    R.chips = chips();
    R.summaryBefore = textOf('sum-online') + '/' + textOf('sum-total');
    clickChip('香港');
    return sleep(300).then(function () {
      R.afterPickChip = activeChipText();
      R.afterPickVisible = cardNames();
      R.summaryAfter = textOf('sum-online') + '/' + textOf('sum-total');
      try { R.stored = localStorage.getItem('probe-group-filter-v1') || ''; } catch (e) { R.stored = ''; }
      // 「未分组」那一组也要能筛：没填分组的机器不能在筛选里消失。
      clickChip('未分组');
      return sleep(300);
    }).then(function () {
      R.ungroupedVisible = cardNames();
      // 切回「香港」，刷新一次：验证选择是**存下来**的，而不是只在内存里。
      // 这一次走**键盘路径**（先聚焦再激活）：那一排是整块重画的，
      // 焦点要是丢了，键盘用户按一次回车就得重新 Tab 一遍。
      R.keyboardChipFocus = activateChipByKeyboard('香港');
      return sleep(200);
    }).then(function () {
      sessionStorage.setItem('probe-group-phase', '1');
      // 把这一遍的观测值交给第二遍（重载之后 R 是新的）。
      try { sessionStorage.setItem('probe-group-result', JSON.stringify(R)); } catch (e) { /* 存不下就少几条观测值 */ }
      // ★ 这一处是**唯一**还依赖 "load 已触发" 的地方：起跑不再等 load 之后，
      // "页面还在加载时调 reload 会打断这一次加载" 就成了一个真实的时序依赖
      // （5 个包 × 3 的满载下红过一次：等浏览器回传结果超时（3m0s），
      // Chrome 只有 GCM 噪声 —— 第二次加载没回传结果）。所以这里自己把
      // "这一次加载已经结束" 补回来：load 已经触发就直接走，否则等它。
      // 它**不是** waitFor（不产生步骤名、不影响任何判据/观测值），只是把
      // reload 放到一个确定的时刻上。
      var loaded = (document.readyState === 'complete')
        ? Promise.resolve()
        : new Promise(function (resolve) { window.addEventListener('load', function () { resolve(); }); });
      return loaded.then(function () {
        window.location.reload();
        // 这一遍**不回传结果**：页面马上要重载，回传的会是第一遍的观测值
        // （mock 只收第一条），第二遍就永远等不到了。返回一个永不 resolve 的
        // promise，让下面的 finish 不执行 —— 重载会自己把这条链带走。
        return new Promise(function () {});
      });
    });
  }

  // ---- 场景二：刷新之后接着看（选择保留 + 拖动闸门）-----------------------
  function secondPass() {
    R.debug = {
      phase: sessionStorage.getItem('probe-group-phase') || '',
      stored: localStorage.getItem('probe-group-filter-v1') || '<null>',
      chips: chips(),
      gridChildren: node('grid') ? node('grid').children.length : -1,
      hiddenFlags: (function () {
        var out = [];
        if (!node('grid')) return out;
        Array.prototype.forEach.call(node('grid').children, function (c) { out.push(c.hidden ? 'hidden' : 'shown'); });
        return out;
      })(),
      filterHidden: node('group-filter') ? node('group-filter').hidden : null
    };
    R.afterReloadChip = activeChipText();
    R.afterReloadVisible = cardNames();
    return goSettings().then(function () {
      R.lockedHandle = handleInfo();
      synthDrag();
      return sleep(800);
    }).then(function () {
      R.orderPutsLocked = orderPuts;
      // 切回「全部」再拖一次：这一次必须真的发 PUT。
      window.location.hash = '#/';
      return waitFor('回到首页', function () { return shown('view-home'); }, 15000);
    }).then(function () { return sleep(300); })
      .then(function () { clickChip('全部'); return sleep(200); })
      .then(goSettings)
      .then(function () {
        R.unlockedHandle = handleInfo();
        var before = orderPuts;
        synthDrag();
        return sleep(1000).then(function () { R.orderPutsUnlocked = orderPuts - before; });
      });
  }

  // ---- 场景三：窄屏 -------------------------------------------------------
  function narrowPass() {
    var doc = document.documentElement;
    R.narrow.viewport = doc.clientWidth;
    R.narrow.chips = chips().length;
    R.narrow.overflowBefore = doc.scrollWidth - doc.clientWidth;
    clickChip('香港');
    return sleep(300).then(function () {
      R.narrow.visibleCards = cardNames().length;
      R.narrow.overflowAfter = doc.scrollWidth - doc.clientWidth;
      var box = node('group-filter');
      R.narrow.filterScroll = box.scrollWidth - box.clientWidth;
      return true;
    });
  }

  // ---- 场景四：四条路由各走一遍，记下每一条上有没有 JS 报错 ----------------
  //
  // 分组筛选改的是首页那一块，但它是**共用的卡片与路由**的一部分：改坏了
  // 详情页/设置页往往一点征兆都没有（报错只落在控制台里）。四条路由是用户点得
  // 最多的那几条：首页 / 节点详情 / 服务器列表 / 告警参数。
  function currentView() {
    var views = ['view-home', 'view-detail', 'view-settings', 'view-login', 'view-setup'];
    for (var i = 0; i < views.length; i++) {
      if (shown(views[i])) {
        if (views[i] !== 'view-settings') return views[i];
        var pane = node('settings-panes').querySelector('.pane:not([hidden])');
        return 'view-settings:' + (pane ? pane.dataset.pane : '?');
      }
    }
    return '（没有可见视图）';
  }

  function routePass() {
    var routes = ['#/', '#/n/' + CFG.nodeID, '#/settings/nodes', '#/settings/alert'];
    var out = [];
    return routes.reduce(function (chain, hash) {
      return chain.then(function () {
        R.routeErrsBefore = R.errs.length;
        window.location.hash = hash;
        return sleep(1000);
      }).then(function () {
        out.push({ hash: hash, errs: R.errs.length - R.routeErrsBefore, view: currentView() });
        return true;
      });
    }, Promise.resolve()).then(function () { R.routes = out; return true; });
  }

  function run() {
    var scenario = CFG.scenario || 'filter';
    restore();
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(home)
      .then(function () {
        if (scenario === 'narrow') return narrowPass();
        var phase = '';
        try { phase = sessionStorage.getItem('probe-group-phase') || ''; } catch (e) { phase = ''; }
        return phase === '1' ? secondPass() : firstPass();
      })
      .then(function () { return routePass(); });
  }

  // shot 模式（人工截图用）：把首页开到指定状态就停住，不回传结果。
  //   group-all → 默认（全部）；group-hk → 选中「香港」；group-none → 选中「未分组」。
  function shot() {
    var mode = CFG.shot;
    return login().then(home).then(function () {
      if (mode === 'group-hk') clickChip('香港');
      else if (mode === 'group-none') clickChip('未分组');
      return sleep(400);
    }).then(function () {
      document.title = 'SHOT-READY:' + mode;
      return true;
    });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:group'; });
  }

  // 起跑**不再等 load 事件**（理由见 HarnessJS 里同一处注释：load 是在等"所有
  // 子资源就绪"这个未来事件，而自检第一步本来就是轮询 DOM）。
  if (CFG.shot) {
    // 截图模式：跑完就停（不回传结果 —— 截图靠 --virtual-time-budget + --screenshot）。
    shot().catch(function (err) {
      document.title = 'SHOT-FAILED:' + String(err && err.message ? err.message : err);
    });
  } else {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  }
})();`

// BillingHarnessJS 是「流量计费方式（双向 / 只算出站 / 只算入站）」那条用例注入页面
// 的自检脚本。
//
// 为什么必须真浏览器：这次改动的**全部可见效果**都在 DOM 与 computed style 里 ——
//
//	· 卡片上「流量」那一格的**标签文字**跟着口径换词（双向流量 / 出站流量 / 入站流量）；
//	· 那一格的数字与百分比是服务端按口径算的（只算出站时不能显示成收 + 发）；
//	· 进度条的档位（绿 / 橙 / 红）跟的是**重算后的**百分比；
//	· 详情页三行右边写着「合计 X · 计费 Y（Z%）· 额度 L」，每一行**正好三处**带
//	  .billed（单口径 ⇒ 计费方向那一段读数 + 计费 + 额度；双向 ⇒ 合计 + 计费 +
//	  额度，方向读数两段都不带），而"标色真的落到哪几段上"只有 computed style
//	  说了算 —— 静态断言只能证明 CSS 里写着 --accent，证明不了浏览器把哪一段匹配上了。
//
// 它顺带量两件静态断言量不了的事：
//  1. 拆分之后那一行的**实际宽度**（.traffic-cell 的 rect）—— gap 没补够就会窄 3.8px；
//  2. .traffic-dir 的 display 与 column-gap（拆分之后间隔必须来自 CSS，不是空格）。
//
// CFG 里用到的字段（都是 HarnessConfig 已有的，不新增契约）：
//
//	nodes  = 名字 → id（要逐台机器开详情页）
//	focus  = 这条用例主要看哪台机器（首页卡片读数与详情页读数都在它上面取）
const BillingHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var NODES = CFG.nodes || {};
  var rawFetch = window.fetch.bind(window);
  var R = { errs: [], steps: [], fatal: '', cards: [], detail: [], detailName: '', tokens: {},
            viewport: 0, docOverflowX: 0 };
  window.__BILLINGRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  function node(id) { return document.getElementById(id); }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function waitFor(what, cond, ms) {
    var deadline = Date.now() + (ms || 20000);
    return new Promise(function (resolve, reject) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { reject(new Error(what + ' 判定抛错: ' + e.message)); return; }
        if (ok) { R.steps.push(what); resolve(true); return; }
        if (Date.now() > deadline) { reject(new Error('等待超时: ' + what)); return; }
        setTimeout(poll, 50);
      })();
    });
  }

  function login() {
    function loggedIn() {
      return shown('view-home') || shown('view-detail') || shown('view-settings');
    }
    return waitFor('出现登录视图', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', loggedIn, 30000);
      });
  }

  // tokenColor 把一个 CSS 变量解析成**浏览器真正算出来的颜色**（rgb(...)）。
  //
  // 为什么不直接读变量里的字符串：那样只能证明 CSS 里写着 #2563eb，证明不了
  // 那一段文字真的被染成了它（选择器没匹配上、被别的规则盖掉，两种情况读变量
  // 都看不出来）。探针元素用完就摘掉，不留痕迹。
  function tokenColor(name) {
    var probe = document.createElement('span');
    probe.style.color = 'var(' + name + ')';
    document.body.appendChild(probe);
    var c = getComputedStyle(probe).color;
    document.body.removeChild(probe);
    return c;
  }

  // 首页那一格：CARD_RES 的第四格（quota）。标签、百分比、副值、进度条的 class 与
  // 它真正画出来的颜色都要取 —— 档位是靠 class 定的，而"绿/橙/红"最终是颜色。
  function readCard(name) {
    var cards = document.querySelectorAll('#grid > .card');
    for (var i = 0; i < cards.length; i++) {
      var nm = cards[i].querySelector('.card-name');
      if (!nm || nm.textContent !== name) continue;
      var cell = cards[i].querySelectorAll('.res-cell')[3];
      if (!cell) throw new Error(name + ' 的卡片上找不到流量格');
      var fill = cell.querySelector('.bar > i');
      var cs = getComputedStyle(fill);
      return {
        name: name,
        label: cell.querySelector('.res-label').textContent,
        pct: cell.querySelector('.res-pct').textContent,
        sub: cell.querySelector('.res-sub').textContent,
        barClass: fill.className,
        barColor: cs.backgroundColor,
        barWidth: fill.style.width
      };
    }
    throw new Error('首页上找不到卡片 ' + name);
  }

  // 详情页「流量信息」卡里那三行（今日 / 本周 / 本周期）。
  //
  // 每一行读：dt 的文案、方向读数**每一段**的文本 / class / 颜色 / 字重、
  // 右边「合计 X · 计费 Y（Z%）· 额度 L」**每一段**的文本 / class / 颜色 / 字重、
  // 那一整段的文本、以及这一行的实际宽度。
  //
  // 窄屏那几条读数（sumLines / sumTopDelta / ddClientWidth / ddScrollWidth /
  // cellHeight）：那三行右边现在写着「合计 · 计费 · 额度」三段，一行**明显变长**，
  // 窄屏会不会折行、会不会横向溢出，只有真几何说了算 ——
  //   · sumLines > 1 ⇔ 合计那一段被拆到了两行（用 Range 跨过它的内容取矩形个数）；
  //   · sumTopDelta 是合计与方向读数的顶边之差：折行后"合计"整段掉到下一行，
  //     这个值会接近一整行的高度；
  //   · ddScrollWidth > ddClientWidth ⇔ 值那一格横向溢出。
  //
  // ⚠️ 这里原来读的是 sum.getClientRects().length，那个数**恒为 1**：.traffic-sum
  // 是 .traffic-cell（inline-flex）的 flex item，按规格会被**块化**（display 由
  // inline 变成 block，同一棵树里的 .traffic-dir 就被量到 "flex"），而块盒的
  // getClientRects() 永远只有一个矩形 —— 于是那条"折行了没有"的断言是空的。
  // 现在用**高度 ÷ 行高**数行数（块盒的高度 = 行数 × 行高）。
  // 也不能用 Range.getClientRects()：Chrome 对一个跨多个子元素的 Range 是按
  // 元素/文本节点给矩形的，三个子 span 会量出 6 个"行"，与折行无关。
  function readDetail() {
    var rows = document.querySelectorAll('#info-traffic .traffic-cell');
    var out = [];
    Array.prototype.forEach.call(rows, function (cell) {
      var dd = cell.parentNode;
      var dt = dd && dd.previousElementSibling;
      var dir = cell.querySelector('.traffic-dir');
      var segs = [];
      var segWidth = 0;
      Array.prototype.forEach.call(dir.querySelectorAll('.traffic-dir-seg'), function (seg) {
        var cs = getComputedStyle(seg);
        segWidth += seg.getBoundingClientRect().width;
        segs.push({
          text: seg.textContent,
          cls: seg.className,
          color: cs.color,
          weight: cs.fontWeight
        });
      });
      var sum = cell.querySelector('.traffic-sum');
      // 右边那一段的**每一段**：合计 / 计费 / 额度各自的 class、颜色与字重。
      // 染色落在哪一段上是这一轮的重点（单口径 ⇒ 方向读数 + 计费 + 额度；双向 ⇒
      // 合计 + 计费 + 额度），而 class 与 computed style 是唯一说得清的读数。
      var sumSegs = [];
      if (sum) {
        Array.prototype.forEach.call(sum.querySelectorAll('.traffic-sum-seg'), function (seg) {
          var scs = getComputedStyle(seg);
          sumSegs.push({
            text: seg.textContent,
            cls: seg.className,
            color: scs.color,
            weight: scs.fontWeight
          });
        });
      }
      var dcs = getComputedStyle(dir);
      var dirW = dir.getBoundingClientRect().width;
      var dirRect = dir.getBoundingClientRect();
      var sumRect = sum ? sum.getBoundingClientRect() : null;
      var sumLines = 0;
      if (sum) {
        var sumLH = parseFloat(getComputedStyle(sum).lineHeight);
        if (sumLH > 0) sumLines = Math.round(sumRect.height / sumLH);
      }
      out.push({
        label: dt ? dt.textContent : '',
        segs: segs,
        sumSegs: sumSegs,
        sum: sum ? sum.textContent : '',
        cellWidth: cell.getBoundingClientRect().width,
        cellHeight: cell.getBoundingClientRect().height,
        dirWidth: dirW,
        // gapFromGeometry 是"方向读数那一段的宽 − 两个子 span 的宽"：它把 CSS 的
        // gap 换算成**真实几何**。拆分前那两个空格里实际只有一个生效（3.836px），
        // 补不回来时这个数就会掉到 0 附近，而静态断言看不出来。
        gapFromGeometry: Math.round((dirW - segWidth) * 1000) / 1000,
        dirDisplay: dcs.display,
        dirGap: dcs.columnGap || dcs.gap || '',
        sumWidth: sumRect ? sumRect.width : 0,
        // 折行判据（见函数注释）：合计那一段占了几行、与方向读数的顶边差。
        sumRects: sum ? sum.getClientRects().length : 0,
        sumLines: sumLines,
        sumTopDelta: sumRect ? Math.abs(sumRect.top - dirRect.top) : 0,
        ddWidth: dd ? dd.getBoundingClientRect().width : 0,
        ddClientWidth: dd ? dd.clientWidth : 0,
        ddScrollWidth: dd ? dd.scrollWidth : 0
      });
    });
    return out;
  }

  function home() {
    window.location.hash = '#/';
    return waitFor('首页卡片就绪', function () {
      return document.querySelectorAll('#grid > .card').length >= Object.keys(NODES).length;
    }, 20000).then(function () { return sleep(600); });
  }

  function detailOf(name) {
    var id = NODES[name];
    if (!id) throw new Error('CFG.nodes 里没有 ' + name);
    window.location.hash = '#/n/' + id;
    return waitFor('详情页就绪 ' + name, function () {
      return shown('view-detail') && document.querySelectorAll('#info-traffic .traffic-cell').length === 3 &&
        document.querySelectorAll('#info-traffic .traffic-dir-seg').length === 6;
    }, 20000).then(function () { return sleep(400); });
  }

  function run() {
    R.detailName = CFG.focus || '';
    var order = Object.keys(NODES);
    if (CFG.scenario === 'billing-narrow') {
      // 窄屏那一趟：把**每一台**机器的详情页都开一遍，把三行的几何读回来。
      // 视口宽度由 Go 侧用 CDP 的 Emulation.setDeviceMetricsOverride 定死成 760。
      //
      // 为什么不是只开 CFG.focus 那一台：那一行的宽度是**内容**撑出来的，而三台
      // 机器的数值与额度都不一样（额度补到末尾之后"谁最宽"会变）—— 只量一台就
      // 不足以回答"窄屏放不放得下"这个问题。图仍然拍 CFG.focus 那一台（见下）。
      return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
        .then(login)
        .then(function () { R.viewport = document.documentElement.clientWidth; return true; })
        .then(function () {
          return order.reduce(function (chain, name) {
            return chain.then(function () {
              return detailOf(name).then(function () {
                R.detail.push({ name: name, rows: readDetail() });
              });
            });
          }, Promise.resolve());
        })
        .then(function () {
          // 截图拍的是"当前这一页"：转回 CFG.focus 那一台，别让图里的机器取决于
          // 名字的排序（Map 的键序在 Go 侧是排过序的）。
          if (CFG.focus && NODES[CFG.focus]) {
            R.detailName = CFG.focus;
            return detailOf(CFG.focus);
          }
          return true;
        })
        .then(function () {
          R.docOverflowX =
            document.documentElement.scrollWidth - document.documentElement.clientWidth;
          R.viewport = document.documentElement.clientWidth;
          return true;
        });
    }
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(home)
      .then(function () {
        // 三台机器的卡片读数（标签 / 数字 / 百分比 / 档位）一次读齐。
        R.cards = order.map(readCard);
        // 逐台开详情页：三行读数与标色都在那上面。
        return order.reduce(function (chain, name) {
          return chain.then(function () {
            return detailOf(name).then(function () {
              R.detail.push({ name: name, rows: readDetail() });
            });
          });
        }, Promise.resolve());
      })
      .then(function () {
        // 颜色令牌最后读：断言"标色用的是 --accent、不是 --ok/--warn/--bad"要的是
        // 浏览器算出来的真值，而不是 CSS 文件里那串字面量。
        R.tokens = {
          accent: tokenColor('--accent'),
          ok: tokenColor('--ok'),
          warn: tokenColor('--warn'),
          bad: tokenColor('--bad')
        };
        R.viewport = document.documentElement.clientWidth;
        R.docOverflowX =
          document.documentElement.scrollWidth - document.documentElement.clientWidth;
      });
  }

  // 截图模式：登录 + 开到 CFG.shot 指定的那一路（"#/" 或 "#/n/<id>"）就停住。
  // 不回传结果 —— 截图由 CDP 侧轮询页面状态决定什么时候按快门。
  function shot() {
    var hash = CFG.shot;
    return login().then(function () {
      window.location.hash = hash || '#/';
      return sleep(1500);
    }).then(function () {
      document.title = 'SHOT-READY:' + hash;
      return true;
    });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:billing'; });
  }

  // 起跑**不再等 load 事件**（理由见 HarnessJS 里同一处注释：load 是在等"所有
  // 子资源就绪"这个未来事件，而自检第一步本来就是轮询 DOM）。
  if (CFG.shot) {
    shot().catch(function (err) {
      document.title = 'SHOT-FAILED:' + String(err && err.message ? err.message : err);
    });
  } else {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  }
})();`
