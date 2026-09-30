package store

// schemaV1 是 v1 的完整表结构（docs/DESIGN.md §6）。
//
// 统一使用 STRICT 表：所有外部输入在写入前都已校验，STRICT 让 SQLite 再挡一层类型错误
// （例如把字符串写进 INTEGER 列会直接报错，而不是被静默转换）。
// 表能少则少：分组/地区是普通列，不做关联表；单管理员放在 settings 里，不做 users 表。
var schemaV1 = []string{
	`CREATE TABLE settings (
		key        TEXT    PRIMARY KEY,
		value      TEXT    NOT NULL,
		updated_at INTEGER NOT NULL
	) STRICT`,

	`CREATE TABLE nodes (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		name             TEXT    NOT NULL,
		group_name       TEXT    NOT NULL DEFAULT '',
		region           TEXT    NOT NULL DEFAULT '',
		note             TEXT    NOT NULL DEFAULT '',
		token_hash       BLOB    NOT NULL,
		token_prefix     TEXT    NOT NULL DEFAULT '',
		token_created_at INTEGER NOT NULL DEFAULT 0,
		iface            TEXT    NOT NULL DEFAULT '',
		interval_sec     INTEGER NOT NULL DEFAULT 1,
		traffic_limit    INTEGER NOT NULL DEFAULT 0,
		traffic_warn_pct INTEGER NOT NULL DEFAULT 80,
		reset_day        INTEGER NOT NULL DEFAULT 1,
		expires_at       INTEGER NOT NULL DEFAULT 0,
		sort_order       INTEGER NOT NULL DEFAULT 0,
		enabled          INTEGER NOT NULL DEFAULT 1,
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	) STRICT`,
	`CREATE UNIQUE INDEX idx_nodes_name ON nodes(name)`,
	`CREATE UNIQUE INDEX idx_nodes_token ON nodes(token_hash)`,

	`CREATE TABLE node_runtime (
		node_id       INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
		last_seen     INTEGER NOT NULL DEFAULT 0,
		status        TEXT    NOT NULL DEFAULT 'unknown',
		cpu_pct       REAL    NOT NULL DEFAULT 0,
		mem_pct       REAL    NOT NULL DEFAULT 0,
		swap_pct      REAL    NOT NULL DEFAULT 0,
		disk_pct      REAL    NOT NULL DEFAULT 0,
		load1         REAL    NOT NULL DEFAULT 0,
		lat_ms        REAL    NOT NULL DEFAULT 0,
		uptime_sec    INTEGER NOT NULL DEFAULT 0,
		boot_id       TEXT    NOT NULL DEFAULT '',
		iface         TEXT    NOT NULL DEFAULT '',
		rx_total      INTEGER NOT NULL DEFAULT 0,
		tx_total      INTEGER NOT NULL DEFAULT 0,
		rx_raw        INTEGER NOT NULL DEFAULT 0,
		tx_raw        INTEGER NOT NULL DEFAULT 0,
		agent_version TEXT    NOT NULL DEFAULT '',
		kernel        TEXT    NOT NULL DEFAULT '',
		os_name       TEXT    NOT NULL DEFAULT '',
		cpu_model     TEXT    NOT NULL DEFAULT '',
		updated_at    INTEGER NOT NULL DEFAULT 0
	) STRICT`,

	`CREATE TABLE traffic_daily (
		node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		day     TEXT    NOT NULL,
		rx      INTEGER NOT NULL DEFAULT 0,
		tx      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (node_id, day)
	) STRICT, WITHOUT ROWID`,

	samplesDDL("samples_10s"),
	samplesDDL("samples_1m"),

	`CREATE TABLE alert_state (
		node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		rule        TEXT    NOT NULL,
		state       TEXT    NOT NULL,
		since       INTEGER NOT NULL,
		last_notify INTEGER NOT NULL DEFAULT 0,
		notify_cnt  INTEGER NOT NULL DEFAULT 0,
		context     TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (node_id, rule)
	) STRICT, WITHOUT ROWID`,

	`CREATE TABLE sessions (
		token_hash   BLOB    PRIMARY KEY,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		ip           TEXT    NOT NULL DEFAULT '',
		ua           TEXT    NOT NULL DEFAULT ''
	) STRICT`,
	`CREATE INDEX idx_sessions_expires ON sessions(expires_at)`,

	`CREATE TABLE audit_log (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		ts      INTEGER NOT NULL,
		action  TEXT    NOT NULL,
		node_id INTEGER NOT NULL DEFAULT 0,
		ip      TEXT    NOT NULL DEFAULT '',
		detail  TEXT    NOT NULL DEFAULT ''
	) STRICT`,
}

// samplesDDL 生成两级历史桶的表结构：10s 与 1m 只差表名。
//
// 每个桶保存 avg/min/max，所以降采样不会抹平短时间的高负载；
// up_cnt/all_cnt 让任意范围的可用率直接从同一张表算出来。
func samplesDDL(table string) string {
	return `CREATE TABLE ` + table + ` (
		node_id  INTEGER NOT NULL,
		ts       INTEGER NOT NULL,
		cpu_avg  REAL    NOT NULL DEFAULT 0,
		cpu_max  REAL    NOT NULL DEFAULT 0,
		mem_avg  REAL    NOT NULL DEFAULT 0,
		mem_max  REAL    NOT NULL DEFAULT 0,
		swap_avg REAL    NOT NULL DEFAULT 0,
		disk_avg REAL    NOT NULL DEFAULT 0,
		disk_max REAL    NOT NULL DEFAULT 0,
		load1_avg REAL   NOT NULL DEFAULT 0,
		rx_rate  REAL    NOT NULL DEFAULT 0,
		rx_max   REAL    NOT NULL DEFAULT 0,
		tx_rate  REAL    NOT NULL DEFAULT 0,
		tx_max   REAL    NOT NULL DEFAULT 0,
		lat_avg  REAL    NOT NULL DEFAULT 0,
		lat_min  REAL    NOT NULL DEFAULT 0,
		lat_max  REAL    NOT NULL DEFAULT 0,
		up_cnt   INTEGER NOT NULL DEFAULT 0,
		all_cnt  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (node_id, ts)
	) STRICT, WITHOUT ROWID`
}

// pingSamplesDDL 是延迟探测的历史桶（迁移 0003 追加）。
//
// 只有 1 分钟一级粒度：探测默认 60 秒一次，若照抄 samples 的 10 秒桶，
// 每个目标每分钟都会得到"6 个空桶 + 1 个有值"，纯属浪费；
// 更长的时间档位由查询侧再聚合（见 PingRange），不需要更细的源数据。
//
// 没有外键（与 samples_* 一致）：写入频率虽低，但外键检查对每分钟每目标一行来说
// 也没必要，节点删除时的清理由 DeleteNode 显式完成。
const pingSamplesDDL = `CREATE TABLE ping_samples_1m (
	node_id   INTEGER NOT NULL,
	target_id INTEGER NOT NULL,
	ts        INTEGER NOT NULL,
	avg_ms    REAL    NOT NULL DEFAULT 0,
	min_ms    REAL    NOT NULL DEFAULT 0,
	max_ms    REAL    NOT NULL DEFAULT 0,
	loss_pct  REAL    NOT NULL DEFAULT 0,
	up_cnt    INTEGER NOT NULL DEFAULT 0,
	all_cnt   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (node_id, target_id, ts)
) STRICT, WITHOUT ROWID`
