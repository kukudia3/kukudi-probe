package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func validNewNode() NewNode {
	return NewNode{
		Name:           "hk-01",
		GroupName:      "香港",
		Region:         "HK",
		Note:           "测试节点",
		IntervalSec:    1,
		TrafficWarnPct: 80,
		ResetDay:       19,
		SortOrder:      1,
	}
}

func TestCreateNodeAndTokenLookup(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	node, token, err := db.CreateNode(ctx, validNewNode(), now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) || len(token) < 40 {
		t.Fatalf("Token 形态不对: %q", token)
	}
	if node.ID <= 0 || node.Name != "hk-01" || !node.Enabled {
		t.Fatalf("节点内容不对: %+v", node)
	}
	if node.TokenPrefix != token[:8] {
		t.Fatalf("展示用前缀 = %q，期望 %q", node.TokenPrefix, token[:8])
	}

	// 用 Token 反查节点（Agent 鉴权路径）。
	got, err := db.NodeByTokenHash(ctx, HashToken(token))
	if err != nil {
		t.Fatalf("按 Token 查节点: %v", err)
	}
	if got.ID != node.ID || got.IntervalSec != 1 || got.ResetDay != 19 {
		t.Fatalf("查回的节点不一致: %+v", got)
	}

	// 明文绝不能出现在库里。
	var stored []byte
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT token_hash FROM nodes WHERE id = ?`, node.ID).Scan(&stored); err != nil {
		t.Fatalf("读取 token_hash: %v", err)
	}
	if string(stored) == token {
		t.Fatal("数据库里存了 Token 明文")
	}
	if len(stored) != 32 {
		t.Fatalf("token_hash 长度 = %d，期望 32（SHA-256）", len(stored))
	}

	if _, err := db.NodeByTokenHash(ctx, HashToken("pba_wrong_token")); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("错误 Token 应当返回 ErrNodeNotFound，实际 %v", err)
	}
}

func TestCreateNodeRejectsBadInput(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	cases := []struct {
		name   string
		mutate func(*NewNode)
	}{
		{"名称为空", func(n *NewNode) { n.Name = "  " }},
		{"名称过长", func(n *NewNode) { n.Name = strings.Repeat("x", maxNodeNameLen+1) }},
		{"备注过长", func(n *NewNode) { n.Note = strings.Repeat("x", maxNoteLen+1) }},
		{"间隔为 0", func(n *NewNode) { n.IntervalSec = 0 }},
		{"间隔过大", func(n *NewNode) { n.IntervalSec = 301 }},
		{"告警阈值越界", func(n *NewNode) { n.TrafficWarnPct = 101 }},
		{"重置日越界", func(n *NewNode) { n.ResetDay = 32 }},
		{"流量额度为负", func(n *NewNode) { n.TrafficLimit = -1 }},
		{"到期时间为负", func(n *NewNode) { n.ExpiresAt = -1 }},
		{"网卡名过长", func(n *NewNode) { n.Iface = strings.Repeat("e", maxNodeIfaceLen+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validNewNode()
			tc.mutate(&in)
			if _, _, err := db.CreateNode(ctx, in, now); !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("应当返回 ErrInvalidNode，实际 %v", err)
			}
		})
	}
}

// 价格三件套的校验：负值、超限，以及"填了价格没填周期"这类半残状态。
//
// 这些规则必须在存储层也成立：API 是给人用的，存储层是给"将来的 CLI / 导入脚本"
// 用的最后一道防线，绕过 API 直接写库的调用方不该能造出算不出月均的节点。
func TestNodePriceValidation(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	cases := []struct {
		name   string
		mutate func(*NewNode)
	}{
		{"价格为负", func(n *NewNode) { n.PriceCents = -1 }},
		{"价格超上限", func(n *NewNode) { n.PriceCents = maxPriceCents + 1; n.Currency = "CNY"; n.BillingMonths = 12 }},
		{"填了价格没填周期", func(n *NewNode) { n.PriceCents = 7121; n.Currency = "CNY" }},
		{"填了周期没填价格", func(n *NewNode) { n.BillingMonths = 12 }},
		{"没周期却填了货币", func(n *NewNode) { n.Currency = "CNY" }},
		{"货币小写", func(n *NewNode) { n.PriceCents = 7121; n.Currency = "cny"; n.BillingMonths = 12 }},
		{"货币含数字", func(n *NewNode) { n.PriceCents = 7121; n.Currency = "CN1"; n.BillingMonths = 12 }},
		{"货币过长", func(n *NewNode) { n.PriceCents = 7121; n.Currency = "ABCDEFGHI"; n.BillingMonths = 12 }},
		{"周期超过上限", func(n *NewNode) { n.PriceCents = 7121; n.Currency = "CNY"; n.BillingMonths = maxBillingMonths + 1 }},
		{"周期为负", func(n *NewNode) { n.BillingMonths = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validNewNode()
			tc.mutate(&in)
			if _, _, err := db.CreateNode(ctx, in, now); !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("应当返回 ErrInvalidNode，实际 %v", err)
			}
		})
	}

	// 更新路径同样要校验：否则"先建一个合法节点，再改成半残状态"就能绕过去。
	base := validNewNode()
	base.PriceCents = 7121
	base.Currency = "CNY"
	base.BillingMonths = 12
	node, _, err := db.CreateNode(ctx, base, now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	broken := node
	broken.BillingMonths = 0 // 有价格没周期
	if err := db.UpdateNode(ctx, broken, now); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("UpdateNode 应当返回 ErrInvalidNode，实际 %v", err)
	}

	// 边界值必须合法：正好到上限、货币正好 8 位、周期正好 120。
	edge := validNewNode()
	edge.Name = "price-edge"
	edge.PriceCents = maxPriceCents
	edge.Currency = "ABCDEFGH"
	edge.BillingMonths = maxBillingMonths
	if _, _, err := db.CreateNode(ctx, edge, now); err != nil {
		t.Fatalf("边界值应当合法: %v", err)
	}

	// 货币留空但价格与周期齐全也是合法的（有人就是不想标币种）。
	noCurrency := validNewNode()
	noCurrency.Name = "price-no-currency"
	noCurrency.PriceCents = 100
	noCurrency.BillingMonths = 1
	if _, _, err := db.CreateNode(ctx, noCurrency, now); err != nil {
		t.Fatalf("不填货币应当合法: %v", err)
	}
}

// 价格字段的读写往返：创建、列表、更新、清空。
func TestNodePriceRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	in := validNewNode()
	in.Name = "price-01"
	in.PriceCents = 7121
	in.Currency = "CNY"
	in.BillingMonths = 12
	node, _, err := db.CreateNode(ctx, in, now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	if node.PriceCents != 7121 || node.Currency != "CNY" || node.BillingMonths != 12 {
		t.Fatalf("价格字段没有落库: %+v", node)
	}

	// 列表接口读的是 ListNodes，扫描列必须一起改（少一列就会整条查询报错）。
	nodes, err := db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("列出节点: %v", err)
	}
	if len(nodes) != 1 || nodes[0].PriceCents != 7121 || nodes[0].Currency != "CNY" || nodes[0].BillingMonths != 12 {
		t.Fatalf("列表里的价格字段不对: %+v", nodes)
	}

	// 改价格
	node.PriceCents = 3600
	node.Currency = "USD"
	node.BillingMonths = 1
	if err := db.UpdateNode(ctx, node, now.Add(time.Minute)); err != nil {
		t.Fatalf("更新节点: %v", err)
	}
	got, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("读回节点: %v", err)
	}
	if got.PriceCents != 3600 || got.Currency != "USD" || got.BillingMonths != 1 {
		t.Fatalf("价格更新未生效: %+v", got)
	}

	// 清空（改成"没填价格"）也要能存
	got.PriceCents = 0
	got.Currency = ""
	got.BillingMonths = 0
	if err := db.UpdateNode(ctx, got, now); err != nil {
		t.Fatalf("清空价格: %v", err)
	}
	back, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("读回节点: %v", err)
	}
	if back.PriceCents != 0 || back.Currency != "" || back.BillingMonths != 0 {
		t.Fatalf("清空后仍然有价格: %+v", back)
	}

	// 不填价格的节点默认就是 0/空（老库里的行升级后也是这样）。
	plain := validNewNode()
	plain.Name = "price-none"
	created, _, err := db.CreateNode(ctx, plain, now)
	if err != nil {
		t.Fatalf("创建无价格节点: %v", err)
	}
	if created.PriceCents != 0 || created.Currency != "" || created.BillingMonths != 0 {
		t.Fatalf("没填价格时不应当是别的值: %+v", created)
	}
}

func TestCreateNodeRejectsDuplicateName(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	if _, _, err := db.CreateNode(ctx, validNewNode(), now); err != nil {
		t.Fatalf("第一次创建: %v", err)
	}
	if _, _, err := db.CreateNode(ctx, validNewNode(), now); !errors.Is(err, ErrNodeNameTaken) {
		t.Fatalf("重名应当返回 ErrNodeNameTaken，实际 %v", err)
	}
}

func TestRotateNodeTokenInvalidatesOld(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	node, oldToken, err := db.CreateNode(ctx, validNewNode(), now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	rotated, newToken, err := db.RotateNodeToken(ctx, node.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("重新生成 Token: %v", err)
	}
	if newToken == oldToken {
		t.Fatal("重新生成的 Token 不应当与旧 Token 相同")
	}
	if rotated.TokenCreatedAt <= node.TokenCreatedAt {
		t.Fatal("Token 创建时间未更新")
	}

	if _, err := db.NodeByTokenHash(ctx, HashToken(oldToken)); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("旧 Token 必须立即失效，实际 %v", err)
	}
	got, err := db.NodeByTokenHash(ctx, HashToken(newToken))
	if err != nil || got.ID != node.ID {
		t.Fatalf("新 Token 应当可用: node=%+v err=%v", got, err)
	}

	if _, _, err := db.RotateNodeToken(ctx, 9999, now); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("不存在的节点应当返回 ErrNodeNotFound，实际 %v", err)
	}
}

func TestUpdateNode(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	node, _, err := db.CreateNode(ctx, validNewNode(), now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	node.Name = "hk-02"
	node.GroupName = "香港-2"
	node.IntervalSec = 5
	node.TrafficLimit = 1 << 40
	node.Enabled = false
	if err := db.UpdateNode(ctx, node, now.Add(time.Minute)); err != nil {
		t.Fatalf("更新节点: %v", err)
	}

	got, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("读回节点: %v", err)
	}
	if got.Name != "hk-02" || got.GroupName != "香港-2" || got.IntervalSec != 5 || got.TrafficLimit != 1<<40 || got.Enabled {
		t.Fatalf("更新未生效: %+v", got)
	}

	// 无效参数
	bad := got
	bad.IntervalSec = 0
	if err := db.UpdateNode(ctx, bad, now); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("无效参数应当返回 ErrInvalidNode，实际 %v", err)
	}

	// 不存在的节点
	ghost := got
	ghost.ID = 9999
	if err := db.UpdateNode(ctx, ghost, now); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("不存在的节点应当返回 ErrNodeNotFound，实际 %v", err)
	}

	// 重名冲突
	if _, _, err := db.CreateNode(ctx, NewNode{
		Name: "other", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, now); err != nil {
		t.Fatalf("创建第二个节点: %v", err)
	}
	conflict := got
	conflict.Name = "other"
	if err := db.UpdateNode(ctx, conflict, now); !errors.Is(err, ErrNodeNameTaken) {
		t.Fatalf("重名应当返回 ErrNodeNameTaken，实际 %v", err)
	}
}

func TestListNodesOrderAndDelete(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	ids := make([]int64, 0, 3)
	for i, name := range []string{"c", "a", "b"} {
		in := validNewNode()
		in.Name = name
		in.SortOrder = 2 - i
		node, _, err := db.CreateNode(ctx, in, now)
		if err != nil {
			t.Fatalf("创建 %s: %v", name, err)
		}
		ids = append(ids, node.ID)
	}

	nodes, err := db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("列出节点: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("节点数 = %d", len(nodes))
	}
	// 创建顺序是 c(2) / a(1) / b(0)，因此按 sort_order 应当是 b / a / c。
	if nodes[0].Name != "b" || nodes[1].Name != "a" || nodes[2].Name != "c" {
		t.Fatalf("排序不符合 sort_order: %+v", nodes)
	}

	if err := db.DeleteNode(ctx, ids[0]); err != nil {
		t.Fatalf("删除节点: %v", err)
	}
	if err := db.DeleteNode(ctx, ids[0]); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("重复删除应当返回 ErrNodeNotFound，实际 %v", err)
	}
	left, err := db.ListNodes(ctx)
	if err != nil || len(left) != 2 {
		t.Fatalf("删除后节点数 = %d, err = %v", len(left), err)
	}
}

func TestDeleteNodeRemovesHistory(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	node, _, err := db.CreateNode(ctx, validNewNode(), now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	nodeID := node.ID

	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO samples_10s (node_id, ts, cpu_avg) VALUES (?, ?, ?)`, nodeID, 1_700_000_000, 1.0); err != nil {
		t.Fatalf("插入 10s 桶: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO samples_1m (node_id, ts, cpu_avg) VALUES (?, ?, ?)`, nodeID, 1_700_000_000, 1.0); err != nil {
		t.Fatalf("插入 1m 桶: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)`, nodeID, "2026-09-29", 100, 200); err != nil {
		t.Fatalf("插入日流量: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO alert_state (node_id, rule, state, since) VALUES (?, ?, ?, ?)`, nodeID, "offline", "firing", 1); err != nil {
		t.Fatalf("插入告警状态: %v", err)
	}

	if err := db.DeleteNode(ctx, nodeID); err != nil {
		t.Fatalf("删除节点: %v", err)
	}

	for _, tc := range []struct {
		table string
		query string
	}{
		{"samples_10s", `SELECT count(*) FROM samples_10s WHERE node_id = ?`},
		{"samples_1m", `SELECT count(*) FROM samples_1m WHERE node_id = ?`},
		{"traffic_daily", `SELECT count(*) FROM traffic_daily WHERE node_id = ?`},
		{"alert_state", `SELECT count(*) FROM alert_state WHERE node_id = ?`},
		{"node_runtime", `SELECT count(*) FROM node_runtime WHERE node_id = ?`},
	} {
		var n int
		if err := db.Reader().QueryRowContext(ctx, tc.query, nodeID).Scan(&n); err != nil {
			t.Fatalf("查询 %s: %v", tc.table, err)
		}
		if n != 0 {
			t.Errorf("删除节点后 %s 仍有 %d 行", tc.table, n)
		}
	}
}

func TestAppendAuditKeepsBounded(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	// 一次性塞进 2100 行，然后调用一次 AppendAudit 触发清理。
	if _, err := db.Writer().ExecContext(ctx, `
		INSERT INTO audit_log (ts, action)
		SELECT 1, 'bulk' FROM (
			WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 2100)
			SELECT i FROM c
		)`); err != nil {
		t.Fatalf("批量插入: %v", err)
	}
	if err := db.AppendAudit(ctx, "node_delete", 1, "127.0.0.1", "删除节点"); err != nil {
		t.Fatalf("写入审计: %v", err)
	}

	var n int
	if err := db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("统计审计: %v", err)
	}
	if n > 2000 {
		t.Fatalf("审计表行数 = %d，应当保持在 2000 以内", n)
	}

	var action string
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT action FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil {
		t.Fatalf("读取最后一条审计: %v", err)
	}
	if action != "node_delete" {
		t.Fatalf("最后一条审计 = %q", action)
	}
}
