package server

// 第二轮安全审计（SECURITY-AUDIT-ROUND2.md）里**跨批次边界**的两条残留半条：
//
//	05-A-4 的接口侧：冷却时间 0 被接口放行 → GET 回显 "0s"、引擎实际按 1m 跑
//	05-A-7 的接口侧：二维码装不下时一律 500「服务端内部错误」（浏览器里一张裂图）
//
// 两条的"另一半"（引擎的 <=0 兜底、编码器的 M→L 降级）已由区域 05 那一批修掉，
// 这里补的是落在 internal/server 的这一半。每条的"撤掉修复会红在哪一处"写在
// 各自的用例注释里。
//
// 05-X-1（≡ 02-2，运行期无锁读写 s.cfg.Alert*）**不在这里**：它已由区域 02
// 那一批修掉（server.go 的 cfgMu + api_admin.go 的两个取值函数），钉住它的用例是
// audit_round2_test.go 的 TestAlertSettingsConcurrentSaveAndReadStayConsistent。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/qr"
	"probe/internal/store"
)

// ---------------------------------------------------------------------------
// 05-A-4（残留半条）：冷却时间 0 在接口层被拒 —— 回显值必须永远等于引擎采用的值
// ---------------------------------------------------------------------------

// 报告里的残留：引擎侧已经把 <=0 兜底成 1 分钟，但接口层放行 0 →
// PUT /api/v1/settings/alert 返回 200、GET /api/v1/settings 回显 "0s"，而引擎
// 实际按 1 分钟跑。也就是"写入被接受、读取回显一个引擎并不采用的值"。
//
// 修法：接口层只拒**引擎会改写的那一段**（<=0），让
// 「写进去的 == 交给引擎的 == 回显的」成为不变量。30s 这类小于引擎下界的取值
// 引擎是原样采用的（normalizeParams 只碰 <=0），接口也照旧放行 —— 把下界抬到
// 1m 会平白把今天可用的取值变成 400，而且与引擎的实际语义不一致。
//
// 反向验证：把 api_admin.go 里那个 `if value <= 0` 去掉（恢复成直接赋值），
// 本用例红在"cooldown=\"0\" 应当 400"那一条 t.Errorf 上。
func TestAlertCooldownZeroIsRejectedSoEchoMatchesEngine(t *testing.T) {
	h := newAuthHarness(t)

	// 基线：先存一个非默认值，被拒的请求必须让它原封不动。
	if status, body := h.put(t, "/api/v1/settings/alert", map[string]any{"cooldown": "5m"}); status != http.StatusOK {
		t.Fatalf("前置保存失败: %d %v", status, body)
	}
	before := h.srv.currentAlertSettings()

	// ① 0 的各种写法都必须 400（time.ParseDuration 眼里的 0 不止 "0" 一种写法）。
	for _, raw := range []string{"0", "0s", "0m", "0h0m0s"} {
		status, body := h.put(t, "/api/v1/settings/alert", map[string]any{"cooldown": raw})
		if status != http.StatusBadRequest {
			t.Errorf("cooldown=%q 应当 400（引擎对 0 的兜底是 1 分钟：放行它就会出现「回显 0s、实际 1m」），实际 %d %v",
				raw, status, body)
			continue
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj == nil || errObj["code"] != "bad_request" {
			t.Errorf("cooldown=%q 的 400 没有走统一错误信封: %v", raw, body)
			continue
		}
		// 文案要给出路，不能只有"超出允许范围"（用户不知道该怎么填）。
		if msg, _ := errObj["message"].(string); !strings.Contains(msg, "必须大于 0") {
			t.Errorf("cooldown=%q 的文案没说清为什么被拒: %q", raw, msg)
		}
		// 被拒的请求不许留下半截改动：内存里的值、交给引擎的参数、GET 回显三处都是原值。
		if got := h.srv.currentAlertSettings(); got != before {
			t.Errorf("被拒的 cooldown=%q 改动了告警参数: %+v", raw, got)
		}
		if got := h.srv.currentAlertParams().NotifyCooldown; got != 5*time.Minute {
			t.Errorf("被拒的 cooldown=%q 改动了交给引擎的参数: %s", raw, got)
		}
		if status, body := h.get(t, "/api/v1/settings"); status != http.StatusOK {
			t.Fatalf("读取设置失败: %d", status)
		} else if alertCfg, _ := body["alert"].(map[string]any); alertCfg["cooldown"] != "5m0s" {
			t.Errorf("被拒的 cooldown=%q 之后 GET 回显变了: %v", raw, alertCfg)
		}
	}

	// 设置页每次都会把四个参数一起发上来：cooldown 的校验排在四个字段的最前面，
	// 所以"0 + 其它参数"也必须整份被拒，不许顺手把后面几个字段改掉。
	if status, _ := h.put(t, "/api/v1/settings/alert", map[string]any{
		"cooldown": "0", "startup_grace": "1h", "debounce": "1s", "recover_stable": "1s",
	}); status != http.StatusBadRequest {
		t.Errorf("cooldown=0 与其它参数一起提交时应当整份 400，实际 %d", status)
	}
	if got := h.srv.currentAlertSettings(); got != before {
		t.Errorf("cooldown=0 被拒时顺手改掉了别的字段（半截改动）: %+v", got)
	}

	// ② 200 的保存：回显值 == 交给引擎的值。
	//
	// handlePutAlertSettings 把 currentAlertParams() 的结果**原样**交给
	// engine.SetParams（api_admin.go 末尾），而引擎只改写 <=0（internal/alert 的
	// normalizeParams，另有 engine_test.go 的用例钉住"非 0 取值一律不变"），
	// 所以只要接口不放行 <=0，"回显的 == 引擎采用的"就成立 —— 这正是这条发现
	// 要的不变量。30s 在列表里是刻意的：它小于引擎下界 1m，但引擎原样采用它，
	// 接口也不该拒（改了这条就说明下界被抬到了 1m，属于过度拒绝）。
	for _, raw := range []string{"1m", "30s", "1ns", "90s", "24h"} {
		want, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("用例自身写错了时长 %q: %v", raw, err)
		}
		status, body := h.put(t, "/api/v1/settings/alert", map[string]any{"cooldown": raw})
		if status != http.StatusOK {
			t.Fatalf("cooldown=%q 应当被接受，实际 %d %v", raw, status, body)
		}
		alertCfg, _ := body["alert"].(map[string]any)
		if alertCfg["cooldown"] != want.String() {
			t.Errorf("cooldown=%q 的保存回显 = %v，期望 %q", raw, alertCfg["cooldown"], want.String())
		}
		if got := h.srv.currentAlertParams().NotifyCooldown; got != want {
			t.Errorf("cooldown=%q 交给引擎的值 = %s，与回显不一致", raw, got)
		}
		status, body = h.get(t, "/api/v1/settings")
		if status != http.StatusOK {
			t.Fatalf("读取设置失败: %d", status)
		}
		alertCfg, _ = body["alert"].(map[string]any)
		if alertCfg["cooldown"] != want.String() {
			t.Errorf("cooldown=%q 之后 GET 回显 = %v，期望 %q", raw, alertCfg["cooldown"], want.String())
		}
	}
}

