// Package server 提供 probe-server 的 HTTP 接口与前端静态资源。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"probe/internal/alert"
	"probe/internal/config"
	"probe/internal/fx"
	"probe/internal/state"
	"probe/internal/store"
	"probe/internal/version"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 16 << 10
	maxBodyBytes      = 1 << 20
	healthzPath       = "/healthz"
	apiPrefix         = "/api/"
)

// Server 把配置、数据库、日志与路由组装在一起。
//
// 用 New 构造后可直接拿 Handler() 做测试，不需要真的监听端口。
type Server struct {
	cfg     config.Server
	db      *store.DB
	log     *slog.Logger
	loc     *time.Location
	started time.Time
	state   *state.Store
	agents  *Agents
	auth    *Auth
	hub     *hub
	agg     *accumulator
	traffic *trafficTracker
	// ping 攒住各目标的最近一次探测结果，由流水线每分钟落一行（见 ping.go）。
	ping *pingTracker
	// online 记每个节点「最近一次进入在线状态的时刻」，用来算连续在线时长
	// （见 online.go）。起点每分钟跟着运行态落盘，重启后接着累加。
	online *onlineTracker
	// trafficCache 缓存流量汇总，避免 1 Hz 循环每秒全表聚合（见 traffic_cache.go）。
	trafficCache trafficCache
	// fx 是当前生效的汇率快照（见 fx.go）。用原子指针是因为它只在后台流水线里
	// 每天换一次，而每个 HTTP 请求、每秒的 SSE 推送都要读它 —— 加锁的话
	// 读路径要为一天一次的写入付代价。
	fx       atomic.Pointer[fx.Snapshot]
	engine   *alert.Engine
	dispatch *alert.Dispatcher
	handler  http.Handler

	// routeSpecs 是注册进 mux 的全部路由（见 routes()）。留着它是为了让测试能
	// **枚举**每一条路由并逐条验证保护策略 —— "漏保护一个写接口"是访客模式最可能
	// 的翻车方式，而且是静默翻车：页面上一眼看不出，别人却能改你的机器
	// （见 guest_test.go 的 TestEveryRouteIsProtected）。
	routeSpecs []routeSpec

	// guestMu / guestOn 是「允许访客查看」的进程内缓存（nil = 还没读过库），
	// 详见 guest.go 的 guestAccessEnabled。
	guestMu sync.Mutex
	guestOn *bool
	// guestReads 是访客读接口的粗限流（按来源 IP）。有会话的管理员不走它。
	guestReads *attemptLimiter

	// streamPingEvery 是 SSE 心跳间隔（默认 ssePingInterval）。
	//
	// 心跳里还做一次身份复查（见 api_stream.go 的 streamIdentityValid），
	// 所以测试要把它调小，好让"复查真的发生过一次"能在用例里观察到 ——
	// 15 秒的心跳没法在单测里等。
	streamPingEvery time.Duration

	trustedProxies []*net.IPNet

	mu       sync.Mutex
	listener net.Listener
}

