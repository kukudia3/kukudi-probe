package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 实时通道的**身份**在四种"不在同一条直路上"的情形下到底会怎样 —— 真浏览器验收。
//
// 背景：v1.4.1 修了「访客只读面板里登录之后，详情页的『本机地址 / 来源 IP』闪一下
// 就消失」（见 _audit/SUBFIX-GUEST-LOGIN-STREAM.md）。修法是 web/app.js 里新增
// `sourceAuthed`（记下**建流时**的身份）+ `ensureStream()`（身份与建流时不一致就先
// 停流再重建），route() 两处判据换成它。修复者自己列了四个"只有代码推演、没有端到端
// 证据"的缺口，这个文件就是给那四条补证据（**不改产品代码**）：
//
//	① 会话被撤销后的降级：服务端仍开着访客查看时，EventSource 可能自己重连、并被
//	   按访客身份接受。`sourceAuthed` 是前端**自己记的标志**，它看不出"浏览器悄悄
//	   换了一条身份不同的连接"。
//	   → TestRevokedSessionSilentlySwitchesStreamToGuest
//	② 跨标签页登录：A 标签是访客态、B 标签登录成管理员，A 的 30 秒复查把身份刷成
//	   管理员之后应当重建实时流。
//	   → TestGuestTabFollowsLoginFromAnotherTab
//	③ 访客 + 两步验证登录：2FA 与密码登录共用 enterApp()，但没有单独验过
//	   "访客态下走 2FA 登录"这条路。
//	   → TestGuestTwoFactorLoginRebuildsStream
//	④ 切后台期间发生身份变化：visibilitychange 那条路上只有代码推演。
//	   → TestIdentityChangeWhilePageHidden
//
// 为什么每条都必须真浏览器 + 真 Agent（做法与仓库里其它浏览器用例完全一样：真服务端
// 在随机端口 + 反代注入自检脚本 + 结果 POST 回 mock + --headless=new，**不用**
// --dump-dom --virtual-time-budget）：
//   - 断言的是"用户看得见的那几行字在不在"（详情页的行标签、卡片的 title、状态栏
//     文案、顶栏有哪些入口），只有真浏览器说得清；
//   - 帧必须**真的在推**：没有 Agent 每秒上报时，服务端的变更集里根本没有这台机器
//     （internal/server/api_stream.go 的 seq 判据），"脱敏帧把管理员那两行覆盖掉"
//     这件事永远不会发生，断言就会变成空的。
//
// ⚠️ 一个必须遵守的坑（修复者踩过，见 guest_browser_test.go 里同一个仪表的注释）：
// 包装 window.EventSource 时，CONNECTING / OPEN / CLOSED 三个常量**必须一起搬过去**。
// 只换构造函数的话 `EventSource.CLOSED` 变成 undefined，而 app.js 正是用
// `es.readyState === EventSource.CLOSED` 判"永久失败"（connectStream 的 error 分支）
// —— 测试脚本会把自己要测的那条分支悄悄改掉，然后骗过自己。
//
// 断言口径：只断"用户能看见的结果"，不断"某个函数被调用了"。唯一的例外是那个实时
// 通道仪表（数了几条流、几帧、其中几帧是脱敏的）：它在这里是**证据的因果链**
// （"登录之后还在收脱敏帧"就等于"那条访客身份的流根本没被换掉"），而且每条用例都
// 先自证仪表不空（登录前确实收到过脱敏帧、确实有帧在推）。

const (
	identityUser     = "admin"
	identityPass     = "a-very-good-password"
	identityNewPass  = "another-very-good-password"
	identityNodeName = "ident-01"
	identityGuestIP  = "127.0.0.1" // 服务端就在回环上：Agent 的 local_ip / observed_ip 都是它
)

