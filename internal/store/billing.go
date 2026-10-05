package store

// 流量计费方式（nodes.traffic_billing，迁移 0008）。
//
// 它回答的是**一个**问题：额度按哪个口径算？
//
//	双向   —— 收 + 发（加这个字段之前的行为）
//	只算出站 —— 只算发
//	只算入站 —— 只算收
//
// 为什么用整数而不是字符串：与 reset_day / billing_months 同一条取舍 ——
// 它是三个固定取值里的一个，不是一个可自由填写的词。整数在库里定长、
// 在索引/比较上不需要排序规则，而且前端的 <select> 与 JSON 都能直接装下。
//
// ⚠️ 0 必须是"双向"：老库（没有这一列）升级后 DEFAULT 0，于是行为与升级前
// 逐字节相同。这条约束决定了编号，不能为了好看把 0 挪作它用。
const (
	BillingBoth   = 0 // 双向：收 + 发（默认）
	BillingTxOnly = 1 // 只算出站：发
	BillingRxOnly = 2 // 只算入站：收
)

// BilledBytes 返回"按计费口径应计入额度的那部分字节"。
//
// 认不出的取值一律按**双向**算：接口层（NewNode.Validate）已经挡掉了三个合法值
// 之外的输入，能走到这里的陌生值只有一种来源 —— 有人直接改了库。那种情况下
// "按老口径算"是唯一不会凭空少算流量的选择（少算的后果是用户超了额度过不了
// 预警，而多算只会让预警早一点响）。
//
// 只在这里实现一次：面板的卡片、详情页的百分比、告警引擎三处读的都是同一个数，
// 各写一遍的话总有一处会漏掉新档位。
func BilledBytes(billing int, rx, tx int64) int64 {
	switch billing {
	case BillingTxOnly:
		return tx
	case BillingRxOnly:
		return rx
	default:
		return rx + tx
	}
}
