package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"probe/internal/store"
)

// 窄屏（手机）下的延迟图：**必须还是一条线**，不能退化成一串孤立圆点。
//
// 背景（两个 bug 叠在一起，都只有 web/ 能修）：
//
//  1. 桶宽改细之后（1h/6h/12h 都是 60 秒），手机端的二次聚合目标反而更大
//     （6h 档 120 秒 > 1.5 × 60 秒）。而 chart.js 的断线判据拿的是**服务端桶宽**
//     （linkedWithPrev：相邻两点间隔 > 1.5 × bucketSec 即断开）——
//     于是每一对相邻点都被判成缺口，每条 run 只剩一个点。
//  2. 聚合用的档位取的是 detail.range（资源卡），而延迟图跟的是 detail.pingRange：
//     两张卡停在不同档位时，1h 的延迟曲线会被按 7d 的聚合目标（1800 秒）合并。
//
// 为什么必须在真浏览器里量：这两条都只在"窄屏 + 有这个档位的数据"时成立，
// 静态断言能钉住代码形状，钉不住"画出来到底是一串点还是一条线"。
// 这里直接钩 canvas，数一帧里画了几个孤立圆点（arc）与几段线段（lineTo）：
// 正常情况线段占绝对多数，坏掉时反过来（lineTo = 0、arc = 点数）。
//
// 三组场景（第三组是"实测点距中位数"那一项的唯一守卫，前两组都能被自报值解释）：
//
//	一、窄屏 6h 档：手机端二次聚合目标（120 秒）> 桶宽（60 秒）；
//	二、探测间隔 300 秒 + 6h 档（桶宽 60 秒）：每 5 个桶里只有 1 个有行；
//	三、自报桶宽 60 秒 / 自报探测间隔 60 秒，而实际点距 300 秒（桌面端，无聚合）——
//	    三个自报项都解释不了这个间距，只有实测中位数能救。这一组在加入实测值之前
//	    必然变红（每一对相邻点都被判成缺口）。
func TestMobileLatencyChartStaysConnected(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "lat-01")

	// 配置一个探测目标（走真接口），再按**真实的 1 分钟桶**喂一段曲线。
	targetID := createPingTarget(t, br, "CF", "1.1.1.1", 443, 60)
	seedPingBuckets(t, h, nodeID, targetID, 60, 5*time.Hour)
	// 资源图（CPU）也喂一段：场景 0 要证明"只动了延迟图"—— 资源图的峰值淡线
	// 必须还在（0.28 透明度的描边），而延迟图上一条都不能有。
	seedCPUSamples(t, h, nodeID)

	// ---- 场景组一：手机端二次聚合 + 两卡档位不同 + 悬浮区间阈值 ----------
	agg := runLatHarness(t, chrome, h, tzHarnessConfig{
		NodeID: nodeID, NodeName: "lat-01", Scenario: "agg",
		User: "admin", Pass: "a-very-good-password",
	}, "600,1400")
	if !agg.Mobile {
		t.Fatalf("视口宽度没有落进 (max-width: 640px)：手机端聚合根本没生效，这条用例什么也没验证到（%s）", agg.Steps)
	}
	checkLatencyLineConnected(t, agg)
	checkLatencyNeverDrawsPeakLine(t, agg)
	checkLatencyAxisFitsMean(t, agg)
	checkLatencyChipsHaveNoPeak(t, agg)
	checkHoverStillShowsPeak(t, agg)
	checkStaleViewKeyIsIgnored(t, agg)
	check7dHoverShowsInterval(t, agg)

	// ---- 场景组二：探测间隔（300 秒）> 桶宽（6h 档 60 秒）----------------
	//
	// 探测间隔是可配的，改大之后"每 5 个桶里只有 1 个有行"，相邻两点的实际间距
	// 变成 300 秒 —— 断线判据必须按这个间距算，否则整条曲线又是一串孤立点。
	// 用另一个节点：它的 ping_samples_1m 是独立的，不会被上面那批每分钟一行的数据盖住。
	targetID2 := createPingTarget(t, br, "CF", "1.1.1.1", 443, 300)
	node2, _ := createNodeViaAPI(t, br, "lat-02")
	seedPingBuckets(t, h, node2, targetID2, 300, 350*time.Minute)

	sparse := runLatHarness(t, chrome, h, tzHarnessConfig{
		NodeID: node2, NodeName: "lat-02", Scenario: "interval",
		User: "admin", Pass: "a-very-good-password",
	}, "600,1400")
	checkLatencyLineConnected(t, sparse)
	checkProbeIntervalRespected(t, sparse)

	// ---- 场景组三：自报的桶宽与实际点距不符（只有"实测中位数"能救的那一类）----
	//
	// 前两组都能被**自报值**解释（聚合目标 120 秒、探测间隔 300 秒），所以它们在
	// "只信自报值"的实现下也是绿的。这一组不行：
	//   - /settings 里的探测间隔是 60 秒（自报），/ping 的 meta.bucket_sec 也是 60 秒；
	//   - 而真正落到库里、画出来的相邻两点间隔是 300 秒（Agent 上报节奏不齐就是
	//     这个形状：配置说 60 秒一次，实际 5 分钟才有一行）。
	// 三个自报项算出来是 60，而 300 > 1.5 × 60 —— 每一对相邻点都被判成缺口，
	// 整条曲线退化成一串孤立圆点。这一组在加入"实测中位数"之前**必然变红**。
	//
	// 用桌面宽度（1500×1100）跑：手机端二次聚合目标那一项此时恒为 0，把自报项
	// 压到最小，能救场的只剩实测值。再换一个节点，数据与上面两组互不干扰。
	targetID3 := createPingTarget(t, br, "CF", "1.1.1.1", 443, 60)
	node3, _ := createNodeViaAPI(t, br, "lat-03")
	seedPingBuckets(t, h, node3, targetID3, 300, 350*time.Minute)

	mismatch := runLatHarness(t, chrome, h, tzHarnessConfig{
		NodeID: node3, NodeName: "lat-03", Scenario: "mismatch",
		User: "admin", Pass: "a-very-good-password",
	}, "1500,1100")
	checkLatencyLineConnected(t, mismatch)
	checkMeasuredSpacingRescues(t, mismatch)
}

