package ui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/alert"
	"probe/internal/e2e/harness"
	"probe/internal/store"
)

// 「流量计费方式」（双向 / 只算出站 / 只算入站）的真浏览器验收。
//
// 为什么必须真跑一遍：这次改动的效果**全部**落在 DOM 与 computed style 上 ——
//   - 卡片上「流量」那一格的标签要跟着口径换词，而数字/百分比要跟着换口径；
//   - 进度条的档位跟的是**重算后**的百分比（只算出站的机器不能按收 + 发变色）；
//   - 详情页三行的方向读数里**只有计费方向那一段**带 .billed（双向两段都带），
//     "标色真的落到哪一段上"只有 computed style 说了算；
//   - 方向读数被拆成了两个子 span，原来那一个生效空格的宽度必须由 CSS 的 gap
//     补回来（否则详情页三行各窄 3.8px，而那在静态断言里只是"少了个 gap"）。
//
// billingScene 是一台机器的现场：口径 + 额度 + 本周期收/发 + 期望读数。
type billingScene struct {
	name    string
	billing int
	limit   int64
	rx, tx  int64
	// wantLabel 是卡片上那一格的标签（用户逐条确认的用词：甲）。
	wantLabel string
	// wantUsed 是本周期**按口径**的已用量（服务端算，前端不许自己加）。
	wantUsed int64
	// wantBar 是进度条的档位类名（'' = 默认绿，见 quotaBarClass）。
	wantBar string
}

// 三台机器各自的额度与收/发都是挑过的：
//
//	双向：30.0 GB / 100 GB ⇒ 30.0%（绿档）—— 默认口径那一档
//	出站：只数发 120 GB / 100 GB ⇒ 120.0%（红档）—— ★ 合计 175 GB 与计费 120 GB
//	      **不是同一个数**，正是「本周期」那一行必须两个都写的理由
//	入站：只数收 341 GB / 400 GB ⇒ 85.25% ⇒ 页面上写 **85.3%**（橙档）
//
// 入站那台的额度（400 GB）与用量（341 GB）是特意凑成 85.25% 的：这是 Go 的 %.1f
// 与浏览器 toFixed 唯一会给出不同字符串的地方（85.2% vs 85.3%）。日报里的百分比
// 必须与页面上一模一样，所以这一档的期望值也按浏览器的规则算（见 jsPct1）。
var billingScenes = []billingScene{
	{"双向流量机器", store.BillingBoth, 100e9, 10e9, 20e9, "双向流量", 30e9, ""},
	{"出站流量机器", store.BillingTxOnly, 100e9, 55e9, 120e9, "出站流量", 120e9, "bad"},
	{"入站流量机器", store.BillingRxOnly, 400e9, 341e9, 5e9, "入站流量", 341e9, "warn"},
}

// billingNoLimitScene 是一台**没填月额度**的机器（limit = 0）。
//
// 它守的是这一轮新补那一段的**反面**：`（Z%）· 额度 L` 在 limit = 0 时整段都不许
// 出现 —— 既不能写出「额度 0 B」（读起来像"额度用光了"，而事实是没填），也不能
// 写一个没有分母的百分比。口径取"只算出站"，这样「合计」与「计费」本来就不是
// 同一个数：顺带证明去掉额度那一段之后，前面两段仍然是完整的。
var billingNoLimitScene = billingScene{
	name: "无额度机器", billing: store.BillingTxOnly, limit: 0,
	rx: 7e9, tx: 3e9, wantLabel: "出站流量", wantUsed: 3e9, wantBar: "",
}

// billingExpected 是一条场景在浏览器里应当看到的全部读数。
type billingExpected struct {
	label    string
	pct      string
	sub      string
	barClass string
	barWidth string
	// todaySum / weekSum 是今日 / 本周两行右边的整段文案：合计 + **按计费口径**的
	// 占比（分母是月额度）。两行的百分比这一轮起跟口径走（用户拍板 ②）。
	todaySum string
	weekSum  string
	// cycleSum 是本周期那一行右边的整段文案：合计 X · 计费 Y（Z%）· 额度 L
	// （用户拍板 ③；额度那一段是第三轮补回来的，用户 2026-10-05 拍板 ①：
	// 「额度不应该消失」）。
	cycleSum string
	// billedSegs 是**计费方向**落在第几段上（0 = ↓ 收、1 = ↑ 发）：
	// 双向两段都标，只算出站标第二段，只算入站标第一段。
	billedSegs []int
}

// jsPct1 按**浏览器** Number.prototype.toFixed(1) 的规则算一位小数百分比。
//
// 为什么期望值这一侧也要复刻浏览器：面板上的百分比是浏览器渲染的
// （app.js 里写的就是 (pct).toFixed(1)），而 Go 的 fmt %.1f 对"恰好落在 .x5 上"
// 的二进制精确值取偶（85.25 → 85.2）。期望值要是跟着 %.1f 走，入站那台就会
// 断言出 85.2%，而真实页面写的是 85.3% —— 那就成了一个永远绿的用例。
func jsPct1(pct float64) string {
	n := math.Floor(pct * 10)
	if pct*10-n >= 0.5 {
		n++
	}
	return strconv.FormatFloat(n/10, 'f', 1, 64) + "%"
}

