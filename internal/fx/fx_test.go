package fx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 换算的三个边界：币种是 CNY、币种不认识、汇率缺失/为 0。
//
// 这里钉的是**意图**，不是某一个数字：不管汇率表长什么样，
// "人民币原值返回、没有可用汇率一律退回原值、绝不返回 0、绝不出现 NaN"
// 这三条都必须成立。
func TestToCNYBranches(t *testing.T) {
	snap := Snapshot{
		Base: BaseCurrency,
		Rates: map[string]float64{
			"USD":  0.2, // 1 CNY = 0.2 USD ⇒ 5 元人民币 = 1 美元
			"JPY":  20,  // 1 CNY = 20 JPY
			"ZERO": 0,   // 垃圾数据：0 不算可用汇率
		},
	}

	cases := []struct {
		name     string
		cents    int64
		currency string
		want     int64
		why      string
	}{
		{"外币按除法换算", 10000, "USD", 50000, "rate 是「1 CNY = ? 外币」，所以人民币 = 外币 ÷ rate"},
		{"换算结果四舍五入到分", 100, "JPY", 5, "100 日元 ÷ 20 = 5 分，不能截断成 4"},
		{"小写币种也认", 10000, "usd", 50000, "库里的值可能是小写，归一化后再查表"},
		{"币种是 CNY", 12345, "CNY", 12345, "人民币口径就是原值，不是 0"},
		{"币种为空", 12345, "", 12345, "没填币种按人民币处理（与 CNY 同一条路）"},
		{"币种不认识", 12345, "XYZ", 12345, "不认识就当人民币（1:1）——绝不能变成 0，也不猜一个汇率"},
		{"汇率缺失", 12345, "EUR", 12345, "这一份表里没有 EUR ⇒ 退回原值（不是 0）"},
		{"汇率为 0", 12345, "ZERO", 12345, "0 是垃圾数据，当缺失处理"},
		{"金额为 0", 0, "USD", 0, "没填价格就是 0（前端据此整行不显示）"},
	}
	for _, tc := range cases {
		got := snap.ToCNY(tc.cents, tc.currency)
		if got != tc.want {
			t.Errorf("%s: ToCNY(%d, %q) = %d，期望 %d（%s）", tc.name, tc.cents, tc.currency, got, tc.want, tc.why)
		}
	}
}

// TryToCNY 是"求和"用的那一版：换算不了时必须**明确说不**，而不是退回原值。
//
// 两者的差别就这一条：ToCNY 退回原值（单台节点显示时要的是这个，界面照原币种
// 渲染），TryToCNY 返回 ok=false（把没换算过的数字按 1:1 加进一个人民币合计，
// 等于凭空编一个汇率，而那个数会被用户当成真实资产）。
func TestTryToCNYNeverSubstitutesOriginalValue(t *testing.T) {
	snap := Snapshot{
		Base:  BaseCurrency,
		Rates: map[string]float64{"USD": 0.2, "JPY": 20, "ZERO": 0},
	}

	// 能换算的（含"本来就是人民币"的两种）：ok=true，且与 ToCNY 给出同一个数
	// —— 同一个输入在两个函数里算出不同的金额，就是 bug。
	for _, tc := range []struct {
		cents    int64
		currency string
		want     int64
	}{
		{10000, "USD", 50000},
		{100, "JPY", 5},
		{10000, "usd", 50000},
		{12345, "CNY", 12345},
		{12345, "", 12345},
		{0, "CNY", 0},
		{0, "", 0},
	} {
		got, ok := snap.TryToCNY(tc.cents, tc.currency)
		if !ok {
			t.Errorf("TryToCNY(%d, %q) 说换不了，但它是人民币口径（或这一份表里有它的汇率）", tc.cents, tc.currency)
			continue
		}
		if got != tc.want {
			t.Errorf("TryToCNY(%d, %q) = %d，期望 %d", tc.cents, tc.currency, got, tc.want)
		}
		if toCNY := snap.ToCNY(tc.cents, tc.currency); toCNY != got {
			t.Errorf("ToCNY(%d, %q) = %d，TryToCNY 给的是 %d：两个函数必须给同一个数",
				tc.cents, tc.currency, toCNY, got)
		}
	}

	// 换算不了的：ok=false，而且**绝不能**把原值当成结果递出来 ——
	// 调用方一不留神（不看 ok）就会把它按 1:1 加进合计里。
	for _, code := range []string{"XYZ", "EUR", "ZERO"} {
		got, ok := snap.TryToCNY(12345, code)
		if ok {
			t.Errorf("TryToCNY(12345, %q) 说能换算：这一份表里根本没有它的可用汇率", code)
		}
		if got != 0 {
			t.Errorf("TryToCNY(12345, %q) 换不了却返回了 %d：调用方不看 ok 就会把它当金额用", code, got)
		}
		// 同一份输入下 ToCNY 仍然是"退回原值"：单台节点的显示口径不许被一起改掉。
		if toCNY := snap.ToCNY(12345, code); toCNY != 12345 {
			t.Errorf("ToCNY(12345, %q) = %d，期望退回原值 12345（单台显示要的是这个）", code, toCNY)
		}
	}
}