// runLatHarness 起一层反代 + 注入，跑一次 Chrome，把页面回传的观测值解出来。
func runLatHarness(t *testing.T, chrome string, h *harness, cfg tzHarnessConfig, windowSize string) latResult {
	t.Helper()
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, latHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 90*time.Second, windowSize)

	var res latResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完（%s）：%s", res.Scenario, res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	// 每个场景各自把"这一趟有没有 JS 报错"打出来：三组场景是三次独立的 Chrome
	// 运行，某一组报错时不该让另外两组看起来也"没跑干净"。
	t.Logf("场景 %s：视口 %s、手机端=%v、JS 报错 %d 条、走过的步骤 = %v",
		res.Scenario, windowSize, res.Mobile, len(res.Errs), res.Steps)
	return res
}

// checkLatencyLineConnected 逐个场景核对"延迟曲线是线、不是一串点"。
func checkLatencyLineConnected(t *testing.T, res latResult) {
	t.Helper()
	if len(res.Phases) == 0 {
		t.Fatal("一个场景都没量到")
	}
	for _, p := range res.Phases {
		// 源点数、桶宽与点距都来自**服务端**（同一时刻的 /ping 响应）：
		// 有了它才能说清"应画多少段线"，而不是只看一个绝对数。
		t.Logf("场景 %s：资源档位=%s 延迟档位=%s；服务端 %d 个点（桶宽 %d 秒、点距 %.0f 秒）→ 这一帧画了 %d 个孤立圆点(arc)、%d 段线(lineTo)",
			p.Name, p.ResourceKey, p.LatKey, p.SourcePoints, p.SourceBucket, p.SourceSpacing, p.Arc, p.LineTo)
		if p.SourcePoints < 20 {
			t.Fatalf("场景 %s 的服务端点数只有 %d：数据没铺够，这条用例没有意义", p.Name, p.SourcePoints)
		}
		// 正常：孤立圆点几乎为 0（只有真的断开的那些桶才会画点）。
		// 坏掉：每个点各自成一段 —— arc ≈ 点数、lineTo 只剩几条横网格线。
		if p.Arc > 2 {
			t.Errorf("场景 %s 画了 %d 个孤立圆点（应当是 0~2 个）：断线判据把正常的相邻点判成了缺口 —— "+
				"曲线退化成一串点，而线上一个线段都没有（lineTo=%d）", p.Name, p.Arc, p.LineTo)
		}
		if min := p.SourcePoints / 4; p.LineTo < min {
			t.Errorf("场景 %s 只画了 %d 段线（至少要 %d 段，源数据 %d 个点）：曲线没连起来",
				p.Name, p.LineTo, min, p.SourcePoints)
		}
	}
}

// checkProbeIntervalRespected 核对"探测间隔 > 桶宽"那一组：数据确实是稀疏的
// （点距 = 探测间隔），而曲线仍然是连着的。
//
// 只说"连成线"是不够的：如果数据本身每分钟一行，那这条断言在探测间隔这件事上
// 什么都没证明。所以要先钉住"点距 300 秒、桶宽 60 秒"这个前提。
func checkProbeIntervalRespected(t *testing.T, res latResult) {
	t.Helper()
	if len(res.Phases) == 0 {
		t.Fatal("探测间隔那一组一个场景都没量到")
	}
	p := res.Phases[0]
	if p.SourceBucket != 60 {
		t.Errorf("6h 档桶宽 = %d 秒，期望 60：前提不对，这条用例验证的不是「探测间隔 > 桶宽」",
			p.SourceBucket)
	}
	if p.SourceSpacing != 300 {
		t.Errorf("服务端相邻两点的间距 = %.0f 秒，期望 300（= 探测间隔）：数据不是按 300 秒一行播撒的",
			p.SourceSpacing)
	}
	if p.SourceSpacing <= float64(p.SourceBucket)*1.5 {
		t.Errorf("点距 %.0f 秒没有超过 1.5 × 桶宽（%d 秒）：这组数据不会触发「探测间隔 > 桶宽」的 bug",
			p.SourceSpacing, p.SourceBucket)
	}
	t.Logf("探测间隔 %0.f 秒 > 桶宽 %d 秒：%d 个点仍然画出了 %d 段线、%d 个孤立圆点",
		p.SourceSpacing, p.SourceBucket, p.SourcePoints, p.LineTo, p.Arc)
}

