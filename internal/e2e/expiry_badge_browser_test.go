package e2e

// 到期提醒徽章（方案 D）的浏览器用例：只能"剩余 ≤7 天"的机器，才会在名称那一行的
// **最右边**多出一个小徽章（见 web/style.css 的 .card-expiry 与 web/app.js 的
// renderExpiryBadge）。
//
// 已过期那一档：上一轮服务端在过期时把剩余天数夹成 0，徽章只能写「已过期」；
// 现在 DTO 多了一个 expired_days（见 internal/server/dto.go），徽章要写「已过期 N 天」
// —— 这一档以前在真数据上**验不到**，这次能验了，所以下面的现场里有两台过期的机器。
//
// 做法与仓库里其它浏览器用例完全一致：真服务端（Server.Run 完整路径）+ 反代注入
// 自检脚本 + 页面把观测值 POST 回 mock + --headless=new，**不用**
// --dump-dom + --virtual-time-budget（首页挂着 SSE 长连接，虚拟时间会一直暂停）。
//
// 为什么这几条非要在真浏览器里量（静态断言证明不了的部分）：
//   - "徽章贴在名称那一行的最右边"是**几何**结论：.card-head 是 space-between，
//     但只差一个插入位置就会变成"紧贴名字右边、离卡片右边缘还有一百多像素"
//     （.card-name-row 的宽度就是它的内容宽度，内部没有剩余空间）；
//   - 三档颜色算出来到底是多少（"淡琥珀"是 color-mix 现算的）只有计算样式说得清；
//   - "长名字只截断名字、徽章一个像素都不缩"是布局结论；
//   - "不会每秒重建 DOM"是**时间**上的结论：要盯着 MutationObserver 看几秒，
//     而且必须先证明这几秒里卡片真的被重画过（否则是一条永远绿的断言）。
//
// 现场是九台机器，一台一档（见 ebNodes）：已过期 3 天 / 已过期 400 天 / 不足 1 天 /
// 剩 1 天 / 剩 3 天 / 剩 7 天 / 剩 8 天（无徽章）/ 没填到期日（无徽章）/ 31 字符中文长名。
// 其中"剩 7 天"那台在整条用例里**每秒都被重画**（Go 侧每 250ms 灌一次新上报），
// 所以它既是"不重建 DOM"的观察对象，也是"续期后徽章消失"的观察对象。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/protocol"
)

// ebLongName 是设计稿里那条 31 字符的中文长名（名称输入上限 64 字符，它离上限还有余量）。
const ebLongName = "洛杉矶-DC02-搬瓦工-CN2GIA-年付-主力机-生产环境"

const (
	ebGone     = "eb-gone"      // 已过期 3 天（这一轮起徽章上要写出天数）
	ebGoneLong = "eb-gone-long" // 已过期 400 天：证明天数来自数据，不是写死的 3
	ebUnder1   = "eb-under1"    // 还有 6 小时（整天数 = 0，但**没**过期）
	ebOne      = "eb-1d"        // 剩 1 天
	ebThree    = "eb-3d"        // 剩 3 天
	ebSeven    = "eb-7d"        // 剩 7 天（这一台每秒都被重画）
	ebEight    = "eb-8d"        // 剩 8 天：按方案 D 不该有徽章
	ebNone     = "eb-none"      // 没填到期日：不该有徽章
)

// ebNode 是现场里的一台机器。
type ebNode struct {
	name string
	// expiresIn 是相对"现在"的到期时刻（秒）。0 = 没填到期日。
	//
	// 为什么用 +12 小时这种"带半天"的值：remaining_days 是**整天数**
	// （internal/server/dto.go 的 applyPricing 用整秒差除以 86400），
	// 取整天数 N 要让 secs 落在 [N 天, N 天 + 1 天) 里，加半天最稳
	// ——用例跑多久都不会滑到隔壁档去。
	expiresIn int64
	// wantText / wantTier 是**人写死的验收标准**（不是把前端的规则再算一遍）：
	// 空 wantTier 表示"这一台 DOM 里不该有徽章"。
	wantText string
	wantTier string
	// live 为 true 的机器由 Go 侧每 250ms 灌一帧上报：卡片因此每秒都被重画，
	// "档位没变就不动 DOM"与"续期后徽章被摘掉"才有判别力。
	live bool
}

// ebNodes 是八台机器的完整现场（期望值逐条写死，见上面 wantText/wantTier）。
var ebNodes = []ebNode{
	// 这一台是这一轮新改动的现场：过期 3 天 ⇒ expired=true 而 remaining_days=0，
	// 「3」只能从服务端新给的 expired_days 拿（拿不到的话这里会红成「已过期」）。
	{name: ebGone, expiresIn: -3 * 86400, wantText: "已过期 3 天", wantTier: "bad"},
	// 同一档的第二个样本：过期 400 天。它同时钉住两件事 —— 天数是**多位数**时
	// 文案照样对；以及那个数字真的来自数据（实现里写死「3 天」的话这里就对不上）。
	{name: ebGoneLong, expiresIn: -400*86400 - 43200, wantText: "已过期 400 天", wantTier: "bad"},
	// 这一台是"绝不能写成剩 0 天"那条坑的现场：还剩 6 小时 ⇒ remaining_days = 0，
	// 但 expired = false（服务端按"剩余秒数 ≤ 0"判，见 dto.go）。
	{name: ebUnder1, expiresIn: 6 * 3600, wantText: "不足 1 天", wantTier: "bad"},
	{name: ebOne, expiresIn: 86400 + 43200, wantText: "剩 1 天", wantTier: "bad"},
	{name: ebThree, expiresIn: 3*86400 + 43200, wantText: "剩 3 天", wantTier: "warn"},
	{name: ebSeven, expiresIn: 7*86400 + 43200, wantText: "剩 7 天", wantTier: "soon", live: true},
	{name: ebEight, expiresIn: 8*86400 + 43200},
	{name: ebNone},
	{name: ebLongName, expiresIn: 3*86400 + 43200, wantText: "剩 3 天", wantTier: "warn"},
}

// ebTierOf 从 className（"card-expiry soon" 这种）里取出档位。
func ebTierOf(cls string) string {
	for _, tier := range []string{"bad", "warn", "soon"} {
		if strings.Contains(cls, " "+tier) {
			return tier
		}
	}
	return ""
}

// ebZeroDaysRe 匹配那个项目明确反对的「0 天」（"剩 0 天" / "已过期 0 天"）。
//
// ⚠ 不能直接 strings.Contains(text, "0 天")：那会把「剩 10 天」「已过期 400 天」
// 这种**正确**的多位数文案也判成红的（"400 天"里就含 "0 天"）。所以要求那个 0
// 前面不是数字 —— 这条护栏的意图一个字都没变，只是不再误伤多位数。
var ebZeroDaysRe = regexp.MustCompile(`(^|[^0-9])0 天`)

// ebZeroDays 报告徽章文案里有没有那个"0 天"。
func ebZeroDays(text string) bool { return ebZeroDaysRe.MatchString(text) }

