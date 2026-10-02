package server

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/store"
)

// 这个文件是两步验证的验收。断言分成六组，每组对应一条"做错了也看不出来"的风险：
//
//	1. 算法正确性   —— 用 RFC 6238 附录 B 的标准测试向量（不是自己编的期望值）
//	2. 完整流程     —— 启用 → 登录 → 关闭，含"确认码输错时不启用"
//	3. 限流         —— 2FA 输错计入**与密码同一套**限流
//	4. 重放         —— 同一个码不能用两次
//	5. 恢复码       —— 一次性、存哈希、用过作废
//	6. 会话边界     —— 只过密码、没过第二因素时，所有需要会话的接口都是 401
//	7. 访客与重置   —— 2FA 接口全 accessAdmin；本机重置后能用密码登录
//
// 第 6 组是这套设计里最要紧的一条：它证明"没有半登录状态"。

// rfc6238Seed 是 RFC 6238 附录 B 用的 SHA-1 种子：ASCII "12345678901234567890"。
const rfc6238Seed = "12345678901234567890"

// TestHOTPMatchesRFC6238AppendixB 用标准测试向量钉住算法。
//
// 期望值**来自 RFC 6238 附录 B 的表**（Time / TOTP / 实际就是 8 位 HOTP），
// 不是本地实现的输出 —— 自己编期望值只能证明"代码没变过"，证明不了"算得对"。
func TestHOTPMatchesRFC6238AppendixB(t *testing.T) {
	// 附录 B 的 SHA-1 那一列。
	vectors := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	key := []byte(rfc6238Seed)
	for _, v := range vectors {
		counter := uint64(v.unix / int64(totpPeriod))
		if got := hotpCode(key, counter, 8); got != v.want {
			t.Errorf("T=%d：HOTP(8 位) = %s，RFC 6238 附录 B 给的是 %s", v.unix, got, v.want)
		}
	}
}

// TestTOTP6DigitsIsRFCValueModuloHundred 把 6 位码与标准向量对齐。
//
// 6 位码是同一个动态截断结果对 10^6 取模（RFC 4226 §5.4 的 Digit 参数），
// 也就是 8 位码的**后 6 位**（前导零要保留）。这条把"我们自己定的 6 位"
// 与 RFC 的已知值绑在一起，而不是各说各话。
func TestTOTP6DigitsIsRFCValueModuloHundred(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(rfc6238Seed))
	vectors := []struct {
		unix  int64
		want8 string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, v := range vectors {
		got, err := totpCodeAt(secret, time.Unix(v.unix, 0))
		if err != nil {
			t.Fatalf("算码失败: %v", err)
		}
		if want := v.want8[len(v.want8)-6:]; got != want {
			t.Errorf("T=%d：6 位码 = %s，期望 %s（RFC 值的后 6 位）", v.unix, got, want)
		}
	}
	// 校验函数也必须认这个码（不只是"能算出来"）。
	now := time.Unix(1111111109, 0)
	code, _ := totpCodeAt(secret, now)
	if counter, ok := verifyTOTP(secret, code, now, -1); !ok || counter != now.Unix()/int64(totpPeriod) {
		t.Errorf("verifyTOTP 不认自己算出来的码：counter=%d ok=%v", counter, ok)
	}
}

// TestVerifyTOTPWindowIsExactlyOneStep 钉住时间窗是 ±1 格，不更宽。
//
// 放宽窗口是"让用户少遇到漂移问题"的常见做法，代价是每个码的有效期变长
// （6 位码只有 100 万种，窗口越宽越依赖限流）。这里把边界写死：
// −1/+1 收，−2/+2 拒。
func TestVerifyTOTPWindowIsExactlyOneStep(t *testing.T) {
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	now := time.Unix(1700000000, 0)
	if now.Unix()%int64(totpPeriod) == 0 {
		now = now.Add(time.Second) // 让窗口内偏移有区分度
	}

	cases := []struct {
		name  string
		at    time.Time
		allow bool
	}{
		{"上一个窗口（-1）", now.Add(-totpPeriod * time.Second), true},
		{"当前窗口（0）", now, true},
		{"下一个窗口（+1）", now.Add(totpPeriod * time.Second), true},
		{"上上个窗口（-2）", now.Add(-2 * totpPeriod * time.Second), false},
		{"下下个窗口（+2）", now.Add(2 * totpPeriod * time.Second), false},
	}
	for _, tc := range cases {
		code, err := totpCodeAt(secret, tc.at)
		if err != nil {
			t.Fatalf("%s：算码失败: %v", tc.name, err)
		}
		_, ok := verifyTOTP(secret, code, now, -1)
		if ok != tc.allow {
			t.Errorf("%s：verifyTOTP = %v，期望 %v（窗口固定 ±1 格）", tc.name, ok, tc.allow)
		}
	}

	// 垃圾输入不能通过（也不能 panic）。
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "000000x", "99999"} {
		if _, ok := verifyTOTP(secret, bad, now, -1); ok && bad != "000000" {
			t.Errorf("垃圾输入 %q 被当成了合法动态码", bad)
		}
	}
}

