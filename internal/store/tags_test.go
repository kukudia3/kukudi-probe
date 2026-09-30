package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 0004 是"给已发布的库加一列"的迁移，只有真的从 v3 库升级上来才算测到。
//
// 与 0002/0003 的用例同样的思路：手工造一个 v1+0002+0003 的库（user_version=3），
// 再让 Open 去补 0004 —— 直接新建的库是 0001~0004 一次跑完的，
// 盖不住"老库缺列"这条路径。
func TestMigration0004UpgradesExistingV3Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")

	raw, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatalf("打开原始数据库: %v", err)
	}
	for _, stmt := range schemaV1 {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("执行 0001 建表语句: %v", err)
		}
	}
	for _, m := range migrations[1:3] { // 0002 与 0003
		for _, stmt := range m.stmts {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("执行 %s 语句: %v", m.name, err)
			}
		}
	}
	// 老库里已经有节点：升级后必须原样还在，并且 tags 是"没有标签"而不是 NULL。
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO nodes (name, token_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"legacy-01", []byte{0x01, 0x02}, 100, 100); err != nil {
		t.Fatalf("插入老节点: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 3`); err != nil {
		t.Fatalf("设置 schema 版本: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭原始数据库: %v", err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("升级打开: %v", err)
	}
	defer func() { _ = db.Close() }()

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != len(migrations) || version < 4 {
		t.Fatalf("升级后版本 = %d，期望 %d（0004_node_tags 及以后的全部迁移）", version, len(migrations))
	}

	legacy, err := db.NodeByID(ctx, 1)
	if err != nil {
		t.Fatalf("读取老节点: %v", err)
	}
	if legacy.Tags == nil {
		t.Fatal("老节点的 tags 不该是 nil：空列表要能序列化成 []，nil 会变成 null")
	}
	if len(legacy.Tags) != 0 {
		t.Fatalf("老节点升级后应当是「没有标签」，实际 %v", legacy.Tags)
	}
	// 库里的默认值就是 '[]'（不是空串、不是 NULL）：读路径只需要处理一种写法。
	var stored string
	if err := db.Reader().QueryRowContext(ctx, `SELECT tags FROM nodes WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("读取 tags 列: %v", err)
	}
	if stored != "[]" {
		t.Fatalf("老行的 tags = %q，期望 []", stored)
	}

	// 新列能写能读（ALTER TABLE 真的生效了，而不是只改了版本号）。
	legacy.Tags = []string{"探针", "搜索"}
	if err := db.UpdateNode(ctx, legacy, time.Now()); err != nil {
		t.Fatalf("更新老节点的标签: %v", err)
	}
	got, err := db.NodeByID(ctx, 1)
	if err != nil {
		t.Fatalf("读回老节点: %v", err)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "探针" || got.Tags[1] != "搜索" {
		t.Fatalf("升级后写标签失败: %v", got.Tags)
	}
}

// 标签校验：去空白、去重、丢空串、单个长度上限、总数量上限。
//
// 长度按**字符**算：一个 16 字的纯中文标签必须合法（按字节算是 48，会被误拒）。
func TestNormalizeTags(t *testing.T) {
	repeat := func(n int, s string) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, s)
		}
		return out
	}

	cases := []struct {
		name  string
		in    []string
		want  []string
		fails bool
	}{
		{name: "去空白与去重", in: []string{" 探针 ", "搜索", "探针", "", "   ", "搜索"},
			want: []string{"探针", "搜索"}},
		{name: "保持首次出现的顺序", in: []string{"b", "a", "b", "c"}, want: []string{"b", "a", "c"}},
		{name: "空输入得到空列表", in: nil, want: []string{}},
		{name: "刚好 8 个", in: []string{"1", "2", "3", "4", "5", "6", "7", "8"},
			want: []string{"1", "2", "3", "4", "5", "6", "7", "8"}},
		{name: "9 个超量", in: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, fails: true},
		// 去重发生在计数之前：9 个同样的标签只算 1 个，不该报错。
		{name: "重复不计入数量上限", in: repeat(9, "探针"), want: []string{"探针"}},
		{name: "16 个汉字合法", in: []string{strings.Repeat("汉", maxTagLen)}, want: []string{strings.Repeat("汉", maxTagLen)}},
		{name: "17 个汉字超长", in: []string{strings.Repeat("汉", maxTagLen+1)}, fails: true},
		{name: "17 个 ASCII 超长", in: []string{strings.Repeat("x", maxTagLen+1)}, fails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTags(tc.in)
			if tc.fails {
				if !errors.Is(err, ErrInvalidNode) {
					t.Fatalf("应当返回 ErrInvalidNode，实际 %v", err)
				}
				// 提示里要说清是哪个标签：一次提交可能有 8 个，只说"某个太长"没法排查。
				if tc.in[len(tc.in)-1] != "" && !strings.Contains(err.Error(), strings.TrimSpace(tc.in[len(tc.in)-1])) {
					t.Fatalf("错误消息里应当带上出问题的标签名：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("归一化结果 = %v，期望 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("归一化结果 = %v，期望 %v", got, tc.want)
				}
			}
			// 空列表必须是非 nil 切片：nil 序列化成 null，前端就得多一条判断。
			if got == nil {
				t.Fatal("归一化结果不该是 nil")
			}
		})
	}
}

// 脏数据一律回退成空列表：读路径不能因为一行坏数据把整个节点列表打挂。
func TestDecodeTagsFallsBackToEmpty(t *testing.T) {
	dirty := []string{
		"",             // 空串（老库的 NULL 经 COALESCE 之后不该出现，但空串仍要能吞）
		"   ",          // 只有空白
		"null",         // JSON 的 null
		"not json",     // 被手工改坏
		"[1,2]",        // 非字符串元素
		`["ok",1]`,     // 混了非字符串元素
		`{"tag":"ok"}`, // 类型整个不对
		`["` + strings.Repeat("汉", maxTagLen+1) + `"]`, // 元素超长
		`["1","2","3","4","5","6","7","8","9"]`,        // 元素个数超量
	}
	for _, raw := range dirty {
		got := decodeTags(raw)
		if got == nil {
			t.Fatalf("decodeTags(%q) 返回 nil：必须是空列表", raw)
		}
		if len(got) != 0 {
			t.Fatalf("decodeTags(%q) = %v，期望空列表", raw, got)
		}
	}

	// 正常值照样读得出来，顺带把脏元素清理掉。
	got := decodeTags(`["  探针 ", "探针", "", "搜索"]`)
	if len(got) != 2 || got[0] != "探针" || got[1] != "搜索" {
		t.Fatalf("正常标签读回不对: %v", got)
	}
}

// 标签要能真的落库、真的读回来（Create 与 Update 两条路径都走一遍）。
func TestNodeTagsRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	in := validNewNode()
	in.Tags = []string{" 探针 ", "搜索", "探针", "  "}
	node, _, err := db.CreateNode(ctx, in, now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	if len(node.Tags) != 2 || node.Tags[0] != "探针" || node.Tags[1] != "搜索" {
		t.Fatalf("创建后的标签没有归一化: %v", node.Tags)
	}

	loaded, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("读回节点: %v", err)
	}
	if len(loaded.Tags) != 2 || loaded.Tags[0] != "探针" {
		t.Fatalf("库里的标签不对: %v", loaded.Tags)
	}

	list, err := db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("列出节点: %v", err)
	}
	if len(list) != 1 || len(list[0].Tags) != 2 {
		t.Fatalf("列表里的标签不对: %+v", list)
	}

	// 改标签：只动 Tags，别的字段原样带回去。
	loaded.Tags = []string{"新标签"}
	if err := db.UpdateNode(ctx, loaded, now); err != nil {
		t.Fatalf("更新标签: %v", err)
	}
	got, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("再读回节点: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "新标签" {
		t.Fatalf("更新后的标签不对: %v", got.Tags)
	}
	if got.Name != node.Name || got.GroupName != node.GroupName || got.IntervalSec != node.IntervalSec {
		t.Fatalf("改标签冲掉了别的字段: %+v", got)
	}

	// 清空标签：库里是 "[]"，读回来是空列表而不是 nil。
	got.Tags = nil
	if err := db.UpdateNode(ctx, got, now); err != nil {
		t.Fatalf("清空标签: %v", err)
	}
	empty, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("读回清空后的节点: %v", err)
	}
	if empty.Tags == nil || len(empty.Tags) != 0 {
		t.Fatalf("清空后应当是空列表，实际 %v", empty.Tags)
	}
}

// 存储层是最后一道防线：绕过 API 直接调 CreateNode/UpdateNode 也不能写进非法标签。
func TestNodeTagsValidationInStore(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	tooLong := validNewNode()
	tooLong.Tags = []string{strings.Repeat("x", maxTagLen+1)}
	if _, _, err := db.CreateNode(ctx, tooLong, now); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("超长标签应当返回 ErrInvalidNode，实际 %v", err)
	}

	tooMany := validNewNode()
	tooMany.Tags = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}
	if _, _, err := db.CreateNode(ctx, tooMany, now); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("超量标签应当返回 ErrInvalidNode，实际 %v", err)
	}

	node, _, err := db.CreateNode(ctx, validNewNode(), now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	node.Tags = []string{strings.Repeat("x", maxTagLen+1)}
	if err := db.UpdateNode(ctx, node, now); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("更新时的超长标签应当返回 ErrInvalidNode，实际 %v", err)
	}
}
