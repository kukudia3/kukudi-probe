package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/store"
)

// Y 轴取整改成"按**步长**取整"（轴顶 = 步长 × 网格线条数）之后，真浏览器验收。
//
// 为什么必须真浏览器：这次改动的全部内容就是"轴顶与刻度长什么样"，而轴顶是
// chart.js 在**每一帧绘制时**算出来的（bounds → yTop → niceStep → 刻度），
// mock 一份数据或静态看一眼源码都证明不了"画出来的是 0/200/400/600/800 而不是
// 0/250/500/750/1000"。这里沿用仓库既有做法：真服务端 + 反向代理注入自检脚本 +
// 钩 canvas 上真正 fillText 出来的字 + 结果 POST 回 mock（**不用**
// --dump-dom + --virtual-time-budget：页面挂着 SSE 长连接，虚拟时间会一直暂停）。
//
// 四组观测：
//  1. **延迟图**：峰值 550ms → 轴顶 800、刻度 [0 200 400 600 800]。旧规则给的是
//     1000（用户截图里那个"上面一半是空的"轴），所以这一组就是这次修的现象本身；
//  2. **CPU 图**：峰值 92%、均值最高 80% → 轴顶**仍然是 100**。百分比图传的是
//     硬上限（yMax = 100），不许被取整规则改成 120；为了让这条有区分度，
//     自检脚本还会把同一个峰值单独喂给"不传 yMax"的合成帧 —— 那边必须是 120，
//     两个数不一样，才说明 CPU 图的 100 来自硬上限而不是碰巧；
//  3. **合成数据对表**：把十几个取值（含 605、0.5 这种边界）直接喂给真的
//     ProbeChart，读回 Y 轴刻度，与 **webui_test.go 里穷举过的那套规则**逐个对数
//     —— 那边扫的是 0…10000 的镜像实现，这里负责证明镜像与真实现一致；
//  4. **路由回归**：#/、#/n/1、#/settings/nodes、#/settings/alert 四条路由走一遍，
//     全程 errs=[]（改动若把某条路径上的图表构造弄崩，这里就是唯一抓得住的地方）。
func TestYAxisTopMatchesStepRuleInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	rule := readYAxisRuleE2E(t)
	h, nodeID := startYAxisFixture(t)

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		NodeID: nodeID, NodeName: "yaxis-01", Scenario: "yaxis",
		User: "admin", Pass: "a-very-good-password",
	}, yaxisHarnessJS)
	mock := newMockServer(t, proxy)

	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 150*time.Second, "1500,1100")

	var res yaxisResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}

	checkYAxisLatencyChart(t, res, rule)
	checkYAxisCPUChart(t, res, rule)
	checkYAxisSynthFrames(t, res, rule)
	checkYAxisRoutes(t, res)
}

// checkYAxisLatencyChart 核对用户报的那一档：数据峰值 550ms → 轴顶 800。
func checkYAxisLatencyChart(t *testing.T, res yaxisResult, rule yaxisRuleE2E) {
	t.Helper()
	if res.Lat.Points < 30 {
		t.Fatalf("延迟曲线只有 %d 个点：数据没铺够，这条用例没有意义", res.Lat.Points)
	}
	// 先把前提钉死：峰值确实是 550ms 那一档（曲线画的是均值，延迟图的 showMax
	// 写死 false，所以撑轴的就是这一串均值里的最大值）。
	if math.Abs(res.Lat.MaxAvg-550) > 0.5 {
		t.Fatalf("延迟曲线的最高点 = %.1f ms，期望 550：前提不对，这条用例验证的不是用户截图那一档", res.Lat.MaxAvg)
	}
	top := yaxisTopOf(t, "延迟图", res.Lat.Labels)
	t.Logf("延迟图：服务端 %d 个点、最高 %.0f ms → Y 轴刻度 %v（轴顶 %v）", res.Lat.Points, res.Lat.MaxAvg, res.Lat.Labels, top)

	// 1) 用户要的那条：750~800 之间（不是 1000）。
	if top < 750 || top > 800 {
		t.Errorf("延迟图 Y 轴上限 = %v ms，期望 750~800（旧规则在 5 与 10 之间没有档位，会把 550 顶到 1000）", top)
	}
	// 2) 与穷举过的那套规则逐个对数：浏览器算出来的轴顶必须等于镜像的 top(550)。
	if want := rule.top(res.Lat.MaxAvg); math.Abs(top-want) > 1e-6*want {
		t.Errorf("延迟图 Y 轴上限 = %v，按 chart.js 的档位表应当是 %v（浏览器里跑的与穷举过的那套规则对不上）", top, want)
	}
	// 3) 刻度必须是**整齐数**：0/200/400/600/800，不是 0/250/500/750/1000，
	//    也不是 0/13/25/38/50 那种四舍五入出来的脏数字。
	want := []float64{0, 200, 400, 600, 800}
	got := parseYAxisLabels(t, "延迟图", res.Lat.Labels)
	if len(got) != len(want) {
		t.Fatalf("延迟图画出 %d 个刻度（%v），期望 %d 个", len(got), res.Lat.Labels, len(want))
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.5 {
			t.Errorf("延迟图第 %d 个刻度 = %v，期望 %v（刻度 = %v）", i+1, got[i], want[i], res.Lat.Labels)
		}
	}
	checkYAxisTicksEven(t, "延迟图", got)
}

