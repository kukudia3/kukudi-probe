package e2e

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/fx"
)

// 汇率换算在**真浏览器**里的验收。
//
// 为什么必须真跑一遍：这一轮改的是"价格怎么显示"，而它坏掉的方式全是静默的 ——
// 卡片照渲染、数字照样有，只是外币没折成人民币、或者两处口径不一致、
// 或者币种是 CNY 时把同一个金额写了两遍。这些在静态断言里都看不出来。
//
// 三个场景（各跑一次 Chrome）：
//  1. 注入一份已知汇率（1 CNY = 0.2 USD）的**假数据源**（本地 httptest，绝不连外网）：
//     USD 节点在首页卡片/详情页/服务器列表三处都显示 "$X USD · ¥Y"，
//     且 Y 由 Go 侧按同一个汇率独立算出来比对；CNY 节点不重复显示；
//     老币种（XYZ）能显示、保存不被改成 CNY；编辑框里货币是**下拉**；
//     设置页能看到汇率的元信息（哪天的、来源、是否兜底）。
//  2. 数据源返回垃圾（取不到汇率）：页面照常显示、不报错，用的是内置兜底表。
//  3. 回归：四条常用路由各走一遍，每条都要 errs=0。
func TestFXConversionInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	// 假汇率源：1 CNY = 0.2 USD（除法口径：10000 分美元 = 50000 分人民币）。
	provider := newFakeFXProvider(t, `{"base":"CNY","date":"2026-03-02","rates":{"USD":0.2,"EUR":0.25,"JPY":20}}`)

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
		func(cfg *config.Server) { cfg.FXRateURL = provider.URL })
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	waitFXReady(t, br, provider.URL)

	// 三台机器：USD（有汇率）、CNY（不该重复显示）、XYZ（老数据里的怪币种）。
	// 到期日都设成"45 天零 6 小时之后"：剩余天数在接下来 6 小时里恒为 45，
	// 页面渲染与 Go 侧算期望值时不会因为跨过一天的边界而对不上。
	expires := time.Now().Add(45*24*time.Hour + 6*time.Hour).Unix()
	usdID := createPricedNode(t, br, "usd-01", 10000, "USD", 1, expires)
	cnyID := createPricedNode(t, br, "cny-01", 3100, "CNY", 1, expires)
	legacyID := createPricedNode(t, br, "old-01", 12345, "XYZ", 1, expires)

	names := map[string]int64{"usd-01": usdID, "cny-01": cnyID, "old-01": legacyID}

	// 期望值在 Go 侧独立算：拿服务端给的**原始金额**，按注入的汇率（0.2）除一遍，
	// 再按前端的货币格式拼出来。页面上的字必须与它一模一样。
	//
	// 快照要在**跑浏览器之前**取：脚本后面会把 usd-01 的币种改成 EUR（那正是
	// 要验的操作），改完之后再读 API 拿到的就不是"页面当时渲染的那份数据"了。
	nodes := fetchNodes(t, br)
	usd := nodeByID(t, nodes, usdID)
	rate := 0.2
	wantUSDPrice := moneyText(intField(t, usd, "price_cents"), "USD") + " · " +
		cnyText(convert(intField(t, usd, "price_cents"), rate)) + " / 月"
	wantUSDMonthly := moneyText(intField(t, usd, "monthly_cents"), "USD") + " · " +
		cnyText(convert(intField(t, usd, "monthly_cents"), rate)) + " / 月"
	wantUSDValue := moneyText(intField(t, usd, "remaining_value_cents"), "USD") + " · " +
		cnyText(convert(intField(t, usd, "remaining_value_cents"), rate))

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		Scenario: "fx", User: "admin", Pass: "a-very-good-password",
		Nodes: names, Focus: "usd-01",
	}, fxHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, "1500,1100")

	var res fxResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 800))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("自检脚本走过的步骤 = %v", res.Steps)

	// 1) 首页卡片：费用行同时给出原币种与人民币口径。
	if got := res.Cards["usd-01"].Cost; !strings.Contains(got, wantUSDPrice) {
		t.Errorf("首页卡片的费用行 = %q，期望包含 %q（原币种 + 人民币口径）", got, wantUSDPrice)
	} else {
		t.Logf("首页卡片 usd-01 费用行 = %q", got)
	}

	// 2) 详情页：三格都带人民币口径。
	detail := res.Detail["usd-01"]
	if detail.Price != wantUSDPrice {
		t.Errorf("详情页价格 = %q，期望 %q", detail.Price, wantUSDPrice)
	}
	if detail.Monthly != wantUSDMonthly {
		t.Errorf("详情页月均 = %q，期望 %q", detail.Monthly, wantUSDMonthly)
	}
	if detail.Value != wantUSDValue {
		t.Errorf("详情页剩余价值 = %q，期望 %q", detail.Value, wantUSDValue)
	}
	t.Logf("详情页 usd-01：价格 %q / 月均 %q / 剩余价值 %q", detail.Price, detail.Monthly, detail.Value)

	// 3) 服务器列表：剩余价值同一口径。
	wantList := "剩余价值 " + wantUSDValue
	if got := res.List["usd-01"]; !strings.Contains(got, wantList) {
		t.Errorf("服务器列表 usd-01 的信息行 = %q，期望包含 %q", got, wantList)
	} else {
		t.Logf("服务器列表 usd-01 = %q", got)
	}

	// 4) CNY 节点：**不重复显示**两遍（只有一个 ¥，也没有第二段金额）。
	//
	// 注意 CNY 节点的原币种写法本来就是「¥31.00 CNY」（fmtMoney 的既有格式，
	// 这一轮没动它）；不许出现的是"再接一段人民币口径"变成两个金额。
	cnyDetail := res.Detail["cny-01"]
	if strings.Count(cnyDetail.Price, "¥") != 1 {
		t.Errorf("CNY 节点详情页价格 = %q，期望只有一个 ¥（不重复显示人民币口径）", cnyDetail.Price)
	}
	if strings.Contains(cnyDetail.Price, "·") {
		t.Errorf("CNY 节点详情页价格 = %q，出现了第二段金额（人民币口径不该再接一遍）", cnyDetail.Price)
	}
	if !strings.Contains(cnyDetail.Price, "31.00") {
		t.Errorf("CNY 节点详情页价格 = %q，期望能看到 31.00", cnyDetail.Price)
	}
	if got := res.Cards["cny-01"].Cost; strings.Count(got, "¥") != 1 {
		t.Errorf("CNY 节点首页卡片 = %q，期望只有一个 ¥", got)
	}
	if got := res.List["cny-01"]; strings.Count(got, "¥") != 1 {
		t.Errorf("CNY 节点服务器列表 = %q，期望只有一个 ¥", got)
	}
	t.Logf("CNY 节点：卡片 %q / 详情 %q / 列表 %q",
		res.Cards["cny-01"].Cost, cnyDetail.Price, res.List["cny-01"])

	// 5) 老币种（XYZ）：照常显示原币种，绝不冒充成换算过的人民币。
	if got := res.Detail["old-01"].Price; strings.Contains(got, "¥") {
		t.Errorf("老币种节点详情页价格 = %q：没有可用汇率时不该出现人民币金额", got)
	}
	if got := res.Cards["old-01"].Cost; !strings.Contains(got, "123.45 XYZ") {
		t.Errorf("老币种节点首页卡片 = %q，期望按原币种显示 123.45 XYZ", got)
	}

	// 6) 编辑框里的货币是**下拉**，并且老币种被临时补成了选项。
	if res.Currency.Tag != "SELECT" {
		t.Errorf("货币控件的标签 = %q，期望 SELECT（下拉）", res.Currency.Tag)
	}
	if len(res.Currency.Options) < 10 {
		t.Errorf("货币下拉只有 %d 个选项：%v", len(res.Currency.Options), res.Currency.Options)
	}
	if !containsString(res.Currency.Options, "") {
		t.Error("货币下拉里应当有「不填」这个空选项（币种是可选的）")
	}
	t.Logf("货币下拉：%d 个选项（%s …）", len(res.Currency.Options), strings.Join(res.Currency.Options[:3], ", "))
	t.Logf("usd-01 编辑框：控件 %s，选中 %q", res.Currency.Tag, res.Currency.Value)

	// 7) 老币种：打开编辑框能看到它，保存**不会**变成 CNY。
	legacy := res.Legacy
	if legacy.Value != "XYZ" {
		t.Errorf("老币种节点打开编辑框时下拉的值 = %q，期望 XYZ（不许静默改成 CNY）", legacy.Value)
	}
	if !strings.Contains(legacy.OptionText, "XYZ") {
		t.Errorf("老币种没有出现在下拉选项里（选项文本 = %q）：用户看不到库里的原值", legacy.OptionText)
	}
	if legacy.SavedCurrency != "XYZ" {
		t.Errorf("老币种保存时发出去的 currency = %q，期望 XYZ（打开看一眼再保存不该改数据）", legacy.SavedCurrency)
	}
	t.Logf("老币种：下拉值 %q 选项文本 %q 保存请求体 currency=%q",
		legacy.Value, legacy.OptionText, legacy.SavedCurrency)

	// 8) 换成另一个外币保存：请求体里的 currency 正确，重新打开还是它。
	if res.Edit.SavedCurrency != "EUR" {
		t.Errorf("选 EUR 保存时请求体 currency = %q，期望 EUR", res.Edit.SavedCurrency)
	}
	if res.Edit.ReopenedValue != "EUR" {
		t.Errorf("重新打开编辑框时下拉 = %q，期望 EUR（保存没生效或回填错了）", res.Edit.ReopenedValue)
	}
	t.Logf("换成 EUR 保存：请求体 currency=%q；重新打开下拉=%q", res.Edit.SavedCurrency, res.Edit.ReopenedValue)

	// 9) 设置页「服务器信息」里能看到汇率的元信息。
	for _, needle := range []string{"汇率数据", "汇率日期", "汇率来源", "2026-03-02", provider.URL} {
		if !strings.Contains(res.FXInfo, needle) {
			t.Errorf("设置页的服务器信息里缺少 %q：用户看不出换算用的是哪天的、从哪来的", needle)
		}
	}
	if strings.Contains(res.FXInfo, "内置兜底") {
		t.Errorf("取到实时汇率时不该显示「内置兜底」：%s", res.FXInfo)
	}
	t.Logf("设置页汇率元信息 = %q", oneLine(res.FXInfo))

	// 10) 回归：四条路由各走一遍，每条都要 errs=0。
	assertRoutes(t, res.Routes, map[string]string{
		"#/":                                  "view-home",
		"#/n/" + strconv.FormatInt(usdID, 10): "view-detail",
		"#/settings/nodes":                    "view-settings:nodes",
		"#/settings/alert":                    "view-settings:alert",
	})

	// 11) 窄屏（380px）再跑一遍：费用那一行多了人民币口径之后不许把页面撑破
	//     （整页横向滚动是这一轮最容易引入的回归）。
	narrow := runFXNarrow(t, chrome, "http://"+h.addr, names)
	if narrow.Overflow > 0 {
		t.Errorf("窄屏（视口 %dpx）下页面横向溢出了 %dpx：加长的费用行撑破了卡片",
			narrow.Viewport, narrow.Overflow)
	} else {
		t.Logf("窄屏实测：CSS 视口 %dpx（--window-size 传的是 380），页面横向溢出 %dpx",
			narrow.Viewport, narrow.Overflow)
	}
	for name, info := range narrow.Cards {
		t.Logf("窄屏下 %s 的费用行 = %q（文字被截 %dpx，卡片溢出 %dpx）",
			name, info.Cost, info.Clip, info.CardOverflow)
	}
}

