package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
)

// 两步验证的真浏览器验收（Chrome + 反代注入自检脚本 + 结果 POST 回 mock，
// 与仓库里其它浏览器用例同一套：**不用** --dump-dom --virtual-time-budget）。
//
// 为什么这条必须用真浏览器跑一遍：两步验证的界面不是一个静态表单，而是
// 一条**有先后顺序的状态机** —— 密码过了才出现第二个输入框、确认码输对了才
// 出现恢复码、关闭之后第二个框必须消失。这些性质在服务端单测里全都看不见
// （它们只证明接口对），而用户碰到的正是界面。
//
// 6 位码**不是写死的**：自检脚本用浏览器自己的 WebCrypto（HMAC-SHA1 + 动态截断）
// 现算 —— 这等于在 Go 的实现之外又多了一份独立实现，写死的码反而证明不了
// "用户手机上的验证器算出来的码能被接受"。
const twofaHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], steps: [], fatal: '',
    checks: {}, codes: [], routes: [], qr: {}, views: []
  };
  window.__TWOFARESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  // setupSecret 是启用流程里那份密钥的副本。
  // 必须自己存一份：启用成功之后界面会切到"已启用"，密钥那块会被清空
  // （那是刻意的 —— 密钥只在启用流程里显示），而后面算码还要用它。
  var setupSecret = '';
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

  function loggedIn() { return shown('view-home') || shown('view-detail') || shown('view-settings'); }
  function onLogin() { return shown('view-login'); }

  // ---------------------------------------------------------------- TOTP
  //
  // 用浏览器的 WebCrypto 现算 6 位码（base32 解码 + HMAC-SHA1 + 动态截断）。
  // offsetSec 用来取"下一个窗口"的码：服务端防重放要求新码的计数器更大，
  // 而同一个 30 秒里既登录又去关闭两步验证本来就该被拒。
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
    var counter = Math.floor(Date.now() / 1000 + (offsetSec || 0)) / 30;
    counter = Math.floor(counter);
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

  // ---------------------------------------------------------------- 登录
  function submitPassword() {
    node('login-user').value = CFG.user;
    node('login-pass').value = CFG.pass;
    node('login-submit').click();
  }
  function logout() {
    var btn = node('btn-logout');
    if (!btn) throw new Error('顶栏没有退出按钮');
    btn.click();
    return waitFor('回到登录页', onLogin, 20000);
  }

  // ---------------------------------------------------------------- 步骤
  function stepFirstLogin() {
    return waitFor('登录页出现', onLogin, 30000).then(function () {
      R.checks.未启用时没有第二个框 = !shown('login-code-field');
      submitPassword();
      return waitFor('未启用两步验证时直接进入应用', loggedIn, 30000);
    });
  }

  function openSecurity() {
    window.location.hash = '#/settings/security';
    return waitFor('设置页打开', function () {
      return shown('view-settings') && shown('twofa-enable') && textOf('twofa-state') === '未启用';
    }, 30000);
  }

  function startSetup() {
    node('twofa-enable').click();
    return waitFor('二维码与密钥出现', function () {
      return shown('twofa-setup') && textOf('twofa-secret').length > 0 && !!node('twofa-qr').getAttribute('src');
    }, 30000).then(function () {
      R.checks.二维码同源 = String(node('twofa-qr').getAttribute('src')).indexOf('/api/v1/twofa/qr') === 0;
      R.checks.二维码已解码 = node('twofa-qr').naturalWidth > 0 && node('twofa-qr').naturalHeight > 0;
      R.qr = {
        src: node('twofa-qr').getAttribute('src'),
        w: node('twofa-qr').naturalWidth,
        h: node('twofa-qr').naturalHeight
      };
      R.checks.密钥原文 = textOf('twofa-secret');
      setupSecret = textOf('twofa-secret');
      R.checks.otpauth链接 = textOf('twofa-url');
      return rawFetch('/api/v1/twofa/qr').then(function (res) {
        R.qr.status = res.status;
        R.qr.type = res.headers.get('Content-Type') || '';
        return res.arrayBuffer();
      }).then(function (buf) {
        var b = new Uint8Array(buf);
        R.qr.bytes = b.length;
        R.qr.pngMagic = b.length > 8 && b[0] === 0x89 && b[1] === 0x50 && b[2] === 0x4E && b[3] === 0x47;
        return true;
      });
    });
  }

  function secretRaw() { return setupSecret.replace(/[^A-Za-z0-9]/g, ''); }

  function confirmWrongThenRight() {
    return totpCode(secretRaw(), 0).then(function (right) {
      var wrong = right === '000000' ? '111111' : '000000';
      node('twofa-confirm-code').value = wrong;
      node('twofa-confirm').click();
      return waitFor('错误确认码被拒（这一栏里有提示）', function () {
        return textOf('twofa-error').length > 0 && shown('twofa-setup') && !shown('twofa-on');
      }, 20000).then(function () {
        R.checks.确认码错误时的提示 = textOf('twofa-error');
        R.checks.确认码错误时未启用 = !shown('twofa-on');
        // 输对了才启用。
        return totpCode(secretRaw(), 0);
      }).then(function (code) {
        node('twofa-confirm-code').value = code;
        node('twofa-confirm').click();
        return waitFor('启用成功', function () { return shown('twofa-on') && !shown('twofa-setup'); }, 20000);
      });
    });
  }

  function captureRecoveryCodes() {
    return waitFor('恢复码出现', function () { return shown('twofa-codes'); }, 20000).then(function () {
      var list = node('twofa-codes-list');
      R.codes = Array.prototype.map.call(list.children, function (li) { return li.textContent; });
      R.checks.恢复码提示 = textOf('twofa-codes').indexOf('只显示这一次') >= 0;
      R.checks.徽章 = textOf('twofa-state');
      node('twofa-codes-done').click();
      return waitFor('恢复码收起', function () { return !shown('twofa-codes'); }, 10000);
    });
  }

  // 第二步登录：密码 → 第二个框 → 错误的码 → 正确的码。
  function loginTwoSteps() {
    return logout().then(function () {
      submitPassword();
      return waitFor('出现第二个输入框', function () {
        return shown('login-code-field') && !shown('login-pass-field');
      }, 20000);
    }).then(function () {
      R.checks.第二步只有一个码框 = !shown('login-user-field') && !shown('login-pass-field') && shown('login-code-field');
      R.checks.第二步按钮文案 = textOf('login-submit');
      R.views.push('login-code');
      node('login-code').value = '000000';
      node('login-submit').click();
      return waitFor('错误提示出现', function () { return textOf('login-error').length > 0; }, 20000);
    }).then(function () {
      R.checks.第二步输错的提示 = textOf('login-error');
      R.checks.第二步输错仍未登录 = onLogin();
      return totpCode(secretRaw(), 30);
    }).then(function (code) {
      node('login-code').value = code;
      node('login-submit').click();
      return waitFor('第二步通过并进入应用', loggedIn, 30000);
    });
  }

  // 恢复码登录：一个能用、同一个不能再用。
  function recoveryCodeLogin() {
    var first = R.codes[0];
    return logout().then(function () {
      submitPassword();
      return waitFor('出现第二个输入框', function () { return shown('login-code-field'); }, 20000);
    }).then(function () {
      node('login-code').value = first;
      node('login-submit').click();
      return waitFor('恢复码登录成功', loggedIn, 30000);
    }).then(function () {
      R.checks.恢复码登录 = true;
      return logout();
    }).then(function () {
      submitPassword();
      return waitFor('出现第二个输入框', function () { return shown('login-code-field'); }, 20000);
    }).then(function () {
      node('login-code').value = first;
      node('login-submit').click();
      return waitFor('同一个恢复码被拒', function () { return textOf('login-error').length > 0 && onLogin(); }, 20000);
    }).then(function () {
      R.checks.恢复码复用被拒 = textOf('login-error');
      // 用第二个恢复码进去，好去做"关闭两步验证"。
      node('login-code').value = R.codes[1];
      node('login-submit').click();
      return waitFor('用第二个恢复码进入应用', loggedIn, 30000);
    });
  }

  // 关闭两步验证：密码 + 一个未用过的恢复码（这里刻意不再用 6 位码：
  // 防重放要求新码的计数器更大，而"同一个 30 秒里既登录又关闭"本来就该被拒，
  // 用恢复码可以把这条用例跑得稳定且与时钟无关）。
  function disableTwoFA() {
    window.location.hash = '#/settings/security';
    return waitFor('安全栏打开', function () { return shown('view-settings') && shown('twofa-on'); }, 20000).then(function () {
      node('twofa-password').value = CFG.pass;
      node('twofa-code').value = R.codes[2];
      node('twofa-disable').click();
      return waitFor('确认框弹出', function () { return node('dlg-confirm') && node('dlg-confirm').open === true; }, 10000);
    }).then(function () {
      node('confirm-ok').click();
      return waitFor('两步验证已关闭', function () {
        return shown('twofa-start') && textOf('twofa-state') === '未启用';
      }, 20000);
    }).then(function () {
      R.checks.关闭后的提示 = textOf('twofa-ok');
    });
  }

  function loginAfterDisable() {
    return logout().then(function () {
      R.checks.关闭后没有第二个框 = !shown('login-code-field');
      R.checks.关闭后只剩一个步骤 = shown('login-pass-field') && shown('login-user-field');
      submitPassword();
      return waitFor('关闭之后密码即可登录', loggedIn, 30000);
    });
  }

  // 回归：四条常用路由各走一遍，每条都必须 errs=0。
  function routes() {
    var list = ['#/', '#/n/1', '#/settings/nodes', '#/settings/alert'];
    var i = 0;
    function next() {
      if (i >= list.length) return Promise.resolve(true);
      var hash = list[i++];
      var before = R.errs.length;
      window.location.hash = hash;
      return sleep(1200).then(function () {
        R.routes.push({ hash: hash, errs: R.errs.length - before, view: currentView() });
        return next();
      });
    }
    return next();
  }

  function currentView() {
    if (shown('view-home')) return 'home';
    if (shown('view-detail')) return 'detail';
    if (shown('view-settings')) return 'settings';
    if (shown('view-login')) return 'login';
    return '?';
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(stepFirstLogin)
      .then(openSecurity)
      .then(startSetup)
      .then(confirmWrongThenRight)
      .then(captureRecoveryCodes)
      .then(loginTwoSteps)
      .then(recoveryCodeLogin)
      .then(disableTwoFA)
      .then(loginAfterDisable)
      .then(routes);
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

  // shot 模式：只把界面开到指定状态就停住（给人截图核对用），不跑完整流程。
  //
  // 与断言那条路的关键区别：这一条带 --virtual-time-budget，页面里的
  // Date.now() 是**虚拟时间**，脚本自己算的 6 位码对不上服务端的钟。
  // 所以现场（是否已启用、码是多少、恢复码是哪几个）都由 Go 侧预置好，
  // 通过 CFG.twofaCode / CFG.recoveryCodes 传进来。
  function shot() {
    var stop = function (what) {
      document.title = 'SHOT-READY:' + what;
      return new Promise(function () { /* 永不 resolve：界面停在这里 */ });
    };
    loginShot()
      .then(function () {
        if (CFG.shot === 'setup') {
          window.location.hash = '#/settings/security';
          return waitFor('安全栏（未启用）', function () {
            return shown('view-settings') && shown('twofa-enable');
          }, 30000).then(function () {
            node('twofa-enable').click();
            return waitFor('二维码与密钥出现', function () {
              return shown('twofa-setup') && textOf('twofa-secret').length > 0;
            }, 30000);
          }).then(function () { return stop('setup'); });
        }
        if (CFG.shot === 'login') return stop('login');
        window.location.hash = '#/settings/security';
        return waitFor('安全栏（已启用）', function () {
          return shown('view-settings') && shown('twofa-on');
        }, 30000).then(function () {
          if (CFG.shot === 'security') return stop('security');
          // 重新生成恢复码：用密码 + 一个预置好的恢复码（与时钟无关）。
          node('twofa-password').value = CFG.pass;
          node('twofa-code').value = (CFG.recoveryCodes || [])[0] || '';
          node('twofa-regen').click();
          return waitFor('恢复码出现', function () { return shown('twofa-codes'); }, 30000);
        }).then(function () { return stop('codes'); });
      })
      .catch(function (err) {
        document.title = 'SHOT-FAIL:' + (err && err.message ? err.message : err);
      });
  }

  // loginShot 走到"登录页"或"登录第二步"或"已进入应用"。
  function loginShot() {
    return waitFor('登录页出现', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        submitPassword();
        return waitFor('第二步或进入应用', function () {
          return shown('login-code-field') || loggedIn();
        }, 30000);
      })
      .then(function () {
        if (loggedIn()) return true;
        if (CFG.shot === 'login') return true;   // 就停在第二步
        node('login-code').value = CFG.twofaCode || '';
        node('login-submit').click();
        return waitFor('第二步通过', loggedIn, 30000);
      });
  }
})();`

// twofaBrowserResult 是自检脚本回传的观测值。
type twofaBrowserResult struct {
	Errs   []string       `json:"errs"`
	Fatal  string         `json:"fatal"`
	Steps  []string       `json:"steps"`
	Checks map[string]any `json:"checks"`
	Codes  []string       `json:"codes"`
	// Routes 是回归那四条路由的观测（每条都要 errs=0）。这里用本文件自己的
	// 结构而不是复用 notifyBrowserRoute：多一个 errs 字段，而那个类型是别人
	// 用例的形状，加字段会牵动它。
	Routes []twofaRouteSample `json:"routes"`
	QR     struct {
		Src      string `json:"src"`
		Status   int    `json:"status"`
		Type     string `json:"type"`
		Bytes    int    `json:"bytes"`
		W        int    `json:"w"`
		H        int    `json:"h"`
		PNGMagic bool   `json:"pngMagic"`
	} `json:"qr"`
	Views []string `json:"views"`
}

// twofaRouteSample 是一条路由走完之后的样子。
type twofaRouteSample struct {
	Hash string `json:"hash"`
	View string `json:"view"`
	Errs int    `json:"errs"`
}

// totpCodeGo 用 Go 标准库现算 6 位码（HMAC-SHA1 + 动态截断）。
//
// 它是这个文件里**唯一**能算码的地方，用途很具体：截图那条路带
// --virtual-time-budget，页面里的 Date.now() 是虚拟时间，算出来的码对不上
// 服务端的钟，所以预置现场与那几张图要用的码都由这里按真实时钟算好、经
// CFG 传给页面。
//
// 它与 internal/server/totp.go 是两份**互相独立**的实现（这里连代码都不共享），
// 两边算出来的码必须一致 —— 这正是 RFC 6238 定义的那件事。
func totpCodeGo(t *testing.T, secret string, offset int64) string {
	t.Helper()
	clean := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(clean)
	if err != nil {
		t.Fatalf("密钥不是合法 base32（%q）: %v", secret, err)
	}
	counter := uint64(time.Now().Unix()/30 + offset)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	value := uint32(sum[off]&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", value%1000000)
}

// twofaFixture 起一个真服务端（关掉汇率取数：用例不该依赖外网）+ 一台机器，
// 并返回一个**已登录**（带会话与 CSRF）的 HTTP 客户端。
func twofaFixture(t *testing.T) (*harness, *browser) {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
		func(cfg *config.Server) { cfg.FX = false })
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	createNodeViaAPI(t, br, "twofa-node")
	return h, br
}

// twofaEnabledFixture 起一个"两步验证已经开着"的服务端，返回密钥与 10 个恢复码。
//
// 为什么走真实 HTTP 接口而不是直接写库：截图要展示的是"用户自己启用之后的样子"，
// 而启用这一步本身（密钥怎么来、恢复码怎么发）就是被验证的对象之一。
//
// 另外它顺带证明了一件事：**用一份独立的 Go 实现算出来的码，服务端认**
// （见 totpCodeGo）。
func twofaEnabledFixture(t *testing.T) (*harness, string, []string) {
	t.Helper()
	h, br := twofaFixture(t)

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
	rawCodes, _ := body["recovery_codes"].([]any)
	codes := make([]string, 0, len(rawCodes))
	for _, item := range rawCodes {
		code, _ := item.(string)
		codes = append(codes, code)
	}
	if len(codes) != 10 {
		t.Fatalf("恢复码个数 = %d，期望 10", len(codes))
	}
	return h, secret, codes
}

// TestTwoFactorInRealBrowser 走一遍"用户在真浏览器里真的能用的那条路"。
func TestTwoFactorInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	h, _ := twofaFixture(t)
	cfg := tzHarnessConfig{User: "admin", Pass: "a-very-good-password"}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, twofaHarnessJS)
	mock := newMockServer(t, proxy)

	raw := runChromeForResult(t, chrome, mock.URL+"/#/settings/security", proxy.result, 240*time.Second, "1500,1100")
	var res twofaBrowserResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("已完成步骤：%v", res.Steps)

	// ① 未启用时登录页**没有**第二个框（不然用户会以为坏了）。
	if v, _ := res.Checks["未启用时没有第二个框"].(bool); !v {
		t.Error("没开两步验证时，登录页不应该有验证码输入框")
	}

	// ② 二维码：同源、真 PNG、能被浏览器解码出来。
	if v, _ := res.Checks["二维码同源"].(bool); !v {
		t.Errorf("二维码的 src 不是同源接口：%q", res.QR.Src)
	}
	if !res.QR.PNGMagic || res.QR.Type == "" || !strings.Contains(res.QR.Type, "image/png") {
		t.Errorf("二维码不是 PNG（type=%q magic=%v bytes=%d）", res.QR.Type, res.QR.PNGMagic, res.QR.Bytes)
	}
	if res.QR.W < 150 || res.QR.H < 150 {
		t.Errorf("二维码解码后的尺寸太小：%dx%d（%d 字节）", res.QR.W, res.QR.H, res.QR.Bytes)
	}
	// 密钥原文与 otpauth 链接必须都显示出来（扫不了码的人要能手动加）。
	secret, _ := res.Checks["密钥原文"].(string)
	link, _ := res.Checks["otpauth链接"].(string)
	if strings.ReplaceAll(secret, " ", "") == "" || !strings.Contains(secret, " ") {
		t.Errorf("密钥原文应当是分组可读的 base32：%q", secret)
	}
	if !strings.HasPrefix(link, "otpauth://totp/") || !strings.Contains(link, "secret=") {
		t.Errorf("otpauth 链接不对：%q", link)
	}

	// ③ 确认码输错时不许启用（提示要有）。
	if v, _ := res.Checks["确认码错误时未启用"].(bool); !v {
		t.Error("确认码输错时不应该启用两步验证")
	}
	if msg, _ := res.Checks["确认码错误时的提示"].(string); msg == "" {
		t.Error("确认码输错时页面上没有提示")
	}

	// ④ 恢复码：10 个、形状对、"只显示这一次"的提示在。
	if len(res.Codes) != 10 {
		t.Fatalf("恢复码个数 = %d，期望 10：%v", len(res.Codes), res.Codes)
	}
	for _, code := range res.Codes {
		if len(code) != 11 || code[5] != '-' {
			t.Errorf("恢复码 %q 的形状不对（期望 XXXXX-XXXXX）", code)
		}
	}
	if v, _ := res.Checks["恢复码提示"].(bool); !v {
		t.Error("恢复码那块没有写「只显示这一次」")
	}
	if badge, _ := res.Checks["徽章"].(string); badge != "已启用" {
		t.Errorf("启用之后状态徽章 = %q，期望「已启用」", badge)
	}

	// ⑤ 登录第二步：只剩一个码框、按钮文案变了、输错有提示且没登录进去。
	if v, _ := res.Checks["第二步只有一个码框"].(bool); !v {
		t.Error("第二步应当只显示验证码输入框（用户名与密码框收起来）")
	}
	if v, _ := res.Checks["第二步输错仍未登录"].(bool); !v {
		t.Error("验证码输错时不应该进入应用")
	}
	if msg, _ := res.Checks["第二步输错的提示"].(string); msg == "" {
		t.Error("验证码输错时页面上没有提示")
	}
	if btn, _ := res.Checks["第二步按钮文案"].(string); !strings.Contains(btn, "验证") {
		t.Errorf("第二步的按钮文案 = %q，期望出现「验证」", btn)
	}

	// ⑥ 恢复码能登录，且同一个不能再用。
	if v, _ := res.Checks["恢复码登录"].(bool); !v {
		t.Error("恢复码没能登录成功")
	}
	if msg, _ := res.Checks["恢复码复用被拒"].(string); msg == "" {
		t.Error("同一个恢复码用第二次时页面上没有提示")
	}

	// ⑦ 关闭之后：界面回到"未启用"，登录页也只有一个步骤。
	if msg, _ := res.Checks["关闭后的提示"].(string); msg == "" {
		t.Error("关闭两步验证之后页面上没有提示")
	}
	if v, _ := res.Checks["关闭后没有第二个框"].(bool); !v {
		t.Error("关闭两步验证之后，登录页不该再有验证码输入框")
	}
	if v, _ := res.Checks["关闭后只剩一个步骤"].(bool); !v {
		t.Error("关闭两步验证之后，登录页应当只剩用户名 + 密码")
	}

	// ⑧ 回归：四条常用路由 errs=0。
	if len(res.Routes) != 4 {
		t.Fatalf("只走了 %d 条路由：%v", len(res.Routes), res.Routes)
	}
	for _, r := range res.Routes {
		if r.Errs != 0 {
			t.Errorf("路由 %s 上有 %d 条 JS 报错", r.Hash, r.Errs)
		}
		t.Logf("路由 %s：errs=%d view=%s", r.Hash, r.Errs, r.View)
	}
}

// TestTwoFactorScreenshots 出四张人工核对的截图（默认跳过）。
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestTwoFactorScreenshots -v
//
// 产出的图：twofa-setup（二维码 + 密钥 + otpauth 链接）、twofa-codes（恢复码）、
// twofa-security（已启用的那一组）、twofa-login（登录第二步）。
//
// 前两张是两个**不同**的现场：setup 那张要求两步验证还没启用（点了「启用」
// 才有一份待确认的密钥与二维码），另外三张要求已经启用（恢复码只能在
// 已启用之后重新生成）。
func TestTwoFactorScreenshots(t *testing.T) {
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

	// 四张图用**三个独立的现场**，不是一个现场连拍四张。
	//
	// 原因是恢复码的一次性语义：登录要花掉一个、"重新生成恢复码"又会把旧的
	// 整批作废。共用一个现场的话，"哪张图先拍、用到第几个恢复码"就变成了
	// 隐式依赖 —— 拍图的顺序一改，图就悄悄拍成了错误状态（这条踩过一次）。
	hOff, _ := twofaFixture(t)
	hSec, _, secCodes := twofaEnabledFixture(t)
	hLogin, _, _ := twofaEnabledFixture(t)
	hCodes, _, codeCodes := twofaEnabledFixture(t)

	shots := []struct {
		name  string
		shot  string
		addr  string
		login string // 登录第二步要填的凭据（6 位码，或一个未用过的恢复码）
		codes []string
		w, h  int
		scale string
	}{
		// setup 那张的现场还没启用（点「启用」才生成待确认的密钥与二维码），
		// 所以登录没有第二步，不需要任何凭据。
		{"twofa-setup", "setup", hOff.addr, "", nil, 1400, 1150, "1.5"},
		// 已启用那一组：登录用恢复码（一次性、与时钟和窗口都无关）。
		{"twofa-security", "security", hSec.addr, secCodes[0], nil, 1400, 1000, "1.5"},
		// 登录第二步那张只需要把界面开到第二步，不用真的登进去。
		{"twofa-login", "login", hLogin.addr, "", nil, 900, 700, "2"},
		// 恢复码那张：登录一个、重新生成时再一个（生成之后旧的整批作废，
		// 所以它必须用自己的现场）。
		{"twofa-codes", "codes", hCodes.addr, codeCodes[1], codeCodes, 1400, 1250, "1.5"},
	}
	for _, s := range shots {
		if only := os.Getenv("PROBE_SHOT_ONLY"); only != "" && !strings.Contains(only, s.name) {
			continue
		}
		cfg := tzHarnessConfig{
			User: "admin", Pass: "a-very-good-password", Shot: s.shot,
			// 登录第二步要用的凭据：用**恢复码**而不是 6 位动态码。
			//
			// 两个原因：截图那条路带 --virtual-time-budget，页面里的 Date.now()
			// 是虚拟时间；而且 6 位码会被防重放挡住（"同一个码不能用两次"）。
			// 恢复码天然一次性，不受时钟与窗口影响。
			TwoFACode: s.login,
			// 重新生成恢复码那一步同样用恢复码（见 shot()）。
			RecoveryCodes: s.codes,
		}
		proxy := newHarnessProxy(t, "http://"+s.addr, cfg, twofaHarnessJS)
		mock := newMockServer(t, proxy)
		out := filepath.Join(outDir, s.name+".png")
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--force-device-scale-factor=" + s.scale,
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + "/#/settings/security",
		}
		runChromeScreenshot(t, chrome, args, out, 120*time.Second)
	}
}

// runChromeScreenshot 起一个 Chrome 出图（与 settings_shot_test.go 同一套参数，
// 只是那一段代码写在那个用例内部；这里复制一份是为了不动 gzip 与设置页那些文件）。
func runChromeScreenshot(t *testing.T, chrome string, args []string, out string, timeout time.Duration) {
	t.Helper()
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
	case <-time.After(timeout):
		killChrome(cmd)
		t.Fatalf("Chrome 超时。输出尾部：\n%s", tail(buf.String(), 800))
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("截图没有生成: %v\nChrome 输出尾部：\n%s", err, tail(buf.String(), 800))
	}
	t.Logf("%s（%d 字节）", out, st.Size())
}
