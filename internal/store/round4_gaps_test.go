package store

// 第四轮「缺口补齐」用例：第二轮安全审计 SECURITY-AUDIT-ROUND2.md §7 区域 04 里
// 被判「本机可补、但当时没做」的三条（逐条编号见 _audit/ROUND3-VERIFY-1.md §3.4）：
//
//	04-2 `traffic_daily.rx + excluded.rx` 在极端增量下的溢出行为
//	04-3 两个进程同时打开同一个 probe.db 并同时迁移
//	04-4 `journal_size_limit` / `auto_vacuum` / `secure_delete` 的当前取值，
//	     以及「删掉的行仍然留在主库 / -wal 里」这件事本身
//
// 本文件只加测试：产品代码一行未改（internal/store 下的非 _test.go 文件逐字节未动）。
// 结论与反向验证记录在 D:\DEEPSEEK\_audit\ROUND4-GAPS.md。

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 04-2：traffic_daily 的累加上界
// ---------------------------------------------------------------------------

// TestTrafficDailySumIsExactUpToInt64AndBreaksAtTheBoundary 覆盖 04-2。
//
// 审计问的是 `rx = rx + excluded.rx`（traffic.go 的 upsert）在极端增量下的行为，
// 当时写的是「需约 2²³ 帧 / 接近 deltaMax(1 TiB) 的增量，未构造场景」。这里分两段回答：
//
//	A. **上界以内是精确的**：单个增量已被服务端 tracker 夹在 deltaMax（默认 1 TiB，
//	   internal/server/traffic.go 的 `rx-nt.lastRx > uint64(t.deltaMax)`），
//	   所以累加只可能被「帧数」推过 int64 上界 —— 本用例把这段算术钉住（≥ 2²³ 帧）；
//	   在界内，64 帧 × 1 TiB 必须逐字节读回 64 TiB。
//	B. **跨过界那一步**：直接注入两次 2⁶²（等价于越界之后的下一帧）。
//	   **实测行为（两段都断言）**：第一次写进去、读回精确的 2⁶²；第二次的加法结果
//	   越出 int64，`modernc.org/sqlite` 拒绝把 REAL 存进 INTEGER 列 —— `FlushTraffic`
//	   **报错**（`constraint failed: cannot store REAL value in INTEGER column
//	   traffic_daily.rx (3091)`），整个事务回滚 ⇒ 之前的 2⁶² 一个字节没变。
//	   也就是说**不是静默回绕/静默精度丢失，而是这台节点自己的流量写入开始报错**
//	   （代价见 ROUND4-GAPS.md：该节点的日流量会一直写不进去，直到人为清掉那一行）。
//
// 为什么能这样"抄近路"而不失一般性：SQLite 的整数加法只关心两个操作数的值与
// 上界，不关心这 2⁶² 是"一帧"还是"2²² 帧 × 1 TiB"累出来的；帧数那一段用算术断言
// 单独钉住（下面的 framesToOverflow）。
//
// 反向验证（已实测，见报告）：把 traffic.go 的 upsert 从 `rx = rx + excluded.rx`
// 改成 `rx = excluded.rx`（覆盖而不是累加）→ 本用例 A 段红在"64 帧之后应当累加到 64 TiB"。
func TestTrafficDailySumIsExactUpToInt64AndBreaksAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()
	const day = "2026-10-03"

	mkNode := func(name string) Node {
		t.Helper()
		node, _, err := db.CreateNode(ctx, testNewNode(name), now)
		if err != nil {
			t.Fatalf("创建节点 %s: %v", name, err)
		}
		return node
	}
	rxOf := func(nodeID int64) (int64, error) {
		t.Helper()
		rows, err := db.TrafficDailySince(ctx, day)
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			if r.NodeID == nodeID {
				return r.Rx, nil
			}
		}
		return 0, fmt.Errorf("traffic_daily 里没有节点 %d 的行", nodeID)
	}

	// ---- A. 界内：64 帧 × deltaMax(1 TiB) ----
	const deltaMax = int64(1) << 40 // 1 TiB，与 config.Default().TrafficDeltaMax 同值
	const framesA = 64
	exact := mkNode("traffic-exact")
	batch := make([]TrafficUpdate, 0, framesA)
	for i := 0; i < framesA; i++ {
		batch = append(batch, TrafficUpdate{NodeID: exact.ID, Day: day, RxDelta: deltaMax, TxDelta: 0})
	}
	if err := db.FlushTraffic(ctx, batch, now); err != nil {
		t.Fatalf("FlushTraffic（界内 64 帧）: %v", err)
	}
	got, err := rxOf(exact.ID)
	if err != nil {
		t.Fatalf("读回界内的累加值: %v", err)
	}
	if want := deltaMax * framesA; got != want {
		t.Errorf("界内累加不精确：%d 帧 × 1 TiB 读回 %d，期望 %d", framesA, got, want)
	}

	// 算术：每帧最多 deltaMax，要到 int64 上界必须 ≥ 2²³ 帧。
	const framesToOverflow = int64(1) << 23
	if framesToOverflow <= math.MaxInt64/deltaMax {
		t.Fatalf("算术前提不成立：2²³ 帧 × 1 TiB 应当越过 int64 上界，但 MaxInt64/1TiB = %d",
			math.MaxInt64/deltaMax)
	}
	t.Logf("界内精确；按 tracker 的夹取上限，单个节点要 ≥ %d 帧 × 1 TiB 才会越过 int64 上界", framesToOverflow)

	// ---- B. 跨界：两次 2⁶² ----
	over := mkNode("traffic-overflow")
	const half = int64(1) << 62
	if err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: over.ID, Day: day, RxDelta: half, TxDelta: 0},
	}, now); err != nil {
		t.Fatalf("FlushTraffic（跨界第 1 次，应当成功）: %v", err)
	}
	if got, err := rxOf(over.ID); err != nil {
		t.Fatalf("读回 2⁶²: %v", err)
	} else if got != half {
		t.Fatalf("第一次写入后读回 %d，期望 %d", got, half)
	}

	// 第二次加法越出 int64：必须**报错**，而不是静默回绕成负数或静默丢精度。
	err = db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: over.ID, Day: day, RxDelta: half, TxDelta: 0},
	}, now)
	if err == nil {
		t.Fatalf("两次 2⁶² 的加法越出 int64 上界，FlushTraffic 却成功了 —— " +
			"要么发生了静默回绕/静默精度丢失，要么这里已经不需要 int64")
	}
	t.Logf("跨界时 FlushTraffic 报错（实测）：%v", err)
	if !strings.Contains(err.Error(), "REAL") && !strings.Contains(err.Error(), "INTEGER") {
		t.Errorf("报错原因不是「整数列存不下溢出的 REAL」：%v", err)
	}
	// 失败的那次不许留下半个事务：之前写进去的 2⁶² 必须一字不变。
	if got, err := rxOf(over.ID); err != nil {
		t.Fatalf("被拒之后重新读回: %v", err)
	} else if got != half {
		t.Errorf("被拒的那次污染了已有数据：读回 %d，期望仍然是 %d", got, half)
	}
}

