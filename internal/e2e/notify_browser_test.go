package e2e

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 「通知」栏里三个定时流量报告开关的真浏览器验证。
//
// 为什么必须用真浏览器（而不是只静态断言 HTML/JS 里有没有那几行）：
//   - "勾上 → 保存 → 离开设置页 → 再进来还是勾着的"这条链路上，任何一环断了
//     （字段名对不上、响应没回填、切栏时不重新拉数据）页面都照常渲染，
//     静态断言一条都不会红；
//   - 三个开关共用一次 PUT，很容易出现"我一个，顺手把另外两个也写回去"的
//     串台 bug —— 只有连着看几轮勾选状态才分得出来。
//
// 做法与仓库里其它浏览器用例完全一致：真服务端 + 反代注入自检脚本
// + --headless=new + 结果 POST 回 mock。**不用** --dump-dom --virtual-time-budget：
// 页面挂着 SSE 长连接，虚拟时间会被一直暂停。
const notifyHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], steps: [], fatal: '', scenarioErrs: {}, rounds: {},
    exists: {}, labels: {}, hashRoutes: [], serverTZ: ''
  };
  window.__NOTIFYRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

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

  // 每个场景各自记一次"这段里新增了几条 JS 报错"：合起来看总数会掩盖
  // "前一个场景的错被后面的场景背了锅"。
  var errBaseline = 0;
  function endScenario(name) {
    R.scenarioErrs[name] = R.errs.length - errBaseline;
    errBaseline = R.errs.length;
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

  function switches() {
    return {
      daily: !!node('notify-daily').checked,
      weekly: !!node('notify-weekly').checked,
      monthly: !!node('notify-monthly').checked
    };
  }
  function setSwitches(want) {
    node('notify-daily').checked = !!want.daily;
    node('notify-weekly').checked = !!want.weekly;
    node('notify-monthly').checked = !!want.monthly;
  }
  // 设置页把服务端值回填完的标志：三个开关与 server-info 是同一个 Promise.all
  // 里填的，后者有内容就说明那一帧已经渲染过了。
  function settingsLoaded() {
    return !!node('server-info') && node('server-info').children.length > 0;
  }
  function openNotify() {
    window.location.hash = '#/settings/notify';
    return waitFor('设置页打开', function () { return shown('view-settings'); }, 30000)
      .then(function () {
        return waitFor('「通知」栏可见', function () {
          var pane = node('notify-daily').closest('section');
          return !!pane && !pane.hidden;
        }, 15000);
      })
      .then(function () { return waitFor('「通知」栏已按服务端回填', settingsLoaded, 15000); });
  }
  function leaveSettings() {
    window.location.hash = '#/';
    return waitFor('离开设置页', function () { return shown('view-home') && !shown('view-settings'); }, 15000);
  }
  function save() {
    node('notify-save').click();
    return waitFor('保存完成', function () {
      return textOf('notify-ok') === '已保存' && textOf('notify-error') === '';
    }, 20000);
  }
  function visit(hash, view, pane, what) {
    window.location.hash = hash;
    return waitFor(what + ' 打开', function () {
      if (!shown(view)) return false;
      if (view === 'view-detail' && textOf('detail-name') !== CFG.nodeName) return false;
      if (pane) {
        var sec = node('settings-panes').querySelector('section[data-pane="' + pane + '"]');
        if (!sec || sec.hidden) return false;
      }
      return true;
    }, 30000).then(function () { return sleep(400); });
  }

  function labels() {
    ['notify-daily', 'notify-weekly', 'notify-monthly'].forEach(function (id) {
      R.exists[id] = !!node(id);
      var el = node(id);
      R.labels[id] = el && el.parentNode ? el.parentNode.textContent.trim() : '';
    });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(login)
      .then(function () { endScenario('登录'); })
      .then(openNotify)
      .then(function () {
        labels();
        R.rounds.initial = switches();
        // 服务端时区在「服务器信息」栏的只读网格里：每一项是一格 .kv-cell
        // （小标签 .kv-k + 值 .kv-v），不再是 <dl> 里成对的 dt/dd。
        var info = node('server-info');
        if (info) {
          var cells = info.querySelectorAll('.kv-cell');
          for (var i = 0; i < cells.length; i++) {
            var k = cells[i].querySelector('.kv-k');
            if (!k || k.textContent !== '时区') continue;
            var v = cells[i].querySelector('.kv-v');
            R.serverTZ = v ? v.textContent : '';
          }
        }
        endScenario('打开通知栏');
      })
      // 场景 1：只打开「日报」，保存 → 离开设置页 → 重新打开。
      // 另外两个必须**保持关着**（串台的话这里就会亮）。
      .then(function () { setSwitches({ daily: true, weekly: false, monthly: false }); return save(); })
      .then(leaveSettings)
      .then(openNotify)
      .then(function () {
        R.rounds.afterDaily = switches();
        endScenario('打开日报并重新进入');
      })
      // 场景 2：三个都打开，重新进入后必须都还在。
      .then(function () { setSwitches({ daily: true, weekly: true, monthly: true }); return save(); })
      .then(leaveSettings)
      .then(openNotify)
      .then(function () {
        R.rounds.afterAll = switches();
        endScenario('三个都打开并重新进入');
      })
      // 场景 3：只关掉「日报」，另外两个不许被顺手关掉。
      .then(function () { setSwitches({ daily: false, weekly: true, monthly: true }); return save(); })
      .then(leaveSettings)
      .then(openNotify)
      .then(function () {
        R.rounds.afterOff = switches();
        endScenario('关掉日报并重新进入');
      })
      // 回归：四个既有入口照旧能打开，且各自没有新增 JS 报错。
      .then(function () { return visit('#/', 'view-home', '', '首页'); })
      .then(function () { endScenario('回归 #/'); })
      .then(function () { return visit('#/n/' + CFG.nodeID, 'view-detail', '', '详情页'); })
      .then(function () { endScenario('回归 #/n/<id>'); })
      .then(function () { return visit('#/settings/nodes', 'view-settings', 'nodes', '服务器列表栏'); })
      .then(function () { endScenario('回归 #/settings/nodes'); })
      .then(function () { return visit('#/settings/alert', 'view-settings', 'alert', '告警参数栏'); })
      .then(function () {
        R.hashRoutes.push({ hash: '#/', view: 'view-home' });
        R.hashRoutes.push({ hash: '#/n/' + CFG.nodeID, view: 'view-detail' });
        R.hashRoutes.push({ hash: '#/settings/nodes', view: 'view-settings' });
        R.hashRoutes.push({ hash: '#/settings/alert', view: 'view-settings' });
        endScenario('回归 #/settings/alert');
      });
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
    if (CFG.shot) { shot(); return; }
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });

  // shot 模式：只把界面开到「通知」栏（给人截图核对用），不跑完整流程。
  function shot() {
    login().then(openNotify).then(function () {
      labels();
      R.rounds.initial = switches();
      document.title = 'SHOT-READY:notify';
    }).catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    });
  }
})();`

// notifyBrowserResult 是自检脚本回传的全部观测值。
type notifyBrowserResult struct {
	Errs         []string                         `json:"errs"`
	Fatal        string                           `json:"fatal"`
	Steps        []string                         `json:"steps"`
	ScenarioErrs map[string]int                   `json:"scenarioErrs"`
	Exists       map[string]bool                  `json:"exists"`
	Labels       map[string]string                `json:"labels"`
	Rounds       map[string]notifyBrowserSwitches `json:"rounds"`
	HashRoutes   []notifyBrowserRoute             `json:"hashRoutes"`
	ServerTZ     string                           `json:"serverTZ"`
}

type notifyBrowserSwitches struct {
	Daily   bool `json:"daily"`
	Weekly  bool `json:"weekly"`
	Monthly bool `json:"monthly"`
}

type notifyBrowserRoute struct {
	Hash string `json:"hash"`
	View string `json:"view"`
}

func TestTrafficNotifySettingsInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "notify-01")

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		NodeID:   nodeID,
		NodeName: "notify-01",
		User:     "admin",
		Pass:     "a-very-good-password",
	}, notifyHarnessJS)
	mock := newMockServer(t, proxy)

	raw := runChromeForResult(t, chrome, mock.URL+"/#/settings/notify", proxy.result, 150*time.Second, "")
	var res notifyBrowserResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}

	// 每个场景都必须 errs=[]：白屏、绑定失败、null 取属性全都落在 window.onerror
	// 与 unhandledrejection 上。
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	for name, n := range res.ScenarioErrs {
		if n != 0 {
			t.Errorf("场景「%s」里有 %d 条 JS 报错", name, n)
		}
	}
	t.Logf("浏览器时区下的服务端时区（--timezone）= %q；完成的步骤：%v", res.ServerTZ, res.Steps)
	t.Logf("各场景 JS 报错数：%v", res.ScenarioErrs)

	// 三个开关存在，而且各自写着触发时刻（用户要能看出"几点发、发的是哪一段"）。
	for _, id := range []string{"notify-daily", "notify-weekly", "notify-monthly"} {
		if !res.Exists[id] {
			t.Errorf("设置页「通知」栏里没有 %s", id)
			continue
		}
		label := res.Labels[id]
		t.Logf("%s 的说明文字 = %q", id, label)
		for _, need := range []string{"09:00", "流量"} {
			if !strings.Contains(label, need) {
				t.Errorf("%s 的说明里缺少 %q：%q", id, need, label)
			}
		}
	}

	// 场景 1：只打开日报 → 重新进入后只有日报是开的。
	afterDaily, ok := res.Rounds["afterDaily"]
	if !ok {
		t.Fatalf("没有拿到「打开日报后重新进入」那一轮的状态：%v", res.Rounds)
	}
	if !afterDaily.Daily {
		t.Error("打开日报并保存后，重新进入设置页时它又变回关的了（没有持久化）")
	}
	if afterDaily.Weekly || afterDaily.Monthly {
		t.Errorf("只打开了日报，另外两个却被顺手改掉了：%+v", afterDaily)
	}

	// 场景 2：三个都打开 → 重新进入后必须都还在。
	afterAll := res.Rounds["afterAll"]
	if !afterAll.Daily || !afterAll.Weekly || !afterAll.Monthly {
		t.Errorf("三个开关都保存后重新进入，实际状态 = %+v", afterAll)
	}

	// 场景 3：只关掉日报 → 它必须是关的，另外两个不受影响。
	afterOff := res.Rounds["afterOff"]
	if afterOff.Daily {
		t.Error("关掉日报并保存后，重新进入设置页时它又是开着的了")
	}
	if !afterOff.Weekly || !afterOff.Monthly {
		t.Errorf("关掉日报时把另外两个也关掉了：%+v", afterOff)
	}

	// 回归的四个入口都真的打开过（而不是被静默跳过）。
	wantRoutes := []struct{ hash, view string }{
		{"#/", "view-home"},
		{"#/n/1", "view-detail"},
		{"#/settings/nodes", "view-settings"},
		{"#/settings/alert", "view-settings"},
	}
	if len(res.HashRoutes) != len(wantRoutes) {
		t.Fatalf("回归路由只跑了 %d 个：%+v", len(res.HashRoutes), res.HashRoutes)
	}
	for i, want := range wantRoutes {
		if res.HashRoutes[i].Hash != want.hash || res.HashRoutes[i].View != want.view {
			t.Errorf("第 %d 个回归入口 = %+v，期望 %s → %s", i+1, res.HashRoutes[i], want.hash, want.view)
		}
	}

	// 浏览器之外再核一遍：库里的值就是最后那一轮（日报关、周报月报开）。
	status, body := br.do(http.MethodGet, "/api/v1/settings/telegram", nil, false)
	if status != http.StatusOK {
		t.Fatalf("读取通知设置失败: %d", status)
	}
	if body["daily_report"] != false {
		t.Errorf("/api/v1/settings/telegram 里 daily_report = %v，期望 false", body["daily_report"])
	}
	if body["weekly_report"] != true || body["monthly_report"] != true {
		t.Errorf("周报/月报应当是开着的，实际 weekly=%v monthly=%v",
			body["weekly_report"], body["monthly_report"])
	}
	t.Logf("服务端侧复核：GET /api/v1/settings/telegram → daily=%v weekly=%v monthly=%v",
		body["daily_report"], body["weekly_report"], body["monthly_report"])
}

// TestTrafficNotifySettingsScreenshot 产出「通知」栏的人工核对截图（默认跳过）。
//
//	$env:PROBE_SHOT_DIR = "D:\shots"; go test ./internal/e2e/ -run TestTrafficNotifySettingsScreenshot -v
//
// 截图前先把三个开关真的 PUT 进库，所以图上那三个勾是页面**从服务端读回来**的
// —— 不是脚本点上去的，也不是注入出来的。
func TestTrafficNotifySettingsScreenshot(t *testing.T) {
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

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "shot-01")

	if status, body := br.do(http.MethodPut, "/api/v1/settings/telegram", map[string]any{
		"enabled": false, "chat_id": "", "bot_token": "",
		"daily_report": true, "weekly_report": true, "monthly_report": true,
	}, true); status != http.StatusOK {
		t.Fatalf("预置三个开关失败: %d %v", status, body)
	}

	cfg := tzHarnessConfig{
		NodeID: nodeID, NodeName: "shot-01",
		User: "admin", Pass: "a-very-good-password",
		Shot: "notify",
	}
	// 注入的是本用例自己的自检脚本（newShotProxy 那份是跨时区用例的，
	// 它的 shot 模式只认 audit / chart / dialog，会把页面开到详情页去）。
	mock := newMockServer(t, newHarnessProxy(t, "http://"+h.addr, cfg, notifyHarnessJS))
	out := filepath.Join(outDir, "notify.png")

	// 与仓库里其它截图用例同一套参数：--virtual-time-budget 只在截图这条路径上用
	// （自动化断言那条路径故意不用它 —— SSE 长连接会让虚拟时间暂停）。
	args := []string{
		"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
		"--hide-scrollbars",
		"--user-data-dir=" + t.TempDir(),
		"--window-size=1500,1100",
		"--virtual-time-budget=60000",
		"--screenshot=" + out,
		mock.URL + "/#/settings/notify",
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
	case <-time.After(90 * time.Second):
		killChrome(cmd)
		t.Fatalf("Chrome 超时。输出尾部：\n%s", tail(buf.String(), 800))
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("截图没有生成: %v\nChrome 输出尾部：\n%s", err, tail(buf.String(), 800))
	}
	t.Logf("notify → %s（%d 字节）", out, st.Size())
}
