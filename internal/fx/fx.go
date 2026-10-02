// Package fx 负责"人民币 → 外币"的汇率快照：取、解析、落库编码、换算。
//
// 方向约定（全项目只有这一套）：数据源都以 **CNY 为基准**，返回的 rates 是
// "**1 人民币等于多少该货币**"（例如 USD: 0.14799）。所以把外币金额换成人民币是
// **除法**（见 ToCNY），不是乘法。
//
// 这个包不做网络调度，也不认识服务端：它只提供"一次快照"的模型与纯函数，
// 什么时候取、失败了怎么办由 internal/server 的定时任务决定（见 server/fx.go）。
package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// BaseCurrency 是汇率表的基准币种，也是界面上"人民币口径"的目标币种。
const BaseCurrency = "CNY"

// Providers 是内置的数据源，**按顺序试，第一个成功的就用**。
//
// 这两个与 Komari Emerald 主题用的是同一对：前者是欧洲央行体系的聚合
// （每天更新一次），后者是 exchangerate-api 的开放端点。两个都以 CNY 为基准
// 返回 {rates: {...}}，语义一致，因此解析逻辑只有一份。
//
// 它们的**响应字段名不同**（date vs base_code/time_last_update_unix），
// 解析时两种都认（见 Parse）。
var Providers = []string{
	"https://api.frankfurter.app/latest?from=CNY",
	"https://open.er-api.com/v6/latest/CNY",
}

// defaultFetchTimeout 是**单个 URL** 的超时。
//
// 一个源不吭声时不能把后台定时任务一起拖住：两个源最坏 10 秒，
// 而汇率晚 10 秒生效没有任何影响。
const defaultFetchTimeout = 5 * time.Second

// maxBodyBytes 是单个响应的读取上限。汇率的 JSON 只有几 KB，
// 1 MiB 足够宽松，同时挡住"对端一直吐数据"把内存吃光。
const maxBodyBytes = 1 << 20

// 汇率的合理区间。只用来挡住垃圾数据（0、负数、NaN、1e300 这类），
// **不是**经济判断：现实中 1 人民币能换到的外币数量都在这个范围内
// （最极端的几种里，越南盾约 3.5e3、伊朗里亚尔约 6e5）。
const (
	minUsableRate = 1e-4
	maxUsableRate = 1e8
)

// Snapshot 是一次汇率取值的全部内容。
//
// 它同时是**落库的形状**（settings 表里那个键存的就是它的 JSON）与内存里
// 当前生效的那一份，因此字段名带 json tag 且要稳定 —— 改字段名等于读不回旧值。
type Snapshot struct {
	// Base 恒为 CNY（写出来是为了让人看 JSON 时不必猜方向）。
	Base string `json:"base"`
	// Date 是汇率对应的日期（数据源给的，不是"我们取的那天"）。
	// 周末与节假日数据源不更新，这个日期会比 FetchedAt 早几天，这是正常的。
	Date string `json:"date"`
	// Source 是取到这一份的那个 URL；内置兜底表为空串。
	Source string `json:"source"`
	// FetchedAt 是我们取到它的时刻（epoch 秒）。
	FetchedAt int64 `json:"fetched_at"`
	// Rates 是"1 CNY = ? 该货币"，键一律大写。
	Rates map[string]float64 `json:"rates"`
	// IsDefault 表示这一份是**内置兜底表**（从没成功取到过实时汇率）。
	//
	// 它不落库：落库的一定是取回来的那一份（或者根本没落库）。标了
	// json:"-"，避免以后有人把内存里的兜底快照直接写进设置表，
	// 下次启动就再也分不清"这是取的还是兜底的"。
	IsDefault bool `json:"-"`
}

// Default 返回**内置的兜底汇率表**。
//
// 这不是实时值，也永远不会自动更新：它只在"从没成功取到过汇率"时使用，
// 目的只有一个 —— **绝不允许因为取不到汇率就让价格显示成 0、空白或报错**。
// 里面的数字是量级正确的静态估计（写死在代码里），偏差可能很大，
// 界面上必须显示"内置兜底"（见 /api/v1/settings 的 fx.is_default）。
//
// 币种只覆盖常见的十几种：兜底表命中不了的外币会退回原值显示（见 ToCNY），
// 那比拿一个凭空的数字去冒充换算结果诚实得多。
func Default() Snapshot {
	return Snapshot{
		Base: BaseCurrency,
		Date: "",
		// Source 留空：界面上"来源"那一栏据此显示"内置兜底表"。
		Source:    "",
		FetchedAt: 0,
		IsDefault: true,
		Rates: map[string]float64{
			"CNY": 1,
			"USD": 0.14,
			"EUR": 0.13,
			"GBP": 0.108,
			"JPY": 20.5,
			"HKD": 1.09,
			"SGD": 0.187,
			"AUD": 0.213,
			"CAD": 0.194,
			"KRW": 190,
			"TWD": 4.45,
			"MYR": 0.59,
			"THB": 4.7,
			"CHF": 0.113,
			"VND": 3550,
		},
	}
}