// TestExpiryBadgeInRealBrowser 是本文件的主用例。
func TestExpiryBadgeInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	ids := ebSeed(t, br)
	liveID := ids[ebSeven]

	// live 那台每 250ms 一帧上报：1 Hz 的 realtimeLoop 每一拍都能看到"seq 涨了"，
	// 于是每秒都有一条**只带它**的变更集推下来 ⇒ 它的卡片每秒都被 updateCard 重画。
	// 这正是线上稳态的样子（interval_sec 默认 1），也是"不重建 DOM"的前提。
	stop := make(chan struct{})
	var stopOnce sync.Once
	defer stopOnce.Do(func() { close(stop) })
	go func() {
		var seq uint64
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			seq++
			h.srv.State().Update(liveID, 1, ebMetrics(seq), 0, time.Now())
		}
	}()

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		User: "admin", Pass: "a-very-good-password", Scenario: "expiry", Nodes: ids,
	}, expiryBadgeHarnessJS)

	// 续期那一步由页面发起：它 POST /__renew，这一层再回 Go 侧改库
	// （页面里的 app.js 是个 IIFE，拿不到它的 CSRF，也不该绕过去自己拼请求）。
	// 同步做、失败就把原因回给页面（页面会把它当成 fatal 带回来，出错时看得见原因）。
	mock := newMockServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__renew" {
			if err := ebRenew(br, liveID, ebSeven, time.Now().Add(60*24*time.Hour).Unix()); err != nil {
				http.Error(w, "续期失败: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		proxy.ServeHTTP(w, r)
	}))

	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, "1500,1100")

	var res ebResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s\n已经走过的步骤：%v\nJS 报错：%v", res.Fatal, res.Steps, res.Errs)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("自检脚本走过的步骤：%v", res.Steps)
	for _, c := range res.Main.Cards {
		t.Logf("卡片 %-14s 徽章=%v 文案=%q 档位=%q 颜色=%s 边框=%s 距右=%.1fpx 宽=%.1f(自然 %.1f) 名字截断=%v head孩子=%d",
			c.Name, c.BadgeExists, c.Text, ebTierOf(c.Cls), c.Color, c.BorderColor, c.Gap, c.Width, c.NaturalWidth, c.TruncatedName, c.HeadChildren)
	}

	if got := len(res.Main.Cards); got != len(ebNodes) {
		t.Fatalf("首页上量到 %d 张卡片，期望 %d 张：现场没铺开，后面的断言都没有判别力", got, len(ebNodes))
	}

	// ---- ① 五档文案与三档颜色 -------------------------------------------------
	//
	// 期望值写死在 ebNodes 里；这里逐台对上去。同时钉住一条项目明确反对的写法：
	// 任何一台机器上的徽章都不许出现「0 天」（还剩 6 小时那台最容易写出它）。
	for _, want := range ebNodes {
		card := res.Main.card(t, want.name)
		if want.wantTier == "" {
			if card.BadgeExists {
				t.Errorf("%s：不该有徽章（>7 天或没填到期日），实际文案 %q —— "+
					"方案 D 的整个前提就是这几张卡片与改动前逐像素一致", want.name, card.Text)
			}
			if card.HeadChildren != 1 {
				t.Errorf("%s：没有徽章时 .card-head 只该有一个孩子（左栏），实际 %d 个："+
					"多出来的孩子会改变布局，卡片就不是「逐像素一致」了", want.name, card.HeadChildren)
			}
			continue
		}
		if !card.BadgeExists {
			t.Errorf("%s：应当有一个到期徽章（期望文案 %q），实际 DOM 里没有", want.name, want.wantText)
			continue
		}
		if card.Text != want.wantText {
			t.Errorf("%s：徽章文案 = %q，期望 %q（文案由 expired / expired_days / remaining_days 三个字段推出来："+
				"过期那一档的天数只能来自 expired_days —— 写错档就会变成「已过期」或者项目明确反对的「剩 0 天」）",
				want.name, card.Text, want.wantText)
		}
		if ebZeroDays(card.Text) {
			t.Errorf("%s：徽章上出现了「%s」——「0 天」这一档必须写成别的（见 internal/server/dto.go 的注释）",
				want.name, card.Text)
		}
		if got := ebTierOf(card.Cls); got != want.wantTier {
			t.Errorf("%s：档位 = %q（class=%q），期望 %q", want.name, got, card.Cls, want.wantTier)
		}
		if card.HeadChildren != 2 {
			t.Errorf("%s：有徽章时 .card-head 该是两个孩子（左栏 + 徽章），实际 %d 个", want.name, card.HeadChildren)
		}
		if card.ParentCls != "card-head" {
			t.Errorf("%s：徽章的父元素是 %q，期望 card-head —— 插进 .card-name-row 够不到最右，"+
				"插进 .card-name 会被 overflow:hidden 裁掉", want.name, card.ParentCls)
		}
		if !card.SameLine {
			t.Errorf("%s：徽章与名字不在同一行上（几何判据没过）", want.name)
		}
		// 贴右：徽章右边缘到卡片内容区右边缘的距离必须是 0（允许半个像素的取整误差）。
		if card.Gap < -0.5 || card.Gap > 0.5 {
			t.Errorf("%s：徽章右边缘距卡片内容区右边缘 %.1fpx，期望 0（这才是「名称那一行的最右边」）",
				want.name, card.Gap)
		}
		// 一个像素都不缩：卡片上的宽度必须等于它脱离 flex 之后的自然宽度。
		if d := card.Width - card.NaturalWidth; d < -0.5 || d > 0.5 {
			t.Errorf("%s：徽章在卡片上被压成了 %.1fpx（自然宽度 %.1fpx）—— 挤压方向必须是「名字让位」",
				want.name, card.Width, card.NaturalWidth)
		}
	}

	// 三档颜色：红 = --bad，橙 = --warn，"淡琥珀" = 同一支 --warn 降不透明度。
	// 期望值由页面里另取一个探针元素现算（与徽章走的是两条独立的取值路径）。
	ebCheckColors(t, "浅色", res.Main, res.Vars)

	// 边框必须是中性的 --tag-border（沿用 .tag 的观感），三档都一样：
	// 只给某一档加底色/彩边会让最不紧急的那一档反而最花。
	for _, c := range res.Main.Cards {
		if !c.BadgeExists {
			continue
		}
		if c.BorderColor != res.Vars["tagBorder"] {
			t.Errorf("%s：徽章边框 = %s，期望 %s（--tag-border，中性描边）", c.Name, c.BorderColor, res.Vars["tagBorder"])
		}
		if c.Background != "rgba(0, 0, 0, 0)" {
			t.Errorf("%s：徽章有底色（%s）—— 方案 D 的观感是「无底色」", c.Name, c.Background)
		}
	}

	// 长名字：只截断名字，徽章照旧贴右、不缩。
	long := res.Main.card(t, ebLongName)
	if !long.TruncatedName {
		t.Error("31 字符的中文长名没有被截断 —— 现场没造出「名字长到挤不下」这个形态，这条断言就没有判别力")
	}
	// 短名字那几张反过来必须**没有**被截断（否则"名字让位"那句结论就无从谈起）。
	if short := res.Main.card(t, ebThree); short.TruncatedName {
		t.Errorf("%s：这么短的名字不该被截断", ebThree)
	}

	// ---- ①b SSE 快照帧里带着新字段，而且 remaining_days 在同一台上仍然是 0 -------
	//
	// 为什么要单独量一次：卡片上的字既可能来自建连时那次 HTTP 拉取、也可能来自推送帧，
	// 光看 DOM 分不清。建连后的第一帧是服务端的**全量快照**（api_stream.go 的
	// snapshotPayload → encodePayload，与每秒推送同一个编码函数），所以它足以回答
	// "推送路径上有没有这个字段"——这一档以前只能靠"两条路径都走同一个 struct"去推断。
	//
	// 帧级同时钉住两件事：新字段到了浏览器（且值对），以及**同一台机器的
	// remaining_days 仍然是 0**（这就是"语义一个字节都没改"的帧级证据）。
	if res.FirstFrameBytes <= 0 || res.FirstFrame == "" {
		t.Fatalf("没抓到 SSE 快照帧（%d 字节）：这条断言就没有判别力", res.FirstFrameBytes)
	}
	var snap struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(res.FirstFrame), &snap); err != nil {
		t.Fatalf("解析 SSE 快照帧: %v", err)
	}
	t.Logf("SSE 快照帧：%d 字节，%d 台机器（含 %s 的 \"expired_days\":400 在内）",
		res.FirstFrameBytes, len(snap.Nodes), ebGoneLong)
	if len(snap.Nodes) != len(ebNodes) {
		t.Errorf("快照帧里有 %d 台机器，期望 %d 台", len(snap.Nodes), len(ebNodes))
	}
	for _, want := range ebNodes {
		var got map[string]any
		for _, n := range snap.Nodes {
			if n["name"] == want.name {
				got = n
				break
			}
		}
		if got == nil {
			t.Errorf("SSE 快照帧里没有 %s", want.name)
			continue
		}
		rawExpiredDays, ok := got["expired_days"]
		if !ok {
			t.Errorf("%s：帧里的节点没有 expired_days（前端每秒收到的那一份拿不到天数，"+
				"徽章就只能写「已过期」）", want.name)
			continue
		}
		expiredDays, _ := rawExpiredDays.(float64)
		remaining, _ := got["remaining_days"].(float64)
		wantExpiredDays := int64(0)
		if want.expiresIn < 0 {
			wantExpiredDays = -want.expiresIn / 86400
		}
		if int64(expiredDays) != wantExpiredDays {
			t.Errorf("%s：帧里的 expired_days = %d，期望 %d", want.name, int64(expiredDays), wantExpiredDays)
		}
		if want.expiresIn < 0 && remaining != 0 {
			t.Errorf("%s：已过期的机器在帧里 remaining_days = %v，期望 0 —— "+
				"这个字段的语义一个字节都不许改（老前端依赖它非负）", want.name, remaining)
		}
	}

	// ---- ② 不会每秒重建 DOM ---------------------------------------------------
	//
	// 同一个档位下连续 4 秒：徽章上的 MutationObserver 必须一次都没响，
	// 而且元素身份不变（不是"删了再建一个一样的"）。前提是这 4 秒里卡片真的被重画过
	// —— 帧数没到 2 就是空断言，直接报红。
	t.Logf("4 秒观察窗口：收到 %d 条推送帧，徽章上的 DOM 变更 %d 次，元素身份不变=%v（文案 %q → %q）",
		res.Churn.Frames, res.Churn.Mutations, res.Churn.SameElement, res.Churn.TextBefore, res.Churn.TextAfter)
	if res.Churn.Missing {
		t.Errorf("%s 上没有徽章：4 秒观察窗口的结论不成立（这一档必须有一个 ≤7 天的徽章）", ebSeven)
	}
	if res.Churn.Frames < 2 {
		t.Errorf("4 秒里只收到 %d 条推送帧（期望 ≥2）：卡片根本没被重画，"+
			"「徽章没动」就成了永远成立的空断言", res.Churn.Frames)
	}
	if res.Churn.Mutations != 0 {
		t.Errorf("4 秒里徽章上发生了 %d 次 DOM 变更（期望 0）：档位没变就不该动 DOM —— "+
			"否则每秒重建会闪，也会白白增加 DOM 操作", res.Churn.Mutations)
	}
	if !res.Churn.SameElement {
		t.Error("4 秒之后徽章换了一个元素（元素身份变了）：这就是「每秒拆了重建」")
	}
	if res.Churn.TextBefore != res.Churn.TextAfter {
		t.Errorf("观察窗口里文案从 %q 变成了 %q（这两个时刻的剩余天数不该变）",
			res.Churn.TextBefore, res.Churn.TextAfter)
	}

	// 帧的字节数（浏览器侧实测）：这一轮 DTO 加了 expired_days，报告里要给数字。
	// 这条断言只保证"量到了"—— 量不到的话那个数字就是 0，读者会以为帧是空的。
	t.Logf("推送帧：最后一帧 %d 字节（整场收到 %d 条）", res.FrameBytes, res.Frames)
	if res.FrameBytes <= 0 {
		t.Errorf("没量到推送帧的字节数（%d）：帧大小那条读数就没有意义了（TextEncoder 没装上？）",
			res.FrameBytes)
	}

	// ---- ③ 深色主题下颜色也跟着换 --------------------------------------------
	if len(res.Dark.Cards) != len(ebNodes) {
		t.Fatalf("深色主题下量到 %d 张卡片，期望 %d 张", len(res.Dark.Cards), len(ebNodes))
	}
	ebCheckColors(t, "深色", res.Dark, res.DarkVars)
	for _, name := range []string{ebGone, ebThree, ebSeven} {
		light := res.Main.card(t, name)
		dark := res.Dark.card(t, name)
		if light.Color == dark.Color {
			t.Errorf("%s：浅色与深色下徽章颜色一模一样（%s）—— 说明它没跟着主题变量走", name, light.Color)
		}
	}
	if res.DarkVars["warn"] == res.Vars["warn"] {
		t.Errorf("深色主题下 --warn 还是 %s：主题根本没切过去，上面那条比较没有意义", res.DarkVars["warn"])
	}

	// ---- ④ 窄屏（320px 视口，落进 <=640px 那一条断点） -------------------------
	//
	// 用同源 iframe 拿到一个**真的** 320px 视口（无头 Chrome 的 --window-size 有最小宽度，
	// 拿不到 320），媒体查询因此是真的生效的。验的是：徽章不换行、贴右、一个像素都不缩，
	// 而且**最多只占它自己那点地方**（宽度 + .card-head 的 10px gap）。
	//
	// 实测的现场（真卡片、真数据）：320px 视口下网格可用宽 273px；名字是 31 个中文字符时
	// .card-name 的最小内容宽度本身就有 328.5px（nowrap），所以**没有徽章**时卡片也已
	// 经是 372.5px 了 —— 横向溢出是既有的，不是这次改动带来的；有徽章是 430.7px，
	// 多出来的正好是 48.2 + 10。名字短的卡片两档都装得下（下面最后一条断言）。
	//
	// 这条用例的判据是**因果**的（narrowProbe）：把徽章从长名那张卡片上摘下来、
	// 逼一次重排，再量同一个宽度 —— 代价超过徽章自身那点地方才算红。
	narrow := res.Narrow
	if narrow == nil {
		t.Fatal("窄屏那一档没有读数：iframe 里的首页没跑起来")
	}
	probe := res.NarrowProbe
	t.Logf("窄屏：视口 %dpx，长名那台的卡片宽 %v，网格 %v/%v（client/scroll），徽章摘掉后 %v / scroll %v（副标题 %q）",
		narrow.InnerWidth, probe["cardW"], res.NarrowDebug["gridClientWidth"], probe["gridScrollW"],
		probe["cardWNoBadge"], probe["gridScrollWNoBadge"], probe["subText"])
	t.Logf("窄屏布局读数：%v", res.NarrowDebug)
	t.Logf("窄屏：各部分的最小内容宽度 %v；摘掉徽章后 card=%v head=%v",
		probe["minContent"], probe["minContentNoBadge"], probe["minContentNoBadge"])
	t.Logf("把列压到容器宽之后溢出的元素（谁撑开的列）：%v", probe["squeezed"])
	if narrow.InnerWidth != 320 {
		t.Fatalf("窄屏那一档的视口是 %dpx，期望 320px（>640 就落不进移动断点，这条断言会失去判别力）", narrow.InnerWidth)
	}
	if res.NarrowDebug["mediaMobile"] != true {
		t.Fatalf("窄屏那一档 (max-width: 640px) 没命中：%v", res.NarrowDebug)
	}
	// 因果判据：徽章在窄屏下最多只能占**它自己那点地方**（宽度 + .card-head 的 10px gap）。
	//
	// 注意这里**不是**"宽度不变"：窄屏下网格是 1fr，而轨道的下限是 auto（= 卡片的最小
	// 内容宽度），徽章作为 .card-head 的第二个孩子必然进这个下限 —— 这是方案 D 选定的
	// 插法自带的代价（见报告里"窄屏"那一节：真卡片本来就已经超过视口宽度）。
	// 这条断言挡的是"徽章还额外顶出了别的宽度"（比如把名字挤到换行、或让别的盒子溢出）。
	cost := ebNum(probe["cardW"]) - ebNum(probe["cardWNoBadge"])
	nl := narrow.card(t, ebLongName)
	if cost > nl.Width+10+0.5 {
		t.Errorf("窄屏下徽章让卡片宽了 %.1fpx，超过它自己的宽度(%.1f)+gap(10)："+
			"说明它不止占了自己的位置（把别的盒子也顶出去了）", cost, nl.Width)
	}
	if cost < -0.5 {
		t.Errorf("窄屏下摘掉徽章之后卡片反而更窄了 %.1fpx？量错了吧", -cost)
	}
	if !nl.BadgeExists || nl.Text != "剩 3 天" {
		t.Errorf("窄屏下长名那台的徽章 = %v / %q，期望在且写着「剩 3 天」", nl.BadgeExists, nl.Text)
	}
	if !nl.SameLine {
		t.Error("窄屏下徽章掉到了另一行上")
	}
	if nl.Gap < -0.5 || nl.Gap > 0.5 {
		t.Errorf("窄屏下徽章右边缘距卡片右边缘 %.1fpx，期望 0", nl.Gap)
	}
	if d := nl.Width - nl.NaturalWidth; d < -0.5 || d > 0.5 {
		t.Errorf("窄屏下徽章被压成了 %.1fpx（自然宽度 %.1fpx）", nl.Width, nl.NaturalWidth)
	}
	// 短名字的卡片（这一张也有徽章）在窄屏下也该是"贴右、同一行、不缩"，
	// 与长名那张唯一不同的是名字有没有被截断。
	//
	// ⚠ 这里**不**断言"短名的卡片装得下"：移动端是单列网格，轨道的宽度由**所有**卡片里
	// 最宽的那个最小内容宽度决定 —— 现场有一台长名机器，于是八张卡片一样宽（都 430.7px）。
	// 这正是既有溢出会波及整页的原因，报告里那一段说的就是它。
	shortNarrow := narrow.card(t, ebSeven)
	if !shortNarrow.BadgeExists {
		t.Fatalf("%s 在窄屏下没有徽章：这一条断言就没有判别力了", ebSeven)
	}
	if !shortNarrow.SameLine {
		t.Errorf("窄屏下 %s 的徽章掉到了另一行上", ebSeven)
	}
	if shortNarrow.Gap < -0.5 || shortNarrow.Gap > 0.5 {
		t.Errorf("窄屏下 %s 的徽章右边缘距卡片右边缘 %.1fpx，期望 0", ebSeven, shortNarrow.Gap)
	}
	if d := shortNarrow.Width - shortNarrow.NaturalWidth; d < -0.5 || d > 0.5 {
		t.Errorf("窄屏下 %s 的徽章被压成了 %.1fpx（自然宽度 %.1fpx）", ebSeven, shortNarrow.Width, shortNarrow.NaturalWidth)
	}
	if shortNarrow.TruncatedName {
		t.Errorf("窄屏下 %s 这么短的名字不该被截断（卡片宽 %.1fpx）", ebSeven, shortNarrow.CardW)
	}

	// ---- ⑤ 续期之后徽章必须被摘掉（不留空壳） ---------------------------------
	//
	// 把 live 那台的到期日改到 60 天之后：档位变成"不显示"，DOM 里不该再有任何
	// 徽章节点，.card-head 也要回到只有一个孩子。
	t.Logf("续期之后：head 的孩子数 = %d，还有徽章 = %v（第 %d 帧时读到的）",
		res.AfterRenew.HeadChildren, res.AfterRenew.Badge, res.AfterRenew.Frames)
	if res.AfterRenew.Badge {
		t.Error("续期（剩余 60 天）之后徽章还在 DOM 里：>7 天时必须把它摘掉，不能留一个空壳")
	}
	if res.AfterRenew.HeadChildren != 1 {
		t.Errorf("续期之后 .card-head 有 %d 个孩子，期望 1 个（只剩左栏）", res.AfterRenew.HeadChildren)
	}
	for _, c := range res.AfterRenew.Cards {
		if ebZeroDays(c.Text) {
			t.Errorf("%s：续期之后徽章文案变成了 %q", c.Name, c.Text)
		}
	}
}

