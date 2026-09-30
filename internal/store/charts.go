package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// KeyVisibleCharts 是"详情页显示哪些图表"的设置键。
const KeyVisibleCharts = "visible_charts"

// AllCharts 是全部合法图表键，顺序即界面顺序。
//
// 它同时被前端（复选框与 data-chart）、服务端（校验与 all 字段）与这里共用，
// 三处只有这一份定义，不会出现"前端多画一张、后端不认"的漂移。
var AllCharts = []string{"cpu", "mem", "disk", "net", "lat", "traffic"}

// IsKnownChart 判断图表键是否合法。
func IsKnownChart(key string) bool {
	for _, k := range AllCharts {
		if k == key {
			return true
		}
	}
	return false
}

// AllChartsCopy 返回全部图表键的副本（调用方拿到的是自己的切片，改不动这份定义）。
func AllChartsCopy() []string {
	out := make([]string, len(AllCharts))
	copy(out, AllCharts)
	return out
}

// VisibleCharts 读取要显示的图表键。
//
// 缺省（键不存在）＝ 全部显示，显式存空数组＝全部隐藏，两者必须区分开：
// 前者是"还没设置过"的新装环境，后者是用户主动把图全关掉。
//
// 脏数据（不是 JSON 数组、或混进未知键）一律当作"全部显示"处理，宁可多画几张小图，
// 也不能让首页因为一行设置解析失败就整块白掉。
func (d *DB) VisibleCharts(ctx context.Context) ([]string, error) {
	raw, ok, err := d.GetSetting(ctx, KeyVisibleCharts)
	if err != nil {
		return nil, err
	}
	if !ok {
		return AllChartsCopy(), nil
	}

	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return AllChartsCopy(), nil
	}
	// 未知键只是画不出来，丢掉即可（不当作脏数据整体回退）。
	out := make([]string, 0, len(parsed))
	seen := make(map[string]bool, len(parsed))
	for _, key := range parsed {
		if !IsKnownChart(key) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out, nil
}

// SetVisibleCharts 保存要显示的图表键。
//
// 与 VisibleCharts 一样做去重与未知键过滤：存储层是最后一道防线，
// 直接调它的调用方（将来的 CLI、导入脚本）不该能把脏数据写进去。
func (d *DB) SetVisibleCharts(ctx context.Context, keys []string) error {
	clean := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !IsKnownChart(key) || seen[key] {
			continue
		}
		seen[key] = true
		clean = append(clean, key)
	}
	// 空数组也要真的写进去（它表示"全部隐藏"，与"键不存在"是两回事）。
	encoded, err := json.Marshal(clean)
	if err != nil {
		return fmt.Errorf("序列化图表设置失败: %w", err)
	}
	return d.SetSettings(ctx, map[string]string{KeyVisibleCharts: string(encoded)})
}
