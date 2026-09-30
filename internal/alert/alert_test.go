package alert

import (
	"strings"
	"testing"
)

// 告警文案里的流量必须是**十进制**单位：用户买额度时商家说的 1 TB 是 10¹² 字节，
// 前端「月流量额度（GB）」也是 1 GB = 10⁹ 字节。写成 GiB/TiB 会让"已用 / 额度"
// 这一行与用户心里的账对不上（本函数曾经就是 1024 进制）。
//
// 小数位规则必须与前端 fmtScaledBytes **逐字一致**：B 不带小数、10 以下两位、
// 10 及以上一位。告警说「已用 1.5 GB」而面板说「1.50 GB」时，用户没法一眼确认
// 说的是不是同一个数 —— 而这条告警的全部意义就是让他去面板上看。
func TestFormatBytesIsDecimal(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{999, "999 B"},
		{1000, "1.00 KB"},
		{1500, "1.50 KB"},
		{1000 * 1000, "1.00 MB"},
		{250 * 1000 * 1000, "250.0 MB"},
		{1000 * 1000 * 1000, "1.00 GB"},
		// 线上最常见的两条：2000 GB 的额度、用了 1600 GB。
		{2000 * 1000 * 1000 * 1000, "2.00 TB"},
		{1600 * 1000 * 1000 * 1000, "1.60 TB"},
		{1000 * 1000 * 1000 * 1000, "1.00 TB"},
		// 跨过 10 这条线就该掉成一位小数（与前端同一条分支）。
		{10 * 1000 * 1000 * 1000, "10.0 GB"},
		{9990 * 1000 * 1000, "9.99 GB"},
		// 负数当 0 处理（调用方不该传，但显示 "-1.0 KB" 更糟）。
		{-5, "0 B"},
		// 999999 不能写成 "1000 KB"：取整前再升一级。
		{999999, "1.00 MB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 二进制后缀一个都不能再出现在告警文案里 —— 那正是"用户看到的额度比实际大 7.4%"
// 的根源（1024³ 与 10⁹ 差 7.37%）。
func TestFormatBytesHasNoBinarySuffix(t *testing.T) {
	for _, n := range []int64{1 << 10, 1 << 20, 1 << 30, 1 << 40, 2000 << 30} {
		got := formatBytes(n)
		for _, bad := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
			if strings.HasSuffix(got, bad) {
				t.Errorf("formatBytes(%d) = %q：还带着二进制单位 %s", n, got, bad)
			}
		}
	}
}