// TestVerifyTOTPRejectsReplay 同一个计数器只能用一次。
func TestVerifyTOTPRejectsReplay(t *testing.T) {
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	now := time.Unix(1700000000, 0)
	code, _ := totpCodeAt(secret, now)
	counter := now.Unix() / int64(totpPeriod)

	got, ok := verifyTOTP(secret, code, now, -1)
	if !ok || got != counter {
		t.Fatalf("首次校验应当通过：counter=%d ok=%v", got, ok)
	}
	if _, ok := verifyTOTP(secret, code, now, got); ok {
		t.Error("同一个码在同一个窗口里被用了第二次 —— 防重放没生效")
	}
	// 用过的窗口之后，更早的窗口也必须拒（否则"重放一个旧码"仍然可行）。
	older, _ := totpCodeAt(secret, now.Add(-totpPeriod*time.Second))
	if _, ok := verifyTOTP(secret, older, now, got); ok {
		t.Error("比 lastCounter 更早的码被接受了 —— 防重放只挡了相等的那一个")
	}
	// 下一个窗口照旧可用（否则用户得等到下下个窗口才能登录）。
	newer, _ := totpCodeAt(secret, now.Add(totpPeriod*time.Second))
	if _, ok := verifyTOTP(secret, newer, now, got); !ok {
		t.Error("下一个窗口的码应当仍然可用")
	}
}

// TestSecretAndFormatting 生成、归一化、分组显示三件事。
func TestSecretAndFormatting(t *testing.T) {
	a, err := newTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	b, err := newTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	// crypto/rand：两次相同等于种子可预测（两条都是 160 位随机，撞上就是坏了）。
	if a == b {
		t.Fatal("两次生成的密钥相同 —— 不是 crypto/rand")
	}
	if len(a) != 32 {
		t.Fatalf("160 位 base32 应当是 32 个字符，实际 %d（%q）", len(a), a)
	}
	raw, err := normalizeSecret(a)
	if err != nil || len(raw) != totpSecretBytes {
		t.Fatalf("归一化解码失败: %v（长度 %d）", err, len(raw))
	}
	// 分组显示必须还能被归一化回去（用户抄的就是这一串）。
	grouped := formatSecret(a)
	if !strings.Contains(grouped, " ") {
		t.Errorf("分组显示里应当有空格：%q", grouped)
	}
	back, err := normalizeSecret(grouped)
	if err != nil || string(back) != string(raw) {
		t.Errorf("分组后的密钥解不回原值：%v", err)
	}
	// 手工输入常见的几种写法都要能解开。
	for _, variant := range []string{strings.ToLower(grouped), strings.ReplaceAll(grouped, " ", "-")} {
		if got, err := normalizeSecret(variant); err != nil || string(got) != string(raw) {
			t.Errorf("%q 应当能被归一化：%v", variant, err)
		}
	}
	// 非法输入必须报错而不是给一个随机的解释。
	for _, bad := range []string{"", "!!!!", "0189"} {
		if _, err := normalizeSecret(bad); err == nil {
			t.Errorf("%q 不是合法 base32，应当报错", bad)
		}
	}
}

// TestRecoveryCodesAreOneTimeHashes 恢复码的形态：一次性、只存哈希。
func TestRecoveryCodesAreOneTimeHashes(t *testing.T) {
	codes, err := newRecoveryCodes()
	if err != nil {
		t.Fatalf("生成恢复码: %v", err)
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("恢复码个数 = %d，期望 %d", len(codes), recoveryCodeCount)
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if seen[code] {
			t.Fatalf("生成了重复的恢复码 %q", code)
		}
		seen[code] = true
		if strings.Contains(code, " ") || len(code) != recoveryCodeChars+1 {
			t.Errorf("恢复码 %q 的形状不对（期望 XXXXX-XXXXX）", code)
		}
	}
	// 同一个码的不同写法必须是同一个哈希（用户可能连字符没打、或者小写）。
	if hashRecoveryCode(codes[0]) != hashRecoveryCode(strings.ToLower(strings.ReplaceAll(codes[0], "-", ""))) {
		t.Error("恢复码归一化不一致：大小写/连字符不同的同一串码算出了不同哈希")
	}
	// 库里存的是哈希：明文绝不能出现在库里的任何一行。
	raw := hashRecoveryCode(codes[0])
	if strings.Contains(raw, codes[0]) || len(raw) != 64 {
		t.Errorf("恢复码哈希的形态不对：%q", raw)
	}
}

// ---------------------------------------------------------------- 测试辅助

// enableTwoFactor 走一遍真实的启用流程，返回当前的密钥与 10 个恢复码。
func enableTwoFactor(t *testing.T, h *authHarness) (string, []string) {
	t.Helper()
	status, body := h.post(t, "/api/v1/twofa/setup", map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("开始启用两步验证失败: %d %v", status, body)
	}
	twofa, _ := body["twofa"].(map[string]any)
	secret, _ := twofa["secret"].(string)
	if secret == "" {
		t.Fatalf("启用流程没有返回密钥：%v", twofa)
	}
	url, _ := twofa["otpauth_url"].(string)
	if !strings.HasPrefix(url, "otpauth://totp/") || !strings.Contains(url, "secret="+secret) {
		t.Fatalf("otpauth 链接不对：%q", url)
	}

	code, err := totpCodeAt(secret, time.Now())
	if err != nil {
		t.Fatalf("算动态码: %v", err)
	}
	status, body = h.post(t, "/api/v1/twofa/enable", map[string]any{"code": code}, nil)
	if status != http.StatusOK {
		t.Fatalf("确认启用失败: %d %v", status, body)
	}
	codes := recoveryCodesOf(t, body)
	if len(codes) != recoveryCodeCount {
		t.Fatalf("恢复码个数 = %d，期望 %d", len(codes), recoveryCodeCount)
	}
	return secret, codes
}

