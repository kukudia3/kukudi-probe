package chart

import (
	"strconv"
	"testing"
	"time"
)

// 本文件是"每天必然红那 10 分钟"的**常驻回归**：它不需要 Chrome、不碰墙上时钟，
// 把"夹具怎么铺数据 + 接口怎么算窗口"这条算术链在**一整天 86400 秒的每一秒**上
// 重放一遍，核对轴那条用例里那句前置条件自检（见 axis_browser_test.go 的
// windowSpansLocalDay）是否真的只在窗口跨日时为真。
//
// 为什么必须单独立一条：那条自检的前置条件是**与时刻有关**的 —— 出问题的那段
// （服务端时区午夜前后）每天只占十几分钟，跑一次真浏览器用例撞不上就等于没测。
// 这里用纯算术把一整天扫完，任何相位都不可能漏。

// axisRangeSpec 复刻 internal/store/ping.go 的 pingRangeSpecs（桶宽 + 窗口长度）。
//
// 刻意在这里写第二份：这张表是**被测行为的一部分**（标签锚点的网格就是它决定的），
// 而从 store 里 import 一份会让"表被改了"这件事在测试里也一起变，等于没有判据。
// 若产品改了表，这里的"行网格 ≤ 桶宽"断言会报出来，提醒重新核对相位。
type axisRangeSpec struct {
	key    string
	bucket int64 // 桶宽（秒）
	win    int64 // 窗口长度（秒）
}

var axisRangeSpecs = []axisRangeSpec{
	{"1h", 60, 3600},
	{"6h", 60, 21600},
	{"12h", 60, 43200},
	{"1d", 120, 86400},
	{"3d", 300, 259200},
	{"7d", 900, 604800},
}

// seedStep 必须与 startAxisFixture 里喂给 SeedPingBuckets 的步长一致。
const seedStep = 60

// axisWindowOf 重放一次查询会看到的首末点与真窗口两端。
//
// 链路（与产品逐字一致）：
//   - 夹具播种（internal/e2e/harness/fixtures.go 的 SeedPingBuckets）：
//     end = now - now%step - step，从 end 往回按 step 铺；
//   - 接口窗口（internal/store/ping.go 的 PingRange.window）：
//     qEnd = now - now%bucket，qStart = qEnd - win；
//   - 查询（同文件的 QueryPingSeries）：取 ts >= qStart && ts < qEnd 的行，
//     每个点的 ts 是 (ts/bucket)*bucket。
func axisWindowOf(now int64, spec axisRangeSpec, step, span int64) (t0, t1, qStart, qEnd int64, ok bool) {
	seedEnd := now - now%step - step
	seedStart := seedEnd - span
	qEnd = now - now%spec.bucket
	qStart = qEnd - spec.win
	for ts := seedStart; ts <= seedEnd; ts += step {
		if ts < qStart || ts >= qEnd {
			continue
		}
		b := ts / spec.bucket * spec.bucket
		if !ok {
			t0, ok = b, true
		}
		t1 = b
	}
	return t0, t1, qStart, qEnd, ok
}

