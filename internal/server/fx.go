package server

import (
	"context"
	"time"

	"probe/internal/fx"
	"probe/internal/store"
)

// fxEvery 是取汇率的周期：**每天一次**。
//
// 数据源本身就是日更的（欧洲央行体系的工作日汇率、exchangerate-api 的 24 小时
// 缓存），取更勤只会白白出网。启动时另取一次（见 pipelineLoop 的开头），
// 所以"今天已经取过、进程重启了"也只会多一次请求，不会漏掉当天的新值。
//
// 是变量而不是常量：测试要把它调小才能验"周期到了真的会再取一次"这件事
// （否则那条用例只能靠等 24 小时，等于没有）。产品代码里没有任何地方改它。
var fxEvery = 24 * time.Hour

// fxFetchBudget 是一次刷新（试完全部数据源）的总预算。
//
// 刷新是**内联**跑在 pipelineLoop 那个 goroutine 里的（不为它单开 goroutine，
// 见 pipelineLoop 的说明）：加上这个上限，最坏情况也只是让落盘晚几秒，
// 不会因为对端一直不吭声而把整条后台流水线拖住。
const fxFetchBudget = 12 * time.Second

// fxFetch 按顺序试数据源，第一个成功的就用。抽成变量是为了测试能换掉它
// ——但真正让测试能注入的其实是 cfg.FXRateURL（指向本地 httptest 服务），
// 这个钩子只用于"连 httptest 都不想要"的极端场景。
var fxFetch = func(ctx context.Context, urls []string) (fx.Snapshot, error) {
	return fx.Client{URLs: urls}.Fetch(ctx)
}

// fxCurrent 返回当前生效的汇率快照（永远非 nil：New 里已放好兜底表）。
func (s *Server) fxCurrent() fx.Snapshot {
	if snap := s.fx.Load(); snap != nil {
		return *snap
	}
	return fx.Default()
}

// fxStore 把快照换成新的那一份（内存 + 落库）。
//
// 落库失败**不**回滚内存：这一份已经取到了、方向也对，先用上；
// 大不了下次重启回到上一份（或兜底表），而"取到了却不用"是纯粹的倒退。
func (s *Server) fxStore(ctx context.Context, snap fx.Snapshot) {
	s.fx.Store(&snap)
	encoded, err := snap.Encode()
	if err != nil {
		s.log.Warn("汇率快照编码失败（本次只在内存里生效）", "err", err)
		return
	}
	if err := s.db.SetSetting(ctx, store.KeyFXRates, encoded); err != nil {
		s.log.Warn("汇率快照落库失败（本次只在内存里生效，重启后会回到上一份）", "err", err)
	}
}

// loadStoredFX 把上次落库的汇率读回内存。
//
// 读失败/内容坏掉都只是"继续用兜底表"：**绝不**让一个显示口径把服务端拦在启动路上。
func (s *Server) loadStoredFX(ctx context.Context) bool {
	raw, ok, err := s.db.GetSetting(ctx, store.KeyFXRates)
	if err != nil {
		s.log.Warn("读取已存的汇率失败，本次用内置兜底表", "err", err)
		return false
	}
	if !ok {
		return false
	}
	snap, ok := fx.Decode(raw)
	if !ok {
		// 手工改坏、或老版本写下的别的结构。不删它：下一次取成功会整体覆盖。
		s.log.Warn("已存的汇率内容无法解析，本次用内置兜底表", "key", store.KeyFXRates)
		return false
	}
	s.fx.Store(&snap)
	return true
}

