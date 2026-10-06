package chart

// 延迟图例那几张卡片上的「丢包」必须**无条件**写出来 —— 真的是 0% 也要写「丢包 0%」。
//
// 用户的原话：「我希望图例里面的浙江电信也展示丢包，不要因为没有丢包而不展示了」，
// 随后又把范围补成「所有图例都改」。截图里三张卡片长这样：
//
//	浙江电信  138 ms · 峰值 256 ms              ← 少一段，因为它的丢包率是 0
//	浙江联通  144 ms · 峰值 192 ms · 丢包 0.2%
//	浙江移动  135 ms · 峰值 381 ms · 丢包 0.1%
//
// 所以这条用例摆出**三种情况各一张卡片**，一趟真浏览器（真服务端 + 真 Chrome，
// 页面走真接口拿真数据）全部量掉：
//
//	① 丢包 0%   —— 有探测记录、一个包都没丢（has_data = true、loss_pct = 0）
//	② 丢包 >0%  —— 有探测记录、丢了一点（has_data = true、loss_pct > 0）
//	③ 没有数据  —— 这一段压根没有探测记录（has_data = false）
//
// ★ ②③ 与 ① 的区别是这条用例的另一半：**"没有数据"绝不许显示成 0%**。
// 丢包是 0 说明"测了，一个都没丢"；没有数据说明"没测到" —— 两者在数据里由服务端的
// has_data 分开（见 internal/server/api_ping.go 的 pingTargetSeries、
// store.QueryPingSeries 的 out.HasData = len(out.Points) > 0），前端拿这个字段早退成
// 「暂无数据」，所以**不用动接口形状**。这条用例把三张卡片的**整行文案**逐字钉死
// （期望值由 /ping 的返回值按同一套口径算出来，不是写死的数字），谁要是把"没有数据"
// 也写成「丢包 0%」，第 ③ 张卡片立刻红。
//
// 为什么必须在真浏览器里量（静态断言不够）：
//   - 多出来的那一段「· 丢包 0%」是**画出来**的东西，静态断言只能证明代码形状；
//   - 窄屏下三张卡片是 grid 的 auto-fit 列（minmax(200px, 1fr)），多一段文字会不会
//     把统计行挤成两行、会不会横向溢出、卡片会不会被顶出容器，只有真布局知道。
//     所以下面用 CDP 把视口**精确**定死成几档，逐张卡片量"统计行占了几行、
//     最宽那一行多宽、可用宽度多少、有没有溢出"。
//
// 反向验证（改了才绿、撤了就红）：把 web/app.js 里那句无条件拼接改回
// `if (t.loss_pct > 0) text += …`，① 的文案断言（checkLegendText 里那一行）
// 与悬浮读数那一条会立刻变红 —— 逐行号见 _audit/ROUND6-LATENCY-LEGEND.md。
//
// 截图（默认不产出，只在设了 PROBE_SHOT_DIR 时写盘）：
//
//	$env:PROBE_SHOT_DIR = "D:\DEEPSEEK\_audit"; go test ./internal/e2e/chart/ -run TestLatencyLegendAlwaysShowsLoss -v
//
// 产出 latency-legend-0.png / latency-legend-有丢包.png / latency-legend-无数据.png
// （三张卡片各自的近景）+ latency-legend-整行.png + latency-legend-窄屏.png。
// 同时还会写两份"整棵子树快照"（样式 + 矩形，逐行 diff 用）：
// latency-legend-snapshot-<label>-1500.txt 与 …-760.txt，label 取 PROBE_SHOT_LABEL。

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/e2e/harness"
	"probe/internal/store"
)

// 三个探测目标的名字就用用户截图里的那三个：万一要出图给用户看，图上认得出。
const (
	legendZeroLabel = "浙江电信" // 丢包 0%
	legendLossLabel = "浙江联通" // 丢包 > 0
	legendNoneLabel = "浙江移动" // 没有数据
)

// 三张卡片上的数字取自用户截图那一档（138/256、144/192）：位数一样，
// 于是"0%"那张卡片的文字严格比">0%"那张短两个字符 —— 下面那条
// "行数不许比有丢包的那张多"的断言因此是有意义的（不是靠数字位数混过去的）。
const (
	legendZeroAvg = 138.0
	legendZeroMax = 256.0
	legendLossAvg = 144.0
	legendLossMax = 192.0
)

// legendFixture 是一条用例要的那点现场：真服务端 + 三个目标 + 一段铺好的曲线。
type legendFixture struct {
	h      *harness.Harness
	br     *harness.Browser
	nodeID int64
	ids    map[string]int64
}

func startLegendFixture(t *testing.T) *legendFixture {
	t.Helper()
	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"),
		time.UTC, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))
	nodeID, _ := harness.CreateNodeViaAPI(t, br, "zj-legend")

	ids := createLegendTargets(t, br)
	// ① 与 ② 铺**同一形状**的曲线（58 个 1 分钟桶，全部落在 1h 档窗口里），
	//    只有丢包不同：① 一个包都没丢，② 中间两个桶各丢了 6%。
	seedLegendSeries(t, h, nodeID, ids[legendZeroLabel], legendZeroAvg, legendZeroMax, nil)
	seedLegendSeries(t, h, nodeID, ids[legendLossLabel], legendLossAvg, legendLossMax,
		map[int]int64{20: 94, 40: 94})
	// ③ 一行都不写：has_data 为假。
	return &legendFixture{h: h, br: br, nodeID: nodeID, ids: ids}
}

// createLegendTargets 一次 PUT 三个目标（PUT 是整体替换语义，分三次调只会剩最后一个）。
func createLegendTargets(t *testing.T, br *harness.Browser) map[string]int64 {
	t.Helper()
	labels := []string{legendZeroLabel, legendLossLabel, legendNoneLabel}
	targets := make([]map[string]any, 0, len(labels))
	for i, label := range labels {
		targets = append(targets, map[string]any{
			"label": label, "type": "tcp",
			"host": "202.96.96." + strconv.Itoa(i+1), "port": 443, "enabled": true,
		})
	}
	status, body := br.Do("PUT", "/api/v1/settings/ping",
		map[string]any{"interval_sec": 60, "targets": targets}, true)
	if status != 200 {
		t.Fatalf("保存三个探测目标失败: %d %v", status, body)
	}
	list, _ := body["targets"].([]any)
	if len(list) != len(labels) {
		t.Fatalf("探测目标数量 = %d，期望 %d：%v", len(list), len(labels), body)
	}
	ids := make(map[string]int64, len(labels))
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		label, _ := item["label"].(string)
		id, _ := item["id"].(float64)
		if id <= 0 {
			t.Fatalf("目标 %q 没有返回 id: %v", label, item)
		}
		ids[label] = int64(id)
	}
	for _, label := range labels {
		if ids[label] == 0 {
			t.Fatalf("接口没返回目标 %q 的 id：%v", label, body)
		}
	}
	return ids
}

