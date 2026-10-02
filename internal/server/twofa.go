package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"

	"probe/internal/qr"
	"probe/internal/store"
)

// 这个文件是两步验证的**流程**：启用 / 关闭 / 重新生成恢复码，以及登录的第二步。
// 算法本身在 totp.go，存储形态在 internal/store/twofa.go。
//
// ---------------------------------------------------------------- 待验证状态
//
// "密码过了、第二因素还没过"这个中间态是本功能里最容易做错的地方，
// **绝不能**用一个会话 Cookie 表达它 —— 那等于"密码一过就已经登录了"，
// 第二因素变成摆设：任何需要会话的接口都会照常放行。
//
// 这里的做法是**一次性票据 + 只存哈希**：
//
//	密码校验通过  → 生成 32 字节随机票据，明文只放进 HttpOnly Cookie
//	              （probe_2fa_ticket，5 分钟），服务端内存里只留它的 SHA-256；
//	第二步校验通过 → 删掉票据（一次性），这时才调用 newSession 签发会话。
//
// 为什么是 Cookie 而不是把票据放进 JSON 响应体交给前端保存：
//   - 放响应体意味着前端要把它写进 localStorage/sessionStorage（JS 可读），
//     一个 XSS 就能把它偷走；HttpOnly Cookie 偷不走。
//   - 这个 Cookie **不是会话**：authenticate()（也就是 Require 与 SessionAlive）
//     只认 probe_session，从不看它。所以"只有密码"的浏览器在服务端看来
//     与"什么都没提交过"完全一样 —— 需要会话的接口一律 401
//     （这条有专门的用例钉住：TestPasswordOnlyGrantsNoSession）。
//
// 票据本身是单次、短时、随 IP 变化不失效（手机切网不该把人踢出去）的；
// 猜码的成本由**与密码登录同一套**的 attemptLimiter 决定（见 handleLoginTwoFactor）。
const (
	// twoFACookieName 是"待验证状态"的 Cookie 名。刻意与会话 Cookie 分开：
	// 名字相同的话，任何一个"顺手读一下 probe_session"的地方都会把它当会话。
	twoFACookieName = "probe_2fa_ticket"
	// twoFATicketTTL 是第二次验证的时限。
	twoFATicketTTL = 5 * time.Minute
	// twoFAPendingTTL 是"正在启用"的密钥在内存里的保留时长。
	twoFAPendingTTL = 10 * time.Minute
	// maxTwoFAEntries 是两张内存表的容量上限（防御性：它们的写入者要么是
	// 已登录管理员，要么是刚过密码校验的请求，正常量级是 1~2 条；
	// 封顶是为了不出现"某个脚本反复调用把内存撑大"这种长期隐患）。
	maxTwoFAEntries = 128
	// twoFAIssuer 是验证器 App 里显示的发行方名字。
	twoFAIssuer = "Probe"
	// twoFAQrScale / twoFAQrQuiet 是二维码的渲染参数：每个模块 5 像素、
	// 四周留 4 个模块的静区。
	//
	// 5 像素的来由很具体：一条 otpauth 链接编出来是版本 4~6（33~41 个模块），
	// 加静区后约 205~245 像素 —— 正好是设置页那块地方的自然尺寸。
	// **不缩放**才扫得清楚（前端的 CSS 也刻意没给这张图写死宽高），
	// 而 8 像素会画到 360 像素，在卡片里显得笨重。
	twoFAQrScale = 5
	twoFAQrQuiet = 4
)

// pendingSetup 是一次"正在启用"的两步验证：密钥已经在服务端生成，
// 但用户还没用当前码确认过。
type pendingSetup struct {
	Secret  string
	Expires time.Time
}

// pendingLogin 是一次"密码已过、第二因素未过"的登录。
type pendingLogin struct {
	Username string
	IP       string
	Expires  time.Time
}

