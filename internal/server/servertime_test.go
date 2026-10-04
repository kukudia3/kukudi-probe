package server

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 相对时间的基准必须是**服务端时钟**，不是浏览器本地时钟。
//
// 用户报的现场：他自己的电脑快了 10 秒，卡片上的「最后通信」就一直写着"10 秒前"，
// 永远到不了「刚刚」（门槛 5 秒）—— 而数据其实每秒都在到。根因是 fmtAgo 用
// `Date.now() - last_seen`：被减数是**服务端**记下的时刻，减数却是浏览器那一秒。
//
// 这一组用例钉住三件事：
//  1. 三个带 server 块的接口都下发 server_now，而且它就是服务端自己的 unix 秒；
//  2. SSE 帧的 ts（前端每秒对表用的那个字段）同样是服务端 unix 秒；
//  3. 前端**没有**任何"算相对时间却读本地时钟"的地方（逐条白名单，见下）。
//
// 真浏览器那一遍（把浏览器时钟改歪 ±10 秒，显示仍然是「刚刚」；撤掉改动则显示
// "10 秒前"）是一次性实测，原始数据见 _audit/ROUND6-SERVERTIME.md —— 它不留在
// e2e 里：Chrome 那一遍要几十秒，而 e2e 整包的时限已经很紧（见那份报告）。
func TestServerNowIsServerUnixSeconds(t *testing.T) {
	h := newAuthHarness(t)
	id, _ := createNodeOverHTTP(t, h, "clock-01")

	cases := []struct {
		name string
		path string
		// 这份响应里还有一个 RFC3339 的 server.time：两个字段必须来自**同一个**
		// time.Now()，否则同一份响应里就有两个"现在"，前端与页面各说各话。
		hasRFC3339 bool
	}{
		{"节点列表", "/api/v1/nodes", true},
		{"节点详情", "/api/v1/nodes/" + strconv.FormatInt(id, 10), true},
		{"设置", "/api/v1/settings", false},
	}
	for _, tc := range cases {
		before := time.Now().Unix()
		status, body := h.get(t, tc.path)
		after := time.Now().Unix()
		if status != http.StatusOK {
			t.Fatalf("%s：HTTP %d %v", tc.name, status, body)
		}
		server, _ := body["server"].(map[string]any)
		if server == nil {
			t.Fatalf("%s：响应里没有 server 块：%v", tc.name, body)
		}
		raw, ok := server["server_now"]
		if !ok {
			t.Fatalf("%s：server 块里没有 server_now（相对时间没有基准可对）：%v", tc.name, server)
		}
		// JSON 里的数字解出来是 float64：必须是**整秒**（毫秒、纳秒或字符串都不行，
		// last_seen / ts 都是整秒，混口径就会算出差了三个数量级的差值）。
		sec, isNum := raw.(float64)
		if !isNum {
			t.Fatalf("%s：server_now 不是数字：%#v", tc.name, raw)
		}
		if sec != math.Trunc(sec) {
			t.Errorf("%s：server_now = %v 不是整秒（与 last_seen / ts 的口径不一致）", tc.name, sec)
		}
		got := int64(sec)
		if got < before || got > after {
			t.Errorf("%s：server_now = %d 不在这次请求的 [%d, %d] 之间 —— 它不是服务端自己的 unix 秒",
				tc.name, got, before, after)
		}
		if !tc.hasRFC3339 {
			continue
		}
		text, _ := server["time"].(string)
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			t.Errorf("%s：server.time 不是 RFC3339：%q", tc.name, text)
			continue
		}
		if parsed.Unix() != got {
			t.Errorf("%s：server.time = %s（unix %d）与 server_now = %d 不是同一秒："+
				"同一份响应里的两个时间字段必须来自同一个 time.Now()", tc.name, text, parsed.Unix(), got)
		}
	}

	// 访客那份也必须带：访客看到的正是同一批卡片、同一行「最后时刻」。
	// （"访客的 server 块里只有哪几个键"由 guest_test.go 钉着，这里只钉值。）
	guestOn(t, h)
	h.anonymousClient(t)
	before := time.Now().Unix()
	status, body := h.get(t, "/api/v1/nodes")
	after := time.Now().Unix()
	if status != http.StatusOK {
		t.Fatalf("访客读取节点失败: %d", status)
	}
	server, _ := body["server"].(map[string]any)
	raw, ok := server["server_now"]
	if !ok {
		t.Fatalf("访客的 server 块里没有 server_now：%v", server)
	}
	sec, isNum := raw.(float64)
	if !isNum {
		t.Fatalf("访客的 server_now 不是数字：%#v", raw)
	}
	if got := int64(sec); got < before || got > after {
		t.Errorf("访客的 server_now = %d 不在 [%d, %d] 之间", got, before, after)
	}
}