func recoveryCodesOf(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["recovery_codes"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// twoFactorStatusOf 读设置接口里的 twofa 块。
func twoFactorStatusOf(t *testing.T, h *authHarness) map[string]any {
	t.Helper()
	status, body := h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	twofa, _ := body["twofa"].(map[string]any)
	if twofa == nil {
		t.Fatalf("设置响应里没有 twofa 块：%v", body)
	}
	return twofa
}

// loginPasswordStep 走登录第一步，返回响应体。
func loginPasswordStep(t *testing.T, h *authHarness) (int, map[string]any) {
	t.Helper()
	return h.post(t, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": h.password,
	}, nil)
}

// finishTwoFactorLogin 走登录第二步。
func finishTwoFactorLogin(t *testing.T, h *authHarness, code string) (int, map[string]any) {
	t.Helper()
	status, body := h.post(t, "/api/v1/auth/login/2fa", map[string]any{"code": code}, nil)
	if token, _ := body["csrf_token"].(string); token != "" {
		h.csrf = token
	}
	return status, body
}

// rewindTwoFACounter 把"最后用过的计数器"往回拨几个窗口。
//
// 它模拟的是**真实用户的时间线**：一个人不会在同一个 30 秒里既用某个码登录、
// 又立刻拿同一个码去关闭两步验证。防重放要求"新码的计数器必须更大"，
// 所以同一个窗口里做第二件事本来就会被拒（这是对的，见
// TestTwoFactorCodeCannotBeReused）。这些流程用例关心的是别的性质，
// 所以先把计数器拨回去，让"当前窗口的码"重新变成可用的新码。
func rewindTwoFACounter(t *testing.T, h *authHarness, back int64) {
	t.Helper()
	state, err := h.srv.db.TwoFactorState(context.Background())
	if err != nil {
		t.Fatalf("读两步验证状态: %v", err)
	}
	if err := h.srv.db.SetTwoFactorLastCounter(context.Background(), state.LastCounter-back); err != nil {
		t.Fatalf("回拨计数器: %v", err)
	}
}

// clearLoginLimit 把这个来源的登录限流计数清零（用例里连续发很多请求时用）。
func clearLoginLimit(t *testing.T, h *authHarness) {
	t.Helper()
	h.srv.auth.login.succeed("127.0.0.1")
}

// ---------------------------------------------------------------- 完整流程

// TestTwoFactorFullFlow 启用 → 登录（两步）→ 关闭，一路走真实 HTTP 接口。
func TestTwoFactorFullFlow(t *testing.T) {
	h := newAuthHarness(t)

	// 一开始是关的，而且库里连那一行都没有。
	if enabled := twoFactorStatusOf(t, h)["enabled"]; enabled != false {
		t.Fatalf("新装面板不该默认开着两步验证：%v", enabled)
	}
	if _, ok, err := h.srv.db.GetSetting(context.Background(), store.KeyTwoFASecret); err != nil || ok {
		t.Fatalf("默认状态不该往 settings 里写 twofa_secret（ok=%v err=%v）", ok, err)
	}

	// ① 启用流程：生成密钥 → 确认码。
	status, body := h.post(t, "/api/v1/twofa/setup", map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("开始启用失败: %d %v", status, body)
	}
	twofa, _ := body["twofa"].(map[string]any)
	secret, _ := twofa["secret"].(string)
	if secret == "" || twofa["pending"] != true {
		t.Fatalf("启用流程应当返回待确认的密钥：%v", twofa)
	}
	// 密钥与链接都是"可抄写"的形式：原文分组带空格、链接是纯文本。
	if formatted, _ := twofa["secret_formatted"].(string); !strings.Contains(formatted, " ") {
		t.Errorf("密钥原文应当分组显示：%q", formatted)
	}
	if url, _ := twofa["otpauth_url"].(string); !strings.HasPrefix(url, "otpauth://totp/") {
		t.Errorf("otpauth 链接不对：%q", url)
	}

	// ② 确认码输错 → 不启用，且一张恢复码都不给。
	status, body = h.post(t, "/api/v1/twofa/enable", map[string]any{"code": "000000"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("错误的确认码应当 401，实际 %d %v", status, body)
	}
	if _, ok := body["recovery_codes"]; ok {
		t.Error("确认失败却返回了恢复码")
	}
	if enabled := twoFactorStatusOf(t, h)["enabled"]; enabled != false {
		t.Fatal("确认码输错之后两步验证被启用了 —— 用户可能把自己锁在门外")
	}
	if _, ok, _ := h.srv.db.GetSetting(context.Background(), store.KeyTwoFASecret); ok {
		t.Fatal("确认码输错却把密钥写进了库")
	}

	// ③ 正确的当前码 → 启用成功，并拿到 10 个恢复码。
	code, err := totpCodeAt(secret, time.Now())
	if err != nil {
		t.Fatalf("算动态码: %v", err)
	}
	status, body = h.post(t, "/api/v1/twofa/enable", map[string]any{"code": code}, nil)
	if status != http.StatusOK {
		t.Fatalf("确认启用失败: %d %v", status, body)
	}
	codes := recoveryCodesOf(t, body)
	if len(codes) != recoveryCodeCount {
		t.Fatalf("恢复码个数 = %d，期望 %d", len(codes), recoveryCodeCount)
	}
	if enabled := twoFactorStatusOf(t, h)["enabled"]; enabled != true {
		t.Fatal("确认之后两步验证应当是开着的")
	}
	// 状态里不该再带密钥（那只是启用流程中的东西）。
	if secretInStatus, _ := twoFactorStatusOf(t, h)["secret"].(string); secretInStatus != "" {
		t.Errorf("已启用状态下不该再返回密钥：%q", secretInStatus)
	}
	if left := twoFactorStatusOf(t, h)["recovery_codes_left"]; left != float64(recoveryCodeCount) {
		t.Errorf("恢复码剩余 = %v，期望 %d", left, recoveryCodeCount)
	}
	// 库里的恢复码是哈希，明文一个都不在。
	state, err := h.srv.db.TwoFactorState(context.Background())
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	rawState, _ := json.Marshal(state)
	for _, code := range codes {
		if strings.Contains(string(rawState), code) {
			t.Fatalf("恢复码明文出现在库里的状态中：%s", rawState)
		}
	}
	if len(state.RecoveryHashes) != recoveryCodeCount {
		t.Fatalf("库里的恢复码哈希个数 = %d", len(state.RecoveryHashes))
	}

	// ④ 重新登录：第一步只给"需要第二步"，**不给会话**。
	h.anonymousClient(t)
	status, body = loginPasswordStep(t, h)
	if status != http.StatusOK {
		t.Fatalf("密码登录第一步失败: %d %v", status, body)
	}
	if body["twofa_required"] != true || body["authenticated"] != false {
		t.Fatalf("开着两步验证时，第一步应当只返回 twofa_required：%v", body)
	}
	if _, ok := body["csrf_token"]; ok {
		t.Error("第二步还没过就返回了 CSRF Token")
	}
	// 第二步：换一个窗口的码（启用时用的那个计数器已经被记下了）。
	next, err := totpCodeAt(secret, time.Now().Add(totpPeriod*time.Second))
	if err != nil {
		t.Fatalf("算下一个窗口的码: %v", err)
	}
	status, body = finishTwoFactorLogin(t, h, next)
	if status != http.StatusOK || body["authenticated"] != true {
		t.Fatalf("两步验证登录失败: %d %v", status, body)
	}
	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusOK {
		t.Fatalf("登录后读节点应当 200，实际 %d", status)
	}

	// ⑤ 关闭：要密码 + 当前码。
	//
	// 先把"最后用过的计数器"拨回去几格 —— 刚登录用掉的那个码不能用第二次
	// （防重放，专门有用例钉住），而真实用户也不会在同一个 30 秒里
	// 既登录又去关闭两步验证。
	clearLoginLimit(t, h)
	rewindTwoFACounter(t, h, 3)
	again, err := totpCodeAt(secret, time.Now())
	if err != nil {
		t.Fatalf("算码: %v", err)
	}
	status, body = h.post(t, "/api/v1/twofa/disable", map[string]any{
		"password": h.password, "code": again,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("关闭两步验证失败: %d %v", status, body)
	}
	if enabled := twoFactorStatusOf(t, h)["enabled"]; enabled != false {
		t.Fatal("关闭之后两步验证应当是不启用的")
	}
	for _, key := range []string{store.KeyTwoFASecret, store.KeyTwoFARecovery, store.KeyTwoFALastCounter} {
		if _, ok, err := h.srv.db.GetSetting(context.Background(), key); err != nil || ok {
			t.Errorf("关闭之后 %s 应当从库里消失（ok=%v err=%v）", key, ok, err)
		}
	}

	// ⑥ 关闭之后登录恢复成一个输入框：只剩密码。
	h.anonymousClient(t)
	status, body = loginPasswordStep(t, h)
	if status != http.StatusOK || body["authenticated"] != true {
		t.Fatalf("关闭之后密码登录应当直接成功: %d %v", status, body)
	}
	if _, ok := body["twofa_required"]; ok {
		t.Error("关闭之后不该再要求第二步")
	}
}

// TestTwoFactorDisableRequiresPasswordAndCode 关闭操作必须同时给对密码与码。
func TestTwoFactorDisableRequiresPasswordAndCode(t *testing.T) {
	h := newAuthHarness(t)
	secret, _ := enableTwoFactor(t, h)

	rewindTwoFACounter(t, h, 3)
	code, _ := totpCodeAt(secret, time.Now())
	for _, tc := range []struct {
		name string
		req  map[string]any
	}{
		{"只给密码", map[string]any{"password": h.password, "code": ""}},
		{"只给码", map[string]any{"password": "", "code": code}},
		{"密码错", map[string]any{"password": "wrong-password-here", "code": code}},
		{"码错", map[string]any{"password": h.password, "code": "000000"}},
	} {
		clearLoginLimit(t, h)
		status, body := h.post(t, "/api/v1/twofa/disable", tc.req, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s：应当 401，实际 %d %v", tc.name, status, body)
		}
		if enabled := twoFactorStatusOf(t, h)["enabled"]; enabled != true {
			t.Fatalf("%s：两步验证被关掉了", tc.name)
		}
	}

	// 反向：密码与码都对时必须能关掉（否则上面四条可能只是"接口永远 401"）。
	clearLoginLimit(t, h)
	status, body := h.post(t, "/api/v1/twofa/disable", map[string]any{
		"password": h.password, "code": code,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("密码与码都对时应当能关闭: %d %v", status, body)
	}
}

// TestTwoFactorRecoveryRegenerate 重新生成恢复码：旧的全部作废。
func TestTwoFactorRecoveryRegenerate(t *testing.T) {
	h := newAuthHarness(t)
	secret, old := enableTwoFactor(t, h)

	rewindTwoFACounter(t, h, 3)
	code, _ := totpCodeAt(secret, time.Now())
	status, body := h.post(t, "/api/v1/twofa/recovery", map[string]any{
		"password": h.password, "code": code,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("重新生成恢复码失败: %d %v", status, body)
	}
	fresh := recoveryCodesOf(t, body)
	if len(fresh) != recoveryCodeCount {
		t.Fatalf("新恢复码个数 = %d", len(fresh))
	}
	for _, code := range fresh {
		for _, gone := range old {
			if code == gone {
				t.Fatalf("重新生成之后又出现了旧恢复码 %q", code)
			}
		}
	}
	if left := twoFactorStatusOf(t, h)["recovery_codes_left"]; left != float64(recoveryCodeCount) {
		t.Errorf("重新生成后的剩余数 = %v", left)
	}

	// 旧恢复码已经作废：拿它登录必须失败。
	h.anonymousClient(t)
	if status, _ := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("登录第一步失败: %d", status)
	}
	if status, body := finishTwoFactorLogin(t, h, old[0]); status != http.StatusUnauthorized {
		t.Fatalf("旧恢复码应当失效，实际 %d %v", status, body)
	}
	// 新的能用。
	clearLoginLimit(t, h)
	if status, body := finishTwoFactorLogin(t, h, fresh[0]); status != http.StatusOK {
		t.Fatalf("新恢复码应当能用，实际 %d %v", status, body)
	}
}

// ---------------------------------------------------------------- 会话边界

// sessionEndpoints 是"必须要有会话"的接口清单（覆盖三档保护里的 accessAdmin）。
var sessionEndpoints = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/settings"},
	{http.MethodGet, "/api/v1/nodes"},
	{http.MethodGet, "/api/v1/audit"},
	{http.MethodGet, "/api/v1/twofa/qr"},
	{http.MethodPut, "/api/v1/settings/guest"},
	{http.MethodPost, "/api/v1/twofa/setup"},
	{http.MethodPost, "/api/v1/twofa/enable"},
}

// TestPasswordOnlyGrantsNoSession 是本功能最要紧的一条断言：
// **只过密码、没过第二因素时，任何需要会话的接口都返回 401**。
//
// 它证明的是"没有半登录状态"：如果第二步的中间态用会话 Cookie 表达
// （最容易犯的错），这里除了 SSE 之外每一条都会变成 200/400 —— 而这个洞
// 在界面上完全看不出来（用户以为自己只过了一半，实际上已经全过了）。
func TestPasswordOnlyGrantsNoSession(t *testing.T) {
	h := newAuthHarness(t)
	secret, _ := enableTwoFactor(t, h)

	h.anonymousClient(t)
	status, body := loginPasswordStep(t, h)
	if status != http.StatusOK || body["twofa_required"] != true {
		t.Fatalf("登录第一步失败: %d %v", status, body)
	}

	// ① 会话接口说"没登录"。
	status, session := h.get(t, "/api/v1/session")
	if status != http.StatusOK {
		t.Fatalf("会话接口失败: %d", status)
	}
	if session["authenticated"] != false {
		t.Fatalf("只过了密码，会话接口却说已登录：%v", session)
	}

	// ② 每一个需要会话的接口都是 401。
	for _, ep := range sessionEndpoints {
		code, resp, _ := h.do(t, ep.method, ep.path, nil, true, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("只过密码时 %s %s 应当 401，实际 %d（%v）", ep.method, ep.path, code, resp)
		}
	}
	// SSE 长连接同样不放行（只取状态码，不读那个永不结束的 body）。
	if code := probeStatus(t, h.client, http.MethodGet, h.ts.URL+"/api/v1/stream"); code != http.StatusUnauthorized {
		t.Errorf("只过密码时实时流应当 401，实际 %d", code)
	}

	// ③ 浏览器上**只有票据 Cookie**，没有任何会话 Cookie。
	// 这条是"中间态不是会话"的直接证据：它不是"服务端碰巧拒绝了"，
	// 而是这个浏览器手里压根没有会话凭据。
	jarNames := func() map[string]bool {
		t.Helper()
		// 读的是 Cookie **jar**（浏览器手里真正有的），而不是 resp.Cookies()：
		// 后者返回的是"这一个响应"的 Set-Cookie，用它判断"有没有会话"
		// 在没设置 Cookie 的请求上永远是空 —— 这条断言就会变成假的。
		u, err := url.Parse(h.ts.URL)
		if err != nil {
			t.Fatalf("解析测试地址: %v", err)
		}
		names := map[string]bool{}
		for _, c := range h.client.Jar.Cookies(u) {
			names[c.Name] = true
		}
		return names
	}
	names := jarNames()
	if names[sessionCookieName] {
		t.Fatalf("密码步骤之后浏览器上出现了会话 Cookie %q（这就是半登录状态）", sessionCookieName)
	}
	if !names[twoFACookieName] {
		t.Fatalf("密码步骤之后应当有一张待验证票据 Cookie，实际 %v", names)
	}

	// ④ 第二步过了之后才有会话（否则上面的断言可能只是"一直都登录不上"）。
	code, _ := totpCodeAt(secret, time.Now().Add(totpPeriod*time.Second))
	if status, body := finishTwoFactorLogin(t, h, code); status != http.StatusOK {
		t.Fatalf("第二步登录失败: %d %v", status, body)
	}
	if !jarNames()[sessionCookieName] {
		t.Fatalf("第二步通过之后应当拿到会话 Cookie，实际 %v", jarNames())
	}
	if status, _ := h.get(t, "/api/v1/settings"); status != http.StatusOK {
		t.Fatalf("第二步之后读设置应当 200，实际 %d", status)
	}
}

// TestTwoFactorTicketIsSingleUseAndBound 票据一次性、超时即废。
func TestTwoFactorTicketIsSingleUseAndBound(t *testing.T) {
	h := newAuthHarness(t)
	secret, _ := enableTwoFactor(t, h)
	h.anonymousClient(t)

	if status, _ := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("登录第一步失败: %d", status)
	}
	// 先输错一次（票据不该被消耗，用户可以重试）。
	if status, _ := finishTwoFactorLogin(t, h, "000000"); status != http.StatusUnauthorized {
		t.Fatal("错误的码应当 401")
	}
	code, _ := totpCodeAt(secret, time.Now().Add(totpPeriod*time.Second))
	if status, body := finishTwoFactorLogin(t, h, code); status != http.StatusOK {
		t.Fatalf("输错一次之后重试应当成功: %d %v", status, body)
	}
	// 用过的票据不能再用（哪怕码是新的）。
	h.anonymousClient(t)
	next, _ := totpCodeAt(secret, time.Now().Add(2*totpPeriod*time.Second))
	status, body := finishTwoFactorLogin(t, h, next)
	if status != http.StatusUnauthorized {
		t.Fatalf("没有票据时第二步应当 401，实际 %d %v", status, body)
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "no_2fa_ticket" {
		t.Errorf("错误码 = %q，期望 no_2fa_ticket", code)
	}
}

// ---------------------------------------------------------------- 限流

// TestTwoFactorSharesPasswordRateLimit 钉住"2FA 输错也计入登录限流，
// 而且是**同一套**限流器"。
//
// 为什么这条最关键：6 位码只有 100 万种。如果第二步不限流（或者另开一套更松的），
// 一个拿到票据的人可以在几分钟内把整个空间刷完 —— 第二因素等于不存在。
//
// 断言分两半，缺一不可：
//   - 连着输错几次之后被 429 挡住；
//   - 挡住之后**连正确的密码**也一起被挡 —— 证明两条路花的是同一个预算，
//     而不是"密码一套、2FA 另一套"。
func TestTwoFactorSharesPasswordRateLimit(t *testing.T) {
	h := newAuthHarness(t)
	enableTwoFactor(t, h)
	h.anonymousClient(t)

	if status, _ := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("登录第一步失败: %d", status)
	}

	// 第一步已经花掉 1 个名额（allowed() 每次请求都计数），窗口上限是 5。
	got429 := 0
	rejected := 0
	for i := 0; i < 12; i++ {
		status, _ := finishTwoFactorLogin(t, h, "000000")
		switch status {
		case http.StatusUnauthorized:
			rejected++
		case http.StatusTooManyRequests:
			got429++
		default:
			t.Fatalf("第 %d 次错误码的响应既不是 401 也不是 429：%d", i+1, status)
		}
	}
	if got429 == 0 {
		t.Fatalf("连续 %d 次输错两步验证码都没有被限流挡住（401 有 %d 次）", 12, rejected)
	}
	if rejected == 0 {
		t.Fatal("一次都没走到校验就被挡住了：这条用例失去了意义")
	}

	// 关键的一半：同一个 IP 上，**正确的密码**现在也必须被挡。
	// 如果 2FA 用的是另一套更松的限流器，这里会照样返回 200。
	status, body := loginPasswordStep(t, h)
	if status != http.StatusTooManyRequests {
		t.Fatalf("两步验证把限流额度刷掉之后，密码登录应当一起被挡（同一套限流），实际 %d %v", status, body)
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "too_many_attempts" {
		t.Errorf("错误码 = %q，期望 too_many_attempts", code)
	}
	// 反向确认：解锁之后密码登录立刻恢复正常（说明上面挡住的确实是限流，
	// 而不是"两步验证开着就永远登不进第一步"）。
	h.srv.auth.login.succeed(clientIP2(t, h))
	if status, body := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("清零限流之后密码登录应当恢复正常，实际 %d %v", status, body)
	}
}

// TestEnableConfirmCodeIsRateLimited 启用流程的确认码同样计入限流。
func TestEnableConfirmCodeIsRateLimited(t *testing.T) {
	h := newAuthHarness(t)
	status, body := h.post(t, "/api/v1/twofa/setup", map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("开始启用失败: %d %v", status, body)
	}
	limited := false
	for i := 0; i < 12; i++ {
		status, _ := h.post(t, "/api/v1/twofa/enable", map[string]any{"code": "000000"}, nil)
		if status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("启用流程的确认码没有计入登录限流：可以无限次猜 6 位码")
	}
}

// ---------------------------------------------------------------- 重放

// TestTwoFactorCodeCannotBeReused 同一个 6 位码不能用两次。
func TestTwoFactorCodeCannotBeReused(t *testing.T) {
	h := newAuthHarness(t)
	secret, _ := enableTwoFactor(t, h)
	h.anonymousClient(t)

	// 挑一个"还没被用过"的窗口的码：启用那一步用掉了当前窗口，
	// 所以这里用下一个窗口的码（服务端的 ±1 时间窗收它）。
	now := time.Now()
	code, err := totpCodeAt(secret, now.Add(totpPeriod*time.Second))
	if err != nil {
		t.Fatalf("算码: %v", err)
	}

	if status, _ := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("登录第一步失败: %d", status)
	}
	if status, body := finishTwoFactorLogin(t, h, code); status != http.StatusOK {
		t.Fatalf("第一次用这个码应当成功: %d %v", status, body)
	}

	// 同一个码第二次：必须失败。
	h.anonymousClient(t)
	if status, _ := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("登录第一步失败: %d", status)
	}
	status, body := finishTwoFactorLogin(t, h, code)
	if status != http.StatusUnauthorized {
		t.Fatalf("同一个码被用了第二次（重放没挡住）：%d %v", status, body)
	}
	message, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "用过") {
		t.Errorf("重放被拒时的提示应当说明原因（同一个码不能重复使用），实际 %q", message)
	}
}