// seedLegendSeries 铺一段**完全落在 1h 档窗口内**的 1 分钟桶。
//
// 窗口是 [end-3600, end)、end = now 向下对齐到分钟（见 store.PingRange.window），
// 所以只铺 [end-3480, end-60]：两端各留一分钟余量，查询窗口怎么对齐都切不掉首尾点
// （切掉了丢包率就不是那 2/58 了，期望值会跟着漂）。
//
// lossAt 是"第几个桶丢了多少"（下标 → up_cnt，100 表示一个都没丢）：整体丢包率是
// SUM(up_cnt)/SUM(all_cnt)（服务端口径，见 store.QueryPingSeries），于是
// 两个桶各丢 6% 得到 (56*100+2*94)/5800 = 0.2069% —— 一位小数正是截图里的 0.2%。
func seedLegendSeries(t *testing.T, h *harness.Harness, nodeID, targetID int64, avg, peak float64, lossAt map[int]int64) {
	t.Helper()
	now := time.Now()
	end := now.Unix() - now.Unix()%60
	const buckets = 58
	out := make([]store.PingBucket, 0, buckets)
	for i := 0; i < buckets; i++ {
		ts := end - 3480 + int64(i)*60
		up := int64(100)
		if lossAt != nil {
			if v, ok := lossAt[i]; ok {
				up = v
			}
		}
		loss := float64(100-up) / 100 * 100
		out = append(out, store.NewPingBucket(nodeID, targetID, ts, avg, avg-5, peak, loss))
	}
	if err := h.DB.UpsertPingBuckets(context.Background(), out); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	t.Logf("目标 %d：铺了 %d 个桶（步长 60 秒，avg %.0f ms、峰值 %.0f ms）", targetID, len(out), avg, peak)
}

// ---------------------------------------------------------------- 观测值

// legendCardObs 是一张卡片在**当前视口**下的读数。
type legendCardObs struct {
	Name  string `json:"name"`
	Stats string `json:"stats"`
	// Lines 是统计行**占了几行**：Range.getClientRects() 一个行盒一个矩形
	// （display:block 的元素自己只会给一个整块矩形，量不出折行）。
	Lines int `json:"lines"`
	// Widest 是最宽那一行的宽度；Avail 是可用内容宽度；Ratio = Widest/Avail。
	Widest float64 `json:"widest"`
	Avail  float64 `json:"avail"`
	Ratio  float64 `json:"ratio"`
	// ScrollW/ClientW 是统计行的横向溢出判据（> 就是被裁了）。
	ScrollW float64 `json:"scrollW"`
	ClientW float64 `json:"clientW"`
	CardW   float64 `json:"cardW"`
	CardH   float64 `json:"cardH"`
	// CardTop/CardLeft 用来判"三张还在同一行"（窄屏下 grid 会不会折列）。
	CardTop     float64 `json:"cardTop"`
	CardLeft    float64 `json:"cardLeft"`
	CardScrollW float64 `json:"cardScrollW"`
	CardClientW float64 `json:"cardClientW"`
	FontSize    float64 `json:"fontSize"`
}

// legendObservation 是一档视口下的全部读数。
type legendObservation struct {
	Viewport int             `json:"viewport"`
	Cards    []legendCardObs `json:"cards"`
}

// readLegendCardsJS 量当前视口下的三张卡片。整段是同步的，返回一个数组。
const readLegendCardsJS = `(function () {
  var box = document.getElementById('lat-targets');
  if (!box) return null;
  var out = [];
  Array.prototype.forEach.call(box.querySelectorAll('.lat-card'), function (card) {
    var stats = card.querySelector('.lat-card-stats');
    var name = card.querySelector('.lat-card-name');
    var cs = getComputedStyle(stats);
    var padL = parseFloat(cs.paddingLeft) || 0;
    var padR = parseFloat(cs.paddingRight) || 0;
    var avail = stats.clientWidth - padL - padR;
    var range = document.createRange();
    range.selectNodeContents(stats);
    var rects = range.getClientRects();
    var lines = 0, widest = 0;
    for (var i = 0; i < rects.length; i++) {
      if (rects[i].width <= 0) continue;
      lines++;
      if (rects[i].width > widest) widest = rects[i].width;
    }
    var cr = card.getBoundingClientRect();
    out.push({
      name: name ? name.textContent : '',
      stats: stats.textContent,
      lines: lines,
      widest: widest,
      avail: avail,
      ratio: avail > 0 ? widest / avail : 0,
      scrollW: stats.scrollWidth,
      clientW: stats.clientWidth,
      cardW: cr.width,
      cardH: cr.height,
      cardTop: cr.top,
      cardLeft: cr.left,
      cardScrollW: card.scrollWidth,
      cardClientW: card.clientWidth,
      fontSize: parseFloat(cs.fontSize) || 0
    });
  });
  return out;
})()`