// ebNum 把 JSON 里的数字（float64）取出来；取不到就当 0。
func ebNum(v any) float64 {
	f, _ := v.(float64)
	return f
}

// ebCheckColors 逐档核对颜色：红档 = --bad，橙档 = --warn，淡琥珀档 = 同一支 --warn
// 但**带透明度**（这就是"降不透明度得到的淡琥珀"，也是项目里没有第二种黄时的做法）。
//
// 期望值来自页面上的探针元素（varColor），与徽章自身的计算样式是两条独立的取值路径，
// 不是把同一个字符串抄两遍。
func ebCheckColors(t *testing.T, label string, m ebMeasure, vars map[string]string) {
	t.Helper()
	bad, okBad := ebParseColor(vars["bad"])
	warn, okWarn := ebParseColor(vars["warn"])
	if !okBad || !okWarn {
		t.Fatalf("%s：--bad / --warn 的计算值解析不出来（%q / %q）", label, vars["bad"], vars["warn"])
	}
	for _, c := range m.Cards {
		tier := ebTierOf(c.Cls)
		if tier == "" {
			continue
		}
		got, ok := ebParseColor(c.Color)
		if !ok {
			t.Errorf("%s/%s：徽章颜色 %q 解析不出来", label, c.Name, c.Color)
			continue
		}
		if tier == "soon" {
			// 淡琥珀：同一支 --warn 的 rgb 分量 + 小于 1 的 alpha。
			if got.R != warn.R || got.G != warn.G || got.B != warn.B {
				t.Errorf("%s/%s：淡琥珀档的颜色 %s（rgb %d,%d,%d）与 --warn（%s = rgb %d,%d,%d）不是同一支琥珀 —— "+
					"项目里没有第二种黄，这一档只能是同一支 --warn 降不透明度",
					label, c.Name, c.Color, got.R, got.G, got.B, vars["warn"], warn.R, warn.G, warn.B)
			}
			if !(got.A > 0 && got.A < 1) {
				t.Errorf("%s/%s：淡琥珀档的颜色 %s 的 alpha = %v，必须是 0<α<1：它要比 ≤3 天那一档淡",
					label, c.Name, c.Color, got.A)
			}
			continue
		}
		want := warn
		if tier == "bad" {
			want = bad
		}
		if got != want {
			t.Errorf("%s/%s：%s 档的颜色 = %s，期望 %s", label, c.Name, tier, c.Color, vars[tier])
		}
	}
}