// TestWindowSpansLocalDayAllDay 是这条算术链的常驻判据：一整天逐秒重放，
// 要求"轴用例用的自检"与"真窗口是否跨日"**逐秒一致**。
//
// 在改之前（1d 档：行网格 300 秒、桶宽 120 秒）这条会红在服务端时区
// 午夜前后：真窗口跨了日，而自检说没跨（旧写法拿折过桶的末点当窗口右端，
// 少算了一个桶）。现在夹具的 60 秒网格能整除六档桶宽，两边逐秒相等。
func TestWindowSpansLocalDayAllDay(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("加载时区: %v", err)
	}
	// 一个任取的当地日：判据与"哪一天"无关（逐秒扫完整天）。
	base := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	const span = 7 * 24 * 3600 // 夹具铺 7 天

	for _, spec := range axisRangeSpecs {
		// 前置：行网格必须整除桶宽。这不是"为了好算"，而是首末点能代表窗口两端的
		// 充要条件（见 startAxisFixture / windowSpansLocalDay 的说明）。
		if seedStep > spec.bucket || spec.bucket%seedStep != 0 {
			t.Fatalf("%s 档：行网格 %d 秒没有整除桶宽 %d 秒 —— "+
				"首末点不再等于窗口两端，那条自检的相位就会随墙上时钟漂",
				spec.key, seedStep, spec.bucket)
		}

		bad := 0
		var firstBad, firstBadMsg string
		for sec := int64(0); sec < 86400; sec++ {
			now := base.Unix() + sec
			t0, t1, qStart, qEnd, ok := axisWindowOf(now, spec, seedStep, span)
			if !ok {
				t.Fatalf("%s 档在 %s 铺不出点：夹具的 span 不够长", spec.key,
					time.Unix(now, 0).In(loc).Format("2006-01-02 15:04:05"))
			}
			// 真值：窗口 [qStart, qEnd) 跨当地自然日 ⇔ 左端与"窗口内最后一秒"不在同一天。
			want := dayStartOf(qStart, loc) != dayStartOf(qEnd-1, loc)
			got := windowSpansLocalDay(t0, t1, spec.bucket, loc)
			if got != want {
				bad++
				if firstBadMsg == "" {
					firstBadMsg = "now=" + time.Unix(now, 0).In(loc).Format("2006-01-02 15:04:05") +
						" t0=" + time.Unix(t0, 0).In(loc).Format("01-02 15:04:05") +
						" t1=" + time.Unix(t1, 0).In(loc).Format("01-02 15:04:05") +
						" 窗口=[" + time.Unix(qStart, 0).In(loc).Format("01-02 15:04:05") + "," +
						time.Unix(qEnd, 0).In(loc).Format("01-02 15:04:05") + ")" +
						" 真=" + strconv.FormatBool(want) + " 自检=" + strconv.FormatBool(got)
					firstBad = firstBadMsg
				}
			}
		}
		if bad != 0 {
			t.Errorf("%s 档（桶宽 %d 秒、窗口 %d 秒）：一整天里有 %d 秒的自检结论与真值不符\n  第一处：%s",
				spec.key, spec.bucket, spec.win, bad, firstBad)
			continue
		}
		t.Logf("%s 档（桶宽 %d 秒、窗口 %d 秒）：86400 秒逐秒一致（假红 0、漏报 0）",
			spec.key, spec.bucket, spec.win)
	}
}

// TestWindowSpansLocalDayCatchesCoarseSeedGrid 是上面那条的**反向验证**：
// 把行网格换回改之前的 300 秒，自检必须立刻在服务端时区午夜前后失灵 ——
// 否则说明上面那条根本没在判这件事（判据哑了）。
//
// 它同时钉住"为什么必须改成 60 秒"：60 秒时六档全绿，300 秒时 1d 档必红。
func TestWindowSpansLocalDayCatchesCoarseSeedGrid(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("加载时区: %v", err)
	}
	base := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	const span = 7 * 24 * 3600

	// 1d 档：桶宽 120 秒。行网格 300 秒不能整除它 ⇒ 首末点与窗口两端脱钩。
	spec := axisRangeSpec{"1d", 120, 86400}
	if spec.bucket%300 == 0 {
		t.Fatal("前提不成立：300 秒竟然整除了 1d 桶宽，这条反向验证要换一个档")
	}

	falseRed := 0
	first := ""
	for sec := int64(0); sec < 86400; sec++ {
		now := base.Unix() + sec
		t0, t1, qStart, qEnd, _ := axisWindowOf(now, spec, 300, span)
		want := dayStartOf(qStart, loc) != dayStartOf(qEnd-1, loc)
		got := windowSpansLocalDay(t0, t1, spec.bucket, loc)
		if want && !got {
			falseRed++
			if first == "" {
				first = "now=" + time.Unix(now, 0).In(loc).Format("2006-01-02 15:04:05") +
					" 窗口=[" + time.Unix(qStart, 0).In(loc).Format("01-02 15:04") + "," +
					time.Unix(qEnd, 0).In(loc).Format("01-02 15:04") + ")"
			}
		}
	}
	if falseRed == 0 {
		t.Error("行网格 300 秒时 1d 档竟然一秒假红都没有：这条反向验证失效了" +
			"（说明 windowSpansLocalDay 没有依赖首末点与窗口两端的关系）")
		return
	}
	t.Logf("行网格 300 秒（改之前）：1d 档一整天有 %d 秒判出假红，第一处 %s", falseRed, first)

	// 同一档换成 60 秒网格：假红必须归零（这是修好之后的状态）。
	falseRed60 := 0
	for sec := int64(0); sec < 86400; sec++ {
		now := base.Unix() + sec
		t0, t1, qStart, qEnd, _ := axisWindowOf(now, spec, 60, span)
		want := dayStartOf(qStart, loc) != dayStartOf(qEnd-1, loc)
		if got := windowSpansLocalDay(t0, t1, spec.bucket, loc); want != got {
			falseRed60++
		}
	}
	if falseRed60 != 0 {
		t.Errorf("行网格 60 秒时 1d 档仍有 %d 秒与真值不符", falseRed60)
	}
}