// ---------------------------------------------------------------------------
// 04-3：两个真进程同时打开同一个库并同时迁移
// ---------------------------------------------------------------------------

// openHelperEnv 是"重新执行本测试二进制当子进程"的开关（Go 官方的 helper-process 惯例）。
const openHelperEnv = "PROBE_STORE_OPEN_HELPER"

// TestStoreOpenHelperProcess 不是一条真正的用例，而是下面那条用例 fork 出来的子进程入口。
// 父进程不设 openHelperEnv 时它立刻返回（不消耗任何时间，也不做任何断言）。
func TestStoreOpenHelperProcess(t *testing.T) {
	if os.Getenv(openHelperEnv) != "1" {
		return
	}
	path := os.Getenv("PROBE_STORE_OPEN_PATH")
	// 所有子进程等到同一个绝对时刻再开库：这才是"同时迁移"。
	if raw := os.Getenv("PROBE_STORE_OPEN_AT"); raw != "" {
		var at int64
		if _, err := fmt.Sscanf(raw, "%d", &at); err == nil {
			if d := time.Until(time.Unix(0, at)); d > 0 {
				time.Sleep(d)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Open(ctx, path)
	if err != nil {
		// 打印成结构化一行，父进程按它判定"失败得干不干净"。
		fmt.Printf("OPEN_ERR: %v\n", err)
		t.Fatalf("Open: %v", err)
	}
	version, err := db.SchemaVersion(ctx)
	if err != nil {
		fmt.Printf("OPEN_ERR: SchemaVersion: %v\n", err)
		_ = db.Close()
		t.Fatalf("SchemaVersion: %v", err)
	}
	if err := db.Close(); err != nil {
		fmt.Printf("OPEN_ERR: Close: %v\n", err)
		t.Fatalf("Close: %v", err)
	}
	fmt.Printf("OPEN_OK: schema=%d\n", version)
}

// TestConcurrentProcessesOpenAndMigrateSameDatabase 覆盖 04-3。
//
// 审计的原话是「两个进程同时打开同一 probe.db 并同时迁移（多实例/容器重复启动）：
// **未实测**；按 `_txlock=immediate` + `busy_timeout(5000)` 推断是"其中一个失败退出"」。
//
// 这里用**真进程**（不是两个 goroutine）：SQLite 的锁语义在跨进程与同进程下不完全一样，
// 而"容器重复启动"本身就是跨进程。断言的是**收敛结果**，不是"谁赢"：
//
//	① 每个子进程要么成功、要么干净地报错 —— 不许 panic / 不许栈退出；
//	② 至少一个成功（否则等于"谁也起不来"，那是另一回事）；
//	③ 风暴之后库是完好的：能重新 Open、schema 版本 = len(migrations)、表齐。
//
// 反向验证（已实测，见报告）：把 store.go 的 `_txlock=immediate` 去掉 → 子进程里
// 出现 `database is locked (5) (SQLITE_BUSY)` 且**没有任何进程成功**，②红。
func TestConcurrentProcessesOpenAndMigrateSameDatabase(t *testing.T) {
	const procs = 4
	path := filepath.Join(t.TempDir(), "probe.db")

	// 4 个进程在同一个绝对时刻开库。给 300ms 让它们都起来。
	at := time.Now().Add(500 * time.Millisecond).UnixNano()

	type result struct {
		out    string
		err    error
		panics bool
	}
	results := make([]result, procs)
	var wg sync.WaitGroup
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestStoreOpenHelperProcess$", "-test.v=false")
			cmd.Env = append(os.Environ(),
				openHelperEnv+"=1",
				"PROBE_STORE_OPEN_PATH="+path,
				fmt.Sprintf("PROBE_STORE_OPEN_AT=%d", at),
			)
			out, err := cmd.CombinedOutput()
			results[i] = result{
				out:    string(out),
				err:    err,
				panics: bytes.Contains(out, []byte("panic:")),
			}
		}(i)
	}
	wg.Wait()

	ok, failed := 0, 0
	for i, r := range results {
		switch {
		case r.panics:
			t.Errorf("子进程 %d panic 了（不是干净报错）：\n%s", i, r.out)
		case r.err == nil:
			ok++
			if !strings.Contains(r.out, "OPEN_OK:") {
				t.Errorf("子进程 %d 退出码为 0 但没有打印 OPEN_OK：\n%s", i, r.out)
			}
		default:
			failed++
			if !strings.Contains(r.out, "OPEN_ERR:") {
				t.Errorf("子进程 %d 失败了，但不是 Open 返回的干净错误：\n%s", i, r.out)
			}
			t.Logf("子进程 %d 干净失败：%s", i, firstLine(r.out))
		}
	}
	t.Logf("真进程并发开库结果：成功 %d / 干净失败 %d / 共 %d", ok, failed, procs)
	if ok == 0 {
		t.Fatalf("没有任何进程成功打开并迁移：%d 个全失败", procs)
	}

	// 风暴之后：库必须仍然可用，且 schema 恰好是当前版本（迁移没有被做两遍）。
	ctx := context.Background()
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("风暴之后重新 Open: %v", err)
	}
	defer func() { _ = again.Close() }()
	version, err := again.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("风暴之后 SchemaVersion: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("风暴之后 schema 版本 = %d，期望 %d", version, len(migrations))
	}
	assertTables(t, again)
}