// ebColor 是一个解析出来的计算色值。
type ebColor struct {
	R, G, B int
	A       float64
}

// ebParseColor 把浏览器给的计算色值解析成 (r, g, b, a)。
//
// 两种写法都要认，因为本项目两种都会出现：
//   - "rgb(217, 119, 6)" / "rgba(217, 119, 6, 0.7)"    —— 普通颜色；
//   - "color(srgb 0.851 0.467 0.0235 / 0.7)"           —— color-mix() 算出来的颜色，
//     Chrome 给的就是这一种（同一支琥珀，只是换了个色空间写法）。
//
// 不解析的话，"淡琥珀是同一支 --warn 降不透明度"这条断言会因为写法不同而假红。
func ebParseColor(v string) (ebColor, bool) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "color(") {
		body := strings.TrimSuffix(strings.TrimPrefix(v, "color("), ")")
		body = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "srgb"))
		head, tail, hasAlpha := strings.Cut(body, "/")
		fields := strings.Fields(head)
		if len(fields) != 3 {
			return ebColor{}, false
		}
		out := ebColor{A: 1}
		dst := []*int{&out.R, &out.G, &out.B}
		for i, f := range fields {
			var fv float64
			if _, err := fmt.Sscanf(f, "%g", &fv); err != nil {
				return ebColor{}, false
			}
			*dst[i] = int(math.Round(fv * 255))
		}
		if hasAlpha {
			var a float64
			if _, err := fmt.Sscanf(strings.TrimSpace(tail), "%g", &a); err != nil {
				return ebColor{}, false
			}
			out.A = a
		}
		return out, true
	}
	body := strings.TrimPrefix(v, "rgba(")
	body = strings.TrimPrefix(body, "rgb(")
	body = strings.TrimSuffix(body, ")")
	fields := strings.Split(body, ",")
	if len(fields) != 3 && len(fields) != 4 {
		return ebColor{}, false
	}
	out := ebColor{A: 1}
	dst := []*int{&out.R, &out.G, &out.B}
	for i := 0; i < 3; i++ {
		var iv int
		if _, err := fmt.Sscanf(strings.TrimSpace(fields[i]), "%d", &iv); err != nil {
			return ebColor{}, false
		}
		*dst[i] = iv
	}
	if len(fields) == 4 {
		var a float64
		if _, err := fmt.Sscanf(strings.TrimSpace(fields[3]), "%g", &a); err != nil {
			return ebColor{}, false
		}
		out.A = a
	}
	return out, true
}

// ebSeed 按 ebNodes 建节点（到期日直接写进创建请求），并核对服务端算出来的
// expired / remaining_days / expired_days 与现场设计一致。
//
// 这一步是**前置条件**断言：现场造错了（比如"剩 8 天"其实算成了 7 天），
// 后面的每一条断言都会在错误的前提下运行 —— 那比红更糟。
func ebSeed(t *testing.T, br *browser) map[string]int64 {
	t.Helper()
	now := time.Now().Unix()
	ids := make(map[string]int64, len(ebNodes))
	for _, n := range ebNodes {
		var expiresAt int64
		if n.expiresIn != 0 {
			expiresAt = now + n.expiresIn
		}
		status, body := br.do(http.MethodPost, "/api/v1/nodes", map[string]any{
			"name": n.name, "group_name": "测试", "region": "HK", "interval_sec": 1,
			"expires_at": expiresAt,
		}, true)
		if status != http.StatusCreated {
			t.Fatalf("创建节点 %s 失败: %d %v", n.name, status, body)
		}
		node, _ := body["node"].(map[string]any)
		id, _ := node["id"].(float64)
		ids[n.name] = int64(id)
	}
	if len(ids) != len(ebNodes) {
		t.Fatalf("建了 %d 台机器，期望 %d 台", len(ids), len(ebNodes))
	}

	// 服务端算出来的三个字段必须与现场设计一致（期望值由 expiresIn 反推）：
	// expired 说"过没过期"，remaining_days 说"还剩几天"（过期时夹 0），
	// expired_days 说"已经过期几天"—— 徽章文案读的就是后两个 + expired。
	status, body := br.do(http.MethodGet, "/api/v1/nodes", nil, false)
	if status != http.StatusOK {
		t.Fatalf("读节点列表: %d", status)
	}
	list, _ := body["nodes"].([]any)
	byName := map[string]map[string]any{}
	for _, raw := range list {
		n, _ := raw.(map[string]any)
		name, _ := n["name"].(string)
		byName[name] = n
	}
	for _, want := range ebNodes {
		got := byName[want.name]
		if got == nil {
			t.Fatalf("节点列表里没有 %s", want.name)
		}
		expired, _ := got["expired"].(bool)
		days, _ := got["remaining_days"].(float64)
		rawExpiredDays, hasExpiredDays := got["expired_days"]
		if !hasExpiredDays {
			t.Fatalf("节点 %s 的响应里没有 expired_days：徽章的「已过期 N 天」读的就是它，"+
				"缺了它这一档只会退回「已过期」，这一轮新加的那条断言就成了空断言", want.name)
		}
		expiredDays, _ := rawExpiredDays.(float64)
		wantExpired := want.expiresIn < 0
		wantDays := int64(0)
		wantExpiredDays := int64(0)
		if want.expiresIn > 0 {
			wantDays = want.expiresIn / 86400
		} else if want.expiresIn < 0 {
			// 已经过去的整天数：向零取整（与 dto.go 的 applyPricing、
			// alert.ExpiryPhrase 是同一口径）。
			wantExpiredDays = -want.expiresIn / 86400
		}
		if expired != wantExpired || int64(days) != wantDays || int64(expiredDays) != wantExpiredDays {
			t.Fatalf("现场造错了 %s：服务端给的是 expired=%v remaining_days=%d expired_days=%d，"+
				"期望 %v / %d / %d（后面所有断言的前提就是这个）",
				want.name, expired, int64(days), int64(expiredDays), wantExpired, wantDays, wantExpiredDays)
		}
	}
	return ids
}