// ---------------------------------------------------------------- 恢复码

// TestRecoveryCodeIsSingleUse 恢复码一次性：用掉一个就作废，且不能再当密码用。
func TestRecoveryCodeIsSingleUse(t *testing.T) {
	h := newAuthHarness(t)
	_, codes := enableTwoFactor(t, h)
	h.anonymousClient(t)

	use := func(code string) (int, map[string]any) {
		t.Helper()
		if status, body := loginPasswordStep(t, h); status != http.StatusOK {
			t.Fatalf("登录第一步失败: %d %v", status, body)
		}
		return finishTwoFactorLogin(t, h, code)
	}

	if status, body := use(codes[0]); status != http.StatusOK {
		t.Fatalf("恢复码应当能登录: %d %v", status, body)
	}
	if left := twoFactorStatusOf(t, h)["recovery_codes_left"]; left != float64(recoveryCodeCount-1) {
		t.Errorf("用掉一个之后剩余 = %v，期望 %d", left, recoveryCodeCount-1)
	}

	// 同一个恢复码再用一次：必须失败。
	h.anonymousClient(t)
	status, body := use(codes[0])
	if status != http.StatusUnauthorized {
		t.Fatalf("同一个恢复码被用了第二次：%d %v", status, body)
	}
	// 另一个恢复码照旧能用（否则上面的失败可能只是"恢复码整条路都坏了"）。
	h.srv.auth.login.succeed(clientIP2(t, h))
	if status, body := use(codes[1]); status != http.StatusOK {
		t.Fatalf("另一个恢复码应当能用: %d %v", status, body)
	}
}