// firstLine 取输出的第一行非空内容（失败详情很长时日志里只留一行）。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 04-4：三个 pragma 的当前取值 + 删掉的行还在文件里
// ---------------------------------------------------------------------------

// TestDeletedSettingBytesRemainInDatabaseFiles 覆盖 04-4。
//
// 审计的原话：「`PRAGMA journal_size_limit` / `auto_vacuum` / `secure_delete` 未覆盖：
// 删除的行（TOTP 种子、登出删掉的会话）会以旧页形式留在主库空闲页与 `-wal` 里直到被覆盖
// —— SQLite 默认行为，本项目"文件权限即边界"的既定前提」。
//
// 本用例把三件事钉死：
//
//	① 三个 pragma 的**当前取值**（DSN 里没有设过它们 —— 见 store.go 的 dsn）；
//	② `secure_delete` 关着这一条与"删掉的值仍能按字节搜到"是同一件事的两面：
//	   TOTP 种子形态的标记写进去 → 删掉 → `wal_checkpoint(TRUNCATE)` → 主库字节里搜得到；
//	③ `-wal` 自身在 checkpoint 之前也含明文（审计的"修正 1"说的就是这条时序）。
//
// ⚠️ 本用例钉住的是**当前实现的真实行为**，不是"期望行为"：它证明的是
// "文件权限（目录 0700 / 文件 0600）是这类数据的唯一边界"这个既定前提**确实成立**。
// 若将来打开 `secure_delete` / `auto_vacuum`，本用例会红 —— 那是一次有意的行为变更，
// 必须同时改 docs/DESIGN.md、docs/SECURITY.md 与 S-1 的结论，不能顺手改断言。
//
// 反向验证（已实测，见报告）：把 DSN 加上 `secure_delete(1)` → ②红在
// "删除之后主库字节里仍应搜得到标记"。
func TestDeletedSettingBytesRemainInDatabaseFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// ① 三个 pragma 的当前取值。
	readPragma := func(name string) int64 {
		t.Helper()
		var v int64
		if err := db.Reader().QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
			t.Fatalf("读 PRAGMA %s: %v", name, err)
		}
		return v
	}
	journalLimit := readPragma("journal_size_limit")
	autoVacuum := readPragma("auto_vacuum")
	secureDelete := readPragma("secure_delete")
	t.Logf("当前 pragma：journal_size_limit=%d auto_vacuum=%d secure_delete=%d",
		journalLimit, autoVacuum, secureDelete)
	if secureDelete != 0 {
		t.Errorf("secure_delete = %d，期望 0（关）：它一旦打开，本用例第 ② 段的前提就不成立了，"+
			"必须同时更新 docs 与 S-1 的结论", secureDelete)
	}

	// ② 写一个 TOTP 种子形态的标记 → 删掉 → checkpoint → 主库字节里搜。
	marker := "TOTPSEED-" + strings.Repeat("Zq7", 24) // 80 字节，够长，不会被相邻写入部分覆盖
	if err := db.SetSetting(ctx, "totp_secret", marker); err != nil {
		t.Fatalf("写入标记: %v", err)
	}
	// 顺手加几行填充，让库里有多个页（更接近真实库的形态，而不是"只有一页、随手被重写"）。
	for i := 0; i < 8; i++ {
		if err := db.SetSetting(ctx, fmt.Sprintf("filler_%02d", i), strings.Repeat("f", 300)); err != nil {
			t.Fatalf("写入填充行 %d: %v", i, err)
		}
	}

	walBytes := readFileIfExists(t, path+"-wal")
	if !bytes.Contains(walBytes, []byte(marker)) {
		t.Errorf("checkpoint 之前 -wal 里就该有明文标记（审计的时序修正）：没搜到（-wal %d 字节）",
			len(walBytes))
	}

	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, "totp_secret"); err != nil {
		t.Fatalf("删除标记行: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	if v, ok, err := db.GetSetting(ctx, "totp_secret"); err != nil {
		t.Fatalf("删除后读回: %v", err)
	} else if ok {
		t.Fatalf("删除后仍然读得到 %q：删除没有生效，本用例失去判别力", v)
	}

	mainBytes := readFileIfExists(t, path)
	if !bytes.Contains(mainBytes, []byte(marker)) {
		t.Errorf("删除 + checkpoint(TRUNCATE) 之后，主库（%d 字节）里再也搜不到标记 —— "+
			"要么 secure_delete 被打开了，要么页被彻底重写了；"+
			"无论哪种，「文件权限即边界」这个前提都需要重新评估", len(mainBytes))
	}
}

// readFileIfExists 读文件；不存在时返回 nil（-wal 在 checkpoint 之后可能被截断成 0 字节）。
func readFileIfExists(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读文件 %s: %v", path, err)
	}
	return data
}
