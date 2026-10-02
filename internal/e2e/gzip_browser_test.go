package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 真浏览器 + 真服务端验证 gzip：既要证明"页面能渲染"（解压后的 JS/CSS/JSON 都对），
// 又要证明"SSE 没被压死"。
//
// 为什么必须用真 Chrome：压死的表现是"连接活着、一个字节不到"，静态断言与
// 单元测试都只能覆盖到自己那一层；而"页面上的实时数值到底有没有在动"只有真
// 浏览器 + 真服务端 + 真 Agent 这一条路能证明。数值不动的判据是**采样**：
// 在每条路由上对「更新于」采三次样，三次必须不同 —— 它同时证明了两件事：
// 页面渲染出来了，而且 SSE 每一秒都真的推到了浏览器。
//
// 不用 --dump-dom --virtual-time-budget：页面挂着 SSE 长连接，虚拟时间会一直
// 暂停（与仓库里其它浏览器用例同一个理由）。

// gzipRouteResult 是一条路由上的观测值（断言全在 Go 这边做）。
type gzipRouteResult struct {
	Path string `json:"path"`
	// View 是这条路由应当停住的视图 id；Pane 是设置页里应当高亮的那一栏。
	View string `json:"view"`
	Pane string `json:"pane"`
	// Errs 是这条路由期间新增的 JS 报错。
	Errs []string `json:"errs"`
	// Samples 是「更新于」的三次采样。
	Samples []string `json:"samples"`
	// SSE 是这条路由期间收到的 nodes 事件数。
	SSE int  `json:"sse"`
	OK  bool `json:"ok"`
}

type gzipBrowserResult struct {
	Errs      []string          `json:"errs"`
	Fatal     string            `json:"fatal"`
	Routes    []gzipRouteResult `json:"routes"`
	SSEEvents int               `json:"sseEvents"`
}

// updatedClock 是「更新于 HH:MM:SS」的形状（fmtClock 在服务端时区下渲染）。
var updatedClock = regexp.MustCompile(`^更新于 [0-9]{2}:[0-9]{2}:[0-9]{2}$`)

func TestGzipInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerWithLogger(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), slog.New(logs))
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, token := createNodeViaAPI(t, br, "gz-browser")

	// 真 Agent：SSE 每秒都有新数据，页面上的「更新于」才会一秒一跳。
	client, _ := newClient(t, "http://"+h.addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()
	waitFor(t, 20*time.Second, "Agent 上报第一帧", func() bool {
		n, ok := h.srv.State().Get(nodeID)
		return ok && n.Seq >= 1
	})

	// 反代 + 只改首页（与仓库里其它浏览器用例同一套做法）：接口一条都不 mock，
	// 浏览器拿到的仍是真服务端的真数据，压缩也发生在真服务端那一侧。
	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		NodeID:   nodeID,
		NodeName: "gz-browser",
		User:     "admin",
		Pass:     "a-very-good-password",
	}, gzipHarnessJS)

	// 记下真服务端给每条路径发的 Content-Encoding：这就是"浏览器真的收到了
	// gzip 字节"的证据（浏览器自己会解开，页面上看不见）。顺带记 Content-Length
	// 与状态码，好让"这条为什么没压"在日志里一眼看得出（多半是小于 512 字节）。
	var encMu sync.Mutex
	encodings := map[string]string{}
	details := map[string]string{}
	proxy.proxy.ModifyResponse = func(resp *http.Response) error {
		encMu.Lock()
		encodings[resp.Request.URL.Path] = resp.Header.Get("Content-Encoding")
		details[resp.Request.URL.Path] = strconv.Itoa(resp.StatusCode) + " CL=" + resp.Header.Get("Content-Length")
		encMu.Unlock()
		return nil
	}

	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 120*time.Second, "")

	var res gzipBrowserResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	if len(res.Routes) != 4 {
		t.Fatalf("只走完了 %d 条路由，期望 4 条：%+v", len(res.Routes), res.Routes)
	}

	want := []struct{ path, view, pane string }{
		{"#/", "view-home", ""},
		{"#/n/" + itoa(nodeID), "view-detail", ""},
		{"#/settings/nodes", "view-settings", "nodes"},
		{"#/settings/alert", "view-settings", "alert"},
	}
	for i, w := range want {
		got := res.Routes[i]
		if got.Path != w.path || got.View != w.view {
			t.Errorf("第 %d 条路由 = %s/%s，期望 %s/%s", i+1, got.Path, got.View, w.path, w.view)
			continue
		}
		if !got.OK {
			t.Errorf("%s：视图没就绪（自检脚本提前退出）", w.path)
			continue
		}
		if len(got.Errs) != 0 {
			t.Errorf("%s 上有 %d 条 JS 报错：%v", w.path, len(got.Errs), got.Errs)
		}
		if w.pane != "" && got.Pane != w.pane {
			t.Errorf("%s 停在设置页的 %q 栏，期望 %q", w.path, got.Pane, w.pane)
		}
		// 「更新于」三次采样必须互不相同 —— 这条同时证明页面渲染出来了、
		// 而且 SSE 一秒都没停（压死的页面这里会一直是一模一样的值）。
		if len(got.Samples) != 3 {
			t.Errorf("%s 的「更新于」只采到 %d 次", w.path, len(got.Samples))
			continue
		}
		seen := map[string]bool{}
		for _, s := range got.Samples {
			if !updatedClock.MatchString(s) {
				t.Errorf("%s 的「更新于」= %q，形状不对（期望「更新于 HH:MM:SS」）", w.path, s)
			}
			seen[s] = true
		}
		if len(seen) != 3 {
			t.Errorf("%s 的「更新于」三次采样没有变化：%v（页面卡住或 SSE 被压死）", w.path, got.Samples)
		}
		if got.SSE < 2 {
			t.Errorf("%s 期间只收到 %d 条 SSE 事件（这条路由上花了约 3 秒，1 Hz 至少该有 2 条）", w.path, got.SSE)
		}
		t.Logf("%s：视图 %s，期间 SSE %d 条，「更新于」%v", w.path, w.view, got.SSE, got.Samples)
	}
	if res.SSEEvents < 8 {
		t.Errorf("整轮只收到 %d 条 SSE 事件，太少", res.SSEEvents)
	}

	encMu.Lock()
	defer encMu.Unlock()
	for _, path := range []string{"/app.js", "/chart.js", "/style.css", "/api/v1/nodes", "/api/v1/nodes/" + itoa(nodeID), "/api/v1/stream"} {
		if encodings[path] != "gzip" {
			t.Errorf("浏览器请求 %s 时真服务端发的是 %q，期望 gzip", path, encodings[path])
		}
	}
	t.Logf("浏览器侧各路径的真服务端 Content-Encoding：%v", encodings)
	t.Logf("同一批路径的状态码与明文 Content-Length（没有 CL 就是被压成 chunked 了）：%v", details)
	t.Logf("整轮观测：JS 报错 %d 条，SSE 事件 %d 条，四条路由的「更新于」都在动",
		len(res.Errs), res.SSEEvents)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// gzipHarnessJS 是注入到页面里的自检脚本：登录 → 四条路由各停 3 秒采样。