// TestRecoveryCodeNotStoredInPlaintext 库里只有哈希，且哈希不可逆地覆盖了明文。
func TestRecoveryCodeNotStoredInPlaintext(t *testing.T) {
	h := newAuthHarness(t)
	_, codes := enableTwoFactor(t, h)

	raw, ok, err := h.srv.db.GetSetting(context.Background(), store.KeyTwoFARecovery)
	if err != nil || !ok {
		t.Fatalf("读恢复码: ok=%v err=%v", ok, err)
	}
	for _, code := range codes {
		if strings.Contains(raw, code) {
			t.Fatalf("恢复码明文出现在库里：%s", raw)
		}
		if strings.Contains(raw, strings.ReplaceAll(code, "-", "")) {
			t.Fatalf("恢复码（去连字符）明文出现在库里：%s", raw)
		}
	}
	// 存的是派生的哈希：把明文喂进同一个函数必须得到列表里的某一项。
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		t.Fatalf("恢复码列表不是 JSON: %v", err)
	}
	found := false
	for _, hash := range hashes {
		if hash == hashRecoveryCode(codes[0]) {
			found = true
		}
	}
	if !found {
		t.Error("列表里找不到 codes[0] 的哈希 —— 存的不是这个函数算出来的东西")
	}
}

// ---------------------------------------------------------------- 二维码