// checkYAxisCPUChart 核对百分比图的硬上限：轴顶必须仍然是 100。
func checkYAxisCPUChart(t *testing.T, res yaxisResult, rule yaxisRuleE2E) {
	t.Helper()
	if res.CPU.Points < 30 {
		t.Fatalf("CPU 曲线只有 %d 个点：数据没铺够", res.CPU.Points)
	}
	if res.CPU.MaxMax < 90 {
		t.Fatalf("CPU 这一档的峰值只有 %.1f%%：不到 90 的话自动规则也会给出 100，这条断言没有区分度",
			res.CPU.MaxMax)
	}
	top := yaxisTopOf(t, "CPU 图", res.CPU.Labels)
	t.Logf("CPU 图：服务端 %d 个点、均值最高 %.1f%%、峰值 %.1f%% → Y 轴刻度 %v（轴顶 %v）",
		res.CPU.Points, res.CPU.MaxAvg, res.CPU.MaxMax, res.CPU.Labels, top)
	if math.Abs(top-100) > 0.5 {
		t.Errorf("CPU 图 Y 轴上限 = %v，期望 100：百分比图的 yMax 是硬上限，不许被步长取整规则改动"+
			"（自动规则在峰值 %.1f%% 上会给出 %v）", top, res.CPU.MaxMax, rule.top(res.CPU.MaxMax))
	}
	got := parseYAxisLabels(t, "CPU 图", res.CPU.Labels)
	want := []float64{0, 25, 50, 75, 100}
	if len(got) != len(want) {
		t.Fatalf("CPU 图画出 %d 个刻度（%v），期望 %d 个", len(got), res.CPU.Labels, len(want))
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.5 {
			t.Errorf("CPU 图第 %d 个刻度 = %v，期望 %v（刻度 = %v）", i+1, got[i], want[i], res.CPU.Labels)
		}
	}

	// 同一个峰值喂给"不传 yMax"的合成帧：那边必须是 120 —— 两个数不一样，
	// 才说明 CPU 图的 100 来自硬上限，而不是"取整规则碰巧也算出 100"。
	s := findYAxisSynth(t, res, res.CPU.MaxMax)
	autoTop := yaxisTopOf(t, "CPU 峰值的自动轴顶", s.Labels)
	if math.Abs(autoTop-rule.top(res.CPU.MaxMax)) > 1e-6*autoTop {
		t.Errorf("同一峰值在不传 yMax 时轴顶 = %v，按档位表应当是 %v", autoTop, rule.top(res.CPU.MaxMax))
	}
	if math.Abs(autoTop-100) < 0.5 {
		t.Errorf("同一峰值在不传 yMax 时也得到 100：这条「硬上限」断言没有区分度，请换一组更高的 CPU 数据")
	}
	t.Logf("同一峰值（%.0f%%）在**不传 yMax** 的合成帧上轴顶 = %v（≠ 100）：CPU 图的 100 确实来自硬上限",
		res.CPU.MaxMax, autoTop)
}