func billingWant() map[string]billingExpected {
	out := make(map[string]billingExpected, len(billingScenes))
	for _, s := range billingScenes {
		pct := float64(s.wantUsed) / float64(s.limit) * 100
		pctText := jsPct1(pct)
		segs := []int{0, 1}
		switch s.billing {
		case store.BillingTxOnly:
			segs = []int{1}
		case store.BillingRxOnly:
			segs = []int{0}
		}
		// 今日 / 本周只有一段"合计（占比）"：三台机器的流量都灌在今天，
		// 所以今日 = 本周 = 本周期（今日 ⊆ 本周 ⊆ 本周期）。
		daySum := alert.FormatBytes(s.rx+s.tx) + "（" + pctText + "）"
		out[s.name] = billingExpected{
			label: s.wantLabel,
			pct:   pctText,
			// 面板的字节写法（app.js 的 fmtBytesDec）与告警文案（alert.FormatBytes）
			// 是**同一套口径**（十进制 + 同一套小数位）：两处漂移的话，用户没法把
			// 告警里的数与面板上的数对上账 —— 这条断言顺带盯着这件事。
			sub:      alert.FormatBytes(s.wantUsed) + " / " + alert.FormatBytes(s.limit),
			barClass: s.wantBar,
			barWidth: fmt.Sprintf("%g%%", math.Min(100, pct)),
			todaySum: daySum,
			weekSum:  daySum,
			// 本周期那一行写的是"这一周期真实的收 + 发"**加上**"按口径记进额度的
			// 用量"（用户拍板：两个都写），占比是后者 ÷ 月额度，末尾再把月额度
			// 写出来（用户第三轮拍板：「额度不应该消失」，它就在自己的百分号后面）。
			cycleSum: "合计 " + alert.FormatBytes(s.rx+s.tx) + " · 计费 " +
				alert.FormatBytes(s.wantUsed) + "（" + pctText + "）· 额度 " +
				alert.FormatBytes(s.limit),
			billedSegs: segs,
		}
	}
	return out
}