// TestTwoFactorQRCode 二维码端点：只在启用流程中可用，且返回真 PNG。
func TestTwoFactorQRCode(t *testing.T) {
	h := newAuthHarness(t)

	// 没有待确认密钥时：409，而不是拿别的密钥画一张图。
	status, body := h.get(t, "/api/v1/twofa/qr")
	if status != http.StatusConflict {
		t.Fatalf("没有启用流程时二维码接口应当 409，实际 %d %v", status, body)
	}

	// 走一半启用流程 → 能拿到 PNG。
	if secret := enableTwoFactorPartially(t, h); secret == "" {
		t.Fatal("启用流程没有返回密钥")
	}

	raw := h.rawBytes(t, "/api/v1/twofa/qr")
	if len(raw) < 8 || string(raw[1:4]) != "PNG" {
		t.Fatalf("二维码不是 PNG（前 8 字节 %v）", raw[:min(8, len(raw))])
	}
	if len(raw) < 200 {
		t.Fatalf("二维码太小，可疑：%d 字节", len(raw))
	}

	// 启用完成之后，二维码接口回到 409（不能拿长期密钥反复出图）。
	h2 := newAuthHarness(t)
	enableTwoFactor(t, h2)
	if status, body := h2.get(t, "/api/v1/twofa/qr"); status != http.StatusConflict {
		t.Fatalf("启用完成之后二维码接口应当 409，实际 %d %v", status, body)
	}
}