// checkYAxisSynthFrames 把十几个取值直接喂给真的 ProbeChart，与 Go 侧那套
// **穷举过**的规则逐个对数（这条用例是"镜像 = 真实现"的桥）。
func checkYAxisSynthFrames(t *testing.T, res yaxisResult, rule yaxisRuleE2E) {
	t.Helper()
	if len(res.Synth) < 12 {
		t.Fatalf("只收到 %d 个合成帧：合成那一段没跑完", len(res.Synth))
	}
	const small = 0.5
	for _, s := range res.Synth {
		top := yaxisTopOf(t, "合成帧 vMax="+trimFloat(s.VMax), s.Labels)
		want := rule.top(s.VMax)
		if math.Abs(top-want) > 1e-6*want {
			t.Errorf("合成帧 vMax=%v 的轴顶 = %v，按 chart.js 的档位表应当是 %v（刻度 %v）",
				s.VMax, top, want, s.Labels)
			continue
		}
		if top < s.VMax {
			t.Errorf("合成帧 vMax=%v 的轴顶 = %v：比数据还小，曲线会被画出界", s.VMax, top)
		}
		if top > s.VMax*1.6 {
			t.Errorf("合成帧 vMax=%v 的轴顶 = %v（%.2f×）：留白超过 1.6 倍上限",
				s.VMax, top, top/s.VMax)
		}
		got := parseYAxisLabels(t, "合成帧 vMax="+trimFloat(s.VMax), s.Labels)
		checkYAxisTicksEven(t, "合成帧 vMax="+trimFloat(s.VMax), got)
		// 极端小数据（用户要求的那一档）：轴顶有限、为正、盖得住峰值。
		if math.Abs(s.VMax-small) < 1e-9 {
			if math.IsNaN(top) || math.IsInf(top, 0) || top <= 0 {
				t.Errorf("vMax=0.5 时轴顶 = %v：NaN/0 会让整张图画不出来", top)
			}
			t.Logf("极端小数据 vMax=0.5 → 轴顶 %.4f，刻度 %v", top, s.Labels)
		}
	}
	t.Logf("合成帧 %d 个（含 vMax=605 与 0.5）逐个与穷举过的档位表对数：全部一致", len(res.Synth))
}

// checkYAxisRoutes 回归四条路由：每条都要渲染出该有的东西，且进入之后不产生新报错。
func checkYAxisRoutes(t *testing.T, res yaxisResult) {
	t.Helper()
	want := []string{"#/", "#/n/1", "#/settings/nodes", "#/settings/alert"}
	if len(res.Routes) != len(want) {
		t.Fatalf("只走完 %d 条路由（%v），期望 %d 条", len(res.Routes), res.Routes, len(want))
	}
	for i, r := range res.Routes {
		if !r.OK {
			t.Errorf("路由 %s 没有渲染出该有的东西", r.Hash)
		}
		if len(r.Errs) != 0 {
			t.Errorf("路由 %s 进入后有 %d 条 JS 报错：%v", r.Hash, len(r.Errs), r.Errs)
		}
		t.Logf("路由 %s：errs=[]，ok=%v", r.Hash, r.OK)
		if i < len(want) && r.Hash != want[i] {
			t.Errorf("第 %d 条路由是 %q，期望 %q（顺序即页面上真实的走法）", i+1, r.Hash, want[i])
		}
	}
}

// ---------------------------------------------------------------- 读数与断言

// yaxisTopOf 取刻度里最大的那个数当轴顶（yFormat 都把 0 写成 "0"、
// 轴顶写成这一列里最大的数）。
func yaxisTopOf(t *testing.T, what string, labels []string) float64 {
	t.Helper()
	vals := parseYAxisLabels(t, what, labels)
	if len(vals) == 0 {
		t.Fatalf("%s 一个 Y 轴刻度都没抓到：图表没画出来，或者刻度被别的绘制路径顶掉了", what)
	}
	top := vals[0]
	for _, v := range vals {
		if v > top {
			top = v
		}
	}
	return top
}