// checkMeasuredSpacingRescues 核对"自报值与实际点距不符"那一组的前提与表现。
//
// 为什么光有 checkLatencyLineConnected 不够：如果自报的桶宽/探测间隔本来就不小于
// 实际点距，那条曲线在"只信自报值"的实现下也是连着的 —— 这一组什么也没证明到。
// 所以先把前提钉死：自报桶宽 60 秒、自报探测间隔 60 秒、手机端聚合没开、
// 而实际点距 300 秒（> 1.5 × 60）。四个数一起才说明"能救场的只有实测中位数"。
func checkMeasuredSpacingRescues(t *testing.T, res latResult) {
	t.Helper()
	if len(res.Phases) == 0 {
		t.Fatal("自报值不符那一组一个场景都没量到")
	}
	if res.Mobile {
		t.Errorf("这一组应当在桌面宽度下跑：窄屏会多出一个自报项（手机端聚合目标），"+
			"就说不清到底是哪一项救回了这条曲线（mobile=%v）", res.Mobile)
	}
	p := res.Phases[0]
	if p.SourceBucket != 60 {
		t.Errorf("自报的桶宽 = %d 秒，期望 60（6h 档）：前提不对，这条用例验证的不是「自报值与实际点距不符」",
			p.SourceBucket)
	}
	if res.ProbeInterval != 60 {
		t.Errorf("自报的探测间隔 = %d 秒，期望 60：前提不对（这一项也是自报值之一）", res.ProbeInterval)
	}
	if p.SourceSpacing != 300 {
		t.Errorf("实际相邻两点间距 = %.0f 秒，期望 300：数据不是按 300 秒一行播撒的",
			p.SourceSpacing)
	}
	if p.SourceSpacing <= float64(p.SourceBucket)*1.5 {
		t.Errorf("实际点距 %.0f 秒没有超过 1.5 × 自报桶宽（%d 秒）：这组数据在"+
			"「只信自报值」的实现下也会是连着的，这条用例什么也没验证到",
			p.SourceSpacing, p.SourceBucket)
	}
	t.Logf("自报桶宽 %d 秒、自报探测间隔 %d 秒，而实际点距 %.0f 秒（> 1.5 × %d）："+
		"%d 个点仍然画出了 %d 段线、%d 个孤立圆点 —— 救回这条曲线的只能是实测点距中位数",
		p.SourceBucket, res.ProbeInterval, p.SourceSpacing, p.SourceBucket,
		p.SourcePoints, p.LineTo, p.Arc)
}

// checkLatencyNeverDrawsPeakLine 核对"延迟图不画峰值淡线，但资源图照旧画"。
//
// 峰值淡线是画布上**唯一**用 0.28 透明度描边的东西（chart.js 的 draw() 里那句
// drawLine(..., 2, ..., 0.28, ...)）。数透明度比数颜色可靠：资源图与延迟图的线色
// 可能撞在一起，而 alpha 是这条线独有的指纹。
//
// 两半都要断言：只断言"延迟图上没有"的话，把引擎里画峰值线的那段整个删掉也能过 ——
// 那样资源图（CPU/内存/磁盘/网络，showMax 仍然是 true）的峰值线就一起没了，
// 而用户只说了延迟图。
func checkLatencyNeverDrawsPeakLine(t *testing.T, res latResult) {
	t.Helper()
	p := res.Peak
	t.Logf("这一帧的 0.28 透明度描边：延迟图 %d 条（期望 0）、CPU 图 %d 条（期望 ≥ 1）",
		p.LatPeakStrokes, p.CPUPeakStrokes)
	if p.LatPeakStrokes != 0 {
		t.Errorf("延迟图上画了 %d 条峰值淡线（0.28 透明度）：用户要求峰值线永远不画"+
			"（「默认不显示峰值线，把按钮也去掉吧」），这条线必须彻底消失", p.LatPeakStrokes)
	}
	if p.CPUPeakStrokes < 1 {
		t.Error("资源图（CPU）的峰值淡线不见了：用户只说了延迟图，其余五张图的峰值线行为一个字都不许动" +
			"（CPU 图的数据里 max > avg，showMax: true 时必然画出那条 0.28 的线）")
	}
}

// checkLatencyAxisFitsMean 核对 Y 轴**不再为峰值留空间**。
//
// 为什么用"轴上限 vs 服务端峰值"来验：轴只按平均线缩放时，上限会落在平均线的
// 量级（数据里均值 20~39ms、峰值是均值的 6 倍以上，两个量级差得很开），
// 被峰值顶上去时上限至少是峰值那个量级。这是"上面留白太多"在画面上唯一可观测的差别
// —— 只数线条数量的话，两种状态下都只有一条平均线。
func checkLatencyAxisFitsMean(t *testing.T, res latResult) {
	t.Helper()
	p := res.Peak
	if p.YTop <= 0 {
		t.Fatalf("没有量到延迟图的 Y 轴刻度：轴上一个数字都没抓到（hoverPeak=%q）", p.HoverPeak)
	}
	if p.SourcePeakMS <= 0 {
		t.Fatalf("没有从 /ping 拿到峰值（sourcePeakMs=%v）：这组数据证明不了「轴不为峰值留空间」", p.SourcePeakMS)
	}
	t.Logf("Y 轴上限 = %d ms；服务端这一档的峰值 = %.0f ms（均值的 6 倍以上）", p.YTop, p.SourcePeakMS)
	// 峰值是均值的 6 倍以上（见 seedPingBuckets）：轴只要把峰值算进去，上限必然
	// 落在峰值附近。用"不到峰值的一半"来判，比钉一个绝对数更抗数据波动。
	if float64(p.YTop) >= p.SourcePeakMS/2 {
		t.Errorf("Y 轴上限 = %d ms，已经接近服务端峰值 %.0f ms：轴又把峰值算进去了 —— "+
			"用户抱怨的「图表上面的留白」就是这么来的", p.YTop, p.SourcePeakMS)
	}
}

