package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 流量计费方式的三个取值必须能原样存进库、原样读回来。
//
// 为什么这一条不能只靠"看代码"：它是**新加的一列**，而列的位置在 nodeSelect /
// scanNode / INSERT / UPDATE 四处各写了一遍 —— 少写一处（比如 UPDATE 里漏掉）
// 的后果是"改了不生效"，而界面上看起来一切正常（保存成功、没有报错）。
func TestTrafficBillingRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	// 1) 什么都不填（老调用方的形状）⇒ 双向，也就是这个字段出现之前的行为。
	in := validNewNode()
	node, _, err := db.CreateNode(ctx, in, now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	if node.TrafficBilling != BillingBoth {
		t.Errorf("没填计费方式时 = %d，期望 %d（双向）", node.TrafficBilling, BillingBoth)
	}

	// 2) 三个取值各建一台，读回来必须逐字节相同。
	for _, want := range []int{BillingBoth, BillingTxOnly, BillingRxOnly} {
		in := validNewNode()
		in.Name = "billing-" + string(rune('0'+want))
		in.TrafficBilling = want
		created, _, err := db.CreateNode(ctx, in, now)
		if err != nil {
			t.Fatalf("创建计费方式 %d 的节点: %v", want, err)
		}
		got, err := db.NodeByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("读回节点: %v", err)
		}
		if got.TrafficBilling != want {
			t.Errorf("创建后 traffic_billing = %d，期望 %d", got.TrafficBilling, want)
		}

		// 3) 改口径也必须真的写进去（UPDATE 那一份 SQL 里的列）。
		got.TrafficBilling = BillingRxOnly
		if err := db.UpdateNode(ctx, got, now.Add(time.Minute)); err != nil {
			t.Fatalf("更新计费方式: %v", err)
		}
		back, err := db.NodeByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("再次读回: %v", err)
		}
		if back.TrafficBilling != BillingRxOnly {
			t.Errorf("改成只算入站后 traffic_billing = %d，期望 %d", back.TrafficBilling, BillingRxOnly)
		}

		// 4) 列表路径（ListNodes 走的是同一个 nodeSelect，但它是另一条 SQL 调用）。
		nodes, err := db.ListNodes(ctx)
		if err != nil {
			t.Fatalf("列出节点: %v", err)
		}
		found := false
		for _, n := range nodes {
			if n.ID == created.ID {
				found = true
				if n.TrafficBilling != BillingRxOnly {
					t.Errorf("列表里的 traffic_billing = %d，期望 %d", n.TrafficBilling, BillingRxOnly)
				}
			}
		}
		if !found {
			t.Fatalf("列表里找不到刚建的节点 %d", created.ID)
		}
	}
}

// 三个合法值之外的输入必须在**存储层**被拒（API 层也挡，但直连库的调用方
// ——导入脚本、将来的 CLI——不该能造出读路径不认识的口径）。
func TestTrafficBillingValidation(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	for _, bad := range []int{-1, 3, 99} {
		in := validNewNode()
		in.TrafficBilling = bad
		if _, _, err := db.CreateNode(ctx, in, now); !errors.Is(err, ErrInvalidNode) {
			t.Errorf("计费方式 %d 应当返回 ErrInvalidNode，实际 %v", bad, err)
		}
	}

	node, _, err := db.CreateNode(ctx, validNewNode(), now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	node.TrafficBilling = 7
	if err := db.UpdateNode(ctx, node, now); !errors.Is(err, ErrInvalidNode) {
		t.Errorf("更新成非法计费方式应当返回 ErrInvalidNode，实际 %v", err)
	}
}

// BilledBytes 是三个口径唯一的实现（面板 / 详情页 / 告警都读它）。
//
// 认不出的取值按双向算是**刻意**的：能走到那里的陌生值只有"有人直接改了库"
// 一种来源，而少算流量的后果是超了额度也不报警。
func TestBilledBytesByMode(t *testing.T) {
	cases := []struct {
		billing  int
		rx, tx   int64
		want     int64
		scenario string
	}{
		{BillingBoth, 400, 600, 1000, "双向 = 收 + 发"},
		{BillingTxOnly, 400, 600, 600, "只算出站 = 发"},
		{BillingRxOnly, 400, 600, 400, "只算入站 = 收"},
		{BillingBoth, 0, 0, 0, "两端都是 0"},
		{7, 400, 600, 1000, "陌生取值回落成双向"},
		{-1, 400, 600, 1000, "负数也回落成双向"},
	}
	for _, c := range cases {
		if got := BilledBytes(c.billing, c.rx, c.tx); got != c.want {
			t.Errorf("BilledBytes(%d, %d, %d) = %d，期望 %d（%s）",
				c.billing, c.rx, c.tx, got, c.want, c.scenario)
		}
	}

	// 双向那一档必须与"老代码里的 rx + tx"逐字节相同：这是"默认零影响"的判据。
	var rx, tx int64 = 1<<40 + 12345, 999
	if got := BilledBytes(BillingBoth, rx, tx); got != rx+tx {
		t.Errorf("双向口径 = %d，期望收 + 发 = %d", got, rx+tx)
	}
}
