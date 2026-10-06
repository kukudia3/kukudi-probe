package chart

import (
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"probe/internal/e2e/harness"
	"probe/internal/store"
)

// 本文件把"纯算术"那条回归（axis_window_test.go）再钉一遍：输入不是重放公式算出来的，
// 而是**真夹具 + 真 store 查询**（harness.SeedPingBucketsOffset + store.QueryPingSeries），
// 只把"现在"作为参数喂进去 —— 于是"服务端时区午夜后几分钟"这种时刻可以在任意时间造出来，
// 不必等到真半夜。
//
// 为什么值得单独写：轴上那条自检的输入（T0/T1）是**产品代码算出来的**，而纯算术回归
// 复刻的只是这条链路的算式。算式抄错一个字（桶宽、开闭区间、播种 end 的取整），
// 回归会"自洽地"通过。走真查询就不存在这个风险：折桶、窗口对齐、开闭区间全部由
// 产品代码自己决定，这里只负责换掉"现在"。

// axisProbeBox 是一个真服务端 + 按指定"现在"铺好的一份探测数据。
type axisProbeBox struct {
	t        *testing.T
	h        *harness.Harness
	nodeID   int64
	targetID int64
}

// newAxisProbeBoxAt 起一个真服务端，并按 step 秒网格在 now 之前铺 7 天数据。
//
// 每个"现在"一份独立的库：探针桶的 ts 是绝对时刻，在同一个库里叠着铺会让上一轮的
// 相位混进这一轮（两端的点就不是这一次窗口的了）。
func newAxisProbeBoxAt(t *testing.T, loc *time.Location, now time.Time, step int) *axisProbeBox {
	t.Helper()
	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), loc, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))
	nodeID, _ := harness.CreateNodeViaAPI(t, br, "axis-window")
	targetID := harness.CreatePingTarget(t, br, "CF", "1.1.1.1", 443, 60)
	harness.SeedPingBucketsOffset(t, h, nodeID, targetID, step, 7*24*time.Hour, now)
	return &axisProbeBox{t: t, h: h, nodeID: nodeID, targetID: targetID}
}

func (b *axisProbeBox) stop() { b.h.Stop(b.t) }

// endpoints 走**真查询**取这一档的首末点 ts。
func (b *axisProbeBox) endpoints(key string, now time.Time) (t0, t1 int64, found bool) {
	b.t.Helper()
	r, ok := store.PingRangeByKey(key)
	if !ok {
		b.t.Fatalf("没有 %q 这一档", key)
	}
	series, err := b.h.DB.QueryPingSeries(context.Background(), b.nodeID, b.targetID, r, now)
	if err != nil {
		b.t.Fatalf("查询 %s 档: %v", key, err)
	}
	if len(series.Points) == 0 {
		return 0, 0, false
	}
	return series.Points[0].TS, series.Points[len(series.Points)-1].TS, true
}

// axisBoundaryOffsets 是"当地日零点起的秒数"里最该盯的那些：午夜后 0~12 分钟逐 30 秒
// （假红原本就出在这一段），加午夜前 7 分钟逐 30 秒（另一侧），加几个远处的对照点。
//
// 这里是**抽样**：逐秒穷举在不需要服务端的那条纯算术回归里做完。
func axisBoundaryOffsets() []int64 {
	offs := make([]int64, 0, 48)
	for sec := int64(0); sec <= 720; sec += 30 {
		offs = append(offs, sec)
	}
	for sec := int64(86400 - 420); sec < 86400; sec += 30 {
		offs = append(offs, sec)
	}
	return append(offs, 1800, 21600, 43200, 64800, 84600)
}

// midnightFalseRedOffsets 是"旧网格必然判错"的那几个采样时刻（用于反向验证）。
var midnightFalseRedOffsets = []int64{0, 60, 120, 180, 240, 300, 360, 480, 540, 600}

