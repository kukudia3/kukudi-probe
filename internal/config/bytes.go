package config

import (
	"fmt"
	"strconv"
	"strings"
)

// 字节单位（1024 进制，与 Agent 上报的口径一致）。
var byteUnits = []struct {
	suffix string
	scale  int64
}{
	{"TiB", 1 << 40},
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// ParseBytes 解析 "1TiB"、"512GiB"、"1048576" 这类人类可读的字节数。
func ParseBytes(spec string) (int64, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return 0, fmt.Errorf("不能为空")
	}
	upper := strings.ToUpper(s)
	for _, unit := range byteUnits {
		u := strings.ToUpper(unit.suffix)
		if !strings.HasSuffix(upper, u) {
			continue
		}
		number := strings.TrimSpace(s[:len(s)-len(unit.suffix)])
		if number == "" {
			return 0, fmt.Errorf("缺少数值")
		}
		value, err := strconv.ParseFloat(number, 64)
		if err != nil {
			return 0, fmt.Errorf("数值 %q 无法解析", number)
		}
		if value < 0 {
			return 0, fmt.Errorf("不能为负")
		}
		total := value * float64(unit.scale)
		if total > float64(1<<62) {
			return 0, fmt.Errorf("数值过大")
		}
		return int64(total), nil
	}

	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析 %q（可用单位：B、KiB、MiB、GiB、TiB）", s)
	}
	if value < 0 {
		return 0, fmt.Errorf("不能为负")
	}
	return value, nil
}

// FormatBytes 把字节数格式化成最合适的单位（用于显示默认值）。
func FormatBytes(bytes int64) string {
	for _, unit := range byteUnits {
		if bytes >= unit.scale && bytes%unit.scale == 0 {
			return strconv.FormatInt(bytes/unit.scale, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(bytes, 10) + "B"
}