// SSE 帧的 ts 是前端**每秒**对表用的那个字段（见 app.js 的 applyPayload），
// 它必须是服务端 unix 秒：这一条坏了，相对时间就会退回浏览器时钟（那正是要修的 bug），
// 而页面上一点异常都看不出来。
func TestSSEFrameTSCarriesServerClock(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "clock-01")

	before := time.Now().Unix()
	reader, stop := startSSE(t, h)
	defer stop()
	// 建连后的第一帧是服务端的**全量快照**（见 handleStream），立刻就会到。
	frame := reader.next(t, 5*time.Second)
	after := time.Now().Unix()

	raw, ok := frame["ts"]
	if !ok {
		t.Fatalf("SSE 帧里没有 ts：相对时间的基准就没有来源（%v）", frame)
	}
	sec, isNum := raw.(float64)
	if !isNum {
		t.Fatalf("SSE 帧的 ts 不是数字：%#v", raw)
	}
	if sec != math.Trunc(sec) {
		t.Errorf("SSE 帧的 ts = %v 不是整秒（与 last_seen 的口径不一致）", sec)
	}
	if got := int64(sec); got < before || got > after {
		t.Errorf("SSE 帧的 ts = %d 不在 [%d, %d] 之间 —— 它不是服务端自己的 unix 秒", got, before, after)
	}
}

// jsEnclosingFuncs 返回 js 里每一处 needle **所在**的那个函数名。
//
// 判据是"往前找最近的一行 `  function name(`"：本仓库的函数都是两空格缩进定义的，
// 静态断言只需要这个粒度，不必真去解析 JS（与 funcBody 同一套取舍）。
// 找不出归属（文件顶层的语句）时返回空串，由调用方当失败处理。
func jsEnclosingFuncs(js, needle string) []string {
	def := regexp.MustCompile(`^  function ([A-Za-z0-9_]+)\(`)
	owner := ""
	var out []string
	for _, line := range strings.Split(js, "\n") {
		if m := def.FindStringSubmatch(line); m != nil {
			owner = m[1]
		}
		if strings.Contains(line, needle) {
			out = append(out, owner)
		}
	}
	return out
}