// refreshFX 取一次汇率，按**三级降级**处理结果：
//
//	① 取成功        → 用新的，并落库（内存里换成新的那一份）
//	② 取失败但有旧值 → 继续用旧的（哪怕过期），只记一条 debug 日志
//	③ 从没取到过     → 用内置的默认汇率表（fx.Default，写死在代码里）
//
// 三条路的共同结果是"内存里永远有一份可用的汇率表"，因此价格**永远不会**
// 因为取不到汇率而变成 0、空白或报错。失败只记一条日志、不刷屏，
// 也不影响同一个循环里的其它定时任务（落盘、rollup、清理照常跑）。
func (s *Server) refreshFX(ctx context.Context) {
	if !s.cfg.FX {
		// 关掉之后连网络都不碰：这一份（上次取到的、或兜底表）继续生效。
		s.log.Debug("汇率自动获取已关闭（--fx=false / PROBE_FX=0），继续使用当前生效的那一份",
			"is_default", s.fxCurrent().IsDefault)
		return
	}

	fctx, cancel := context.WithTimeout(ctx, fxFetchBudget)
	defer cancel()
	snap, err := fxFetch(fctx, s.cfg.FXRateURLs())
	if err != nil {
		// 只此一条：两个数据源各失败一次也只汇总成这一行（errors.Join）。
		current := s.fxCurrent()
		s.log.Debug("取汇率失败，继续使用上一份",
			"err", err, "is_default", current.IsDefault, "date", current.Date, "source", current.Source)
		return
	}
	s.fxStore(ctx, snap)
	s.log.Info("汇率已更新", "date", snap.Date, "source", snap.Source, "currencies", len(snap.Rates))
}

// applyFX 给节点 DTO 补上"人民币口径"的三个字段。
//
// 放在 dtoFor 里（而不是 buildNodeDTO 里）：buildNodeDTO 是不碰服务端状态的纯函数
// （测试直接调它），汇率是进程级状态，只有组装对外视图的那一层知道它。
//
// 原有的原币种字段一个都不动 —— 换算只是**并列多给一个口径**，
// 前端在不该重复显示的时候自己判断（见 app.js 的 moneyBothText）。
func (s *Server) applyFX(dto *nodeDTO) {
	snap := s.fxCurrent()
	dto.PriceCNYCents = snap.ToCNY(dto.PriceCents, dto.Currency)
	dto.MonthlyCNYCents = snap.ToCNY(dto.MonthlyCents, dto.Currency)
	dto.RemainingValueCNYCents = snap.ToCNY(dto.RemainingValueCents, dto.Currency)
	// 只有"真的换算了"才为 true：币种为空/CNY、或没有可用汇率（未知币种、
	// 汇率缺失）时都是 false，界面据此只显示原币种一处。
	dto.CNYConverted = snap.Convertible(dto.Currency)
}

// fxSettings 是下发给前端的汇率元信息（GET /api/v1/settings 的 fx 字段）。
//
// 用户要能一眼看出"这个换算用的是哪天的、从哪来的、是不是兜底的"：
// 价格上多出来的那个 ¥ 数字如果不写清出处，没人敢拿它对账。
type fxSettings struct {
	// Enabled 表示是否开着"每天自动获取"（--fx）。
	Enabled bool `json:"enabled"`
	// IsDefault 表示当前生效的是内置兜底表（从没成功取到过汇率）。
	IsDefault bool `json:"is_default"`
	// Date 是汇率对应的日期（数据源给的）；兜底表为空串。
	Date string `json:"date"`
	// Source 是取到这一份的 URL；兜底表为空串。
	Source string `json:"source"`
	// FetchedAt 是我们取到它的时刻（epoch 秒，按服务端时区渲染由前端负责 ——
	// 前端只用 fmtAgo 显示"多久以前"，时间口径仍与服务端一致）。
	FetchedAt int64 `json:"fetched_at"`
	// Currencies 是这一份里可用的币种数量（让用户知道覆盖面，不必把整张表下发）。
	Currencies int `json:"currencies"`
}

func (s *Server) currentFXSettings() fxSettings {
	snap := s.fxCurrent()
	return fxSettings{
		Enabled:    s.cfg.FX,
		IsDefault:  snap.IsDefault,
		Date:       snap.Date,
		Source:     snap.Source,
		FetchedAt:  snap.FetchedAt,
		Currencies: len(snap.Rates),
	}
}