// twoFactorStatus 是两步验证的状态（设置页渲染与每次操作后的回执都用它）。
type twoFactorStatus struct {
	Enabled bool `json:"enabled"`
	// Pending 为真表示"密钥已生成、等用户输一次当前码确认"。
	Pending bool `json:"pending"`
	// RecoveryCodesLeft 是还没用过的恢复码个数。
	RecoveryCodesLeft int `json:"recovery_codes_left"`
	// 下面几个只在 Pending 时有值。带上它们是让"刷新设置页"能回到同一个二维码：
	// 密钥本来就绑在这个会话上（见 pendingKey），对同一个管理员显示没有额外暴露。
	Secret          string `json:"secret,omitempty"`
	SecretFormatted string `json:"secret_formatted,omitempty"`
	OTPAuthURL      string `json:"otpauth_url,omitempty"`
	Issuer          string `json:"issuer,omitempty"`
	Account         string `json:"account,omitempty"`
	Digits          int    `json:"digits,omitempty"`
	Period          int    `json:"period,omitempty"`
}

// ---------------------------------------------------------------- 内存状态

// pendingKey 把会话 Token 变成"待确认密钥"的键。
//
// 存**哈希**而不是原文（与票据同一套理由）：这两张表都在内存里，
// 万一被 dump 出来，里面不该有能直接拿去冒充会话的东西。
func pendingKey(sessionToken string) string {
	sum := sha256.Sum256([]byte("twofa-pending\x00" + sessionToken))
	return hex.EncodeToString(sum[:])
}

// ticketKey 同理，把票据明文变成内存里的键。
func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte("twofa-ticket\x00" + ticket))
	return hex.EncodeToString(sum[:])
}

// pruneTwoFALocked 清掉过期的条目，并在超量时淘汰最旧的一条。
//
// 调用方必须已持有 twoFAMu。
func (a *Auth) pruneTwoFALocked(now time.Time) {
	for k, v := range a.twoFAPending {
		if now.After(v.Expires) {
			delete(a.twoFAPending, k)
		}
	}
	for k, v := range a.twoFATickets {
		if now.After(v.Expires) {
			delete(a.twoFATickets, k)
		}
	}
	for len(a.twoFAPending) >= maxTwoFAEntries {
		evictOldestLocked(a.twoFAPending, func(v pendingSetup) time.Time { return v.Expires })
	}
	for len(a.twoFATickets) >= maxTwoFAEntries {
		evictOldestLocked(a.twoFATickets, func(v pendingLogin) time.Time { return v.Expires })
	}
}

// evictOldestLocked 丢掉最早过期的一条（map 顺序随机，这里只求"总是能腾出一格"）。
func evictOldestLocked[V any](m map[string]V, expires func(V) time.Time) {
	var (
		oldestKey string
		oldest    time.Time
	)
	for k, v := range m {
		if oldestKey == "" || expires(v).Before(oldest) {
			oldestKey, oldest = k, expires(v)
		}
	}
	if oldestKey != "" {
		delete(m, oldestKey)
	}
}

// storePendingSetup 记下"这个会话正在启用两步验证"。
func (a *Auth) storePendingSetup(sessionToken, secret string, now time.Time) {
	a.twoFAMu.Lock()
	defer a.twoFAMu.Unlock()
	a.pruneTwoFALocked(now)
	a.twoFAPending[pendingKey(sessionToken)] = pendingSetup{
		Secret:  secret,
		Expires: now.Add(twoFAPendingTTL),
	}
}

// pendingSecret 取出这个会话正在确认的密钥；没有或已过期时 ok=false。
func (a *Auth) pendingSecret(sessionToken string, now time.Time) (string, bool) {
	a.twoFAMu.Lock()
	defer a.twoFAMu.Unlock()
	p, ok := a.twoFAPending[pendingKey(sessionToken)]
	if !ok || now.After(p.Expires) {
		return "", false
	}
	return p.Secret, true
}

// dropPendingSetup 忘掉这个会话的待确认密钥（确认成功、取消、关闭两步验证时调用）。
func (a *Auth) dropPendingSetup(sessionToken string) {
	a.twoFAMu.Lock()
	defer a.twoFAMu.Unlock()
	delete(a.twoFAPending, pendingKey(sessionToken))
}

// issueTicket 生成一张"第二因素待验证"的一次性票据，并把它写进 Cookie。
func (a *Auth) issueTicket(w http.ResponseWriter, r *http.Request, username string, now time.Time) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("生成两步验证票据失败: %w", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)

	a.twoFAMu.Lock()
	a.pruneTwoFALocked(now)
	a.twoFATickets[ticketKey(ticket)] = pendingLogin{
		Username: username,
		IP:       clientIP(r),
		Expires:  now.Add(twoFATicketTTL),
	}
	a.twoFAMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     twoFACookieName,
		Value:    ticket,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cookieSecure(r),
		MaxAge:   int(twoFATicketTTL.Seconds()),
	})
	return nil
}

