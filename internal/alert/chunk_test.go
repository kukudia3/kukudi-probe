package alert

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// unitsOf 是测试自己的"UTF-16 单元数"计数器。
//
// **故意**不复用 msgUnits：断言与被测实现用同一个函数时，"把 msgUnits 退化成数 rune"
// 这种回归会让断言跟着一起失效（自己和自己永远一致）。
func unitsOf(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// TestChunkLinesCountsUTF16Units 守住分片计账的单位：UTF-16 单元，而不是 rune。
//
// 每行 1500 个 emoji：rune 数 1500、UTF-16 单元数 3000。按 rune 计账时两行
// （1501+1501 = 3002 ≤ 3500）会落进同一片，实际是 6002 个单元 —— Telegram 按
// 4096 单元整条拒收，用户一条都收不到。
func TestChunkLinesCountsUTF16Units(t *testing.T) {
	line := strings.Repeat("🔴", 1500) + "\n" // 1500 rune / 3000 个单元

	chunks := ChunkLines([]string{line, line}, 0, alertMessageMaxUnits)
	if len(chunks) != 2 {
		t.Fatalf("每行 3000 个 UTF-16 单元，3500 的预算下一片只能放一行，实际 %d 片", len(chunks))
	}
	for i, chunk := range chunks {
		if units := unitsOf(strings.Join(chunk, "\n")); units > alertMessageMaxUnits {
			t.Errorf("第 %d 片有 %d 个 UTF-16 单元，超过预算 %d", i+1, units, alertMessageMaxUnits)
		}
	}
}

// TestRenderMessagesCountsUTF16Units 守住同一条口径在告警侧也成立。
//
// 一条 3000 个 emoji 的消息：rune 数约 3000（看起来在 3500 的预算之内），
// Telegram 按单元算是 6000 —— 超过 4096，整条被拒收（重试三次后计入 failed，
// 用户连半条都看不到）。按 rune 计账时这里走"装得下"的快路径，根本不会分片。
func TestRenderMessagesCountsUTF16Units(t *testing.T) {
	batch := []Notification{{
		NodeID:   1,
		Rule:     RuleOffline,
		Severity: SeverityCritical,
		Title:    strings.Repeat("🔴", 3000),
		Body:     "x",
	}}

	msgs := renderMessages(batch, alertMessageMaxUnits)
	if len(msgs) == 0 {
		t.Fatal("渲染结果为空")
	}
	for i, m := range msgs {
		if units := unitsOf(m.Body); units > 4096 {
			t.Errorf("第 %d 条有 %d 个 UTF-16 单元，超过 Telegram 的 4096（整条会被拒收）", i+1, units)
		}
	}
	// 单条事件本身就超长：只能截断，而且必须留下"已截断"的说明
	// （静默丢内容正是分片功能要消灭的那个故障）。
	if !strings.HasSuffix(msgs[0].Body, "…（已截断）") {
		t.Errorf("超长内容被丢弃时必须留下标记，实际结尾：%q", lastRunes(msgs[0].Body, 20))
	}
}

// lastRunes 返回文本末尾的 n 个 rune（只用于错误信息，避免把 6000 个 emoji 打进日志）。
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// TestRenderMessagesClampsNoCoalesce 是 05-A-6 在告警侧的形态。
//
// NoCoalesce 的消息是调用方（定时流量报告）自己切好片的，这里不重新分片 ——
// 但"不重新分片"不等于"不设防"：某一片本身超长（名字列刻意不截断，几千字符的
// 名字能让一行顶破 4096）时，Telegram 会把**整条**拒收，收件人连半条都收不到。
// 所以它必须和其它路径一样过截断兜底。
//
// 合法数据（名字 ≤64 rune）永远进不到截断分支 —— 输出逐字节不变，这一点由
// 报告侧的既有用例（每片 ≤3000 单元、每台机器都在）钉着。
//
// 反向验证的说明（如实）：把 NoCoalesce 分支的截断撤掉之后，这条用例**仍然绿** ——
// 因为单条消息的批次走到下面的分片逻辑时，产出的文本与这个分支逐字节相同
// （Dispatcher 永远一次只送一条 NoCoalesce 通知，见 Start）。所以它是**不变式**
// 用例（"NoCoalesce 的消息也必须在预算内"），而不是分支用例；真正可反向验证的是
// 报告侧那道兜底（internal/server 的 TestTrafficReportSingleChunkLengthGuard）。
func TestRenderMessagesClampsNoCoalesce(t *testing.T) {
	// 20000 个汉字 = 20000 个 UTF-16 单元：远超 4096。
	batch := []Notification{{
		Rule:       RuleTrafficReport,
		Severity:   SeverityInfo,
		Body:       "📊 流量日报（昨天 10-06） 1/1\n统计区间：…\n" + strings.Repeat("名", 20000),
		NoCoalesce: true,
	}}

	msgs := renderMessages(batch, alertMessageMaxUnits)
	if len(msgs) != 1 {
		t.Fatalf("NoCoalesce 的消息不该被重新分片，实际 %d 条", len(msgs))
	}
	units := unitsOf(msgs[0].Body)
	if units > alertMessageMaxUnits {
		t.Fatalf("这条有 %d 个 UTF-16 单元，超过预算 %d：会被 Telegram 整条拒收", units, alertMessageMaxUnits)
	}
	if !strings.HasSuffix(msgs[0].Body, "…（已截断）") {
		t.Errorf("超长内容被截掉时必须留下标记，实际结尾：%q", lastRunes(msgs[0].Body, 20))
	}
	if !strings.HasPrefix(msgs[0].Body, "📊 流量日报") {
		t.Errorf("截断把标题切掉了，实际开头：%q", lastRunes(msgs[0].Body, 20))
	}
	// 没超预算的消息一个字都不动（快路径 + NoCoalesce 两条分支都必须如此）。
	short := []Notification{{
		Rule: RuleTrafficReport, Body: "📊 流量日报（昨天 10-06）\n合计 0 B", NoCoalesce: true,
	}}
	if got := renderMessages(short, alertMessageMaxUnits); len(got) != 1 || got[0].Body != short[0].Body {
		t.Fatalf("没超预算的 NoCoalesce 消息被改动了：%q", got[0].Body)
	}
}

// TestTruncateCoversCharacterClasses 覆盖分片/截断的四种字符形态。
//
// 为什么要按字符类别分开钉：这套账目全部按 **UTF-16 单元** 算（Telegram 的口径），
// 而四种字符的"rune 数 ÷ 单元数"各不相同 —— 纯 ASCII 与汉字是 1:1，星平面字符
// （emoji、生僻字、旗帜）是 1:2。按 rune 计账时，星平面字符密集的文本会被判定成
// "装得下"，到线上就是整条被拒收（重试三次后计入 failed，用户连半条都看不到）。
//
// 另外两件事一并钉住：截断点必须落在**整字符边界**上（不许把一个字符劈成半个、
// 切出非法 UTF-8），以及"截断后的内容必须是原文的前缀"（不许凭空造字）。
func TestTruncateCoversCharacterClasses(t *testing.T) {
	const maxUnits = 3000
	cases := []struct {
		name    string
		unit    string
		perUnit int // 一个字符占几个 UTF-16 单元
	}{
		{"纯 ASCII", "a", 1},
		{"纯中文", "名", 1},
		{"emoji 密集", "🔴", 2},
		{"星平面字符", "𠀀", 2}, // U+20000（CJK 扩展 B）
		{"旗帜（两个星平面字符合成一个）", "🇭🇰", 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Repeat(c.unit, maxUnits+2) // 一定超过预算（每个字符至少占 1 个单元）
			out, truncated := Truncate(text, maxUnits)
			if !truncated {
				t.Fatal("超长文本应当报告发生了截断（调用方要靠它记 Warn）")
			}
			if units := unitsOf(out); units > maxUnits {
				t.Fatalf("截断后仍有 %d 个单元（上限 %d）", units, maxUnits)
			}
			if !strings.HasSuffix(out, "…（已截断）") {
				t.Fatalf("截断必须留下标记，实际结尾：%q", lastRunes(out, 20))
			}
			if !utf8.ValidString(out) {
				t.Fatal("截断切出了非法 UTF-8：把一个字符劈成了两半")
			}
			body := strings.TrimSuffix(out, "\n…（已截断）")
			if !strings.HasPrefix(text, body) {
				t.Fatal("截断后的内容不是原文的前缀")
			}
			if units := unitsOf(body); units%c.perUnit != 0 {
				t.Fatalf("留下来的 %d 个单元不是 %d 的整数倍：有字符被劈开了", units, c.perUnit)
			}
		})
	}

	// 恰好装得下：一个字都不动，也**不**报截断（否则报告路径会为每条消息刷 Warn）。
	exact := strings.Repeat("名", maxUnits)
	if got, truncated := Truncate(exact, maxUnits); truncated || got != exact {
		t.Fatalf("恰好装得下时不该截断（%v，长度 %d）", truncated, unitsOf(got))
	}
}
