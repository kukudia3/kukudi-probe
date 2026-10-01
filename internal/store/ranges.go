package store

import "time"

// Range 是一个历史时间档位。
//
// 六档范围（1h/6h/12h/1d/3d/7d）与 X 轴刻度来自用户定稿；桶宽与源层是**推导**出来的
// （见 pickBucket / pickSource），并用测试锁死与 docs/DESIGN.md §10.3 的表格一致。
type Range struct {
	Key    string
	Window time.Duration
	// Bucket 是桶宽（秒），Source 是源表。
	Bucket int64
	Source string
	// TickBaseSec 是 X 轴的**基准间隔**（秒）：标签锚点之间的最小间隔，
	// 也就是"用户语义上的刻度"（1h 档 = 每 1 分钟一根）。
	//
	// 它**不等于**屏幕上真实的标签间隔：放不下时由前端按标签文本的实际像素宽度
	// 自动按整齐倍数稀疏（见 web/chart.js 的 xLabelStep）。为什么把稀疏放到前端：
	// 只有那边量得到标签的真实宽度（measureText）与画布宽度；服务端拍一个
	// "最多 8 个"的规则，在换个字号/格式/窄画布之后必然要么挤要么空。
	//
	// 它也可能比**桶宽**还细（1h 档基准 60 秒、桶宽 10 秒；6h 档基准 60 秒、
	// 桶宽 30 秒）：基准间隔讲的是刻度语义，不是数据粒度。
	TickBaseSec int64
	// MobileAggSec 是手机端二次聚合的目标间隔（0=不聚合）。
	MobileAggSec int64
}

// Points 返回该档位的点数。
func (r Range) Points() int {
	if r.Bucket <= 0 {
		return 0
	}
	return int(r.Window.Seconds()) / int(r.Bucket)
}

// window 返回查询窗口 [start, end)，两端都对齐到桶网格。
//
// 为什么要对齐：查询结果里的每个点都代表"一个完整的桶"。如果起点不对齐，
// 第一个（以及"现在"所在的最后一个）桶可能只统计了几秒到几分钟的数据，
// 却被画成一个完整桶——例如 7d 档的第一个 15 分钟点可能只包含 1 分钟数据。
// 对齐之后：点数恰好等于 Points()，首尾都是完整桶；代价是最新点最多滞后
// 一个桶（1h 档 10 秒，7d 档 15 分钟），这对历史曲线完全可接受。
func (r Range) window(now time.Time) (int64, int64) {
	if r.Bucket <= 0 {
		return now.Unix(), now.Unix()
	}
	end := now.Unix() - (now.Unix() % r.Bucket)
	return end - int64(r.Window.Seconds()), end
}

// TickLabelSec 返回 X 轴标签间隔（秒），现在**恒等于 TickBaseSec**。
//
// 它历史上是"服务端抽稀到 ≤8 个标签"的结果，而 chart.js 拿它**同时**画两样东西：
// 标签，以及每个标签位置上的竖网格线（`step = tickLabelSec || tickBaseSec`）。
// 现在这两件事都不成立了：
//   - 竖网格线整条删掉了（读数值靠横网格线 + Y 轴刻度，见 web/chart.js 的 draw）；
//   - 标签稀疏改由前端按 measureText 的实际宽度做（整齐倍数，见 xLabelStep）。
//
// 所以这个字段**不再影响画面上的任何一条线**，值恒等于 TickBaseSec。
// 保留它只是为了不改动响应里的 tick_label_sec（契约与老客户端还在读）；
// 新代码一律读 tick_base_sec。
func (r Range) TickLabelSec() int64 { return r.TickBaseSec }

// bucketLadder 是允许的桶宽梯级（秒）。只用这些"整齐"的值，
// 这样同一个范围切换时刻度稳定、标签不会跳。
var bucketLadder = []int64{10, 20, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400}

// maxPoints 是任何档位返回点数的硬上限（设计约束：≤1000）。
const maxPoints = 1000

// tier 是一级存储。
type tier struct {
	table     string
	width     int64
	retention time.Duration
}

// tiers 由细到粗排列。
var tiers = []tier{
	{TableSamples10s, 10, 12 * time.Hour},
	{TableSamples1m, 60, 8 * 24 * time.Hour},
}

// pickBucket 取"使点数不超过 1000 的最小整齐桶宽"。
func pickBucket(window time.Duration) int64 {
	seconds := int64(window.Seconds())
	for _, b := range bucketLadder {
		if seconds/b <= maxPoints {
			return b
		}
	}
	return bucketLadder[len(bucketLadder)-1]
}

// pickSource 取能覆盖该范围的最细一层。
//
// 若最终桶宽已经不比更粗一层更细，且那一层也覆盖这个范围，就直接用更粗层：
// 结果完全相同，但要扫的行数少一个数量级。
func pickSource(window time.Duration, bucket int64) string {
	idx := -1
	for i, t := range tiers {
		if t.retention > window {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = len(tiers) - 1
	}
	for idx+1 < len(tiers) {
		next := tiers[idx+1]
		if next.retention > window && bucket >= next.width {
			idx++
			continue
		}
		break
	}
	return tiers[idx].table
}

// rangeSpecs 是六档的"用户定稿部分"：范围、X 轴**基准**间隔与手机端聚合目标。
//
// tickBase 这一列是用户定稿的新表（旧的 10m/2h/3h/6h/1d/2d 换成
// 1m/1m/1m/2m/5m/15m）：它比大多数档位的桶宽细得多，屏幕上必然放不下 ——
// 放不下的部分由前端按标签实际宽度自动稀疏（整齐倍数），服务端不再预先抽稀。
var rangeSpecs = []struct {
	key       string
	window    time.Duration
	tickBase  int64
	mobileAgg int64
}{
	{"1h", time.Hour, 60, 0},
	{"6h", 6 * time.Hour, 60, 120},
	{"12h", 12 * time.Hour, 60, 180},
	{"1d", 24 * time.Hour, 120, 300},
	{"3d", 72 * time.Hour, 300, 900},
	{"7d", 7 * 24 * time.Hour, 900, 1800},
}

// Ranges 返回六档（顺序固定：从短到长）。
func Ranges() []Range {
	out := make([]Range, 0, len(rangeSpecs))
	for _, spec := range rangeSpecs {
		bucket := pickBucket(spec.window)
		out = append(out, Range{
			Key:          spec.key,
			Window:       spec.window,
			Bucket:       bucket,
			Source:       pickSource(spec.window, bucket),
			TickBaseSec:  spec.tickBase,
			MobileAggSec: spec.mobileAgg,
		})
	}
	return out
}

// RangeByKey 按 key 取档位。
func RangeByKey(key string) (Range, bool) {
	for _, r := range Ranges() {
		if r.Key == key {
			return r, true
		}
	}
	return Range{}, false
}