// providerResponse 是两个数据源的响应并集（字段名不同，认得的就填）。
type providerResponse struct {
	// frankfurter: {"amount":1.0,"base":"CNY","date":"2026-01-02","rates":{...}}
	Base string `json:"base"`
	Date string `json:"date"`
	// open.er-api.com: {"result":"success","base_code":"CNY",
	//   "time_last_update_unix":1767312000,"rates":{...}}
	BaseCode           string             `json:"base_code"`
	TimeLastUpdateUnix int64              `json:"time_last_update_unix"`
	Rates              map[string]float64 `json:"rates"`
}

// Parse 把一次 HTTP 响应解析成快照。source 是这次请求的 URL，now 用于兜底日期。
//
// 校验很严：**宁可让这一级失败降级到旧值**，也不能把一份方向不对的汇率
// 悄悄用上去。外币金额除以错误的汇率不会报错，只会让所有价格静默地错一大截。
func Parse(source string, body []byte, now time.Time) (Snapshot, error) {
	var resp providerResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Snapshot{}, fmt.Errorf("解析汇率响应失败: %w", err)
	}
	if len(resp.Rates) == 0 {
		// 垃圾 JSON、限流页、{"result":"error"} 都会落到这里。
		return Snapshot{}, errors.New("汇率响应里没有 rates")
	}
	// 数据源可能忽略请求里的基准币种（或走了缓存返回别的基准）。
	// 方向一旦不是 CNY，换算就是错的 —— 直接拒掉，让上一级降级。
	if base := firstNonEmpty(resp.Base, resp.BaseCode); base != "" && !strings.EqualFold(base, BaseCurrency) {
		return Snapshot{}, fmt.Errorf("汇率响应的基准币种是 %q，期望 %s", base, BaseCurrency)
	}

	rates := make(map[string]float64, len(resp.Rates))
	for code, rate := range resp.Rates {
		key := normalizeCode(code)
		if key == "" || !usableRate(rate) {
			continue
		}
		rates[key] = rate
	}
	if len(rates) == 0 {
		return Snapshot{}, errors.New("汇率响应里没有一个可用的汇率")
	}

	date := strings.TrimSpace(resp.Date)
	if _, err := time.Parse("2006-01-02", date); err != nil {
		date = ""
	}
	if date == "" && resp.TimeLastUpdateUnix > 0 {
		date = time.Unix(resp.TimeLastUpdateUnix, 0).UTC().Format("2006-01-02")
	}
	if date == "" {
		// 两个源都没给日期：用"取到的这一天"。宁可用我们的日期，
		// 也不留一个空串让界面显示"哪天的：—"。
		date = now.Format("2006-01-02")
	}

	return Snapshot{
		Base:      BaseCurrency,
		Date:      date,
		Source:    source,
		FetchedAt: now.Unix(),
		Rates:     rates,
		IsDefault: false,
	}, nil
}

// Decode 把设置表里存的 JSON 读回快照。
//
// 读路径**不许因为一行坏数据报错**（与 store.decodeTags 同一条原则）：
// 设置表可能被手工改过、也可能是更老的版本写的。解不出来就返回 ok=false，
// 调用方继续用内存里的兜底表。
func Decode(raw string) (Snapshot, bool) {
	if strings.TrimSpace(raw) == "" {
		return Snapshot{}, false
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return Snapshot{}, false
	}
	if len(snap.Rates) == 0 {
		return Snapshot{}, false
	}
	// 落库的那一份永远是"取回来的"，IsDefault 由内存状态决定，不读库里的值。
	snap.IsDefault = false
	snap.Base = BaseCurrency
	return snap, true
}

// Encode 编码成落库的 JSON 文本。
func (s Snapshot) Encode() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("编码汇率快照失败: %w", err)
	}
	return string(data), nil
}

// Rate 返回"1 CNY = ? code"，第二个值表示有没有可用汇率。
func (s Snapshot) Rate(code string) (float64, bool) {
	key := normalizeCode(code)
	if key == "" {
		return 0, false
	}
	rate, ok := s.Rates[key]
	if !ok || !usableRate(rate) {
		return 0, false
	}
	return rate, true
}

// Convertible 报告某个币种能不能真的换算成人民币。
//
// 空币种与 CNY 返回 false：它们本来就是人民币口径（见 ToCNY），
// 界面据此**不**重复显示两遍同一个金额。
func (s Snapshot) Convertible(code string) bool {
	key := normalizeCode(code)
	if key == "" || key == BaseCurrency {
		return false
	}
	_, ok := s.Rate(key)
	return ok
}

