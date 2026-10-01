package e2e

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/store"
)

// 这次改动在**取数级**上的对照表：**同一份数据**下，六档 × 五张资源图（+ 流量图）
// 的 Y 轴在改前（对轴顶取整，v1.0.34）与改后（对步长取整）各是什么样。
//
// 为什么要有这张表：单元测试证明的是"规则本身满足三条硬指标"，浏览器用例证明的是
// "延迟图上画出来的确实是 0/200/400/600/800"。两者都答不了"换成别的档位、别的图，
// 会不会有哪一档反而更空"——那要拿**真实接口返回的真实数据**逐个档位算一遍。
// 所以这里走真服务端：播一份确定性的数据（cpu/mem/disk/rx/tx 都是 ts 的函数，
// 同时写进 10 秒层与 1 分钟层），然后对六档 × 五个指标取数，按 chart.js 的
// bounds() 口径（showMax 开着时峰值也撑轴）算出 vMax，再分别按新旧两套规则算轴顶。
//
// 读数注意：五张资源图里只有**网络速率**走自动取整 —— CPU/内存/磁盘是百分比图
// （app.js 的 pctOpts 传了 yMax: 100，那是硬上限），它们的轴顶在改前改后都是 100。
// 这条同样是"这次改动的影响面"的一部分，所以也在表里逐档列出来。
func TestYAxisStepTableOldVsNew(t *testing.T) {
	rule := readYAxisRuleE2E(t)

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "yaxis-table")
	seedYAxisTableSamples(t, h, nodeID)
	seedYAxisTraffic(t, h, nodeID)

	t.Log("同一份数据：cpu 13…57% 正弦 + 每 6 小时一次 88% 尖峰（峰值 92%）、" +
		"mem 40…62%（峰值 64%）、disk 55…71%（峰值 72%）、" +
		"rx 0.3…2.4 MB/s、tx 0.1…0.9 MB/s、日流量最高 465 GB")

	tighter, same, wider := 0, 0, 0
	for _, key := range []string{"1h", "6h", "12h", "1d", "3d", "7d"} {
		// 五个指标的 vMax（chart.js 的 bounds() 口径：showMax 开着时 p[1] 与 p[2] 都撑轴）
		vMaxOf := map[string]float64{}
		for _, metric := range []string{"cpu", "mem", "disk", "net_down", "net_up"} {
			avg, max := yaxisSeriesMax(t, br, nodeID, key, metric)
			if max > avg {
				vMaxOf[metric] = max
			} else {
				vMaxOf[metric] = avg
			}
		}
		// 网络图是上下行**两条线共用一个 Y 轴**，撑轴的是两者的最大值。
		netMax := math.Max(vMaxOf["net_down"], vMaxOf["net_up"])

		rows := []struct {
			chart    string
			vMax     float64
			fixedMax float64 // 0 = 自动取整
		}{
			{"CPU %", vMaxOf["cpu"], 100},
			{"内存 %", vMaxOf["mem"], 100},
			{"磁盘 %", vMaxOf["disk"], 100},
			{"网络速率", netMax, 0},
		}
		for _, row := range rows {
			oldTop := yaxisOldTop(row.vMax, row.fixedMax)
			newTop := rule.top(row.vMax)
			if row.fixedMax > 0 {
				newTop = row.fixedMax
			}
			t.Logf("%-4s %-9s vMax=%-10s 旧 top=%-10s [%s]  新 top=%-10s [%s]",
				key, row.chart, yaxisText(row.vMax), yaxisText(oldTop),
				yaxisTicksText(oldTop, rule.lines, row.fixedMax), yaxisText(newTop),
				yaxisTicksText(newTop, rule.lines, row.fixedMax))

			// 1) 数据永远不许被画出界。
			if newTop < row.vMax {
				t.Errorf("%s 档 %s：新轴顶 %v < 峰值 %v", key, row.chart, newTop, row.vMax)
			}
			// 2) 整齐数：轴顶 ÷ 网格线条数 必须落在档位表上。
			if !rule.isLadderStep(newTop / rule.lines) {
				t.Errorf("%s 档 %s：新轴顶 %v 的步长 %v 不在档位表上（刻度会读不出来）",
					key, row.chart, newTop, newTop/rule.lines)
			}
			// 3) 自动取整那几张图：留白不许超过峰值的 1.6 倍，也不许比旧规则更空
			//    （硬上限图不适用：vMax 可以比 100 大，此时"留白"没有意义）。
			if row.fixedMax == 0 {
				if newTop > row.vMax*1.6 {
					t.Errorf("%s 档 %s：新轴顶 %v 超过峰值 %v 的 1.6 倍", key, row.chart, newTop, row.vMax)
				}
				switch {
				case newTop < oldTop:
					tighter++
				case newTop == oldTop:
					same++
				default:
					wider++
					if newTop > oldTop*1.25 {
						t.Errorf("%s 档 %s：新轴顶 %v 比旧轴顶 %v 高 %.2f 倍（超过 1.25 倍上限）",
							key, row.chart, newTop, oldTop, newTop/oldTop)
					}
				}
			}
		}
	}

	// 流量图不跟六档走（它恒为近 7 天），所以单独一行。
	down, up := yaxisTrafficMax(t, br, nodeID)
	trafficMax := math.Max(down, up)
	oldTop := yaxisOldTop(trafficMax, 0)
	newTop := rule.top(trafficMax)
	t.Logf("%-4s %-9s vMax=%-10s 旧 top=%-10s [%s]  新 top=%-10s [%s]",
		"7d", "日流量", yaxisText(trafficMax), yaxisText(oldTop), yaxisTicksText(oldTop, rule.lines, 0),
		yaxisText(newTop), yaxisTicksText(newTop, rule.lines, 0))
	if newTop < trafficMax || newTop > trafficMax*1.6 {
		t.Errorf("流量图新轴顶 %v 不在 [峰值 %v, 峰值×1.6] 区间里", newTop, trafficMax)
	}
	if newTop > oldTop {
		wider++
		if newTop > oldTop*1.25 {
			t.Errorf("流量图新轴顶 %v 比旧轴顶 %v 高 %.2f 倍（超过 1.25 倍上限）", newTop, oldTop, newTop/oldTop)
		}
	} else if newTop < oldTop {
		tighter++
	} else {
		same++
	}

	t.Logf("自动取整那几张图汇总：新规则更紧 %d 档、持平 %d 档、更松 %d 档（更松的都 ≤ 旧轴顶的 1.25 倍）",
		tighter, same, wider)
}

