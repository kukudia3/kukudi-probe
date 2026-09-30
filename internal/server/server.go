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
	"syscall"
	"time"

	"probe/internal/alert"
	"probe/internal/config"
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
	// trafficCache 缓存流量汇总，避免 1 Hz 循环每秒全表聚合（见 traffic_cache.go）。
	trafficCache trafficCache
	engine       *alert.Engine
	dispatch     *alert.Dispatcher
	handler      http.Handler

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
		engine:         alert.NewEngine(alertParams(cfg), time.Now()),
		dispatch:       alert.NewDispatcher(logger, nil, alert.DefaultDispatcherOptions()),
		trustedProxies: trusted,
	}
	s.applyNotifiers(alertConfig{})
	s.agents = NewAgents(cfg, db, st, agg, traffic, ping, logger)
	s.auth = NewAuth(db, cfg, logger, trusted)
	s.handler = s.withMiddleware(s.buildMux())
	return s
}

// Handler 返回完整的处理链。
func (s *Server) Handler() http.Handler { return s.handler }

// State 返回内存状态容器（前端实时视图与测试都会用到）。
func (s *Server) State() *state.Store { return s.state }

// alertParams 把服务端配置翻译成告警参数。
func alertParams(cfg config.Server) alert.Params {
	params := alert.DefaultParams()
	params.NotifyCooldown = cfg.AlertCooldown
	params.StartupGrace = cfg.AlertStartupGrace
	params.OfflineDebounce = cfg.AlertDebounce
	params.RecoverStable = cfg.AlertRecoverStable
	return params
}

// Dispatcher 暴露通知流水线（测试与诊断用）。
func (s *Server) Dispatcher() *alert.Dispatcher { return s.dispatch }

// Engine 暴露规则引擎（测试用）。
func (s *Server) Engine() *alert.Engine { return s.engine }

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+healthzPath, s.handleHealthz)

	// Agent 通道：用自己的 Bearer Token 鉴权，不参与会话/CSRF。
	mux.HandleFunc("GET "+apiPrefix+"v1/agent/ws", s.agents.Handle)

	// 登录前可访问的接口。
	mux.HandleFunc("GET "+apiPrefix+"v1/session", s.auth.HandleSession)
	mux.HandleFunc("POST "+apiPrefix+"v1/setup", s.auth.HandleSetup)
	mux.HandleFunc("POST "+apiPrefix+"v1/auth/login", s.auth.HandleLogin)
	mux.HandleFunc("POST "+apiPrefix+"v1/auth/logout", s.auth.HandleLogout)
	mux.HandleFunc("POST "+apiPrefix+"v1/auth/password", s.auth.Require(s.auth.HandleChangePassword))

	// 需要登录的接口。
	mux.HandleFunc("GET "+apiPrefix+"v1/nodes", s.auth.Require(s.handleListNodes))
	// 首页总览：一次给出"所有机器加起来"的合计与每节点最近一小时的探测分桶。
	mux.HandleFunc("GET "+apiPrefix+"v1/overview", s.auth.Require(s.handleOverview))
	mux.HandleFunc("POST "+apiPrefix+"v1/nodes", s.auth.Require(s.handleCreateNode))
	mux.HandleFunc("GET "+apiPrefix+"v1/nodes/{id}", s.auth.Require(s.handleNodeDetail))
	mux.HandleFunc("GET "+apiPrefix+"v1/nodes/{id}/series", s.auth.Require(s.handleSeries))
	mux.HandleFunc("GET "+apiPrefix+"v1/nodes/{id}/ping", s.auth.Require(s.handleNodePing))
	mux.HandleFunc("GET "+apiPrefix+"v1/nodes/{id}/traffic", s.auth.Require(s.handleTraffic))
	mux.HandleFunc("PATCH "+apiPrefix+"v1/nodes/{id}", s.auth.Require(s.handleUpdateNode))
	// PUT 与 PATCH 是同一个处理函数：改标签的界面（设置 → 服务器列表 →「编辑标签」）
	// 按已确认的接口约定用 PUT，而 PATCH 是在用的旧写法（详情页的「编辑」按钮），
	// 两者语义完全一样（整体替换），没必要让其中一个突然 405。
	mux.HandleFunc("PUT "+apiPrefix+"v1/nodes/{id}", s.auth.Require(s.handleUpdateNode))
	mux.HandleFunc("DELETE "+apiPrefix+"v1/nodes/{id}", s.auth.Require(s.handleDeleteNode))
	mux.HandleFunc("POST "+apiPrefix+"v1/nodes/{id}/token", s.auth.Require(s.handleRotateNodeToken))
	mux.HandleFunc("GET "+apiPrefix+"v1/audit", s.auth.Require(s.handleListAudit))
	mux.HandleFunc("GET "+apiPrefix+"v1/settings", s.auth.Require(s.handleGetSettings))
	mux.HandleFunc("PUT "+apiPrefix+"v1/settings/alert", s.auth.Require(s.handlePutAlertSettings))
	mux.HandleFunc("PUT "+apiPrefix+"v1/settings/charts", s.auth.Require(s.handlePutChartSettings))
	mux.HandleFunc("PUT "+apiPrefix+"v1/settings/ping", s.auth.Require(s.handlePutPingSettings))
	mux.HandleFunc("GET "+apiPrefix+"v1/stream", s.auth.Require(s.handleStream))

	// 设置（Phase 8 先做通知配置，完整设置页在 Phase 9）。
	mux.HandleFunc("GET "+apiPrefix+"v1/settings/telegram", s.auth.Require(s.handleGetTelegramSettings))
	mux.HandleFunc("PUT "+apiPrefix+"v1/settings/telegram", s.auth.Require(s.handlePutTelegramSettings))
	mux.HandleFunc("POST "+apiPrefix+"v1/settings/telegram/test", s.auth.Require(s.handleTestTelegram))

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
		mux.HandleFunc(method+" "+apiPrefix, s.handleAPINotFound)
	}
	s.registerWeb(mux)
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

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	resp := healthzResponse{
		OK:        true,
		Version:   version.Version,
		Commit:    version.Commit,
		UptimeSec: int64(time.Since(s.started).Seconds()),
		DB:        "ok",
		Time:      time.Now().In(s.loc).Format(time.RFC3339),
	}
	status := http.StatusOK
	if s.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			resp.OK = false
			resp.DB = "error"
			status = http.StatusServiceUnavailable
			s.log.Error("数据库健康检查失败", "err", err)
		}
	}
	s.writeJSON(w, status, resp)
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
	defer stopPush()
	go s.realtimeLoop(pushCtx)
	go s.pipelineLoop(pushCtx)
	s.dispatch.Start(pushCtx)

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

	// 后台流水线是被 stopPush 停掉的（defer 在 Run 返回时才执行），所以这里
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
