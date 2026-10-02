package alert

import (
	"strings"
	"testing"
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