// New 构造 Server。
func New(cfg config.Server, db *store.DB, logger *slog.Logger, loc *time.Location) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if loc == nil {
		loc = time.Local
	}
	trusted, err := config.ParseTrustedProxies(cfg.TrustedProxy)
	if err != nil {
		logger.Warn("--trusted-proxy 配置无效，将不信任任何转发头", "err", err)
	}

	st := state.New()
	agg := newAccumulator(bucketWidth)
	traffic := newTrafficTracker(cfg.TrafficDeltaMax)
	ping := newPingTracker()
	online := newOnlineTracker()
	s := &Server{
		cfg:            cfg,
		db:             db,
		log:            logger,
		loc:            loc,
		started:        time.Now(),
		state:          st,
		hub:            newHub(logger),
		agg:            agg,
		traffic:        traffic,
		ping:           ping,
		online:         online,
		engine:         alert.NewEngine(alertParams(cfg, loc), time.Now()),
		dispatch:       alert.NewDispatcher(logger, nil, alert.DefaultDispatcherOptions()),
		trustedProxies: trusted,
	}
	s.applyNotifiers(alertConfig{})
	// 汇率先放兜底表：**任何**时刻读到的都必须是一份可用的汇率，
	// 否则价格的人民币口径会在这段空窗里显示成 0（见 fx.go 的三级降级）。
	fallback := fx.Default()
	s.fx.Store(&fallback)
	s.agents = NewAgents(cfg, db, st, agg, traffic, ping, logger)
	s.auth = NewAuth(db, cfg, logger, trusted)
	// 会话接口要把「允许访客查看」的当前值告诉前端（前端据此决定显示只读面板
	// 还是登录页）。Auth 自己不查库，回调给 Server —— 开关的缓存只有一份。
	s.auth.guestAccess = s.guestAccessEnabled
	// 会话被撤销（登出、改密）之后要把该会话**已经建立**的 SSE 连接关掉：
	// 那条流不会再经过一次鉴权，不主动关就会继续每秒收到完整 nodeDTO。
	// Auth 不持有 hub（它只认会话），所以也走回调。
	s.auth.onSessionRevoked = s.revokeStreams
	s.guestReads = newAttemptLimiter(guestReadLimit, guestReadWindow, 0, 0)
	s.streamPingEvery = ssePingInterval
	s.handler = s.withMiddleware(s.buildMux())
	return s
}

// revokeStreams 是"会话被撤销"之后的通知口（由 Auth 调用，见 New 里的注入）。
//
// reason 只用于日志（logout / password_change），tokenHash 为空表示"关掉
// 全部管理员连接"（改密时拿不到被注销那几条会话的哈希，见 hub.revokeAdmins）。
func (s *Server) revokeStreams(reason string, tokenHash []byte) {
	if s.hub == nil {
		return
	}
	if len(tokenHash) == 0 {
		s.hub.revokeAdmins(reason)
		return
	}
	s.hub.revokeSession(reason, tokenHash)
}

// Handler 返回完整的处理链。
func (s *Server) Handler() http.Handler { return s.handler }

// State 返回内存状态容器（前端实时视图与测试都会用到）。
func (s *Server) State() *state.Store { return s.state }

// alertParams 把服务端配置翻译成告警参数。
//
// loc 是服务端时区（--timezone）：告警文案里的每个时刻都按它渲染，
// 与面板、定时报告同源（见 alert.Params.Loc）。
func alertParams(cfg config.Server, loc *time.Location) alert.Params {
	params := alert.DefaultParams()
	params.NotifyCooldown = cfg.AlertCooldown
	params.StartupGrace = cfg.AlertStartupGrace
	params.OfflineDebounce = cfg.AlertDebounce
	params.RecoverStable = cfg.AlertRecoverStable
	params.Loc = loc
	return params
}

// Dispatcher 暴露通知流水线（测试与诊断用）。
func (s *Server) Dispatcher() *alert.Dispatcher { return s.dispatch }

// Engine 暴露规则引擎（测试用）。
func (s *Server) Engine() *alert.Engine { return s.engine }

// accessKind 说明一条路由"谁能访问"。
//
// 零值是 accessAdmin —— 这是**刻意**的：有人加路由时忘了标注，得到的是"必须登录"，
// 而不是"公开"。忘了标注的方向只能是更安全的那一边。
type accessKind int

const (
	// accessAdmin：永远需要登录。所有写接口（POST/PUT/PATCH/DELETE）、设置、审计、
	// 以及任何管理类读接口都必须是这一类。
	accessAdmin accessKind = iota
	// accessGuestRead：访客可读 —— 只在「允许访客查看」打开、且没有会话时放行，
	// 而且响应必须经过 guest.go 的白名单脱敏。**只允许标在 GET 上**。
	accessGuestRead
	// accessOpen：与会话无关的路由（登录/初始化/心跳/静态资源/robots/404 兜底）。
	accessOpen
)