// lookupTicket 找出这个请求带着的票据（**不消费**：验证码输错时要能重试）。
func (a *Auth) lookupTicket(r *http.Request, now time.Time) (pendingLogin, bool) {
	cookie, err := r.Cookie(twoFACookieName)
	if err != nil || cookie.Value == "" || len(cookie.Value) > maxTokenLen {
		return pendingLogin{}, false
	}
	a.twoFAMu.Lock()
	defer a.twoFAMu.Unlock()
	t, ok := a.twoFATickets[ticketKey(cookie.Value)]
	if !ok {
		return pendingLogin{}, false
	}
	if now.After(t.Expires) {
		delete(a.twoFATickets, ticketKey(cookie.Value))
		return pendingLogin{}, false
	}
	return t, true
}

// consumeTicket 用掉这张票据（成功登录时调用，保证一次性）。
func (a *Auth) consumeTicket(r *http.Request) {
	cookie, err := r.Cookie(twoFACookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	a.twoFAMu.Lock()
	defer a.twoFAMu.Unlock()
	delete(a.twoFATickets, ticketKey(cookie.Value))
}

// clearTicketCookie 清掉浏览器上的票据 Cookie。
func (a *Auth) clearTicketCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     twoFACookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cookieSecure(r),
		MaxAge:   -1,
	})
}

// ---------------------------------------------------------------- 状态

// twoFactorStatusFor 组装设置页要显示的状态。
func (a *Auth) twoFactorStatusFor(ctx context.Context, sessionToken, username string, now time.Time) (twoFactorStatus, error) {
	state, err := a.db.TwoFactorState(ctx)
	if err != nil {
		return twoFactorStatus{}, err
	}
	out := twoFactorStatus{
		Enabled:           state.Enabled(),
		RecoveryCodesLeft: len(state.RecoveryHashes),
	}
	if secret, ok := a.pendingSecret(sessionToken, now); ok {
		out.Pending = true
		out.Secret = secret
		out.SecretFormatted = formatSecret(secret)
		out.OTPAuthURL = otpauthURL(twoFAIssuer, username, secret)
		out.Issuer = twoFAIssuer
		out.Account = username
		out.Digits = totpDigits
		out.Period = totpPeriod
	}
	return out, nil
}

// ---------------------------------------------------------------- 启用

type twoFACodeRequest struct {
	Code string `json:"code"`
}