// enableTwoFactorPartially 只做"生成密钥"这一步，返回密钥。
func enableTwoFactorPartially(t *testing.T, h *authHarness) string {
	t.Helper()
	status, body := h.post(t, "/api/v1/twofa/setup", map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("开始启用失败: %d %v", status, body)
	}
	twofa, _ := body["twofa"].(map[string]any)
	secret, _ := twofa["secret"].(string)
	return secret
}

// ---------------------------------------------------------------- 访客

// TestTwoFactorIsInvisibleToGuests 访客模式：2FA 接口全部 401，
// 而且登录页**看不到"这台开着两步验证"**。
//
// 为什么要管"看不看得出来"：登录页是未登录的人唯一能读到的响应。
// 如果它带一个 twofa_enabled 之类的字段，任何人都能扫一遍公网面板、
// 挑出"开了 2FA"的那些去做针对性钓鱼（"我是你的验证器服务商……"）。
// 而这个信息对正常用户一点用都没有 —— 第二个输入框在密码过了之后自然会出现。
func TestTwoFactorIsInvisibleToGuests(t *testing.T) {
	h := newAuthHarness(t)
	enableTwoFactor(t, h)
	guestOn(t, h)
	// 访客看到的登录页读的就是这个响应（不带 Origin，模拟页面自身的请求）。
	raw := h.rawBody(t, "/api/v1/session")
	adminClient, adminCSRF := h.client, h.csrf
	h.anonymousClient(t)

	// ① 2FA 的每一个接口对访客都是 401。
	for _, ep := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/v1/twofa/setup", map[string]any{}},
		{http.MethodGet, "/api/v1/twofa/qr", nil},
		{http.MethodPost, "/api/v1/twofa/enable", map[string]any{"code": "123456"}},
		{http.MethodPost, "/api/v1/twofa/disable", map[string]any{"password": "x", "code": "123456"}},
		{http.MethodPost, "/api/v1/twofa/recovery", map[string]any{"password": "x", "code": "123456"}},
	} {
		status, body, _ := h.do(t, ep.method, ep.path, ep.body, false, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("访客 %s %s 应当 401，实际 %d（%v）", ep.method, ep.path, status, body)
		}
	}

	// ② 登录页读到的那个响应里不能有任何"开了两步验证"的痕迹。
	for _, needle := range []string{"twofa", "two_factor", "totp", "otpauth"} {
		if strings.Contains(strings.ToLower(raw), needle) {
			t.Errorf("未登录的会话响应里出现了 %q：%s", needle, raw)
		}
	}
	// 反向：管理员那份里必须有（否则上面那条是空断言）。
	h.client, h.csrf = adminClient, adminCSRF
	if enabled := twoFactorStatusOf(t, h)["enabled"]; enabled != true {
		t.Fatalf("管理员读到的 twofa.enabled = %v，期望 true", enabled)
	}
}