// ebRenew 走真接口把一台机器的到期日改到将来（用户点「续期」的效果）。
//
// 这个接口是**整体替换**语义（handleUpdateNode 把请求体里的字段逐个盖到当前配置上），
// 所以请求体里必须原样带上建节点时那几个字段：缺了 traffic_warn_pct / reset_day
// 会被存储层当成非法值（0）直接拒掉。
//
// 不用 browser.do：那个在出错时 t.Fatalf，而这里是从 HTTP handler 那一侧的
// goroutine 上调用的（Fatalf 在非测试 goroutine 上会打乱用例的收尾）。
func ebRenew(br *browser, id int64, name string, expiresAt int64) error {
	payload, err := json.Marshal(map[string]any{
		"name": name, "group_name": "测试", "region": "HK", "interval_sec": 1,
		"traffic_warn_pct": 80, "reset_day": 1,
		"expires_at": expiresAt,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch, br.base+fmt.Sprintf("/api/v1/nodes/%d", id), strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", br.csrf)
	resp, err := br.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("状态码 %d：%s", resp.StatusCode, raw)
	}
	return nil
}

// ebMetrics 造一帧"心跳在动"的上报：seq 变了就够，指标本身与徽章无关。
func ebMetrics(seq uint64) protocol.Metrics {
	return protocol.Metrics{Net: protocol.Net{
		Iface: "eth0", RxRate: float64(seq), TxRate: float64(seq), BootID: "boot-eb",
	}}
}

// ---------------------------------------------------------------- 截图

// TestExpiryBadgeScreenshots 给人工核对出一条路：把改造后的真卡片截成 PNG。
//
// 默认**跳过**（CI 上不该往磁盘里写图）。要看图时：
//
//	$env:PROBE_SHOT_DIR = "D:\DEEPSEEK\_audit"; go test ./internal/e2e/ -run TestExpiryBadgeScreenshots -v
//
// 与断言那条路径的唯一区别是这里带 --virtual-time-budget（截图必须等页面画完），
// 而页面里的自检脚本在 screenshot 模式下走的是"铺开卡片就停"那条分支
// （见 expiryBadgeHarnessJS 里的 CFG.shot），不会把徽章摘掉。
func TestExpiryBadgeScreenshots(t *testing.T) {
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

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	ids := ebSeed(t, br)

	shots := []struct {
		name  string
		theme string
		w, h  int
	}{
		{"expiry-badge-实现后", "", 1500, 1180},
		{"expiry-badge-实现后-深色", "dark", 1500, 1180},
	}
	for _, s := range shots {
		cfg := tzHarnessConfig{
			User: "admin", Pass: "a-very-good-password", Scenario: "expiry",
			Nodes: ids, Shot: "expiry", Theme: s.theme,
		}
		shotMock := newMockServer(t, newHarnessProxy(t, "http://"+h.addr, cfg, expiryBadgeHarnessJS))
		out := filepath.Join(outDir, s.name+".png")
		// 九张卡片 + 七个徽章（现场里有两台机器按设计就没有徽章）都画出来了才截。
		ready := "document.querySelectorAll('#grid > .card').length === 9" +
			" && document.querySelectorAll('.card-expiry').length === 7"
		cdpShot(t, chrome, shotMock.URL+"/#/", out, ready, "", s.w, s.h)
		st, err := os.Stat(out)
		if err != nil {
			t.Fatalf("%s: 截图没有生成: %v", s.name, err)
		}
		t.Logf("%s → %s（%d 字节）", s.name, out, st.Size())
	}
}

// ebExpiredReadyJS 判断"那两张过期的卡片都画好了、徽章文案也都对"。
//
// 为什么不去数 .card-expiry 的总数：这一条只关心过期那一档，现场加机器不该影响它，
// 文案本身才是有判别力的东西（少了天数字段时它会变成「已过期」⇒ 这里不成立）。
const ebExpiredReadyJS = `(function () {
  var want = ['已过期 3 天', '已过期 400 天'], got = {};
  var bs = document.querySelectorAll('.card-expiry');
  for (var i = 0; i < bs.length; i++) { got[bs[i].textContent] = true; }
  for (var j = 0; j < want.length; j++) { if (!got[want[j]]) return false; }
  return document.querySelectorAll('#grid > .card').length >= 9;
})()`

// ebExpiredClipJS 给出"要截哪一块"：页面上所有带「已过期」徽章的卡片的外接矩形，
// 四周留 16px、放大 2 倍 —— 徽章文字只有 11px，原尺寸在整页图里看不清。
const ebExpiredClipJS = `(function () {
  var rs = [], cards = document.querySelectorAll('#grid > .card');
  for (var i = 0; i < cards.length; i++) {
    var b = cards[i].querySelector('.card-expiry');
    if (b && b.textContent.indexOf('已过期') === 0) rs.push(cards[i].getBoundingClientRect());
  }
  if (!rs.length) return null;
  var l = rs[0].left, t = rs[0].top, r = rs[0].right, bo = rs[0].bottom;
  for (var j = 1; j < rs.length; j++) {
    if (rs[j].left < l) l = rs[j].left;
    if (rs[j].top < t) t = rs[j].top;
    if (rs[j].right > r) r = rs[j].right;
    if (rs[j].bottom > bo) bo = rs[j].bottom;
  }
  var pad = 16;
  return { x: Math.max(0, l - pad), y: Math.max(0, t - pad),
           width: r - l + 2 * pad, height: bo - t + 2 * pad, scale: 2 };
})()`

// TestExpiryBadgeExpiredDaysScreenshot 单独截"已过期 N 天"那两张真卡片（一小块、2 倍大）。
//
// 与上面那条整页截图的分工：整页图用来看色阶与布局，这张用来看**天数**是否真的写出来了
// —— 交付时要有一张能看清「已过期 3 天」/「已过期 400 天」的图。默认同样跳过：
//
//	$env:PROBE_SHOT_DIR = "D:\DEEPSEEK\_audit"; go test ./internal/e2e/ -run TestExpiryBadgeExpiredDaysScreenshot -v
func TestExpiryBadgeExpiredDaysScreenshot(t *testing.T) {
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

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	ids := ebSeed(t, br)

	cfg := tzHarnessConfig{
		User: "admin", Pass: "a-very-good-password", Scenario: "expiry",
		Nodes: ids, Shot: "expiry",
	}
	shotMock := newMockServer(t, newHarnessProxy(t, "http://"+h.addr, cfg, expiryBadgeHarnessJS))
	out := filepath.Join(outDir, "expiry-badge-过期天数.png")
	cdpShot(t, chrome, shotMock.URL+"/#/", out, ebExpiredReadyJS, ebExpiredClipJS, 1500, 1180)
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("截图没有生成: %v", err)
	}
	t.Logf("已过期天数的那两张卡片 → %s（%d 字节，按卡片外接矩形裁、放大 2 倍）", out, st.Size())
}

