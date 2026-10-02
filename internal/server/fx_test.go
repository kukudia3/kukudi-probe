package server

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/fx"
	"probe/internal/store"
)

// fxProvider 冒充汇率 API 的本地服务（**测试里不许连真实外网**）。
//
// 三种形态各对应一条降级路径：正常返回 / 返回垃圾 / 超时。
type fxProvider struct {
	*httptest.Server
	hits atomic.Int32
}

// newFXProvider 起一个返回固定 JSON 的假数据源。
func newFXProvider(t *testing.T, body string) *fxProvider {
	t.Helper()
	p := &fxProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(p.Server.Close)
	return p
}

// newFXGarbageProvider 起一个"200 但内容是垃圾"的假数据源。
func newFXGarbageProvider(t *testing.T) *fxProvider {
	t.Helper()
	return newFXProvider(t, `<html><body>502 Bad Gateway</body></html>`)
}

// newFXSilentProvider 起一个"永远不回答"的假数据源（配合 context 超时用）。
func newFXSilentProvider(t *testing.T) *fxProvider {
	t.Helper()
	p := &fxProvider{}
	done := make(chan struct{})
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		select {
		case <-done:
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(func() { close(done); p.Server.Close() })
	return p
}

// validFXBody 是一份"能用的"响应，汇率取整数便于算期望值。
const validFXBody = `{"base":"CNY","date":"2026-03-02","rates":{"USD":0.2,"JPY":20}}`

// ① 取成功 → 用新的 + 落库。
func TestFXRefreshStoresFetchedSnapshot(t *testing.T) {
	provider := newFXProvider(t, validFXBody)
	cfg := config.Default()
	cfg.FXRateURL = provider.URL
	h := newAuthHarnessWithConfig(t, cfg)
	ctx := context.Background()

	h.srv.loadStoredFX(ctx)
	if !h.srv.fxCurrent().IsDefault {
		t.Fatal("库里还没有值时应当先用内置兜底表")
	}
	h.srv.refreshFX(ctx)

	snap := h.srv.fxCurrent()
	if snap.IsDefault {
		t.Error("取成功之后不该还是兜底表")
	}
	if snap.Source != provider.URL {
		t.Errorf("来源 = %q，期望 %q", snap.Source, provider.URL)
	}
	if snap.Date != "2026-03-02" {
		t.Errorf("日期 = %q，期望数据源给的 2026-03-02", snap.Date)
	}
	if rate, ok := snap.Rate("USD"); !ok || rate != 0.2 {
		t.Errorf("USD 汇率 = %v/%v，期望 0.2", rate, ok)
	}

	// 落库：换一个"重启后"的 Server 读同一份数据库，必须读到这一份。
	raw, ok, err := h.srv.db.GetSetting(ctx, store.KeyFXRates)
	if err != nil || !ok {
		t.Fatalf("汇率没有落库: ok=%v err=%v", ok, err)
	}
	restored, ok := fx.Decode(raw)
	if !ok {
		t.Fatalf("落库的内容解不回来: %s", raw)
	}
	if restored.Source != provider.URL || restored.Date != "2026-03-02" {
		t.Errorf("落库的内容不对: %+v", restored)
	}
	// 关键：IsDefault 不落库 —— 读回来的一定是"取到过的那一份"。
	if restored.IsDefault {
		t.Error("落库的快照不该带 IsDefault 标记")
	}
}

// ② 取失败但有旧值 → 继续用旧的（哪怕过期），并且**不刷屏**（每个源一条，汇总成一行）。
func TestFXRefreshKeepsPreviousValueWhenFetchFails(t *testing.T) {
	good := newFXProvider(t, validFXBody)
	cfg := config.Default()
	cfg.FXRateURL = good.URL
	h := newAuthHarnessWithConfig(t, cfg)
	ctx := context.Background()

	h.srv.loadStoredFX(ctx)
	h.srv.refreshFX(ctx)
	before := h.srv.fxCurrent()
	if before.IsDefault {
		t.Fatal("第一次取汇率就没有成功")
	}

	// 数据源坏掉（垃圾 JSON / 连不上）。
	garbage := newFXGarbageProvider(t)
	h.srv.cfg.FXRateURL = garbage.URL
	h.srv.refreshFX(ctx)
	after := h.srv.fxCurrent()

	if after.Source != before.Source || after.Date != before.Date || after.FetchedAt != before.FetchedAt {
		t.Errorf("取失败时应当继续用旧值，实际换成了 %+v（旧值 %+v）", after, before)
	}
	if rate, ok := after.Rate("USD"); !ok || rate != 0.2 {
		t.Errorf("旧汇率表应当原样还在，USD = %v/%v", rate, ok)
	}

	// 换一个"重启后"的 Server（内存是空的）也必须是旧值而不是兜底表：
	// 这正是"取失败但库里有旧值"最常见的样子。
	fresh := New(cfg, h.srv.db, nil, time.UTC)
	fresh.loadStoredFX(ctx)
	if got := fresh.fxCurrent(); got.Source != before.Source || got.IsDefault {
		t.Errorf("重启后应当读回上次落库的那一份，实际 %+v", got)
	}
}

// ③ 从没取到过 → 用内置默认汇率表（写死在代码里），价格照常有人民币口径。
func TestFXRefreshFallsBackToBuiltInTable(t *testing.T) {
	garbage := newFXGarbageProvider(t)
	cfg := config.Default()
	cfg.FXRateURL = garbage.URL
	h := newAuthHarnessWithConfig(t, cfg)
	ctx := context.Background()

	h.srv.loadStoredFX(ctx) // 库里什么都没有
	h.srv.refreshFX(ctx)

	snap := h.srv.fxCurrent()
	if !snap.IsDefault {
		t.Errorf("从没取到过时应当是内置兜底表，实际 %+v", snap)
	}
	if len(snap.Rates) == 0 {
		t.Fatal("兜底表是空的：价格的人民币口径会变成 0")
	}
	if got := snap.ToCNY(10000, "USD"); got <= 0 {
		t.Errorf("用兜底表换算 10000 分 USD = %d，期望正数", got)
	}
	if garbage.hits.Load() == 0 {
		t.Error("假数据源一次都没被请求：这条用例没有真的走到「取失败」那一级")
	}
}

// 取汇率失败**不影响**其它功能：节点列表、详情、设置照常可用，
// 而且金额绝不会因为取不到汇率变成 0。
func TestFXFailureDoesNotBreakOtherFeatures(t *testing.T) {
	garbage := newFXGarbageProvider(t)
	cfg := config.Default()
	cfg.FXRateURL = garbage.URL
	h := newAuthHarnessWithConfig(t, cfg)

	h.srv.refreshFX(context.Background())

	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "usd-01", "interval_sec": 1,
		"price_cents": 10000, "currency": "USD", "billing_months": 1,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}

	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("汇率取不到时节点列表应当照常可用，实际 %d", status)
	}
	node := firstNodeFromList(t, list)
	if got := node["price_cents"]; got != float64(10000) {
		t.Errorf("原币种金额被改动了: %v（原币种字段一个都不许动）", got)
	}
	cny, _ := node["price_cny_cents"].(float64)
	if cny <= 0 {
		t.Errorf("price_cny_cents = %v，取不到汇率时也必须是正数（兜底表）", node["price_cny_cents"])
	}
	if node["cny_converted"] != true {
		t.Errorf("USD 在兜底表里有汇率，cny_converted 应当为 true，实际 %v", node["cny_converted"])
	}
	// 换算口径：人民币 = 原币 ÷ rate（rate 来自兜底表）。
	rate, ok := fx.Default().Rate("USD")
	if !ok {
		t.Fatal("兜底表里没有 USD")
	}
	want := int64(math.Round(float64(10000) / rate))
	if int64(cny) != want {
		t.Errorf("price_cny_cents = %d，期望 %d（10000 ÷ %v）", int64(cny), want, rate)
	}

	// 设置接口也要照常返回，并且明确告诉用户"这是兜底值"。
	status, all := h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("设置接口应当照常可用，实际 %d", status)
	}
	fxMeta, ok := all["fx"].(map[string]any)
	if !ok {
		t.Fatalf("设置里没有 fx 元信息: %v", all["fx"])
	}
	if fxMeta["is_default"] != true {
		t.Errorf("取不到汇率时 is_default 应当为 true，实际 %v", fxMeta["is_default"])
	}
	if fxMeta["enabled"] != true {
		t.Errorf("默认配置下 enabled 应当为 true，实际 %v", fxMeta["enabled"])
	}
}