// yaxisSeriesMax 取一个指标在某一档里的最高均值与最高峰值（chart.js 的 bounds() 口径）。
func yaxisSeriesMax(t *testing.T, br *browser, nodeID int64, key, metric string) (avg, max float64) {
	t.Helper()
	status, body := br.do(http.MethodGet,
		"/api/v1/nodes/"+strconv.FormatInt(nodeID, 10)+"/series?range="+key+"&metric="+metric, nil, false)
	if status != http.StatusOK {
		t.Fatalf("%s 档 %s 取数失败: %d %v", key, metric, status, body)
	}
	points, _ := body["points"].([]any)
	if len(points) == 0 {
		t.Fatalf("%s 档 %s 一个点都没有：数据没铺够", key, metric)
	}
	for _, raw := range points {
		p, _ := raw.([]any)
		if len(p) < 3 {
			t.Fatalf("%s 档 %s 的点不是三元组: %v", key, metric, raw)
		}
		a, _ := p[1].(float64)
		m, _ := p[2].(float64)
		if a > avg {
			avg = a
		}
		if m > max {
			max = m
		}
	}
	return avg, max
}

// yaxisTrafficMax 取日流量图两个方向的最大值（下行/上行）。
func yaxisTrafficMax(t *testing.T, br *browser, nodeID int64) (down, up float64) {
	t.Helper()
	status, body := br.do(http.MethodGet,
		"/api/v1/nodes/"+strconv.FormatInt(nodeID, 10)+"/traffic?days=7", nil, false)
	if status != http.StatusOK {
		t.Fatalf("流量取数失败: %d %v", status, body)
	}
	points, _ := body["points"].([]any)
	if len(points) == 0 {
		t.Fatalf("流量图一个点都没有：数据没铺够")
	}
	for _, raw := range points {
		p, _ := raw.([]any)
		if len(p) < 3 {
			t.Fatalf("流量图的点不是三元组: %v", raw)
		}
		d, _ := p[1].(float64)
		u, _ := p[2].(float64)
		if d > down {
			down = d
		}
		if u > up {
			up = u
		}
	}
	return down, up
}

// yaxisOldTop 是 v1.0.34 的轴顶（对轴顶取整的 niceCeil，参数是 vMax×1.1）。
func yaxisOldTop(vMax, fixedMax float64) float64 {
	if fixedMax > 0 {
		return fixedMax
	}
	if vMax <= 0 {
		return 1
	}
	value := vMax * 1.1
	base := math.Pow(10, math.Floor(math.Log10(value)))
	norm := value / base
	step := 10.0
	switch {
	case norm <= 1:
		step = 1
	case norm <= 2:
		step = 2
	case norm <= 2.5:
		step = 2.5
	case norm <= 5:
		step = 5
	}
	return step * base
}

// isLadderStep 判断一个步长是不是"档位 × 10 的整数次幂"。
func (r yaxisRuleE2E) isLadderStep(step float64) bool {
	if !(step > 0) {
		return false
	}
	for k := -6; k <= 9; k++ {
		base := math.Pow(10, float64(k))
		for _, rung := range r.ladder {
			if math.Abs(rung*base-step) <= 1e-9*step {
				return true
			}
		}
	}
	return false
}

// yaxisTicksText 把刻度拼成日志里的一行（固定上限图按固定上限切分，与 draw() 一致）。
func yaxisTicksText(top, lines, fixedMax float64) string {
	if top <= 0 {
		return "—"
	}
	parts := make([]string, 0, int(lines)+1)
	for i := 0; float64(i) <= lines; i++ {
		parts = append(parts, yaxisText(top/lines*float64(i)))
	}
	return strings.Join(parts, "/")
}