// routeSpec 是一条注册进 mux 的路由。
//
// 为什么路由是一张**表**而不是一串直接的 mux.HandleFunc：测试要能枚举每一条路由、
// 逐条验证保护策略（见 guest_test.go 的 TestEveryRouteIsProtected）。"漏保护一个
// 写接口"是访客模式最可能的翻车方式，而且翻得悄无声息 —— 所以路由必须是可枚举的
// 数据；另有一条静态测试钉住"本包里所有 mux.Handle* 调用只能出现在下面那个
// 注册循环里"，免得新路由绕过这张表。
type routeSpec struct {
	Method  string
	Pattern string // ServeMux 的模式（不含方法前缀）
	Access  accessKind
	Handler http.HandlerFunc
}

// wrap 按访问级别给处理函数套上对应的保护。
func (rt routeSpec) wrap(s *Server) http.HandlerFunc {
	switch rt.Access {
	case accessGuestRead:
		return s.guestOrAdmin(rt.Handler)
	case accessOpen:
		// accessOpen 只是"不需要会话"，不是"谁发起的都受理"。
		// 同源校验与有没有会话无关（见 checkSameOrigin），所以这一档也要过。
		return s.checkOrigin(rt.Handler)
	default:
		return s.auth.Require(rt.Handler)
	}
}

// checkOrigin 是 accessOpen 那一档的同源校验。
//
// 为什么这一档也要：这些路由里有**三条写/登录**接口（POST /setup、/auth/login、
// /auth/logout）。它们的跨站可利用性确实受限（JSON 请求体要先过预检），
// 但这层约束是"顺带"来的、不是设计出来的，而且 /healthz 与静态资源同样会受理
// 任何页面发起的请求。既然 checkSameOrigin 的语义是"跨站请求根本不该被受理"，
// 那它就该覆盖全部三档 —— 而不是只在"刚好有人写了校验"的那两条路上生效。
//
// 对不带 Origin 的请求没有影响：运维探针、Agent 的 WS 连接、curl 都不发这个头，
// checkSameOrigin 见到空 Origin 直接放行（理由见它的注释）。推荐的部署方式
// （Caddy / nginx 的 `proxy_set_header Host $host`，见 docs/DEPLOY.md）保留 Host，
// 所以同源判定在反代后面依然成立 —— 这一点本来就被 auth.Require 依赖着。
func (s *Server) checkOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkSameOrigin(r); err != nil {
			s.writeJSON(w, http.StatusForbidden, errorEnvelope{Error: apiError{
				Code: "bad_origin", Message: err.Error()}})
			return
		}
		next(w, r)
	}
}