// parseYAxisLabels 把刻度文字解析成数字，顺便挡住 NaN/Infinity。
//
// 百分比图的 yFormat 自带一个 "%" 后缀（"0%"、"25%"…），先摘掉它：
// 这一条只关心**数值**，单位由各自的格式化函数负责（那部分不在这条用例的范围里）。
func parseYAxisLabels(t *testing.T, what string, labels []string) []float64 {
	t.Helper()
	if len(labels) == 0 {
		t.Fatalf("%s 的 Y 轴刻度是空的", what)
	}
	out := make([]float64, 0, len(labels))
	for _, raw := range labels {
		text := strings.TrimSuffix(strings.TrimSpace(raw), "%")
		if strings.Contains(text, "NaN") || strings.Contains(text, "Infinity") {
			t.Errorf("%s 的刻度里出现了 %q：轴范围算出了 NaN/Infinity", what, raw)
			continue
		}
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			t.Errorf("%s 的刻度 %q 解析不出数字", what, raw)
			continue
		}
		out = append(out, v)
	}
	return out
}

// checkYAxisTicksEven 核对刻度是**等距**的（这就是"整齐"在画面上的样子）：
// 相邻两个刻度之间的差必须处处相等，而且从 0 开始。
func checkYAxisTicksEven(t *testing.T, what string, ticks []float64) {
	t.Helper()
	if len(ticks) < 3 {
		t.Errorf("%s 只有 %d 个刻度（%v）：网格线太少，读不出量级", what, len(ticks), ticks)
		return
	}
	if math.Abs(ticks[0]) > 1e-9 {
		t.Errorf("%s 的第一个刻度 = %v，期望 0（轴从 0 起）", what, ticks[0])
	}
	step := ticks[1] - ticks[0]
	if step <= 0 {
		t.Errorf("%s 的刻度不是递增的：%v", what, ticks)
		return
	}
	for i := 2; i < len(ticks); i++ {
		if d := ticks[i] - ticks[i-1]; math.Abs(d-step) > 1e-6*step {
			t.Errorf("%s 的刻度不等距（%v）：相邻差 %v 与 %v 不一致 —— 读起来就不是整齐数了",
				what, ticks, d, step)
			return
		}
	}
}

// findYAxisSynth 在合成帧里找 vMax 对应的那一帧（CPU 那条要拿它证明"硬上限"）。
func findYAxisSynth(t *testing.T, res yaxisResult, vMax float64) yaxisSynth {
	t.Helper()
	for _, s := range res.Synth {
		if math.Abs(s.VMax-vMax) <= 1e-9*math.Max(1, vMax) {
			return s
		}
	}
	t.Fatalf("合成帧里没有 vMax=%v 那一档（收到 %d 帧）", vMax, len(res.Synth))
	return yaxisSynth{}
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ---------------------------------------------------------------- 数据播种

// startYAxisFixture 起一个真服务端并铺好这一档数据：三条延迟曲线（最高 550ms）+
// 一段 CPU（均值最高 80%、峰值 92%）。浏览器用例与截图用例共用它，
// 这样"截图里看到的"与"断言里量的"是同一份数据。
func startYAxisFixture(t *testing.T) (*harness, int64) {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "yaxis-01")

	targetIDs := createYAxisPingTargets(t, br)
	seedYAxisPingBuckets(t, h, nodeID, targetIDs, 60, 55*time.Minute)
	seedYAxisCPUSamples(t, h, nodeID)
	return h, nodeID
}

// createYAxisPingTargets 走真接口配三个探测目标：三条曲线都落在 200~550ms，
// 与用户截图里那三条曲线的量级一致。
func createYAxisPingTargets(t *testing.T, br *browser) []int64 {
	t.Helper()
	targets := []map[string]any{
		{"label": "CF", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
		{"label": "Google", "type": "tcp", "host": "8.8.8.8", "port": 443, "enabled": true},
		{"label": "Quad9", "type": "tcp", "host": "9.9.9.9", "port": 443, "enabled": true},
	}
	status, body := br.do("PUT", "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets":      targets,
	}, true)
	if status != 200 {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}
	list, _ := body["targets"].([]any)
	if len(list) != len(targets) {
		t.Fatalf("探测目标数量 = %d，期望 %d", len(list), len(targets))
	}
	ids := make([]int64, 0, len(list))
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		id, _ := item["id"].(float64)
		if id <= 0 {
			t.Fatalf("探测目标 ID 不合法: %v", item)
		}
		ids = append(ids, int64(id))
	}
	return ids
}