func TestTrafficBillingModesInRealBrowser(t *testing.T) {
	chrome := harness.FindChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	seeded := seedBillingNodes(t, ctx, dbPath, billingScenes)
	want := billingWant()

	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", dbPath, time.UTC, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))

	// 让三台机器各自**真的连一次**（跑的是仓库里那个假 /proc 的 Agent）。
	//
	// 为什么非要这一步：卡片上那四格在"从未上报过"时整列都是 —（见 updateCard 的
	// neverReported）—— 没有 last_seen 就看不到数字与百分比，而这次要验的正是
	// 那两个数。Agent 的每拍读数来自固定 fixture，第一拍只建基线、之后差值为 0，
	// 所以库里那些精心播撒的流量不会被它改动。
	agentCtx, stopAgents := context.WithCancel(ctx)
	defer stopAgents()
	ids := make(map[string]int64, len(seeded))
	for name, s := range seeded {
		ids[name] = s.id
		client, _ := harness.NewClient(t, "http://"+h.Addr, s.token)
		go func() { _ = client.Run(agentCtx) }()
	}
	harness.WaitFor(t, 30*time.Second, "三台机器都上报过（否则卡片上是 —）", func() bool {
		status, body := br.Do("GET", "/api/v1/nodes", nil, false)
		if status != 200 {
			return false
		}
		nodes, _ := body["nodes"].([]any)
		reported := 0
		for _, raw := range nodes {
			n, _ := raw.(map[string]any)
			if seen, _ := n["last_seen"].(float64); seen > 0 {
				reported++
			}
		}
		return reported == len(seeded)
	})

	cfg := harness.HarnessConfig{
		User: "admin", Pass: harness.AdminPass, Scenario: "billing",
		Nodes: ids, Focus: billingScenes[0].name,
	}
	proxy := harness.NewHarnessProxy(t, "http://"+h.Addr, cfg, harness.BillingHarnessJS)
	mock := harness.NewMockServer(t, proxy)
	raw := harness.RunChromeForResult(t, chrome, mock.URL+"/", proxy.Result, 240*time.Second, "1500,1100")

	var res billingResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, harness.Tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("自检脚本走过的步骤 = %v", res.Steps)
	t.Logf("页面上的颜色令牌（三套主题各自解析后的真值）= %+v", res.Tokens)

	// ---- 1) 卡片：标签 / 数字 / 百分比 / 进度条档位与长度 --------------------
	if len(res.Cards) != len(billingScenes) {
		t.Fatalf("卡片读数 = %d 张，期望 %d 张：%+v", len(res.Cards), len(billingScenes), res.Cards)
	}
	barColors := map[string]string{}
	for _, card := range res.Cards {
		w, ok := want[card.Name]
		if !ok {
			t.Fatalf("浏览器读到了一张没登记过的卡片 %q", card.Name)
		}
		if card.Label != w.label {
			t.Errorf("%s：卡片标签 = %q，期望 %q（标签必须跟着计费口径换词）",
				card.Name, card.Label, w.label)
		}
		if card.Pct != w.pct {
			t.Errorf("%s：卡片百分比 = %q，期望 %q（按计费口径重算）", card.Name, card.Pct, w.pct)
		}
		if card.Sub != w.sub {
			t.Errorf("%s：卡片已用量 = %q，期望 %q（%s）", card.Name, card.Sub, w.sub,
				"服务端按口径算好的 traffic_cycle_used，前端不许自己把收 + 发加起来")
		}
		if card.BarClass != w.barClass {
			t.Errorf("%s：进度条档位 = %q，期望 %q（档位跟的是重算后的百分比）",
				card.Name, card.BarClass, w.barClass)
		}
		if card.BarWidth != w.barWidth {
			t.Errorf("%s：进度条长度 = %q，期望 %q", card.Name, card.BarWidth, w.barWidth)
		}
		barColors[card.BarClass] = card.BarColor
		t.Logf("卡片 %s：标签=%q 百分比=%s 副值=%q 档位=%q 条宽=%s 条色=%s",
			card.Name, card.Label, card.Pct, card.Sub, card.BarClass, card.BarWidth, card.BarColor)
	}
	// 三个档位必须是三种颜色：只有 class 不同、颜色却一样的话，用户在屏幕上
	// 根本看不出档位变了（这正是"档位"这件事的全部意义）。
	if len(barColors) != 3 {
		t.Fatalf("三个档位没有各出现一次：%+v", barColors)
	}
	seen := map[string]string{}
	for class, color := range barColors {
		if other, dup := seen[color]; dup {
			t.Errorf("档位 %q 与 %q 画出来是同一种颜色（%s）：屏幕上分不出档位",
				class, other, color)
		}
		seen[color] = class
	}
	// 绿/橙/红必须真的是 --ok / --warn / --bad，而不是别的什么颜色。
	if got := barColors[""]; got != res.Tokens.OK {
		t.Errorf("默认（绿）档的条色 = %s，期望 --ok = %s", got, res.Tokens.OK)
	}
	if got := barColors["warn"]; got != res.Tokens.Warn {
		t.Errorf("预警（橙）档的条色 = %s，期望 --warn = %s", got, res.Tokens.Warn)
	}
	if got := barColors["bad"]; got != res.Tokens.Bad {
		t.Errorf("超额（红）档的条色 = %s，期望 --bad = %s", got, res.Tokens.Bad)
	}

	// ---- 2) 详情页三行：标色落在哪一段 + 本周期百分比 ----------------------
	if len(res.Detail) != len(billingScenes) {
		t.Fatalf("详情页读数 = %d 台，期望 %d 台", len(res.Detail), len(billingScenes))
	}
	for _, node := range res.Detail {
		w, ok := want[node.Name]
		if !ok {
			t.Fatalf("浏览器读到了一台没登记过的机器 %q", node.Name)
		}
		if len(node.Rows) != 3 {
			t.Fatalf("%s：详情页流量行 = %d 行，期望 3 行（今日 / 本周 / 本周期）", node.Name, len(node.Rows))
		}
		for _, row := range node.Rows {
			if len(row.Segs) != 2 {
				t.Fatalf("%s / %s：方向读数 = %d 段，期望 2 段（拆分之后一个方向一段）",
					node.Name, row.Label, len(row.Segs))
			}
			// 计费方向标对了没有：三段里只有该标的那些带 .billed。
			wantBilled := map[int]bool{}
			for _, i := range w.billedSegs {
				wantBilled[i] = true
			}
			for i, seg := range row.Segs {
				gotBilled := strings.Contains(seg.Cls, "billed")
				if gotBilled != wantBilled[i] {
					t.Errorf("%s / %s：第 %d 段（%q）标色 = %v，期望 %v",
						node.Name, row.Label, i+1, seg.Text, gotBilled, wantBilled[i])
				}
				if gotBilled {
					if seg.Color != res.Tokens.Accent {
						t.Errorf("%s / %s：标色的那一段颜色 = %s，期望 --accent = %s",
							node.Name, row.Label, seg.Color, res.Tokens.Accent)
					}
					if seg.Weight != "600" {
						t.Errorf("%s / %s：标色的那一段字重 = %s，期望 600（不靠颜色也能看出标了哪一段）",
							node.Name, row.Label, seg.Weight)
					}
					// ★ 绝不许用 --ok / --warn / --bad：那三个已经被"用量档位"占用，
					// 再拿它们标方向会让同一个数字被读成"用量到 80% 了"。
					for _, banned := range []string{res.Tokens.OK, res.Tokens.Warn, res.Tokens.Bad} {
						if seg.Color == banned {
							t.Errorf("%s / %s：标色用了用量档位的语义色 %s", node.Name, row.Label, banned)
						}
					}
				} else {
					if seg.Weight == "600" {
						t.Errorf("%s / %s：没标的那一段字重也是 600", node.Name, row.Label)
					}
					if seg.Color == res.Tokens.Accent {
						t.Errorf("%s / %s：没标的那一段也用了强调色", node.Name, row.Label)
					}
				}
			}
			t.Logf("%s / %s：↓段=%q(标=%v) ↑段=%q(标=%v) 合计=%q 行宽=%.3f 方向宽=%.3f 补回=%.3fpx display=%s gap=%s",
				node.Name, row.Label,
				row.Segs[0].Text, strings.Contains(row.Segs[0].Cls, "billed"),
				row.Segs[1].Text, strings.Contains(row.Segs[1].Cls, "billed"),
				row.Sum, row.CellWidth, row.DirWidth, row.GapFromGeometry, row.DirDisplay, row.DirGap)
			// 拆分前那**一个**生效空格的宽度（实测 3.836px）必须由 gap 补回来：
			// 量的是真几何（方向读数那一段的宽 − 两个子 span 的宽），不是 CSS 文本。
			if math.Abs(row.GapFromGeometry-3.836) > 0.05 {
				t.Errorf("%s / %s：方向读数里补回来的间隔 = %.3fpx，期望 3.836px"+
					"（拆分前那两个空格里实际只有一个生效，少补了整行就会窄 3.8px）",
					node.Name, row.Label, row.GapFromGeometry)
			}
			if row.DirDisplay != "flex" {
				t.Errorf("%s / %s：方向读数的 display = %q，期望 flex（inline-flex 作为 flex item 会被块化）",
					node.Name, row.Label, row.DirDisplay)
			}
			if row.DirGap != "3.836px" {
				t.Errorf("%s / %s：.traffic-dir 的 gap = %q，期望 3.836px", node.Name, row.Label, row.DirGap)
			}
		}

		// 今日 / 本周两行：合计（收 + 发）+ **按计费口径**的占比。
		// 只算出站的机器上，这个占比不能按"收 + 发"算（那会写出一个比卡片更大的
		// 数，同一台机器在同一屏上两个说法）。
		if node.Rows[0].Label != "今日流量" || node.Rows[1].Label != "本周流量" {
			t.Fatalf("%s：前两行的标签 = %q / %q，期望「今日流量」「本周流量」",
				node.Name, node.Rows[0].Label, node.Rows[1].Label)
		}
		if node.Rows[0].Sum != w.todaySum {
			t.Errorf("%s：今日那一行 = %q，期望 %q（合计是收 + 发，占比按计费口径）",
				node.Name, node.Rows[0].Sum, w.todaySum)
		}
		if node.Rows[1].Sum != w.weekSum {
			t.Errorf("%s：本周那一行 = %q，期望 %q（合计是收 + 发，占比按计费口径）",
				node.Name, node.Rows[1].Sum, w.weekSum)
		}

		// 本周期那一行的占比必须跟着口径变，而且**两个数都要写**、额度也要看得见：
		// 「合计 X · 计费 Y（Z%）· 额度 L」（用户逐字确认的格式）。
		cycle := node.Rows[2]
		if cycle.Label != "本周期流量" {
			t.Fatalf("%s：第三行的标签 = %q，期望「本周期流量」", node.Name, cycle.Label)
		}
		if cycle.Sum != w.cycleSum {
			t.Errorf("%s：本周期那一行 = %q，期望 %q（合计是真实的收 + 发，"+
				"计费是按口径记进额度的用量，末尾是月额度）", node.Name, cycle.Sum, w.cycleSum)
		}
	}

	// ---- 3) 窄屏 760：那一行变长了，会不会折行 / 溢出 ----------------------
	//
	// 单独起一趟 Chrome，用 CDP 把**布局视口定死成 760**（理由见 billingNarrowProbe）：
	// 把三台机器三行的真几何读回来。为什么必须真量：`.traffic-cell` 是 inline-flex +
	// flex-wrap，放不下时"合计"整段会**掉到下一行**（不是从中间断开）——
	// 那在文本断言里一点痕迹都没有（textContent 一模一样）。
	//
	// 三台都量（不是只量焦点那台）：那一行的宽度是内容撑出来的，补上月额度之后
	// **最宽的一行换人了** —— 实测 出站 494.250px > 入站 486.703px > 双向 465.484px
	// （上一轮"双向最宽"的结论已经不成立），只量一台就会漏掉真正最挤的那行。
	outDir := os.Getenv("PROBE_SHOT_DIR")
	narrowPNG := ""
	if outDir != "" {
		narrowPNG = filepath.Join(outDir, "billing-窄屏-本周期行.png")
	}
	narrowCfg := harness.HarnessConfig{
		User: "admin", Pass: harness.AdminPass, Scenario: "billing-narrow",
		Nodes: ids, Focus: billingScenes[0].name,
	}
	narrowProxy := harness.NewHarnessProxy(t, "http://"+h.Addr, narrowCfg, harness.BillingHarnessJS)
	narrowMock := harness.NewMockServer(t, narrowProxy)
	narrow := billingNarrowProbe(t, chrome, narrowMock.URL+"/", narrowProxy.Result, narrowPNG)
	assertBillingNarrow(t, narrow, want)

	// ---- 4) 截图（人工核对用；没设 PROBE_SHOT_DIR 时不产出）----------------
	if outDir != "" {
		billingShots(t, chrome, "http://"+h.Addr, ids, outDir)
	}
}