// 400 那条分支是在 cfgMu 的**持锁闭包**里 return 的（靠 defer 放锁）。这条用例是
// 本机唯一做得了的死锁排查（go test -race 在 32 位 gcc 下编译不了，见交付说明）：
// 让一直撞 400 的写者、写合法值的写者与读者并发打同一个 Server，
// 再用 TryLock 做一次确定性判据 —— 任何一次"忘了放锁"都逃不过这两层。
//
// 反向验证（已实测）：把闭包里的 `defer s.cfgMu.Unlock()` 删掉 → 五个并发请求
// 全挂在该 handler 里（客户端 10s 超时后逐个退出，于是"被拒/合法/读"三个计数
// 全是 0），TryLock 也拿不到锁 → 本用例报「cfgMu 被留在了锁定状态」；随后
// httptest.Server.Close 因为那几条卡住的连接一直等（日志里
// "httptest.Server blocked in Close after 5 seconds"），要到包级 -timeout 才
// 结束 —— 这就是"handler 里漏放锁"的真实表现，也说明这条用例必须存在。
func TestAlertCooldownRejectionDoesNotStrandConfigLock(t *testing.T) {
	h := newAuthHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ok   int // 合法保存（200）
		rej  int // 被拒的 0（400）
		read int // GET /api/v1/settings（200）
		bad  []string
	)
	note := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(bad) < 8 {
			bad = append(bad, fmt.Sprintf(format, args...))
		}
	}

	// 两个"一直被拒"的写者：撞的是新加的那条 400 分支。
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				status, err := h.putNoFatalStatus("/api/v1/settings/alert", map[string]any{"cooldown": "0"})
				switch {
				case err != nil:
					note("被拒写者出错: %v", err)
				case status == http.StatusBadRequest:
					mu.Lock()
					rej++
					mu.Unlock()
				default:
					note("cooldown=0 返回了 %d，期望 400", status)
				}
			}
		}()
	}
	// 两个合法写者：撞的是"持锁赋值"那段。
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for ctx.Err() == nil {
				status, err := h.putNoFatalStatus("/api/v1/settings/alert", map[string]any{
					"cooldown": (time.Duration(i+1) * time.Minute).String()})
				switch {
				case err != nil:
					note("合法写者出错: %v", err)
				case status == http.StatusOK:
					mu.Lock()
					ok++
					mu.Unlock()
				default:
					note("合法保存返回了 %d，期望 200", status)
				}
			}
		}(i)
	}
	// 一个读者：撞的是 RLock 那一侧（RWMutex 的读路径）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			status, err := h.getNoFatalStatus("/api/v1/settings")
			switch {
			case err != nil:
				note("读者出错: %v", err)
			case status == http.StatusOK:
				mu.Lock()
				read++
				mu.Unlock()
			default:
				note("GET /settings 返回了 %d，期望 200", status)
			}
		}
	}()

	time.Sleep(700 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("并发请求卡住了：某个分支没有释放 cfgMu（死锁）")
	}

	// 确定性判据：并发请求都结束了，写锁必须能**立刻**拿到。400 那条分支是在
	// 持锁闭包里 return 的（靠 defer 放锁），漏了放锁这里就拿不到 —— 不必依赖
	// "下一个请求会不会超时"这种时序信号。
	if !h.srv.cfgMu.TryLock() {
		t.Error("cfgMu 被留在了锁定状态：400 分支没有释放锁（此后所有请求都会挂住）")
	} else {
		h.srv.cfgMu.Unlock()
	}

	mu.Lock()
	defer mu.Unlock()
	for _, e := range bad {
		t.Errorf("%s", e)
	}
	// 判据：三类请求都得真的发生过，否则这条用例没有判别力。
	if rej < 10 || ok < 10 || read < 10 {
		t.Fatalf("并发量不足，用例失去判别力：被拒 %d / 合法 %d / 读 %d", rej, ok, read)
	}
}