// 对端一直不吭声时，刷新必须在**预算内**返回：后台流水线里的其它定时任务
// （落盘、rollup、清理）不能被一次出网拖住。
func TestFXRefreshRespectsBudget(t *testing.T) {
	silent := newFXSilentProvider(t)
	good := newFXProvider(t, validFXBody)

	cfg := config.Default()
	cfg.FXRateURL = good.URL
	h := newAuthHarnessWithConfig(t, cfg)
	h.srv.refreshFX(context.Background())
	before := h.srv.fxCurrent()

	// 用一个 300ms 就取消的 context 模拟"预算用尽"，避免测试真的等 5 秒。
	h.srv.cfg.FXRateURL = silent.URL
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	h.srv.refreshFX(ctx)
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("刷新花了 %s，超时没有生效（会拖住后台流水线）", elapsed)
	}
	if got := h.srv.fxCurrent(); got.FetchedAt != before.FetchedAt {
		t.Errorf("超时之后应当继续用旧值，实际 %+v", got)
	}
}

// 关掉功能（--fx=false）：不发任何网络请求，一直用上次取到的值；
// 从没取到过就用内置兜底表 —— 价格照常显示。
func TestFXDisabledNeverFetchesAndKeepsOldOrDefault(t *testing.T) {
	counting := newFXProvider(t, validFXBody)
	ctx := context.Background()

	// 先造出"上次取到过的那一份"（模拟开着的时候取到的）。
	onCfg := config.Default()
	onCfg.FXRateURL = counting.URL
	on := newAuthHarnessWithConfig(t, onCfg)
	on.srv.loadStoredFX(ctx)
	on.srv.refreshFX(ctx)
	stored := on.srv.fxCurrent()
	hitsAfterFetch := counting.hits.Load()
	if hitsAfterFetch == 0 {
		t.Fatal("开着的状态下应当请求过数据源")
	}

	// 关掉：同一个数据库，新的进程。
	offCfg := config.Default()
	offCfg.FX = false
	offCfg.FXRateURL = counting.URL
	off := New(offCfg, on.srv.db, nil, time.UTC)

	off.loadStoredFX(ctx)
	off.refreshFX(ctx)
	if got := counting.hits.Load(); got != hitsAfterFetch {
		t.Errorf("关掉之后不该再发请求，请求数从 %d 变成了 %d", hitsAfterFetch, got)
	}
	got := off.fxCurrent()
	if got.Source != stored.Source || got.Date != stored.Date {
		t.Errorf("关掉之后应当继续用上次取到的那一份，实际 %+v（期望 %+v）", got, stored)
	}
	if s := off.currentFXSettings(); s.Enabled {
		t.Error("关掉时下发的元信息里 enabled 必须为 false，否则界面会显示「每天自动获取」")
	}

	// 从没取到过 + 关闭 → 兜底表，金额不为 0。
	empty := New(offCfg, newEmptyDB(t), nil, time.UTC)
	empty.loadStoredFX(ctx)
	empty.refreshFX(ctx)
	snap := empty.fxCurrent()
	if !snap.IsDefault {
		t.Errorf("没有任何已存值时应当是兜底表，实际 %+v", snap)
	}
	if snap.ToCNY(10000, "USD") <= 0 {
		t.Error("关闭 + 兜底表时换算不能是 0")
	}
}

