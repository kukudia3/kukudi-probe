package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"probe/internal/config"
	"probe/internal/store"
)

const (
	// sessionCookieName 是会话 Cookie 名。
	sessionCookieName = "probe_session"
	// sessionTTL 是会话有效期（每次请求滑动续期）。
	sessionTTL = 7 * 24 * time.Hour
	// csrfSuffix 用于从会话 Token 派生 CSRF Token（不需要额外存储）。
	csrfSuffix = "\x00probe-csrf"
	// 密码长度限制。
	minPasswordLen = 10
	maxPasswordLen = 200
	maxUserLen     = 64
	// setupCodeBytes 决定初始化码长度（6 字节 → 12 个十六进制字符）。
	setupCodeBytes = 6
	// verifyConcurrency 限制同时进行的密码校验数量：
	// Argon2id 每次占 64MiB，必须防止并发登录把内存打满。
	verifyConcurrency = 2
	// openRouteLimit / openRouteWindow 是"与会话无关、但每个请求都要打库"
	// 那两条路由（/api/v1/session、/auth/logout）的粗限流：每个来源 IP 每分钟
	// 最多这么多次。
	//
	// 与访客读限流（guest.go 的 guestReadLimit）同一个量级、同一套理由：
	// 正常前端一次页面加载只问一次 /api/v1/session（之后每 30 秒复查一次），
	// 点一次「退出」才发一次 /auth/logout，而它们每次至少一次库操作
	// （会话查询 / 设置读 / 删会话），且**不需要任何凭据**就能触发。
	openRouteLimit  = 300
	openRouteWindow = time.Minute
)

// authUser 是已通过会话鉴权的管理员。
type authUser struct {
	Username string
	// Token 是原始会话 Token（只在服务端内存里流转，用于派生 CSRF Token）。
	Token   string
	Expires time.Time
}

type authUserKey struct{}

// userFrom 从请求上下文取出已鉴权的管理员。
func userFrom(ctx context.Context) (authUser, bool) {
	u, ok := ctx.Value(authUserKey{}).(authUser)
	return u, ok
}

// Auth 负责首次初始化、登录、会话与 CSRF。
type Auth struct {
	db  *store.DB
	cfg config.Server
	log *slog.Logger

	mu          sync.Mutex
	setupCode   string // 明文只存在于进程内存（日志里打印一次，数据库里没有它）
	setupExpiry time.Time
	adminUser   string // 首次读取后缓存，避免每个请求都查库

	login   *attemptLimiter
	setup   *attemptLimiter
	verify  chan struct{} // 限制同时进行的 Argon2 计算（每个占 64MiB）
	trusted []*net.IPNet

	// openReads 是 accessOpen 那两条"每次请求都要打库"的路由的粗限流（按来源 IP）：
	// /api/v1/session 与 /auth/logout（见 openRouteLimit）。
	openReads *attemptLimiter

	// twoFAGlobal 是与来源 IP **无关**的第二因素闸门（见 twofa.go）。
	//
	// 为什么它是独立的一层：a.login 按来源 IP 分桶，拥有 IP 池的人"换个 IP
	// 就是一份新预算"；而这一段的前提本来就是密码已经泄漏，所以第二因素
	// 需要一个不随 IP 变化的速率上限。它是**指针**，测试可以换成小额度。
	twoFAGlobal *attemptLimiter

	// 两步验证的两张**内存表**（算法与流程见 twofa.go）。
	//
	// 为什么放在内存而不是库里：
	//   - twoFAPending（"正在启用"的密钥）本来就不该落库：用户还没确认过它，
	//     落库就等于"库里有一份可能永远用不上的密钥"，而过期清理要另写一套；
	//     重启后作废是**想要**的行为（回到"点一次启用"而已）。
	//   - twoFATickets（"密码过了、第二因素没过"）更是只能放内存：它是一张
	//     5 分钟的一次性票据，落库等于给"半登录状态"留了一份可被 dump 的凭据。
	//
	// 键都是哈希（见 pendingKey / ticketKey），所以这两张表即使被 dump 出来，
	// 里面也没有能直接拿去冒充会话或密钥的东西。
	twoFAMu      sync.Mutex
	twoFAPending map[string]pendingSetup
	twoFATickets map[string]pendingLogin

	// guestAccess 报告「允许访客查看」是否打开（由 Server 注入，见 New）。
	//
	// 为什么是一个回调而不是让 Auth 自己查库：那个开关的进程内缓存只有一份，
	// 放在 Server 上（见 guest.go 的 guestAccessEnabled）；Auth 再造一份就会
	// 出现"设置页存了、会话接口还按旧值回答"这种两处不同步的问题。
	// 为 nil 时按关处理（fail closed）。
	guestAccess func(context.Context) bool

	// onSessionRevoked 在**会话被撤销之后**被调用（由 Server 注入，见 New）。
	//
	// 为什么需要它：鉴权只在请求进入时发生一次，而 SSE 是一条**长连接** ——
	// 登出/改密之后，那条已经建立的流不会自己停下来，它会继续每秒把完整的
	// nodeDTO（含 observed_ip / local_ip / note / boot_id）推给一个已经不是
	// 管理员的浏览器。Auth 只认会话、不认识 hub，所以把"关连接"这件事回调出去
	// （与 guestAccess 同一种做法）。
	//
	// tokenHash 是**被撤销的那个会话**的 Token 哈希；为空表示"拿不到具体是哪几条，
	// 把全部管理员连接都关掉"（改密码就是这种，见 hub.revokeAdmins）。
	onSessionRevoked func(reason string, tokenHash []byte)
}