// checkLatencyChipsHaveNoPeak 核对开关那一行只剩三个，「峰值线」彻底没了。
//
// 只说"少了峰值线"是不够的：顺手多出一个别的 chip（比如把 peak 改成"峰值"）同样要挡住
// —— 所以这里逐个比对整行文案与顺序。
func checkLatencyChipsHaveNoPeak(t *testing.T, res latResult) {
	t.Helper()
	want := []string{"延迟", "丢包", "平滑曲线"}
	got := res.Peak.Chips
	t.Logf("延迟卡开关那一行 = %v（期望 %v）", got, want)
	if len(got) != len(want) {
		t.Fatalf("开关那一行有 %d 个 chip（%v），期望 %d 个：%v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 chip = %q，期望 %q（顺序就是页面顺序）", i+1, got[i], want[i])
		}
	}
	for _, label := range got {
		if strings.Contains(label, "峰值") {
			t.Errorf("开关那一行还有 %q：用户要求把「峰值线」这个按钮去掉", label)
		}
	}
}

// checkHoverStillShowsPeak 核对**悬浮读数里的峰值那一行还在**。
//
// 这是用户明确点名要保留的那一件事（"我需要保留移到上面的时候也能显示峰值"），
// 也是这次改动最容易在后续清理里被顺手删掉的一行 —— 画面上少一行浮层文本
// 一点征兆都没有，只有把鼠标移上去逐行读才发现，所以必须在真浏览器里读浮层。
func checkHoverStillShowsPeak(t *testing.T, res latResult) {
	t.Helper()
	p := res.Peak
	if p.HoverPeak == "" {
		t.Fatalf("悬浮浮层里没有「峰值 N ms」这一行（这一帧画在浮层里的行 = %v）："+
			"峰值线可以不画，但用户要求悬浮里仍然能看到峰值", p.HoverRows)
	}
	t.Logf("悬浮浮层里的行 = %v（峰值那一行 = %q）", p.HoverRows, p.HoverPeak)
}

// checkStaleViewKeyIsIgnored 核对"老浏览器里存着的那份旧开关结构"不会出事。
//
// 删掉「峰值线」之后，localStorage 里存过的 peak 键就是一份没人认识的状态。
// 这里有两件事要一起成立（harness 一开始就预置了 {mean,loss,peak,smooth} 四键）：
//
//  1. **读**到它不能白屏、不能报错 —— 后面所有场景（详情页、曲线、悬浮）照常跑完，
//     而且 res.Errs 是空的（runLatHarness 已经在别处断言）；三个开关也必须还在，
//     不能被那一份畸形结构连累丢掉；
//  2. **写**回去时那个残留键要消失：用户切一下开关之后，存着的就是三键对象。
func checkStaleViewKeyIsIgnored(t *testing.T, res latResult) {
	t.Helper()
	if strings.Contains(res.Peak.StoredView, "peak") {
		t.Errorf("切过开关之后 localStorage 里还是 %q：残留的 peak 键应当随写回消失"+
			"（setLatView 写的就是 latView 读出来的那份三键对象）", res.Peak.StoredView)
	}
	for _, key := range []string{"mean", "loss", "smooth"} {
		if !strings.Contains(res.Peak.StoredView, key) {
			t.Errorf("切过开关之后存着的是 %q，少了 %s：旧结构只是多了一个键，"+
				"另外三个用户选择必须原样保留", res.Peak.StoredView, key)
		}
	}
	t.Logf("预置了旧的四个键（含 peak）之后切一下开关，localStorage = %s", res.Peak.StoredView)
}

// check7dHoverShowsInterval 核对"7d 档（桶宽 900 秒）的悬浮必须写成时间段"。
//
// 这是被一次真实的回归逼出来的：悬浮区间的阈值原来写死 3600 秒（当时的 7d 桶宽），
// 桶宽改细成 900 之后 7d 档的悬浮退回了单个时刻 —— 用户要的"看得到这一段有多长"
// 被悄悄抵消一半，而页面上一点异常都没有。静态断言只能钉住那个常量，
// "画出来到底有没有那一段区间"必须在真浏览器里读浮层。
func check7dHoverShowsInterval(t *testing.T, res latResult) {
	t.Helper()
	h := res.Hover7d
	if h.RangeKey == "" {
		t.Fatal("没有取到 7d 档的悬浮观测")
	}
	t.Logf("7d 档悬浮：点 ts=%d（桶宽 %d 秒）→ 浮层第一行 %q；这一帧画在浮层里的行 = %v",
		h.TargetTS, h.Bucket, h.Time, h.Rows)
	if h.Time == "" {
		t.Fatalf("7d 档的悬浮浮层没有写成时间段（拿到的行：%v）—— "+
			"桶宽 900 秒 > 最细的历史桶 60 秒，就该显示 08:30–08:45 这样的区间", h.Rows)
	}
	// 区间的右端必须正好是"起点 + 桶宽"（开区间）：用页面上那个 ts 反推。
	// 这里只钉形状（HH:MM–HH:MM），具体几分钟由上面的 bucket 与 Go 侧的阈值共同保证。
	if !regexp.MustCompile(`^([0-9]{2}-[0-9]{2} )?[0-9]{2}:[0-9]{2}–([0-9]{2}-[0-9]{2} )?[0-9]{2}:[0-9]{2}$`).MatchString(h.Time) {
		t.Errorf("7d 档悬浮的时间行 = %q，不是「起点–终点」的形状", h.Time)
	}
}

