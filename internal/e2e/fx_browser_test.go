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
	"sort"
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
// 卡片照渲染、数字照样有，只是外币没折成人民币、或者三处口径不一致、
// 或者没有可用汇率时把一个没换算过的数冒充成了人民币。这些在静态断言里都看不出来。
//
// 三个场景（各跑一次 Chrome）：
//  1. 注入一份已知汇率（1 CNY = 0.2 USD）的**假数据源**（本地 httptest，绝不连外网）：
//     USD 节点在首页卡片/详情页/服务器列表三处都只显示「¥Y」（Y 由 Go 侧按同一个
//     汇率独立算出来比对，且**不许**再出现原币种与双币连接符）；CNY 与"没填币种"的
//     节点也显示 ¥金额、后面不再跟币种代码；老币种（XYZ）能显示、保存不被改成 CNY；
//     空币种的老节点打开编辑框能看见「（未设置）」、保存后仍然是空；
//     货币下拉**没有空选项**、新建节点时默认人民币；设置页能看到汇率的元信息
//     （哪天的、来源、是否兜底）。
//  2. 数据源返回垃圾（取不到汇率，is_default=true，用的是内置兜底表）：USD 照常
//     换算成人民币；兜底表里**没有**的币种（XYZ）在三个地方都如实退回原币种金额 ——
//     页面不报错、也不出现一个冒充过的人民币数字。
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

	// 四台机器：USD（有汇率）、CNY（币种就是人民币）、XYZ（老数据里的怪币种，
	// 汇率表里没有它）、以及**币种为空**的老节点（库里存的是空串）。
	// 到期日都设成"45 天零 6 小时之后"：剩余天数在接下来 6 小时里恒为 45，
	// 页面渲染与 Go 侧算期望值时不会因为跨过一天的边界而对不上。
	expires := time.Now().Add(45*24*time.Hour + 6*time.Hour).Unix()
	usdID := createPricedNode(t, br, "usd-01", 10000, "USD", 1, expires)
	cnyID := createPricedNode(t, br, "cny-01", 3100, "CNY", 1, expires)
	legacyID := createPricedNode(t, br, "old-01", 12345, "XYZ", 1, expires)
	emptyID := createPricedNode(t, br, "empty-01", 5000, "", 1, expires)

	names := map[string]int64{
		"usd-01": usdID, "cny-01": cnyID, "old-01": legacyID, "empty-01": emptyID,
	}

	// 期望值在 Go 侧独立算：拿服务端给的**原始金额**，按注入的汇率（0.2）除一遍。
	// 页面上出现的那串字必须与它一模一样 —— 而且**只有**这一串：原币种那一截
	// 与双币连接符都该没了（用户要的是"统一显示人民币"）。
	//
	// 快照要在**跑浏览器之前**取：脚本后面会把 usd-01 的币种改成 EUR（那正是
	// 要验的操作），改完之后再读 API 拿到的就不是"页面当时渲染的那份数据"了。
	nodes := fetchNodes(t, br)
	usd := nodeByID(t, nodes, usdID)
	cny := nodeByID(t, nodes, cnyID)
	old := nodeByID(t, nodes, legacyID)
	rate := 0.2
	wantUSDPrice := cnyText(convert(intField(t, usd, "price_cents"), rate)) + " / 月"
	wantUSDMonthly := cnyText(convert(intField(t, usd, "monthly_cents"), rate)) + " / 月"
	wantUSDValue := cnyText(convert(intField(t, usd, "remaining_value_cents"), rate))
	// 没有可用汇率的币种（XYZ）：三处都应当**原样**显示原币种金额。
	wantOldPrice := moneyText(intField(t, old, "price_cents"), "XYZ") + " / 月"
	wantOldValue := moneyText(intField(t, old, "remaining_value_cents"), "XYZ")
	// 币种为空：服务端按人民币处理（见 internal/fx 的 ToCNY），所以显示 ¥金额。
	empty := nodeByID(t, nodes, emptyID)
	wantEmptyPrice := moneyText(intField(t, empty, "price_cents"), "") + " / 月"
	wantEmptyValue := moneyText(intField(t, empty, "remaining_value_cents"), "")
	// CNY 节点同理：人民币金额后面**不再跟一个 CNY**（用户要的"去掉废话"）。
	wantCNYPrice := moneyText(intField(t, cny, "price_cents"), "CNY") + " / 月"
	wantCNYValue := moneyText(intField(t, cny, "remaining_value_cents"), "CNY")

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

	// 1) 首页卡片：费用行**只**显示人民币。
	if got := res.Cards["usd-01"].Cost; !strings.Contains(got, wantUSDPrice) {
		t.Errorf("首页卡片的费用行 = %q，期望包含 %q（只显示人民币）", got, wantUSDPrice)
	} else {
		assertOnlyCNY(t, "首页卡片 usd-01 的费用行", got)
		t.Logf("首页卡片 usd-01 费用行 = %q", got)
	}

	// 2) 详情页：三格都只有人民币。
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
	assertOnlyCNY(t, "详情页 usd-01 的价格", detail.Price)
	assertOnlyCNY(t, "详情页 usd-01 的月均", detail.Monthly)
	assertOnlyCNY(t, "详情页 usd-01 的剩余价值", detail.Value)
	t.Logf("详情页 usd-01：价格 %q / 月均 %q / 剩余价值 %q", detail.Price, detail.Monthly, detail.Value)

	// 3) 服务器列表：剩余价值同一口径。整行还带着 IP/分组/到期天数（那些是
	//    「有才显示」的别的字段），所以按"包含"断言，再单独确认整行只有一个 ¥。
	wantList := "剩余价值 " + wantUSDValue
	if got := res.List["usd-01"]; !strings.Contains(got, wantList) {
		t.Errorf("服务器列表 usd-01 的信息行 = %q，期望包含 %q", got, wantList)
	} else {
		assertOnlyCNY(t, "服务器列表 usd-01 的信息行", got)
		t.Logf("服务器列表 usd-01 = %q", got)
	}

	// 4) 币种本来就是人民币的节点：金额**显示不变**（¥31.00），但那个多余的
	//    CNY 代码字样已经去掉 —— 既然只剩人民币了，再写一遍币种代码是废话。
	cnyDetail := res.Detail["cny-01"]
	if cnyDetail.Price != wantCNYPrice {
		t.Errorf("CNY 节点详情页价格 = %q，期望 %q（¥金额 + 周期，后面不再跟一个 CNY）", cnyDetail.Price, wantCNYPrice)
	}
	if cnyDetail.Value != wantCNYValue {
		t.Errorf("CNY 节点详情页剩余价值 = %q，期望 %q", cnyDetail.Value, wantCNYValue)
	}
	assertOnlyCNY(t, "CNY 节点详情页价格", cnyDetail.Price)
	assertOnlyCNY(t, "CNY 节点详情页剩余价值", cnyDetail.Value)
	assertOnlyCNY(t, "CNY 节点首页卡片", res.Cards["cny-01"].Cost)
	assertOnlyCNY(t, "CNY 节点服务器列表", res.List["cny-01"])
	t.Logf("CNY 节点：卡片 %q / 详情价格 %q / 详情剩余价值 %q / 列表 %q",
		res.Cards["cny-01"].Cost, cnyDetail.Price, cnyDetail.Value, res.List["cny-01"])

	// 4b) **币种为空**的老节点：服务端把它当人民币处理（见 internal/fx 的 ToCNY），
	//     所以三个地方都显示 ¥金额。它不是"没有汇率"，不该退回成一个光秃秃的数字
	//     —— 那样同一台机器在卡片与总览区就会一个带 ¥、一个不带。
	if got := res.Cards["empty-01"].Cost; !strings.Contains(got, wantEmptyPrice) {
		t.Errorf("空币种节点首页卡片 = %q，期望包含 %q（没填币种按人民币处理）", got, wantEmptyPrice)
	}
	if got := res.Detail["empty-01"].Value; got != wantEmptyValue {
		t.Errorf("空币种节点详情页剩余价值 = %q，期望 %q（没填币种按人民币处理）", got, wantEmptyValue)
	}
	assertOnlyCNY(t, "空币种节点首页卡片", res.Cards["empty-01"].Cost)
	assertOnlyCNY(t, "空币种节点服务器列表", res.List["empty-01"])
	t.Logf("空币种节点：卡片 %q / 详情剩余价值 %q / 列表 %q",
		res.Cards["empty-01"].Cost, res.Detail["empty-01"].Value, res.List["empty-01"])

	// 5) 老币种（XYZ）：**这一份汇率表里没有它**，三个地方都要如实退回原币种金额 ——
	//    绝不冒充成换算过的人民币。
	//
	// 这是这一轮最容易漏的一条：服务端在换算不了时把人民币口径退回的是**原值**
	// （语义就是"这个数没换算过"），前端要是照着人民币口径渲染，页面上就会出现
	// 一个看起来完全正常的"人民币"金额，拿去对账差一整截。
	if got := res.Cards["old-01"].Cost; !strings.Contains(got, wantOldPrice) {
		t.Errorf("老币种节点首页卡片 = %q，期望包含 %q（没有可用汇率就退回原币种金额）", got, wantOldPrice)
	}
	if got := res.Detail["old-01"].Price; got != wantOldPrice {
		t.Errorf("老币种节点详情页价格 = %q，期望 %q（没有可用汇率就退回原币种金额）", got, wantOldPrice)
	}
	if got := res.Detail["old-01"].Value; got != wantOldValue {
		t.Errorf("老币种节点详情页剩余价值 = %q，期望 %q", got, wantOldValue)
	}
	if got := res.List["old-01"]; !strings.Contains(got, "剩余价值 "+wantOldValue) {
		t.Errorf("老币种节点服务器列表 = %q，期望包含 %q", got, "剩余价值 "+wantOldValue)
	}
	for _, place := range []struct{ where, got string }{
		{"首页卡片", res.Cards["old-01"].Cost},
		{"详情页价格", res.Detail["old-01"].Price},
		{"详情页剩余价值", res.Detail["old-01"].Value},
		{"服务器列表", res.List["old-01"]},
	} {
		if strings.Contains(place.got, "¥") {
			t.Errorf("老币种节点%s = %q：没有可用汇率时不许出现人民币金额（会让用户以为换算过了）", place.where, place.got)
		}
	}
	t.Logf("老币种节点：卡片 %q / 详情价格 %q / 详情剩余价值 %q / 列表 %q",
		res.Cards["old-01"].Cost, res.Detail["old-01"].Price, res.Detail["old-01"].Value, res.List["old-01"])

	// 6) 编辑框里的货币是**下拉**，而且里面**没有空选项**（「不填」已经去掉）。
	if res.Currency.Tag != "SELECT" {
		t.Errorf("货币控件的标签 = %q，期望 SELECT（下拉）", res.Currency.Tag)
	}
	if len(res.Currency.Options) < 10 {
		t.Errorf("货币下拉只有 %d 个选项：%v", len(res.Currency.Options), res.Currency.Options)
	}
	if containsString(res.Currency.Options, "") {
		t.Errorf("货币下拉里还有空选项：%v（「不填」应当已经去掉，默认人民币）", res.Currency.Options)
	}
	// 一个都没选中（selectedIndex = -1）时下拉显示成一片空白：这在美元节点上
	// 不该发生（USD 就在列表里）。
	if res.Currency.SelectedIndex < 0 {
		t.Errorf("usd-01 的编辑框里货币下拉一个都没选中（selectedIndex=%d）：控件会显示成一片空白", res.Currency.SelectedIndex)
	}
	t.Logf("货币下拉：%d 个选项（%s …）", len(res.Currency.Options), strings.Join(res.Currency.Options[:3], ", "))
	t.Logf("usd-01 编辑框：控件 %s，选中 %q", res.Currency.Tag, res.Currency.Value)

	// 6b) **新建**节点：货币下拉必须默认人民币。
	//
	// 顺序上它排在最后是有意的：这会话里刚刚依次编辑过 USD → EUR、老币种 XYZ、
	// 以及空币种，下拉里还残留着为它们临时补出来的选项。新建时看到 CNY 才能证明
	// "上一次编辑留下的选择"没有漏过来（这也正是默认值必须写在 JS 里、
	// 不能只靠 index.html 的 selected 的原因）。
	if res.NewCurrency.Value != "CNY" {
		t.Errorf("新建节点时货币下拉 = %q，期望 CNY（直接默认人民币）", res.NewCurrency.Value)
	}
	if res.NewCurrency.SelectedIndex != 0 {
		t.Errorf("新建节点时货币下拉选中的下标 = %d，期望 0（人民币是第一项）", res.NewCurrency.SelectedIndex)
	}
	if !strings.Contains(res.NewCurrency.SelectedText, "CNY") {
		t.Errorf("新建节点时选中的那一项文本 = %q，期望是人民币那一项", res.NewCurrency.SelectedText)
	}
	if containsString(res.NewCurrency.Options, "") {
		t.Errorf("新建节点时货币下拉里有空选项：%v", res.NewCurrency.Options)
	}
	t.Logf("新建节点：货币下拉 %d 个选项，选中 %q（文本 %q）",
		len(res.NewCurrency.Options), res.NewCurrency.Value, res.NewCurrency.SelectedText)

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

	// 7b) **币种为空**的老节点：同样要看得见、同样要原样保存。
	//
	// 这里是"去掉空选项"最容易出事的地方：没有那个临时补出来的「（未设置）」选项，
	// select.value = '' 会让它一个都不选中（selectedIndex = -1，显示成一片空白）——
	// 用户既看不出它原来是"没设置"，保存时还会把一个空 value 当成他的选择。
	if res.Empty.Value != "" {
		t.Errorf("空币种节点打开编辑框时下拉的值 = %q，期望空串（不许静默填成 CNY）", res.Empty.Value)
	}
	if res.Empty.SelectedIndex < 0 {
		t.Errorf("空币种节点打开编辑框时下拉一个都没选中（selectedIndex=%d）：用户看不出它原来是「没设置」", res.Empty.SelectedIndex)
	}
	if !strings.Contains(res.Empty.SelectedText, "未设置") {
		t.Errorf("空币种节点选中的那一项文本 = %q，期望能看出它「未设置」（例如「（未设置）」）", res.Empty.SelectedText)
	}
	if res.Empty.SavedCurrency != "" {
		t.Errorf("空币种节点保存时发出去的 currency = %q，期望空串（打开看一眼再保存不该填上一个币种）", res.Empty.SavedCurrency)
	}
	t.Logf("空币种：下拉值 %q（选中项下标 %d，文本 %q）保存请求体 currency=%q",
		res.Empty.Value, res.Empty.SelectedIndex, res.Empty.SelectedText, res.Empty.SavedCurrency)

	// 7c) 落库核对：上面那两次"打开看一眼再保存"之后，库里的值必须**一个都没变**。
	//     页面上显示什么与库里存着什么完全是两回事 —— 只有回读接口才能证明
	//     没有把用户的币种改坏（这正是上一轮明确定过的那条红线）。
	after := fetchNodes(t, br)
	if got, _ := nodeByID(t, after, legacyID)["currency"].(string); got != "XYZ" {
		t.Errorf("保存之后库里老币种节点的 currency = %q，期望仍然是 XYZ（被界面改坏了）", got)
	}
	if got, _ := nodeByID(t, after, emptyID)["currency"].(string); got != "" {
		t.Errorf("保存之后库里空币种节点的 currency = %q，期望仍然是空串（被界面改坏了）", got)
	}

	// 7d) **新建**节点：默认人民币要真的落库，而且"不填价格"的机器必须仍然建得出来。
	//
	// 这里卡着一条服务端的硬规则："没有计费周期时，价格与货币都必须留空"
	// （见 store.Node.Validate）。下拉默认选中人民币之后，如果请求体照直把 CNY 发出去，
	// 新建一台不填价格的机器就会被拒 —— 用户什么都没做错却建不出来。
	// 所以"有周期才发币种"这条判断必须成立，两边都要验：
	// 填了价格的 → CNY；没填价格的 → 空串（而且真的建出来了）。
	if res.Created.NoPriceCurrency != "" {
		t.Errorf("新建一台**不填价格**的机器时请求体 currency = %q，期望空串（服务端要求没有计费周期时价格与货币都留空）",
			res.Created.NoPriceCurrency)
	}
	if res.Created.PricedCurrency != "CNY" {
		t.Errorf("新建一台填了价格的机器时请求体 currency = %q，期望 CNY（下拉默认人民币，用户没碰过它）",
			res.Created.PricedCurrency)
	}
	if got, _ := nodeByName(t, after, "noprice-01")["currency"].(string); got != "" {
		t.Errorf("不填价格建出来的机器落库 currency = %q，期望空串", got)
	}
	if got, _ := nodeByName(t, after, "priced-01")["currency"].(string); got != "CNY" {
		t.Errorf("填了价格建出来的机器落库 currency = %q，期望 CNY（默认值没有真的存下去）", got)
	}
	t.Logf("新建节点：不填价格的请求体 currency=%q、填了价格的 currency=%q（都已落库核对）",
		res.Created.NoPriceCurrency, res.Created.PricedCurrency)

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

	// 11) 窄屏（380px）再跑一遍：费用那一行现在只剩人民币了，短了不等于没问题
	//     （换行、截断、整页横向滚动都要看一眼）。
	narrow := runFXNarrow(t, chrome, "http://"+h.addr, names)
	if narrow.Overflow > 0 {
		t.Errorf("窄屏（视口 %dpx）下页面横向溢出了 %dpx：费用那一行撑破了卡片",
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

	// 两台机器：USD（内置兜底表里有它的汇率）与 XYZ（兜底表里**没有**这个币种）。
	// 后者才是这一条要验的东西：换算不了时，三个地方都必须如实退回原币种金额。
	expires := time.Now().Add(45*24*time.Hour + 6*time.Hour).Unix()
	usdID := createPricedNode(t, br, "usd-01", 10000, "USD", 1, expires)
	xyzID := createPricedNode(t, br, "xyz-01", 12345, "XYZ", 1, expires)

	nodes := fetchNodes(t, br)
	usd := nodeByID(t, nodes, usdID)
	xyz := nodeByID(t, nodes, xyzID)
	// 期望值用内置兜底表在 Go 侧算（与页面无关）。
	rate, ok := fx.Default().Rate("USD")
	if !ok {
		t.Fatal("内置兜底表里没有 USD：价格会给不出人民币口径")
	}
	wantUSDPrice := cnyText(convert(intField(t, usd, "price_cents"), rate)) + " / 月"
	wantUSDValue := cnyText(convert(intField(t, usd, "remaining_value_cents"), rate))
	wantXYZPrice := moneyText(intField(t, xyz, "price_cents"), "XYZ") + " / 月"
	wantXYZValue := moneyText(intField(t, xyz, "remaining_value_cents"), "XYZ")

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		Scenario: "fxdefault", User: "admin", Pass: "a-very-good-password",
		Nodes: map[string]int64{"usd-01": usdID, "xyz-01": xyzID}, Focus: "usd-01",
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

	// USD 在兜底表里，三个地方照样是人民币金额（绝不能因为取不到实时汇率就变成
	// 0/空白 —— 那是三级降级里第 ③ 级存在的唯一理由）。
	if got := res.Cards["usd-01"].Cost; !strings.Contains(got, wantUSDPrice) {
		t.Errorf("取不到实时汇率时首页卡片 = %q，期望包含 %q（内置兜底表也要能换算成人民币）", got, wantUSDPrice)
	}
	if got := res.Detail["usd-01"].Price; got != wantUSDPrice {
		t.Errorf("取不到实时汇率时详情页价格 = %q，期望 %q（内置兜底表换算）", got, wantUSDPrice)
	}
	if got := res.Detail["usd-01"].Value; got != wantUSDValue {
		t.Errorf("取不到实时汇率时详情页剩余价值 = %q，期望 %q", got, wantUSDValue)
	}
	if got := res.List["usd-01"]; !strings.Contains(got, "剩余价值 "+wantUSDValue) {
		t.Errorf("取不到实时汇率时服务器列表 = %q，期望包含 %q", got, "剩余价值 "+wantUSDValue)
	}
	t.Logf("取不到实时汇率时 usd-01：卡片 %q / 详情价格 %q / 详情剩余价值 %q / 列表 %q",
		res.Cards["usd-01"].Cost, res.Detail["usd-01"].Price, res.Detail["usd-01"].Value, res.List["usd-01"])

	// XYZ 在兜底表里没有 —— **三个地方**都要退回原币种金额，一个 ¥ 都不许有。
	//
	// 这条与上一段是一对：取不到汇率时，能换算的照常换算，换算不了的如实退回。
	// 两件事都必须成立，只做一半都是错的（后者会让用户把一个没换算过的数当人民币）。
	if got := res.Cards["xyz-01"].Cost; !strings.Contains(got, wantXYZPrice) {
		t.Errorf("没有可用汇率的币种在首页卡片 = %q，期望包含 %q（退回原币种金额）", got, wantXYZPrice)
	}
	if got := res.Detail["xyz-01"].Price; got != wantXYZPrice {
		t.Errorf("没有可用汇率的币种在详情页价格 = %q，期望 %q（退回原币种金额）", got, wantXYZPrice)
	}
	if got := res.Detail["xyz-01"].Value; got != wantXYZValue {
		t.Errorf("没有可用汇率的币种在详情页剩余价值 = %q，期望 %q（退回原币种金额）", got, wantXYZValue)
	}
	if got := res.List["xyz-01"]; !strings.Contains(got, "剩余价值 "+wantXYZValue) {
		t.Errorf("没有可用汇率的币种在服务器列表 = %q，期望包含 %q", got, "剩余价值 "+wantXYZValue)
	}
	for _, place := range []struct{ where, got string }{
		{"首页卡片", res.Cards["xyz-01"].Cost},
		{"详情页价格", res.Detail["xyz-01"].Price},
		{"详情页剩余价值", res.Detail["xyz-01"].Value},
		{"服务器列表", res.List["xyz-01"]},
	} {
		if strings.Contains(place.got, "¥") {
			t.Errorf("没有可用汇率的币种在%s = %q：不许出现人民币金额（这个数没换算过，套一个 ¥ 会让用户以为它换算过了）", place.where, place.got)
		}
	}
	t.Logf("没有可用汇率的币种 xyz-01：卡片 %q / 详情价格 %q / 详情剩余价值 %q / 列表 %q",
		res.Cards["xyz-01"].Cost, res.Detail["xyz-01"].Price, res.Detail["xyz-01"].Value, res.List["xyz-01"])

	for _, needle := range []string{"汇率数据", "内置兜底"} {
		if !strings.Contains(res.FXInfo, needle) {
			t.Errorf("设置页的服务器信息里缺少 %q：用户看不出这是兜底值", needle)
		}
	}
	t.Logf("取不到汇率时设置页 = %q", oneLine(res.FXInfo))
}

// ---------------------------------------------------------------- 总览区「剩余价值」

// overviewCaseNode 是总览用例里的一台机器：币种 + 价格（分）。
type overviewCaseNode struct {
	code  string
	cents int64
}

// overviewWant 是 Go 侧**独立**算出来的"总览区那一格该显示什么"。
//
// 独立 = 自己拿 /api/v1/nodes 里各节点的 remaining_value_cents 逐一折一遍
// （rate 是"1 CNY = ? 外币"，所以人民币 = 外币 ÷ rate），完全不碰被测的换算函数。
type overviewWant struct {
	// CNY 是人民币那一段（"" = 这一格不该出现 ¥）。
	CNY string
	// Unconverted 是换算不了的币种那几段（"182.51 XYZ"），按币种代码排序。
	Unconverted []string
	// Text 是整格拼出来的那一行。
	Text string
	// CNYIfSummedBlindly 是"把换算不了的也按 1:1 并进去"会算出来的那一段。
	// 它**绝不该**出现在页面上 —— 留着它是为了让断言能直说人话
	// （见 TestOverviewRemainingValueCNYInRealBrowser 里那一条）。
	CNYIfSummedBlindly string
}

func overviewWantOf(t *testing.T, br *browser, rate float64) overviewWant {
	t.Helper()
	status, body := br.do(http.MethodGet, "/api/v1/nodes", nil, false)
	if status != http.StatusOK {
		t.Fatalf("读取节点列表失败: %d", status)
	}
	raw, _ := body["nodes"].([]any)

	byCode := map[string]int64{}
	codes := []string{}
	for _, item := range raw {
		n, _ := item.(map[string]any)
		code, _ := n["currency"].(string)
		if _, seen := byCode[code]; !seen {
			codes = append(codes, code)
		}
		byCode[code] += intField(t, n, "remaining_value_cents")
	}

	out := overviewWant{Unconverted: []string{}}
	sumUnconverted := int64(0)
	unconverted := []string{}
	for _, code := range codes {
		cents := byCode[code]
		switch code {
		case "", "CNY":
			// 人民币口径（"没填币种"按人民币处理，见 internal/fx 的 ToCNY）。
			out.CNY = cnyText(cents + cnyCentsOf(out.CNY))
		case "USD":
			out.CNY = cnyText(int64(math.Round(float64(cents)/rate)) + cnyCentsOf(out.CNY))
		default:
			unconverted = append(unconverted, code)
			sumUnconverted += cents
		}
	}
	sort.Strings(unconverted)

	parts := []string{}
	if out.CNY != "" {
		parts = append(parts, out.CNY)
		out.CNYIfSummedBlindly = cnyText(cnyCentsOf(out.CNY) + sumUnconverted)
	}
	for _, code := range unconverted {
		seg := moneyText(byCode[code], code)
		out.Unconverted = append(out.Unconverted, seg)
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		out.Text = "—"
		return out
	}
	out.Text = strings.Join(parts, " + ")
	return out
}

// cnyCentsOf 把 cnyText 拼出来的「¥123.45」还原成 12345 分。
//
// 为什么绕这一圈：期望值要一段一段累加，而每一段都得**重新格式化**一次
// （与页面上的写法逐一对应）。直接存分会让"折了几次"变成另一个状态变量，
// 反而更容易写错。
func cnyCentsOf(text string) int64 {
	if text == "" {
		return 0
	}
	cents, err := strconv.ParseFloat(strings.TrimPrefix(text, "¥"), 64)
	if err != nil {
		panic("cnyCentsOf 只接受 cnyText 的输出，收到: " + text)
	}
	return int64(math.Round(cents * 100))
}

// 首页总览区「剩余价值」那一格，在**真浏览器**里的三种币种构成。
//
// 为什么必须真跑：金额对不对、是不是只剩一行、换算不了的有没有被偷偷按 1:1 并进
// 人民币合计里 —— 静态断言一条都看不出来，页面上出现的永远是一个"看着很正常"的数字。
//
// 三条规则（见 server/overview.go 的 remainingValueCNY）：
//
//	① 全都能换算 → 只有一行 ¥金额；
//	② 混入换算不了的币种 → "¥X + Y XYZ"，XYZ 那一笔**没有**被并进 ¥ 里；
//	③ 全都换算不了 → 连 ¥ 都不出现（更不能是 ¥0.00）。
func TestOverviewRemainingValueCNYInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	// 假汇率源只给 USD：XYZ / ABC 就是"这一份表里没有它"的币种。
	const rate = 0.2

	cases := []struct {
		name string
		// nodes 是这一轮的机器（币种 + 价格），都会带上 45 天的到期日。
		nodes []overviewCaseNode
		// wantHasCNY 表示这一格该不该出现 ¥。
		wantHasCNY bool
	}{
		{"全都能换算", []overviewCaseNode{{"CNY", 12000}, {"USD", 10000}, {"", 5000}}, true},
		{"混入一个换算不了的币种", []overviewCaseNode{{"CNY", 12000}, {"USD", 10000}, {"XYZ", 12345}}, true},
		{"全都换算不了", []overviewCaseNode{{"XYZ", 12345}, {"ABC", 700}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newFakeFXProvider(t, `{"base":"CNY","date":"2026-03-02","rates":{"USD":0.2}}`)
			logs := &captureHandler{}
			h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
				func(cfg *config.Server) { cfg.FXRateURL = provider.URL })
			br := newBrowser(t, "http://"+h.addr)
			setupAdmin(t, br, waitSetupCode(t, logs))
			waitFXReady(t, br, provider.URL)

			// 到期日固定成"45 天零 6 小时之后"：剩余价值不是 0，三种情况才都算得出来。
			expires := time.Now().Add(45*24*time.Hour + 6*time.Hour).Unix()
			names := map[string]int64{}
			for i, n := range tc.nodes {
				name := "ov-" + strconv.Itoa(i)
				names[name] = createPricedNode(t, br, name, n.cents, n.code, 1, expires)
			}

			want := overviewWantOf(t, br, rate)

			proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
				Scenario: "overview", User: "admin", Pass: "a-very-good-password",
				Nodes: names, Focus: "ov-0",
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
				t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
			}

			if !res.Overview.Found {
				t.Fatal("页面里找不到标题为「剩余价值」的那一格")
			}
			// 用户要的是"直接显示剩余价值多少￥"：**一行**。
			if len(res.Overview.Lines) != 1 {
				t.Fatalf("总览区「剩余价值」有 %d 行 %v，期望只有 1 行（%q）",
					len(res.Overview.Lines), res.Overview.Lines, want.Text)
			}
			got := res.Overview.Lines[0]
			if got != want.Text {
				t.Errorf("总览区「剩余价值」= %q，期望 %q（Go 侧拿原始金额独立折出来的）", got, want.Text)
			}
			t.Logf("总览区「剩余价值」= %q", got)

			// 规则①/③：该有 ¥ 才有；没有可换算的币种时连 ¥0.00 都不许出现
			// （那个 0 不是"一文不值"，是"这些币种都换不了"）。
			if hasYen := strings.Contains(got, "¥"); hasYen != tc.wantHasCNY {
				t.Errorf("总览区「剩余价值」= %q，出现 ¥ = %v，期望 %v", got, hasYen, tc.wantHasCNY)
			}
			if strings.Contains(got, "¥0.00") {
				t.Errorf("总览区「剩余价值」= %q：出现了 ¥0.00（这一轮夹具里没有值 0 的机器，"+
					"这个 0 只可能来自「没有可用汇率却硬写了一个人民币合计」）", got)
			}
			// 规则②：换算不了的那几段必须**原样**跟在后面。
			for _, seg := range want.Unconverted {
				if !strings.Contains(got, seg) {
					t.Errorf("总览区「剩余价值」= %q，缺少 %q（换算不了的币种要原样列出来，不能消失）", got, seg)
				}
			}
			// 最要命的那种错法：把换算不了的按 1:1 并进人民币合计 —— 那是凭空编一个
			// 汇率，而用户会拿这个数当真实资产。夹具里的 XYZ 是 123.45 元，
			// 并进去之后人民币那一段会变成另一个数（见 CNYIfSummedBlindly）。
			if want.CNYIfSummedBlindly != want.CNY && strings.Contains(got, want.CNYIfSummedBlindly) {
				t.Errorf("总览区「剩余价值」= %q：人民币那一段是「把换算不了的按 1:1 并进去」算出来的 %q"+
					"（等于凭空编了一个汇率）", got, want.CNYIfSummedBlindly)
			}

			// 回归：四条常用路由各自 errs=0、停在预期的视图上。
			assertRoutes(t, res.Routes, map[string]string{
				"#/": "view-home",
				"#/n/" + strconv.FormatInt(names["ov-0"], 10): "view-detail",
				"#/settings/nodes": "view-settings:nodes",
				"#/settings/alert": "view-settings:alert",
			})
		})
	}
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

	Currency    fxSelect     `json:"currency"`
	NewCurrency fxSelect     `json:"newCurrency"`
	Created     fxCreate     `json:"created"`
	Edit        fxEdit       `json:"edit"`
	Legacy      fxLegacy     `json:"legacy"`
	Empty       fxLegacy     `json:"empty"`
	Overview    fxOverview   `json:"overview"`
	FXInfo      string       `json:"fxInfo"`
	Narrow      fxNarrow     `json:"narrow"`
	Routes      []groupRoute `json:"routes"`
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
	// SelectedIndex 是当前选中项在 options 里的下标，-1 表示**一个都没选中**。
	//
	// 这个字段专为"币种为空"准备：下拉里没有空选项之后，给 select.value 赋一个
	// 匹配不上的值会让 selectedIndex 变成 -1，控件显示成一片空白 ——
	// 光看 Value 是空串分辨不出"选中了「（未设置）」"与"什么都没选中"。
	SelectedIndex int `json:"selectedIndex"`
	// SelectedText 是选中那一项的文案（「CNY 人民币」/「（未设置）」…）。
	SelectedText string `json:"selectedText"`
}