// ToCNY 把"某种货币的金额（分）"换算成人民币（分）。
//
// 两条边界，都是刻意的选择：
//
//  1. **币种为空或 CNY** → 原值返回。人民币计价不需要换算，返回 0 会被读成
//     "这台机器免费"。
//  2. **没有可用汇率**（未知币种、或这一份汇率表里没有这个币种、或汇率是 0/
//     负数/离谱值）→ 也返回**原值**。理由：
//     - 绝不允许出现 0：0 在界面上是"免费/一文不值"，而不是"换算不了"；
//     - 不猜汇率：按 1:1 退回原值也不会凭空造出一个假的换算结果；
//     - 不认识就当人民币处理（1:1）与"退回原值"是同一件事，不需要第二条规则；
//     - 原始币种字段始终照旧下发，界面在无法换算时只显示原币种一处
//     （靠 Convertible 判断），所以用户看到的不会是一个冒充过的数字。
func (s Snapshot) ToCNY(cents int64, code string) int64 {
	if cents == 0 {
		return 0
	}
	if cny, ok := s.TryToCNY(cents, code); ok {
		return cny
	}
	// 没有可用汇率 → 退回原值（理由见上面那一整段）。
	return cents
}

// TryToCNY 与 ToCNY 只差一件事：**换算不了时它不退原值**，而是明确地说
// "这个币种我换不了"。
//
// 返回值：
//   - ok=true  → 第一个值是折成人民币之后的分值：币种为空或 CNY 时就是原值
//     （它们本来就是人民币口径），有可用汇率时是除法算出来的值。
//   - ok=false → 这个币种**没有可用汇率**，第一个值恒为 0。调用方**不许**把它
//     当成金额用 —— 尤其不许按 1:1 加进任何人民币合计里。
//
// 为什么除了 ToCNY 还要有它：**单个显示**与**求和**是两件事。
// 单台节点的价格走 ToCNY（那个场景下"退回原值 + 界面照原币种显示"是对的，
// 见 app.js 的 nodeMoney）；而"把好几台机器的剩余价值折成一个人民币总额"这种
// 求和，一旦把换算不了的按 1:1 并进去，得到的就不是"没有汇率"而是**凭空编出来
// 的一个汇率** —— 用户会拿这个数当真实资产去决策（见 server/overview.go 的
// remainingValueCNY）。
func (s Snapshot) TryToCNY(cents int64, code string) (int64, bool) {
	key := normalizeCode(code)
	if key == "" || key == BaseCurrency {
		return cents, true
	}
	rate, ok := s.Rate(key)
	if !ok {
		return 0, false
	}
	// 除法：rate 是"1 CNY = ? 外币"，所以"外币 ÷ rate = 人民币"。
	// 四舍五入到分：与 ToCNY 必须是**同一个数**（两处算出来不一样就是 bug）。
	return int64(math.Round(float64(cents) / rate)), true
}

// Client 按顺序试 Providers，返回第一个成功的快照。
type Client struct {
	// URLs 是要试的数据源，按顺序。空表示用内置的 Providers。
	URLs []string
	// HTTP 为空时用带 defaultFetchTimeout 的默认客户端。
	HTTP *http.Client
	// Now 为空时用 time.Now（测试可以注入固定时刻）。
	Now func() time.Time
}

// Fetch 依次请求每个数据源，返回第一个成功解析的快照。
//
// 全部失败时返回 errors.Join 起来的错误：调用方**只记一条日志**，
// 不逐条刷屏（见 server/fx.go 的 refreshFX）。
func (c Client) Fetch(ctx context.Context) (Snapshot, error) {
	urls := c.URLs
	if len(urls) == 0 {
		urls = Providers
	}
	client := c.HTTP
	if client == nil {
		// Timeout 是**每个请求**的超时（不是整个客户端的），
		// 因此每个 URL 各自最多 defaultFetchTimeout。
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}

	var errs []error
	for _, url := range urls {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		snap, err := c.fetchOne(ctx, client, url, now())
		if err == nil {
			return snap, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", url, err))
	}
	if len(errs) == 0 {
		return Snapshot{}, errors.New("没有配置任何汇率数据源")
	}
	return Snapshot{}, errors.Join(errs...)
}

func (c Client) fetchOne(ctx context.Context, client *http.Client, url string, now time.Time) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Snapshot{}, err
	}
	// 不带 Cookie、不带 Referer：这是一次纯粹的服务端出网请求。
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Snapshot{}, err
	}
	return Parse(url, body, now)
}

// normalizeCode 归一化币种代码：去空白、转大写。库里/表单里的值都可能是小写。
func normalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// usableRate 判断一个汇率能不能用：挡掉 0、负数、NaN、Inf 与离谱的量级。
func usableRate(rate float64) bool {
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		return false
	}
	return rate >= minUsableRate && rate <= maxUsableRate
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