// runFXNarrow 再跑一次 Chrome（380×900），只看窄屏下的布局。
func runFXNarrow(t *testing.T, chrome, base string, names map[string]int64) fxNarrow {
	t.Helper()
	proxy := newHarnessProxy(t, base, tzHarnessConfig{
		Scenario: "fxnarrow", User: "admin", Pass: "a-very-good-password",
		Nodes: names, Focus: "usd-01",
	}, fxHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 120*time.Second, "380,900")

	var res fxResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析窄屏场景的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("窄屏场景没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("窄屏场景里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	return res.Narrow
}

// 取不到汇率（数据源返回垃圾）时：页面**照常显示**、不报错，用的是内置兜底表。
//
// 这条是三级降级里第 ③ 级的端到端验收：从没取到过汇率的新库 + 一个坏数据源。
func TestFXDefaultTableWhenProviderBrokenInBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	// 200 但内容是垃圾：与"限流页面 / 返回错误结构"是同一种失败。
	broken := newFakeFXProvider(t, `<html>502 Bad Gateway</html>`)

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
		func(cfg *config.Server) { cfg.FXRateURL = broken.URL })
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	waitFXDefault(t, br)
	usdID := createPricedNode(t, br, "usd-01", 10000, "USD", 1, 0)

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		Scenario: "fxdefault", User: "admin", Pass: "a-very-good-password",
		Nodes: map[string]int64{"usd-01": usdID},
	}, fxHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 120*time.Second, "1500,1100")

	var res fxResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 800))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("取不到汇率时页面报了 %d 条 JS 错误：%v", len(res.Errs), res.Errs)
	}

	// 期望值用内置兜底表在 Go 侧算（与页面无关）。
	rate, ok := fx.Default().Rate("USD")
	if !ok {
		t.Fatal("内置兜底表里没有 USD：价格会给不出人民币口径")
	}
	want := moneyText(10000, "USD") + " · " + cnyText(convert(10000, rate))
	if got := res.Cards["usd-01"].Cost; !strings.Contains(got, want) {
		t.Errorf("取不到汇率时首页卡片 = %q，期望包含 %q（兜底表也要能换算，不能是 0/空白）", got, want)
	} else {
		t.Logf("取不到汇率时首页卡片 = %q（兜底表换算）", got)
	}
	if got := res.Detail["usd-01"].Price; !strings.Contains(got, "¥") {
		t.Errorf("取不到汇率时详情页价格 = %q，期望仍有人问币口径", got)
	}
	for _, needle := range []string{"汇率数据", "内置兜底"} {
		if !strings.Contains(res.FXInfo, needle) {
			t.Errorf("设置页的服务器信息里缺少 %q：用户看不出这是兜底值", needle)
		}
	}
	t.Logf("取不到汇率时设置页 = %q", oneLine(res.FXInfo))
}