// guestAccessOn 报告访客开关是否打开（未注入回调时按关处理）。
func (a *Auth) guestAccessOn(ctx context.Context) bool {
	if a.guestAccess == nil {
		return false
	}
	return a.guestAccess(ctx)
}

// revokeStreams 通知"某个会话已经被撤销"（未注入回调时什么也不做）。
func (a *Auth) revokeStreams(reason string, tokenHash []byte) {
	if a.onSessionRevoked == nil {
		return
	}
	a.onSessionRevoked(reason, tokenHash)
}

// SessionAlive 只读地报告"这个请求带着一个仍然有效的会话"。
//
// 与 authenticate 的区别很具体，而且是刻意的：
//   - **不做滑动续期**（authenticate 会按 SessionRenewInterval 写一次库）。
//     心跳每 15 秒复查一次身份，顺手续期等于把 7 天的会话有效期取消掉 ——
//     一条挂着的长连接会让会话永不过期。
//   - 不查管理员用户名（复查只需要"还是不是管理员"，不需要他是谁）。
//
// 两个调用方都用得上这两条：SSE 的心跳复查（api_stream.go）与 /healthz 判断
// "要不要下发版本与 commit"（探针可能每秒打一次，同样不该续期）。
//
// 查不动时返回 false（fail closed）：在"要不要继续把数据发出去"这条路上，
// 取不到答案时唯一安全的方向是当它没登录。
func (a *Auth) SessionAlive(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" || len(cookie.Value) > maxTokenLen {
		return false
	}
	ok, err := a.db.SessionValid(r.Context(), store.HashToken(cookie.Value), time.Now())
	if err != nil {
		a.log.Warn("复查会话失败，本次按失效处理", "err", err)
		return false
	}
	return ok
}

// NewAuth 构造认证组件。trusted 是可信反向代理网段（为空表示不信任任何转发头）。
func NewAuth(db *store.DB, cfg config.Server, log *slog.Logger, trusted []*net.IPNet) *Auth {
	return &Auth{
		db:      db,
		cfg:     cfg,
		log:     log,
		login:   newAttemptLimiter(5, time.Minute, 10, 15*time.Minute),
		setup:   newAttemptLimiter(5, 10*time.Minute, 10, 30*time.Minute),
		verify:  make(chan struct{}, verifyConcurrency),
		trusted: trusted,

		openReads:   newAttemptLimiter(openRouteLimit, openRouteWindow, 0, 0),
		twoFAGlobal: newAttemptLimiter(twoFAGlobalAttempts, twoFAGlobalWindow, 0, 0),

		twoFAPending: make(map[string]pendingSetup),
		twoFATickets: make(map[string]pendingLogin),
	}
}

