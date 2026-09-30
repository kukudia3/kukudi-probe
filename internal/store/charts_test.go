package store

import (
	"context"
	"reflect"
	"testing"
)

// 缺省（键不存在）＝ 全部显示：新装的环境不该是"一张图都没有"。
func TestVisibleChartsDefaultsToAll(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	got, err := db.VisibleCharts(ctx)
	if err != nil {
		t.Fatalf("读取图表设置: %v", err)
	}
	if !reflect.DeepEqual(got, AllCharts) {
		t.Fatalf("缺省应当返回全部 %d 个图表 %v，实际 %v", len(AllCharts), AllCharts, got)
	}
	if len(got) != 6 {
		t.Fatalf("合法图表应当是 6 个，实际 %d", len(got))
	}

	// 返回的必须是副本：调用方（前端渲染路径）改到它不能污染键表本身。
	got[0] = "hacked"
	if AllCharts[0] != "cpu" {
		t.Fatal("VisibleCharts 返回了 AllCharts 本身，调用方能把键表改坏")
	}
}

// 脏数据（手改库、旧版本写坏、人为塞了别的 JSON）一律回退到"全部显示"。
//
// 宁可多画几张图，也不能让首页因为一行设置解析失败就整块白掉——
// 这个接口在每次进详情页前都会被调用。
func TestVisibleChartsFallsBackOnDirtyValue(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	for _, dirty := range []string{
		"",              // 空字符串
		"not json",      // 完全不是 JSON
		`{"cpu": true}`, // 是 JSON 但不是字符串数组
		`"cpu"`,         // 是 JSON 字符串，不是数组
		`[1, 2]`,        // 数组里是数字
		`["cpu",`,       // 截断的 JSON（写到一半断电）
	} {
		if err := db.SetSettings(ctx, map[string]string{KeyVisibleCharts: dirty}); err != nil {
			t.Fatalf("写入脏数据 %q: %v", dirty, err)
		}
		got, err := db.VisibleCharts(ctx)
		if err != nil {
			t.Fatalf("脏数据 %q 不该返回错误: %v", dirty, err)
		}
		if !reflect.DeepEqual(got, AllCharts) {
			t.Errorf("脏数据 %q 应当回退到全部显示，实际 %v", dirty, got)
		}
	}
}

// Set / Get 往返，以及三种必须区分开的状态：缺省（全显示）、子集、空数组（全隐藏）。
func TestSetVisibleChartsRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	if err := db.SetVisibleCharts(ctx, []string{"lat", "cpu"}); err != nil {
		t.Fatalf("保存图表设置: %v", err)
	}
	got, err := db.VisibleCharts(ctx)
	if err != nil {
		t.Fatalf("读取图表设置: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"lat", "cpu"}) {
		t.Fatalf("读回的值 = %v，期望 [lat cpu]（顺序应当保持）", got)
	}

	// 重复项与未知键被过滤：未知键画不出来，留着只会变成界面上永远空着的一格。
	if err := db.SetVisibleCharts(ctx, []string{"cpu", "cpu", "bogus", "mem"}); err != nil {
		t.Fatalf("保存图表设置: %v", err)
	}
	if got, err = db.VisibleCharts(ctx); err != nil {
		t.Fatalf("读取图表设置: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cpu", "mem"}) {
		t.Fatalf("去重/过滤后的值 = %v，期望 [cpu mem]", got)
	}

	// 显式空数组 = 全部隐藏，必须与"键不存在"（全部显示）区分开。
	if err := db.SetVisibleCharts(ctx, nil); err != nil {
		t.Fatalf("保存空设置: %v", err)
	}
	got, err = db.VisibleCharts(ctx)
	if err != nil {
		t.Fatalf("读取图表设置: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("显式存空数组应当表示全部隐藏，实际 %v", got)
	}
	raw, ok, err := db.GetSetting(ctx, KeyVisibleCharts)
	if err != nil || !ok {
		t.Fatalf("读取原始设置: %v ok=%v", err, ok)
	}
	// 存成 null 的话，前端拿到 null 会当成"服务端没给"而回退成全部显示。
	if raw != "[]" {
		t.Fatalf("空设置应当存成 []，实际 %q", raw)
	}

	// 绕过 SetVisibleCharts 直接写进未知键时，读取侧也要忽略它。
	if err := db.SetSettings(ctx, map[string]string{KeyVisibleCharts: `["cpu","nope"]`}); err != nil {
		t.Fatalf("写入设置: %v", err)
	}
	if got, err = db.VisibleCharts(ctx); err != nil {
		t.Fatalf("读取图表设置: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cpu"}) {
		t.Fatalf("未知键应当被忽略，实际 %v", got)
	}
}

func TestIsKnownChart(t *testing.T) {
	for _, key := range AllCharts {
		if !IsKnownChart(key) {
			t.Errorf("%q 应当是合法图表键", key)
		}
	}
	for _, key := range []string{"", "CPU", "gpu", "net_down", "traffic "} {
		if IsKnownChart(key) {
			t.Errorf("%q 不该被当成合法图表键", key)
		}
	}
}