// fxCreate 是两次"真的把节点建出来"的请求体观测值。
//
// 为什么不能只看下拉选了什么：下拉上选中 CNY ≠ 库里存的是 CNY。请求体里到底发了
// 什么币种，只有把那次 POST 的 body 抓下来才知道 —— 而"用户不碰下拉就会存 CNY"
// 正是这一条改动要保证的事。
type fxCreate struct {
	// NoPriceCurrency 是"**不填价格**直接创建"那一次的请求体 currency。
	// 必须是空串：服务端规定"没有计费周期时，价格与货币都必须留空"
	// （见 store.Node.Validate），而下拉现在默认选中人民币。
	NoPriceCurrency string `json:"noPriceCurrency"`
	// PricedCurrency 是"填了价格 + 1 个月"那一次的请求体 currency。必须是 CNY。
	PricedCurrency string `json:"pricedCurrency"`
}

type fxEdit struct {
	// SavedCurrency 是"选 EUR 保存"那一次请求体里的 currency。
	SavedCurrency string `json:"savedCurrency"`
	// ReopenedValue 是重新打开编辑框时下拉的值。
	ReopenedValue string `json:"reopenedValue"`
}

// fxLegacy 是"下拉装不下的值"那两次编辑的观测值。老币种（XYZ）与**空币种**共用
// 这一个结构：它们在界面上的处理本来就是同一条路（临时补一个选项让它显示出来）。
type fxLegacy struct {
	Value      string `json:"value"`
	OptionText string `json:"optionText"`
	// SelectedIndex / SelectedText 见 fxSelect 的同名字段：空币种那一路全靠它们
	// 分辨"选中了「（未设置）」"与"一个都没选中（一片空白）"。
	SelectedIndex int    `json:"selectedIndex"`
	SelectedText  string `json:"selectedText"`
	SavedCurrency string `json:"savedCurrency"`
}