// measureLegend 把视口定死成 w×h 之后量一次卡片读数（等布局稳下来再读）。
//
// 为什么要等：换视口会触发 resize —— 延迟图在手机断点下还会做一次二次聚合，
// 卡片可能被重建。等到**连续两次读数完全一样**再返回，免得量到一个中间态。
func measureLegend(t *testing.T, s *cdpSession, w, h int) legendObservation {
	t.Helper()
	s.setViewport(w, h)
	var prev string
	deadline := time.Now().Add(15 * time.Second)
	var obs legendObservation
	for time.Now().Before(deadline) {
		obs = legendObservation{Viewport: w}
		s.evalJSON(readLegendCardsJS, &obs.Cards)
		raw := fmt.Sprintf("%+v", obs.Cards)
		if raw == prev && len(obs.Cards) > 0 {
			return obs
		}
		prev = raw
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("视口 %d×%d 下的卡片读数一直没稳下来（最后一次：%+v）", w, h, obs.Cards)
	return obs
}

// findCard 按名字取一张卡片的读数。
func findCard(t *testing.T, obs legendObservation, name string) legendCardObs {
	t.Helper()
	for _, c := range obs.Cards {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%d 宽的视口下没有找到卡片 %q（读到的是 %v）", obs.Viewport, name, legendNamesOf(obs))
	return legendCardObs{}
}

func legendNamesOf(obs legendObservation) []string {
	out := make([]string, 0, len(obs.Cards))
	for _, c := range obs.Cards {
		out = append(out, c.Name)
	}
	return out
}

// ---------------------------------------------------------------- 期望文案

// legendExpectText 按 web/app.js 的 latTargetText 口径拼出**期望**的那一行。
//
// 这里刻意把口径再写一遍：用例要钉的是"页面上写着什么"，期望值必须独立于被测
// 实现 —— 直接从 app.js 里抄一份格式过去就等于把断言写成同义反复。数字本身来自
// /ping 的返回值（见 readLegendAPI），不是写死的常量。
func legendExpectText(hasData bool, avgMS, peakMS, lossPct float64) string {
	if !hasData {
		return "暂无数据"
	}
	text := "—"
	if avgMS > 0 {
		text = strconv.FormatFloat(math.Round(avgMS), 'f', 0, 64) + " ms"
	}
	peak := "—"
	if peakMS > 0 {
		peak = strconv.FormatFloat(math.Round(peakMS), 'f', 0, 64) + " ms"
	}
	// 丢包那一段无条件写：0 沿用全站已有的写法「0%」（迷你条的行首读数就是这么写的，
	// 见 web/app.js 的 miniLossText 与 fmtPct1 的注释：写 0.0% 反而像没测到）。
	loss := "0%"
	if lossPct > 0 {
		loss = strconv.FormatFloat(lossPct, 'f', 1, 64) + "%"
	}
	return text + " · 峰值 " + peak + " · 丢包 " + loss
}

// legendAPIValues 是一个目标在 /ping 里的那份聚合值。
//
// 为什么让用例自己去问接口：卡片上的数字全部由服务端算好（前端不做算术），
// 所以"页面上写的那一行"必须等于"接口给的那一份按同一套格式拼出来的那一行"。
type legendAPIValues struct {
	HasData bool    `json:"has_data"`
	Label   string  `json:"label"`
	AvgMS   float64 `json:"avg_ms"`
	PeakMS  float64 `json:"peak_ms"`
	LossPct float64 `json:"loss_pct"`
}

func readLegendAPI(t *testing.T, f *legendFixture) map[string]legendAPIValues {
	t.Helper()
	status, body := f.br.Do("GET", "/api/v1/nodes/"+strconv.FormatInt(f.nodeID, 10)+"/ping?range=1h", nil, false)
	if status != 200 {
		t.Fatalf("读 /ping 失败: %d %v", status, body)
	}
	list, _ := body["targets"].([]any)
	if len(list) != 3 {
		t.Fatalf("/ping 返回 %d 个目标，期望 3：%v", len(list), body)
	}
	out := make(map[string]legendAPIValues, len(list))
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		if label, ok := item["label"].(string); ok {
			get := func(key string) float64 { v, _ := item[key].(float64); return v }
			out[label] = legendAPIValues{
				HasData: item["has_data"] == true, Label: label,
				AvgMS: get("avg_ms"), PeakMS: get("peak_ms"), LossPct: get("loss_pct"),
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- 用例

// TestLatencyLegendAlwaysShowsLoss 是这一轮的主用例（真服务端 + 真 Chrome）。
func TestLatencyLegendAlwaysShowsLoss(t *testing.T) {
	chrome := harness.FindChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	f := startLegendFixture(t)
	api := readLegendAPI(t, f)

	// ---- 前提先钉死：三种情况的数据真的在库里，而且真的分得开 ----
	zero, loss, none := api[legendZeroLabel], api[legendLossLabel], api[legendNoneLabel]
	if !zero.HasData || zero.LossPct != 0 {
		t.Fatalf("%s 的前提不对：has_data=%v loss_pct=%v，要的是「有数据且一个包都没丢」"+
			"（否则这条用例证明不了 0%% 也会显示）", legendZeroLabel, zero.HasData, zero.LossPct)
	}
	if !loss.HasData || loss.LossPct <= 0 {
		t.Fatalf("%s 的前提不对：has_data=%v loss_pct=%v，要的是「有数据且丢了包」",
			legendLossLabel, loss.HasData, loss.LossPct)
	}
	if none.HasData {
		t.Fatalf("%s 的前提不对：has_data=%v，要的是「这一段压根没有探测记录」", legendNoneLabel, none.HasData)
	}

	wantZero := legendExpectText(zero.HasData, zero.AvgMS, zero.PeakMS, zero.LossPct)
	wantLoss := legendExpectText(loss.HasData, loss.AvgMS, loss.PeakMS, loss.LossPct)
	wantNone := legendExpectText(none.HasData, none.AvgMS, none.PeakMS, none.LossPct)
	t.Logf("接口给的聚合值：%s {has_data=%v avg=%.2f peak=%.2f loss=%.4f} → 期望 %q",
		legendZeroLabel, zero.HasData, zero.AvgMS, zero.PeakMS, zero.LossPct, wantZero)
	t.Logf("                %s {has_data=%v avg=%.2f peak=%.2f loss=%.4f} → 期望 %q",
		legendLossLabel, loss.HasData, loss.AvgMS, loss.PeakMS, loss.LossPct, wantLoss)
	t.Logf("                %s {has_data=%v} → 期望 %q", legendNoneLabel, none.HasData, wantNone)

	// ---- 起 Chrome（布局视口由 CDP 精确定死，不用 --window-size）----
	pageURL := "http://" + f.h.Addr + "/"
	s := startCDPSession(t, chrome, pageURL, 1500, 1180)
	loginLegendPage(t, s)
	s.eval("window.location.hash = '#/n/" + strconv.FormatInt(f.nodeID, 10) + "'")
	s.waitJS("详情页的延迟图例画出三张卡片", legendReadyJS, 60*time.Second)

	// ---- ① 三种情况的整行文案（宽屏）----
	wide := measureLegend(t, s, 1500, 1180)
	if len(wide.Cards) != 3 {
		t.Fatalf("延迟图例的卡片数 = %d，期望 3（%v）", len(wide.Cards), legendNamesOf(wide))
	}
	checkLegendText(t, wide, legendZeroLabel, wantZero)
	checkLegendText(t, wide, legendLossLabel, wantLoss)
	checkLegendText(t, wide, legendNoneLabel, wantNone)

	// 用户要的那一条，单独再钉一次：0% 必须**真的出现在屏幕上**，而且旁边那张
	// 没有数据的卡片**不许**出现「丢包」或「0%」（这是这次改动最容易踩错的地方）。
	gotZero := findCard(t, wide, legendZeroLabel).Stats
	if !strings.Contains(gotZero, "丢包 0%") {
		t.Errorf("%s 的卡片 = %q，里面没有「丢包 0%%」：丢包率为 0 时也要照实写出来"+
			"（用户原话「不要因为没有丢包而不展示」）", legendZeroLabel, gotZero)
	}
	gotNone := findCard(t, wide, legendNoneLabel).Stats
	if strings.Contains(gotNone, "丢包") || strings.Contains(gotNone, "0%") {
		t.Errorf("%s 的卡片 = %q：这一段**没有探测记录**，绝不能写成「丢包 0%%」"+
			"（没有数据 ≠ 一个包都没丢）", legendNoneLabel, gotNone)
	}

	// ---- ② 窄屏：三张卡片的行宽 / 有没有挤、折行、溢出 ----
	//
	// 1500：宽屏三列；760：三列但每张只剩 ~220px（auto-fit 的 minmax(200px, 1fr)）；
	// 600：落在 max-width: 640 的移动断点上，改单列。
	outDir := os.Getenv("PROBE_SHOT_DIR")
	checkLegendGeometry(t, "宽屏 1500", wide)
	checkLegendRow(t, "宽屏 1500", wide, 1)
	narrow := measureLegend(t, s, 760, 1000)
	checkLegendGeometry(t, "窄屏 760", narrow)
	checkLegendRow(t, "窄屏 760", narrow, 1)
	legendNarrowShot(t, s, outDir)
	mobile := measureLegend(t, s, 600, 1200)
	checkLegendGeometry(t, "窄屏 600（移动断点）", mobile)
	checkLegendRow(t, "窄屏 600（移动断点）", mobile, 3)

	// 截图与悬浮读数都回到宽屏那一档（与用户截图同一个版式）。
	s.setViewport(1500, 1180)
	time.Sleep(400 * time.Millisecond)

	// ---- ③ 人工核对截图（只在设了 PROBE_SHOT_DIR 时产出）----
	if outDir != "" {
		legendShots(t, s, outDir)
	}

	// ---- ④ 整棵子树快照（宽/窄各一份，给 before/after 逐行 diff 用）----
	// 放在悬浮读数那一条**前面**：那一条是 Fatalf（量不到就是量不到），而快照是
	// "本次跑出来的记录"—— 断言红了也要留下，否则改前/改后就没得比。
	writeLegendSnapshot(t, s, outDir, 1500, 1180)
	writeLegendSnapshot(t, s, outDir, 760, 1000)

	// ---- ⑤ 悬浮读数里那一行（chart.js 的 drawHover，第二个展示丢包的地方）----
	s.setViewport(1500, 1180)
	time.Sleep(400 * time.Millisecond)
	checkLegendHoverLoss(t, s)

	// ---- ⑥ 浮层整体必须落在画布内（"窄屏 + 每个目标都贡献 3 行"那一档）----
	//
	// 为什么必须专门摆这一档：只有"每个目标都有数据"时浮层才高到贴住画布底边 ——
	// 也就是用户真实现场（截图里三个目标）。上面那台机器第三个目标没有数据，
	// 浮层只有 7 行，怎么量都测不到夹取。第二台机器三个目标都有数据（10 行）：
	// 760 宽的画布只有 150px 高，不夹取时浮层底边落在 163.9 —— 最后一行被裁掉。
	checkLegendTipFitsCanvas(t, s, outDir, f)

	// ---- ⑦ 行距压缩那一档（14px 一行放不下时压行距，**一行都不减**）----
	//
	// 4 个目标 = 13 行：按 14px 排是 191.8px，170px 的画布怎么顶都放不下 ——
	// 只有压行距能救。这一档证明"通用"：不是只修 10 行那一种。
	checkLegendTipShrinksToFit(t, s, outDir, f)
}

// checkLegendText 逐字比对一张卡片的整行文案。
func checkLegendText(t *testing.T, obs legendObservation, label, want string) {
	t.Helper()
	card := findCard(t, obs, label)
	if card.Stats != want {
		t.Errorf("%d 宽的视口下 %s 的卡片 = %q，期望 %q", obs.Viewport, label, card.Stats, want)
	}
}

// checkLegendHoverLoss 核对**悬浮读数**里那一行也照实写了 0%。
//
// 观测量是 canvas 上真正画出来的字：chart.js 的浮层是画在 canvas 上的，DOM 里没有
// 它。做法与仓库里其它用例一样 —— 钩住 CanvasRenderingContext2D.fillText，再把鼠标
// 移到延迟图上（真派发 mousemove，走页面自己的 onMove → draw）。
func checkLegendHoverLoss(t *testing.T, s *cdpSession) {
	t.Helper()
	if !s.evalBool(`(function () {
      var chips = document.querySelectorAll('#lat-targets .lat-chips .chip');
      for (var i = 0; i < chips.length; i++) {
        if (chips[i].textContent === '丢包') return chips[i].getAttribute('aria-pressed') === 'true';
      }
      return false;
    })()`) {
		t.Fatalf("「丢包」这枚 chip 不是打开状态：竖条不画，悬浮读数里也就不会有丢包那一行" +
			"（本机默认是打开的，先确认前提）")
	}
	// 装钩子：把这一帧画到 canvas 上的每一段字都收下来。
	s.eval(`(function () {
      window.__legendTexts = [];
      if (!window.__legendHooked) {
        window.__legendHooked = true;
        var orig = CanvasRenderingContext2D.prototype.fillText;
        CanvasRenderingContext2D.prototype.fillText = function (t) {
          try { window.__legendTexts.push(String(t)); } catch (e) {}
          return orig.apply(this, arguments);
        };
      }
      return true;
    })()`)

	var rows, lossRows, peakRows []string
	// 浮层里那几行都带两个空格缩进（chart.js 的 drawHover），标签行没有。
	// 横坐标试几个位置：万一某一个恰好落在没有点的缺口里，换一个再试。
	for _, frac := range []string{"0.5", "0.4", "0.6", "0.3", "0.7"} {
		if !s.evalBool(`(function () {
          window.__legendTexts = [];
          var c = document.getElementById('chart-lat');
          if (!c) return false;
          if (c.scrollIntoView) c.scrollIntoView({ block: 'center' });
          var r = c.getBoundingClientRect();
          c.dispatchEvent(new MouseEvent('mousemove', {
            clientX: r.left + r.width * ` + frac + `, clientY: r.top + r.height * 0.5, bubbles: true
          }));
          return true;
        })()`) {
			t.Fatalf("页面上没有 #chart-lat：延迟图没画出来")
		}
		rows, lossRows, peakRows = nil, nil, nil
		s.evalJSON(`window.__legendTexts`, &rows)
		for _, row := range rows {
			if !strings.HasPrefix(row, "  ") {
				continue
			}
			if strings.Contains(row, "丢包") {
				lossRows = append(lossRows, row)
			}
			if strings.Contains(row, "峰值") {
				peakRows = append(peakRows, row)
			}
		}
		if len(lossRows) > 0 {
			break
		}
	}
	t.Logf("悬浮读数里画出来的行 = %v（峰值 %d 行、丢包 %d 行）", rows, len(peakRows), len(lossRows))
	if len(lossRows) == 0 {
		t.Fatalf("悬浮读数里一行「丢包 X%%」都没有（这一帧画出来的字 = %v）："+
			"要么丢包那一行被整个删掉了，要么三个目标的浮层都没画出来", rows)
	}
	const want = "  丢包 0%"
	if !harness.ContainsString(lossRows, want) {
		t.Errorf("悬浮读数里的丢包行 = %v，里面没有 %q：0%% 也要写出来"+
			"（用户要的是「测了就该有数」，不是「只在丢包时才列一行」）", lossRows, want)
	}
	if len(lossRows) != 2 {
		t.Errorf("悬浮读数里的丢包行有 %d 行，期望 2 行（只有两个目标有数据，"+
			"%s 这一段没有探测记录，不该凭空多出一行「丢包 0%%」）：%v",
			len(lossRows), legendNoneLabel, lossRows)
	}
}

// ---------------------------------------------------------------- 浮层几何

// legendTipBox 是悬浮浮层的那块背景矩形（chart.js 用 roundRect 画的**唯一**一处，
// 所以钩住 roundRect 就等于钩住浮层本体）。
type legendTipBox struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// legendTipObs 是一帧悬浮读数的几何：浮层矩形 + 这一帧画出来的字（含坐标）+ 画布尺寸。
type legendTipObs struct {
	Box     *legendTipBox `json:"box"`
	Texts   []string      `json:"texts"` // "文本\u0001y\u0001x"
	CanvasW float64       `json:"canvasW"`
	CanvasH float64       `json:"canvasH"`
}

// legendTipRow 是浮层里的一行。
type legendTipRow struct {
	Text string
	Y    float64
}

// checkLegendTipFitsCanvas 是"浮层整体落在画布内"那一条。
//
// 为什么必须真浏览器量：浮层是画在 canvas 上的，DOM 里没有它 —— 静态断言只能
// 证明代码形状（"有没有写 Math.min"），证明不了这一帧画出来的矩形到底有没有
// 越过画布边界。这里钩 roundRect 拿到矩形本体，再拿这一帧 fillText 的坐标核对
// 每一行的字。
//
// 四类判据（都取真几何）：
//  1. 矩形**上下左右**都在画布内（右/下边界超出即报，超出多少写进消息）；
//  2. 浮层里的**行数**必须是 1 + 目标数 × 3 —— 不许靠"少画一行"来放得下
//     （那等于把用户要的丢包读数又藏回去）；
//  3. 每一行的字都落在画布内（y + 字号 ≤ 画布高），且行距不小于字号 11px
//     （压缩行距可以，压到读不出来不行）；
//  4. 三个目标各有一行「丢包 X%」。
func checkLegendTipFitsCanvas(t *testing.T, s *cdpSession, outDir string, f *legendFixture) {
	t.Helper()
	crowd := startLegendCrowdNode(t, f)
	s.eval("window.location.hash = '#/n/" + strconv.FormatInt(crowd, 10) + "'")
	s.waitJS("第二台机器的三张卡片都有读数（浮层要凑到 10 行）", legendCrowdReadyJS, 60*time.Second)
	installLegendTipHooks(t, s)

	// 宽屏与窄屏各量一遍：窄屏那一档（画布只剩 150px）才是夹取真正起作用的地方。
	for _, size := range [][2]int{{1500, 1180}, {760, 1000}} {
		s.setViewport(size[0], size[1])
		time.Sleep(500 * time.Millisecond)
		obs := captureLegendTip(t, s, "0.5")
		rows := legendTipRows(obs)
		if obs.Box != nil {
			t.Logf("%d 宽：画布 %.0f×%.0f；浮层矩形 (%.2f, %.2f, %.2f×%.2f)，底边 %.2f；"+
				"里面 %d 行、行距 %.2fpx", size[0], obs.CanvasW, obs.CanvasH,
				obs.Box.X, obs.Box.Y, obs.Box.W, obs.Box.H, obs.Box.Y+obs.Box.H,
				len(rows), tipRowGap(rows))
		}
		legendTipShot(t, s, outDir, size[0], 3)
		checkLegendTipInsideCanvas(t, fmt.Sprintf("%d 宽（三个目标都有数据）", size[0]), obs, 10, 3)
	}
}

// startLegendCrowdNode 建第二台机器并给**三个目标都**铺数据。
//
// 为什么不用第一台：它第三个目标故意没有数据（卡片那三条断言要的就是"没有数据
// 不写 0%"），浮层因此只有 7 行 —— 够不到画布底边，量不出夹取。
func startLegendCrowdNode(t *testing.T, f *legendFixture) int64 {
	t.Helper()
	id, _ := harness.CreateNodeViaAPI(t, f.br, "zj-legend-crowd")
	seedLegendSeries(t, f.h, id, f.ids[legendZeroLabel], legendZeroAvg, legendZeroMax, nil)
	seedLegendSeries(t, f.h, id, f.ids[legendLossLabel], legendLossAvg, legendLossMax,
		map[int]int64{20: 94, 40: 94})
	seedLegendSeries(t, f.h, id, f.ids[legendNoneLabel], 135, 381, nil)
	return id
}

// legendCrowdReadyJS 与 legendReadyJS 同一条判据，只是额外要求三张卡片**都有读数**
// （没有"暂无数据"）：浮层要凑到 10 行才测得到夹取。
const legendCrowdReadyJS = `(function () {
  var box = document.getElementById('lat-targets');
  if (!box || box.hidden) return false;
  var cards = box.querySelectorAll('.lat-card');
  if (cards.length !== 3) return false;
  for (var i = 0; i < cards.length; i++) {
    var s = cards[i].querySelector('.lat-card-stats');
    if (!s || !s.textContent || s.textContent === '暂无数据') return false;
  }
  var c = document.getElementById('chart-lat');
  return !!c && c.getBoundingClientRect().width > 0;
})()`

// installLegendTipHooks 钩住 canvas：roundRect（浮层背景）、fillText（每一行字 + 坐标）。
//
// roundRect 是浮层**唯一**的调用方（chart.js 里就这一处），所以捕到的那块矩形
// 一定是浮层本体；老浏览器没有 roundRect 时走 ctx.rect 兜底（只在没有 roundRect
// 时才记 rect，免得被别处的 rect 覆盖）。
func installLegendTipHooks(t *testing.T, s *cdpSession) {
	t.Helper()
	if !s.evalBool(`(function () {
      window.__tipBox = null;
      window.__tipTexts = [];
      if (window.__tipHooked) return true;
      window.__tipHooked = true;
      var proto = CanvasRenderingContext2D.prototype;
      var origRR = proto.roundRect;
      if (origRR) {
        proto.roundRect = function (x, y, w, h) {
          try { window.__tipBox = { x: x, y: y, w: w, h: h }; } catch (e) {}
          return origRR.apply(this, arguments);
        };
      }
      var origRect = proto.rect;
      proto.rect = function (x, y, w, h) {
        if (!origRR) { try { window.__tipBox = { x: x, y: y, w: w, h: h }; } catch (e) {} }
        return origRect.apply(this, arguments);
      };
      var origText = proto.fillText;
      proto.fillText = function (text, x, y) {
        try { window.__tipTexts.push(String(text) + '\u0001' + y + '\u0001' + x); } catch (e) {}
        return origText.apply(this, arguments);
      };
      return true;
    })()`) {
		t.Fatalf("装不上 canvas 钩子（roundRect / fillText）")
	}
}

// captureLegendTip 把鼠标移到延迟图中间，把这一帧的浮层矩形与所有文字坐标收回来。
func captureLegendTip(t *testing.T, s *cdpSession, frac string) legendTipObs {
	t.Helper()
	var obs legendTipObs
	s.evalJSON(`(function () {
      window.__tipBox = null;
      window.__tipTexts = [];
      var c = document.getElementById('chart-lat');
      if (!c) return null;
      if (c.scrollIntoView) c.scrollIntoView({ block: 'center' });
      var r = c.getBoundingClientRect();
      c.dispatchEvent(new MouseEvent('mousemove', {
        clientX: r.left + r.width * `+frac+`, clientY: r.top + r.height * 0.5, bubbles: true }));
      return { box: window.__tipBox, texts: window.__tipTexts,
               canvasW: r.width, canvasH: r.height };
    })()`, &obs)
	if obs.Box == nil {
		t.Fatalf("这一帧没有画出浮层（roundRect 没被调用）：鼠标位置 %.2s 附近可能没有点", frac)
	}
	return obs
}

// legendTipRows 只取**浮层里**的行：浮层的每一行都画在 boxX + pad(=7) 处，
// 而坐标轴标签画在别处（x 完全不同）—— 用 x 就能把它们分开。
func legendTipRows(obs legendTipObs) []legendTipRow {
	if obs.Box == nil {
		return nil
	}
	out := make([]legendTipRow, 0, len(obs.Texts))
	for _, raw := range obs.Texts {
		parts := strings.Split(raw, "\u0001")
		if len(parts) != 3 {
			continue
		}
		y, errY := strconv.ParseFloat(parts[1], 64)
		x, errX := strconv.ParseFloat(parts[2], 64)
		if errY != nil || errX != nil {
			continue
		}
		if math.Abs(x-(obs.Box.X+7)) > 0.6 {
			continue
		}
		out = append(out, legendTipRow{Text: parts[0], Y: y})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Y < out[j].Y })
	return out
}

// tipRowGap 返回相邻两行的行距（不足两行时给 0）。
func tipRowGap(rows []legendTipRow) float64 {
	if len(rows) < 2 {
		return 0
	}
	return rows[1].Y - rows[0].Y
}

// tipTextsOf 把行文本抽出来（报错信息里看得见浮层到底画了什么）。
func tipTextsOf(rows []legendTipRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Text)
	}
	return out
}

// checkLegendTipInsideCanvas 核对浮层矩形、每一行字、行距、行数与丢包行数。
//
// wantRows 是浮层应有的行数（1 时间 + 目标数 × 3），wantLoss 是其中「丢包」那几行的条数
// （= 有数据的目标数）：两个都钉死，是为了让"靠少画几行来放得下"这种规避当场变红。
func checkLegendTipInsideCanvas(t *testing.T, tag string, obs legendTipObs, wantRows, wantLoss int) {
	t.Helper()
	if obs.Box == nil {
		t.Fatalf("%s：没有捕获到浮层矩形（roundRect 没被调用 —— 浮层根本没画出来）", tag)
	}
	box := obs.Box
	if box.X < -0.51 || box.Y < -0.51 {
		t.Errorf("%s：浮层左上角跑到画布外了（x=%.2f, y=%.2f）", tag, box.X, box.Y)
	}
	if over := box.X + box.W - obs.CanvasW; over > 0.51 {
		t.Errorf("%s：浮层右边超出画布 %.2fpx（矩形右边界 %.2f > 画布宽 %.2f）",
			tag, over, box.X+box.W, obs.CanvasW)
	}
	if over := box.Y + box.H - obs.CanvasH; over > 0.51 {
		t.Errorf("%s：浮层下边超出画布 %.2fpx（矩形下边界 %.2f > 画布高 %.2f）："+
			"最后一行会被画布裁掉 —— boxY 必须上下都夹（见 chart.js 的 drawHover）",
			tag, over, box.Y+box.H, obs.CanvasH)
	}
	rows := legendTipRows(obs)
	if len(rows) != wantRows {
		t.Errorf("%s：浮层里画了 %d 行，期望 %d 行（1 时间 + 3 目标 × 3 行 = 读数 + 峰值 + 丢包）："+
			"不许为了放得下而少画行（少了行就是把丢包读数藏回去）：%v",
			tag, len(rows), wantRows, tipTextsOf(rows))
	}
	for i, row := range rows {
		if row.Y < box.Y-0.51 {
			t.Errorf("%s：第 %d 行 %q 画在浮层上边界之上（y=%.2f < 矩形顶 %.2f）", tag, i, row.Text, row.Y, box.Y)
		}
		if row.Y+11 > obs.CanvasH+0.51 {
			t.Errorf("%s：第 %d 行 %q 的字超出画布底边（y=%.2f + 字号 11px > 画布高 %.2f）",
				tag, i, row.Text, row.Y, obs.CanvasH)
		}
		if i > 0 {
			if gap := row.Y - rows[i-1].Y; gap < 11-0.01 {
				t.Errorf("%s：第 %d 行与上一行只隔 %.2fpx（下限是字号 11px）："+
					"压缩行距可以，压到叠在一起读不出来不行", tag, i, gap)
			}
		}
	}
	loss := 0
	for _, row := range rows {
		if strings.HasPrefix(row.Text, "  ") && strings.Contains(row.Text, "丢包") {
			loss++
		}
	}
	if loss != wantLoss {
		t.Errorf("%s：浮层里的丢包行 = %d 行，期望 %d 行（每个有数据的目标一行）", tag, loss, wantLoss)
	}
}

// legendTipShot 给**带浮层的画布**留一张图（人工核对"夹取前后"就靠它）。
//
// 文件名里带上目标数：同一个视口下 3 个目标与 4 个目标的浮层不是同一个东西
// （10 行 vs 13 行、行距 14px vs 12.32px），不能互相覆盖。
func legendTipShot(t *testing.T, s *cdpSession, outDir string, w, targets int) {
	t.Helper()
	if outDir == "" {
		return
	}
	label := os.Getenv("PROBE_SHOT_LABEL")
	if label == "" {
		label = "run"
	}
	clip := s.clipOf("", `document.getElementById('chart-lat').getBoundingClientRect()`, 4)
	if clip == nil {
		t.Errorf("浮层截图（%d 宽）：页面上没有 #chart-lat", w)
		return
	}
	s.screenshot(filepath.Join(outDir,
		fmt.Sprintf("latency-legend-浮层-%d-%d目标-%s.png", w, targets, label)), clip)
}

// legendFourthLabel 是第 4 个探测目标：只给"行距压缩"那一档用（§⑦）。
const legendFourthLabel = "浙江广电"

// checkLegendTipShrinksToFit 是"14px 一行放不下时压行距（一行都不减）"那一条。
//
// 为什么需要单独一档：3 个目标（10 行）在窄屏靠"整体往上顶"就够了（boxH 149.8
// 刚好等于画布 150），压行距那条分支根本不会执行。4 个目标 = 13 行 = 191.8px，
// 顶到 0 也放不下 —— 这时只能压行距，而下限（字号 11px）与"一行都不减"两条
// 规矩同时被这一档钉住。
func checkLegendTipShrinksToFit(t *testing.T, s *cdpSession, outDir string, f *legendFixture) {
	t.Helper()
	addLegendFourthTarget(t, f)
	node4, _ := harness.CreateNodeViaAPI(t, f.br, "zj-legend-crowd4")
	for _, label := range []string{legendZeroLabel, legendLossLabel, legendNoneLabel, legendFourthLabel} {
		seedLegendSeries(t, f.h, node4, f.ids[label], 140, 300, nil)
	}
	s.eval("window.location.hash = '#/n/" + strconv.FormatInt(node4, 10) + "'")
	s.waitJS("第三台机器的四张卡片都有读数（浮层 13 行）", legendCrowd4ReadyJS, 60*time.Second)

	// 宽屏：13 行 × 14px 放不下 170px 的画布 ⇒ 行距被压到 ~12.3px，整块仍落在画布内。
	s.setViewport(1500, 1180)
	time.Sleep(500 * time.Millisecond)
	obs := captureLegendTip(t, s, "0.5")
	rows := legendTipRows(obs)
	gap := tipRowGap(rows)
	t.Logf("1500 宽（4 个目标）：浮层 %d 行、行距 %.2fpx、矩形 (%.2f, %.2f, %.2f×%.2f)、底边 %.2f、画布 %.0f×%.0f",
		len(rows), gap, obs.Box.X, obs.Box.Y, obs.Box.W, obs.Box.H, obs.Box.Y+obs.Box.H, obs.CanvasW, obs.CanvasH)
	checkLegendTipInsideCanvas(t, "1500 宽（4 个目标：13 行，行距被压缩）", obs, 13, 4)
	if gap >= 14-0.01 {
		t.Errorf("1500 宽 4 个目标：行距还是 %.2fpx（没被压缩）—— 13 行按 14px 排是 191.8px，"+
			"超出 170px 的画布 21.8px，压缩那条分支没生效", gap)
	}
	if gap < 11-0.01 {
		t.Errorf("1500 宽 4 个目标：行距被压到 %.2fpx，低于可读下限（字号 11px）", gap)
	}
	legendTipShot(t, s, outDir, 1500, 4)

	// 窄屏 4 个目标：**已知的物理上限**，只记录不断言。
	//
	// 13 行 × 11px 下限 + 上下内边距 9.8 = 152.8px > 画布 150px：单列、字号 11px
	// 的画布浮层在 150px 里放不下 13 行 —— 这是画布高度决定的，不是夹取的漏洞
	// （真要在窄屏放 4 个以上目标，得改浮层形态：多栏 / DOM 浮层）。这里如实记录数字。
	s.setViewport(760, 1000)
	time.Sleep(500 * time.Millisecond)
	narrow := captureLegendTip(t, s, "0.5")
	narrowRows := legendTipRows(narrow)
	t.Logf("760 宽（4 个目标，已知上限）：浮层 %d 行、行距 %.2fpx、矩形顶 %.2f、底边 %.2f、画布高 %.0f"+
		"（超出 %.2fpx —— 13 行压到 11px 下限仍要 152.8px）",
		len(narrowRows), tipRowGap(narrowRows), narrow.Box.Y, narrow.Box.Y+narrow.Box.H,
		narrow.CanvasH, narrow.Box.Y+narrow.Box.H-narrow.CanvasH)
	legendTipShot(t, s, outDir, 760, 4)
}

// addLegendFourthTarget 把探测目标从 3 个改成 4 个（PUT 是整体替换语义），并把
// 接口返回的 id 回填进 fixture —— 不回填就会拿着 id=0 去铺数据（一条也铺不进去，
// 页面上第四张卡片永远是「暂无数据」）。
//
// 为什么要"加"而不是一开始就 4 个：目标列表是全局的，前两台机器的卡片断言
// （尤其是"没有数据那张必须写暂无数据"）都建立在"一共 3 个目标"上。
// 走真接口 PUT，已存在的三个目标按 (type, host, port) 复用同一个 id。
func addLegendFourthTarget(t *testing.T, f *legendFixture) {
	t.Helper()
	labels := []string{legendZeroLabel, legendLossLabel, legendNoneLabel, legendFourthLabel}
	targets := make([]map[string]any, 0, len(labels))
	for i, label := range labels {
		targets = append(targets, map[string]any{
			"label": label, "type": "tcp",
			"host": "202.96.96." + strconv.Itoa(i+1), "port": 443, "enabled": true,
		})
	}
	status, body := f.br.Do("PUT", "/api/v1/settings/ping",
		map[string]any{"interval_sec": 60, "targets": targets}, true)
	if status != 200 {
		t.Fatalf("把探测目标改成 4 个失败: %d %v", status, body)
	}
	list, _ := body["targets"].([]any)
	if len(list) != 4 {
		t.Fatalf("探测目标数量 = %d，期望 4：%v", len(list), body)
	}
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		label, _ := item["label"].(string)
		id, _ := item["id"].(float64)
		if id <= 0 {
			t.Fatalf("目标 %q 没有返回 id: %v", label, item)
		}
		f.ids[label] = int64(id)
	}
	if f.ids[legendFourthLabel] == 0 {
		t.Fatalf("接口没返回第四个目标的 id：%v", body)
	}
}

// legendCrowd4ReadyJS 与 legendReadyJS 同一条判据，只是要求**四张**卡片都有读数。
const legendCrowd4ReadyJS = `(function () {
  var box = document.getElementById('lat-targets');
  if (!box || box.hidden) return false;
  var cards = box.querySelectorAll('.lat-card');
  if (cards.length !== 4) return false;
  for (var i = 0; i < cards.length; i++) {
    var s = cards[i].querySelector('.lat-card-stats');
    if (!s || !s.textContent || s.textContent === '暂无数据') return false;
  }
  var c = document.getElementById('chart-lat');
  return !!c && c.getBoundingClientRect().width > 0;
})()`

// checkLegendGeometry 是"没挤、没折行、没溢出"那一条的判据（每一档视口都过一遍）。
//
// 四个判据都取真几何，不看文本：
//   - scrollW ≤ clientW + 0.5：统计行没有横向溢出（被裁掉就看不全了）；
//   - ratio ≤ 1：最宽的那一行确实放得下；
//   - 三张卡片的字号一致：多一段文字不该带来任何排版差异；
//   - 0% 那张卡片的行数 ≤ >0% 那张：两张卡片的文字位数一样，而「0%」比「0.2%」
//     短两个字符 —— 多出来的那一段**不能**让 0% 的卡片比旁边的先折行。
func checkLegendGeometry(t *testing.T, tag string, obs legendObservation) {
	t.Helper()
	zero := findCard(t, obs, legendZeroLabel)
	loss := findCard(t, obs, legendLossLabel)
	none := findCard(t, obs, legendNoneLabel)
	for _, c := range obs.Cards {
		if c.ScrollW > c.ClientW+0.5 {
			t.Errorf("%s：%s 的统计行横向溢出了（scrollWidth %.1f > clientWidth %.1f）：%q",
				tag, c.Name, c.ScrollW, c.ClientW, c.Stats)
		}
		if c.CardScrollW > c.CardClientW+0.5 {
			t.Errorf("%s：%s 的卡片被内容顶出容器（scrollWidth %.1f > clientWidth %.1f）",
				tag, c.Name, c.CardScrollW, c.CardClientW)
		}
		if c.Lines < 1 {
			t.Errorf("%s：%s 的统计行量到 %d 行（Range.getClientRects 没拿到行盒）", tag, c.Name, c.Lines)
		}
		if c.Ratio > 1 {
			t.Errorf("%s：%s 的统计行最宽一行 %.1fpx 超过可用宽度 %.1fpx（%.1f%%）",
				tag, c.Name, c.Widest, c.Avail, c.Ratio*100)
		}
		t.Logf("%s：%-8s 统计行 %d 行，最宽 %.1fpx / 可用 %.1fpx（%.1f%%），卡片 %.1f×%.1f，字号 %.1fpx，文案 %q",
			tag, c.Name, c.Lines, c.Widest, c.Avail, c.Ratio*100, c.CardW, c.CardH, c.FontSize, c.Stats)
	}
	if zero.Lines > loss.Lines {
		t.Errorf("%s：丢包 0%% 的卡片占了 %d 行，比有丢包的那张（%d 行）还多 —— "+
			"多出来的那一段把统计行挤折行了（%q vs %q）",
			tag, zero.Lines, loss.Lines, zero.Stats, loss.Stats)
	}
	if none.Lines < 1 || none.Stats != "暂无数据" {
		t.Errorf("%s：没有数据的那张卡片 = %q（%d 行），期望「暂无数据」", tag, none.Stats, none.Lines)
	}
	if zero.FontSize != loss.FontSize || zero.FontSize != none.FontSize {
		t.Errorf("%s：三张卡片的统计行字号不一致（%.2f / %.2f / %.2f）",
			tag, zero.FontSize, loss.FontSize, none.FontSize)
	}
}

// checkLegendRow 核对三张卡片还在一行里（wantRows = 1）或者各自占一行（= 3）。
func checkLegendRow(t *testing.T, tag string, obs legendObservation, wantRows int) {
	t.Helper()
	tops := map[int]int{}
	for _, c := range obs.Cards {
		tops[int(math.Round(c.CardTop))]++
	}
	if len(tops) != wantRows {
		t.Errorf("%s：三张卡片占了 %d 行（顶边 %v），期望 %d 行", tag, len(tops), tops, wantRows)
	}
}

// legendReadyJS 是"延迟图例画齐了"的判据。
const legendReadyJS = `(function () {
  var box = document.getElementById('lat-targets');
  if (!box || box.hidden) return false;
  var cards = box.querySelectorAll('.lat-card');
  if (cards.length !== 3) return false;
  for (var i = 0; i < cards.length; i++) {
    var s = cards[i].querySelector('.lat-card-stats');
    if (!s || !s.textContent) return false;
  }
  var c = document.getElementById('chart-lat');
  return !!c && c.getBoundingClientRect().width > 0;
})()`

// loginLegendPage 走真登录表单（不注入 Cookie，整条链路与用户手点一样）。
func loginLegendPage(t *testing.T, s *cdpSession) {
	t.Helper()
	s.waitJS("登录视图出现", `(function () {
      var el = document.getElementById('view-login');
      var home = document.getElementById('view-home');
      var detail = document.getElementById('view-detail');
      return (!!el && !el.hidden) || (!!home && !home.hidden) || (!!detail && !detail.hidden);
    })()`, 30*time.Second)
	if s.evalBool(`(function () {
      var home = document.getElementById('view-home');
      var detail = document.getElementById('view-detail');
      return (!!home && !home.hidden) || (!!detail && !detail.hidden);
    })()`) {
		return
	}
	if !s.evalBool(`(function () {
      var u = document.getElementById('login-user');
      var p = document.getElementById('login-pass');
      var b = document.getElementById('login-submit');
      if (!u || !p || !b) return false;
      u.value = 'admin';
      p.value = '` + harness.AdminPass + `';
      b.click();
      return true;
    })()`) {
		t.Fatalf("登录表单不在页面上（login-user / login-pass / login-submit）")
	}
	s.waitJS("登录后进入应用", `(function () {
      var home = document.getElementById('view-home');
      var detail = document.getElementById('view-detail');
      return (!!home && !home.hidden) || (!!detail && !detail.hidden);
    })()`, 30*time.Second)
}

// scrollLegendIntoViewJS 生成"把某个元素滚到视口中间"的表达式。
func scrollLegendIntoViewJS(id string) string {
	return `(function () {
      var el = document.getElementById('` + id + `');
      if (!el) return false;
      if (el.scrollIntoView) el.scrollIntoView({ block: 'center' });
      return true;
    })()`
}

// legendNarrowShot 在 760 那一档下出一张整行近景（没设 PROBE_SHOT_DIR 时什么都不做）。
func legendNarrowShot(t *testing.T, s *cdpSession, outDir string) {
	t.Helper()
	if outDir == "" {
		return
	}
	clip := s.clipOf(scrollLegendIntoViewJS("lat-targets"),
		"document.getElementById('lat-targets').getBoundingClientRect()", 8)
	if clip == nil {
		t.Errorf("截图 latency-legend-窄屏.png：页面上没有 #lat-targets")
		return
	}
	s.screenshot(filepath.Join(outDir, "latency-legend-窄屏.png"), clip)
}

// legendShots 产出人工核对用的三张卡片近景 + 整行近景。
//
// 三张图分别对应三种情况，文件名与报告里那份交付清单一一对应。
func legendShots(t *testing.T, s *cdpSession, outDir string) {
	t.Helper()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("创建截图目录: %v", err)
	}
	names := []struct{ label, file string }{
		{legendZeroLabel, "latency-legend-0.png"},
		{legendLossLabel, "latency-legend-有丢包.png"},
		{legendNoneLabel, "latency-legend-无数据.png"},
	}
	for _, item := range names {
		// 先把这张卡片滚进视口（滚完再取矩形，clip 用的才是滚动后的位置）。
		clip := s.clipOf("", `(function () {
          var box = document.getElementById('lat-targets');
          if (!box) return null;
          var cards = box.querySelectorAll('.lat-card');
          for (var i = 0; i < cards.length; i++) {
            var n = cards[i].querySelector('.lat-card-name');
            if (n && n.textContent === '`+item.label+`') {
              if (cards[i].scrollIntoView) cards[i].scrollIntoView({ block: 'center' });
              return cards[i].getBoundingClientRect();
            }
          }
          return null;
        })()`, 6)
		if clip == nil {
			t.Errorf("截图 %s：页面上没有 %s 这张卡片", item.file, item.label)
			continue
		}
		s.screenshot(filepath.Join(outDir, item.file), clip)
	}
	// 整行近景：三张卡片放在一起才看得出"多出来的那一段与旁边一致"。
	clip := s.clipOf(scrollLegendIntoViewJS("lat-targets"),
		"document.getElementById('lat-targets').getBoundingClientRect()", 8)
	if clip == nil {
		t.Errorf("截图 latency-legend-整行.png：页面上没有 #lat-targets")
		return
	}
	s.screenshot(filepath.Join(outDir, "latency-legend-整行.png"), clip)
}