// handleTwoFASetup 开始启用两步验证：生成密钥、把它绑在当前会话上。
//
// 这里**只生成**，不落库：密钥要等用户用当前码确认过之后才写进 settings
// （见 handleTwoFAEnable）。少了那一步，用户可能把密钥抄错、App 里配错，
// 而服务端已经认定"开着两步验证" —— 下次登录就把自己锁在门外。
//
// 为什么不要求重新输一次密码：这是一次**已登录会话**内的操作，而它本身
// 不降低任何安全性（此刻两步验证还没生效）；真正危险的是"关闭"，
// 那个操作要密码 + 当前码（见 handleTwoFADisable）。
func (a *Auth) handleTwoFASetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userFrom(ctx)
	if !ok {
		a.unauthorized(w, errors.New("未登录"))
		return
	}
	now := time.Now()

	state, err := a.db.TwoFactorState(ctx)
	if err != nil {
		a.log.Error("读取两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if state.Enabled() {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "twofa_enabled", Message: "两步验证已经启用；要换验证器请先关闭它"}})
		return
	}

	secret, err := newTOTPSecret()
	if err != nil {
		a.log.Error("生成两步验证密钥失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.storePendingSetup(user.Token, secret, now)

	status, err := a.twoFactorStatusFor(ctx, user.Token, user.Username, now)
	if err != nil {
		a.log.Error("组装两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"twofa": status})
}

// handleTwoFAQR 把"当前待确认密钥"的 otpauth 链接渲染成 PNG 二维码。
//
// 三条自我约束：
//   - **绝不调用任何在线二维码服务**：链接里装着用户的 TOTP 种子，发给第三方
//     等于把第二因素交出去。所以二维码是服务端用 internal/qr（纯标准库）自己画的。
//   - 只在**启用流程中**可用：没有待确认密钥时返回 409，而不是拿库里的
//     已启用密钥画一张图（那等于把长期种子变成一个随时可取的接口）。
//   - 响应不缓存：no-store。
func (a *Auth) handleTwoFAQR(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		a.unauthorized(w, errors.New("未登录"))
		return
	}
	secret, ok := a.pendingSecret(user.Token, time.Now())
	if !ok {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "no_pending_setup", Message: "当前没有正在进行的启用流程，请先点「启用两步验证」"}})
		return
	}

	code, err := qr.Encode(otpauthURL(twoFAIssuer, user.Username, secret), qr.LevelM)
	if err != nil {
		a.log.Error("生成二维码失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	png, err := code.PNG(twoFAQrScale, twoFAQrQuiet)
	if err != nil {
		a.log.Error("渲染二维码失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(png)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(png); err != nil {
		a.log.Debug("写出二维码时连接已断开", "err", err)
	}
}

// handleTwoFAEnable 用一次"当前的 6 位码"确认启用，并返回一次性恢复码。
//
// 这一步**不能省**（用户可能抄错密钥、App 里配错），也不接受"跳过"：
// 确认失败时一张恢复码都不会生成，库里也不会出现任何密钥。
func (a *Auth) handleTwoFAEnable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userFrom(ctx)
	if !ok {
		a.unauthorized(w, errors.New("未登录"))
		return
	}
	ip := clientIP(r)
	now := time.Now()

	// 确认码也是 6 位码，同样受登录限流约束（理由见 handleLoginTwoFactor）。
	if ok, retry := a.login.allowed(ip, now); !ok {
		a.tooMany(w, retry)
		return
	}

	var req twoFACodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, err)
		return
	}

	secret, ok := a.pendingSecret(user.Token, now)
	if !ok {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "no_pending_setup", Message: "启用流程已超时，请重新点「启用两步验证」"}})
		return
	}

	counter, ok := verifyTOTP(secret, req.Code, now, -1)
	if !ok {
		a.login.fail(ip, now)
		a.log.Warn("两步验证启用确认失败", "ip", ip, "username", user.Username)
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code: "bad_totp_code", Message: "动态码不正确：请确认手机时间准确、App 里选的是「基于时间」的 6 位码"}})
		return
	}

	codes, err := newRecoveryCodes()
	if err != nil {
		a.log.Error("生成恢复码失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if err := a.db.EnableTwoFactor(ctx, secret, hashRecoveryCodes(codes), counter); err != nil {
		a.log.Error("保存两步验证失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.dropPendingSetup(user.Token)
	a.login.succeed(ip)

	if err := a.db.AppendAudit(ctx, "twofa_enable", 0, ip, "启用两步验证（by "+user.Username+"）"); err != nil {
		a.log.Warn("写入审计日志失败", "err", err)
	}
	a.log.Warn("管理员已启用两步验证", "username", user.Username, "ip", ip)

	status, err := a.twoFactorStatusFor(ctx, user.Token, user.Username, now)
	if err != nil {
		a.log.Error("组装两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"twofa":          status,
		"recovery_codes": codes,
	})
}

// ---------------------------------------------------------------- 关闭 / 重新生成

type twoFACredentialsRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// verifyTwoFACredentials 校验"密码 + 第二因素（当前码或一个未用过的恢复码）"。
//
// 关闭与重新生成恢复码都走这里：两个操作都会改变"谁能登录"，
// 都必须同时证明"我知道密码"和"我拿着验证器"。
//
// 返回 (ok, 给用户看的错误消息, 内部错误)。
func (a *Auth) verifyTwoFACredentials(ctx context.Context, req twoFACredentialsRequest, state store.TwoFactorState, now time.Time) (bool, string, error) {
	username, hash, ok, err := a.db.AdminAccount(ctx)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "尚未初始化管理员", nil
	}

	passOK, err := a.verifyWithLimit(ctx, hash, req.Password)
	if err != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		a.log.Error("校验密码失败", "err", err, "username", username)
		passOK = false
	}
	if !passOK {
		return false, "当前密码不正确", nil
	}

	// 第二因素：先试 6 位动态码，再试恢复码。
	if counter, ok := verifyTOTP(state.Secret, req.Code, now, state.LastCounter); ok {
		// 防重放：先把计数器落库再放行。写不进去就不放行（fail closed）——
		// 否则同一个码还能再用一次，"同一个码不能用两次"就成了空话。
		if err := a.db.SetTwoFactorLastCounter(ctx, counter); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	if usedTOTPCode(state.Secret, state.LastCounter, req.Code) {
		return false, "这个动态码刚刚已经用过（同一个码不能重复使用），请等验证器刷新出下一个码再试", nil
	}
	if code := strings.TrimSpace(req.Code); code != "" {
		used, err := a.db.ConsumeRecoveryCode(ctx, hashRecoveryCode(code))
		if err != nil {
			return false, "", err
		}
		if used {
			return true, "", nil
		}
	}
	return false, "动态码或恢复码不正确（恢复码只能用一次）", nil
}

// handleTwoFADisable 关闭两步验证：要【密码 + 当前 6 位码】（或一个未用过的恢复码）。
//
// 为什么关闭比启用严格得多：启用是"往里加一把锁"，关闭是"把锁拆掉" ——
// 一个已经拿到会话的攻击者（比如偷了 Cookie）如果能直接关掉两步验证，
// 那这把锁就形同虚设。所以这里要密码（他偷不到）**和**第二因素。
func (a *Auth) handleTwoFADisable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userFrom(ctx)
	if !ok {
		a.unauthorized(w, errors.New("未登录"))
		return
	}
	ip := clientIP(r)
	now := time.Now()

	if ok, retry := a.login.allowed(ip, now); !ok {
		a.tooMany(w, retry)
		return
	}

	var req twoFACredentialsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, err)
		return
	}
	state, err := a.db.TwoFactorState(ctx)
	if err != nil {
		a.log.Error("读取两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !state.Enabled() {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "twofa_disabled", Message: "两步验证本来就没有启用"}})
		return
	}

	verified, reason, err := a.verifyTwoFACredentials(ctx, req, state, now)
	if err != nil {
		if ctx.Err() != nil {
			a.log.Debug("关闭两步验证的请求在等待校验时被取消")
			return
		}
		a.log.Error("关闭两步验证时校验失败", "err", err, "username", user.Username)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !verified {
		a.login.fail(ip, now)
		a.log.Warn("关闭两步验证被拒", "ip", ip, "username", user.Username, "reason", reason)
		if err := a.db.AppendAudit(ctx, "twofa_disable_failed", 0, ip,
			"关闭两步验证失败（by "+user.Username+"）"); err != nil {
			a.log.Warn("写入审计日志失败", "err", err)
		}
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code: "bad_credentials", Message: reason}})
		return
	}

	if _, err := a.db.ClearTwoFactor(ctx); err != nil {
		a.log.Error("清除两步验证失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.dropPendingSetup(user.Token)
	a.login.succeed(ip)

	if err := a.db.AppendAudit(ctx, "twofa_disable", 0, ip, "关闭两步验证（by "+user.Username+"）"); err != nil {
		a.log.Warn("写入审计日志失败", "err", err)
	}
	a.log.Warn("管理员已关闭两步验证：登录将只校验密码", "username", user.Username, "ip", ip)

	status, err := a.twoFactorStatusFor(ctx, user.Token, user.Username, now)
	if err != nil {
		a.log.Error("组装两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"twofa": status})
}

// handleTwoFARecovery 重新生成恢复码（旧的**全部作废**）：同样要密码 + 当前码。
//
// 为什么要有这条路：恢复码是纸上的东西，抄丢了、用掉几个之后用户会想补一份。
// 没有它的话，"重新生成"只能靠"关闭 + 重新启用"，那会换掉密钥，
// 用户得在每台设备上重新扫一遍。
func (a *Auth) handleTwoFARecovery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userFrom(ctx)
	if !ok {
		a.unauthorized(w, errors.New("未登录"))
		return
	}
	ip := clientIP(r)
	now := time.Now()

	if ok, retry := a.login.allowed(ip, now); !ok {
		a.tooMany(w, retry)
		return
	}

	var req twoFACredentialsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, err)
		return
	}
	state, err := a.db.TwoFactorState(ctx)
	if err != nil {
		a.log.Error("读取两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !state.Enabled() {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "twofa_disabled", Message: "两步验证还没有启用"}})
		return
	}

	verified, reason, err := a.verifyTwoFACredentials(ctx, req, state, now)
	if err != nil {
		if ctx.Err() != nil {
			a.log.Debug("重新生成恢复码的请求在等待校验时被取消")
			return
		}
		a.log.Error("重新生成恢复码时校验失败", "err", err, "username", user.Username)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !verified {
		a.login.fail(ip, now)
		a.log.Warn("重新生成恢复码被拒", "ip", ip, "username", user.Username, "reason", reason)
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code: "bad_credentials", Message: reason}})
		return
	}

	codes, err := newRecoveryCodes()
	if err != nil {
		a.log.Error("生成恢复码失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if err := a.db.SetTwoFactorRecovery(ctx, hashRecoveryCodes(codes)); err != nil {
		a.log.Error("保存恢复码失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.login.succeed(ip)
	if err := a.db.AppendAudit(ctx, "twofa_recovery_regenerate", 0, ip,
		"重新生成两步验证恢复码（by "+user.Username+"，旧的已全部作废）"); err != nil {
		a.log.Warn("写入审计日志失败", "err", err)
	}
	a.log.Info("已重新生成两步验证恢复码", "username", user.Username, "ip", ip)

	status, err := a.twoFactorStatusFor(ctx, user.Token, user.Username, now)
	if err != nil {
		a.log.Error("组装两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"twofa":          status,
		"recovery_codes": codes,
	})
}

// ---------------------------------------------------------------- 登录第二步

// handleLoginTwoFactor 是登录的第二步：拿一次性票据 + 6 位码（或恢复码）换会话。
//
// 三条必须同时成立才有会话：
//  1. 手里有**没过期的一次性票据**（也就是"密码那一步刚刚真的过了一次"）；
//  2. 码校验通过（时间窗 ±1，且同一个计数器没用过）；
//  3. 这次尝试没有被登录限流挡住。
//
// 第 3 条是本功能里最关键的一条：6 位码只有 100 万种，如果没有限流，
// 一个拿到票据的人可以在一分钟内把整个空间刷完。这里用的是**与密码登录
// 完全同一个** attemptLimiter 实例（a.login），不是另开一套更松的 ——
// 两套限流器的表现是"密码撞 5 次就锁，而码可以撞 500 次"，等于没有。
func (a *Auth) handleLoginTwoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := clientIP(r)
	now := time.Now()

	if ok, retry := a.login.allowed(ip, now); !ok {
		a.tooMany(w, retry)
		return
	}

	var req twoFACodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, err)
		return
	}

	ticket, ok := a.lookupTicket(r, now)
	if !ok {
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code:    "no_2fa_ticket",
			Message: "两步验证已超时，请重新输入用户名与密码"}})
		return
	}

	state, err := a.db.TwoFactorState(ctx)
	if err != nil {
		a.log.Error("读取两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !state.Enabled() {
		// 两步验证在这中间被关掉了（本机 --reset-2fa，或另一个标签页里关闭）。
		// 这时**不能**顺手签发会话：票据是在"两步验证开着"的前提下发出的，
		// 而"关掉两步验证"是一次需要密码 + 当前码的显式操作。让用户重新登录一次，
		// 代价极小，却避免了"旧票据在重置之后仍然能换到会话"。
		a.consumeTicket(r)
		a.clearTicketCookie(w, r)
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code:    "no_2fa_ticket",
			Message: "两步验证状态已改变，请重新输入用户名与密码"}})
		return
	}

	verified := false
	if counter, ok := verifyTOTP(state.Secret, req.Code, now, state.LastCounter); ok {
		// 防重放的落库必须在**签发会话之前**：写不进去就不放行。
		if err := a.db.SetTwoFactorLastCounter(ctx, counter); err != nil {
			a.log.Error("记录两步验证计数器失败", "err", err)
			a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
			return
		}
		verified = true
	} else if code := strings.TrimSpace(req.Code); code != "" {
		used, err := a.db.ConsumeRecoveryCode(ctx, hashRecoveryCode(code))
		if err != nil {
			a.log.Error("校验恢复码失败", "err", err)
			a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
			return
		}
		verified = used
	}

	if !verified {
		a.login.fail(ip, now)
		// issued_ip 是"密码那一步是从哪来的"：与本次请求的来源不一致时，
		// 多半是正常的（手机切了网），但也可能是票据被人搬到了别处用 ——
		// 记下来，让这种情况在日志里留痕。
		a.log.Warn("两步验证失败", "ip", ip, "issued_ip", ticket.IP, "username", ticket.Username)
		if err := a.db.AppendAudit(ctx, "login_2fa_failed", 0, ip,
			"两步验证失败（用户名 "+ticket.Username+"）"); err != nil {
			a.log.Warn("写入审计日志失败", "err", err)
		}
		message := "动态码不正确（也可以用一个未使用过的恢复码）"
		if usedTOTPCode(state.Secret, state.LastCounter, req.Code) {
			// 同一个码不能用两次：说清楚，否则用户会去怀疑手机时钟。
			message = "这个动态码刚刚已经用过（同一个码不能重复使用），请等验证器刷新出下一个码再试"
		}
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code: "bad_totp_code", Message: message}})
		return
	}

	// 到位了：票据一次性作废，这时才签发会话。
	a.consumeTicket(r)
	a.clearTicketCookie(w, r)
	a.login.succeed(ip)
	if _, err := a.db.DeleteExpiredSessions(ctx, now); err != nil {
		a.log.Warn("清理过期会话失败", "err", err)
	}
	token, err := a.newSession(ctx, w, r, ip, now)
	if err != nil {
		a.log.Error("创建会话失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.log.Info("管理员登录成功（两步验证）", "username", ticket.Username, "ip", ip)
	if err := a.db.AppendAudit(ctx, "login", 0, ip, "登录成功 "+ticket.Username+"（两步验证）"); err != nil {
		a.log.Warn("写入审计日志失败", "err", err)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      ticket.Username,
		"csrf_token":    csrfFor(token),
	})
}