// fxOverview 是首页总览区「剩余价值」那一格的渲染结果。
//
// 记的是**逐行**的内容：用户要的是"这一格只显示一行人民币"，所以"行数"本身就是
// 断言的一部分（改动之前它是按币种分行的，有几种币就几行）。
type fxOverview struct {
	// Found 表示页面里找到了标题为「剩余价值」的那一格。
	Found bool     `json:"found"`
	Lines []string `json:"lines"`
	Text  string   `json:"text"`
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

// moneyText 与 app.js 的 fmtMoney 同一套格式：人民币（以及"没填币种"——服务端
// 把它当人民币处理）只写「¥金额」，外币写"符号 + 金额 + 代码"（认不得符号的写
// "金额 代码"）。前端改了格式这里会一起红：这正是"页面上的字必须与 Go 算出来的
// 一模一样"这条验收标准本身。
func moneyText(cents int64, code string) string {
	symbols := map[string]string{"USD": "$", "EUR": "€", "JPY": "¥", "GBP": "£"}
	amount := strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
	if code == "" || code == "CNY" {
		return "¥" + amount
	}
	if sym, ok := symbols[code]; ok {
		return sym + amount + " " + code
	}
	return amount + " " + code
}

// cnyText 是"换算成人民币之后"的格式：只有 ¥ 与两位小数，后面**不跟**币种代码
// （与 app.js 的 nodeMoney/fmtMoney 一致：¥ 本身已经说明是人民币）。
func cnyText(cents int64) string {
	return "¥" + strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
}

// assertOnlyCNY 钉住"这一处只显示人民币"：恰好一个 ¥、没有原币种代码、
// 也没有双币格式留下来的连接符。
//
// 为什么连" · ¥"一起挡：双币格式是"$100.00 USD · ¥500.00"，只挡 $ 与 USD 的话，
// 哪天原币种那一段换个写法（比如币种代码丢了）双币还是会从眼前溜过去。
// 至于"+45 天"前面那个 · —— 它是**别的字段**的分隔符，这一轮明确要保留，
// 所以挡的是紧跟在 ¥ 前面的那一个（" · ¥"），不是所有的 ·。
func assertOnlyCNY(t *testing.T, where, text string) {
	t.Helper()
	if strings.Count(text, "¥") != 1 {
		t.Errorf("%s = %q，期望恰好一个 ¥（价格一律只显示人民币，不该有第二段金额）", where, text)
	}
	for _, bad := range []string{"$", "USD", "CNY", " · ¥", "¥—"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s = %q，出现了 %q：价格要一律只显示人民币（不留原币种、不留双币连接符）", where, text, bad)
		}
	}
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

// nodeByName 按名字找节点（浏览器里新建出来的那些只知道名字，拿不到 id）。
func nodeByName(t *testing.T, nodes []map[string]any, name string) map[string]any {
	t.Helper()
	for _, node := range nodes {
		if got, _ := node["name"].(string); got == name {
			return node
		}
	}
	t.Fatalf("节点列表里没有名叫 %q 的节点（浏览器里那次创建没成功？）", name)
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

// TestFXScreenshots 产出人工核对用的几张 PNG：
//
//	fx-home        首页卡片 —— 「费用」行只剩人民币（不再有 "$100.00 USD · "）
//	fx-detail      USD 节点的详情页 —— 顶部三格（价格/月均/剩余价值）只剩人民币
//	fx-create      **新增**节点对话框 —— 货币下拉里没有空选项，默认选中人民币
//	fx-empty       **空币种**老节点的编辑框 —— 临时补出来的「（未设置）」被选中
//	fx-dialog      编辑 USD 节点的对话框（货币是下拉，选中 USD）
//	fx-legacy      老币种节点（XYZ）的编辑框 —— 下拉里临时补出来的「XYZ（库里的原值）」
//	fx-settings    设置页「服务器信息」里的汇率元信息（哪天的、来源、是否兜底）
//	fx-home-narrow 窄屏手机的首页卡片 —— 费用那一行会不会把卡片撑破
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
		{"fx-home", "home", "usd-01", 1500, 1000, 1},
		{"fx-detail", "detail", "usd-01", 1500, 1000, 1},
		{"fx-create", "create", "usd-01", 1500, 1000, 1},
		{"fx-empty", "empty", "usd-01", 1500, 1000, 1},
		{"fx-dialog", "dialog", "usd-01", 1500, 1000, 1},
		{"fx-legacy", "legacy", "usd-01", 1500, 1000, 1},
		{"fx-settings", "settings", "usd-01", 1500, 1000, 1},
		// 窄屏再看一眼首页卡片：费用那一行短了，但"会不会被截 / 卡片会不会撑破"
		// 只有看图才知道。2 倍缩放 + 760 的窗口 ⇒ CSS 视口 380px
		// （Windows 上直接传 380 拿不到）。
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

// startFXShotFixture 起一个装了"美元 / 人民币 / 老币种（XYZ）/ 空币种"四台机器的服务端，
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
		"usd-01":   createPricedNode(t, br, "usd-01", 10000, "USD", 1, expires),
		"cny-01":   createPricedNode(t, br, "cny-01", 3100, "CNY", 1, expires),
		"old-01":   createPricedNode(t, br, "old-01", 12345, "XYZ", 1, expires),
		"empty-01": createPricedNode(t, br, "empty-01", 5000, "", 1, expires),
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
    currency: {}, newCurrency: {}, created: {}, edit: {}, legacy: {}, empty: {},
    overview: {}, fxInfo: '', narrow: { cards: {} }, routes: []
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
    // 用 >=（而不是 ===）：这个用例跑完时库里已经多了两台**刚刚建出来**的机器，
    // 后面那次窄屏复跑看到的就是它们全都在。这里要的是"在意的这几张卡片都画出来了"。
    return waitFor('首页卡片都画出来', function () {
      return shown('view-home') && node('grid') && node('grid').children.length >= NAMES.length;
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

  // currencyInfo 读货币下拉当前的样子。除了选项列表与 value，还要记下
  // **选中的是第几项、那一项写的是什么** —— 币种为空时 value 是空串，
  // 光看它分不清"选中了「（未设置）」"与"一个都没选中（selectedIndex = -1）"，
  // 而后者在下拉上显示成一片空白，正是要去挡的那种坏法。
  function currencyInfo() {
    var sel = node('node-currency');
    var options = [];
    for (var i = 0; i < sel.options.length; i++) options.push(sel.options[i].value);
    var legacy = sel.querySelector('option[data-legacy]');
    return {
      tag: sel.tagName,
      options: options,
      value: sel.value,
      selectedIndex: sel.selectedIndex,
      selectedText: sel.selectedIndex >= 0 ? sel.options[sel.selectedIndex].textContent : '',
      optionText: legacy ? legacy.textContent : ''
    };
  }

  // openCreate 从**首页**点「新增节点」把新建对话框打开。
  //
  // 「新增节点」按钮只在首页可见（见 app.js 的 setView），所以先回首页。
  function openCreate() {
    window.location.hash = '#/';
    return waitFor('回到首页', function () { return shown('view-home'); }, 20000)
      .then(function () { return sleep(300); })
      .then(function () {
        node('btn-add').click();
        return waitFor('新增节点对话框打开', function () {
          return node('dlg-node') && node('dlg-node').open;
        }, 15000);
      })
      .then(function () { return sleep(200); });
  }

  // closeTokenDialog 关掉"创建成功"之后弹出的 Token 对话框。
  //
  // 它是模态的（showModal），不关掉后面什么都点不动；而且 Token 只显示一次，
  // 这个用例不需要它。
  function closeTokenDialog() {
    if (node('dlg-token') && node('dlg-token').open) {
      node('token-close').click();
      return sleep(200);
    }
    return sleep(50);
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
      // 汇率元信息在「服务器信息」栏的第二个分组里（#fx-info）。
      // 这里把两个容器拼起来读：用例关心的是"这一栏里能不能看到汇率的出处"，
      // 而不是它落在哪一个容器里（改版把 10 项参数与 4 项汇率拆成了两组，
      // 前者是 5 列网格、后者是长值行）。
      R.fxInfo = textOf('server-info') + textOf('fx-info');
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
  // 费用那一行在窄屏上最容易出问题：卡片被撑破（整页横向滚动），
  // 或者金额被截得读不出来 —— 只有真跑一遍量一下才知道。
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

  // ---- 场景四：总览区「剩余价值」那一格 -----------------------------------
  //
  // 按**标题**找那一格（而不是按下标）：以后中间插一格也不会读错地方。
  function overviewValueBox() {
    var overview = node('overview');
    if (!overview) return null;
    var box = null;
    Array.prototype.forEach.call(overview.children, function (cell) {
      var label = cell.querySelector('.ov-label');
      if (label && label.textContent === '剩余价值') box = cell.querySelector('.ov-value');
    });
    return box;
  }

  function readOverviewValue() {
    var box = overviewValueBox();
    if (!box) { R.overview = { found: false, lines: [], text: '' }; return; }
    var lines = [];
    Array.prototype.forEach.call(box.querySelectorAll('.ov-line'), function (span) {
      lines.push(span.textContent);
    });
    R.overview = { found: true, lines: lines, text: box.textContent };
  }

  // 总览是分钟级轮询（OVERVIEW_POLL_MS），进首页之后要等它回来才有内容。
  function waitOverviewValue() {
    return waitFor('总览区剩余价值已渲染', function () {
      var box = overviewValueBox();
      return node('overview') && !node('overview').hidden && !!box && box.children.length > 0;
    }, 20000).then(function () { return sleep(200); });
  }

  function overviewPass() {
    return goHome().then(waitOverviewValue).then(function () { readOverviewValue(); });
  }

  // ---- 场景一：完整流程 ---------------------------------------------------
  //
  // 顺序是有讲究的：编辑四台机器（USD / CNY… 其中 USD 会被改成 EUR、老币种与
  // 空币种只打开看一眼就保存）放在前面，**新建**对话框放在最后 —— 那时下拉里
  // 还残留着上一步为空币种临时补出来的选项，新建时必须自己回到人民币。
  function fullPass() {
    return goHome()
      .then(function () { readCards(); return goDetail('usd-01'); })
      .then(function () { readDetail('usd-01'); return goDetail('cny-01'); })
      .then(function () { readDetail('cny-01'); return goDetail('old-01'); })
      .then(function () { readDetail('old-01'); return goDetail('empty-01'); })
      .then(function () { readDetail('empty-01'); return goSettings('nodes', true); })
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
        R.legacy.selectedIndex = info.selectedIndex;
        R.legacy.selectedText = info.selectedText;
        // 什么都不改，直接保存：库里的币种必须原样回去。
        return submitDialog();
      })
      .then(function (body) {
        R.legacy.savedCurrency = body ? body.currency : '';
        return goSettings('nodes', true);
      })
      .then(function () { return openEdit('empty-01'); })
      .then(function () {
        var info = currencyInfo();
        R.empty.value = info.value;
        R.empty.optionText = info.optionText;
        R.empty.selectedIndex = info.selectedIndex;
        R.empty.selectedText = info.selectedText;
        // 同样什么都不改，直接保存：库里存的空串必须原样回去（既不能被填成
        // 人民币，也不能因为"没有匹配的选项"而发出去一个别的值）。
        return submitDialog();
      })
      .then(function (body) {
        R.empty.savedCurrency = body ? body.currency : '';
        return openCreate();
      })
      .then(function () {
        R.newCurrency = currencyInfo();
        // 不填价格直接创建。服务端规定"没有计费周期时，价格与货币都必须留空"，
        // 而下拉现在**默认选中人民币** —— 请求体里必须是空串，而且**必须建得出来**。
        // 这是"默认人民币"最容易踩坏的一处：不做这个判断的话，新建一台不填价格的
        // 机器会被服务端拒掉（用户什么都没做错，却建不出来）。
        node('node-name').value = 'noprice-01';
        return submitDialog();
      })
      .then(function (body) {
        R.created.noPriceCurrency = body ? body.currency : '（没抓到请求体）';
        return closeTokenDialog();
      })
      .then(function () { return openCreate(); })
      .then(function () {
        // 填了价格与周期：下拉里默认选中的人民币这回该真的发出去了。
        node('node-name').value = 'priced-01';
        node('node-price').value = '100';
        node('node-billing').value = '1';
        return submitDialog();
      })
      .then(function (body) {
        R.created.pricedCurrency = body ? body.currency : '（没抓到请求体）';
        return closeTokenDialog();
      });
  }

  // ---- 场景二：取不到汇率（内置兜底表）----------------------------------
  //
  // 两台机器都要读全三个地方：USD（兜底表里有它的汇率）应当照常是人民币金额，
  // XYZ（兜底表里没有它）应当**退回原币种金额** —— 后半句才是这一段的主角。
  function defaultPass() {
    return goHome().then(function () {
      readCards();
      return NAMES.reduce(function (chain, name) {
        return chain.then(function () { return goDetail(name); })
          .then(function () { readDetail(name); });
      }, Promise.resolve());
    }).then(function () { return goSettings('nodes', true); })
      .then(function () { readList(); return readFXInfo(); });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:fx'; });
  }

  // 截图模式（人工核对用）：把界面开到指定状态就停住，不回传结果。
  //   create   → **新增**节点对话框（看下拉里没有空选项、默认选中人民币）
  //   dialog   → 打开 usd-01 的编辑框（看货币下拉）
  //   legacy   → 打开 old-01 的编辑框（看"库里的原值"那个临时选项）
  //   detail   → usd-01 的详情页（看顶部三格只剩人民币）
  //   settings → 设置页的服务器信息（看汇率元信息）
  //   home     → 首页卡片（看「费用」行只剩人民币）
  function shot() {
    var mode = CFG.shot;
    var focus = CFG.focus || NAMES[0];
    return login().then(function () {
      if (mode === 'settings') return readFXInfo();
      if (mode === 'home') return goHome();
      if (mode === 'detail') return goDetail(focus);
      if (mode === 'create') {
        // 「新增节点」按钮只在首页可见（见 app.js 的 setView），所以先回首页。
        return goHome().then(function () { node('btn-add').click(); })
          .then(function () {
            return waitFor('新增节点对话框打开', function () {
              return node('dlg-node') && node('dlg-node').open;
            }, 15000);
          });
      }
      if (mode === 'legacy') {
        return goSettings('nodes', true).then(function () { return openEdit('old-01'); });
      }
      if (mode === 'empty') {
        return goSettings('nodes', true).then(function () { return openEdit('empty-01'); });
      }
      return goSettings('nodes', true).then(function () { return openEdit(focus); });
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
        if (scenario === 'overview') return overviewPass();
        return scenario === 'fxdefault' ? defaultPass() : fullPass();
      })
      .then(routePass)
      .catch(function (err) { R.fatal = String(err && err.message ? err.message : err); })
      .then(finish, finish);
  });
})();`