// 库里那行是脏数据（手工改坏、老版本写的别的结构）时：读路径不许报错、
// 不许崩，继续用兜底表。
func TestLoadStoredFXIgnoresGarbage(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	if err := h.srv.db.SetSetting(ctx, store.KeyFXRates, "这不是 JSON"); err != nil {
		t.Fatalf("写入脏数据: %v", err)
	}
	if ok := h.srv.loadStoredFX(ctx); ok {
		t.Error("脏数据不该被当成有效的汇率")
	}
	if snap := h.srv.fxCurrent(); !snap.IsDefault {
		t.Errorf("脏数据时应当回落到兜底表，实际 %+v", snap)
	}
}

// 人民币口径的字段是**并列新增**的：原有的原币种字段一个都不许动。
//
// 三处金额（价格 / 月均 / 剩余价值）都要有对应的人民币值，且币种是 CNY 时
// cny_converted 必须为 false（否则界面会把同一个金额写两遍）。
func TestDTOCNYFieldsForCNYAndForeignCurrency(t *testing.T) {
	h := newAuthHarness(t)

	rate := 0.2
	h.srv.fx.Store(&fx.Snapshot{
		Base: fx.BaseCurrency, Date: "2026-03-02", Source: "test", FetchedAt: time.Now().Unix(),
		Rates: map[string]float64{"USD": rate},
	})

	// USD 节点：12 个月 1200.00 美元。
	status, created := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "usd-01", "interval_sec": 1,
		"price_cents": 120000, "currency": "USD", "billing_months": 12,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建 USD 节点失败: %d %v", status, created)
	}
	usd, _ := created["node"].(map[string]any)
	wantPrice := int64(math.Round(120000 / rate))
	wantMonthly := int64(math.Round(float64(120000/12) / rate))
	if got := int64(usd["price_cny_cents"].(float64)); got != wantPrice {
		t.Errorf("USD 节点 price_cny_cents = %d，期望 %d", got, wantPrice)
	}
	if got := int64(usd["monthly_cny_cents"].(float64)); got != wantMonthly {
		t.Errorf("USD 节点 monthly_cny_cents = %d，期望 %d", got, wantMonthly)
	}
	if usd["cny_converted"] != true {
		t.Errorf("USD 节点 cny_converted = %v，期望 true", usd["cny_converted"])
	}
	// 原字段不动。
	if usd["price_cents"] != float64(120000) || usd["monthly_cents"] != float64(10000) {
		t.Errorf("原币种字段被改动了: price=%v monthly=%v", usd["price_cents"], usd["monthly_cents"])
	}

	// CNY 节点：人民币口径**等于**原值，且 cny_converted=false（界面不重复显示）。
	status, created = h.post(t, "/api/v1/nodes", map[string]any{
		"name": "cny-01", "interval_sec": 1,
		"price_cents": 3100, "currency": "CNY", "billing_months": 1,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建 CNY 节点失败: %d %v", status, created)
	}
	cny, _ := created["node"].(map[string]any)
	if cny["price_cny_cents"] != cny["price_cents"] {
		t.Errorf("CNY 节点的人民币口径应当等于原值: %v vs %v", cny["price_cny_cents"], cny["price_cents"])
	}
	if cny["cny_converted"] != false {
		t.Errorf("CNY 节点 cny_converted = %v，期望 false（否则界面会显示两遍）", cny["cny_converted"])
	}

	// 未知币种：退回原值、标记为"没换算"，绝不出现 0。
	status, created = h.post(t, "/api/v1/nodes", map[string]any{
		"name": "old-01", "interval_sec": 1,
		"price_cents": 12345, "currency": "XYZ", "billing_months": 1,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建老币种节点失败: %d %v", status, created)
	}
	old, _ := created["node"].(map[string]any)
	if old["price_cny_cents"] != float64(12345) {
		t.Errorf("未知币种应当退回原值 12345，实际 %v", old["price_cny_cents"])
	}
	if old["cny_converted"] != false {
		t.Errorf("未知币种 cny_converted = %v，期望 false", old["cny_converted"])
	}
	if old["currency"] != "XYZ" {
		t.Errorf("原始币种被改写了: %v", old["currency"])
	}
}

// 取汇率的**时机**：挂在既有的后台流水线上 —— 启动时取一次，之后每天一次。
//
// 这条用例真的把 pipelineLoop 跑起来（而不是直接调 refreshFX），因为"挂在哪个
// goroutine 上、启动时到底取不取"正是要验的东西：漏掉启动那一次的表现是
// "重启之后要等到明天才有人民币口径"，而页面上完全看不出原因。
func TestPipelineFetchesFXAtStartupAndThenPeriodically(t *testing.T) {
	provider := newFXProvider(t, validFXBody)
	cfg := config.Default()
	cfg.FXRateURL = provider.URL
	// 守卫先建（在 t.TempDir() 之前）：它的核账跑在清理链的最后。
	guard := newBackgroundGuard(t)
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	guard.watchDir(filepath.Dir(dbPath))
	db := newEmptyDBAt(t, dbPath)
	s := New(cfg, db, nil, time.UTC)

	// 把"每天"调成 60ms：验的是"周期到了会再取一次"这个机制，
	// 而不是那个 24 小时的数字本身。
	original := fxEvery
	fxEvery = 60 * time.Millisecond
	t.Cleanup(func() { fxEvery = original })

	// 这个循环带着 60ms 的汇率 ticker，而每一跳都会写一次 settings ——
	// "用例返回后它还在写库"的概率一点都不低。所以必须在返回前等它真的退出：
	// 只 cancel 是发信号就走，SQLite 会在 t.TempDir() 的 RemoveAll 空隙里重建
	// -wal/-shm，Linux 上于是报 "directory not empty"（见 bgloop_test.go）。
	loop := guard.start("pipelineLoop", s.pipelineLoop)
	defer loop.stop()

	// ① 启动时取一次（流水线一起来就该有值，不必等任何人来访问页面）。
	waitFor(t, 5*time.Second, "pipelineLoop 启动时取一次汇率", func() bool {
		return s.fxCurrent().Source == provider.URL
	})
	if _, ok, err := db.GetSetting(context.Background(), store.KeyFXRates); err != nil || !ok {
		t.Errorf("启动时取到的汇率没有落库: ok=%v err=%v", ok, err)
	}

	// ② 周期到了会再取一次（ticker 真的挂上了，不是只在启动时调了一次）。
	before := provider.hits.Load()
	waitFor(t, 5*time.Second, "汇率周期到了再取一次", func() bool {
		return provider.hits.Load() > before
	})
}

// newEmptyDB 开一个干净的空库（测试里只用于"从没取到过汇率"的场景）。
func newEmptyDB(t *testing.T) *store.DB {
	t.Helper()
	return newEmptyDBAt(t, filepath.Join(t.TempDir(), "probe.db"))
}

// newEmptyDBAt 与上面一样，只是库路径由调用方给：
// 守卫要盯住那个数据目录时，得先把路径拿在手里。
func newEmptyDBAt(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
