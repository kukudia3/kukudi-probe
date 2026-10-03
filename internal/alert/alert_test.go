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
		if got := FormatBytes(c.in); got != c.want {
			t.Errorf("FormatBytes(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 二进制后缀一个都不能再出现在告警文案里 —— 那正是"用户看到的额度比实际大 7.4%"
// 的根源（1024³ 与 10⁹ 差 7.37%）。
func TestFormatBytesHasNoBinarySuffix(t *testing.T) {
	for _, n := range []int64{1 << 10, 1 << 20, 1 << 30, 1 << 40, 2000 << 30} {
		got := FormatBytes(n)
		for _, bad := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
			if strings.HasSuffix(got, bad) {
				t.Errorf("FormatBytes(%d) = %q：还带着二进制单位 %s", n, got, bad)
			}
		}
	}
}

// TestDisplayNameKeepsOneLine 钉住 02·04-2（告警文案侧的控制字符清洗）。
//
// 为什么这条不能省：告警正文是**多行**文本（displayName 之后才是「最后通信：…」
// 「服务端时间：…」），而节点名/分组/地区在接口与库层都没有字符集限制 —— 名字里
// 一个 \n 就能让正文多出一行，把真实的那几行挤到伪造行之下（实测 3 行变 4 行，
// 而且不依赖任何"客户端怎么渲染"的未确认前提：Telegram 请求体没有 parse_mode）。
//
// 清洗集与报告侧 singleLine **逐字符一致**：Cc（\n \r \t U+0085）、Zl/Zp
// （U+2028/U+2029，这两个**不是** Cc 却会被当成换行）、Bidi_Control（U+202E
// 这类双向控制符）。刻意不动整个 Cf —— U+200D 零宽连接符是 emoji 序列的一部分，
// U+00AD 软连字符也是合法字符，一律抹掉会把正常名字改样。
func TestDisplayNameKeepsOneLine(t *testing.T) {
	cases := []struct {
		name, group, region, want string
	}{
		// 正常名字逐字节不变（中文、emoji、星平面字符都不在清洗集里）。
		{"hk-01", "香港", "HK", "hk-01（香港 · HK）"},
		{"hk-01", "", "", "hk-01"},
		{"hk-01", "hk-01", "", "hk-01"}, // 分组与名称相同：不重复写
		{"东京-01🇯🇵", "亚太", "JP", "东京-01🇯🇵（亚太 · JP）"},
		// Cc：换行、回车、制表、NEL。逐个字符换成空格，所以 \r\n 会留下两个
		// 空格（与报告侧 singleLine 同一口径：两步都只是"换个空格"）。
		{"a\nb", "", "", "a b"},
		{"a\r\nb", "", "", "a  b"},
		{"a\tb", "", "", "a b"},
		{"a\u0085b", "", "", "a b"},
		// Zl / Zp：U+2028 / U+2029。
		{"a\u2028b", "", "", "a b"},
		{"a\u2029b", "", "", "a b"},
		// Bidi_Control：从右到左覆盖（U+202E）能把后面的字符显示成别的样子。
		{"hk-01\u202egnp.exe", "", "", "hk-01 gnp.exe"},
		{"a\u200fb", "", "", "a b"}, // RLM
		// 残留（两处口径一致的取舍）：U+200B 零宽空格与 U+FEFF 属于 Cf 而不是
		// Bidi_Control，按报告侧同一份口径**原样保留** —— 它们不换行，但能让两个
		// 不同的名字看起来一样，这一条列为残余风险。
		{"a\u200bb", "", "", "a\u200bb"},
		{"a\ufeffb", "", "", "a\ufeffb"},
		// 分组与地区同样要清洗：它们也会进正文。
		{"hk-01", "香港\n合计 1.00 GB", "HK", "hk-01（香港 合计 1.00 GB · HK）"},
		{"hk-01", "", "HK\u2028", "hk-01（HK）"},
		// 首尾空白去掉（与报告侧 singleLine 同口径）；只有空白的分组等于没填。
		{"  hk-01  ", "   ", "", "hk-01"},
	}
	for _, c := range cases {
		got := DisplayName(c.name, c.group, c.region)
		if got != c.want {
			t.Errorf("DisplayName(%q, %q, %q) = %q，期望 %q", c.name, c.group, c.region, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("DisplayName(%q, …) = %q：还有能断行的字符", c.name, got)
		}
	}
}