// 它一旦误报为 true，界面就会出现"¥45.00 · ¥45.00"这种把同一个数字写两遍的样子
// （币种是 CNY 时）或者"把一个没换算过的数字冒充成换算结果"（汇率缺失时）。
func TestConvertibleOnlyForRealConversions(t *testing.T) {
	snap := Snapshot{Rates: map[string]float64{"USD": 0.2, "ZERO": 0}}
	for _, code := range []string{"", "CNY", "cny", "XYZ", "EUR", "ZERO"} {
		if snap.Convertible(code) {
			t.Errorf("Convertible(%q) = true，但它并没有可用汇率（或本来就是人民币）", code)
		}
	}
	if !snap.Convertible("usd") {
		t.Error("USD 有可用汇率，Convertible 却为 false：界面上会少显示人民币口径")
	}
}

// 两个数据源都返回 {rates: {...}}，但日期字段名不同（date / time_last_update_unix）。
// 解析必须两种都认，否则第二个源永远用不上（取到了却被当成垃圾丢掉）。
func TestParseAcceptsBothProviderShapes(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	frankfurter := []byte(`{"amount":1.0,"base":"CNY","date":"2026-03-02","rates":{"USD":0.14799,"JPY":20.5}}`)
	got, err := Parse("https://api.frankfurter.app/latest?from=CNY", frankfurter, now)
	if err != nil {
		t.Fatalf("解析 frankfurter 形状失败: %v", err)
	}
	if got.Date != "2026-03-02" {
		t.Errorf("日期 = %q，期望用响应里的 date（数据源给的那一天）", got.Date)
	}
	if got.Source != "https://api.frankfurter.app/latest?from=CNY" {
		t.Errorf("来源 = %q，期望就是这次请求的 URL", got.Source)
	}
	if got.IsDefault {
		t.Error("取到的快照不该被标成兜底表")
	}
	if got.FetchedAt != now.Unix() {
		t.Errorf("FetchedAt = %d，期望 %d（取到的时刻）", got.FetchedAt, now.Unix())
	}
	if rate, ok := got.Rate("usd"); !ok || rate != 0.14799 {
		t.Errorf("USD 汇率 = %v/%v，期望 0.14799（键应当被归一化成大写）", rate, ok)
	}

	erapi := []byte(`{"result":"success","base_code":"CNY","time_last_update_unix":1772409600,"rates":{"USD":0.15,"EUR":0.13}}`)
	got2, err := Parse("https://open.er-api.com/v6/latest/CNY", erapi, now)
	if err != nil {
		t.Fatalf("解析 er-api 形状失败: %v", err)
	}
	// 这个源不给 date，只给 unix 时间戳：日期必须从它推出来，不能留空。
	wantDate := time.Unix(1772409600, 0).UTC().Format("2006-01-02")
	if got2.Date != wantDate {
		t.Errorf("日期 = %q，期望从 time_last_update_unix 推出来的 %q", got2.Date, wantDate)
	}
	if len(got2.Rates) != 2 {
		t.Errorf("汇率条数 = %d，期望 2", len(got2.Rates))
	}
}

// 解析必须**严**：宁可让这一级失败降级到上一份，也不能把方向不对/空表用上去。
func TestParseRejectsUnusableResponses(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		body string
	}{
		{"不是 JSON", `<html>503 Service Unavailable</html>`},
		{"空 JSON 对象", `{}`},
		{"rates 为空", `{"base":"CNY","rates":{}}`},
		{"全是不可用汇率", `{"base":"CNY","rates":{"USD":0,"EUR":-1}}`},
		{"基准币种不对", `{"base":"USD","rates":{"CNY":7.1}}`},
		{"er-api 的错误响应", `{"result":"error","error-type":"unsupported-code"}`},
		{"NaN 汇率", `{"base":"CNY","rates":{"USD":1e999}}`},
	}
	for _, tc := range cases {
		if _, err := Parse("https://example.invalid", []byte(tc.body), now); err == nil {
			t.Errorf("%s：应当解析失败（否则会把错误的汇率用上去）", tc.name)
		}
	}
}