// seedYAxisPingBuckets 按真实的 1 分钟桶喂三条延迟曲线：均值在 220…550ms 之间
// 起伏，**最高点恰好是 550**（i == 11 那一格）——这就是用户截图里那一档数据。
//
// 三条曲线各带一个相位偏移（k*4），所以它们的峰不在同一分钟，图上看着是三条
// 各走各的线；但三者的最大值都到 550，撑轴的仍然是 550 那一档。
func seedYAxisPingBuckets(t *testing.T, h *harness, nodeID int64, targetIDs []int64, step int, span time.Duration) {
	t.Helper()
	now := time.Now()
	end := now.Unix() - now.Unix()%int64(step) - int64(step)
	start := end - int64(span.Seconds())
	var buckets []store.PingBucket
	peak := 0.0
	for k, targetID := range targetIDs {
		for ts := start; ts <= end; ts += int64(step) {
			i := int((ts/int64(step) + int64(k*4)) % 12)
			avg := float64(220 + i*30) // 220…550 ms
			if avg > peak {
				peak = avg
			}
			// 峰值给 avg + 40：延迟图的 showMax 写死 false，它不会撑轴，
			// 但悬浮读数里那一行「峰值」还要有东西可显示。
			buckets = append(buckets, store.NewPingBucket(nodeID, targetID, ts, avg, avg-10, avg+40, 0))
		}
	}
	if len(buckets) < 60 {
		t.Fatalf("播撒的探测桶只有 %d 个，太少", len(buckets))
	}
	if peak != 550 {
		t.Fatalf("播撒出来的延迟最高点 = %v ms，期望 550：数据没铺成用户截图那一档", peak)
	}
	if err := h.db.UpsertPingBuckets(context.Background(), buckets); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	t.Logf("播撒探测桶 %d 个（%d 个目标 × %d 分钟、步长 %d 秒），均值 220…550 ms",
		len(buckets), len(targetIDs), int(span.Minutes()), step)
}

// seedYAxisCPUSamples 喂一段 CPU：均值 74…80%（"最高点 ≈ 80%"），峰值 92%。
//
// 峰值刻意高过 90：自动规则在 92 上会给出 120（92×1.1/4 = 25.3 → 步长 30 → 轴顶 120），
// 而百分比图传了 yMax = 100，轴顶必须仍然是 100 —— 两个数不一样，这条断言才有区分度
// （峰值取 80 的话，自动规则也正好给 100，那条断言就什么也没证明）。
func seedYAxisCPUSamples(t *testing.T, h *harness, nodeID int64) {
	t.Helper()
	end := time.Now().Add(-10 * time.Minute).Truncate(10 * time.Second)
	start := time.Now().Add(-25 * time.Minute).Truncate(10 * time.Second)
	buckets := make([]store.SampleBucket, 0, 90)
	for ts := start; !ts.After(end); ts = ts.Add(10 * time.Second) {
		i := len(buckets)
		buckets = append(buckets, store.SampleBucket{
			NodeID: nodeID, TS: ts.Unix(),
			CPUAvg: float64(74 + i%7), CPUMax: 92, // 均值 74…80、峰值 92
			MemAvg: 20, MemMax: 22, Up: 10, All: 10,
		})
	}
	if len(buckets) < 30 {
		t.Fatalf("播撒的 CPU 桶只有 %d 个，太少", len(buckets))
	}
	if err := h.db.InsertBuckets(context.Background(), store.TableSamples10s, buckets); err != nil {
		t.Fatalf("写入 CPU 桶: %v", err)
	}
	t.Logf("播撒 CPU 桶 %d 个（均值 74…80%%、峰值 92%%）", len(buckets))
}

// ---------------------------------------------------------------- Y 轴规则镜像

// yaxisRuleE2E 是 chart.js 那套取整规则的镜像（档位表、网格线条数、余量都从
// web/chart.js **源码里读**，不在这里抄第二份数）。
//
// 它与 internal/server/webui_test.go 里那份穷举过 0…10000 的镜像同源：
// 那边负责"扫遍所有取值"，这里负责"证明浏览器里跑的与扫过的那套一致"。
type yaxisRuleE2E struct {
	ladder []float64
	lines  float64
	head   float64
}

