package store

import (
	"testing"
	"time"
)

// TestAllRangeWindowsAlignWithBuckets 守住"标签与桶都钉在绝对时间网格上"这条设计：
// 基准间隔必须整除窗口长度（否则窗口滑动时两端的标签会一多一少、看起来在跳），
// 对齐后的窗口恰好能容纳 Points() 个完整桶。
//
// 这里**不再**断言"标签不超过 8 个"：服务端已经不抽稀了（屏幕上放不下时由前端按
// 标签的实际像素宽度自动稀疏，见 web/chart.js 的 xLabelStep），基准间隔本来就可能
// 比屏幕能放下的密得多 —— 1h 档 60 个、12h 档 720 个，这正是本次改动的目的。
func TestAllRangeWindowsAlignWithBuckets(t *testing.T) {
	now := time.Now()
	for _, r := range Ranges() {
		if r.TickBaseSec <= 0 {
			t.Errorf("%s：基准间隔 = %d，必须是正数", r.Key, r.TickBaseSec)
		} else if int64(r.Window.Seconds())%r.TickBaseSec != 0 {
			t.Errorf("%s：基准间隔 %d 不能整除窗口 %d 秒，窗口滑动时两端的标签会一多一少",
				r.Key, r.TickBaseSec, int64(r.Window.Seconds()))
		}
		// 服务端不再抽稀：这个字段保留只为不改响应契约，值必须与基准间隔一致。
		if got := r.TickLabelSec(); got != r.TickBaseSec {
			t.Errorf("%s：tick_label_sec = %d，应当恒等于基准间隔 %d（服务端不再抽稀）",
				r.Key, got, r.TickBaseSec)
		}
		// 窗口两端对齐到桶网格后，正好容纳 Points() 个完整桶。
		start, end := r.window(now)
		if count := int(end-start) / int(r.Bucket); count != r.Points() {
			t.Errorf("%s：对齐窗口能容纳 %d 个桶，期望 %d", r.Key, count, r.Points())
		}
		if end%r.Bucket != 0 || start%r.Bucket != 0 {
			t.Errorf("%s：窗口端点没有落在桶网格上（%d, %d）", r.Key, start, end)
		}
		if end > now.Unix() {
			t.Errorf("%s：窗口终点不该超过当前时间", r.Key)
		}
	}
}

// 下面这张表就是 docs/DESIGN.md §10.3 的定稿内容：
// 桶宽与源层是推导出来的，必须和设计文档完全一致。
func TestRangesMatchDesignTable(t *testing.T) {
	want := []struct {
		key      string
		bucket   int64
		source   string
		points   int
		tickBase int64
		mobile   int64
	}{
		{"1h", 10, TableSamples10s, 360, 60, 0},
		{"6h", 30, TableSamples10s, 720, 60, 120},
		{"12h", 60, TableSamples1m, 720, 60, 180},
		{"1d", 120, TableSamples1m, 720, 120, 300},
		{"3d", 300, TableSamples1m, 864, 300, 900},
		{"7d", 900, TableSamples1m, 672, 900, 1800},
	}

	got := Ranges()
	if len(got) != len(want) {
		t.Fatalf("档位数量 = %d，期望 %d", len(got), len(want))
	}
	for i, w := range want {
		r := got[i]
		if r.Key != w.key || r.Bucket != w.bucket || r.Source != w.source || r.Points() != w.points {
			t.Errorf("第 %d 档 = %+v（点数 %d），期望 key=%s bucket=%d source=%s points=%d",
				i, r, r.Points(), w.key, w.bucket, w.source, w.points)
		}
		if r.TickBaseSec != w.tickBase || r.MobileAggSec != w.mobile {
			t.Errorf("%s 刻度 = %d/%d，期望 %d/%d", r.Key, r.TickBaseSec, r.MobileAggSec, w.tickBase, w.mobile)
		}
		if r.Points() > maxPoints {
			t.Errorf("%s 点数 %d 超过上限 %d", r.Key, r.Points(), maxPoints)
		}
	}
}

func TestRangeByKey(t *testing.T) {
	if _, ok := RangeByKey("1h"); !ok {
		t.Fatal("1h 应当存在")
	}
	if _, ok := RangeByKey("1y"); ok {
		t.Fatal("1y 不在定稿的六档里")
	}
	if _, ok := RangeByKey(""); ok {
		t.Fatal("空 key 不应当命中")
	}
}

func TestAllRangesStayUnderPointLimit(t *testing.T) {
	// 更极端的窗口也必须被压到 1000 点以内（桶宽梯级自动放大）。
	for _, window := range []time.Duration{30 * time.Minute, 14 * 24 * time.Hour, 90 * 24 * time.Hour, 365 * 24 * time.Hour} {
		bucket := pickBucket(window)
		points := int64(window.Seconds()) / bucket
		if points > maxPoints {
			t.Errorf("窗口 %s 的桶宽 %ds 会导致 %d 个点", window, bucket, points)
		}
	}
}