// routes 是全部路由及其访问级别。
//
// 分类只由 Access 一个字段决定，所以"这条接口公开还是私有"在代码里是一眼可见的
// （而不是散落在各个 handler 里）：读接口里只有 /nodes、/nodes/{id}、/series、
// /ping、/traffic、/overview、/stream 标了 accessGuestRead；**所有写接口一律
// accessAdmin**，与「允许访客查看」这个开关无关。
func (s *Server) routes() []routeSpec {
	routes := []routeSpec{
		// ---- 与会话无关 ----

		// 探活：运维探针要用，且它不含任何节点信息（版本、运行时长、库是否可用）。
		{http.MethodGet, healthzPath, accessOpen, s.handleHealthz},
		// Agent 通道：用自己的 Bearer Token 鉴权，不参与会话/CSRF。
		{http.MethodGet, apiPrefix + "v1/agent/ws", accessOpen, s.agents.Handle},
		// 登录前可访问的接口。
		{http.MethodGet, apiPrefix + "v1/session", accessOpen, s.auth.HandleSession},
		{http.MethodPost, apiPrefix + "v1/setup", accessOpen, s.auth.HandleSetup},
		{http.MethodPost, apiPrefix + "v1/auth/login", accessOpen, s.auth.HandleLogin},
		{http.MethodPost, apiPrefix + "v1/auth/logout", accessOpen, s.auth.HandleLogout},
		// 登录的第二步：这一步必须能在**没有会话**时调用 —— 它换的就是会话，
		// 而"密码已过、第二因素未过"的浏览器手上只有一张一次性票据 Cookie
		// （probe_2fa_ticket），服务端只认它的哈希、5 分钟、用完即废。
		// 它不是会话，也永远不参与 authenticate()。
		{http.MethodPost, apiPrefix + "v1/auth/login/2fa", accessOpen, s.auth.handleLoginTwoFactor},
		// 改密码虽然是 POST，但它本来就要会话（而且处理函数自己也再查一次用户）。
		{http.MethodPost, apiPrefix + "v1/auth/password", accessAdmin, s.auth.HandleChangePassword},

		// ---- 两步验证（全部 accessAdmin：访客一个字节都看不到）----
		//
		// 这一组是**扩大暴露面**的接口：它能读出（启用流程中的）TOTP 种子、
		// 能关掉第二因素。所以四条写入接口与那条画二维码的 GET 都必须是
		// accessAdmin；二维码那条还额外要求"当前会话确实处在启用流程里"
		// （见 handleTwoFAQR），拿不到密钥就画不出图。
		{http.MethodPost, apiPrefix + "v1/twofa/setup", accessAdmin, s.auth.handleTwoFASetup},
		{http.MethodGet, apiPrefix + "v1/twofa/qr", accessAdmin, s.auth.handleTwoFAQR},
		{http.MethodPost, apiPrefix + "v1/twofa/enable", accessAdmin, s.auth.handleTwoFAEnable},
		{http.MethodPost, apiPrefix + "v1/twofa/disable", accessAdmin, s.auth.handleTwoFADisable},
		{http.MethodPost, apiPrefix + "v1/twofa/recovery", accessAdmin, s.auth.handleTwoFARecovery},

		// ---- 访客可读（开关打开时无会话也能访问；响应经白名单脱敏）----
		{http.MethodGet, apiPrefix + "v1/nodes", accessGuestRead, s.handleListNodes},
		// 首页总览：一次给出"所有机器加起来"的合计与每节点最近一小时的探测分桶。
		{http.MethodGet, apiPrefix + "v1/overview", accessGuestRead, s.handleOverview},
		{http.MethodGet, apiPrefix + "v1/nodes/{id}", accessGuestRead, s.handleNodeDetail},
		{http.MethodGet, apiPrefix + "v1/nodes/{id}/series", accessGuestRead, s.handleSeries},
		{http.MethodGet, apiPrefix + "v1/nodes/{id}/ping", accessGuestRead, s.handleNodePing},
		{http.MethodGet, apiPrefix + "v1/nodes/{id}/traffic", accessGuestRead, s.handleTraffic},
		// 实时流：脱敏与 HTTP 响应走同一个函数（见 api_stream.go 的按角色广播）。
		{http.MethodGet, apiPrefix + "v1/stream", accessGuestRead, s.handleStream},

		// ---- 永远需要登录 ----
		{http.MethodPost, apiPrefix + "v1/nodes", accessAdmin, s.handleCreateNode},
		{http.MethodPatch, apiPrefix + "v1/nodes/{id}", accessAdmin, s.handleUpdateNode},
		// PUT 与 PATCH 是同一个处理函数：改标签的界面（设置 → 服务器列表 →「编辑标签」）
		// 按已确认的接口约定用 PUT，而 PATCH 是在用的旧写法（详情页的「编辑」按钮），
		// 两者语义完全一样（整体替换），没必要让其中一个突然 405。
		{http.MethodPut, apiPrefix + "v1/nodes/{id}", accessAdmin, s.handleUpdateNode},
		// 批量重排（设置 → 服务器列表 拖动排序）。
		//
		// 它和上面那条 "PUT /nodes/{id}" 共存：Go 1.22 的 ServeMux 里**字面量路径比
		// 通配更具体**，同一个方法下更具体的模式优先匹配，所以 /nodes/order 不会被
		// {id} 吃掉、注册时也不会 panic。这条依赖"更具体优先"的规则不太显眼，
		// 测试里有专门一条用例钉住它（TestReorderNodesRouteCoexistsWithNodeRoute），
		// 免得以后有人调换顺序或改成前缀匹配时静默坏掉（表现是重排接口变成
		// "节点 ID 非法"的 400）。
		{http.MethodPut, apiPrefix + "v1/nodes/order", accessAdmin, s.handleReorderNodes},
		{http.MethodDelete, apiPrefix + "v1/nodes/{id}", accessAdmin, s.handleDeleteNode},
		{http.MethodPost, apiPrefix + "v1/nodes/{id}/token", accessAdmin, s.handleRotateNodeToken},
		{http.MethodGet, apiPrefix + "v1/audit", accessAdmin, s.handleListAudit},
		{http.MethodGet, apiPrefix + "v1/settings", accessAdmin, s.handleGetSettings},
		{http.MethodPut, apiPrefix + "v1/settings/alert", accessAdmin, s.handlePutAlertSettings},
		{http.MethodPut, apiPrefix + "v1/settings/charts", accessAdmin, s.handlePutChartSettings},
		{http.MethodPut, apiPrefix + "v1/settings/ping", accessAdmin, s.handlePutPingSettings},
		// 「允许访客查看」这个开关自己当然永远是管理接口：访客能改的话，
		// 他就能把开关关掉（或者反过来打开），权限模型当场失效。
		{http.MethodPut, apiPrefix + "v1/settings/guest", accessAdmin, s.handlePutGuestSettings},

		// 设置（Phase 8 先做通知配置，完整设置页在 Phase 9）。
		{http.MethodGet, apiPrefix + "v1/settings/telegram", accessAdmin, s.handleGetTelegramSettings},
		{http.MethodPut, apiPrefix + "v1/settings/telegram", accessAdmin, s.handlePutTelegramSettings},
		{http.MethodPost, apiPrefix + "v1/settings/telegram/test", accessAdmin, s.handleTestTelegram},

		// ---- 静态前端与兜底 ----
		//
		// 前端资源本来就不需要会话（登录页的 HTML/JS/CSS 也得先发下去），
		// 而它们里面没有任何数据。
		{http.MethodGet, "/robots.txt", accessOpen, s.handleRobots},
		{http.MethodGet, "/", accessOpen, s.handleWeb},
	}

	// 未注册的路径：/api/ 下所有响应都必须是 JSON 错误信封。
	// 每个方法都注册一次，否则"路径对方法错"（例如 GET 一个只支持 POST 的接口）
	// 会落到 ServeMux 内置的纯文本 405，前端解析 JSON 会失败、提示变成"未知错误"。
	//
	// 注意不要注册 HEAD：Go 1.22 的 ServeMux 里 GET 模式本身就匹配 HEAD，
	// 再注册 "HEAD /api/" 会因为"方法更少但路径更宽"与具体路由冲突而 panic。
	for _, method := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete,
	} {
		routes = append(routes, routeSpec{method, apiPrefix, accessOpen, s.handleAPINotFound})
	}
	return routes
}

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	s.routeSpecs = s.routes()
	// 全部路由都从这张表注册（含静态资源与 404 兜底）：静态测试钉住
	// "本包里 mux.Handle* 调用只能出现在这一处"，否则新加的路由会绕过枚举测试。
	for _, rt := range s.routeSpecs {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.wrap(s))
	}
	return mux
}