// createPingTarget 走真接口配一个探测目标（顺带把探测间隔设成 intervalSec），
// 返回目标的 ID。
//
// 第二次调用时目标按 (type, host, port) 复用同一个 id（服务端就是这么匹配的），
// 所以只改间隔、不会把曲线身份换掉。
func createPingTarget(t *testing.T, br *browser, label, host string, port, intervalSec int) int64 {
	t.Helper()
	status, body := br.do("PUT", "/api/v1/settings/ping", map[string]any{
		"interval_sec": intervalSec,
		"targets": []map[string]any{
			{"label": label, "type": "tcp", "host": host, "port": port, "enabled": true},
		},
	}, true)
	if status != 200 {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}
	targets, _ := body["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("探测目标数量 = %d，期望 1", len(targets))
	}
	first, _ := targets[0].(map[string]any)
	id, _ := first["id"].(float64)
	if id <= 0 {
		t.Fatalf("探测目标 ID 不合法: %v", first)
	}
	if got, _ := body["interval_sec"].(float64); int(got) != intervalSec {
		t.Fatalf("探测间隔 = %v，期望 %d", body["interval_sec"], intervalSec)
	}
	return int64(id)
}

// seedPingBuckets 按 step 秒的间距喂一段延迟曲线（默认场景就是**真实的 1 分钟桶**）。
//
// 平均值在 20~39ms 之间小幅起伏，峰值给到平均值的 6 倍以上 ——
// 这样"轴要不要算峰值"在画面上一眼可辨（默认关掉之后轴会矮一大截）。
//
// step 就是"探测间隔"：300 秒一行时，60 秒宽的桶里每 5 个才有 1 个有数据 ——
// 这正是"探测间隔 > 桶宽"要复现的情形。
func seedPingBuckets(t *testing.T, h *harness, nodeID, targetID int64, step int, span time.Duration) {
	t.Helper()
	now := time.Now()
	end := now.Unix() - now.Unix()%int64(step) - int64(step)
	start := end - int64(span.Seconds())
	var buckets []store.PingBucket
	for ts := start; ts <= end; ts += int64(step) {
		i := (ts / int64(step)) % 20
		avg := float64(20 + i) // 20…39 ms
		peak := avg*6 + 30     // 150…264 ms
		buckets = append(buckets, store.NewPingBucket(nodeID, targetID, ts, avg, avg-5, peak, 0))
	}
	if len(buckets) < 20 {
		t.Fatalf("播撒的探测桶只有 %d 个，太少", len(buckets))
	}
	if err := h.db.UpsertPingBuckets(context.Background(), buckets); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	t.Logf("播撒探测桶 %d 个（节点 %d，ts %d … %d，步长 %d 秒）", len(buckets), nodeID, start, end, step)
}

// latResult 是窄屏延迟图那条用例的观测值。
type latResult struct {
	Errs     []string `json:"errs"`
	Fatal    string   `json:"fatal"`
	Steps    []string `json:"steps"`
	Mobile   bool     `json:"mobile"`
	Scenario string   `json:"scenario"`
	// ProbeInterval 是**自报**的探测间隔（/settings 的 ping.interval_sec）。
	// "自报值与实际点距不符"那一组要靠它证明前提：这一项与实际点距对不上。
	ProbeInterval int `json:"probeInterval"`

	Phases  []latPhase `json:"phases"`
	Peak    latPeak    `json:"peak"`
	Hover7d latHover   `json:"hover7d"`
}

type latPhase struct {
	Name          string  `json:"name"`
	ResourceKey   string  `json:"resourceKey"`
	LatKey        string  `json:"latKey"`
	SourcePoints  int     `json:"sourcePoints"`
	SourceBucket  int     `json:"sourceBucket"`
	SourceSpacing float64 `json:"sourceSpacing"`
	Arc           int     `json:"arc"`
	LineTo        int     `json:"lineTo"`
	MoveTo        int     `json:"moveTo"`
}

// latHover 记一次悬浮观测：鼠标停在哪个点上、浮层第一行写的是什么。
type latHover struct {
	RangeKey string   `json:"rangeKey"`
	TargetTS int64    `json:"targetTS"`
	Bucket   int      `json:"bucket"`
	Time     string   `json:"time"`
	Rows     []string `json:"rows"`
}

// latPeak 是"峰值线永远不画、但悬浮里要留着峰值"这一组的观测值。
type latPeak struct {
	// Chips 是延迟卡那一行开关的文案（顺序即页面顺序）。
	Chips []string `json:"chips"`
	// LatPeakStrokes 是延迟图这一帧里 0.28 透明度描边的次数（峰值淡线的指纹）—— 必须是 0。
	LatPeakStrokes int `json:"latPeakStrokes"`
	// CPUPeakStrokes 是资源图（CPU）同一帧里的同类描边次数 —— 必须 ≥ 1（用户只动了延迟图）。
	CPUPeakStrokes int `json:"cpuPeakStrokes"`
	// YTop 是延迟图 Y 轴上限；SourcePeakMS 是服务端这一档里峰值的最大值。
	// 轴只为平均线留空间时，前者应当远小于后者。
	YTop         int     `json:"yTop"`
	SourcePeakMS float64 `json:"sourcePeakMs"`
	// HoverRows 是悬浮浮层里的行；HoverPeak 是其中「峰值 N ms」那一行。
	HoverRows []string `json:"hoverRows"`
	HoverPeak string   `json:"hoverPeak"`
	// StoredView 是"点一下开关之后"localStorage 里存着的那份状态：删掉「峰值线」
	// 之后它应当只剩三个键（残留的 peak 键随写回消失），而**读**到旧的四键结构
	// 不能出错（runAgg 一开始就预置了一份旧的）。
	StoredView string `json:"storedView"`
}