// cdpShot 打开页面、等页面里的条件成立，然后截一张图。
//
// clipJS 非空时，用它（在页面里求值）给出要截的那一块 {x,y,width,height,scale}：
// 整页图里一个几十像素宽的徽章缩下来看不清，交付用的那张要能看清字。
//
// 为什么不用仓库里其它截图用例那套 --virtual-time-budget + --screenshot：首页挂着
// SSE 长连接，虚拟时间在"还有网络请求没完成"时不往前走 —— 这条路径在本机**实测会一直
// 挂到超时**（预算 60s 与 15s 都试过，120s 也不出图），而虚拟时间一旦卡住没有任何
// 办法从外面救回来。改走 DevTools 协议之后页面按**真实时间**跑（SSE 与断言那条路径
// 完全一样），"什么时候可以截"由 Go 侧轮询页面自己的状态决定。
func cdpShot(t *testing.T, chrome, pageURL, out, readyJS, clipJS string, w, h int) {
	t.Helper()
	profile, err := os.MkdirTemp("", "probe-expiry-shot-")
	if err != nil {
		t.Fatalf("创建 Chrome 画像目录: %v", err)
	}
	defer func() { _ = os.RemoveAll(profile) }()

	args := []string{
		"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--disable-extensions", "--hide-scrollbars",
		"--remote-debugging-port=0", "--remote-allow-origins=*",
		"--user-data-dir=" + profile,
		"--window-size=" + strconv.Itoa(w) + "," + strconv.Itoa(h),
		pageURL,
	}
	cmd := exec.Command(chrome, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 Chrome: %v", err)
	}
	defer func() {
		killChrome(cmd)
		_ = cmd.Wait()
	}()

	// Chrome 把真正选中的调试端口写在画像目录里的 DevToolsActivePort（第一行）。
	portFile := filepath.Join(profile, "DevToolsActivePort")
	var port string
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(portFile); err == nil {
			if line, _, _ := strings.Cut(string(raw), "\n"); strings.TrimSpace(line) != "" {
				port = strings.TrimSpace(line)
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if port == "" {
		t.Fatalf("等不到 DevToolsActivePort。Chrome 输出尾部：\n%s", tail(buf.String(), 800))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn := dialCDP(t, ctx, port)
	defer conn.CloseNow()

	// 等页面自己把八张卡片与六个徽章都画出来（真实时间，SSE 照常）。
	ready := false
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := cdpBool(ctx, conn, 1, readyJS); err == nil && v {
			ready = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		title, _ := cdpEvalString(ctx, conn, 2, "document.title + ' | ' + document.querySelectorAll('#grid > .card').length + ' 张卡片 / ' + document.querySelectorAll('.card-expiry').length + ' 个徽章'")
		t.Fatalf("60 秒内页面没画齐（%s）：截图会是一张残缺的图，宁可不截", title)
	}

	params := map[string]any{"format": "png"}
	if clipJS != "" {
		// 裁哪一块由页面自己算（用 returnByValue 把 {x,y,width,height,scale} 取回来）。
		// 取到 null 说明页面上没有要截的那种卡片 —— 宁可不截，也不出一张空图。
		v, err := cdpEval(ctx, conn, 2, clipJS)
		if err != nil {
			t.Fatalf("算截图范围: %v", err)
		}
		if v == nil {
			t.Fatalf("算截图范围：表达式返回 null（页面上没有要截的那种卡片）")
		}
		params["clip"] = v
	}
	res, err := cdpCall(ctx, conn, 3, "Page.captureScreenshot", params)
	if err != nil {
		t.Fatalf("Page.captureScreenshot: %v", err)
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &shot); err != nil {
		t.Fatalf("解析截图结果: %v", err)
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatalf("解码截图: %v", err)
	}
	if err := os.WriteFile(out, png, 0o644); err != nil {
		t.Fatalf("写截图: %v", err)
	}
}

// dialCDP 连上页面目标的 DevTools WebSocket。
func dialCDP(t *testing.T, ctx context.Context, port string) *websocket.Conn {
	t.Helper()
	var wsURL string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
		if err == nil {
			var list []struct {
				Type string `json:"type"`
				WS   string `json:"webSocketDebuggerUrl"`
			}
			if dec := json.NewDecoder(resp.Body); dec.Decode(&list) == nil {
				for _, e := range list {
					if e.Type == "page" && e.WS != "" {
						wsURL = e.WS
						break
					}
				}
			}
			_ = resp.Body.Close()
		}
		if wsURL != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if wsURL == "" {
		t.Fatal("拿不到页面目标的 webSocketDebuggerUrl")
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("连接 DevTools: %v", err)
	}
	// 一张 1500×1180 的 PNG 的 base64 有几百 KB，默认 32KB 的读上限装不下。
	conn.SetReadLimit(64 << 20)
	return conn
}

// cdpCall 发一次 DevTools 调用并等它自己的那条回包（事件通知直接跳过）。
func cdpCall(ctx context.Context, conn *websocket.Conn, id int, method string, params any) (json.RawMessage, error) {
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, err
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var got struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &got) != nil || got.ID != id {
			continue
		}
		if got.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, got.Error.Message)
		}
		return got.Result, nil
	}
}

