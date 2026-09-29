package store

import (
	"testing"
	"time"
)

// TestAllRangeTickLabelsStayReadable 守住"刻度标签超过 8 个就抽稀"这条规则，
// 并确认对齐后的窗口恰好能容纳 Points() 个完整桶。
func TestAllRangeTickLabelsStayReadable(t *testing.T) {
	now := time.Now()
	for _, r := range Ranges() {
		shown := int64(r.Window.Seconds()) / r.TickLabelSec()
		if shown > 8 {
			t.Errorf("%s：抽稀后仍有 %d 个标签（上限 8）", r.Key, shown)
		}
		if r.TickLabelSec()%r.TickBaseSec != 0 {
			t.Errorf("%s：标签间隔 %d 不是基础刻度 %d 的整数倍", r.Key, r.TickLabelSec(), r.TickBaseSec)
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
		{"1h", 10, TableSamples10s, 360, 600, 0},
		{"6h", 30, TableSamples10s, 720, 7200, 120},
		{"12h", 60, TableSamples1m, 720, 10800, 180},
		{"1d", 120, TableSamples1m, 720, 21600, 300},
		{"3d", 300, TableSamples1m, 864, 86400, 900},
		{"7d", 900, TableSamples1m, 672, 172800, 1800},
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

func TestTickLabelSecDecimation(t *testing.T) {
	cases := map[string]int64{
		"1h":  600,    // 6 个标签，不用抽稀
		"6h":  7200,   // 3 个
		"12h": 10800,  // 4 个
		"1d":  21600,  // 4 个
		"3d":  86400,  // 3 个
		"7d":  172800, // 3~4 个
	}
	for key, want := range cases {
		r, ok := RangeByKey(key)
		if !ok {
			t.Fatalf("%s 不存在", key)
		}
		if got := r.TickLabelSec(); got != want {
			t.Errorf("%s 标签间隔 = %d，期望 %d", key, got, want)
		}
		if labels := int64(r.Window.Seconds()) / r.TickLabelSec(); labels > 8 {
			t.Errorf("%s 抽稀后仍有 %d 个标签", key, labels)
		}
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