func readYAxisRuleE2E(t *testing.T) yaxisRuleE2E {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "chart.js"))
	if err != nil {
		t.Fatalf("读取 web/chart.js: %v", err)
	}
	js := string(data)

	var rule yaxisRuleE2E
	m := regexp.MustCompile(`var Y_STEP_LADDER = \[([^\]]*)\];`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("web/chart.js 里找不到 Y_STEP_LADDER")
	}
	for _, raw := range strings.Split(m[1], ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			t.Fatalf("Y_STEP_LADDER 里解析不出数字：%q", raw)
		}
		rule.ladder = append(rule.ladder, v)
	}
	lines := regexp.MustCompile(`var Y_GRID_LINES = (\d+);`).FindStringSubmatch(js)
	if lines == nil {
		t.Fatal("web/chart.js 里找不到 Y_GRID_LINES")
	}
	n, err := strconv.Atoi(lines[1])
	if err != nil || n < 2 {
		t.Fatalf("Y_GRID_LINES = %q 不合法", lines[1])
	}
	rule.lines = float64(n)
	head := regexp.MustCompile(`var Y_HEADROOM = ([\d.]+);`).FindStringSubmatch(js)
	if head == nil {
		t.Fatal("web/chart.js 里找不到 Y_HEADROOM")
	}
	h, err := strconv.ParseFloat(head[1], 64)
	if err != nil || !(h > 1) {
		t.Fatalf("Y_HEADROOM = %q 不合法（必须大于 1）", head[1])
	}
	rule.head = h
	return rule
}

// top 与 chart.js 的 yTop 同一套（不传 yMax 时的自动分支）。
func (r yaxisRuleE2E) top(vMax float64) float64 {
	if !(vMax > 0) {
		return 1
	}
	base := math.Pow(10, math.Floor(math.Log10(vMax*r.head/r.lines)))
	norm := vMax * r.head / r.lines / base
	step := 10 * base
	for _, rung := range r.ladder {
		if norm <= rung {
			step = rung * base
			break
		}
	}
	return step * r.lines
}

// ---------------------------------------------------------------- 观测值

type yaxisResult struct {
	Errs   []string     `json:"errs"`
	Fatal  string       `json:"fatal"`
	Steps  []string     `json:"steps"`
	Lat    yaxisChart   `json:"lat"`
	CPU    yaxisChart   `json:"cpu"`
	Synth  []yaxisSynth `json:"synth"`
	Routes []yaxisRoute `json:"routes"`
}

// yaxisChart 是一张图上"Y 轴刻度"的观测值 + 服务端那一档的数据规模。
type yaxisChart struct {
	// Labels 是这一帧里画在 Y 轴上的刻度文字（按从 0 到轴顶的顺序）。
	Labels []string `json:"labels"`
	// MaxAvg / MaxMax 是服务端数据的最高均值 / 最高峰值（撑轴的是前者，
	// 但 CPU 那类图 showMax 开着，撑轴的是后者）。
	MaxAvg float64 `json:"maxAvg"`
	MaxMax float64 `json:"maxMax"`
	Points int     `json:"points"`
}

// yaxisSynth 是一帧"直接喂给 ProbeChart 的合成数据"。
type yaxisSynth struct {
	VMax   float64  `json:"vmax"`
	Labels []string `json:"labels"`
	Note   string   `json:"note"`
}

type yaxisRoute struct {
	Hash string   `json:"hash"`
	OK   bool     `json:"ok"`
	Errs []string `json:"errs"`
}