// cdpEval 求一个 JS 表达式的值（returnByValue，所以字符串/布尔直接拿得到）。
func cdpEval(ctx context.Context, conn *websocket.Conn, id int, expr string) (any, error) {
	res, err := cdpCall(ctx, conn, id, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return out.Result.Value, nil
}

func cdpBool(ctx context.Context, conn *websocket.Conn, id int, expr string) (bool, error) {
	v, err := cdpEval(ctx, conn, id, expr)
	if err != nil {
		return false, err
	}
	b, _ := v.(bool)
	return b, nil
}

func cdpEvalString(ctx context.Context, conn *websocket.Conn, id int, expr string) (string, error) {
	v, err := cdpEval(ctx, conn, id, expr)
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	return s, nil
}

// ---------------------------------------------------------------- 观测值

type ebResult struct {
	Errs     []string   `json:"errs"`
	Fatal    string     `json:"fatal"`
	Steps    []string   `json:"steps"`
	Main     ebMeasure  `json:"main"`
	Dark     ebMeasure  `json:"dark"`
	Narrow   *ebMeasure `json:"narrow"`
	Vars     ebVars     `json:"vars"`
	DarkVars ebVars     `json:"darkVars"`
	Frames   int        `json:"frames"`
	// FrameBytes 是浏览器侧量到的**真帧**字节数（最后一条 "nodes" 事件的 UTF-8 长度，
	// 见自检脚本里的 EventSource 包装）。这一轮 DTO 多了一个字段，报告里的
	// "帧大了多少字节"就是拿它 + Go 侧那条精确测量一起说的。
	FrameBytes int `json:"frameBytes"`
	// FirstFrame / FirstFrameBytes 是建连后第一帧（服务端发的**全量快照**，原样回传）。
	// 卡片上的字可能来自建连时那次 HTTP 拉取，只有帧本身能证明新字段真的在推送路径上。
	FirstFrame      string    `json:"firstFrame"`
	FirstFrameBytes int       `json:"firstFrameBytes"`
	Churn           ebChurn   `json:"churn"`
	AfterRenew      ebRenewed `json:"afterRenew"`
	// NarrowDebug 是窄屏那一档"宽度是谁定的"的读数（红了的时候用来看现场）。
	NarrowDebug map[string]any `json:"narrowDebug"`
	// NarrowProbe 是窄屏下的因果实验：摘掉徽章再量一次宽度（见 narrowProbe）。
	NarrowProbe map[string]any `json:"narrowProbe"`
	// TopDebug 是顶层窗口的同一份读数（用来对照：窄屏那一档到底哪里不一样）。
	TopDebug map[string]any `json:"topDebug"`
}

type ebVars map[string]string

type ebMeasure struct {
	InnerWidth   int      `json:"innerWidth"`
	GridOverflow float64  `json:"gridOverflow"`
	Cards        []ebCard `json:"cards"`
}

func (m ebMeasure) card(t *testing.T, name string) ebCard {
	t.Helper()
	for _, c := range m.Cards {
		if c.Name == name {
			return c
		}
	}
	names := make([]string, 0, len(m.Cards))
	for _, c := range m.Cards {
		names = append(names, c.Name)
	}
	t.Fatalf("量到的卡片里没有 %q（读到的是 %v）", name, names)
	return ebCard{}
}

type ebCard struct {
	Name          string  `json:"name"`
	HeadChildren  int     `json:"headChildren"`
	BadgeExists   bool    `json:"badgeExists"`
	Text          string  `json:"text"`
	Cls           string  `json:"cls"`
	Color         string  `json:"color"`
	BorderColor   string  `json:"borderColor"`
	Background    string  `json:"background"`
	ParentCls     string  `json:"parentCls"`
	Gap           float64 `json:"gap"`
	Width         float64 `json:"width"`
	NaturalWidth  float64 `json:"naturalWidth"`
	TruncatedName bool    `json:"truncatedName"`
	SameLine      bool    `json:"sameLine"`
	CardW         float64 `json:"cardW"`
}

type ebChurn struct {
	Mutations   int    `json:"mutations"`
	Frames      int    `json:"frames"`
	SameElement bool   `json:"sameElement"`
	TextBefore  string `json:"textBefore"`
	TextAfter   string `json:"textAfter"`
	// Missing 表示观察对象根本不存在（live 那台上没有徽章）：这时候
	// "徽章没动"是一句空话，必须单独报出来。
	Missing bool `json:"missing"`
}

type ebRenewed struct {
	Badge        bool     `json:"badge"`
	HeadChildren int      `json:"headChildren"`
	Frames       int      `json:"frames"`
	Cards        []ebCard `json:"cards"`
}

// ---------------------------------------------------------------- 自检脚本

// expiryBadgeHarnessJS 是这条用例的自检脚本：只做观测，一句断言都不下
// （判据全在 Go 侧，出错时的红字才读得懂）。
//
// 它按顺序做五件事：读一遍首页 → 盯住 live 那台的徽章看 4 秒 → 切深色再读一遍
// → 在 320px 的同源 iframe 里再读一遍 → 请求续期、等徽章消失后再读一次。
const expiryBadgeHarnessJS = `(function () {
  'use strict';
  // 这份脚本会被注入到每一个 "/" 响应里 —— 包括下面那个窄屏 iframe 自己加载的那一份。
  // 只有顶层那一份跑整套流程：iframe 里那一份再套一个 iframe 就没完没了了。
  if (window.top !== window) return;
  var CFG = window.__TZCFG || {};
  var LIVE = 'eb-7d';
  var NARROW = 320;
  var rawFetch = window.fetch.bind(window);
  var R = { errs: [], fatal: '', steps: [], frames: 0, frameBytes: 0, firstFrame: '', firstFrameBytes: 0 };

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // 数推送帧：注入的脚本排在 app.js 之前，所以包一层 EventSource 就能装上钩子。
  // 「不重建 DOM」那条断言的判别力全靠它 —— 没有帧 = 卡片根本没被重画。
  // 顺手量一下帧的**字节数**（UTF-8，与线上传输一致）：这一轮 DTO 多了一个字段，
  // 报告里要给出"一帧大了多少"，这里量的是真帧、不是估算。
  (function () {
    var Real = window.EventSource;
    if (!Real) return;
    var enc = window.TextEncoder ? new window.TextEncoder() : null;
    var Wrapped = function (url, opts) {
      var es = new Real(url, opts);
      es.addEventListener('nodes', function (e) {
        if (R.frames === 0 && typeof e.data === 'string') {
          // 建连后的第一帧是服务端的**全量快照**（api_stream.go 的 snapshotPayload，
          // 与每秒推送走同一个 encodePayload）。把原文留给 Go 侧：卡片上的字既可能
          // 来自建连时那次 HTTP 拉取、也可能来自推送帧，只看 DOM 是分不清的。
          R.firstFrame = e.data;
          R.firstFrameBytes = enc ? enc.encode(e.data).length : e.data.length;
        }
        R.frames++;
        if (enc && typeof e.data === 'string') R.frameBytes = enc.encode(e.data).length;
      });
      return es;
    };
    Wrapped.prototype = Real.prototype;
    window.EventSource = Wrapped;
  })();

  function node(id) { return document.getElementById(id); }
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

  // varColor 在页面上现取一支 CSS 变量的**计算值**（与徽章的计算样式是两条路径）。
  function varColor(d, w, name) {
    var p = d.createElement('span');
    p.style.color = 'var(' + name + ')';
    d.body.appendChild(p);
    var v = w.getComputedStyle(p).color;
    p.parentNode.removeChild(p);
    return v;
  }

  function cardByName(d, name) {
    var grid = d.getElementById('grid');
    if (!grid) return null;
    for (var i = 0; i < grid.children.length; i++) {
      var c = grid.children[i];
      if (c.hidden) continue;
      var n = c.querySelector('.card-name');
      if (n && n.textContent === name) return c;
    }
    return null;
  }

  // layoutDebug 读一遍"宽度是谁定的"：窄屏那条断言一旦红，看这一段就知道
  // 是媒体查询没生效、还是卡片里某个东西的最小内容宽度把列撑开了。
  function layoutDebug(w, d) {
    var grid = d.getElementById('grid');
    var out = {
      innerWidth: w.innerWidth,
      docClientWidth: d.documentElement.clientWidth,
      bodyClientWidth: d.body.clientWidth,
      gridClientWidth: grid ? grid.clientWidth : -1,
      gridScrollWidth: grid ? grid.scrollWidth : -1,
      gridRectWidth: grid ? grid.getBoundingClientRect().width : -1,
      gridCols: grid ? w.getComputedStyle(grid).gridTemplateColumns : '',
      mediaMobile: w.matchMedia('(max-width: 640px)').matches,
      cardW: -1, resW: -1, fadeW: -1, linesW: -1, headW: -1, subW: -1, nameRowW: -1
    };
    var card = grid ? grid.querySelector('.card') : null;
    if (!card) return out;
    function rectW(sel) {
      var el = card.querySelector(sel);
      return el ? el.getBoundingClientRect().width : -1;
    }
    out.cardW = card.getBoundingClientRect().width;
    out.headW = rectW('.card-head');
    out.subW = rectW('.card-sub');
    out.nameRowW = rectW('.card-name-row');
    out.resW = rectW('.card-res');
    out.fadeW = rectW('.card-fade');
    out.linesW = rectW('.card-lines');
    return out;
  }

  // longCard 找那张名字最长的卡片（就是现场里那条 31 字符的中文长名）。
  function longCard(d) {
    var grid = d.getElementById('grid');
    if (!grid) return null;
    var best = null, bestLen = 20;
    for (var i = 0; i < grid.children.length; i++) {
      var n = grid.children[i].querySelector('.card-name');
      if (n && n.textContent.length > bestLen) { best = grid.children[i]; bestLen = n.textContent.length; }
    }
    return best;
  }

  // minContentWidth 用"width: min-content 的离屏盒子"量一个元素的最小内容宽度。
  //
  // 为什么要它：窄屏下网格是 grid-template-columns: 1fr，轨道的下限是 auto
  // ⇒ 等于卡片的最小内容宽度。谁是那个下限，只有各自量一遍才说得清。
  function minContentWidth(d, el) {
    if (!el) return -1;
    var box = d.createElement('div');
    box.style.cssText = 'position:absolute;left:-10000px;top:0;width:min-content';
    box.appendChild(el.cloneNode(true));
    d.body.appendChild(box);
    var w = box.getBoundingClientRect().width;
    box.parentNode.removeChild(box);
    return w;
  }

  // narrowProbe 回答窄屏下唯一真要紧的那个问题：**徽章有没有把卡片顶宽**。
  //
  // 做法是因果实验而不是看数字：把徽章从那张卡片上摘下来、逼一次重排，再量同一个宽度。
  // 宽度不变 ⇒ 撑开列的是别的东西（既有现象，不在这次改动范围内）；
  // 宽度少了徽章那几十像素 ⇒ 就是徽章干的。
  // 顺手再把"到底是谁撑开的"点名：把列压到容器宽度，谁溢出就记谁。
  function narrowProbe(w, d) {
    var out = {
      cardW: -1, cardWNoBadge: -1, gridScrollW: -1, gridScrollWNoBadge: -1,
      squeezed: [], squeezedCardW: -1, subText: '', minContent: {}, minContentNoBadge: {}
    };
    var grid = d.getElementById('grid');
    var card = longCard(d);
    if (!grid || !card) return out;
    var head = card.querySelector('.card-head');
    var sub = card.querySelector('.card-sub');
    out.subText = sub ? sub.textContent : '';
    var sel = {
      card: card, head: head,
      nameRow: card.querySelector('.card-name-row'), name: card.querySelector('.card-name'),
      sub: sub, res: card.querySelector('.card-res'), fade: card.querySelector('.card-fade'),
      lines: card.querySelector('.card-lines'), tags: card.querySelector('.card-tags')
    };
    Object.keys(sel).forEach(function (k) { out.minContent[k] = minContentWidth(d, sel[k]); });
    out.cardW = card.getBoundingClientRect().width;
    out.gridScrollW = grid.scrollWidth;
    var b = card.querySelector('.card-expiry');
    var parent = b ? b.parentNode : null;
    if (b) parent.removeChild(b);
    void card.offsetWidth;
    out.minContentNoBadge = { card: minContentWidth(d, card), head: minContentWidth(d, head) };
    out.cardWNoBadge = card.getBoundingClientRect().width;
    out.gridScrollWNoBadge = grid.scrollWidth;
    var old = grid.style.gridTemplateColumns;
    grid.style.gridTemplateColumns = 'minmax(0, 1fr)';
    void card.offsetWidth;
    out.squeezedCardW = card.getBoundingClientRect().width;
    var all = card.querySelectorAll('*');
    for (var i = 0; i < all.length && out.squeezed.length < 12; i++) {
      var over = all[i].scrollWidth - all[i].clientWidth;
      if (over > 1) {
        out.squeezed.push((all[i].className || all[i].tagName) + '=' + all[i].clientWidth + '+' + over);
      }
    }
    grid.style.gridTemplateColumns = old;
    if (b) parent.appendChild(b);   // 原样放回去（它本来就是 .card-head 的最后一个孩子）
    void card.offsetWidth;
    return out;
  }

  // measure 在给定的 window/document 里读一遍首页上的每张卡片。
  function measure(w, d) {
    var out = { innerWidth: w.innerWidth, gridOverflow: 0, cards: [] };
    var grid = d.getElementById('grid');
    if (!grid) return out;
    out.gridOverflow = grid.scrollWidth - grid.clientWidth;
    Array.prototype.forEach.call(grid.children, function (card) {
      if (card.hidden) return;
      var nameEl = card.querySelector('.card-name');
      var row = card.querySelector('.card-name-row');
      var head = card.querySelector('.card-head');
      var b = card.querySelector('.card-expiry');
      var cs = w.getComputedStyle(card);
      var cr = card.getBoundingClientRect();
      var contentRight = cr.right - parseFloat(cs.borderRightWidth) - parseFloat(cs.paddingRight);
      var rec = {
        name: nameEl ? nameEl.textContent : '?',
        headChildren: head ? head.children.length : -1,
        badgeExists: !!b,
        cardW: cr.width,
        truncatedName: !!(nameEl && nameEl.scrollWidth > nameEl.clientWidth + 1)
      };
      if (b) {
        var bs = w.getComputedStyle(b);
        var br = b.getBoundingClientRect();
        var rr = row ? row.getBoundingClientRect() : null;
        rec.text = b.textContent || '';
        rec.cls = b.className;
        rec.color = bs.color;
        rec.borderColor = bs.borderColor;
        rec.background = bs.backgroundColor;
        rec.parentCls = b.parentNode ? b.parentNode.className : '';
        rec.gap = contentRight - br.right;
        rec.width = br.width;
        rec.sameLine = !!(rr && br.top < rr.bottom && br.bottom > rr.top);
        // 自然宽度：克隆一份放到 body 上量（脱离 flex 的挤压），
        // 与卡片上的实际宽度相等才是"一个像素都不缩"。
        var clone = b.cloneNode(true);
        clone.style.position = 'absolute';
        clone.style.left = '-10000px';
        clone.style.top = '0';
        d.body.appendChild(clone);
        rec.naturalWidth = clone.getBoundingClientRect().width;
        clone.parentNode.removeChild(clone);
      }
      out.cards.push(rec);
    });
    return out;
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

  function namesOf(d) {
    var grid = d ? d.getElementById('grid') : null;
    if (!grid) return '（没有 #grid）';
    var out = [];
    for (var i = 0; i < grid.children.length; i++) {
      var n = grid.children[i].querySelector('.card-name');
      out.push(n ? n.textContent : '?');
    }
    return out.join(' / ');
  }

  // CARDS 是"现场一共有几台机器"（= Go 侧 ebNodes 的条数，改了那边也要改这里）。
  // 判定用的是 >=，但**不能**写小：写小了会在最后几张卡片还没画出来时就往下读，
  // Go 侧那条"量到的卡片数不对"于是变成偶发红。
  var CARDS = 9;

  // 传的是"取文档的函数"而不是文档本身：iframe 在 appendChild 那一刻的 contentDocument
  // 还是那张 about:blank，真文档换了对象之后闭包里抓着的仍然是旧的那张。
  function waitCards(getDoc, n) {
    return waitFor('首页铺开 ' + n + ' 张卡片', function () {
      var d = getDoc();
      var grid = d ? d.getElementById('grid') : null;
      return !!grid && grid.children.length >= n;
    }, 30000).catch(function (err) {
      var d = getDoc();
      var grid = d ? d.getElementById('grid') : null;
      throw new Error(err.message + '（实际 ' + (grid ? grid.children.length : -1) + ' 张：' + namesOf(d) + '）');
    });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(function () { return waitCards(function () { return document; }, CARDS); })
      .then(function () { return sleep(1500); })
      .then(function () {
        R.vars = {
          bad: varColor(document, window, '--bad'),
          warn: varColor(document, window, '--warn'),
          tagBorder: varColor(document, window, '--tag-border')
        };
        R.main = measure(window, document);
        R.topDebug = layoutDebug(window, document);
        R.steps.push('读完 ' + R.main.cards.length + ' 张卡片（浅色）');
      })

      // ① 4 秒观察窗口：同一档位下徽章上的 DOM 变更必须是 0。
      .then(function () {
        var card = cardByName(document, LIVE);
        var b = card ? card.querySelector('.card-expiry') : null;
        if (!b) {
          // 没有徽章就不下结论（也不在这里抛错）：让 Go 侧那条断言报红，
          // 一次跑完能看到全部红点，比在自检脚本里中断清楚。
          R.churn = { mutations: -1, frames: R.frames, sameElement: false, textBefore: '', textAfter: '', missing: true };
          R.steps.push('live 那台（' + LIVE + '）上没有徽章，观察窗口没有意义');
          return true;
        }
        var hits = 0;
        var mo = new MutationObserver(function (list) { hits += list.length; });
        mo.observe(b, { childList: true, characterData: true, subtree: true, attributes: true });
        var frames0 = R.frames;
        var before = b.textContent;
        return sleep(4000).then(function () {
          mo.disconnect();
          var now = card.querySelector('.card-expiry');
          R.churn = {
            mutations: hits,
            frames: R.frames - frames0,
            sameElement: now === b,
            textBefore: before,
            textAfter: now ? now.textContent : ''
          };
          R.steps.push('4 秒窗口：推送帧 ' + R.churn.frames + '，徽章上的 DOM 变更 ' + hits + ' 次');
        });
      })

      // ② 深色主题：同一批卡片再读一遍（走 app.js 自己那条 data-theme 分支）。
      .then(function () {
        document.documentElement.dataset.theme = 'dark';
        return sleep(400);
      })
      .then(function () {
        R.darkVars = {
          bad: varColor(document, window, '--bad'),
          warn: varColor(document, window, '--warn'),
          tagBorder: varColor(document, window, '--tag-border')
        };
        R.dark = measure(window, document);
        R.steps.push('读完 ' + R.dark.cards.length + ' 张卡片（深色）');
        delete document.documentElement.dataset.theme;
        return sleep(200);
      })

      // ③ 窄屏：同源 iframe 拿到一个真的 NARROW 视口（媒体查询因此真的生效）。
      .then(function () {
        var f = document.createElement('iframe');
        f.style.cssText = 'position:fixed;left:-9999px;top:0;border:0;width:' + NARROW + 'px;height:900px';
        f.src = '/';   // iframe 的视口就是它的宽度，媒体查询因此是真的按 NARROW 生效
        document.body.appendChild(f);
        return waitCards(function () { return f.contentDocument; }, CARDS)
          .then(function () { return sleep(800); })
          .then(function () {
            R.narrow = measure(f.contentWindow, f.contentDocument);
            R.narrowDebug = layoutDebug(f.contentWindow, f.contentDocument);
            R.narrowProbe = narrowProbe(f.contentWindow, f.contentDocument);
            R.steps.push('读完窄屏（' + R.narrow.innerWidth + 'px 视口）的卡片');
          });
      })

      // ④ 续期：把 live 那台改到 60 天之后，等徽章从 DOM 里消失。
      .then(function () {
        return rawFetch('/__renew', { method: 'POST' }).then(function (res) {
          if (!res.ok) throw new Error('/__renew 返回 ' + res.status);
          R.steps.push('已请求把 ' + LIVE + ' 续期到 60 天之后');
        });
      })
      .then(function () {
        return waitFor('续期后徽章被摘掉', function () {
          var c = cardByName(document, LIVE);
          return !!c && !c.querySelector('.card-expiry');
        }, 20000);
      })
      .then(function () { return sleep(1200); })
      .then(function () {
        var c = cardByName(document, LIVE);
        R.afterRenew = {
          badge: !!(c && c.querySelector('.card-expiry')),
          headChildren: c ? c.querySelector('.card-head').children.length : -1,
          frames: R.frames,
          cards: measure(window, document).cards
        };
        R.steps.push('续期后 DOM 里已经没有徽章了');
      });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:expiry'; });
  }

  // shot 模式（人工截图用）：把首页开到八张卡片都要在的状态就停住，**不回传结果**
  // —— 与仓库里其它截图用例同一套做法（截图靠 --virtual-time-budget + --screenshot）。
  function shot() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(function () {
        if (CFG.theme === 'dark') document.documentElement.dataset.theme = 'dark';
        return true;
      })
      .then(login)
      .then(function () { return waitCards(function () { return document; }, CARDS); })
      .then(function () {
        document.title = 'SHOT-READY:expiry';
        return true;
      });
  }

  window.addEventListener('load', function () {
    if (CFG.shot) {
      shot().catch(function (err) {
        document.title = 'SHOT-FAILED:' + String(err && err.message ? err.message : err);
      });
      return;
    }
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`