// 六档的 X 轴**基准间隔**是用户定稿的新表（改动：旧的 10m/2h/3h/6h/1d/2d 换成
// 1m/1m/1m/2m/5m/15m）。
//
// 为什么基准间隔可以比桶宽还细（6h 档基准 60 秒、桶宽 30 秒；1h 档基准 60 秒、
// 桶宽 10 秒）：基准间隔是"刻度语义"，不是数据粒度。屏幕上真实的标签间隔由前端
// 按标签的实际像素宽度自动稀疏（整齐倍数，见 web/chart.js 的 xLabelStep）——
// 所以"基准 1 分钟"不会被画成 60 个挤在一起的标签。
func TestRangeTickBaseTable(t *testing.T) {
	want := map[string]int64{
		"1h":  60,  // 每 1 分钟
		"6h":  60,  // 每 1 分钟
		"12h": 60,  // 每 1 分钟
		"1d":  120, // 每 2 分钟
		"3d":  300, // 每 5 分钟
		"7d":  900, // 每 15 分钟
	}
	ranges := Ranges()
	if len(ranges) != len(want) {
		t.Fatalf("档位数量 = %d，期望 %d", len(ranges), len(want))
	}
	for _, r := range ranges {
		w, ok := want[r.Key]
		if !ok {
			t.Errorf("出现了未知档位 %q", r.Key)
			continue
		}
		if r.TickBaseSec != w {
			t.Errorf("%s 基准间隔 = %d 秒，期望 %d 秒", r.Key, r.TickBaseSec, w)
		}
	}
}

// 资源图与延迟图是两张档位表（桶宽不同），但**刻度是档位的属性**：同一个 1h 档，
// 无论画 CPU 还是画延迟都是"每 1 分钟"这一档。
//
// 两张表各带一份基准间隔（它们的 key/window 本来就各有一份），这里钉住它们不许漂移：
// 各改各的不会报任何错，画面上只是"延迟图的标签落在另一个网格上"，
// 而这种错位只有把两张图并排看才发现。
func TestPingRangeTickBaseMatchesResourceRanges(t *testing.T) {
	byKey := map[string]int64{}
	for _, r := range Ranges() {
		byKey[r.Key] = r.TickBaseSec
	}
	checked := 0
	for _, r := range PingRanges() {
		want, ok := byKey[r.Key]
		if !ok {
			t.Errorf("延迟图的档位 %q 在资源图里不存在", r.Key)
			continue
		}
		checked++
		if r.TickBaseSec != want {
			t.Errorf("%s 档：延迟图的基准间隔 = %d 秒，资源图 = %d 秒，两张表必须一致",
				r.Key, r.TickBaseSec, want)
		}
	}
	if checked != len(byKey) {
		t.Errorf("只对上了 %d 个档位，资源图有 %d 个", checked, len(byKey))
	}
}

// 服务端不再抽稀：tick_label_sec 恒等于 tick_base_sec，保留字段只为不改响应契约。
//
// 抽稀改由前端按 measureText 量到的实际宽度做（见 web/chart.js 的 xLabelStep）：
// 服务端看不见字号、标签格式与画布宽度，拍一个"最多 8 个"的规则必然在某个档位上
// 要么挤要么空 —— 而"挤"在页面上只是看起来有点糊，不会报任何错。
func TestTickLabelSecIsBaseInterval(t *testing.T) {
	for _, r := range Ranges() {
		if got := r.TickLabelSec(); got != r.TickBaseSec {
			t.Errorf("%s：tick_label_sec = %d，期望与基准间隔 %d 相同", r.Key, got, r.TickBaseSec)
		}
	}
	// 非法输入不 panic，也不返回一个"看起来能用"的数（调用方据此退回默认值）。
	zero := Range{Key: "x", Window: time.Hour}
	if got := zero.TickLabelSec(); got != 0 {
		t.Errorf("基准间隔为 0 时应当返回 0，实际 %d", got)
	}
}

func TestPickBucketIsFromLadder(t *testing.T) {
	allowed := map[int64]bool{}
	for _, b := range bucketLadder {
		allowed[b] = true
	}
	for _, r := range Ranges() {
		if !allowed[r.Bucket] {
			t.Errorf("%s 的桶宽 %d 不在整齐梯级里", r.Key, r.Bucket)
		}
		if r.Bucket%10 != 0 {
			t.Errorf("%s 的桶宽 %d 不是源粒度的整数倍", r.Key, r.Bucket)
		}
	}
}