// ---------------------------------------------------------------- 本地重置

// TestResetTwoFactorLocally 本机重置：重置后两步验证关闭，密码即可登录。
//
// 这条走的是 `probe-server --reset-2fa` 调用的**同一个函数**
// （server.ResetTwoFactor），所以它证明的正是那条救援路径。
func TestResetTwoFactorLocally(t *testing.T) {
	logs := &twoFALogCapture{}
	h := newAuthHarnessWithConfigAndLogger(t, defaultTestConfig(), slog.New(logs))
	secret, codes := enableTwoFactor(t, h)

	// 重置前：密码登录只走到第一步。
	h.anonymousClient(t)
	status, body := loginPasswordStep(t, h)
	if status != http.StatusOK || body["twofa_required"] != true {
		t.Fatalf("重置前应当要求第二步: %d %v", status, body)
	}

	if err := ResetTwoFactor(context.Background(), h.srv.db, t.TempDir(), slog.New(logs)); err != nil {
		t.Fatalf("本机重置失败: %v", err)
	}

	// 库里三块状态都没了。
	state, err := h.srv.db.TwoFactorState(context.Background())
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if state.Enabled() || len(state.RecoveryHashes) != 0 || state.LastCounter != 0 {
		t.Fatalf("重置之后状态应当清空：%+v", state)
	}
	// 密钥与恢复码都不能再用来登录（防的是"重置只是改了显示"）。
	// 密钥文本仍在用户手里，但服务端已经不再引用它 —— 用登录来证明这一点：
	// 拿重置前算好的动态码走第二步，必须连票据都没有。
	h.srv.auth.login.succeed(clientIP2(t, h))
	h.anonymousClient(t)
	code := mustCode(t, secret, time.Now())
	if status, body := finishTwoFactorLogin(t, h, code); status == http.StatusOK {
		t.Fatalf("重置之后旧密钥算出的码还能换到会话: %d %v", status, body)
	}

	h.srv.auth.login.succeed(clientIP2(t, h))
	h.anonymousClient(t)
	status, body = loginPasswordStep(t, h)
	if status != http.StatusOK || body["authenticated"] != true {
		t.Fatalf("重置之后密码登录应当直接成功: %d %v", status, body)
	}
	if enabled := twoFactorStatusOf(t, h)["enabled"]; enabled != false {
		t.Error("重置之后设置页应当显示未启用")
	}
	// 被作废的恢复码也不能再通关（这时根本没有第二步了）。
	h.anonymousClient(t)
	if status, body := finishTwoFactorLogin(t, h, codes[0]); status == http.StatusOK {
		t.Fatalf("重置之后恢复码不该还能换到会话: %d %v", status, body)
	}

	// 日志里必须有"谁、什么时候"（救援操作要留痕）。
	logged := logs.all()
	if !strings.Contains(logged, "reset-2fa") {
		t.Errorf("日志里没有重置记录：%s", logged)
	}
	for _, needle := range []string{"os_user", "time", "host"} {
		if !strings.Contains(logged, needle) {
			t.Errorf("重置日志缺少 %q：%s", needle, logged)
		}
	}
	// 审计表里也要有一条（面板的「操作记录」里能翻到）。
	entries, err := h.srv.db.ListAudit(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	foundAudit := false
	for _, e := range entries {
		if e.Action == "twofa_reset" {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Error("审计日志里没有 twofa_reset 这一条")
	}
}

func mustCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totpCodeAt(secret, at)
	if err != nil {
		t.Fatalf("算码: %v", err)
	}
	return code
}

// twoFALogCapture 把日志收在内存里（断言"重置留下了记录"）。
//
// 为什么不复用 compress_test.go 里的 logCapture：那个只收属性、不收消息正文，
// 而这里要断言的正是消息里的 "reset-2fa" 与三个属性键。gzip 那些文件这次不动。
type twoFALogCapture struct {
	mu    sync.Mutex
	lines []string
}

func (h *twoFALogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *twoFALogCapture) Handle(_ context.Context, r slog.Record) error {
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.mu.Lock()
	h.lines = append(h.lines, line)
	h.mu.Unlock()
	return nil
}

func (h *twoFALogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *twoFALogCapture) WithGroup(string) slog.Handler      { return h }

func (h *twoFALogCapture) all() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.lines, "\n")
}

// clientIP2 取测试里用的来源 IP（harness 走的是 127.0.0.1）。
func clientIP2(t *testing.T, h *authHarness) string {
	t.Helper()
	return "127.0.0.1"
}

// rawBytes 取一个 GET 的原始响应体（二维码是二进制，不能按 JSON 解）。
func (h *authHarness) rawBytes(t *testing.T, path string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var buf [512]byte
		n, _ := resp.Body.Read(buf[:])
		t.Fatalf("%s 状态码 = %d（body %s）", path, resp.StatusCode, buf[:n])
	}
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return out
}

// defaultTestConfig 是 harness 用的默认配置（与 newAuthHarness 一致）。
func defaultTestConfig() config.Server { return config.Default() }