// identityHarnessJS 是注入页面里的自检脚本。
//
// 与 guestHarnessJS 的关系：那个脚本服务的是"访客脱敏"那几条用例，这个脚本服务的是
// "身份在页面存活期间变化"这四条。两份脚本各自完整，不共用分支 —— 共用的话，
// 改一条用例的现场就可能把另一条的断言悄悄喂成空的。
const identityHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var SCEN = CFG.scenario || '';
  var rawFetch = window.fetch.bind(window);

  // ================================================================ B 标签
  //
  // 「跨标签页登录」需要一个**另一个浏览器上下文**：A 标签停在访客只读面板，
  // B 标签用真登录表单登录成管理员。同一个 Chrome 画像 → Cookie 共享，所以
  // A 那一份会话会跟着变，而 A 的 app.js 一点都不知道（这正是要看的东西）。
  //
  // B 标签**不回传结果**：/__result 只收 A 那一条（mock 那边 once）。它跑完登录
  // 就停住，把观测留给 A。
  if (window.location.search.indexOf('tabB') >= 0) {
    var TB = { steps: [], err: '', t0: Date.now() };
    window.__TABBRESULT = TB;
    window.__TABB_T0 = TB.t0;
    function tbNode(id) { return document.getElementById(id); }
    function tbShown(id) { var n = tbNode(id); return !!n && !n.hidden; }
    // 登录成功的判据是**管理员入口在文档里**（顶栏那个「退出」）：访客面板
    // 也算"在应用里"，拿它当判据的话 B 标签会以为自己已经登录了。
    function tbAdmin() { return !!tbNode('btn-logout'); }
    // ⚠️ 必须先等应用渲染出第一屏，再判 tbAdmin()：那三个管理员入口**本来就在
    // index.html 里**，是 refreshSession() 之后才被 applyTopbarEntries 摘掉的。
    // 不等就判的话，页面刚解析完（脚本 +0ms）就能"看见"那个「退出」按钮 ——
    // 于是 B 标签会以为自己已经登录了（第一次跑就是这么被骗过去的）。
    function tbSettled() {
      return tbShown('view-home') || tbShown('view-login') || tbShown('view-setup') ||
        tbShown('view-detail') || tbShown('view-settings');
    }
    function tbSleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
    function tbStep(what) { TB.steps.push(what + '@+' + (Date.now() - TB.t0) + 'ms'); }
    var tbDeadline = Date.now() + 90000;
    // 等 A 标签发令（window.__TABB_GO）才登录：无头浏览器里新窗口的**首次加载能拖到
    // 十几秒之后**（实测过一次 >25 秒），先开好、让它自己启动完，再在最需要的时刻登录。
    (function tbTick() {
      if (!tbSettled()) {
        if (Date.now() > tbDeadline) { TB.err = 'B 标签等不到第一屏'; document.title = 'TABB-FAIL'; return; }
        tbSleep(100).then(tbTick);
        return;
      }
      if (tbAdmin()) {
        tbStep('B 标签已登录（顶栏出现「退出」）');
        document.title = 'TABB-OK';
        return;
      }
      if (!window.__TABB_GO) {
        if (Date.now() > tbDeadline) { TB.err = 'B 标签一直没等到 A 的登录指令'; document.title = 'TABB-FAIL'; return; }
        tbSleep(200).then(tbTick);
        return;
      }
      if (!tbShown('view-login')) {
        if (Date.now() > tbDeadline) { TB.err = 'B 标签等不到登录页'; document.title = 'TABB-FAIL'; return; }
        // 访客面板上直接改地址进登录页：与只读提示里那个入口落到同一个地址。
        if (tbNode('view-login')) { try { window.location.hash = '#/login'; } catch (e) { /* 忽略 */ } }
        tbSleep(100).then(tbTick);
        return;
      }
      tbNode('login-user').value = CFG.user;
      tbNode('login-pass').value = CFG.pass;
      tbNode('login-submit').click();
      tbStep('B 标签提交了登录表单');
      document.title = 'TABB-SUBMITTED';
      (function tbWait() {
        if (tbAdmin()) { tbStep('B 标签登录成功'); document.title = 'TABB-OK'; return; }
        if (Date.now() > tbDeadline) { TB.err = 'B 标签登录后顶栏没有出现「退出」'; document.title = 'TABB-FAIL'; return; }
        tbSleep(150).then(tbWait);
      })();
    })();
    return;
  }

  // ================================================================ A 标签

  var R = {
    errs: [], fatal: '', steps: [], scenario: SCEN, samples: [],
    guestCardTitles: [], guestNetwork: [], guestFrames: 0, guestMasked: 0,
    adminCardTitles: [], preNetwork: [], cardTitlesAfter: [],
    afterCardTitles: [], adminEntriesAfter: [], guestBarAfter: false, liveText: '',
    adminFramesBefore: 0, adminPrivateBefore: 0,
    tabBOpened: false, tabBLoggedIn: false, tabBFallback: false, tabBError: '',
    sessionAuthedMs: 0, switchMs: 0, loginTriggerAt: 0,
    realHidden: false, usedSynthetic: false, hiddenDuringLogin: false, visEvents: [],
    liveBeforeSecondHide: '', liveWhileHidden: '', liveAfterSecondShow: '',
    loginEntryShown: false, twofa: {}, finalText: '', finalText2: ''
  };
  window.__IDENTITYRESULT = R;

  // ---- 实时通道仪表 ------------------------------------------------------
  //
  // 按"阶段"分档统计：guest / admin / revoked / after / after2fa / resume。
  // 每个阶段都记 建了几条流（created）、收到几帧、其中多少帧是**脱敏**的、
  // 多少帧带私有字段。判定"这条流是谁建的"用的就是服务端的行为：
  // 访客帧里没有 local_ip / observed_ip（internal/server/guest.go 的白名单）。
  var stream = { created: 0, opens: 0, frames: 0, masked: 0, privateFrames: 0, phase: 'guest', armed: '', byPhase: {} };
  R.stream = stream;
  function bucket(name) {
    var b = stream.byPhase[name];
    if (!b) { b = stream.byPhase[name] = { created: 0, opens: 0, frames: 0, masked: 0, privateFrames: 0 }; }
    return b;
  }
  // armPhase 把"下一档"挂上：**下一次新建 EventSource 的那一刻**才切过去（见下面
  // 包装器里的那三行）。为什么需要它：身份一变，app.js 会先停旧流、再按新身份建一条，
  // 而这两件事之间隔着一个异步往返（loadNodes → connectStream）；页面这边没法在
  // "登录成功"那一瞬间就把档切过去 —— 那会把旧访客流在这段窗口里推来的脱敏帧算进
  // 新档，变成偶发红。按"新建连接"这个动作切档，语义就干净了：
  // 新档 = 这条**新身份**的流建起来之后的一切。
  function armPhase(name) { stream.armed = name; }
  bucket('guest');
  if (window.EventSource) {
    var RealES = window.EventSource;
    var wrapped = function (url, opts) {
      if (stream.armed) { stream.phase = stream.armed; stream.armed = ''; }
      var es = new RealES(url, opts);
      stream.created++; bucket(stream.phase).created++;
      // 'open' 每次（重）连上都会来一次：浏览器自己重连也会来 ——
      // 这正是①要的证据：**没有**新建对象，但底层连接换了一条。
      es.addEventListener('open', function () { stream.opens++; bucket(stream.phase).opens++; });
      es.addEventListener('nodes', function (event) {
        var masked = false, priv = false;
        try {
          var payload = JSON.parse(event.data);
          (payload.nodes || []).forEach(function (dto) {
            if (dto.id !== CFG.nodeID) return;
            if (Object.prototype.hasOwnProperty.call(dto, 'local_ip') ||
                Object.prototype.hasOwnProperty.call(dto, 'observed_ip')) priv = true;
            else masked = true;
          });
        } catch (e) { /* 数据本身的问题由应用自己显示，这里只统计 */ }
        var b = bucket(stream.phase);
        b.frames++; stream.frames++;
        if (priv) { b.privateFrames++; stream.privateFrames++; }
        if (masked) { b.masked++; stream.masked++; }
      });
      return es;
    };
    // ⚠️ 三个常量必须原样带过去：app.js 用 es.readyState === EventSource.CLOSED
    // 判"永久失败"（见 connectStream 的 error 分支）。只换构造函数会让
    // EventSource.CLOSED 变成 undefined —— 测试脚本就把自己要测的分支改掉了。
    wrapped.CONNECTING = RealES.CONNECTING;
    wrapped.OPEN = RealES.OPEN;
    wrapped.CLOSED = RealES.CLOSED;
    wrapped.prototype = RealES.prototype;
    window.EventSource = wrapped;
  }

  // 记下每一次"可见性"变化（真的切标签页会来，合成构造也会来）：
  // ④ 的回报里要能看出这条用例到底是在真后台里跑的，还是退成了合成构造。
  document.addEventListener('visibilitychange', function () {
    R.visEvents.push((document.hidden ? 'hidden' : 'visible') + '@' + Date.now());
  });

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // speedUpPoll 只给"未登录时的会话复查"那个定时器提速（跨标签页那条用例）。
  //
  // app.js 里它是 window.setInterval(fn, POLL_SESSION_MS = 30000)（main() 里那段）。
  // 断言的对象是**那条链**（复查发现身份变了 → refreshSession() → route() →
  // ensureStream() 把访客流换掉），不是"30 秒"这个数字；而等一次真轮询要 30 秒，
  // 这个包的整包预算（10 分钟超时，CI 上还跑 -race）撑不住两次。所以：
  //   · 只在 scenario=crosstab 时打开（别的用例逐字不变）；
  //   · 只改延迟正好是 30000 的那一个（页面刚载入、还没登录时只会有它；
  //     详情页那个 30 秒整段刷新在身份切换之后才建，那时已经关掉了）；
  //   · 身份一换就把开关关掉（下面 crosstabPass 里），后面的定时器全部原样。
  // 万一有人把 POLL_SESSION_MS 改成别的值：这里的包装不生效，用例会退化成"等一次
  // 真轮询"，仍然正确，只是慢 —— 不会变成假绿。
  R.speedUpPoll = SCEN === 'crosstab';
  var realSetInterval = window.setInterval.bind(window);
  window.setInterval = function (fn, ms) {
    var delay = ms;
    if (R.speedUpPoll && delay === 30000) delay = 4000;
    return realSetInterval(fn, delay);
  };

  // ---- 小工具 ------------------------------------------------------------
  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function loggedIn() { return shown('view-home') || shown('view-detail') || shown('view-settings'); }
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
  // waitForSoft 与 waitFor 的区别：超时**不失败**，只是回 false。
  // ④ 里"能不能真的把页面切到后台"不该由浏览器实现决定用例的红绿。
  function waitForSoft(what, cond, ms) {
    return new Promise(function (resolve) {
      var deadline = Date.now() + (ms || 2000);
      (function poll() {
        var ok = false;
        try { ok = !!cond(); } catch (e) { ok = false; }
        if (ok) { R.steps.push(what); resolve(true); return; }
        if (Date.now() > deadline) { resolve(false); return; }
        setTimeout(poll, 50);
      })();
    });
  }
  function dlLabels(id) {
    var dl = node(id);
    if (!dl) return [];
    var out = [];
    Array.prototype.forEach.call(dl.querySelectorAll('dt'), function (dt) { out.push(dt.textContent); });
    return out;
  }
  function dlText(id) { var dl = node(id); return dl ? dl.textContent : ''; }
  function cardTitles() {
    var grid = node('grid');
    if (!grid) return [];
    var out = [];
    Array.prototype.forEach.call(grid.children, function (c) { out.push(c.title || ''); });
    return out;
  }
  function cardTitleHasIP() {
    return cardTitles().some(function (t) { return t.indexOf('127.0.0.1') >= 0; });
  }
  function adminEntries() {
    return ['btn-add', 'btn-settings', 'btn-logout'].filter(function (id) { return !!node(id); });
  }
  function waitDetail() {
    return waitFor('详情页就绪', function () {
      return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
    }, 30000);
  }
  // sampleNetworkRows 连续采样「网络信息」卡的行标签：每 step 毫秒一次，共 ms 毫秒。
  // 用连续采样而不是两个点：用户报告的现象是"闪一下然后消失"，抹掉发生在中间某一拍。
  function sampleNetworkRows(ms, step) {
    var out = [];
    var start = Date.now();
    return new Promise(function (resolve) {
      (function tick() {
        out.push({ t: Date.now() - start, labels: dlLabels('info-network') });
        if (Date.now() - start >= ms) { resolve(out); return; }
        setTimeout(tick, step);
      })();
    });
  }

  // mark 是"页面 → Go"的时刻信号：Go 那边收到之后做一件页面自己做不到的事
  // （① 要在**另一个会话**里改密，才能真的撤销这个浏览器的会话）。
  // 它同步等到那件事做完才返回 —— 这样"哪些帧算撤销之后的"就没有含糊的中间态。
  function mark(name) {
    return rawFetch('/__mark', { method: 'POST', body: name }).then(function (res) {
      if (!res.ok) throw new Error('时刻信号 ' + name + ' 被拒: HTTP ' + res.status);
      return true;
    });
  }

  // ---- 登录 --------------------------------------------------------------
  function submitPassword() {
    node('login-user').value = CFG.user;
    node('login-pass').value = CFG.pass;
    node('login-submit').click();
  }
  // loginFromGuestBar 走用户真实的那条路：只读提示里的「管理员登录」入口 → 登录表单。
  function loginFromGuestBar() {
    if (node('btn-login-entry')) node('btn-login-entry').click();
    else window.location.hash = '#/login';
    return waitFor('登录页出现', function () { return shown('view-login'); }, 15000)
      .then(function () {
        submitPassword();
        return waitFor('登录后回到应用', loggedIn, 30000);
      });
  }
  function settleGuest() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(function () { return sleep(600); })
      .then(function () {
        return waitFor('访客进入只读面板', function () {
          return shown('view-home') && node('grid') && node('grid').children.length >= 1;
        }, 30000);
      })
      .then(function () { return sleep(2000); });   // 让这条访客流真的开始推帧
  }

  // ---- 另一个上下文里登录（② 与 ④ 的触发条件）----------------------------
  var tabBWin = null;
  function tabBTitle() {
    try { return (tabBWin && tabBWin.document) ? String(tabBWin.document.title || '') : ''; }
    catch (e) { return '(读不到: ' + e.message + ')'; }
  }
  function tabBBootMs() {
    try {
      if (tabBWin && tabBWin.__TABB_T0 && R.tabBOpenedAt) return tabBWin.__TABB_T0 - R.tabBOpenedAt;
    } catch (e) { /* 窗口可能已经关掉 */ }
    return -1;
  }
  // openTabB 打开第二个标签页并**立刻返回**（不等它加载完）。
  //
  // 无头浏览器里新窗口的首次加载可能要十几秒（实测 >25 秒才跑起脚本），所以用法是
  // "先把 B 开起来、让它自己启动"，需要它登录时再发令（signalTabBLogin）。
  function openTabB() {
    var w = null;
    try { w = window.open('/?tabB=1', 'probeTabB'); } catch (e) { w = null; }
    tabBWin = w;
    R.tabBOpened = !!w;
    R.tabBOpenedAt = Date.now();
    return w;
  }
  function sessionIsAuthed() {
    return rawFetch('/api/v1/session', {
      headers: { 'Accept': 'application/json' }, credentials: 'same-origin', cache: 'no-store'
    }).then(function (r) { return r.json(); }).then(function (d) { return !!(d && d.authenticated); });
  }
  // waitSessionAuthed 只**只读地**问一句"现在这个浏览器有没有会话"。
  // 它不改应用状态（app.js 那边要等它自己 30 秒一次的复查），所以它同时是
  // "另一个上下文确实登录成功了"的证据。
  function waitSessionAuthed(ms) {
    var deadline = Date.now() + ms;
    return new Promise(function (resolve, reject) {
      (function poll() {
        sessionIsAuthed().then(function (ok) {
          if (ok) { resolve(true); return; }
          if (Date.now() > deadline) { reject(new Error('另一个上下文登录之后会话仍未建立')); return; }
          setTimeout(poll, 300);
        }, function (err) { reject(err); });
      })();
    });
  }
  function rawLogin() {
    R.tabBFallback = true;
    return rawFetch('/api/v1/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: CFG.user, password: CFG.pass })
    }).then(function (res) {
      if (!res.ok) throw new Error('另一个上下文登录失败: HTTP ' + res.status);
      return true;
    });
  }
  // loginElsewhere 让"另一个上下文"登录：优先让 B 标签自己去登录表单走一遍，
  // B 那条路走不通（没开出来、启动太慢、登录页进不去）才退化成 A 页面里的 raw fetch
  // —— 从 A 的 app.js 角度看两者完全一样（Cookie 在别处建立、它一点都不知道）。
  function loginElsewhere(w) {
    if (!w) return rawLogin().then(function () { return afterLoginElsewhere(); });
    try { w.__TABB_GO = true; } catch (e) { /* 读不到就等超时走退化路径 */ }
    return waitSessionAuthed(25000).then(function () { R.tabBLoggedIn = true; },
      function (err) {
        R.tabBError = String(err && err.message ? err.message : err);
        R.tabBTitle = tabBTitle();
        R.tabBSteps = (w.__TABBRESULT && w.__TABBRESULT.steps) || [];
        R.tabBInner = (w.__TABBRESULT && w.__TABBRESULT.err) || '';
        return rawLogin();
      }).then(function () { return afterLoginElsewhere(); });
  }
  function afterLoginElsewhere() {
    R.tabBBootMs = tabBBootMs();
    R.sessionAuthedMs = Date.now() - R.loginTriggerAt;
    return true;
  }
  // bringToFront 把 A 标签拉回前台。
  //
  // 为什么需要：window.open 出来的 B 标签会占住焦点，A 变成后台标签之后**定时器会被
  // 节流**（实测采样从 150ms 一拍变成约 1 秒一拍）。采样稀疏会削弱"那两行一直在"这条
  // 断言的说服力，所以登录一确认就把 B 关掉、把焦点拿回来。
  function bringToFront(w) {
    if (w) { try { w.close(); } catch (e) { /* 忽略 */ } }
    try { window.focus(); } catch (e) { /* 忽略 */ }
    return waitForSoft('A 标签回到前台', function () { return document.hidden !== true; }, 3000);
  }

  // ---- 可见性：真的切标签页优先，不行就合成 ------------------------------
  //
  // 无头 Chrome 里"另开一个标签页把当前页挤到后台"不保证生效（弹窗可能被拦、
  // 无头下的可见性语义也可能不跟手）。所以④的做法是：**先试真的**，真的没发生
  // 才退化成"把 document.hidden 改成常量 + 派发一个真的 visibilitychange 事件"。
  // app.js 的处理器读的就是 document.hidden（见 bind 里的 visibilitychange 分支），
  // 事件本身也是真的；退化与否如实记在结果里，不假装。
  function setHiddenFlag(hidden) {
    try {
      Object.defineProperty(document, 'hidden', { configurable: true, get: function () { return hidden; } });
      Object.defineProperty(document, 'visibilityState', {
        configurable: true, get: function () { return hidden ? 'hidden' : 'visible'; }
      });
    } catch (e) { R.errs.push('改 document.hidden 失败: ' + e.message); }
    document.dispatchEvent(new Event('visibilitychange'));
  }
  function hideSynthetic() { R.usedSynthetic = true; setHiddenFlag(true); }
  function showSynthetic() { R.usedSynthetic = true; setHiddenFlag(false); }

  // ---- ① 会话被撤销后的降级 ----------------------------------------------
  //
  // 走法：访客面板 → 登录管理员 → 停在详情页（两行地址在）→ 让 Go 在**另一个会话**
  // 里改密（服务端因此撤销这个浏览器的会话，并主动关掉那条管理员实时流）→
  // 浏览器那条 EventSource 会自己重连，而服务端此刻按**访客**接受它（访客开关开着）
  // → 采样 14 秒，看用户看见什么。
  function revokePass() {
    return settleGuest()
      .then(function () {
        R.guestCardTitles = cardTitles();
        R.guestFrames = bucket('guest').frames;
        R.guestMasked = bucket('guest').masked;
        R.steps.push('访客首页就绪');
        return loginFromGuestBar();
      })
      .then(function () { stream.phase = 'admin'; return sleep(2500); })
      .then(function () {
        R.adminCardTitles = cardTitles();
        R.adminFramesBefore = bucket('admin').frames;
        R.adminPrivateBefore = bucket('admin').privateFrames;
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitDetail();
      })
      .then(function () { return sleep(1200); })
      .then(function () {
        R.preNetwork = dlLabels('info-network');
        R.steps.push('撤销前：详情页两行地址在');
        return mark('revoke');
      })
      // 撤销已经**确认**了再切档：这样"带私有字段的帧"只会落在 admin 那一档里，
      // 不会有一帧在飞的私有数据被算进 revoked（那会让断言偶发红）。
      .then(function () { return sleep(500); })
      .then(function () {
        stream.phase = 'revoked';
        R.justAfterRevoke = { labels: dlLabels('info-network'), live: textOf('live-text') };
        // 采样 9 秒：重连要等约 3 秒（服务端的 retry 提示是 3000ms），剩下 6 秒够
        // 六七拍脱敏帧把管理员那两行覆盖掉、也够看出"覆盖之后没再回来"。
        // （这条用例的预算要省着花：整包有 10 分钟超时，见文件头的说明。）
        return sampleNetworkRows(9000, 200);
      })
      .then(function (samples) {
        R.samples = samples;
        R.finalText = dlText('info-network');
        R.adminEntriesAfter = adminEntries();
        R.guestBarAfter = shown('guest-bar');
        R.liveText = textOf('live-text');
        R.cardTitlesAfter = cardTitles();
        R.steps.push('撤销后 9 秒采样完成');
        return true;
      });
  }

  // ---- ②+④ 跨标签页登录 / 后台期间身份变化 -------------------------------
  //
  // 两条缺口（② 跨标签页登录、④ 切后台期间发生身份变化）**共用一次页面生命周期**，
  // 不是为了省事：分开跑的话两条各自都要等一次 30 秒会话复查（POLL_SESSION_MS），
  // 而这个包有整包 10 分钟的超时预算（CI 上还要跑 -race）。触发条件本来就是同一件事
  // —— A 标签是访客态、B 标签在**另一个上下文**里登录成管理员；而 B 标签一被聚焦，
  // A 就进了后台，所以"身份变化发生在页面处于后台的时候"天然成立（结果里如实记下来，
  // 不成立时才用合成构造补上）。两条缺口的断言各自独立、各自会红（见 Go 那两段）。
  function crosstabPass() {
    // 先把 B 标签开起来、让它自己启动（无头里新窗口的首次加载可能要十几秒），
    // A 标签趁这段时间把访客那一遍观测做完 —— 需要它登录时再发令。
    var tabB = openTabB();
    return settleGuest()
      .then(function () {
        R.guestCardTitles = cardTitles();
        R.guestFrames = bucket('guest').frames;
        R.guestMasked = bucket('guest').masked;
        R.steps.push('访客首页就绪（访客流已建）');
        R.loginTriggerAt = Date.now();
        armPhase('after');
        // ④ 的第一半：页面进后台了吗（B 标签抢了焦点）；不行就合成。
        return waitForSoft('真的进入后台', function () { return document.hidden === true; }, 2500);
      })
      .then(function (realHidden) {
        R.realHidden = realHidden;
        if (!realHidden) hideSynthetic();
        // 后台期间：另一个上下文（B 标签）登录成管理员。
        return loginElsewhere(tabB);
      })
      .then(function () {
        R.hiddenDuringLogin = document.hidden === true;
        // 身份是在"页面在后台"的时候变的；回到前台之后靠 30 秒复查把它带过来。
        return waitFor('A 标签发现身份变成管理员', function () {
          return !!node('btn-logout') && !shown('guest-bar');
        }, 45000);
      })
      .then(function () {
        R.switchMs = Date.now() - R.loginTriggerAt;
        // 提速开关关掉：后面的定时器（详情页那个 30 秒整段刷新）全部按原样跑。
        R.speedUpPoll = false;
        // 关掉 B 标签、把 A 拉回前台：后台标签的定时器被节流，采样会稀疏。
        return bringToFront(tabB);
      })
      .then(function (realVisible) {
        if (!realVisible || R.usedSynthetic) showSynthetic();
        return sleep(1200);
      })
      .then(function () {
        R.afterCardTitles = cardTitles();
        R.adminEntriesAfter = adminEntries();
        R.guestBarAfter = shown('guest-bar');
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitDetail();
      })
      .then(function () { return sampleNetworkRows(4000, 150); })
      .then(function (samples) {
        R.samples = samples;
        R.finalText = dlText('info-network');
        R.liveText = textOf('live-text');
        R.steps.push('身份切换之后的首页/详情页观测完成');
        // ④ 的第二半：再切一次后台/前台，实时流必须**自己**接回来。
        R.liveBeforeSecondHide = textOf('live-text');
        hideSynthetic();
        return sleep(400);
      })
      .then(function () {
        R.liveWhileHidden = textOf('live-text');
        armPhase('resume');
        showSynthetic();
        return waitFor('实时流自己接回来', function () {
          return textOf('live-text') === '实时' && bucket('resume').frames >= 1;
        }, 20000);
      })
      .then(function () { return sleep(1200); })
      .then(function () {
        R.liveAfterSecondShow = textOf('live-text');
        R.finalText2 = dlText('info-network');
        R.steps.push('第二次回到前台后的实时流观测完成');
        return true;
      });
  }

  // ---- ③ 访客 + 两步验证登录 ---------------------------------------------
  //
  // 6 位码在页面里用 WebCrypto 现算（base32 解码 + HMAC-SHA1 + 动态截断）：
  // 这在 Go 的实现之外又多一份独立实现，而且不受"提前算好的码跨了 30 秒窗口"影响
  // （见 tzHarnessConfig 里 TwoFASecret 的说明）。
  function base32Decode(s) {
    var alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
    var clean = String(s).toUpperCase().replace(/[^A-Z2-7]/g, '');
    var value = 0, bits = 0, out = [];
    for (var i = 0; i < clean.length; i++) {
      value = (value << 5) | alphabet.indexOf(clean.charAt(i));
      bits += 5;
      if (bits >= 8) { out.push((value >>> (bits - 8)) & 0xFF); bits -= 8; }
    }
    return new Uint8Array(out);
  }
  function totpCode(secret, offsetSec) {
    var counter = Math.floor(Math.floor(Date.now() / 1000 + (offsetSec || 0)) / 30);
    var msg = new Uint8Array(8);
    var c = counter;
    for (var i = 7; i >= 0; i--) { msg[i] = c & 0xFF; c = Math.floor(c / 256); }
    return crypto.subtle.importKey('raw', base32Decode(secret), { name: 'HMAC', hash: 'SHA-1' }, false, ['sign'])
      .then(function (key) { return crypto.subtle.sign('HMAC', key, msg); })
      .then(function (sig) {
        var h = new Uint8Array(sig);
        var off = h[h.length - 1] & 0x0f;
        var value = (((h[off] & 0x7f) << 24) | (h[off + 1] << 16) | (h[off + 2] << 8) | h[off + 3]) >>> 0;
        var text = String(value % 1000000);
        while (text.length < 6) text = '0' + text;
        return text;
      });
  }

  function twofaPass() {
    return settleGuest()
      .then(function () {
        R.guestCardTitles = cardTitles();
        R.guestFrames = bucket('guest').frames;
        R.guestMasked = bucket('guest').masked;
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitDetail();
      })
      .then(function () { return sleep(800); })
      .then(function () {
        R.guestNetwork = dlLabels('info-network');
        window.location.hash = '#/';
        return waitFor('回到访客首页', function () { return shown('view-home') && shown('guest-bar'); }, 20000);
      })
      .then(function () {
        R.loginEntryShown = !!node('btn-login-entry');
        return loginFromGuestBarPasswordOnly();
      })
      .then(function () {
        R.twofa = {
          stepShown: shown('login-code-field'),
          stepOnlyCode: !shown('login-user-field') && !shown('login-pass-field') && shown('login-code-field'),
          submitLabel: textOf('login-submit'),
          logoutAbsent: !node('btn-logout'),
          guestFramesDuring: bucket('guest').frames
        };
        R.steps.push('两步验证第二步已出现（还在登录页）');
        // offsetSec=30 是**必须**的：服务端防重放要求新码的时间计数器更大
        // （internal/server/totp.go 的 verifyTOTP：counter <= lastCounter 直接跳过），
        // 而"启用两步验证"那一步已经用掉了**当前窗口**的码（fixture 里 totpCodeGo(...,0)）。
        // 同一窗口里再拿同一个码去登录会被判"码不正确"。取下一个窗口的码即可，
        // 而 ±1 窗的容差（totpSkew=1）保证它一定落在可接受范围内。
        return totpCode(String(CFG.twofaSecret || ''), 30);
      })
      .then(function (code) {
        R.twofa.codeLen = code.length;
        node('login-code').value = code;
        armPhase('after2fa');
        node('login-submit').click();
        return waitFor('两步验证通过并进入应用', loggedIn, 30000).catch(function (err) {
          // 失败时把页面上那句话一起带回去：不然 Fatal 里只有一句"等待超时"。
          R.twofa.errorText = textOf('login-error');
          R.twofa.backToPassword = shown('login-pass-field');
          throw err;
        });
      })
      .then(function () { return sleep(1000); })
      .then(function () {
        return waitFor('登录后卡片 title 带上来源 IP', cardTitleHasIP, 20000);
      })
      .then(function () {
        R.afterCardTitles = cardTitles();
        R.adminEntriesAfter = adminEntries();
        R.guestBarAfter = shown('guest-bar');
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitDetail();
      })
      .then(function () { return sampleNetworkRows(4000, 150); })
      .then(function (samples) {
        R.samples = samples;
        R.finalText = dlText('info-network');
        R.liveText = textOf('live-text');
        R.steps.push('两步验证登录后的观测完成');
        return true;
      });
  }
  // loginFromGuestBarPasswordOnly 只走到"密码提交完、第二步出现"为止。
  function loginFromGuestBarPasswordOnly() {
    if (node('btn-login-entry')) node('btn-login-entry').click();
    else window.location.hash = '#/login';
    return waitFor('登录页出现', function () { return shown('view-login'); }, 15000)
      .then(function () {
        submitPassword();
        return waitFor('出现第二步验证码框', function () {
          return shown('login-code-field') && !shown('login-pass-field');
        }, 25000);
      });
  }

  // ---- ④ 切后台期间发生身份变化（与 ② 合并在 crosstabPass 里）------------

  function run() {
    if (SCEN === 'revoke') return revokePass();
    if (SCEN === 'crosstab') return crosstabPass();
    if (SCEN === 'twofa') return twofaPass();
    throw new Error('未知的 scenario: ' + SCEN);
  }

  function finish() {
    // B 标签自己那边的进度也顺手带回去：它没登录成功时，回报里要能看出卡在哪一步。
    try {
      if (tabBWin && tabBWin.__TABBRESULT) {
        R.tabBSteps = tabBWin.__TABBRESULT.steps || [];
        R.tabBInner = tabBWin.__TABBRESULT.err || '';
      }
      R.tabBTitle = tabBTitle();
    } catch (e) { /* 窗口已经关掉了：忽略 */ }
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:identity'; });
  }

  window.addEventListener('load', function () {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
      // 把页面上那句提示一起带回去：不然回报里只有一句"等待超时"。
      if (R.twofa && R.twofa.errorText) R.fatal += '（登录页上的错误提示：' + R.twofa.errorText + '）';
    }).then(finish, finish);
  });
})();`

// identityStreamPhase 是某一个阶段里的实时通道计数（见 identityHarnessJS 的仪表）。
type identityStreamPhase struct {
	Created int `json:"created"`
	Opens   int `json:"opens"`
	Frames  int `json:"frames"`
	// Masked 是**脱敏**帧（这台节点的 DTO 里没有 local_ip / observed_ip ⇒ 那条流是访客身份）。
	Masked int `json:"masked"`
	// PrivateFrames 是带私有字段的帧（那条流是管理员身份）。
	PrivateFrames int `json:"privateFrames"`
}

// identityResult 是这四条用例从浏览器里带回来的观测值。
type identityResult struct {
	Errs     []string `json:"errs"`
	Fatal    string   `json:"fatal"`
	Steps    []string `json:"steps"`
	Scenario string   `json:"scenario"`

	Stream struct {
		Created       int                            `json:"created"`
		Opens         int                            `json:"opens"`
		Frames        int                            `json:"frames"`
		Masked        int                            `json:"masked"`
		PrivateFrames int                            `json:"privateFrames"`
		ByPhase       map[string]identityStreamPhase `json:"byPhase"`
	} `json:"stream"`

	// 访客那一遍（四条用例共用的现场自检）。
	GuestCardTitles []string `json:"guestCardTitles"`
	GuestNetwork    []string `json:"guestNetwork"`
	GuestFrames     int      `json:"guestFrames"`
	GuestMasked     int      `json:"guestMasked"`

	// 切换/登录之后那一遍。
	AfterCardTitles   []string `json:"afterCardTitles"`
	AdminEntriesAfter []string `json:"adminEntriesAfter"`
	GuestBarAfter     bool     `json:"guestBarAfter"`
	LiveText          string   `json:"liveText"`
	FinalText         string   `json:"finalText"`
	FinalText2        string   `json:"finalText2"`

	// ① 撤销之前那一遍。
	AdminCardTitles    []string `json:"adminCardTitles"`
	AdminFramesBefore  int      `json:"adminFramesBefore"`
	AdminPrivateBefore int      `json:"adminPrivateBefore"`
	PreNetwork         []string `json:"preNetwork"`
	JustAfterRevoke    struct {
		Labels []string `json:"labels"`
		Live   string   `json:"live"`
	} `json:"justAfterRevoke"`
	CardTitlesAfter []string `json:"cardTitlesAfter"`

	// ② 与 ④ 的"另一个上下文登录"现场记录。
	TabBOpened   bool   `json:"tabBOpened"`
	TabBLoggedIn bool   `json:"tabBLoggedIn"`
	TabBFallback bool   `json:"tabBFallback"`
	TabBError    string `json:"tabBError"`
	// B 标签自己那边的进度（它没登录成功时，回报里要能看出卡在哪一步）。
	TabBTitle string   `json:"tabBTitle"`
	TabBSteps []string `json:"tabBSteps"`
	TabBInner string   `json:"tabBInner"`
	// TabBBootMs 是"B 标签里的脚本开始跑"距离 window.open 多久（无头浏览器里
	// 新窗口的首次加载可能要十几秒，这个数就是那条证据）。
	TabBBootMs        int64    `json:"tabBBootMs"`
	SessionAuthedMs   int64    `json:"sessionAuthedMs"`
	SwitchMs          int64    `json:"switchMs"`
	RealHidden        bool     `json:"realHidden"`
	UsedSynthetic     bool     `json:"usedSynthetic"`
	HiddenDuringLogin bool     `json:"hiddenDuringLogin"`
	VisEvents         []string `json:"visEvents"`
	// ④ 第二次切后台/回前台那一遍。
	LiveBeforeSecondHide string `json:"liveBeforeSecondHide"`
	LiveWhileHidden      string `json:"liveWhileHidden"`
	LiveAfterSecondShow  string `json:"liveAfterSecondShow"`

	// ③ 两步验证那一步的现场记录。
	LoginEntryShown bool `json:"loginEntryShown"`
	TwoFA           struct {
		StepShown         bool   `json:"stepShown"`
		StepOnlyCode      bool   `json:"stepOnlyCode"`
		SubmitLabel       string `json:"submitLabel"`
		LogoutAbsent      bool   `json:"logoutAbsent"`
		GuestFramesDuring int    `json:"guestFramesDuring"`
		CodeLen           int    `json:"codeLen"`
		// 第二步提交失败时页面上那句话（只用于把失败原因带回来）。
		ErrorText      string `json:"errorText"`
		BackToPassword bool   `json:"backToPassword"`
	} `json:"twofa"`

	Samples []guestSwitchSample `json:"samples"`
}

// identityFixture 是这四条用例共用的现场：一台节点 + 打开的访客开关（+ 可选的两步验证）。
//
// 与 startGuestSwitchFixture 的区别只有一处：把 Go 侧那个 browser（**另一个会话**）
// 也交出来 —— ① 需要在别处改密才能撤销浏览器手里那条会话。
type identityFixture struct {
	h      *harness
	br     *browser
	nodeID int64
	token  string
	secret string // 两步验证密钥（只有 ③ 非空）
}

func startIdentityFixture(t *testing.T, withTwoFA bool) *identityFixture {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	nodeID, token := createNodeViaAPI(t, br, identityNodeName)

	// 打开「允许访客查看」（走真实接口）：① 里那条重连**必须**被按访客接受，
	// 否则它拿到的是 401、走的是另一条分支（CLOSED → 复查 → 回登录页）。
	if status, body := br.do(http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": true}, true); status != http.StatusOK {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}
	f := &identityFixture{h: h, br: br, nodeID: nodeID, token: token}
	if token == "" {
		t.Fatal("建节点没有返回 Token：这条用例起不了 Agent")
	}

	if withTwoFA {
		// 走真实接口启用两步验证（与 twofa_browser_test.go 的现场同一套做法）：
		// 密钥由服务端生成，确认码用一份**独立的 Go 实现**算（totpCodeGo）。
		status, body := br.do(http.MethodPost, "/api/v1/twofa/setup", map[string]any{}, true)
		if status != http.StatusOK {
			t.Fatalf("开始启用两步验证失败: %d %v", status, body)
		}
		twofa, _ := body["twofa"].(map[string]any)
		secret, _ := twofa["secret"].(string)
		if secret == "" {
			t.Fatalf("启用流程没有返回密钥：%v", twofa)
		}
		status, body = br.do(http.MethodPost, "/api/v1/twofa/enable", map[string]any{
			"code": totpCodeGo(t, secret, 0),
		}, true)
		if status != http.StatusOK {
			t.Fatalf("确认启用失败: %d %v", status, body)
		}
		f.secret = secret
	}
	return f
}

// startIdentityAgent 起一个真 Agent：节点每秒上报一拍，服务端每秒的变更集里
// 因此总带着这台机器 —— 没有它，"脱敏帧覆盖管理员数据"这件事永远不会发生。
func startIdentityAgent(t *testing.T, f *identityFixture) {
	t.Helper()
	client, _ := newClient(t, "http://"+f.h.addr, f.token)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = client.Run(ctx) }()
	waitFor(t, 15*time.Second, "Agent 上报两拍", func() bool {
		n, ok := f.h.srv.State().Get(f.nodeID)
		return ok && n.Seq >= 2
	})
}

// identityProxy 在"反代 + 注入自检脚本"那一层之上，再加一个"页面 → Go"的时刻信号口。
//
// 为什么需要它：① 要在**另一个会话**里改密（页面自己做不到这件事），而且"哪些帧算
// 撤销之后的"必须有个确定的时刻。信号是页面主动发的，Go 在同一个 HTTP 请求里把那件事
// **同步做完**再回 204 —— 页面拿到回执就说明会话已经撤销了，没有含糊的中间态。
type identityProxy struct {
	*tzProxy
	onMark func(name string) error
}

func newIdentityProxy(t *testing.T, base string, cfg tzHarnessConfig, harnessJS string, onMark func(string) error) *identityProxy {
	t.Helper()
	return &identityProxy{tzProxy: newHarnessProxy(t, base, cfg, harnessJS), onMark: onMark}
}

func (p *identityProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__mark" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 256))
		name := strings.TrimSpace(string(body))
		if p.onMark != nil {
			if err := p.onMark(name); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p.tzProxy.ServeHTTP(w, r)
}

// changePasswordFromOtherSession 用 Go 侧那个 browser（**另一个会话**）改密。
//
// 为什么必须是另一个会话：改密只注销"其它"会话（当前那条保留），所以要让浏览器手里
// 那条会话真的失效，只能从别处发起。服务端在改密成功之后还会主动关掉**全部**管理员
// 实时连接（internal/server/auth.go 的 revokeStreams → hub.revokeAdmins），那条正挂着
// SSE 的长连接因此被打断 —— 这正是"会话被撤销之后浏览器那条 EventSource 会自己重连"
// 的来源，也是 ① 要观察的那条路。
//
// 这个函数在 httptest 的 goroutine 里跑（不是测试 goroutine），所以**不能**用
// t.Fatalf：出错就返回 error，由页面自己变成 R.fatal，用例那边会红在一句清楚的话上。
func changePasswordFromOtherSession(f *identityFixture) error {
	payload, err := json.Marshal(map[string]string{
		"current_password": identityPass,
		"new_password":     identityNewPass,
		"new_password2":    identityNewPass,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+f.h.addr+"/api/v1/auth/password", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.br.csrf)
	resp, err := f.br.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("另一个会话改密失败: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// runIdentityScenario 是四条用例共用的一段：起反代 + mock + Chrome，把结果解回来，
// 并做三条所有用例都该过的公共断言。
func runIdentityScenario(t *testing.T, f *identityFixture, cfg tzHarnessConfig, onMark func(string) error, extraChromeArgs ...string) *identityResult {
	t.Helper()
	proxy := newIdentityProxy(t, "http://"+f.h.addr, cfg, identityHarnessJS, onMark)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, findChrome(), mock.URL+"/", proxy.result, 300*time.Second, "1500,1100", extraChromeArgs...)

	var res identityResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("自检脚本走过的步骤 = %v", res.Steps)
	return &res
}

// logTabBSituation 把"另一个上下文"那一半的现场打出来（② 与 ④ 共用）。
func logTabBSituation(t *testing.T, res *identityResult) {
	t.Helper()
	t.Logf("另一个上下文登录：开了 B 标签 = %v；B 标签脚本启动耗时 = %dms；B 标签登录成功 = %v；"+
		"退化成页面内 fetch = %v；B 标签报错 = %q",
		res.TabBOpened, res.TabBBootMs, res.TabBLoggedIn, res.TabBFallback, res.TabBError)
	t.Logf("B 标签那边：标题 = %q；进度 = %v；它自己的错误 = %q",
		res.TabBTitle, res.TabBSteps, res.TabBInner)
	t.Logf("会话建立耗时 = %dms；A 标签自己发现身份变了耗时 = %dms", res.SessionAuthedMs, res.SwitchMs)
}

// labelOf 把一次 identityResult 里的实时通道计数排版成一行日志。
func labelOf(res *identityResult) string {
	var parts []string
	for _, name := range []string{"guest", "admin", "revoked", "after", "after2fa", "resume"} {
		p, ok := res.Stream.ByPhase[name]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s{建流%d/开%d/帧%d/脱敏%d/私有%d}",
			name, p.Created, p.Opens, p.Frames, p.Masked, p.PrivateFrames))
	}
	return strings.Join(parts, " ")
}

// assertGuestSideIsMasked 是四条用例共用的现场自检：访客那一遍**看不到**私有字段。
//
// 少了它，"登录之后看到了地址"可能只是因为访客本来就该看到 —— 那就成了空断言。
// 这里只查三条用例都会经过的东西（首页卡片 + 实时帧）；访客详情页那一遍只有 ③ 走
// （②④ 合并之后为了省掉一次页面导航没再走那一遍），见 assertGuestDetailHasNoAddressRows。
func assertGuestSideIsMasked(t *testing.T, res *identityResult) {
	t.Helper()
	if res.GuestFrames == 0 {
		t.Fatalf("访客那一遍一帧数据都没收到 —— 仪表没起作用，后面所有断言都是空的")
	}
	if res.GuestMasked == 0 {
		t.Fatalf("访客那一遍没有收到过脱敏帧（%d 帧里 0 帧脱敏）—— 现场不对", res.GuestFrames)
	}
	if len(res.GuestCardTitles) == 0 {
		t.Fatalf("访客那一遍首页一张卡片都没有 —— 现场没搭起来")
	}
	if anyContains(res.GuestCardTitles, identityGuestIP) {
		t.Errorf("访客首页卡片的 title 里不该有 observed_ip：%v", res.GuestCardTitles)
	}
}

// assertGuestDetailHasNoAddressRows 断言访客自己看详情页时那两行**整行不画**。
// 只有真的让访客打开过详情页的场景（② 与 ③）才该调它。
func assertGuestDetailHasNoAddressRows(t *testing.T, res *identityResult) {
	t.Helper()
	if len(res.GuestNetwork) == 0 {
		t.Fatalf("访客详情页的「网络信息」卡是空的 —— 现场没搭起来")
	}
	if hasAddressRows(res.GuestNetwork) {
		t.Errorf("访客详情页不该有「本机地址 / 来源 IP」两行：%v", res.GuestNetwork)
	}
}

// assertSwitchedToAdmin 断言"身份切换之后，用户看见的是一屏完整的管理员数据"。
//
// 四件事一起断，缺一条就说明不了问题：
//   - 顶栏三个管理员入口回到文档里、只读提示条收起（页面自己认了管理员身份）；
//   - 首页卡片 title 里出现 observed_ip（管理员那一份 /api/v1/nodes 落地了）；
//   - 详情页那两行地址在采样窗口内**一直在**（不是"闪一下"）；
//   - 那段时间收到的帧里没有一条是脱敏帧，且确实收到过带私有字段的帧
//     （新那条流是**管理员身份**的，而且真的在推）。
func assertSwitchedToAdmin(t *testing.T, res *identityResult, phase, what string) {
	t.Helper()
	if got := strings.Join(res.AdminEntriesAfter, ","); got != "btn-add,btn-settings,btn-logout" {
		t.Errorf("%s：顶栏三个管理员入口应当回到文档里，实际 %v", what, res.AdminEntriesAfter)
	}
	if res.GuestBarAfter {
		t.Errorf("%s：登录之后不该再显示「只读」提示条", what)
	}
	if !anyContains(res.AfterCardTitles, identityGuestIP) {
		t.Errorf("%s：首页卡片的 title 里应当有 observed_ip（管理员数据），实际 %v",
			what, res.AfterCardTitles)
	}
	p := res.Stream.ByPhase[phase]
	if p.Created < 1 {
		t.Errorf("%s：这一轮里前端**没有**按新身份重建实时流（这一档建流 %d 条）—— "+
			"访客身份的那条流还挂在页面上", what, p.Created)
	}
	if p.PrivateFrames < 1 {
		t.Errorf("%s：这一轮里一帧带私有字段的数据都没收到（%d 帧）—— 新的那条流不是管理员身份，"+
			"或者根本没有数据在推", what, p.Frames)
	}
	if p.Masked != 0 {
		t.Errorf("%s：这一轮里仍然收到 %d 条**访客脱敏**帧（共 %d 帧）—— 那条访客身份的实时流没有被换掉",
			what, p.Masked, p.Frames)
	}
	if len(res.Samples) < 4 {
		t.Fatalf("%s：连续采样点太少（%d 个），这条用例说明不了稳定性", what, len(res.Samples))
	}
	if len(res.Samples) < 20 {
		// 后台标签的定时器会被浏览器节流（实测 150ms 一拍会变成约 1 秒一拍）。
		// 采样稀疏不构成失败（下面每一条断言仍然成立），但要在日志里说清楚。
		t.Logf("注意：%s 的采样点只有 %d 个（期望 20+）—— 页面当时可能在后台，"+
			"定时器被节流了", what, len(res.Samples))
	}
	withRows, firstMissing := 0, -1
	for i, s := range res.Samples {
		if hasAddressRows(s.Labels) {
			withRows++
			continue
		}
		if firstMissing < 0 {
			firstMissing = i
		}
	}
	last := res.Samples[len(res.Samples)-1]
	t.Logf("%s：采样 %d 个点（带两行地址的 %d 个）；最后一个采样点（t=%dms）标签 = %v；"+
		"「网络信息」卡文本 = %q", what, len(res.Samples), withRows, last.T, last.Labels, res.FinalText)
	if withRows == 0 {
		t.Fatalf("%s：「本机地址 / 来源 IP」一次都没出现过（%d 个采样点全是 %v）—— "+
			"管理员那份详情数据没有落地", what, len(res.Samples), last.Labels)
	}
	if !hasAddressRows(last.Labels) {
		t.Errorf("%s：「本机地址 / 来源 IP」在采样窗口内消失了：第 %d 个采样点（t=%dms）起"+
			"「网络信息」卡里只剩 %v —— 被脱敏帧覆盖掉了", what, firstMissing, res.Samples[firstMissing].T, last.Labels)
	}
	if !strings.Contains(res.FinalText, identityGuestIP) {
		t.Errorf("%s：「网络信息」卡的文本里看不到 Agent 的地址：%q", what, res.FinalText)
	}
}

// TestRevokedSessionSilentlySwitchesStreamToGuest 是缺口 ① 的真浏览器证据：
// **会话被撤销之后**（服务端仍开着访客查看）那条实时连接到底变成了什么，以及
// 用户在页面上看得见什么。
//
// 链路（每一环都在真浏览器里跑出来）：管理员在页面里 → 服务端主动关掉这条管理员流
// （改密时 revokeStreams，见 auth.go）→ 浏览器那条 EventSource **自己**重连
// （同一个对象、app.js 一点都不知道）→ 服务端此刻没有会话、但访客开关开着 ⇒ 按
// **访客**接受 → 脱敏帧开始往这一屏管理员页面上写。
//
// 这条用例断言两件事，性质不同，回报里必须分开读：
//
//	(A) 服务端那一半（**期望行为**）：撤销之后再也不许有带私有字段的帧。
//	    反向验证：把 auth.go 里那句 revokeStreams 撤掉 → 这条断言变红。
//	(B) 前端那一半（**缺口①的现状，不是期望行为**）：页面自认为还是管理员
//	    （三个入口还在、状态栏写着「实时」），而数据已经是访客那一份 ——
//	    `sourceAuthed` 是前端自己记的标志，它看不出"浏览器悄悄换了一条身份不同的
//	    连接"。这条用例现在**绿**就等于缺口存在；哪天有人把缺口修掉（重连后重新
//	    核对身份），(B) 这几条会转红，届时应当按新的期望行为重写断言。
func TestRevokedSessionSilentlySwitchesStreamToGuest(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	f := startIdentityFixture(t, false)
	startIdentityAgent(t, f)

	// atomic 而不是普通 bool：时刻信号的处理跑在 httptest 的 goroutine 里
	// （CI 会用 -race 跑全量测试）。
	var revoked atomic.Bool
	res := runIdentityScenario(t, f, tzHarnessConfig{
		NodeID: f.nodeID, NodeName: identityNodeName,
		User: identityUser, Pass: identityPass,
		Scenario: "revoke",
	}, func(name string) error {
		if name != "revoke" {
			return fmt.Errorf("未知的时刻信号 %q", name)
		}
		if err := changePasswordFromOtherSession(f); err != nil {
			return err
		}
		revoked.Store(true)
		return nil
	})
	if !revoked.Load() {
		t.Fatalf("浏览器从头到尾没有发过 revoke 时刻信号 —— 这条用例要观察的路径没跑到")
	}

	assertGuestSideIsMasked(t, res)

	admin, hasAdmin := res.Stream.ByPhase["admin"]
	revok, _ := res.Stream.ByPhase["revoked"]
	t.Logf("实时通道分档 = %s", labelOf(res))

	// ---- 现场自检：撤销之前，管理员那一份数据真的在推、真的在页面上 ----
	if !hasAdmin || admin.Frames < 2 {
		t.Fatalf("撤销之前只收到 %d 帧（阶段 %+v）—— 现场没搭起来，下面的断言会是空的", admin.Frames, admin)
	}
	if admin.PrivateFrames == 0 {
		t.Fatalf("撤销之前一帧带私有字段的管理员数据都没收到 —— 仪表没起作用")
	}
	if !anyContains(res.AdminCardTitles, identityGuestIP) {
		t.Fatalf("撤销之前首页卡片的 title 里应当有 observed_ip：%v", res.AdminCardTitles)
	}
	if !hasAddressRows(res.PreNetwork) {
		t.Fatalf("撤销之前详情页应当有「本机地址 / 来源 IP」两行，实际 %v", res.PreNetwork)
	}

	// ---- (A) 服务端那一半：撤销之后再也不许推私有字段 ----
	if revok.PrivateFrames != 0 {
		t.Errorf("会话被撤销之后仍然收到 %d 条**带私有字段**的帧（共 %d 帧）：那条管理员身份的"+
			"长连接没有被服务端撤销（应当在改密时被 revokeStreams 关掉）", revok.PrivateFrames, revok.Frames)
	}
	if revok.Masked < 3 {
		t.Errorf("会话被撤销之后只收到 %d 条访客脱敏帧（共 %d 帧）：浏览器那条 EventSource "+
			"没有自己重连，或者重连没有被按访客接受 —— 这条用例要观察的降级路径没有发生",
			revok.Masked, revok.Frames)
	}

	// ---- (B) 前端那一半：缺口①的现状 ----
	//
	// 注意读法：这几条断言的是**现在的事实**，不是"应该这样"。
	if revok.Created != 0 {
		t.Errorf("缺口①的现状与预期不符：撤销之后前端自己建了 %d 条新流 —— "+
			"那就不是「浏览器悄悄换了一条连接」了，这条用例的前提不成立", revok.Created)
	}
	if got := strings.Join(res.AdminEntriesAfter, ","); got != "btn-add,btn-settings,btn-logout" {
		t.Errorf("缺口①的现状与预期不符：撤销之后顶栏应当**仍然**留着三个管理员入口"+
			"（页面根本不知道自己的会话已经没了），实际 %v", res.AdminEntriesAfter)
	}
	if res.LiveText != "实时" {
		t.Errorf("缺口①的现状与预期不符：撤销之后状态栏应当**仍然**写着「实时」"+
			"（脱敏帧还在推），实际写着 %q", res.LiveText)
	}
	if anyContains(res.CardTitlesAfter, identityGuestIP) {
		t.Errorf("缺口①的现状与预期不符：撤销之后首页卡片的 title 应当已经被脱敏帧改写成"+
			"没有来源 IP 的样子，实际 %v", res.CardTitlesAfter)
	}
	last := res.Samples[len(res.Samples)-1]
	t.Logf("撤销后采样 %d 个点；最后一个采样点（t=%dms）标签 = %v；状态栏 = %q；顶栏 = %v",
		len(res.Samples), last.T, last.Labels, res.LiveText, res.AdminEntriesAfter)
	if hasAddressRows(last.Labels) {
		t.Errorf("缺口①的现状与预期不符：撤销后 %d 毫秒里「网络信息」卡一直是 %v —— "+
			"脱敏帧没有覆盖掉管理员那两行", last.T, last.Labels)
	} else if hasAddressRows(res.JustAfterRevoke.Labels) {
		t.Logf("缺口①：撤销那一刻（t≈500ms）那两行还在（%v），随后被访客帧抹掉 —— 与用户报的"+
			"「闪一下然后消失」是同一个形状", res.JustAfterRevoke.Labels)
	}
}

// TestGuestTabFollowsCrossTabLoginWhileHidden 一次跑完缺口 ② 与 ④ 两条用例。
//
// 为什么合成一条：两条各自都要等一次 30 秒会话复查（POLL_SESSION_MS），而这个包有
// 整包 10 分钟的超时预算（CI 上还跑 -race）—— 分开跑等于把那次等待付两遍。触发条件
// 本来就是同一件事：A 标签是访客态、B 标签在**另一个上下文**里登录成管理员；而 B 标签
// 一被聚焦，A 就进了后台，"身份变化发生在页面处于后台的时候"天然成立。
//
// 两条用例的断言各自独立、各自会红（见下面两个 t.Run）：
//
//	subtest crosstab（缺口 ②）—— A 标签不刷新页面，靠自己的 30 秒复查把身份换成管理员：
//	  refreshSession() → route() → ensureStream() → 换掉那条访客身份的实时流。
//	  断言用户看得见的东西：顶栏入口、卡片 title 里的来源 IP、详情页那两行地址稳定，
//	  以及"切换之后一条脱敏帧都不许再来"。
//	  反向验证：把 ensureStream() 的身份判据撤掉 → 这一档必然红。
//
//	subtest background（缺口 ④）—— 身份变化发生在页面**处于后台**的时候；
//	  回到前台之后它必须自己收敛成管理员，而且"再切一次后台/前台"时实时流要自己接回来
//	  （visibilitychange 的恢复分支，判据是 inApp() 而不是"在不在首页"）。
//	  断言：切后台之后状态栏变「未连接」、回前台之后回到「实时」且帧继续进来、
//	  接回来的那条流是管理员身份（没有一条脱敏帧）、那两行地址还在。
//	  反向验证：把 `!source && inApp()` 改回 `!source && !el.viewHome.hidden` → 这一档必然红。
func TestGuestTabFollowsCrossTabLoginWhileHidden(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	f := startIdentityFixture(t, false)
	startIdentityAgent(t, f)

	res := runIdentityScenario(t, f, tzHarnessConfig{
		NodeID: f.nodeID, NodeName: identityNodeName,
		User: identityUser, Pass: identityPass,
		Scenario: "crosstab",
	}, nil, "--disable-popup-blocking")

	assertGuestSideIsMasked(t, res)
	t.Logf("实时通道分档 = %s", labelOf(res))
	logTabBSituation(t, res)
	t.Logf("可见性构造：真的进过后台 = %v；用过合成构造 = %v；登录那一刻页面在后台 = %v；"+
		"可见性变化轨迹 = %v", res.RealHidden, res.UsedSynthetic, res.HiddenDuringLogin, res.VisEvents)

	// ---- 缺口 ② 的断言：跨标签页登录被 A 标签自己跟上 ----
	t.Run("crosstab", func(t *testing.T) {
		if res.TabBFallback {
			t.Errorf("这一轮没有真的用到第二个标签页（window.open 被拦或 B 标签没登录成功："+
				"开了 = %v、B 标签报错 = %q、B 标签进度 = %v）—— 退化成页面内 fetch 就不算"+
				"「跨标签页」的证据了", res.TabBOpened, res.TabBError, res.TabBSteps)
		}
		if !res.TabBLoggedIn {
			t.Errorf("B 标签没有把会话建立起来（报错 %q、B 标签进度 %v）", res.TabBError, res.TabBSteps)
		}
		if res.SwitchMs > 45000 {
			t.Errorf("A 标签用了 %dms 才发现身份变了（超过一个复查周期太多）", res.SwitchMs)
		}
		assertSwitchedToAdmin(t, res, "after", "跨标签页登录之后")
	})

	// ---- 缺口 ④ 的断言：后台期间身份变化 + 回前台后实时流自己接回来 ----
	t.Run("background", func(t *testing.T) {
		if !res.HiddenDuringLogin {
			t.Errorf("这一轮里「另一个上下文登录成功的那一刻」页面并不在后台 —— " +
				"缺口④要构造的条件没有成立")
		}
		resume, hasResume := res.Stream.ByPhase["resume"]
		if res.LiveBeforeSecondHide != "实时" {
			t.Fatalf("第二次切后台之前状态栏应当写着「实时」，实际 %q", res.LiveBeforeSecondHide)
		}
		if res.LiveWhileHidden != "未连接" {
			t.Errorf("切到后台之后实时流没有被停掉：状态栏写着 %q（期望「未连接」）—— "+
				"后台还在收每秒的数据，是纯粹的浪费", res.LiveWhileHidden)
		}
		if !hasResume || resume.Created < 1 {
			t.Errorf("回到前台之后实时流没有自己接回来：这一档建流 %d 条（状态栏 %q）",
				resume.Created, res.LiveAfterSecondShow)
		}
		if res.LiveAfterSecondShow != "实时" {
			t.Errorf("回到前台之后状态栏应当回到「实时」，实际 %q", res.LiveAfterSecondShow)
		}
		if hasResume && resume.PrivateFrames < 1 {
			t.Errorf("回到前台之后一帧带私有字段的数据都没收到（%d 帧）—— 接回来的那条流"+
				"不是管理员身份，或者根本没有数据在推", resume.Frames)
		}
		if hasResume && resume.Masked != 0 {
			t.Errorf("回到前台之后收到 %d 条访客脱敏帧 —— 接回来的那条流是访客身份的", resume.Masked)
		}
		last := res.Samples[len(res.Samples)-1]
		if !hasAddressRows(last.Labels) {
			t.Errorf("第二次回到前台之后详情页的「本机地址 / 来源 IP」不见了：采样最后一点 = %v",
				last.Labels)
		}
		if !strings.Contains(res.FinalText2, identityGuestIP) {
			t.Errorf("第二次回到前台之后「网络信息」卡的文本里看不到 Agent 的地址：%q", res.FinalText2)
		}
	})
}

// TestGuestTwoFactorLoginRebuildsStream 是缺口 ③ 的真浏览器用例：
// **访客态下走两步验证登录**这条路（与密码登录共用 enterApp()，但从来没单独验过）。
//
// 两步验证把"登录"这件事拉成了两段：第一步只拿到一张 5 分钟的一次性票据（不是会话），
// 第二步才签发会话、才调 enterApp()。这中间页面在登录视图上停留更久，而那条**访客身份**
// 的实时流一直活着 —— 正是原来那个 bug 的现场。
//
// 断言分两段：
//   - 现场自检：第二步**真的**出现了（只剩一个验证码框、按钮写着「验证并登录」、此时
//     还没有任何管理员入口）—— 少了这条，用例会悄悄退化成"普通密码登录"而自己不知道；
//   - 登录之后：一屏完整的管理员数据，且那两行地址在采样窗口内一直在、没有一条脱敏帧。
func TestGuestTwoFactorLoginRebuildsStream(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	f := startIdentityFixture(t, true)
	startIdentityAgent(t, f)

	res := runIdentityScenario(t, f, tzHarnessConfig{
		NodeID: f.nodeID, NodeName: identityNodeName,
		User: identityUser, Pass: identityPass,
		Scenario:      "twofa",
		TwoFASecret:   f.secret,
		RecoveryCodes: nil,
	}, nil)

	assertGuestSideIsMasked(t, res)
	assertGuestDetailHasNoAddressRows(t, res)
	t.Logf("实时通道分档 = %s", labelOf(res))
	t.Logf("两步验证：第二步出现 = %v；只剩验证码框 = %v；按钮文案 = %q；当时没有管理员入口 = %v；"+
		"第二步期间访客流的帧数 = %d；现算的 6 位码长度 = %d",
		res.TwoFA.StepShown, res.TwoFA.StepOnlyCode, res.TwoFA.SubmitLabel, res.TwoFA.LogoutAbsent,
		res.TwoFA.GuestFramesDuring, res.TwoFA.CodeLen)

	// ---- 现场自检：这**真的**是两步验证那条路 ----
	if !res.LoginEntryShown {
		t.Fatalf("访客面板上没有「管理员登录」入口 —— 现场不对")
	}
	if !res.TwoFA.StepShown || !res.TwoFA.StepOnlyCode {
		t.Fatalf("密码提交之后应当只剩一个验证码框（用户名与密码框收起来），实际 "+
			"stepShown=%v stepOnlyCode=%v —— 两步验证没有生效，这条用例会退化成普通登录",
			res.TwoFA.StepShown, res.TwoFA.StepOnlyCode)
	}
	if res.TwoFA.CodeLen != 6 {
		t.Fatalf("页面里现算的验证码长度 = %d，期望 6", res.TwoFA.CodeLen)
	}
	if !res.TwoFA.LogoutAbsent {
		t.Errorf("第二步还没过的时候不该有「退出」入口（那时手里只有一张一次性票据，没有会话）")
	}
	if res.TwoFA.GuestFramesDuring == 0 {
		t.Errorf("第二步期间那条访客流一帧都没收到 —— 现场不对（这条流本该一直活着）")
	}

	assertSwitchedToAdmin(t, res, "after2fa", "访客态下两步验证登录之后")
}