type healthzResponse struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	UptimeSec int64  `json:"uptime_sec"`
	DB        string `json:"db"`
	Time      string `json:"time"`
}

// healthzAnonymous 是**匿名**探针看到的响应体：只有"活着没有"。
//
// 为什么要单独一个形状而不是给 healthzResponse 加 omitempty：这里要表达的是一条
// 安全性质（"未登录的人拿不到版本"），用"零值恰好被省略"来实现它太隐晦 ——
// 换个字段类型、改个 tag 就可能悄悄失效。显式的结构体让"匿名看到什么"在代码里
// 一眼可见，测试也能直接钉住键集合。
type healthzAnonymous struct {
	OK bool   `json:"ok"`
	DB string `json:"db"`
}

// handleHealthz 是探活接口。**保持免鉴权**（运维探针、容器 healthcheck、
// 反代都要在登录之前打得通，改成 401 等于让所有探针失效），
// 但响应体**按身份裁剪**。
//
// 为什么要裁剪：version + commit 组合起来可以直接拿去挑已知漏洞
// （"这个版本有 CVE-x"），而**同一份信息在 /api/v1/settings 里是要登录的** ——
// 一个出口上锁、另一个出口敞着，那道门就等于没装。所以：
//
//	匿名    → {"ok":true,"db":"ok"}（库不可用时 ok:false，状态码 503 不变）
//	有会话  → 照旧带 version / commit / uptime_sec / time
//
// 匿名的响应里连 time 都不给：它是面板进程按 --timezone 渲染的时刻，
// 而时区本身不是访客该知道的东西。库探活的结果（db）必须留着 ——
// 探针靠它判断进程是不是还健康。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// 会话判定走**只读**的 SessionAlive：探针可能每秒打一次，
	// 不该给会话续期（见 auth.SessionAlive）。
	authenticated := s.auth.SessionAlive(r)

	status := http.StatusOK
	dbState := "ok"
	if s.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			dbState = "error"
			status = http.StatusServiceUnavailable
			s.log.Error("数据库健康检查失败", "err", err)
		}
	}

	if !authenticated {
		s.writeJSON(w, status, healthzAnonymous{OK: dbState == "ok", DB: dbState})
		return
	}
	s.writeJSON(w, status, healthzResponse{
		OK:        dbState == "ok",
		Version:   version.Version,
		Commit:    version.Commit,
		UptimeSec: int64(time.Since(s.started).Seconds()),
		DB:        dbState,
		Time:      time.Now().In(s.loc).Format(time.RFC3339),
	})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusNotFound, errorEnvelope{Error: apiError{
		Code:    "not_found",
		Message: "接口不存在",
	}})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