// 内置兜底表是"从没取到过汇率"时的最后一道防线：它必须有值、覆盖常见币种、
// 标的 IsDefault 为 true（界面据此显示「内置兜底」），而且换算不会崩。
func TestDefaultSnapshotIsUsableFallback(t *testing.T) {
	snap := Default()
	if !snap.IsDefault {
		t.Error("Default() 必须标着 IsDefault，否则界面会把兜底表当成实时汇率")
	}
	if snap.Source != "" {
		t.Errorf("兜底表不该有来源 URL，实际 %q", snap.Source)
	}
	// 不钉具体数字（那些是量级估计，以后可能调整），只钉"它是一张能用的表"。
	for _, code := range []string{"USD", "EUR", "GBP", "JPY", "HKD", "SGD", "AUD", "CAD", "KRW", "TWD", "MYR", "THB", "CHF"} {
		if _, ok := snap.Rate(code); !ok {
			t.Errorf("兜底表里没有 %s：这个币种的价格会退回原值显示", code)
		}
	}
	if _, ok := snap.Rate("CNY"); !ok {
		t.Error("兜底表里应当有 CNY 自己（rate=1）")
	}
	// 兜底表也要能正常换算：不能出现 0 或负数。
	got := snap.ToCNY(10000, "USD")
	if got <= 0 {
		t.Errorf("用兜底表换算 10000 分 USD = %d，期望一个正数（绝不返回 0）", got)
	}
}

// 落库的往返，以及"库里是脏数据"时的读路径：解不出来就当没有，
// 让调用方继续用兜底表，绝不报错刷屏（与 store.decodeTags 同一条原则）。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	snap := Snapshot{Base: BaseCurrency, Date: "2026-03-02", Source: "https://example.com/x", FetchedAt: 1772409600}
	snap.Rates = map[string]float64{"USD": 0.14799}
	snap.IsDefault = true // 落库前不该带着这个标记：读回来必须重新判断

	raw, err := snap.Encode()
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	got, ok := Decode(raw)
	if !ok {
		t.Fatal("自己编码出来的内容应当能解回来")
	}
	if got.Date != snap.Date || got.Source != snap.Source || got.FetchedAt != snap.FetchedAt {
		t.Errorf("往返之后内容变了: %+v", got)
	}
	if got.IsDefault {
		t.Error("落库的那一份一定是取回来的，读回来不该是 IsDefault")
	}

	for _, bad := range []string{"", "   ", "不是 JSON", `{"rates":{}}`, `[]`} {
		if _, ok := Decode(bad); ok {
			t.Errorf("Decode(%q) 应当返回 ok=false（调用方继续用兜底表）", bad)
		}
	}
}

// 数据源按顺序试：第一个坏了要用第二个，两个都坏了才失败（且错误里要能看出是谁坏了）。
func TestFetchFallsThroughToSecondProvider(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>503</html>`))
	}))
	defer broken.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"base":"CNY","date":"2026-03-02","rates":{"USD":0.2}}`))
	}))
	defer good.Close()

	c := Client{URLs: []string{broken.URL, good.URL}}
	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("第一个源坏了应当继续试第二个，实际报错: %v", err)
	}
	if snap.Source != good.URL {
		t.Errorf("来源 = %q，期望第二个源 %q", snap.Source, good.URL)
	}

	// 两个都坏：返回的是一条汇总错误（调用方只记一条日志，不刷屏）。
	_, err = Client{URLs: []string{broken.URL, broken.URL}}.Fetch(context.Background())
	if err == nil {
		t.Fatal("两个源都坏时应当报错")
	}
	if !strings.Contains(err.Error(), broken.URL) {
		t.Errorf("错误里应当写清是哪个源失败了，实际: %v", err)
	}
}

// 单个源的超时必须自己生效（默认 5 秒；测试里把客户端超时调小来验同一段代码）。
// 卡住的那个源不能让 Fetch 一直挂着 —— 后台流水线会被它拖住。
func TestFetchTimesOutOnSilentProvider(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer slow.Close()

	start := time.Now()
	_, err := Client{URLs: []string{slow.URL}, HTTP: &http.Client{Timeout: 80 * time.Millisecond}}.Fetch(context.Background())
	if err == nil {
		t.Fatal("对端不吭声时应当超时报错")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("等待了 %s，超时没有生效", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "Client.Timeout") {
		t.Logf("超时错误的形态: %v（只用于观察，不强制形状）", err)
	}
}