// NeedsSetup 报告是否还没有管理员账号。
func (a *Auth) NeedsSetup(ctx context.Context) (bool, error) {
	_, _, ok, err := a.db.AdminAccount(ctx)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

// EnsureSetupCode 在没有管理员时生成一次性初始化码并打印到日志。
//
// 初始化码只存在于进程内存（数据库里存的是它的哈希），因此重启会作废重发；
// 这个设计的目的很明确：不允许"第一个连上服务端的人"直接抢注管理员。
func (a *Auth) EnsureSetupCode(ctx context.Context) error {
	needs, err := a.NeedsSetup(ctx)
	if err != nil {
		return err
	}
	if !needs {
		return nil
	}

	raw := make([]byte, setupCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("生成初始化码失败: %w", err)
	}
	code := hex.EncodeToString(raw)

	a.mu.Lock()
	a.setupCode = code
	a.setupExpiry = time.Now().Add(a.cfg.SetupCodeTTL)
	a.mu.Unlock()

	a.log.Warn("尚未初始化管理员：请在网页上用它完成初始化（切勿把这一行贴给别人）",
		"setup_code", code,
		"expires_in", a.cfg.SetupCodeTTL.String(),
		"hint", "初始化完成后这一行不再出现")
	return nil
}

// setupCodeMatches 恒定时间比较初始化码，并检查有效期。
func (a *Auth) setupCodeMatches(code string, now time.Time) bool {
	a.mu.Lock()
	current := a.setupCode
	expiry := a.setupExpiry
	a.mu.Unlock()

	if current == "" || now.After(expiry) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(code), []byte(current)) == 1
}

// csrfFor 从会话 Token 派生 CSRF Token。
//
// 用派生而不是另存一个随机值：少一张表、少一次查询，安全性等价
// ——攻击者拿不到 HttpOnly 的会话 Cookie，就推不出这个值。
func csrfFor(sessionToken string) string {
	sum := sha256.Sum256([]byte(sessionToken + csrfSuffix))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Require 包装需要登录的处理函数：校验会话，再校验同源；写操作还要校验 CSRF。
//
// 同源校验对**所有**请求都生效（不只写操作，也不只有会话的接口）：
// 这一档（accessAdmin）在这里做，accessGuestRead 在 guestOrAdmin 的**最前面**做
// （必须覆盖访客分支，否则未登录的读接口完全不校验），accessOpen 在
// routeSpec.wrap 里做。三档一处不漏 —— "漏一处分支"正是这条规则以前的样子：
// 注释写着"对所有 /api/ 请求都生效"，实现里却只管有会话的那一半。
//
// 为什么跨站 GET 也要挡：虽然读不到响应（我们没有 CORS 头），但"根本不该受理"
// 更简单也更安全——省得以后有人加了一个宽松的 CORS 头就意外打开一个口子。
func (a *Auth) Require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := a.authenticate(r)
		if err != nil {
			a.unauthorized(w, err)
			return
		}
		if err := checkSameOrigin(r); err != nil {
			a.writeJSON(w, http.StatusForbidden, errorEnvelope{Error: apiError{
				Code: "bad_origin", Message: err.Error()}})
			return
		}
		if isMutating(r.Method) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrfFor(user.Token))) != 1 {
				a.writeJSON(w, http.StatusForbidden, errorEnvelope{Error: apiError{
					Code: "bad_csrf", Message: "CSRF 校验失败，请刷新页面后重试"}})
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authUserKey{}, user)))
	}
}

func (a *Auth) authenticate(r *http.Request) (authUser, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return authUser{}, errors.New("未登录")
	}
	if len(cookie.Value) > maxTokenLen {
		return authUser{}, errors.New("会话 Token 过长")
	}
	now := time.Now()
	session, err := a.db.SessionByHash(r.Context(), store.HashToken(cookie.Value), now, sessionTTL)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			return authUser{}, errors.New("会话已失效")
		}
		a.log.Error("读取会话失败", "err", err)
		return authUser{}, errors.New("会话校验失败")
	}

	username, err := a.adminUsername(r.Context())
	if err != nil {
		return authUser{}, err
	}
	return authUser{Username: username, Token: cookie.Value, Expires: time.Unix(session.ExpiresAt, 0)}, nil
}