// ---------------------------------------------------------------- 整棵子树快照

// snapshotLegendJS 把 #view-detail 整棵子树摊成"一行一个键值"的文本。
//
// 为什么是这一棵树：延迟图例只是详情页里的一小块，而"这次改动会不会碰到别处"
// 正是要用快照回答的问题 —— 根取到视图层，多出来的差异一个都跑不掉。
//
// 每行形如：`路径|属性=值` / `路径|rect=x,y,w,h` / `路径|text=...`。
// text 只收 #charts-latency 里的元素：别处的文字（更新时间、速率、相对时刻）会
// 随时间变，收进来只会把 before/after 的 diff 搅浑；样式与矩形则全树都收。
const snapshotLegendJS = `(function () {
  var root = document.getElementById('view-detail');
  if (!root) return 'NO-VIEW-DETAIL';
  var latCard = document.getElementById('charts-latency');
  var PROPS = ['display', 'position', 'fontSize', 'fontWeight', 'lineHeight', 'color',
    'backgroundColor', 'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft',
    'marginTop', 'marginRight', 'marginBottom', 'marginLeft',
    'borderTopWidth', 'borderRightWidth', 'borderBottomWidth', 'borderLeftWidth',
    'borderTopLeftRadius', 'opacity', 'textAlign', 'overflowX', 'overflowY', 'whiteSpace',
    'overflowWrap', 'gridTemplateColumns', 'rowGap', 'columnGap', 'flexDirection',
    'alignItems', 'justifyContent', 'flexGrow', 'flexShrink', 'flexBasis', 'visibility',
    'boxSizing', 'textOverflow', 'verticalAlign', 'letterSpacing'];
  function pathOf(el) {
    var parts = [];
    while (el && el !== root) {
      var i = 1, sib = el;
      while ((sib = sib.previousElementSibling)) i++;
      parts.unshift(el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + ':' + i);
      el = el.parentNode;
    }
    parts.unshift('#view-detail');
    return parts.join('>');
  }
  function r2(v) { return Math.round(v * 100) / 100; }
  var lines = [];
  var all = [root].concat(Array.prototype.slice.call(root.querySelectorAll('*')));
  all.forEach(function (el) {
    var p = pathOf(el);
    var cs = getComputedStyle(el);
    PROPS.forEach(function (prop) {
      lines.push(p + '|' + prop + '=' + cs[prop]);
    });
    var r = el.getBoundingClientRect();
    lines.push(p + '|rect=' + r2(r.left) + ',' + r2(r.top) + ',' + r2(r.width) + ',' + r2(r.height));
    if (el.tagName === 'CANVAS') {
      lines.push(p + '|bitmap=' + el.width + 'x' + el.height);
    }
    if (latCard && latCard.contains(el) && el.children.length === 0) {
      lines.push(p + '|text=' + el.textContent);
    }
  });
  return lines.join('\n');
})()`

// writeLegendSnapshot 在给定视口下写一份整棵子树快照（没设 PROBE_SHOT_DIR 时只打日志）。
func writeLegendSnapshot(t *testing.T, s *cdpSession, outDir string, w, h int) {
	t.Helper()
	s.setViewport(w, h)
	time.Sleep(500 * time.Millisecond)
	raw := s.evalString(snapshotLegendJS)
	if raw == "NO-VIEW-DETAIL" {
		t.Fatalf("快照：页面上没有 #view-detail")
	}
	lines := strings.Split(raw, "\n")
	sort.Strings(lines)
	text := strings.Join(lines, "\n") + "\n"
	t.Logf("整棵子树快照（%d 宽）：%d 行", w, len(lines))
	if outDir == "" {
		return
	}
	label := os.Getenv("PROBE_SHOT_LABEL")
	if label == "" {
		label = "run"
	}
	out := filepath.Join(outDir, "latency-legend-snapshot-"+label+"-"+strconv.Itoa(w)+".txt")
	if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
		t.Fatalf("写快照: %v", err)
	}
	t.Logf("整棵子树快照 → %s（%d 行）", out, len(lines))
}