// TestWindowSpansLocalDayThroughRealQuery 在**真查询路径**上核对判据：
// 每个采样时刻起一个服务端、按 seedStep 铺数据，六档分别取首末点，
// 与"产品自己算的窗口跨没跨当地自然日"对照。
func TestWindowSpansLocalDayThroughRealQuery(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("加载时区: %v", err)
	}
	base := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)

	type result struct {
		checked int
		bad     int
		first   string
	}
	per := map[string]*result{}
	for _, spec := range axisRangeSpecs {
		per[spec.key] = &result{}
	}

	for _, off := range axisBoundaryOffsets() {
		now := time.Unix(base.Unix()+off, 0).In(loc)
		box := newAxisProbeBoxAt(t, loc, now, seedStep)
		for _, spec := range axisRangeSpecs {
			r, ok := store.PingRangeByKey(spec.key)
			if !ok {
				t.Fatalf("没有 %q 这一档", spec.key)
			}
			t0, t1, found := box.endpoints(spec.key, now)
			if !found {
				t.Fatalf("%s 档在 %s 一个点都没有（刚按 %d 秒网格铺过）",
					spec.key, now.Format("01-02 15:04:05"), seedStep)
			}
			qEnd := now.Unix() - now.Unix()%r.Bucket
			qStart := qEnd - int64(r.Window.Seconds())
			want := dayStartOf(qStart, loc) != dayStartOf(qEnd-1, loc)
			got := windowSpansLocalDay(t0, t1, r.Bucket, loc)
			res := per[spec.key]
			res.checked++
			if got != want {
				res.bad++
				if res.first == "" {
					res.first = "now=" + now.Format("01-02 15:04:05") +
						" t0=" + time.Unix(t0, 0).In(loc).Format("01-02 15:04:05") +
						" t1=" + time.Unix(t1, 0).In(loc).Format("01-02 15:04:05") +
						" 窗口=[" + time.Unix(qStart, 0).In(loc).Format("01-02 15:04:05") + "," +
						time.Unix(qEnd, 0).In(loc).Format("01-02 15:04:05") + ")" +
						" 真=" + strconv.FormatBool(want) + " 判据=" + strconv.FormatBool(got)
				}
			}
		}
		box.stop()
	}

	for _, spec := range axisRangeSpecs {
		res := per[spec.key]
		if res.bad != 0 {
			t.Errorf("%s 档：真查询 %d 个当地时刻里有 %d 个与真值不符\n  第一处：%s",
				spec.key, res.checked, res.bad, res.first)
			continue
		}
		t.Logf("%s 档：真查询 %d 个当地时刻全部一致（含午夜前后边界逐 30 秒）", spec.key, res.checked)
	}
}

// TestRealQueryReproducesMidnightFalseRed 是**反向验证**：同一条真查询链路，
// 只把夹具的行网格换成改之前的 300 秒，服务端时区午夜后必须判出假红 ——
// 这正是用户报的"每天必然红约 10 分钟"；换成 60 秒后同一时刻必须与真值一致。
func TestRealQueryReproducesMidnightFalseRed(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("加载时区: %v", err)
	}
	r, ok := store.PingRangeByKey("1d")
	if !ok {
		t.Fatal("没有 1d 档")
	}
	base := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)

	f1 := func(ts int64) string { return time.Unix(ts, 0).In(loc).Format("01-02 15:04:05") }
	falseReds := 0
	for _, off := range midnightFalseRedOffsets {
		now := time.Unix(base.Unix()+off, 0).In(loc)
		qEnd := now.Unix() - now.Unix()%r.Bucket
		qStart := qEnd - int64(r.Window.Seconds())
		want := dayStartOf(qStart, loc) != dayStartOf(qEnd-1, loc)

		// 改之前：行网格 300 秒（整除不了 1d 的 120 秒桶）。
		old := newAxisProbeBoxAt(t, loc, now, 300)
		t0, t1, _ := old.endpoints("1d", now)
		gotOld := windowSpansLocalDay(t0, t1, r.Bucket, loc)
		old.stop()

		// 改之后：行网格 seedStep。
		fresh := newAxisProbeBoxAt(t, loc, now, seedStep)
		t0b, t1b, _ := fresh.endpoints("1d", now)
		gotNew := windowSpansLocalDay(t0b, t1b, r.Bucket, loc)
		fresh.stop()

		t.Logf("当地 %s 真值=%v | 行网格 300s：T0=%s T1=%s 判=%v | 行网格 %ds：T0=%s T1=%s 判=%v",
			now.Format("15:04:05"), want, f1(t0), f1(t1), gotOld, seedStep, f1(t0b), f1(t1b), gotNew)

		if want && !gotOld {
			falseReds++
			t.Logf("  ★ %s：旧网格判出假红（真窗口跨了日，判据说没跨）", now.Format("15:04:05"))
		}
		if gotNew != want {
			t.Errorf("  %s：行网格 %d 秒下仍与真值不符（真=%v 判据=%v）",
				now.Format("15:04:05"), seedStep, want, gotNew)
		}
	}
	if falseReds == 0 {
		t.Error("旧网格一次假红都没复现出来：这条反向验证失效了（判据没有依赖行网格与桶宽的关系）")
		return
	}
	t.Logf("★ 旧网格（300 秒）在这 %d 个采样时刻里复现出 %d 次假红；换成 %d 秒后全部与真值一致",
		len(midnightFalseRedOffsets), falseReds, seedStep)
}