func (a *Auth) adminUsername(ctx context.Context) (string, error) {
	a.mu.Lock()
	cached := a.adminUser
	a.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	username, _, ok, err := a.db.AdminAccount(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("管理员尚未初始化")
	}
	a.mu.Lock()
	a.adminUser = username
	a.mu.Unlock()
	return username, nil
}

func (a *Auth) unauthorized(w http.ResponseWriter, err error) {
	a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
		Code: "unauthorized", Message: err.Error()}})
}

// HandleSession 返回当前会话状态，前端据此决定显示登录页、初始化页还是首页。
func (a *Auth) HandleSession(w http.ResponseWriter, r *http.Request) {
	// 这条路由是 accessOpen（登录页必须先问它），但每次请求都要打库：
	// 未初始化时要读管理员表，带 Cookie 时还要查一次会话。所以先过粗限流 ——
	// 没有它，未认证的人可以用它把 4 条读连接占满（见 openRouteLimit）。
	if ok, retry := a.openReads.allowed(clientIP(r), time.Now()); !ok {
		a.tooMany(w, retry)
		return
	}
	needsSetup, err := a.NeedsSetup(r.Context())
	if err != nil {
		a.log.Error("查询管理员状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	resp := map[string]any{
		"needs_setup":   needsSetup,
		"authenticated": false,
		"username":      "",
		"csrf_token":    "",
		// 「允许访客查看」的当前值：前端据此决定"没登录时显示只读面板还是登录页"。
		// 它本身不是秘密（访客打开首页就能看出来），而且必须在**登录之前**就能读到
		// —— 这正是前端唯一能问这个问题的时机。
		"guest_access": a.guestAccessOn(r.Context()),
	}
	if !needsSetup {
		if user, err := a.authenticate(r); err == nil {
			resp["authenticated"] = true
			resp["username"] = user.Username
			resp["csrf_token"] = csrfFor(user.Token)
		}
	}
	a.writeJSON(w, http.StatusOK, resp)
}

type setupRequest struct {
	Code     string `json:"code"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// HandleSetup 完成首次初始化：校验一次性初始化码，创建管理员并直接登录。
func (a *Auth) HandleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := clientIP(r)
	now := time.Now()

	if ok, retry := a.setup.allowed(ip, now); !ok {
		a.tooMany(w, retry)
		return
	}
	needsSetup, err := a.NeedsSetup(ctx)
	if err != nil {
		a.log.Error("查询管理员状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !needsSetup {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "already_initialized", Message: "管理员已存在，无法重复初始化"}})
		return
	}

	var req setupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, err)
		return
	}
	if !a.setupCodeMatches(strings.TrimSpace(req.Code), now) {
		a.setup.fail(ip, now)
		a.log.Warn("初始化码校验失败", "ip", ip)
		a.writeJSON(w, http.StatusForbidden, errorEnvelope{Error: apiError{
			Code: "bad_setup_code", Message: "初始化码不正确或已过期（重启服务端可重新生成，见服务端日志）"}})
		return
	}

	username := strings.TrimSpace(req.Username)
	if err := validateCredentials(username, req.Password); err != nil {
		a.badRequest(w, err)
		return
	}

	hash, err := a.hashWithLimit(ctx, req.Password)
	if err != nil {
		a.log.Error("计算密码哈希失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	// 条件创建：并发提交（双击、多标签页、脚本）时只有一个请求能建成管理员，
	// 另一个拿 409 而不是"静默把先建好的账号覆盖掉"。
	created, err := a.db.CreateAdminIfAbsent(ctx, username, hash)
	if err != nil {
		a.log.Error("创建管理员失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !created {
		a.log.Warn("初始化请求竞争失败（已经有管理员）", "ip", ip)
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "already_initialized", Message: "管理员已存在，无法重复初始化"}})
		return
	}
	a.mu.Lock()
	a.setupCode = ""
	a.adminUser = username
	a.mu.Unlock()

	a.setup.succeed(ip)
	if err := a.db.AppendAudit(ctx, "admin_setup", 0, ip, "创建管理员 "+username); err != nil {
		a.log.Warn("写入审计日志失败", "err", err)
	}
	a.log.Info("管理员已完成初始化", "username", username, "ip", ip)

	token, err := a.newSession(ctx, w, r, ip, now)
	if err != nil {
		a.log.Error("创建会话失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "username": username, "csrf_token": csrfFor(token),
	})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	NewPassword2    string `json:"new_password2"`
}

// HandleChangePassword 修改管理员密码。
//
// 安全要点：必须验证当前密码；改完**注销其它所有会话**（只保留当前这条），
// 这样"密码泄漏后改密"才能真正把入侵者踢出去。
func (a *Auth) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := clientIP(r)
	now := time.Now()

	// 这条路由已经被 Require 包过（server.go），用户就在 context 里：
	// 再 authenticate 一次等于多做一次会话查询 + 一次续期写。
	user, ok := userFrom(ctx)
	if !ok {
		a.unauthorized(w, errors.New("未登录"))
		return
	}
	if ok, retry := a.login.allowed(ip, now); !ok {
		a.tooMany(w, retry)
		return
	}

	var req changePasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, err)
		return
	}
	if req.NewPassword != req.NewPassword2 {
		a.badRequest(w, errors.New("两次输入的新密码不一致"))
		return
	}

	username, hash, ok, err := a.db.AdminAccount(ctx)
	if err != nil {
		a.log.Error("读取管理员失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !ok {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "needs_setup", Message: "尚未初始化管理员"}})
		return
	}

	currentOK, err := a.verifyWithLimit(ctx, hash, req.CurrentPassword)
	if err != nil {
		if ctx.Err() != nil {
			// 客户端断开/请求超时：不是服务端故障，也不必刷 Error 日志。
			a.log.Debug("改密请求在等待密码校验时被取消")
			return
		}
		a.log.Error("密码校验失败", "err", err)
		currentOK = false
	}
	if !currentOK {
		a.login.fail(ip, now)
		a.log.Warn("修改密码时当前密码错误", "ip", ip, "username", username)
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code: "bad_credentials", Message: "当前密码不正确"}})
		return
	}
	// 与初始化路径共用同一套密码策略（含"密码不能与用户名相同"）。
	if err := validateCredentials(username, req.NewPassword); err != nil {
		a.badRequest(w, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.NewPassword), []byte(req.CurrentPassword)) == 1 {
		a.badRequest(w, errors.New("新密码不能与当前密码相同"))
		return
	}

	newHash, err := a.hashWithLimit(ctx, req.NewPassword)
	if err != nil {
		if ctx.Err() != nil {
			a.log.Debug("改密请求在等待密码哈希时被取消")
			return
		}
		a.log.Error("计算密码哈希失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if err := a.db.SetAdminAccount(ctx, username, newHash); err != nil {
		a.log.Error("更新管理员密码失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}

	// 只保留当前会话，其余全部注销。
	//
	// 这一步失败必须如实告诉调用方：管理员改密的动机往往是"怀疑密码泄漏"，
	// 如果踢人失败却回 200，他会以为入侵者已经被清掉了。
	removed, err := a.db.DeleteSessionsExcept(ctx, store.HashToken(user.Token))
	if err != nil {
		a.log.Error("注销其它会话失败（密码已改，但其它设备仍处于登录状态）", "err", err, "ip", ip)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code:    "revoke_failed",
			Message: "密码已修改，但注销其它设备的登录状态失败，请重试（必要时重启服务端）"}})
		return
	}
	a.login.succeed(ip)
	// 被注销的那些会话如果正挂着 SSE，靠"下次请求"是拦不住的：那条流不会再请求。
	// 拿不到被注销会话的哈希（DeleteSessionsExcept 只回计数），所以关掉**全部**
	// 管理员连接 —— 当前会话也一起关，它 Cookie 仍有效，会自动重连一次
	// （见 hub.revokeAdmins 里对两个方向代价的说明）。
	a.revokeStreams("password_change", nil)
	if err := a.db.AppendAudit(ctx, "password_change", 0, ip,
		fmt.Sprintf("修改管理员密码（注销其它会话 %d 个）", removed)); err != nil {
		a.log.Warn("写入审计日志失败", "err", err)
	}
	a.log.Info("管理员已修改密码", "username", username, "ip", ip, "revoked_sessions", removed)
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked_sessions": removed})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// HandleLogin 校验账号密码并签发会话。
func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := clientIP(r)
	now := time.Now()

	if ok, retry := a.login.allowed(ip, now); !ok {
		a.tooMany(w, retry)
		return
	}

	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, err)
		return
	}

	username, hash, ok, err := a.db.AdminAccount(ctx)
	if err != nil {
		a.log.Error("读取管理员失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if !ok {
		a.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "needs_setup", Message: "尚未初始化管理员，请先完成初始化"}})
		return
	}

	// 用户名比较也走恒定时间；密码校验失败与用户名错误返回同一条消息。
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(strings.TrimSpace(req.Username))) == 1
	passOK, err := a.verifyWithLimit(ctx, hash, req.Password)
	if err != nil {
		a.log.Error("密码校验失败", "err", err)
		passOK = false
	}
	if !userOK || !passOK {
		a.login.fail(ip, now)
		a.log.Warn("登录失败", "ip", ip, "username_len", len(req.Username))
		// 登录失败也记一笔：暴力破解尝试应当能从界面上一眼看出来。
		// 只记用户名长度，绝不记录密码。
		if err := a.db.AppendAudit(ctx, "login_failed", 0, ip,
			fmt.Sprintf("登录失败（用户名长度 %d）", len(req.Username))); err != nil {
			a.log.Warn("写入审计日志失败", "err", err)
		}
		a.writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: apiError{
			Code: "bad_credentials", Message: "用户名或密码错误"}})
		return
	}

	// 密码这一关过了。**但先别急着签发会话**：如果开着两步验证，这里只是
	// 第一步 —— 会话要等第二步（handleLoginTwoFactor）过了才签发，
	// 中间态由一张一次性票据表达（见 twofa.go 顶部的说明）。
	//
	// 注意 a.login.succeed(ip) 也**不能**在这里调：它是"一次完整登录成功"的
	// 记号，放在这里等于"只要密码对就把失败计数清零"，而攻击者用一个已知的
	// 正确密码就能把 2FA 的限流计数反复洗掉。
	state, err := a.db.TwoFactorState(ctx)
	if err != nil {
		a.log.Error("读取两步验证状态失败", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if state.Enabled() {
		if err := a.issueTicket(w, r, username, now); err != nil {
			a.log.Error("签发两步验证票据失败", "err", err)
			a.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
			return
		}
		a.log.Info("密码校验通过，等待两步验证", "username", username, "ip", ip)
		a.writeJSON(w, http.StatusOK, map[string]any{
			"authenticated":  false,
			"twofa_required": true,
			"username":       username,
			"expires_in":     int(twoFATicketTTL.Seconds()),
		})
		return
	}

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
	a.log.Info("管理员登录成功", "username", username, "ip", ip)
	if err := a.db.AppendAudit(ctx, "login", 0, ip, "登录成功 "+username); err != nil {
		a.log.Warn("写入审计日志失败", "err", err)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "username": username, "csrf_token": csrfFor(token),
	})
}

// HandleLogout 注销当前会话。
//
// 删会话只是"下次请求会被拒"，而已经建立的 SSE 长连接不会再请求第二次 ——
// 所以这里必须**主动**把它关掉（见 revokeStreams）。关的时候不看删除是否成功：
// 关一条流的代价只是浏览器自动重连一次（Cookie 还有效的话会重新鉴权成功），
// 而漏关的代价是一条已经不该存在的流继续收 IP。
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	// 与 HandleSession 同一条理由，但这条更贵：只要带一个**非空** Cookie，
	// 未认证的请求就会查一次会话、并在唯一那条写连接上删一次会话
	// （DeleteSession）。所以限流必须在碰 Cookie 之前。
	if ok, retry := a.openReads.allowed(clientIP(r), time.Now()); !ok {
		a.tooMany(w, retry)
		return
	}
	username := ""
	if user, err := a.authenticate(r); err == nil {
		username = user.Username
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		hash := store.HashToken(cookie.Value)
		if err := a.db.DeleteSession(r.Context(), hash); err != nil {
			a.log.Warn("删除会话失败", "err", err)
		}
		a.revokeStreams("logout", hash)
	}
	if username != "" {
		if err := a.db.AppendAudit(r.Context(), "logout", 0, clientIP(r), "退出登录 "+username); err != nil {
			a.log.Warn("写入审计日志失败", "err", err)
		}
	}
	a.clearCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) newSession(ctx context.Context, w http.ResponseWriter, r *http.Request, ip string, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成会话 Token 失败: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := a.db.CreateSession(ctx, store.HashToken(token), sessionTTL, now, ip, r.UserAgent()); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cookieSecure(r),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return token, nil
}

// cookieSecure 判断"这次请求是不是走的加密通道"。
//
// 判断逻辑在 requestIsHTTPS（middleware.go）：HSTS 头与它必须同源，
// 否则会出现"同一个响应里 Cookie 带了 Secure、HSTS 却是空的"这种自相矛盾。
func (a *Auth) cookieSecure(r *http.Request) bool {
	return requestIsHTTPS(r, a.trusted)
}

func (a *Auth) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cookieSecure(r),
		MaxAge:   -1,
	})
}

// hashWithLimit / verifyWithLimit 把昂贵的 Argon2 计算限制在 2 个并发。
//
// 等待时响应 ctx 取消：客户端断开或请求超时后不该继续排队，
// 更不该在轮到自己时白算一次 64MiB 的哈希（否则很容易被堆满 goroutine）。
func (a *Auth) hashWithLimit(ctx context.Context, password string) (string, error) {
	if err := a.acquireVerify(ctx); err != nil {
		return "", err
	}
	defer func() { <-a.verify }()
	return hashPassword(password)
}

func (a *Auth) verifyWithLimit(ctx context.Context, hash, password string) (bool, error) {
	if err := a.acquireVerify(ctx); err != nil {
		return false, err
	}
	defer func() { <-a.verify }()
	return verifyPassword(hash, password)
}

func (a *Auth) acquireVerify(ctx context.Context) error {
	select {
	case a.verify <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Auth) badRequest(w http.ResponseWriter, err error) {
	a.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{Code: "bad_request", Message: err.Error()}})
}

func (a *Auth) tooMany(w http.ResponseWriter, retry time.Duration) {
	seconds := int(retry.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	a.writeJSON(w, http.StatusTooManyRequests, errorEnvelope{Error: apiError{
		Code: "too_many_attempts", Message: fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", seconds)}})
}

func (a *Auth) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.log.Warn("写出 JSON 响应失败", "err", err)
	}
}

// validateCredentials 校验用户名与密码的基本要求。
//
// 长度按**字符**（rune）算，与界面提示（"至少 10 位"、"最多 64 位"）和
// store 的截断语义保持一致：按字节算的话，4 个汉字的密码（12 字节）能通过
// "至少 10 位"，而 22 个汉字的用户名（66 字节）会被误判成超长。
func validateCredentials(username, password string) error {
	switch {
	case username == "":
		return errors.New("用户名不能为空")
	case utf8.RuneCountInString(username) > maxUserLen:
		return fmt.Errorf("用户名长度不能超过 %d 位", maxUserLen)
	case password == username:
		return errors.New("密码不能与用户名相同")
	}
	return validatePassword(password)
}

func validatePassword(password string) error {
	length := utf8.RuneCountInString(password)
	switch {
	case length < minPasswordLen:
		return fmt.Errorf("密码至少 %d 位", minPasswordLen)
	case length > maxPasswordLen:
		return fmt.Errorf("密码长度不能超过 %d 位", maxPasswordLen)
	}
	return nil
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// checkSameOrigin 校验 Origin 与 Host 一致（浏览器跨站请求会带上 Origin）。
// 没有 Origin 的请求（curl、Agent）不做限制——它们本来就不带 Cookie 自动发送。
func checkSameOrigin(r *http.Request) error {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return errors.New("Origin 头不合法")
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return fmt.Errorf("Origin %q 与 Host %q 不一致", u.Host, r.Host)
	}
	return nil
}

// decodeJSON 按大小上限解析 JSON 请求体，并拒绝未知字段拼错造成的静默失败。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求内容不合法: %w", err)
	}
	return nil
}

// attemptLimiter 是按 key（这里是来源 IP）计数的登录/初始化限流器。
//
// 规则：窗口内最多 max 次尝试；连续失败 lockAfter 次后锁定 lockFor。
type attemptLimiter struct {
	max       int
	window    time.Duration
	lockAfter int
	lockFor   time.Duration

	mu      sync.Mutex
	entries map[string]*attemptEntry
}

type attemptEntry struct {
	windowStart time.Time
	count       int
	consecutive int
	lockedUntil time.Time
	lastSeen    time.Time
}

func newAttemptLimiter(max int, window time.Duration, lockAfter int, lockFor time.Duration) *attemptLimiter {
	return &attemptLimiter{
		max: max, window: window, lockAfter: lockAfter, lockFor: lockFor,
		entries: make(map[string]*attemptEntry),
	}
}

// lockoutOnlyMax 是"只做连续失败锁定"的限流器的窗口额度：大到不可能被触到。
//
// 这些调用方数的是**失败**（Agent 鉴权失败），正常流量（同一 NAT 后面十几台
// Agent 一起重连）不该因为"成功"被扣额度 —— 所以窗口配额这一半是被刻意让开的，
// 真正生效的是 fail/succeed 与 lockedUntil（见 agentconn.go 的 authFails）。
const lockoutOnlyMax = 1 << 30

// newLockoutLimiter 构造一个只做"连续失败即锁定"的限流器。
func newLockoutLimiter(lockAfter int, lockFor time.Duration) *attemptLimiter {
	return newAttemptLimiter(lockoutOnlyMax, time.Minute, lockAfter, lockFor)
}

func (l *attemptLimiter) allowed(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(key, now)

	if now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	if now.Sub(e.windowStart) >= l.window {
		e.windowStart = now
		e.count = 0
	}
	if e.count >= l.max {
		return false, l.window - now.Sub(e.windowStart)
	}
	e.count++
	return true, 0
}

func (l *attemptLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(key, now)
	e.consecutive++
	if l.lockAfter > 0 && e.consecutive >= l.lockAfter {
		e.lockedUntil = now.Add(l.lockFor)
		e.consecutive = 0
	}
}

func (l *attemptLimiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok {
		e.count = 0
		e.consecutive = 0
		e.lockedUntil = time.Time{}
	}
}

// entry 取出（或新建）某个 key 的状态，并顺手做一次规模控制。
//
// 表大小必须有上限：来源 IP 是外部可控的，不封顶就是内存放大。
// 但**不能整体清空**：那等于把所有人的锁定期（15 分钟登录锁、30 分钟初始化锁）
// 一起抹掉——攻击者用 IP 池灌满 1024 条就能随时重置自己的锁定状态。
// 这里改成"每次只淘汰一条"：优先淘汰最久没出现且未锁定的，其次淘汰最早解锁的。
func (l *attemptLimiter) entry(key string, now time.Time) *attemptEntry {
	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= maxLimiterEntries {
			l.evictOne(now)
		}
		e = &attemptEntry{windowStart: now}
		l.entries[key] = e
	}
	e.lastSeen = now
	return e
}

// maxLimiterEntries 是限流表的上限（约 1024 个来源）。
const maxLimiterEntries = 1024

// evictOne 丢掉一条最"没用"的记录，保证表大小严格封顶。
func (l *attemptLimiter) evictOne(now time.Time) {
	var (
		oldestKey   string
		oldestSeen  time.Time
		soonestKey  string
		soonestLock time.Time
	)
	for k, v := range l.entries {
		if v.lockedUntil.IsZero() || !v.lockedUntil.After(now) {
			// 未锁定（或锁定已过期）：优先按"最久没出现"淘汰。
			if oldestKey == "" || v.lastSeen.Before(oldestSeen) {
				oldestKey, oldestSeen = k, v.lastSeen
			}
			continue
		}
		// 锁定中：记录下来，只有在没有未锁定记录时才动它。
		if soonestKey == "" || v.lockedUntil.Before(soonestLock) {
			soonestKey, soonestLock = k, v.lockedUntil
		}
	}
	switch {
	case oldestKey != "":
		delete(l.entries, oldestKey)
	case soonestKey != "":
		delete(l.entries, soonestKey)
	}
}
