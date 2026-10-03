package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migration 是一次不可回退的结构变更，对应 PRAGMA user_version 的一次递增。
type migration struct {
	name  string
	stmts []string
}

// migrations 只前进不回退。
//
// 已发布的迁移绝不能修改（否则老部署的库会与新代码不一致）；
// 任何结构变化都追加新元素。
var migrations = []migration{
	{name: "0001_init", stmts: schemaV1},
	// 0002 只做加法：给已有部署补上价格字段。DEFAULT 0/'' 让老行天然是"没填价格"，
	// 不需要在迁移里回填，也不会让老的 PATCH 请求突然校验失败。
	{name: "0002_node_price", stmts: []string{
		`ALTER TABLE nodes ADD COLUMN price_cents INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN currency TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN billing_months INTEGER NOT NULL DEFAULT 0`,
	}},
	// 0003 只加一张新表：延迟探测（ping_samples_1m）。
	//
	// 不动 0001/0002 的任何语句 —— 老库升级时只会多出这张空表，
	// 已有数据、已有设置（包括 visible_charts）一律不受影响。
	{name: "0003_ping_samples", stmts: []string{pingSamplesDDL}},
	// 0004 给节点加「标签」列：存 JSON 字符串数组（如 ["探针","搜索"]）。
	//
	// 为什么不建关联表：标签永远跟着节点**整体**读写（一次最多 64 个，界面上就是
	// 一行徽章），既不做跨节点的标签查询，也不需要在标签改名时批量更新。一张
	// node_tags 表要多一次 join、多一套增删改逻辑，还要处理"删节点时级联"，
	// 换不来任何东西 —— 与"分组/地区是普通列"同一条取舍（见 schema.go 的说明）。
	//
	// DEFAULT '[]' 让老行天然是"没有标签"，不需要在迁移里回填，也不会让老的
	// PATCH 请求突然校验失败；NOT NULL 保证读路径永远拿到字符串而不是 NULL。
	{name: "0004_node_tags", stmts: []string{
		`ALTER TABLE nodes ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`,
	}},
	// 0005 给运行态加「最近一次进入在线状态的时刻」（Unix 秒，0 = 不在线）。
	//
	// 为什么要单独存一个时刻而不是直接存"在线了多久"：时长是**派生值**，
	// 由"现在 − 起点"算出来（见 internal/server/online.go）。存时长的话，
	// 服务端每写一次库都要重新加一遍，写库间隔一抖动数字就不准；起点则是
	// 一次状态翻转才变一次，重启后接着算即可。
	//
	// 为什么落在 node_runtime 而不是 nodes：它是"最后状态"的一部分（与 last_seen
	// 同一类），跟着每分钟的运行态落盘走，不需要额外的写入通道。
	//
	// DEFAULT 0 让老行天然是"不在线"：升级后第一个在线周期会重新起算，
	// 不需要在迁移里回填，也不会让老的启动路径读到 NULL。
	{name: "0005_node_online_since", stmts: []string{
		`ALTER TABLE node_runtime ADD COLUMN online_since INTEGER NOT NULL DEFAULT 0`,
	}},
	// 0006 把存量 traffic_limit 从"用户填的 GB × 1024³"换算成"× 10⁹"。
	//
	// 背景：流量额度输入框的标签一直写着「月流量额度（GB）」（= 10⁹ 字节），
	// 但旧代码按 GiB（1024³）存，两者差 7.37%。用户填 2000 以为买了 2000 GB，
	// 库里其实是 2147 GB —— 于是「80% 预警」要等真实用量到 86% 才响，用户可能
	// 在收到预警前就超了商家的额度。前端与告警文案现在都统一按 10⁹ 算，这一条
	// 负责把**已经入库的字节数**改回用户当初填的那个 GB 数：
	//
	//	新值 = 旧值 / 1024³ × 10⁹ = 旧值 / 1.073741824
	//
	// ROUND 是为了容忍小数 GB（旧前端本来就会 Math.round 成整 GB，但库里可能
	// 有手改或用 API 直接写进去的值）。
	//
	// ⚠️ 这条 UPDATE **依赖只执行一次**（PRAGMA user_version 把关），不能重复跑：
	// 再跑一遍会把 2000 GB 又除一次 1.073741824 变成 1863 GB。所以它是一条迁移，
	// 而不是某个每次启动都跑的"数据修正"。
	//
	// WHERE traffic_limit > 0 保住「0 = 不限」的语义：0 除出来还是 0，但显式写出来
	// 才能表明这是有意保留的，也免得将来有人给 0 加上别的含义。
	{name: "0006_traffic_limit_decimal_gb", stmts: []string{
		`UPDATE nodes SET traffic_limit = CAST(ROUND(traffic_limit / 1.073741824) AS INTEGER) WHERE traffic_limit > 0`,
	}},
	// 0007 给 ping_samples_1m 补一条按 ts 的**覆盖**索引。
	//
	// 为什么需要：这张表是 WITHOUT ROWID、主键 (node_id, target_id, ts)，而
	// QueryOverviewPing（首页每张卡片的迷你探测条）的 WHERE 只有 ts 范围、没有
	// node_id 等值条件 —— 主键那棵 B 树完全用不上，只能整表扫描再建临时 B 树分组。
	// 它是库里最大的表（节点数 × 目标数 × 分钟数 × 保留期），而这条查询**访客可打**
	// （GET /api/v1/overview 是 accessGuestRead），window/buckets 又由调用方给，
	// 极端参数（window=168h&buckets=3600）能把它变成一次全表扫描 + 大响应。
	//
	// 为什么带 node_id / target_id / avg_ms / up_cnt / all_cnt（覆盖索引）：
	// 查询要的三样（avg_ms×up_cnt 的和、up_cnt、all_cnt）与两个分组键都在索引里，
	// SQLite 不必为每一行回表。本机 691,200 行（20 节点×3 目标×8 天）实测：
	//
	//	              1h/10     1h/600    168h/3600
	//	无索引        32.8 ms   35.1 ms   468.7 ms
	//	仅 (ts)        4.1 ms    6.6 ms  1021.6 ms   ← 宽窗口比全表扫还慢（每行都要回表）
	//	本索引         2.0 ms    4.1 ms   634.7 ms   ← 纯索引扫描，不回表
	//
	// ⚠️ 覆盖索引的代价：索引里存了 6 列，体积接近主表本身（按 1.7M 行估约几十 MB），
	// 插入/删除要同步维护它（探测是每分钟每目标一行，写放大可忽略）。
	//
	// ⚠️ **宽窗口仍然变慢**（468→635 ms）：瓶颈是 7 天窗口本来就要扫过全部行，
	// 索引改变不了这件事。真正的根治是同时收敛 overviewWindowMax（接口契约变更，
	// 本轮**没有**做，见交付说明的残留风险）。窄窗口（前端实际只发 1h/10）从
	// 33 ms 掉到 2 ms，这才是这条迁移的收益所在。
	//
	// ⚠️ 这是**阻塞式**的 CREATE INDEX：老库升级时会在启动路径上等它建完
	// （实测 691,200 行 0.35 s；1.7M 行约 1 s 量级）。SQLite 没有 Postgres 那种
	// CONCURRENTLY，迁移机制里也做不到"分批建"（索引是原子的），所以只能照最朴素
	// 的写法走既有迁移事务：建索引与 user_version 同事务提交，中断则整体回滚、
	// 下次启动重来。
	{name: "0007_ping_samples_ts_index", stmts: []string{
		`CREATE INDEX idx_ping_samples_1m_ts ON ping_samples_1m(ts, node_id, target_id, avg_ms, up_cnt, all_cnt)`,
	}},
}

func migrate(ctx context.Context, db *sql.DB) error {
	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	// 下界必须先查：PRAGMA user_version 是头页里的**有符号** 4 字节整数，
	// 手工改库、头页损坏、或把别的软件的 SQLite 文件当成本程序的库时它可以是负数。
	// 负数会让下面的 migrations[current] 直接越界 panic（进程带栈退出、systemd
	// 反复重启，日志里没有一句"库不对"），所以在这里把它变成一条能读懂的报错。
	if current < 0 {
		return fmt.Errorf("数据库 schema 版本为 %d（负数）：文件可能已损坏或被别的工具改过，拒绝启动", current)
	}
	if current > len(migrations) {
		return fmt.Errorf("数据库 schema 版本为 %d，高于本程序支持的 %d：请升级 probe-server，不要用旧版本打开新库", current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		m := migrations[i]
		if err := applyMigration(ctx, db, m, i+1); err != nil {
			return fmt.Errorf("迁移 %s 失败: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // 已提交时返回 ErrTxDone，忽略

	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w（出错语句：%.80s）", err, stmt)
		}
	}
	// version 是本包内的常量，不存在注入风险。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	return v, nil
}