// ---------------------------------------------------------------- 自检脚本的观测值

type fxResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`

	// Cards / Detail / List 是同一台机器在三个地方的渲染结果（键是节点名字）。
	Cards  map[string]fxCard   `json:"cards"`
	Detail map[string]fxDetail `json:"detail"`
	List   map[string]string   `json:"list"`

	Currency fxSelect     `json:"currency"`
	Edit     fxEdit       `json:"edit"`
	Legacy   fxLegacy     `json:"legacy"`
	FXInfo   string       `json:"fxInfo"`
	Narrow   fxNarrow     `json:"narrow"`
	Routes   []groupRoute `json:"routes"`
}

type fxCard struct {
	Cost   string `json:"cost"`
	Hidden bool   `json:"hidden"`
}

type fxDetail struct {
	Price   string `json:"price"`
	Monthly string `json:"monthly"`
	Value   string `json:"value"`
}

type fxSelect struct {
	Tag     string   `json:"tag"`
	Options []string `json:"options"`
	Value   string   `json:"value"`
}

type fxEdit struct {
	// SavedCurrency 是"选 EUR 保存"那一次请求体里的 currency。
	SavedCurrency string `json:"savedCurrency"`
	// ReopenedValue 是重新打开编辑框时下拉的值。
	ReopenedValue string `json:"reopenedValue"`
}

type fxLegacy struct {
	Value         string `json:"value"`
	OptionText    string `json:"optionText"`
	SavedCurrency string `json:"savedCurrency"`
}

// fxNarrow 是窄屏那一遍的观测值。
type fxNarrow struct {
	Viewport int `json:"viewport"`
	Overflow int `json:"overflow"`
	Cards    map[string]struct {
		Cost         string `json:"cost"`
		Clip         int    `json:"clip"`
		CardOverflow int    `json:"cardOverflow"`
	} `json:"cards"`
}

// ---------------------------------------------------------------- 期望值（Go 侧算）

// convert 与 internal/fx 的除法口径一致：人民币 = 外币 ÷ rate，四舍五入到分。
func convert(cents int64, rate float64) int64 {
	return int64(math.Round(float64(cents) / rate))
}

// moneyText 与 app.js 的 fmtMoney 同一套格式（符号表 + "金额 代码"）。
// 前端改了格式这里会一起红：这正是"页面上的字必须与 Go 算出来的一致"。
func moneyText(cents int64, code string) string {
	symbols := map[string]string{"CNY": "¥", "USD": "$", "EUR": "€", "JPY": "¥", "GBP": "£"}
	amount := strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
	if code == "" {
		return amount
	}
	if sym, ok := symbols[code]; ok {
		return sym + amount + " " + code
	}
	return amount + " " + code
}

// cnyText 是"人民币口径"那一段的格式：只有 ¥ 与金额，不再跟一个 CNY
// （与 app.js 的 moneyBothText 一致：符号本身已经说明是人民币）。
func cnyText(cents int64) string {
	return "¥" + strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
}

func intField(t *testing.T, node map[string]any, key string) int64 {
	t.Helper()
	v, ok := node[key].(float64)
	if !ok {
		t.Fatalf("节点字段 %s 不是数字: %v", key, node[key])
	}
	return int64(v)
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// oneLine 把多行的文本压成一行，便于日志里读。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ---------------------------------------------------------------- 服务端侧的小工具

// newFakeFXProvider 起一个假的汇率数据源（本地 httptest，测试绝不连真实外网）。
func newFakeFXProvider(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitFXReady 等服务端启动时那次取汇率完成（页面可能比它先加载完，会读到兜底值）。
func waitFXReady(t *testing.T, br *browser, wantSource string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body := br.do(http.MethodGet, "/api/v1/settings", nil, false)
		if status == http.StatusOK {
			if meta, ok := body["fx"].(map[string]any); ok {
				if meta["is_default"] == false && meta["source"] == wantSource {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等服务端取到汇率超时（期望来源 %s）", wantSource)
}

// waitFXDefault 等"这一份是内置兜底表"这个状态确定下来（假数据源一定会失败）。
func waitFXDefault(t *testing.T, br *browser) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body := br.do(http.MethodGet, "/api/v1/settings", nil, false)
		if status == http.StatusOK {
			if meta, ok := body["fx"].(map[string]any); ok && meta["is_default"] == true {
				// 再等一拍：确认它不是"还没开始取"的中间状态。
				time.Sleep(200 * time.Millisecond)
				_, again := br.do(http.MethodGet, "/api/v1/settings", nil, false)
				if m2, ok := again["fx"].(map[string]any); ok && m2["is_default"] == true {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等服务端回落到内置兜底表超时")
}

// createPricedNode 建一台带价格的机器，返回 id。
func createPricedNode(t *testing.T, br *browser, name string, cents int64, currency string, months int, expiresAt int64) int64 {
	t.Helper()
	status, body := br.do(http.MethodPost, "/api/v1/nodes", map[string]any{
		"name": name, "interval_sec": 1, "region": "HK",
		"price_cents": cents, "currency": currency, "billing_months": months,
		"expires_at": expiresAt,
	}, true)
	if status != http.StatusCreated {
		t.Fatalf("创建节点 %s 失败: %d %v", name, status, body)
	}
	node, _ := body["node"].(map[string]any)
	id, _ := node["id"].(float64)
	if id <= 0 {
		t.Fatalf("创建节点 %s 没有返回 id: %v", name, body)
	}
	return int64(id)
}

// fetchNodes 取回节点列表（页面上的金额就是它渲染出来的）。
func fetchNodes(t *testing.T, br *browser) []map[string]any {
	t.Helper()
	status, body := br.do(http.MethodGet, "/api/v1/nodes", nil, false)
	if status != http.StatusOK {
		t.Fatalf("读取节点列表失败: %d", status)
	}
	raw, _ := body["nodes"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if node, ok := item.(map[string]any); ok {
			out = append(out, node)
		}
	}
	return out
}

func nodeByID(t *testing.T, nodes []map[string]any, id int64) map[string]any {
	t.Helper()
	for _, node := range nodes {
		if int64(node["id"].(float64)) == id {
			return node
		}
	}
	t.Fatalf("节点列表里没有 id=%d", id)
	return nil
}

// assertRoutes 检查每条路由上"有没有新的 JS 报错"以及"最后停在哪个视图"。
func assertRoutes(t *testing.T, routes []groupRoute, want map[string]string) {
	t.Helper()
	if len(routes) != len(want) {
		t.Fatalf("只走了 %d 条路由，期望 %d 条：%+v", len(routes), len(want), routes)
	}
	for _, r := range routes {
		t.Logf("路由 %s → 视图 %s（这一段里有 %d 条 JS 报错）", r.Hash, r.View, r.Errs)
		if r.Errs != 0 {
			t.Errorf("路由 %s 上有 %d 条 JS 报错", r.Hash, r.Errs)
		}
		expected, ok := want[r.Hash]
		if !ok {
			t.Errorf("浏览器走了一条没登记的路由 %s", r.Hash)
			continue
		}
		if r.View != expected {
			t.Errorf("路由 %s 最后停在 %q，期望 %q", r.Hash, r.View, expected)
		}
	}
}

// ---------------------------------------------------------------- 人工核对截图

// TestFXScreenshots 产出人工核对用的五张 PNG：
//
//	fx-dialog      编辑节点对话框（货币是下拉，选中 USD）
//	fx-legacy      老币种节点（XYZ）的编辑框 —— 下拉里临时补出来的「XYZ（库里的原值）」
//	fx-detail      USD 节点的详情页（价格/月均/剩余价值三处双币显示）
//	fx-settings    设置页「服务器信息」里的汇率元信息（哪天的、来源、是否兜底）
//	fx-home-narrow 窄屏手机的首页卡片 —— 费用那一行变长了会不会撑破卡片
//
// 窄屏那张用 --force-device-scale-factor=2 把 CSS 视口折半：Windows 上 Chrome 的
// 窗口有最小宽度（实测 485px，`--window-size=380` 拿不到 380 的视口，截出来只是
// 485px 布局的裁剪）。折半之后 CSS 视口是真的 380px，才是手机的布局。
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）：要看图时设
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestFXScreenshots -v
func TestFXScreenshots(t *testing.T) {
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

	h, names := startFXShotFixture(t)

	shots := []struct {
		name  string
		shot  string
		focus string
		w, h  int
		scale int
	}{
		{"fx-dialog", "dialog", "usd-01", 1500, 1000, 1},
		{"fx-legacy", "legacy", "usd-01", 1500, 1000, 1},
		{"fx-detail", "detail", "usd-01", 1500, 1000, 1},
		{"fx-settings", "settings", "usd-01", 1500, 1000, 1},
		// 窄屏再看一眼首页卡片：加了一段人民币口径之后费用那一行更长了，
		// "会不会把卡片撑破 / 折得没法读"只有看图才知道。
		// 2 倍缩放 + 760 的窗口 ⇒ CSS 视口 380px（Windows 上直接传 380 拿不到）。
		{"fx-home-narrow", "home", "usd-01", 760, 1800, 2},
	}
	for _, s := range shots {
		if only := os.Getenv("PROBE_SHOT_ONLY"); only != "" && !strings.Contains(only, s.name) {
			continue
		}
		cfg := tzHarnessConfig{
			Scenario: "fx", User: "admin", Pass: "a-very-good-password",
			Nodes: names, Focus: s.focus, Shot: s.shot,
		}
		mock := newMockServer(t, newShotProxyWith(t, "http://"+h.addr, cfg, fxHarnessJS))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把界面开到目标状态
		// （自动化断言那条路径故意不用它：长连接 SSE 会让虚拟时间暂停）。
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + "/",
		}
		if s.scale > 1 {
			args = append(args, "--force-device-scale-factor="+strconv.Itoa(s.scale))
		}
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
		case <-time.After(90 * time.Second):
			killChrome(cmd)
			t.Fatalf("%s: Chrome 超时。输出尾部：\n%s", s.name, tail(buf.String(), 800))
		}
		st, err := os.Stat(out)
		if err != nil {
			t.Fatalf("%s: 截图没有生成: %v\nChrome 输出尾部：\n%s", s.name, err, tail(buf.String(), 800))
		}
		t.Logf("%s → %s（%d 字节）", s.name, out, st.Size())
	}
}

// startFXShotFixture 起一个装了"美元 / 人民币 / 老币种"三台机器的服务端，
// 汇率来自本地假数据源（1 CNY = 0.2 USD）。截图与自动化断言用的是同一份数据。
func startFXShotFixture(t *testing.T) (*harness, map[string]int64) {
	t.Helper()
	provider := newFakeFXProvider(t, `{"base":"CNY","date":"2026-03-02","rates":{"USD":0.2,"EUR":0.25,"JPY":20}}`)
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
		func(cfg *config.Server) { cfg.FXRateURL = provider.URL })
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	waitFXReady(t, br, provider.URL)

	expires := time.Now().Add(45*24*time.Hour + 6*time.Hour).Unix()
	return h, map[string]int64{
		"usd-01": createPricedNode(t, br, "usd-01", 10000, "USD", 1, expires),
		"cny-01": createPricedNode(t, br, "cny-01", 3100, "CNY", 1, expires),
		"old-01": createPricedNode(t, br, "old-01", 12345, "XYZ", 1, expires),
	}
}

// ---------------------------------------------------------------- 浏览器里的自检脚本

// fxHarnessJS 是注入到页面里的自检脚本（与其它浏览器用例同一套做法：
// 真服务端 + 反代注入 + 结果 POST 回 mock，断言全部在 Go 那边做）。
//
// 它按用户的真实操作驱动界面：登录 → 首页读卡片费用 → 进详情页读三格 →
// 进设置页读服务器列表与汇率元信息 → 打开编辑框（看货币控件、换币种保存、
// 重新打开核对）→ 老币种打开+保存 → 四条路由各走一遍。
const fxHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var NODES = CFG.nodes || {};
  var NAMES = Object.keys(NODES);
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], steps: [], fatal: '',
    cards: {}, detail: {}, list: {},
    currency: {}, edit: {}, legacy: {}, fxInfo: '', narrow: { cards: {} }, routes: []
  };
  window.__FXRESULT = R;

  // 截图模式：把 EventSource 换成不联网的替身（与其它截图用例同一个理由：
  // 挂着的 SSE 长连接会让 --virtual-time-budget 永远耗不完，Chrome 就不截图了）。
  if (CFG.shot) {
    window.EventSource = function () {
      var listeners = {};
      this.addEventListener = function (name, fn) { (listeners[name] = listeners[name] || []).push(fn); };
      this.close = function () {};
      setTimeout(function () {
        (listeners['open'] || []).forEach(function (fn) { fn({ type: 'open' }); });
      }, 0);
    };
  }

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // ---- 记下"写节点"的请求体（保存后要核对 currency 到底是什么）-------------
  var writes = [];
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var method = ((init && init.method) || 'GET').toUpperCase();
    if (method !== 'GET' && url.indexOf('/api/v1/nodes') >= 0 && init && init.body) {
      try { writes.push(JSON.parse(init.body)); } catch (e) { /* 不是 JSON 就不记 */ }
    }
    return rawFetch(input, init);
  };

  // ---- 小工具 -------------------------------------------------------------
  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
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

  function login() {
    function loggedIn() {
      return shown('view-home') || shown('view-detail') || shown('view-settings');
    }
    return waitFor('出现登录视图', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', loggedIn, 30000);
      });
  }

  function goHome() {
    window.location.hash = '#/';
    return waitFor('首页三张卡片都画出来', function () {
      return shown('view-home') && node('grid') && node('grid').children.length === NAMES.length;
    }, 30000).then(function () { return sleep(400); });
  }

  // 首页卡片：按名字找到卡片，读「费用」那一行的值（隐藏时也记一笔）。
  function cardOf(name) {
    var grid = node('grid');
    if (!grid) return null;
    var found = null;
    Array.prototype.forEach.call(grid.children, function (card) {
      var n = card.querySelector('.card-name');
      if (n && n.textContent === name) found = card;
    });
    return found;
  }

  function readCards() {
    NAMES.forEach(function (name) {
      var card = cardOf(name);
      if (!card) { R.cards[name] = { cost: '（没有这张卡片）', hidden: true }; return; }
      var cost = '';
      var hidden = true;
      Array.prototype.forEach.call(card.querySelectorAll('.line'), function (line) {
        var label = line.querySelector('.line-label');
        if (label && label.textContent === '费用') {
          var value = line.querySelector('.line-value');
          cost = value ? value.textContent : '';
          hidden = !!line.hidden;
        }
      });
      R.cards[name] = { cost: cost, hidden: hidden };
    });
  }

  function goDetail(name) {
    window.location.hash = '#/n/' + NODES[name];
    return waitFor('详情页打开：' + name, function () {
      return shown('view-detail') && textOf('detail-name') === name;
    }, 30000).then(function () { return sleep(400); });
  }

  function readDetail(name) {
    R.detail[name] = {
      price: textOf('stat-price'),
      monthly: textOf('stat-monthly'),
      value: textOf('stat-value')
    };
  }

  function goSettings(pane, waitForRows) {
    window.location.hash = '#/settings/' + pane;
    return waitFor('设置页打开：' + pane, function () {
      return shown('view-settings');
    }, 30000).then(function () {
      if (!waitForRows) return sleep(400);
      return waitFor('服务器列表就绪', function () {
        return node('nodes-list') && node('nodes-list').children.length === NAMES.length;
      }, 30000).then(function () { return sleep(400); });
    });
  }

  function rowOf(name) {
    var list = node('nodes-list');
    if (!list) return null;
    var found = null;
    Array.prototype.forEach.call(list.children, function (row) {
      var n = row.querySelector('.node-item-name');
      if (n && n.textContent === name) found = row;
    });
    return found;
  }

  function readList() {
    NAMES.forEach(function (name) {
      var row = rowOf(name);
      var meta = row ? row.querySelector('.node-item-meta') : null;
      R.list[name] = meta ? meta.textContent : '（没有这一行）';
    });
  }

  // 通过服务器列表那一行的「编辑节点」按钮打开对话框。
  function openEdit(name) {
    var row = rowOf(name);
    if (!row) throw new Error('服务器列表里找不到 ' + name);
    var btn = row.querySelector('.node-item-acts button');
    if (!btn) throw new Error(name + ' 那一行没有编辑按钮');
    btn.click();
    return waitFor('编辑对话框打开：' + name, function () {
      return node('dlg-node') && node('dlg-node').open;
    }, 15000).then(function () { return sleep(200); });
  }

  function currencyInfo() {
    var sel = node('node-currency');
    var options = [];
    for (var i = 0; i < sel.options.length; i++) options.push(sel.options[i].value);
    var legacy = sel.querySelector('option[data-legacy]');
    return {
      tag: sel.tagName,
      options: options,
      value: sel.value,
      optionText: legacy ? legacy.textContent : ''
    };
  }

  function submitDialog() {
    var before = writes.length;
    node('node-submit').click();
    return waitFor('对话框关闭（保存成功）', function () {
      return !node('dlg-node').open;
    }, 20000).then(function () {
      return sleep(400).then(function () {
        return writes.length > before ? writes[writes.length - 1] : null;
      });
    });
  }

  function readFXInfo() {
    return goSettings('server', false).then(function () {
      return waitFor('服务器信息已渲染', function () {
        return node('server-info') && node('server-info').children.length > 0;
      }, 15000);
    }).then(function () {
      R.fxInfo = textOf('server-info');
    });
  }

  function currentView() {
    var views = ['view-home', 'view-detail', 'view-settings', 'view-login', 'view-setup'];
    for (var i = 0; i < views.length; i++) {
      if (shown(views[i])) {
        if (views[i] !== 'view-settings') return views[i];
        var pane = node('settings-panes').querySelector('.pane:not([hidden])');
        return 'view-settings:' + (pane ? pane.dataset.pane : '?');
      }
    }
    return '（没有可见视图）';
  }

  function routePass() {
    // 详情页那条路由走 CFG.focus 指定的那台机器（Go 侧用它算期望值，
    // 两边必须指同一台；名字在 JSON 里是按字典序排的，别去猜"第一个"是谁）。
    var focus = CFG.focus || NAMES[0];
    var routes = ['#/', '#/n/' + NODES[focus], '#/settings/nodes', '#/settings/alert'];
    var out = [];
    return routes.reduce(function (chain, hash) {
      return chain.then(function () {
        R.routeErrsBefore = R.errs.length;
        window.location.hash = hash;
        return sleep(1000);
      }).then(function () {
        out.push({ hash: hash, errs: R.errs.length - R.routeErrsBefore, view: currentView() });
        return true;
      });
    }, Promise.resolve()).then(function () { R.routes = out; return true; });
  }

  // ---- 场景三：窄屏（380px）下再走一遍 -----------------------------------
  //
  // 费用那一行现在多了一段人民币口径，窄屏上最容易出问题的就是它：
  // 卡片被撑破（整页横向滚动），或者金额被截得读不出来。
  function narrowPass() {
    var doc = document.documentElement;
    R.narrow.viewport = doc.clientWidth;
    R.narrow.overflow = doc.scrollWidth - doc.clientWidth;
    R.narrow.cards = {};
    NAMES.forEach(function (name) {
      var card = cardOf(name);
      if (!card) return;
      var info = { cost: '', clip: 0, cardOverflow: card.scrollWidth - card.clientWidth };
      Array.prototype.forEach.call(card.querySelectorAll('.line'), function (line) {
        var label = line.querySelector('.line-label');
        if (label && label.textContent === '费用') {
          var value = line.querySelector('.line-value');
          info.cost = value ? value.textContent : '';
          info.clip = value ? value.scrollWidth - value.clientWidth : 0;
        }
      });
      R.narrow.cards[name] = info;
    });
    return sleep(200);
  }

  // ---- 场景一：完整流程 ---------------------------------------------------
  function fullPass() {
    return goHome()
      .then(function () { readCards(); return goDetail('usd-01'); })
      .then(function () { readDetail('usd-01'); return goDetail('cny-01'); })
      .then(function () { readDetail('cny-01'); return goDetail('old-01'); })
      .then(function () { readDetail('old-01'); return goSettings('nodes', true); })
      .then(function () { readList(); return readFXInfo(); })
      .then(function () { return goSettings('nodes', true); })
      .then(function () { return openEdit('usd-01'); })
      .then(function () {
        R.currency = currencyInfo();
        // 选一个**非 CNY** 的币种保存：请求体里必须是它。
        node('node-currency').value = 'EUR';
        return submitDialog();
      })
      .then(function (body) {
        R.edit.savedCurrency = body ? body.currency : '';
        return goSettings('nodes', true);
      })
      .then(function () { return openEdit('usd-01'); })
      .then(function () {
        R.edit.reopenedValue = node('node-currency').value;
        node('node-cancel').click();
        return sleep(200);
      })
      .then(function () { return openEdit('old-01'); })
      .then(function () {
        var info = currencyInfo();
        R.legacy.value = info.value;
        R.legacy.optionText = info.optionText;
        // 什么都不改，直接保存：库里的币种必须原样回去。
        return submitDialog();
      })
      .then(function (body) {
        R.legacy.savedCurrency = body ? body.currency : '';
      });
  }

  // ---- 场景二：取不到汇率（内置兜底表）----------------------------------
  function defaultPass() {
    return goHome().then(function () {
      readCards();
      return goDetail(NAMES[0]);
    }).then(function () {
      readDetail(NAMES[0]);
      return readFXInfo();
    });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:fx'; });
  }

  // 截图模式（人工核对用）：把界面开到指定状态就停住，不回传结果。
  //   dialog   → 打开 usd-01 的编辑框（看货币下拉）
  //   legacy   → 打开 old-01 的编辑框（看"库里的原值"那个临时选项）
  //   detail   → usd-01 的详情页（看价格处的双币显示）
  //   settings → 设置页的服务器信息（看汇率元信息）
  //   home     → 首页卡片（窄屏下看费用那一行会不会被撑破）
  function shot() {
    var mode = CFG.shot;
    var focus = CFG.focus || NAMES[0];
    return login().then(function () {
      if (mode === 'settings') return readFXInfo();
      if (mode === 'home') return goHome();
      if (mode === 'detail') return goDetail(focus);
      return goSettings('nodes', true).then(function () {
        return openEdit(mode === 'legacy' ? 'old-01' : focus);
      });
    }).then(function () { return sleep(600); })
      .then(function () { document.title = 'SHOT-READY:' + mode; });
  }

  window.addEventListener('load', function () {
    var scenario = CFG.scenario || 'fx';
    if (CFG.shot) {
      shot().catch(function (err) {
        document.title = 'SHOT-FAILED:' + String(err && err.message ? err.message : err);
      });
      return;
    }
    waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(function () {
        if (scenario === 'fxnarrow') return goHome().then(narrowPass);
        return scenario === 'fxdefault' ? defaultPass() : fullPass();
      })
      .then(routePass)
      .catch(function (err) { R.fatal = String(err && err.message ? err.message : err); })
      .then(finish, finish);
  });
})();`