// ---------------------------------------------------------------------------
// 05-A-7（残留半条）：二维码装不下时是 4xx + 明确文案，而不是 500
// ---------------------------------------------------------------------------

// qrTooLongUsername 找一个"多字节且长到连 L 级也装不下"的管理员用户名。
//
// 长度不写死：临界点取决于 otpauth 链接的固定前缀（issuer、参数、密钥长度），
// 写死会在任何一处前缀变化时变成假失败。从 1 个汉字往上找第一个
// qrEncode(..., LevelM) 返回 ErrTooLong 的长度 —— 注意编码器内部已经会从 M 降到
// L（05-A-7 的上半条），所以"返回 ErrTooLong"就等于"连 L 也装不下"，
// 也就是接口层唯一该给出人话的那种失败。
func qrTooLongUsername(t *testing.T) string {
	t.Helper()
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	for n := 1; n <= maxUserLen; n++ {
		name := strings.Repeat("汉", n)
		link := otpauthURL(twoFAIssuer, name, secret)
		if _, err := qrEncode(link, qr.LevelM); errors.Is(err, qr.ErrTooLong) {
			t.Logf("临界点：%d 个汉字（链接 %d 字节）连 L 级也装不下", n, len(link))
			return name
		}
	}
	t.Fatalf("用户名上限 %d 位内找不到「连 L 都装不下」的多字节用户名，用例失去判别力", maxUserLen)
	return ""
}