// TestBillingWithoutLimitInRealBrowser 用**真浏览器**读一遍"没填月额度"的那台机器。
//
// 为什么必须真跑：`limit = 0` 那条分支的效果是"某一段文字**不出现**"，而静态断言
// 只能证明"早退写在追加额度之前" —— 它证明不了浏览器里那一行真的是
// `合计 X · 计费 Y`（拼接顺序一改，静态断言照样绿，页面上却会多出「额度 0 B」）。
// 用户对这一段的要求是逐字的：没填额度 ⇒ 不写这一段。
func TestBillingWithoutLimitInRealBrowser(t *testing.T) {
	chrome := harness.FindChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	s := billingNoLimitScene
	seeded := seedBillingNodes(t, ctx, dbPath, []billingScene{s})
	id := seeded[s.name].id

	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", dbPath, time.UTC, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))

	// 让这台机器真的连一次（跑的是仓库里那个假 /proc 的 Agent）：卡片上那四格在
	// "从未上报过"时整列都是 —，而这条用例要读的正是卡片上写的是什么。
	// Agent 第一拍只建基线、之后差值为 0，不会改动播撒进去的流量。
	agentCtx, stopAgent := context.WithCancel(ctx)
	defer stopAgent()
	client, _ := harness.NewClient(t, "http://"+h.Addr, seeded[s.name].token)
	go func() { _ = client.Run(agentCtx) }()
	harness.WaitFor(t, 30*time.Second, "那台机器上报过（否则卡片上是 —）", func() bool {
		status, body := br.Do("GET", "/api/v1/nodes", nil, false)
		if status != 200 {
			return false
		}
		nodes, _ := body["nodes"].([]any)
		for _, raw := range nodes {
			n, _ := raw.(map[string]any)
			if seen, _ := n["last_seen"].(float64); seen > 0 {
				return true
			}
		}
		return false
	})

	cfg := harness.HarnessConfig{
		User: "admin", Pass: harness.AdminPass, Scenario: "billing",
		Nodes: map[string]int64{s.name: id}, Focus: s.name,
	}
	proxy := harness.NewHarnessProxy(t, "http://"+h.Addr, cfg, harness.BillingHarnessJS)
	mock := harness.NewMockServer(t, proxy)
	raw := harness.RunChromeForResult(t, chrome, mock.URL+"/", proxy.Result, 240*time.Second, "1500,1100")

	var res billingResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, harness.Tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	if len(res.Detail) != 1 || len(res.Detail[0].Rows) != 3 {
		t.Fatalf("详情页读数 = %+v，期望 1 台 3 行", res.Detail)
	}
	rows := res.Detail[0].Rows
	if got := res.Detail[0].Name; got != s.name {
		t.Fatalf("浏览器读到的机器 = %q，期望 %q", got, s.name)
	}

	total := alert.FormatBytes(s.rx + s.tx) // 合计：真实的收 + 发
	used := alert.FormatBytes(s.wantUsed)   // 计费：按口径记进额度的用量
	wantCycle := "合计 " + total + " · 计费 " + used
	t.Logf("没填额度的机器：本周期那一行 = %q（期望 %q）；今日 %q；卡片百分比 %q 副值 %q",
		rows[2].Sum, wantCycle, rows[0].Sum, res.Cards[0].Pct, res.Cards[0].Sub)

	if rows[2].Sum != wantCycle {
		t.Errorf("本周期那一行 = %q，期望 %q（没填额度时「（Z%%）· 额度 L」整段都不写）",
			rows[2].Sum, wantCycle)
	}
	if strings.Contains(rows[2].Sum, "额度") {
		t.Errorf("没填额度的机器写出了「额度」那一段：%q（limit = 0 是「没填」，不是「额度为零」）",
			rows[2].Sum)
	}
	if strings.Contains(rows[2].Sum, "%") {
		t.Errorf("没填额度的机器写出了百分比：%q（没有分母的百分比没有意义）", rows[2].Sum)
	}
	// 今日 / 本周两行同理：只有合计，没有占比。
	if rows[0].Sum != total || rows[1].Sum != total {
		t.Errorf("今日 / 本周两行 = %q / %q，期望都是 %q（没额度 ⇒ 只写合计）",
			rows[0].Sum, rows[1].Sum, total)
	}
	// 卡片：没填额度时百分比那一格写 —、进度条留空（写 0% 会被读成"这个月没用过"）。
	if len(res.Cards) != 1 {
		t.Fatalf("卡片读数 = %d 张，期望 1 张", len(res.Cards))
	}
	if res.Cards[0].Label != s.wantLabel {
		t.Errorf("卡片标签 = %q，期望 %q", res.Cards[0].Label, s.wantLabel)
	}
	if res.Cards[0].Pct != "—" {
		t.Errorf("没填额度时卡片百分比 = %q，期望 —（不是 0.0%%）", res.Cards[0].Pct)
	}
	if res.Cards[0].Sub != used {
		t.Errorf("没填额度时卡片副值 = %q，期望 %q（只写已用，不写 / 0）", res.Cards[0].Sub, used)
	}

	// 出图是**可选**的（与主用例一致：没设 PROBE_SHOT_DIR 时不产出）。
	if outDir := os.Getenv("PROBE_SHOT_DIR"); outDir != "" {
		shotCfg := harness.HarnessConfig{
			User: "admin", Pass: harness.AdminPass, Scenario: "billing",
			Nodes: map[string]int64{s.name: id}, Shot: fmt.Sprintf("#/n/%d", id),
		}
		shotProxy := harness.NewHarnessProxy(t, "http://"+h.Addr, shotCfg, harness.BillingHarnessJS)
		shotMock := harness.NewMockServer(t, shotProxy)
		out := filepath.Join(outDir, "billing-无额度-本周期行.png")
		ready := `document.title.indexOf('SHOT-READY') === 0 && ` +
			`document.querySelectorAll('#info-traffic .traffic-cell').length === 3`
		cdpShot(t, chrome, shotMock.URL+"/", out, ready, "", 1500, 1180)
		if st, err := os.Stat(out); err != nil {
			t.Fatalf("%s: 截图没有生成: %v", out, err)
		} else {
			t.Logf("没填额度的机器 → %s（%d 字节）", out, st.Size())
		}
	}
}