// latHarnessJS 是延迟图那条用例的自检脚本。
//
// 它只做两件事：**按用户的真实操作切档位/悬浮**，以及**把 canvas 上真正发生的
// 绘制调用数出来**（arc = 孤立圆点、lineTo = 线段、stroke 按透明度分桶 =
// 峰值淡线、以及 Y 轴刻度上的数字）。
// 断言全在 Go 那边：什么算"连成一条线"、什么算"轴没为峰值留空间"是测试的判断，
// 不是页面的。
const latHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = { errs: [], fatal: '', steps: [], mobile: false, scenario: CFG.scenario || 'agg',
            probeInterval: 0, phases: [], peak: {}, hover7d: {} };
  window.__LATRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // ---- 钩 canvas：这一帧画了几个点、几段线、Y 轴最高刻度是多少 ----------
  var counts = {};
  var yLabels = [];
  var allTexts = [];   // 悬浮浮层的行也在这里面（浮层是这一帧最后画的东西）
  var strokes = {};    // canvas id → 这一帧的描边统计（按透明度分桶）
  function bucketOf(canvas) {
    var id = canvas && canvas.id ? canvas.id : '?';
    return counts[id] || (counts[id] = { arc: 0, lineTo: 0, moveTo: 0 });
  }
  function strokeOf(canvas) {
    var id = canvas && canvas.id ? canvas.id : '?';
    return strokes[id] || (strokes[id] = { total: 0, peak: 0 });
  }
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    // allCanvases=true 的钩子对**每张画布**都计数（描边要同时看延迟图与 CPU 图：
    // "只动了延迟图"这件事只有两边一起数才说得清）。其余钩子只数延迟图。
    function wrap(name, hit, allCanvases) {
      var orig = proto[name];
      proto[name] = function () {
        if (allCanvases || (this.canvas && this.canvas.id === 'chart-lat')) hit(this, arguments);
        return orig.apply(this, arguments);
      };
    }
    wrap('arc', function (ctx) { bucketOf(ctx.canvas).arc++; });
    wrap('lineTo', function (ctx) { bucketOf(ctx.canvas).lineTo++; });
    wrap('moveTo', function (ctx) { bucketOf(ctx.canvas).moveTo++; });
    // 描边：按**透明度**分桶。峰值淡线是画布上唯一用 0.28 画的线
    // （chart.js 的 draw()：drawLine(..., 2, ..., 0.28, ...)），
    // 数它比数颜色可靠 —— 资源图与延迟图的线色可能撞在一起。
    wrap('stroke', function (ctx) {
      var s = strokeOf(ctx.canvas);
      s.total++;
      if (Math.abs(ctx.globalAlpha - 0.28) < 0.001) s.peak++;
    }, true);
    wrap('fillText', function (ctx, args) {
      var text = String(args[0]);
      allTexts.push(text);
      // Y 轴刻度是右对齐画在 g.left - 6 = 58 处的纯数字（延迟图的 yFormat 就是 v.toFixed(0)）。
      // 悬浮浮层里的数字带单位（"12 ms"），过不了这个正则。
      if (Math.abs(args[1] - 58) < 1.5 && /^[0-9]+$/.test(text)) yLabels.push(parseInt(text, 10));
    });
  })();
  function resetCounts() {
    counts['chart-lat'] = { arc: 0, lineTo: 0, moveTo: 0 };
    strokes = {};
    yLabels = [];
    allTexts = [];
  }
  function strokeStatsOf(id) {
    return strokes[id] || { total: 0, peak: 0 };
  }
  function snapshot() {
    var c = bucketOf(document.getElementById('chart-lat'));
    var top = 0;
    yLabels.forEach(function (v) { if (v > top) top = v; });
    return {
      arc: c.arc, lineTo: c.lineTo, moveTo: c.moveTo, yTop: top,
      latPeakStrokes: strokeStatsOf('chart-lat').peak,
      cpuPeakStrokes: strokeStatsOf('chart-cpu').peak
    };
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

  // ---- 取数完成的通知：/ping 每次回来都记一笔 ----------------------------
  var pingDone = 0;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    return rawFetch(input, init).then(function (res) {
      if (/\/ping\?range=/.test(url)) pingDone++;
      return res;
    });
  };

  function clickRange(containerId, key) {
    var box = node(containerId);
    if (!box) throw new Error('找不到档位容器 ' + containerId);
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent === key) { btns[i].click(); return true; }
    }
    throw new Error(containerId + ' 里没有档位按钮 ' + key);
  }

  // chips 返回延迟卡那一行开关的按钮（顺序即页面顺序）。
  //
  // 这里曾经有一个 peakChip()（按文案「峰值线」找那个开关并点击）：chip 已经删掉，
  // 现在要用的是"整行到底有哪几个"—— 找不到「峰值线」正是这条用例要断言的事。
  function chips() {
    var row = document.querySelector('#lat-targets .lat-chips');
    if (!row) throw new Error('找不到开关那一行');
    var out = [];
    var btns = row.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) out.push(btns[i]);
    return out;
  }

  // redrawAndMeasure 强制重画一帧，再数这一帧的绘制调用。
  // 触发方式是 window 的 resize（图表引擎监听它，80ms 防抖后整张重画）——
  // 这样"被数的那一帧"是我们自己触发的，不会数到半张图。
  function redrawAndMeasure() {
    resetCounts();
    window.dispatchEvent(new Event('resize'));
    return sleep(600).then(snapshot);
  }

  // 切**延迟卡**的档位：等 /ping 回来 + 等页面把这一帧画完，再数。
  function switchLatRange(key) {
    var want = pingDone + 1;
    clickRange('lat-ranges', key);
    return waitFor('延迟曲线重新取数（' + key + '）', function () { return pingDone >= want; }, 20000)
      .then(function () { return sleep(300); });
  }

  // 切**资源卡**的档位：它只重取 /series，不发 /ping —— 等一小会儿就够了。
  function switchResourceRange(key) {
    clickRange('detail-ranges', key);
    return sleep(500);
  }

  // measure 记录一个场景：这一档位下服务端给了多少点（以及点距）、这一帧画了什么。
  function measure(name, resourceKey, latKey) {
    return redrawAndMeasure().then(function (m) {
      return getJSON('/api/v1/nodes/' + CFG.nodeID + '/ping?range=' + latKey).then(function (data) {
        var meta = data.meta || {};
        var targets = data.targets || [];
        var pts = (targets[0] && targets[0].points) || [];
        // 服务端相邻两点的实际间距：它才是断线判据要比的那个数
        // （探测间隔 > 桶宽时，点距是探测间隔而不是桶宽）。
        var spacing = pts.length > 1 ? pts[1][0] - pts[0][0] : 0;
        R.phases.push({
          name: name, resourceKey: resourceKey, latKey: latKey,
          sourcePoints: pts.length, sourceBucket: meta.bucket_sec || 0,
          sourceSpacing: spacing,
          arc: m.arc, lineTo: m.lineTo, moveTo: m.moveTo
        });
        return true;
      });
    });
  }

  // hoverMid 把鼠标停在这条曲线的中间那个点上，把浮层第一行（时间）读出来。
  //
  // 横坐标按图表引擎的线性映射算：x = left(64) + (ts-t0)/(t1-t0) * (w-left-right)。
  function hoverMid(rangeKey, bucket) {
    var canvas = node('chart-lat');
    var rect = canvas.getBoundingClientRect();
    var w = Math.round(rect.width);
    return getJSON('/api/v1/nodes/' + CFG.nodeID + '/ping?range=' + rangeKey).then(function (data) {
      var pts = (data.targets && data.targets[0] && data.targets[0].points) || [];
      if (pts.length < 3) throw new Error(rangeKey + ' 档只有 ' + pts.length + ' 个点，没法悬浮');
      var t0 = pts[0][0], t1 = pts[pts.length - 1][0];
      var mid = pts[Math.floor(pts.length / 2)][0];
      var plotW = w - 64 - 8;
      var px = 64 + (mid - t0) / Math.max(t1 - t0, 1) * plotW;
      if (px > w - 10) px = w - 10;
      if (px < 70) px = 70;
      var before = allTexts.length;
      canvas.dispatchEvent(new MouseEvent('mousemove', {
        clientX: rect.left + px, clientY: rect.top + 60, bubbles: true
      }));
      var rows = allTexts.slice(before);
      canvas.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }));
      R.hover7d = {
        rangeKey: rangeKey, targetTS: mid, bucket: bucket, rows: rows,
        // 浮层第一行就是时间（chart.js 的 drawHover 里 rows[0]）。
        // 区间两端跨天时形如 "09-29 23:30–09-30 00:30"，否则是 "16:30–16:45"。
        time: rows.filter(function (t) { return /^([0-9]{2}-[0-9]{2} )?[0-9]{2}:[0-9]{2}–/.test(t); })[0] || ''
      };
      // 浮层里的行原样返回：调用方自己决定要哪一行（7d 档要时间区间、
      // 峰值那一组要「峰值 N ms」）——判定全在 Go 侧。
      return rows;
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

  function openDetail() {
    window.location.hash = '#/n/' + CFG.nodeID;
    return waitFor('详情页打开', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000)
      .then(function () {
        // 延迟卡自己那组档位要等 /nodes/{id} 回来才渲染出来。
        return waitFor('延迟档位按钮就绪', function () {
          return node('lat-ranges') && node('lat-ranges').querySelectorAll('button').length > 0;
        }, 30000);
      })
      .then(function () {
        // 探测目标的卡片与那一行开关是 /ping 回来之后才渲染的 —— 它出现了就说明
        // 数据已经到手（此时 canvas 上至少已经有横网格线，lineTo > 0）。
        return waitFor('探测目标与开关就绪', function () {
          return !!document.querySelector('#lat-targets .lat-chips');
        }, 30000);
      })
      .then(function () { return sleep(400); });
  }

  // peakPass 是场景 0：峰值线永远不画、Y 轴只为平均线留空间、chip 行只剩三个、
  // 悬浮浮层里仍然有峰值，以及**老浏览器里存着的那份旧开关结构**（四个键、含 peak）
  // 不会把页面弄坏。
  //
  // 为什么要在**同一帧**里同时数延迟图与 CPU 图的 0.28 描边：只数延迟图的话，
  // "把引擎里画峰值线那段整个删掉"也能过 —— 而用户只说了延迟图。
  function peakPass() {
    return redrawAndMeasure().then(function (m) {
      R.peak.latPeakStrokes = m.latPeakStrokes;
      R.peak.cpuPeakStrokes = m.cpuPeakStrokes;
      R.peak.yTop = m.yTop;
      R.peak.chips = chips().map(function (b) { return b.textContent; });
      R.steps.push('数完这一帧的描边与开关行');
      // 服务端这一档里峰值的最大值：用来证明 Y 轴确实没把它算进去
      // （轴只按平均线缩放时，上限会落在平均线的量级，与峰值差一个数量级）。
      return getJSON('/api/v1/nodes/' + CFG.nodeID + '/ping?range=1h');
    }).then(function (data) {
      var pts = (data.targets && data.targets[0] && data.targets[0].points) || [];
      var peak = 0;
      pts.forEach(function (p) { if (p[2] > peak) peak = p[2]; });
      R.peak.sourcePeakMs = peak;
      // 进详情页时的默认延迟档位就是 1h（档位表第一项）。
      return hoverMid('1h', 60);
    }).then(function (rows) {
      R.peak.hoverRows = rows;
      // 浮层里那一行的原文形如 "  峰值 260 ms"（chart.js 的 drawHover 里带两个空格缩进）。
      R.peak.hoverPeak = rows.filter(function (t) { return /峰值\s+[0-9]/.test(t); })[0] || '';
      // 旧结构里的 peak 键：用户切一下任意一个开关，写回去的就是三键对象
      // （读的时候多出来的键被忽略 —— 见 app.js 的 latView / LAT_VIEW_DEFAULT）。
      // 两次点击把它切回原状，后面的场景不受影响。
      var smooth = chips().filter(function (b) { return b.textContent === '平滑曲线'; })[0];
      if (!smooth) throw new Error('开关行里没有「平滑曲线」');
      smooth.click();
      R.peak.storedView = localStorage.getItem('probe-ping-view-v1') || '';
      smooth.click();
      return true;
    });
  }

  // scenario 'agg'：手机端二次聚合 + 断线判据（两卡档位不同、聚合目标大于桶宽）。
  function runAgg() {
    // 老浏览器里存着的那份**旧开关结构**（删掉「峰值线」之前是四个键、含 peak）：
    // 原样写一份进去再走完整个流程 —— 读到不认识的键必须被安全忽略，
    // 不能白屏、不能报错、也不能把另外三个开关一起丢掉。
    try {
      localStorage.setItem('probe-ping-view-v1', JSON.stringify({ mean: true, loss: true, peak: true, smooth: false }));
      R.steps.push('预置旧的四个键开关结构（含 peak）');
    } catch (e) {
      R.steps.push('localStorage 不可写，跳过旧结构预置: ' + e.message);
    }
    return openDetail()
      // ---- 场景 0：峰值线永远不画（资源图照旧）+ 轴不为峰值留空间 + 悬浮里仍有峰值
      .then(peakPass)
      // ---- 场景 1：资源图与延迟图都停在 6h（手机端聚合目标 120 秒 > 桶宽 60 秒）
      .then(function () { return switchResourceRange('6h'); })
      .then(function () { return switchLatRange('6h'); })
      .then(function () { return measure('两卡都 6h', '6h', '6h'); })
      // ---- 场景 2：资源图切到 7d，延迟图仍停在 6h（聚合目标必须还是 6h 那一份）
      .then(function () { return switchResourceRange('7d'); })
      .then(function () { return switchLatRange('12h'); })
      .then(function () { return switchLatRange('6h'); })
      .then(function () { return measure('资源 7d + 延迟 6h', '7d', '6h'); })
      // ---- 场景 3：7d 档（桶宽 900 秒）的悬浮必须写成时间段，不是一个时刻
      .then(function () { return switchLatRange('7d'); })
      .then(function () { return hoverMid('7d', 900); });
  }

  // scenario 'interval'：探测间隔（300 秒）比桶宽（6h 档 60 秒）大 5 倍时，
  // 每个桶里只有 1/5 有行 —— 曲线仍然必须连成一条线。
  function runInterval() {
    return openDetail()
      .then(function () { return switchLatRange('6h'); })
      .then(function () { return measure('探测间隔 300 秒 + 6h 档', '1h', '6h'); });
  }

  // scenario 'mismatch'：自报的桶宽（60 秒）与实际点距（300 秒）不符 ——
  // 三个自报项（桶宽 60 / 探测间隔 60 / 桌面端聚合 0）全都解释不了这个间距，
  // 只有「实测中位数」能把它抬到 300，曲线才连得起来。
  //
  // 自报的探测间隔从 /settings 读一遍记下来：Go 那边要拿它证明"这一项也是自报的、
  // 而且与实际点距对不上"，否则这组数据可能本来就被别的自报项解释掉了。
  function runMismatch() {
    return openDetail()
      .then(function () { return getJSON('/api/v1/settings'); })
      .then(function (cfg) {
        R.probeInterval = (cfg.ping && cfg.ping.interval_sec) || 0;
        R.steps.push('记下自报的探测间隔 ' + R.probeInterval + ' 秒');
        return switchLatRange('6h');
      })
      .then(function () { return measure('自报桶宽 60 秒 / 实际点距 300 秒', '1h', '6h'); });
  }

  function run() {
    R.mobile = window.matchMedia('(max-width: 640px)').matches;
    var scenario = CFG.scenario || 'agg';
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(function () {
        if (scenario === 'interval') return runInterval();
        if (scenario === 'mismatch') return runMismatch();
        return runAgg();
      });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:lat'; });
  }

  window.addEventListener('load', function () {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`