// yaxisText 是日志里的数值写法：字节量级用 K/M 缩写，其余原样。
func yaxisText(v float64) string {
	switch {
	case math.Abs(v) >= 1e9:
		return trimFloat(math.Round(v/1e7)/100) + "G"
	case math.Abs(v) >= 1e6:
		return trimFloat(math.Round(v/1e4)/100) + "M"
	case math.Abs(v) >= 1e3:
		return trimFloat(math.Round(v/1e2)/10) + "K"
	default:
		return trimFloat(math.Round(v*1000) / 1000)
	}
}

// ---------------------------------------------------------------- 播种

// yaxisSampleAt 是"同一份数据"的唯一定义：cpu/mem/disk/rx/tx 全是 ts 的确定性函数。
//
// 每 6 小时有一次 5 分钟的尖峰（cpu 88%、rx 2.4MB/s）：长档位的 vMax 由它决定，
// 短档位则多半只看得到正弦那一段 —— 这正是"同一个峰值在不同档位下被顶到哪一档"
// 要观察的东西。
func yaxisSampleAt(nodeID, ts int64) store.SampleBucket {
	phase := float64(ts%10800) / 10800 * 2 * math.Pi
	sin := (math.Sin(phase) + 1) / 2 // 0…1

	cpuAvg := 13 + 44*sin     // 13…57%
	memAvg := 40 + 22*sin     // 40…62%
	diskAvg := 55 + 16*sin    // 55…71%
	rxRate := 3e5 + 1.5e6*sin // 0.3…1.8 MB/s
	txRate := 1e5 + 6e5*sin   // 0.1…0.7 MB/s
	if ts%21600 < 300 {       // 每 6 小时一次、持续 5 分钟的尖峰
		cpuAvg = 88 + float64(ts%4)
		memAvg = 63 + float64(ts%2)
		diskAvg = 71 + float64(ts%2)
		rxRate = 2.2e6 + float64(ts%3)*1e5 // 最高 2.4 MB/s
		txRate = 8e5 + float64(ts%3)*5e4   // 最高 0.9 MB/s
	}
	return store.SampleBucket{
		NodeID: nodeID, TS: ts,
		CPUAvg: cpuAvg, CPUMax: cpuAvg + 4,
		MemAvg: memAvg, MemMax: memAvg + 2,
		DiskAvg: diskAvg, DiskMax: diskAvg + 1,
		RxRate: rxRate, RxMax: rxRate * 1.05,
		TxRate: txRate, TxMax: txRate * 1.05,
		Up: 10, All: 10,
	}
}

// seedYAxisTableSamples 把同一份数据写进**两层**：10 秒层管 1h/6h，1 分钟层管 12h 及以上
// （见 store.Ranges 的 pickSource —— 12h 起改用 samples_1m）。两层用同一个函数，
// 所以"同一份数据下的六个档位"这句话是真的，而不是六份各自播的数据。
func seedYAxisTableSamples(t *testing.T, h *harness, nodeID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	// 10 秒层：12 小时（这一层的保留期就是 12 小时）。
	fine := make([]store.SampleBucket, 0, 4320)
	end10 := now.Unix() - now.Unix()%10
	for ts := end10 - 12*3600; ts <= end10; ts += 10 {
		fine = append(fine, yaxisSampleAt(nodeID, ts))
	}
	if err := h.db.InsertBuckets(ctx, store.TableSamples10s, fine); err != nil {
		t.Fatalf("写入 10 秒层: %v", err)
	}

	// 1 分钟层：7 天。
	coarse := make([]store.SampleBucket, 0, 10080)
	end60 := now.Unix() - now.Unix()%60
	for ts := end60 - 7*24*3600; ts <= end60; ts += 60 {
		coarse = append(coarse, yaxisSampleAt(nodeID, ts))
	}
	if err := h.db.InsertBuckets(ctx, store.TableSamples1m, coarse); err != nil {
		t.Fatalf("写入 1 分钟层: %v", err)
	}
	t.Logf("播撒样本：10 秒层 %d 行（近 12 小时）、1 分钟层 %d 行（近 7 天）", len(fine), len(coarse))
}

// seedYAxisTraffic 写 7 天日流量：最高一天 465 GB（下行），上行约一半。
func seedYAxisTraffic(t *testing.T, h *harness, nodeID int64) {
	t.Helper()
	ctx := context.Background()
	today := time.Now().In(time.UTC)
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	for i := 6; i >= 0; i-- {
		d := day.AddDate(0, 0, -i)
		down := int64(300+i*25) * 1_000_000_000 // 300…450 GB
		if i == 3 {
			down = 465_000_000_000 // 最高的一天
		}
		if _, err := h.db.Writer().ExecContext(ctx,
			`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)`,
			nodeID, store.FormatDay(d), down, down/2); err != nil {
			t.Fatalf("写入日流量 %s: %v", store.FormatDay(d), err)
		}
	}
}