// 触发条件就是"管理员用户名又多字节又长"：otpauth 链接里的用户名按 URL 转义
// （一个汉字 9 个字符），够长时链接在**版本 10 的 L 级**下也装不下，编码器的
// M→L 降级就无能为力了 —— 这一种失败必须由接口层给出说得清的答复。
//
// 反向验证：把 twofa.go 里的 `errors.Is(err, qr.ErrTooLong)` 分支删掉
// （恢复成一律 500）→ 本用例红在"用户名装不下时仍然 500"那一条 t.Fatalf 上。
func TestTwoFactorQRTooLongIsConflictWithHint(t *testing.T) {
	h := newAuthHarnessWithUsername(t, qrTooLongUsername(t))

	// 启用流程第一步照常成功：密钥与 otpauth 链接就在同一个响应里 ——
	// 它们是"二维码画不出来"之后用户仍能完成绑定的根据。
	status, body := h.post(t, "/api/v1/twofa/setup", map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("开始启用失败: %d %v", status, body)
	}
	twofa, _ := body["twofa"].(map[string]any)
	if secret, _ := twofa["secret_formatted"].(string); secret == "" {
		t.Fatalf("待确认状态里没有可手动输入的密钥: %v", twofa)
	}

	// 二维码接口：改前恒 500「服务端内部错误」（浏览器里只剩一张裂图）。
	status, body = h.get(t, "/api/v1/twofa/qr")
	if status == http.StatusInternalServerError {
		t.Fatalf("用户名装不下时仍然 500：这不是服务端故障，用户也没有出路（改前就是这样）: %v", body)
	}
	if status != http.StatusConflict {
		t.Fatalf("装不下时应当 409（报告建议 409/422，这里用本包既有的 409 口径），实际 %d %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("错误响应不是统一信封: %v", body)
	}
	if errObj["code"] != "qr_too_long" {
		t.Errorf("错误码 = %v，期望 qr_too_long（前端要能据此单独提示）", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "手动") {
		t.Errorf("文案没有告诉用户怎么办（请手动输入密钥）: %q", msg)
	}

	// 用户真的还能绑：同一条 otpauth 链接必须仍在（否则"4xx + 文案"就是空头支票）。
	status, settings := h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	twofaSettings, _ := settings["twofa"].(map[string]any)
	if link, _ := twofaSettings["otpauth_url"].(string); !strings.HasPrefix(link, "otpauth://totp/") {
		t.Errorf("二维码画不出来时 otpauth 链接也不可用了（用户就真的没法绑定）: %v", twofaSettings)
	}
}

// 非 ErrTooLong 的内部失败必须仍然是 500 —— 审计的硬要求是"只把这一种降成 4xx，
// 别把 500 全改掉"。
//
// 真实编码器只有 ErrTooLong 一种失败能由内容触发（纠错等级是 twofa.go 写死的），
// 所以这条分支没有这个替身钩子就根本到不了：把 qrEncode 换成一个只会报内部错误
// 的替身（产品代码里没有任何地方改它；与 fx.go 的 fxFetch 同一种写法）。
//
// 顺带钉住判据是 errors.Is 而不是 `err == qr.ErrTooLong`：包了一层 %w 的
// 同义错误也必须走 409。
func TestTwoFactorQRInternalFailureStill500(t *testing.T) {
	h := newAuthHarness(t)
	enableTwoFactorPartially(t, h)

	original := qrEncode
	t.Cleanup(func() { qrEncode = original })

	qrEncode = func(string, qr.Level) (*qr.Code, error) {
		return nil, errors.New("编码器内部出问题")
	}
	status, body := h.get(t, "/api/v1/twofa/qr")
	if status != http.StatusInternalServerError {
		t.Fatalf("非 ErrTooLong 的编码失败必须仍然是 500（别把 500 全改成 4xx），实际 %d %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "internal" || errObj["message"] != "服务端内部错误" {
		t.Errorf("500 的响应形状被改动了: %v", body)
	}

	// 包装过的 ErrTooLong 仍然算"装不下"（errors.Is 语义）。
	qrEncode = func(string, qr.Level) (*qr.Code, error) {
		return nil, fmt.Errorf("降到 L 之后仍然装不下: %w", qr.ErrTooLong)
	}
	status, body = h.get(t, "/api/v1/twofa/qr")
	if status != http.StatusConflict {
		t.Fatalf("包装过的 ErrTooLong 应当仍然走 409（判据必须是 errors.Is），实际 %d %v", status, body)
	}
}

// newAuthHarnessWithUsername 与 newAuthHarness 相同，只是管理员用户名可指定。
//
// 为什么另起一份而不是改 newAuthHarnessFull：那把用户名写死成 "admin"，而
// 05-A-7 的触发条件**正是**"多字节且较长的用户名"（链接里被 URL 转义，一个汉字
// 9 个字符）。放在本文件里也免得去动别的组正在用的那份脚手架。
func newAuthHarnessWithUsername(t *testing.T, username string) *authHarness {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
	if err := s.auth.EnsureSetupCode(ctx); err != nil {
		t.Fatalf("生成初始化码: %v", err)
	}

	guard := newBackgroundGuard(t)
	guard.watchDir(filepath.Dir(dbPath))
	realtime := guard.start("realtimeLoop", s.realtimeLoop)
	dispatch := guard.start("dispatch", s.dispatch.Start)
	stopLoops := func() {
		dispatch.stop()
		realtime.stop()
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(stopLoops)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	h := &authHarness{
		ts:       ts,
		srv:      s,
		client:   &http.Client{Jar: jar, Timeout: 10 * time.Second},
		username: username,
		password: "correct-horse-battery-staple",
		cancel:   stopLoops,
	}
	status, body := h.post(t, "/api/v1/setup", map[string]any{
		"code": s.auth.setupCode, "username": username, "password": h.password,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("初始化失败（用户名 %d 个 rune）: HTTP %d %v", len([]rune(username)), status, body)
	}
	h.csrf, _ = body["csrf_token"].(string)
	if h.csrf == "" {
		t.Fatal("初始化后没有拿到 CSRF Token")
	}
	return h
}

// putNoFatalStatus 是 h.put 的并发安全版本：返回状态码而不是在失败时 t.Fatalf
// （t.Fatalf 只能从测试主 goroutine 调）。
func (h *authHarness) putNoFatalStatus(path string, payload map[string]any) (int, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPut, h.ts.URL+path, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CSRF-Token", h.csrf)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// getNoFatalStatus 同上，读一侧。
func (h *authHarness) getNoFatalStatus(path string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