//
// 断言一句都不在这里做：它只负责把"看到了什么"回传（期望值都在 Go 那边），
// 与仓库里其它浏览器用例同一个分工。
const gzipHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = { errs: [], fatal: '', routes: [], sseEvents: 0 };
  window.__GZRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // 数 SSE：这条长连接就是"页面看上去卡死"的那条命脉。
  var OrigES = window.EventSource;
  window.EventSource = function (url, cfg) {
    var es = new OrigES(url, cfg);
    es.addEventListener('nodes', function () { R.sseEvents++; });
    return es;
  };
  window.EventSource.prototype = OrigES.prototype;

  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function paneShown(name) {
    if (!name) return true;
    var sec = document.querySelector('#view-settings .pane[data-pane="' + name + '"]');
    return !!sec && !sec.hidden;
  }
  function waitFor(what, cond, ms) {
    var deadline = Date.now() + (ms || 20000);
    return new Promise(function (resolve, reject) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { reject(new Error(what + ' 判定抛错: ' + e.message)); return; }
        if (ok) { resolve(true); return; }
        if (Date.now() > deadline) { reject(new Error('等待超时: ' + what)); return; }
        setTimeout(poll, 50);
      })();
    });
  }

  function loggedIn() { return shown('view-home') || shown('view-detail') || shown('view-settings'); }
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

  // 一条路由：等它真的停在该视图（设置页还要停在该栏）上，然后对「更新于」
  // 采样三次（间隔 1.4 秒 —— SSE 是 1 Hz，三次必然落在不同的秒上）。
  function visit(path, view, pane, extra) {
    var rec = { path: path, view: view, pane: '', errs: [], samples: [], sse: 0, ok: false };
    var errsBefore = R.errs.length;
    var sseBefore = R.sseEvents;
    window.location.hash = path;
    return waitFor(path + ' 视图就绪', function () {
      return shown(view) && paneShown(pane) && (!extra || extra());
    }, 30000).then(function () {
      rec.pane = pane || '';
      return sleep(300);
    }).then(function () {
      rec.samples.push(textOf('updated'));
      return sleep(1400);
    }).then(function () {
      rec.samples.push(textOf('updated'));
      return sleep(1400);
    }).then(function () {
      rec.samples.push(textOf('updated'));
      rec.errs = R.errs.slice(errsBefore);
      rec.sse = R.sseEvents - sseBefore;
      rec.ok = true;
      R.routes.push(rec);
      return true;
    });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(login)
      .then(function () { return visit('#/', 'view-home', '', null); })
      .then(function () {
        return visit('#/n/' + CFG.nodeID, 'view-detail', '', function () {
          return textOf('detail-name') === CFG.nodeName;
        });
      })
      .then(function () { return visit('#/settings/nodes', 'view-settings', 'nodes', null); })
      .then(function () { return visit('#/settings/alert', 'view-settings', 'alert', null); });
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

  window.addEventListener('load', function () {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`