// ---------------------------------------------------------------- 本地重置

// ResetTwoFactor 是 `probe-server --reset-2fa` 的实现：关掉两步验证（含全部恢复码），
// 并留下"谁、什么时候做的"。
//
// 这是"忘了密码 + 丢了验证器"的唯一出路，所以它**不校验任何凭据** ——
// 密码可能正是忘掉的那一个。安全性由"必须在服务器本机执行"这一条保证
// （能在本机执行命令的人本来就能直接改数据库文件、读走 Agent 上报的数据，
// 多要一道面板密码不会带来任何实际保护，只会让用户永久锁在门外）。
//
// 记录分两处，缺一不可：
//   - 日志（stdout）：本机操作者当场能看到，也留在 journald / 日志文件里；
//   - 审计表：面板的「操作记录」里能翻到，与其它管理操作在同一条时间线上。
//
// 放在 internal/server 而不是 cmd 里，是为了让它能被直接测试
// （见 twofa_test.go 的 TestResetTwoFactorLocally 与 cmd/probe-server 的 CLI 用例）。
func ResetTwoFactor(ctx context.Context, db *store.DB, dataDir string, logger *slog.Logger) error {
	if db == nil {
		return errors.New("数据库不可用")
	}
	cleared, err := db.ClearTwoFactor(ctx)
	if err != nil {
		return fmt.Errorf("清除两步验证失败: %w", err)
	}

	who := "unknown"
	if u, err := user.Current(); err == nil {
		who = u.Username
		if u.Uid != "" {
			who += " (uid " + u.Uid + ")"
		}
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	now := time.Now()

	detail := fmt.Sprintf("本机命令行关闭两步验证并作废全部恢复码（操作者 %s@%s）", who, host)
	if !cleared {
		detail = fmt.Sprintf("本机命令行关闭两步验证：本来就没有启用（操作者 %s@%s）", who, host)
	}
	if err := db.AppendAudit(ctx, "twofa_reset", 0, "local", detail); err != nil {
		logger.Warn("写入审计日志失败", "err", err)
	}

	logger.Warn("已按 --reset-2fa 关闭两步验证：现在登录只需密码，请在登录后立刻重新启用",
		"time", now.Format(time.RFC3339),
		"os_user", who,
		"host", host,
		"data_dir", dataDir,
		"changed", cleared)
	return nil
}