// TestFrontendRelativeTimeUsesServerClock 钉住前端的口径：
// **显示"多久以前"一律以服务端时间为基准**，而"两次本地事件之间的间隔"继续用本地时钟。
//
// 这两类必须分开，不能一刀切：
//   - 相对时间显示 = 两个**绝对时刻**相减，而被减数是服务端记的 ⇒ 本地表快了就凭空多出几秒；
//   - 间隔 = 两次**本地事件**之间隔了多久 ⇒ 时钟快慢不影响差值，把它换成服务端基准反而
//     会把网络往返的抖动算进去（服务端的"现在"只在一帧到达时才更新一次）。
func TestFrontendRelativeTimeUsesServerClock(t *testing.T) {
	js := stripJSComments(readAsset(t, "app.js"))

	// ① 「多久以前」的格式化函数必须走 serverNow()，不许再读本地时钟。
	ago := funcBody(js, "function fmtAgo(")
	if ago == "" {
		t.Fatal("app.js 里找不到 fmtAgo()")
	}
	if !strings.Contains(ago, "serverNow()") {
		t.Error("fmtAgo 必须以 serverNow() 为基准：用 Date.now() 的话，用户电脑快 10 秒就会把" +
			"刚收到的数据写成\"10 秒前\"，永远到不了「刚刚」")
	}
	if strings.Contains(ago, "Date.now()") {
		t.Error("fmtAgo 里不该再出现 Date.now()：本地时钟不是基准（见 app.js 的「服务端时钟基准」）")
	}

	// ② serverNow() 的退化路径：还没拿到过 server_now 时退回本地时钟，
	//    而不是算出 NaN / 0（老服务端 + 新前端必须与改动前完全一样）。
	now := funcBody(js, "function serverNow(")
	if now == "" {
		t.Fatal("app.js 里找不到 serverNow()")
	}
	if !strings.Contains(now, "clockOffsetMs === null") {
		t.Error("serverNow() 必须显式判断\"还没拿到过 server_now\"（clockOffsetMs === null）并退回本地时钟")
	}
	if !strings.Contains(now, "Math.floor(Date.now() / 1000)") {
		t.Error("serverNow() 的退化路径必须是本地时钟取整秒（老服务端 + 新前端时行为与改动前一致）")
	}
	if !strings.Contains(now, "clockOffsetMs)") {
		t.Error("serverNow() 拿到过 server_now 时要把偏移加回去（clockOffsetMs）")
	}

	// ③ 偏移必须真的被记下来（把 server_now 读进来又丢掉 = 白加一个字段）。
	note := funcBody(js, "function noteServerNow(")
	if note == "" {
		t.Fatal("app.js 里找不到 noteServerNow()")
	}
	if !strings.Contains(note, "clockOffsetMs = sec * 1000 - Date.now()") {
		t.Error("noteServerNow 要把偏移记进 clockOffsetMs（服务端秒 − 本地毫秒），否则 serverNow() 永远没有基准")
	}
	if !strings.Contains(note, "isFinite(sec)") || !strings.Contains(note, "sec <= 0") {
		t.Error("noteServerNow 要挡掉不合理的时间值（字段缺失 / 被改坏）：坏值不该污染已经对好的表")
	}

	// ④ 两条来源都要接上：每个 HTTP 响应的唯一出口 api()，以及每秒一帧的 SSE。
	apiFn := funcBody(js, "function api(")
	if apiFn == "" {
		t.Fatal("app.js 里找不到 api()")
	}
	if !strings.Contains(apiFn, "noteServerNow(data.server.server_now)") {
		t.Error("api() 要把响应里的 server.server_now 拿去对表：放在这个唯一出口上，新加的取数路径才不会漏")
	}
	payload := funcBody(js, "function applyPayload(")
	if payload == "" {
		t.Fatal("app.js 里找不到 applyPayload()")
	}
	if !strings.Contains(payload, "noteServerNow(payload.ts)") {
		t.Error("applyPayload 要拿 SSE 帧的 ts 对表：SSE 每秒一帧，偏移因此每秒都在跟（时钟慢慢漂也能跟回来）")
	}

	// ⑤ ★ 全仓白名单：app.js 里**每一处** Date.now() 都必须落在下面这两类里，
	//    每一类都写明理由。多出一处就红 —— "用本地时钟算要显示给人看的相对时间"
	//    正是这次修掉的 bug，而它坏起来是静默的（页面照常渲染，只是差几秒）。
	//
	//    窄屏/宽屏的样式一个字都没动，这条断言与那件事无关：它管的是 JS 里的取值口径。
	want := map[string]string{
		"serverNow":        "对表本身：还没拿到 server_now 时的退化路径",
		"noteServerNow":    "对表本身：算偏移",
		"loadOverview":     "间隔：\"总览停在多久以前的数据上\"只进 console.warn，不进界面",
		"liveRatesFresh":   "间隔：最近一次收帧到现在（时钟快慢不影响间隔）",
		"liveWatchdogTick": "间隔：看门狗判\"多久没收到帧\"",
		"connectStream":    "间隔：记下建连时刻与收帧时刻",
		"renderTwoFA":      "不是时间：二维码 URL 的防缓存参数，要的只是\"每次都不同\"",
	}
	got := jsEnclosingFuncs(js, "Date.now()")
	if len(got) == 0 {
		t.Fatal("app.js 里一处 Date.now() 都没有？那这条白名单断言就没有判别力了")
	}
	for _, fn := range got {
		if _, ok := want[fn]; !ok {
			where := fn
			if where == "" {
				where = "（函数外）"
			}
			t.Errorf("app.js 的 %s 里出现了 Date.now()，而它不在白名单里。"+
				"若这一处算的是要**显示给人看**的相对时间，它必须走 serverNow()（用户电脑快 10 秒时"+
				"否则会写成\"10 秒前\"）；若确实是\"两次本地事件之间的间隔\"，请连同理由加进 want", where)
		}
	}
	for fn, why := range want {
		found := false
		for _, g := range got {
			if g == fn {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("白名单里的 %s()（%s）已经不再用 Date.now()：请把这一行从 want 里删掉 —— "+
				"清单必须与代码一致，否则它会慢慢变成一份没人核对的名单", fn, why)
		}
	}

	// ⑥ 显示相对时间的地方一共三处，全部经由 fmtAgo（同一个函数 = 同一套口径）。
	//    多出一处就红：新加一处"多久以前"必须有人停下来想想它的基准。
	callers := jsEnclosingFuncs(js, "fmtAgo(")
	gotSet := map[string]bool{}
	for _, fn := range callers {
		gotSet[fn] = true
	}
	// fmtAgo 自己的定义行也会被算进来（`function fmtAgo(unixSec) {`）。
	for _, fn := range []string{"fmtAgo", "updateCard", "renderDetailInfo", "fxRows"} {
		if !gotSet[fn] {
			t.Errorf("app.js 里找不到 %s 对 fmtAgo 的调用：三处显示相对时间的地方（首页卡片「最后通信」、"+
				"详情页「最后通信」、设置页「汇率取得于」）少了一处，或者改了名字 —— 请重新核对这份清单", fn)
		}
	}
	for fn := range gotSet {
		if fn != "fmtAgo" && fn != "updateCard" && fn != "renderDetailInfo" && fn != "fxRows" {
			t.Errorf("app.js 的 %s() 也在调 fmtAgo：全仓显示相对时间的地方本来是这三处 + 定义一处，"+
				"新加的这处同样以服务端时钟为基准（fmtAgo 内部统一），但请把它的名字补进这份清单", fn)
		}
	}
}
