package alert

import (
	"fmt"
	"strings"
)

// alertMessageMaxUnits 是一条告警消息的长度预算（单位是 UTF-16 单元，与 Telegram 一致）。
//
// Telegram 的 sendMessage 上限是 4096 个 UTF-16 单元（一个 🔴 占 2 个），这里取 3500，
// 留出约 15% 余量。超限的后果不是"少显示一点"，而是**整条被拒收** —— 分发器重试三次
// 之后计入 failed，用户什么也收不到，也不知道丢了什么。宁可早切一片，也不去贴那条线。
//
// 为什么单位必须是 UTF-16 单元而不是 rune：单元数 = rune 数 + 星平面字符数，
// 按 rune 计账时那 15% 余量在 emoji/旗帜密集的消息上会失效（renderMessages 会认定
// "3500 装得下"而不分片，Telegram 按 4096 单元整条拒收）。预算、快路径、
// ChunkLines 的计账、截断四处现在用的是同一个单位，账目才对得上。
const alertMessageMaxUnits = 3500

// msgUnits 返回文本在 Telegram 眼里有多长：UTF-16 单元数。
//
// 一个 rune 占 1 个单元，除非它在星平面（U+10000 以上，emoji、旗帜、生僻字）——
// 那些在 UTF-16 里是代理对，占 2 个。这就是 Telegram 数 4096 的方式。
func msgUnits(s string) int {
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

// chunkCounterReserve 是分片序号（"（9/9）"）在长度预算里占的位置。
const chunkCounterReserve = 8

// renderOne 渲染**一条**事件：Title 非空时是「图标 + 标题 + 换行 + 正文」。
//
// Title 为空表示这条通知的 Body 就是完整消息（分发器合并之后交给通知器的那一条、
// 以及定时流量报告的每一片都是这样，见 batchNotification 与 internal/server 的
// trafficReportNotifications）—— 这时不再补标题行：补了消息开头就会出现两行标题，
// 而且补的那一行图标是按 severity 猜的。
func renderOne(n Notification) string {
	if n.Title == "" {
		return n.Body
	}
	var b strings.Builder
	b.WriteString(icon(n))
	b.WriteString(" ")
	b.WriteString(n.Title)
	if n.Body != "" {
		b.WriteString("\n")
	}
	b.WriteString(n.Body)
	return b.String()
}

// renderBlocks 把一批事件逐条渲染成"块"（每块一条事件，可能多行）。
func renderBlocks(batch []Notification) []string {
	blocks := make([]string, 0, len(batch))
	for _, n := range batch {
		blocks = append(blocks, renderOne(n))
	}
	return blocks
}

// batchSeverity 返回批内最高级别（🔴 > 🟡 > 🟢）。
func batchSeverity(batch []Notification) Severity {
	severity := SeverityInfo
	for _, n := range batch {
		switch n.Severity {
		case SeverityCritical:
			return SeverityCritical
		case SeverityWarn:
			severity = SeverityWarn
		}
	}
	return severity
}

// batchHeadLine 返回合并批次的头行（只发一条事件时为空字符串）。
//
// 头行的图标与文字**同源**：都取自批内最高级别（见 batchSeverity）。以前是图标
// 取最高级、文字取第一条，于是会得到「🔴 流量接近额度 等 2 条」—— 图标说这是
// 严重告警，文字说的却是一条预警规则，一眼看不出这一批到底是什么级别。
// 而文字里写具体规则名还有第二个毛病：那条规则的标题会在消息里出现两遍
// （头行一遍、它自己那一块一遍）。
//
// 所以头行只说**这一批有几条**：级别交给图标，数量交给文字，谁都不重复。
func batchHeadLine(batch []Notification) string {
	if len(batch) < 2 {
		return ""
	}
	return icon(Notification{Severity: batchSeverity(batch)}) + fmt.Sprintf(" 合并 %d 条通知", len(batch))
}

// renderMessages 把一批事件渲染成 1..N 条通知，每条都在 maxUnits 个 UTF-16 单元以内。
//
// 为什么必须有这一步：合并窗口最多收 10 条事件（见 DispatcherOptions），每条都带
// 节点名、分组、地区与两三行正文 —— 叠起来超过 Telegram 的 4096 时整条消息会被
// 拒收，用户不会收到任何提示。这里把"发不出去"换成"分成几条发出去"。
//
// 切法是**按整条事件切**，复用定时流量报告那套 ChunkLines：报告切的是"行"
// （每行一台机器），这里切的是"块"（每块一条事件）。把一条事件从中间劈开
// （标题在一片、节点名在另一片）比少收半条还难读。
func renderMessages(batch []Notification, maxUnits int) []Notification {
	if len(batch) == 0 {
		return nil
	}
	// NoCoalesce：调用方（定时流量报告）已经自己切好片了，这里一个字都不动 ——
	// 它每一片都带着自己的「📊 标题 i/N」头行，再切一次会把头行与正文拆散。
	if batch[0].NoCoalesce {
		return []Notification{batchNotification(batch, RenderBatch(batch))}
	}
	// 快路径：一条消息装得下（线上绝大多数告警都走这里），输出与分片功能加入之前
	// 逐字节一致 —— 只有真的超长时才动格式。
	if text := RenderBatch(batch); msgUnits(text) <= maxUnits {
		return []Notification{batchNotification(batch, text)}
	}

	blocks, head := renderBlocks(batch), batchHeadLine(batch)
	budget := maxUnits - chunkCounterReserve
	if head != "" {
		budget -= msgUnits(head) + 2 // +2 是头行与正文之间那个空行
	}
	// 每个元素带一个尾随换行，片内用 "\n" 连接 —— 于是块与块之间正好空一行，
	// 而 ChunkLines 按「元素字符数 + 1 个连接符」计账，账目与实际长度一致
	// （详见 ChunkLines 的注释）。
	units := make([]string, len(blocks))
	for i, block := range blocks {
		units[i] = block + "\n"
	}
	chunks := ChunkLines(units, 0, budget)

	out := make([]Notification, 0, len(chunks))
	for i, chunk := range chunks {
		header := head
		// 分片序号：收件人必须能看出"还有没有下文"，否则最后一片丢了他也不知道。
		// 只有真的分了片才写（chunks > 1 ⟹ 至少两块 ⟹ 头行非空，见 batchHeadLine）。
		if len(chunks) > 1 {
			header = fmt.Sprintf("%s（%d/%d）", head, i+1, len(chunks))
		}
		text := strings.TrimRight(strings.Join(chunk, "\n"), "\n")
		if header != "" {
			text = header + "\n\n" + text
		}
		// 兜底：单块本身就超长时（例如被直接写进库的超长节点名），ChunkLines 会
		// 让它独占一片而不是从中间切断 —— 那一片仍然超限，只能截断。
		out = append(out, batchNotification(batch, truncateWithMark(text, maxUnits)))
	}
	return out
}

// truncateWithMark 把超长文本截到 maxUnits 个 UTF-16 单元以内，并在末尾注明"已截断"。
//
// 截断是**有损**的：排在后面的机器会就此从这个批次里消失。硬截而不说明，
// 用户根本不知道少了东西 —— 而"静默丢内容"正是要修的毛病（超长消息被 Telegram
// 整条拒收时，用户连半条都收不到）。所以任何一次截断都必须留下这句话。
func truncateWithMark(text string, maxUnits int) string {
	const mark = "\n…（已截断）"
	if msgUnits(text) <= maxUnits {
		return text
	}
	keep := maxUnits - msgUnits(mark)
	if keep < 0 {
		keep = 0
	}
	// 按 rune 累加单元数，装不下就停：按 rune 切是为了不把一个汉字劈成半个
	// （字节切会切出乱码），而计数必须按单元走 —— 只数 rune 的话，星平面字符
	// 密集时截断后的文本仍然超限。
	var b strings.Builder
	used := 0
	for _, r := range text {
		u := 1
		if r > 0xFFFF {
			u = 2
		}
		if used+u > keep {
			break
		}
		b.WriteRune(r)
		used += u
	}
	return b.String() + mark
}

// ChunkLines 把元素贪心地切成若干片，每片不超过 maxUnits 个 UTF-16 单元（含元素之间的换行）。
//
// 两个调用方共用它，只有一处切分实现，长度账目才不会在两处慢慢跑偏：
//   - 定时流量报告（internal/server）：元素是"行"，每行一台机器，join 用 "\n"；
//   - 告警消息（本包 renderMessages）：元素是"块"，每块一条事件。
//
// 不变量：**绝不从元素中间切断**（一条机器记录、一条事件被劈成两条都没有意义）。
// 元素本身超长时它独占一片，由调用方决定怎么办（报告不允许截断名字，
// 告警用 truncateWithMark 截断并注明）。
//
// reserve 指定"末尾几个元素必须落在同一片里"（流量报告用它保住分隔线 + 合计）。
func ChunkLines(lines []string, reserve, maxUnits int) [][]string {
	if maxUnits <= 0 {
		maxUnits = alertMessageMaxUnits
	}
	if reserve < 0 || reserve > len(lines) {
		reserve = 0
	}
	head, tail := lines[:len(lines)-reserve], lines[len(lines)-reserve:]

	var chunks [][]string
	cur := make([]string, 0, len(head))
	size := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		chunks = append(chunks, cur)
		cur = make([]string, 0, len(head))
		size = 0
	}
	for _, line := range head {
		need := msgUnits(line) + 1 // +1 是连接符
		// 单片里的第一个元素不因为超长就被切走：宁可让这一片超一点，
		// 也不能把一个元素从中间切断。
		if len(cur) > 0 && size+need > maxUnits {
			flush()
		}
		cur = append(cur, line)
		size += need
	}

	tailSize := 0
	for _, line := range tail {
		tailSize += msgUnits(line) + 1
	}
	if len(cur) > 0 && size+tailSize > maxUnits {
		flush()
	}
	cur = append(cur, tail...)
	flush()
	return chunks
}