// billingNarrowProbe 在**精确 760** 的布局视口下跑一遍窄屏自检，并按需出一张图。
//
// 为什么不用 RunChromeForResult 的 --window-size=760：headless 下那样得到的布局
// 视口是 **744**（窗口装饰吃掉 16px，本机实测）—— 而这一条要量的正是"760 宽"。
// CDP 的 Emulation.setDeviceMetricsOverride 按 CSS 像素定死视口，与窗口装饰、
// DPI、Chrome 版本都无关（仓库里 expo 那几张截图走的就是同一套 CDP）。
//
// 页内跑的是与宽屏**同一份** BillingHarnessJS（scenario=billing-narrow 那一趟），
// 所以窄屏与宽屏的读数出自同一个 readDetail()，可以直接对比；
// 窄屏那一趟会把三台机器的详情页都开一遍（见 harness 里那一段的说明）。
func billingNarrowProbe(t *testing.T, chrome, pageURL string, result chan []byte, outPNG string) billingResult {
	t.Helper()
	profile, err := os.MkdirTemp("", "probe-billing-narrow-")
	if err != nil {
		t.Fatalf("创建 Chrome 画像目录: %v", err)
	}
	defer func() { _ = os.RemoveAll(profile) }()

	args := []string{
		"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--disable-extensions", "--hide-scrollbars",
		"--remote-debugging-port=0", "--remote-allow-origins=*",
		"--user-data-dir=" + profile,
		"--window-size=1500,1000",
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
		harness.KillChrome(cmd)
		_ = cmd.Wait()
	}()

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
		t.Fatalf("等不到 DevToolsActivePort。Chrome 输出尾部：\n%s", harness.Tail(buf.String(), 800))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	conn := dialCDP(t, ctx, port)
	defer conn.CloseNow()

	// 定死视口：760 × 1000 CSS 像素（mobile=false ⇒ 不套用手机端的视口缩放）。
	if _, err := cdpCall(ctx, conn, 1, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": 760, "height": 1000, "deviceScaleFactor": 1, "mobile": false,
	}); err != nil {
		t.Fatalf("把布局视口设成 760: %v", err)
	}
	// 覆盖之后再重新导航一次：让页面**从第一帧**就跑在 760 的视口里
	// （否则首屏是按窗口宽度排的，后面才被重排）。
	if _, err := cdpCall(ctx, conn, 2, "Page.navigate", map[string]any{"url": pageURL}); err != nil {
		t.Fatalf("重新导航: %v", err)
	}

	var raw []byte
	select {
	case raw = <-result:
	case <-time.After(240 * time.Second):
		t.Fatalf("等窄屏那一趟回传结果超时。Chrome 输出尾部：\n%s", harness.Tail(buf.String(), 800))
	}

	var res billingResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析窄屏那一趟的结果: %v\n原始内容：%s", err, harness.Tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("窄屏那一趟的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("窄屏那一趟里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}

	if outPNG == "" {
		return res
	}
	// 出图：先把「流量信息」卡滚到视口中间（760 宽时它在首屏之外），
	// 再按卡片的外接矩形裁一张 —— 整页缩到 760 宽之后那一行看不清字。
	if _, err := cdpEval(ctx, conn, 3,
		`(function(){var d=document.getElementById('info-traffic');`+
			`if(d&&d.scrollIntoView)d.scrollIntoView({block:'center'});return true;})()`); err != nil {
		t.Fatalf("把流量卡滚进视口: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	clip, err := cdpEval(ctx, conn, 4, `(function () {
      var dl = document.getElementById('info-traffic');
      var card = dl && dl.closest ? dl.closest('.card') : null;
      if (!card) return null;
      var r = card.getBoundingClientRect();
      return { x: r.left + window.scrollX - 8, y: r.top + window.scrollY - 8,
               width: r.width + 16, height: r.height + 16, scale: 1 };
    })()`)
	if err != nil {
		t.Fatalf("算截图范围: %v", err)
	}
	if clip == nil {
		t.Fatal("页面上找不到「流量信息」卡：宁可不截，也不出一张空图")
	}
	shotRaw, err := cdpCall(ctx, conn, 5, "Page.captureScreenshot",
		map[string]any{"format": "png", "clip": clip})
	if err != nil {
		t.Fatalf("Page.captureScreenshot: %v", err)
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(shotRaw, &shot); err != nil {
		t.Fatalf("解析截图结果: %v", err)
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatalf("解码截图: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPNG), 0o755); err != nil {
		t.Fatalf("创建截图目录: %v", err)
	}
	if err := os.WriteFile(outPNG, png, 0o644); err != nil {
		t.Fatalf("写截图: %v", err)
	}
	t.Logf("窄屏 760 的流量信息卡 → %s（%d 字节）", outPNG, len(png))
	return res
}

// assertBillingNarrow 是"窄屏 760 不折行、不溢出"那一条的判据。
//
// 四个判据都取**真几何**，不看文本：
//   - sumRects == 1：合计那一段只占一行（inline 元素跨行时 getClientRects 会给多个）；
//   - sumTopDelta ≤ 2px：合计与方向读数还在同一条基线上（折行后它会掉下去一整行）；
//   - ddScrollWidth ≤ ddClientWidth：值那一格没有横向溢出；
//   - cellWidth ≤ ddWidth：那一行的内容宽度没超过可用宽度。
//
// ★ 三台机器**每一行的每一行**都过一遍：`本周期` 那一行的宽度是内容撑出来的，
// 而三台的数值与额度都不一样 —— 补上月额度之后最宽的那一行不再是焦点机器
// （实测 出站 494.250px > 双向 465.484px），只量一台就会漏掉真正最挤的那一行。
//
// 顺带把宽度账打出来（行宽 / 可用宽度 / 占了多少），交付说明里的数字就是它。
func assertBillingNarrow(t *testing.T, res billingResult, want map[string]billingExpected) {
	t.Helper()
	if res.Viewport != 760 {
		t.Fatalf("窄屏那一趟的布局视口 = %d，期望 760（CDP Emulation.setDeviceMetricsOverride 定死的值）",
			res.Viewport)
	}
	if len(res.Detail) != len(billingScenes) {
		t.Fatalf("窄屏那一趟读了 %d 台机器，期望 %d 台（三台的数值与额度不同，一行都少不得）",
			len(res.Detail), len(billingScenes))
	}
	seen := map[string]bool{}
	for _, got := range res.Detail {
		w, ok := want[got.Name]
		if !ok {
			t.Fatalf("窄屏那一趟读到一台没登记过的机器 %q", got.Name)
		}
		seen[got.Name] = true
		if len(got.Rows) != 3 {
			t.Fatalf("%s：窄屏那一趟读到 %d 行，期望 3 行（今日 / 本周 / 本周期）",
				got.Name, len(got.Rows))
		}

		// 文案先钉死：折行与裁剪都改不了 textContent，但它证明"该显示的一个都不少"。
		if got.Rows[0].Sum != w.todaySum || got.Rows[1].Sum != w.weekSum {
			t.Errorf("%s：窄屏下今日 / 本周那两行 = %q / %q，期望 %q / %q",
				got.Name, got.Rows[0].Sum, got.Rows[1].Sum, w.todaySum, w.weekSum)
		}
		cycle := got.Rows[2]
		if cycle.Sum != w.cycleSum {
			t.Errorf("%s：窄屏下本周期那一行 = %q，期望 %q", got.Name, cycle.Sum, w.cycleSum)
		}

		for _, row := range got.Rows {
			if row.SumRects != 1 {
				t.Errorf("%s / %s：窄屏 760 下合计那一段占了 %d 行（折行了）",
					got.Name, row.Label, row.SumRects)
			}
			if row.SumTopDelta > 2 {
				t.Errorf("%s / %s：窄屏 760 下合计与方向读数错位 %.3fpx（>2px 说明它掉到了下一行）",
					got.Name, row.Label, row.SumTopDelta)
			}
			if row.DDScrollWidth > row.DDClientWidth {
				t.Errorf("%s / %s：窄屏 760 下值那一格横向溢出（scrollWidth %.3f > clientWidth %.3f）",
					got.Name, row.Label, row.DDScrollWidth, row.DDClientWidth)
			}
			if row.CellWidth > row.DDWidth+0.5 {
				t.Errorf("%s / %s：窄屏 760 下行宽 %.3fpx 超过了可用宽度 %.3fpx",
					got.Name, row.Label, row.CellWidth, row.DDWidth)
			}
		}
		// 本周期那一行不能比今日那一行高（高出来就是折行）。
		if cycle.CellHeight > got.Rows[0].CellHeight+0.5 {
			t.Errorf("%s：窄屏 760 下本周期那一行高 %.3fpx，今日那一行 %.3fpx（高出来说明折了行）",
				got.Name, cycle.CellHeight, got.Rows[0].CellHeight)
		}
		t.Logf("窄屏 760 / %s：本周期那一行 %.3fpx = 方向读数 %.3fpx + gap 16px + 合计 %.3fpx；"+
			"可用宽度 %.3fpx ⇒ 占 %.1f%%，剩余 %.3fpx；%q",
			got.Name, cycle.CellWidth, cycle.DirWidth, cycle.SumWidth, cycle.DDWidth,
			cycle.CellWidth/cycle.DDWidth*100, cycle.DDWidth-cycle.CellWidth, cycle.Sum)
	}
	for _, s := range billingScenes {
		if !seen[s.name] {
			t.Errorf("窄屏那一趟没读到 %q 的详情页", s.name)
		}
	}
	if res.DocOverflowX != 0 {
		t.Errorf("窄屏 760 下整个文档横向溢出了 %.1fpx", res.DocOverflowX)
	}
}

// seededNode 是播撒进库的一台机器：浏览器与 Agent 都按 id 找它。
type seededNode struct {
	id    int64
	token string
}

// seedBillingNodes 把给定的几台机器与它们的本周期流量先写进库，再让服务端启动。
//
// 为什么在启动之前就写好：流量汇总是有缓存的（trafficCache），从外面写库之后
// 缓存里可能还留着旧的汇总（e2e 包够不到服务端那个私有缓存）；先把数据备齐，
// 服务端第一次读到它就是对的。
func seedBillingNodes(t *testing.T, ctx context.Context, dbPath string, scenes []billingScene) map[string]seededNode {
	t.Helper()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer func() { _ = db.Close() }()

	day := store.FormatDay(time.Now().UTC())
	out := make(map[string]seededNode, len(scenes))
	for _, s := range scenes {
		node, token, err := db.CreateNode(ctx, store.NewNode{
			Name: s.name, GroupName: "计费", Region: "HK", IntervalSec: 1,
			TrafficWarnPct: 80, ResetDay: 1,
			TrafficLimit: s.limit, TrafficBilling: s.billing,
		}, time.Now())
		if err != nil {
			t.Fatalf("创建节点 %s: %v", s.name, err)
		}
		out[s.name] = seededNode{id: node.ID, token: token}
		if err := db.FlushTraffic(ctx, []store.TrafficUpdate{{
			NodeID: node.ID, Day: day,
			RxDelta: s.rx, TxDelta: s.tx,
			RxTotal: uint64(s.rx), TxTotal: uint64(s.tx),
		}}, time.Now()); err != nil {
			t.Fatalf("写入 %s 的流量: %v", s.name, err)
		}
		t.Logf("播撒 %s：口径=%d 额度=%s 本周期收=%s 发=%s",
			s.name, s.billing, alert.FormatBytes(s.limit),
			alert.FormatBytes(s.rx), alert.FormatBytes(s.tx))
	}
	return out
}

// billingShots 出五张人工核对的 PNG：三种口径各一张详情页（看标色落在哪几段、
// 本周期那一行的「合计 X · 计费 Y（Z%）」），一张首页（看卡片标签三态），
// 外加一张**窄屏 760**的流量信息卡（看那一行有没有折行 / 顶到边）。
//
// 为什么详情页那三张是**主交付**：标签换词在首页上只有三个字，而"标色标在哪一段"
// 与"合计 / 计费两个数"只有在详情页那三行里才看得见；首页那张是给标签与进度条
// 档位做对照的；窄屏那张是这一轮新增文案的宽度证据。
func billingShots(t *testing.T, chrome, base string, ids map[string]int64, outDir string) {
	t.Helper()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("创建截图目录: %v", err)
	}
	names := map[int]string{
		store.BillingBoth:   "双向",
		store.BillingTxOnly: "出站",
		store.BillingRxOnly: "入站",
	}
	for _, s := range billingScenes {
		cfg := harness.HarnessConfig{
			User: "admin", Pass: harness.AdminPass, Scenario: "billing",
			Nodes: ids, Shot: fmt.Sprintf("#/n/%d", ids[s.name]),
		}
		proxy := harness.NewHarnessProxy(t, base, cfg, harness.BillingHarnessJS)
		mock := harness.NewMockServer(t, proxy)
		out := filepath.Join(outDir, "billing-口径-"+names[s.billing]+".png")
		// 等详情页真的画完：那三行一共 6 段，计费方向那几段带上 .billed 才算数
		// （双向 ⇒ 6 段全标；只算出站 ⇒ 三行各一段 = 3 段）。
		billedWant := 3 * len(billingWant()[s.name].billedSegs)
		ready := `document.title.indexOf('SHOT-READY') === 0 && ` +
			`document.querySelectorAll('#info-traffic .traffic-dir-seg').length === 6 && ` +
			fmt.Sprintf(`document.querySelectorAll('#info-traffic .traffic-dir-seg.billed').length === %d`, billedWant)
		cdpShot(t, chrome, mock.URL+"/", out, ready, "", 1500, 1180)
		st, err := os.Stat(out)
		if err != nil {
			t.Fatalf("%s: 截图没有生成: %v", out, err)
		}
		t.Logf("%s → %s（%d 字节）", s.name, out, st.Size())
	}

	// 首页那张：三张卡片的标签同时可见（双向流量 / 出站流量 / 入站流量）。
	cfg := harness.HarnessConfig{
		User: "admin", Pass: harness.AdminPass, Scenario: "billing",
		Nodes: ids, Shot: "#/",
	}
	proxy := harness.NewHarnessProxy(t, base, cfg, harness.BillingHarnessJS)
	mock := harness.NewMockServer(t, proxy)
	out := filepath.Join(outDir, "billing-卡片标签-三态.png")
	ready := `document.title.indexOf('SHOT-READY') === 0 && ` +
		`document.querySelectorAll('#grid > .card').length === 3 && ` +
		`document.querySelectorAll('#grid .res-cell .res-label').length === 12`
	cdpShot(t, chrome, mock.URL+"/", out, ready, "", 1500, 1180)
	if st, err := os.Stat(out); err != nil {
		t.Fatalf("%s: 截图没有生成: %v", out, err)
	} else {
		t.Logf("首页三张卡片 → %s（%d 字节）", out, st.Size())
	}
}

// ---------------------------------------------------------------- 观测值

type billingResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`
	// Viewport / DocOverflowX 是"这一趟的布局视口有多宽、文档横向溢出了多少"：
	// 窄屏那一趟靠它们证明量的是 760，而不是"窗口设了 760"。
	Viewport     int     `json:"viewport"`
	DocOverflowX float64 `json:"docOverflowX"`
	Cards        []struct {
		Name     string `json:"name"`
		Label    string `json:"label"`
		Pct      string `json:"pct"`
		Sub      string `json:"sub"`
		BarClass string `json:"barClass"`
		BarColor string `json:"barColor"`
		BarWidth string `json:"barWidth"`
	} `json:"cards"`
	Detail []struct {
		Name string `json:"name"`
		Rows []struct {
			Label string `json:"label"`
			Segs  []struct {
				Text   string `json:"text"`
				Cls    string `json:"cls"`
				Color  string `json:"color"`
				Weight string `json:"weight"`
			} `json:"segs"`
			Sum             string  `json:"sum"`
			CellWidth       float64 `json:"cellWidth"`
			CellHeight      float64 `json:"cellHeight"`
			DirWidth        float64 `json:"dirWidth"`
			GapFromGeometry float64 `json:"gapFromGeometry"`
			DirDisplay      string  `json:"dirDisplay"`
			DirGap          string  `json:"dirGap"`
			// ↓ 这五个只在窄屏那一条断言里用（见 assertBillingNarrow）。
			SumWidth      float64 `json:"sumWidth"`
			SumRects      int     `json:"sumRects"`
			SumTopDelta   float64 `json:"sumTopDelta"`
			DDWidth       float64 `json:"ddWidth"`
			DDClientWidth float64 `json:"ddClientWidth"`
			DDScrollWidth float64 `json:"ddScrollWidth"`
		} `json:"rows"`
	} `json:"detail"`
	// Tokens 是页面把 CSS 变量解析成 rgb 之后的真值：断言因此与"是哪套主题"无关，
	// 但仍然钉得住"标色用的是 --accent，不是 --ok/--warn/--bad"。
	Tokens struct {
		Accent string `json:"accent"`
		OK     string `json:"ok"`
		Warn   string `json:"warn"`
		Bad    string `json:"bad"`
	} `json:"tokens"`
}