// writeJSON 统一写出 JSON 响应。响应头一旦写出就无法再改成错误码，
// 因此编码失败只记录日志（唯一现实原因是客户端提前断开）。
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Warn("写出 JSON 响应失败", "err", err)
	}
}

// Addr 返回实际监听地址（Run 之前或退出后为 nil；用 :0 时这里是内核分配的真实端口）。
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Run 监听端口并阻塞服务，直到收到退出信号或服务出错。
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.listener = nil
		s.mu.Unlock()
		_ = listener.Close()
	}()

	hs := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	scheme := "http"
	if s.cfg.TLSCert != "" {
		scheme = "https"
	}
	s.log.Info("probe-server 已启动",
		"version", version.Version,
		"scheme", scheme,
		"listen", listener.Addr().String(),
		"data_dir", s.cfg.DataDir,
		"timezone", s.loc.String(),
	)
	if !s.cfg.LoopbackListen() && s.cfg.TLSCert == "" {
		s.log.Warn("正在用明文 HTTP 监听非本机地址：管理员密码与 Agent Token 会明文经过网络，" +
			"请用 Caddy/nginx 提供 TLS，或改用 --tls-cert/--tls-key")
	}

	// 首次运行：生成一次性初始化码（只在日志里出现一次）。
	if err := s.auth.EnsureSetupCode(ctx); err != nil {
		s.log.Error("生成初始化码失败", "err", err)
	}
	// 恢复上次退出前的最后状态（恢复出来的节点一律显示为"未连接"）。
	if n, err := s.seedFromRuntime(ctx); err != nil {
		s.log.Warn("恢复上次的节点状态失败", "err", err)
	} else if n > 0 {
		s.log.Info("已恢复上次的节点状态", "nodes", n)
	}
	// 载入流量基线：增量永远从"上次落盘的基线"算起，因此服务端重启不丢也不重。
	if baselines, err := s.db.TrafficBaselines(ctx); err != nil {
		s.log.Warn("载入流量基线失败", "err", err)
	} else {
		s.traffic.seed(baselines)
	}
	// 载入告警状态（重启不重复轰炸）与通知配置。
	if err := s.loadAlertStates(ctx); err != nil {
		s.log.Warn("载入告警状态失败", "err", err)
	}
	if cfg, err := s.loadAlertConfig(ctx); err != nil {
		s.log.Warn("载入通知配置失败", "err", err)
	} else {
		s.applyNotifiers(cfg)
		if cfg.Enabled && cfg.Token != "" && cfg.ChatID != "" {
			s.log.Info("Telegram 通知已启用")
		}
	}
	if s.cfg.Retention10s < 12*time.Hour {
		s.log.Warn("--retention-10s 短于 12 小时：1h/6h 曲线可能出现空洞",
			"retention_10s", s.cfg.Retention10s.String())
	}
	if s.cfg.Retention1m < 8*24*time.Hour {
		s.log.Warn("--retention-1m 短于 8 天：12h/1d/3d/7d 曲线可能出现空洞",
			"retention_1m", s.cfg.Retention1m.String())
	}

	// 后台流水线：聚合落盘、rollup、运行态、清理 + 每秒的告警评估与实时推送。
	// 只有 2 个 goroutine + 1 个通知发送 worker。
	pushCtx, stopPush := context.WithCancel(ctx)
	// 退出时不只是"发个取消信号"：要**等这三个后台执行体真的退出**才让 Run 返回。
	//
	// 理由很实在：Run 的调用方（cmd/probe-server，以及 internal/e2e 里起真服务端
	// 的脚手架）一拿到返回就会去关数据库、删数据目录。只要还有一个循环在跑
	// （它可能正卡在一次写库中间），"Run 已返回"就是骗人的 —— Linux 上表现为
	// RemoveAll 报 "directory not empty"，而 Windows 的 RemoveAll 宽容，
	// 同样一份代码在本机完全看不出来。
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); s.realtimeLoop(pushCtx) }()
	go func() { defer bg.Done(); s.pipelineLoop(pushCtx) }()
	s.dispatch.Start(pushCtx)

	// stopBackground 幂等：cancel 可以重复调用，等两个已经关上的口子/计数器也是立刻返回。
	stopBackground := func() {
		stopPush()
		<-s.dispatch.Done()
		bg.Wait()
	}
	// defer 这一份覆盖上面所有提前 return 的路径（监听失败、HTTP 异常退出……）。
	defer stopBackground()

	errCh := make(chan error, 1)
	go func() {
		var err error
		if s.cfg.TLSCert != "" {
			err = hs.ServeTLS(listener, s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			err = hs.Serve(listener)
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
		return nil
	case <-ctx.Done():
		s.log.Info("收到退出信号，开始优雅退出", "grace", s.cfg.ShutdownGrace.String())
	}

	// 先让 Agent 立刻收到关闭帧（它们会立即重连），再关掉浏览器连接与 HTTP 服务。
	s.agents.Shutdown("server shutting down")
	// 浏览器的 SSE 是长连接：不主动关掉的话，http.Shutdown 会一直等到宽限期结束，
	// 正常重启被记成"优雅退出超时"（systemd 会当成失败）。
	s.hub.shutdown()

	// 后台流水线先停稳（含"等它们真的退出"），再做收尾落盘：循环是收到取消
	// 信号就走的，不会自己补最后一次落盘，所以这一步不能省；而先停再写，
	// 收尾落盘就不会和还在跑的循环抢同一批内存桶（两边同时 flush 会让同一批
	// 桶重复插入，或者被 requeue 回一个再也没人来取的队列）。
	stopBackground()

	// 用独立的 context 做最后一次落盘：把"已结束但还没等到下一个 tick"的桶写出去。
	fctx, fcancel := context.WithTimeout(context.Background(), 3*time.Second)
	s.flushSamples(fctx)
	s.flushRuntime(fctx)
	s.flushTraffic(fctx)
	s.flushPings(fctx)
	fcancel()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownGrace)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅退出超时: %w", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case <-time.After(time.Second):
		// Shutdown 已返回，监听 goroutine 只是还没来得及回报；不必再等。
	}
	s.log.Info("probe-server 已退出")
	return nil
}