// yaxisHarnessJS 是注入到页面里的自检脚本（与 tz/latency/axis 那几条同一套做法：
// 真服务端 + 反代注入 + 结果 POST 回 mock）。
//
// 它只做两件事：**按用户的真实操作走页面**（登录 → 详情页 → 四条路由），
// 以及**把 canvas 上真正画出来的字记下来**。一句断言都不在这里做。
const yaxisHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], fatal: '', steps: [],
    lat: { labels: [], maxAvg: 0, maxMax: 0, points: 0 },
    cpu: { labels: [], maxAvg: 0, maxMax: 0, points: 0 },
    synth: [], routes: []
  };
  window.__YAXISRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // 截图模式：把 EventSource 换成一个不联网的替身。
  // 为什么必须换：--virtual-time-budget 在"还有网络请求挂着"时会**暂停虚拟时间**，
  // 而实时流（SSE）正是一条永远挂着的请求 —— 预算永远耗不完，Chrome 也就不截图
  // （只能被杀掉）。截图要的是一屏静态画面，延迟图的数据由 /ping 一次取回来，
  // 不依赖实时流。
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

  // ---- 钩 canvas：按画布 id 记下每一帧真正画出来的字 ------------------------
  // Y 轴刻度是右对齐画在 g.left - 6 = 58 处的（chart.js 的 layout 里 left 恒为 64），
  // 所以"x ≈ 58"这一条就能把刻度从 X 轴标签（画在绘图区下方、居中锚定，最左也只到
  // g.left = 64）与悬浮浮层里分出来。
  var drawn = {};
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    var orig = proto.fillText;
    proto.fillText = function (text, x, y) {
      var id = this.canvas ? this.canvas.id : '';
      var list = drawn[id] || (drawn[id] = []);
      list.push({ text: String(text), x: x, y: y });
      return orig.apply(this, arguments);
    };
  })();
  function markOf(id) { return (drawn[id] || []).length; }
  function yLabelsSince(id, mark) {
    var out = [];
    (drawn[id] || []).slice(mark).forEach(function (r) {
      if (Math.abs(r.x - 58) < 1.5) out.push(r.text);
    });
    return out;
  }

  // ---- 小工具 -------------------------------------------------------------
  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function getJSON(url) {
    return rawFetch(url, { headers: { 'Accept': 'application/json' }, credentials: 'same-origin', cache: 'no-store' })
      .then(function (res) { return res.json(); });
  }
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
  function maxOf(points, idx) {
    var m = 0;
    (points || []).forEach(function (p) { if (p[idx] > m) m = p[idx]; });
    return m;
  }

  // ---- 合成帧：拿真的 ProbeChart 画一份指定的数据，把 Y 轴刻度读回来 -------
  //
  // 这一条不经过 app.js：它验的是引擎自己的取整规则。画布放在屏幕外
  // （position:absolute; left:-9999px），显式给 600×220 ——
  // chart.js 的 layout() 是按 getBoundingClientRect 量尺寸的，display:none 量到 0。
  var synthHost = null;
  var synthSeq = 0;
  function synthTop(v) {
    if (!synthHost) {
      synthHost = document.createElement('div');
      synthHost.style.cssText = 'position:absolute;left:-9999px;top:0;width:600px;height:220px;';
      document.body.appendChild(synthHost);
    }
    synthSeq++;
    var cv = document.createElement('canvas');
    cv.id = 'chart-synth-' + synthSeq;
    cv.style.width = '600px';
    cv.style.height = '220px';
    synthHost.appendChild(cv);
    var mark = markOf(cv.id);
    var chart = window.ProbeChart.create(cv, {
      series: [{
        label: 's', color: '#2563eb',
        points: [[1700000000, v], [1700000060, v]]
      }],
      // 刻度文字原样打出来：默认的 String(Math.round(v)) 会把 0.5 那一档的
      // 小数部分四舍五入掉，NaN 也会以 "NaN" 出现在结果里（Go 那边会挡）。
      yFormat: function (x) { return String(x); },
      tickBaseSec: 60
    });
    chart.redraw();
    var labels = yLabelsSince(cv.id, mark);
    chart.destroy();
    return labels;
  }

  // SYNTH 是"与 Go 侧穷举过的档位表对表"的取值：包含用户那一例（605）、
  // 各档位的边界（4/22/39/100/2200/4000）与极端小数据（0.5）。
  var SYNTH = [0.5, 4, 22, 39, 100, 200, 550, 605, 780, 1000, 2200, 4000, 1000000];

  function synthPass() {
    SYNTH.forEach(function (v) {
      R.synth.push({ vmax: v, labels: synthTop(v), note: 'sweep' });
    });
    // CPU 那一档：把图上的**峰值**单独喂给自动规则（不传 yMax）。
    // 它必然不是 100 —— 这正是"CPU 图的 100 来自硬上限、不是碰巧"的证据。
    R.synth.push({ vmax: R.cpu.maxMax, labels: synthTop(R.cpu.maxMax), note: 'cpu' });
    R.steps.push('合成帧 ' + R.synth.length + ' 个');
    return true;
  }

  // ---- 登录 / 详情页 -------------------------------------------------------
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

  function openDetail() {
    window.location.hash = '#/n/' + CFG.nodeID;
    return waitFor('详情页打开', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000)
      .then(function () {
        return waitFor('延迟图已绘制', function () { return markOf('chart-lat') > 5; }, 30000);
      })
      .then(function () {
        return waitFor('CPU 图已绘制', function () { return markOf('chart-cpu') > 5; }, 30000);
      })
      .then(function () { return sleep(400); });
  }

  // captureCharts 自己触发一次**完整重画**（resize → 80ms 防抖 → 整张重画），
  // 再把这一次重画里的 Y 轴刻度收下来 —— 量到的因此是稳定的一帧，
  // 而不是"某次局部重画剩下的半张图"。
  function captureCharts() {
    var latMark = markOf('chart-lat');
    var cpuMark = markOf('chart-cpu');
    window.dispatchEvent(new Event('resize'));
    return sleep(600).then(function () {
      R.lat.labels = yLabelsSince('chart-lat', latMark);
      R.cpu.labels = yLabelsSince('chart-cpu', cpuMark);
      R.steps.push('收下延迟图与 CPU 图的 Y 轴刻度');
      // 服务端这一档的数据规模：延迟图撑轴的是均值（showMax 写死 false），
      // CPU 图撑轴的是峰值（pctOpts 的 showMax 是 true）。
      return getJSON('/api/v1/nodes/' + CFG.nodeID + '/ping?range=1h');
    }).then(function (data) {
      var points = [];
      (data.targets || []).forEach(function (t) { points = points.concat(t.points || []); });
      R.lat.maxAvg = maxOf(points, 1);
      R.lat.points = points.length;
      return getJSON('/api/v1/nodes/' + CFG.nodeID + '/series?range=1h&metric=cpu');
    }).then(function (data) {
      var pts = data.points || [];
      R.cpu.maxAvg = maxOf(pts, 1);
      R.cpu.maxMax = maxOf(pts, 2);
      R.cpu.points = pts.length;
      return true;
    });
  }

  // ---- 四条路由 ------------------------------------------------------------
  function routeReady(hash) {
    if (hash === '#/') {
      return shown('view-home') && node('grid') && node('grid').children.length > 0;
    }
    if (hash.indexOf('#/n/') === 0) {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }
    var pane = hash.split('/')[2];
    var sec = document.querySelector('#settings-panes .pane[data-pane="' + pane + '"]');
    if (!shown('view-settings') || !sec || sec.hidden) return false;
    if (pane === 'nodes') return node('nodes-list') && node('nodes-list').children.length > 0;
    return true;
  }

  function routePass() {
    var hashes = ['#/', '#/n/' + CFG.nodeID, '#/settings/nodes', '#/settings/alert'];
    return hashes.reduce(function (chain, hash) {
      return chain.then(function () {
        var before = R.errs.length;
        window.location.hash = hash;
        return waitFor('路由 ' + hash + ' 渲染完成', function () { return routeReady(hash); }, 30000)
          .then(function () { return sleep(300); })
          .then(function () {
            R.routes.push({ hash: hash, ok: true, errs: R.errs.slice(before) });
          });
      });
    }, Promise.resolve());
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!window.ProbeChart; }, 30000)
      .then(login)
      .then(openDetail)
      .then(captureCharts)
      .then(synthPass)
      .then(routePass);
  }

  // shot 模式（人工截图用）：把延迟图这一张卡留成近景，然后停住不回传结果，
  // 只把标题改成 SHOT-READY（截图用例靠 --virtual-time-budget + --screenshot 等它）。
  //
  // 近景用**隐藏其它块**而不是滚动定位：图表是异步加载的，页面高度在加载过程中会变，
  // scrollIntoView 算出来的位置到截图时已经不对了。
  function shot() {
    return login().then(openDetail).then(function () {
      var css = document.createElement('style');
      css.textContent =
        'header.top, footer { display: none !important; }' +
        '.detail-head, .stat-row, .info-grid, #charts-resources { display: none !important; }' +
        '.detail-wrap { padding-top: 0 !important; margin-top: 0 !important; }' +
        '#charts-latency { margin: 0 !important; }';
      document.head.appendChild(css);
      window.scrollTo(0, 0);
      return sleep(500);
    }).then(function () {
      window.scrollTo(0, 0);
      document.title = 'SHOT-READY:yaxis';
      return true;
    });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:yaxis'; });
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
