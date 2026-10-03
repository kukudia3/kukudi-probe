package store

// 第四轮「缺口补齐」用例：第二轮安全审计 SECURITY-AUDIT-ROUND2.md §7 区域 04 里
// 被判「本机可补、但当时没做」的三条（逐条编号见 _audit/ROUND3-VERIFY-1.md §3.4）：
//
//	04-2 `traffic_daily.rx + excluded.rx` 在极端增量下的溢出行为
//	04-3 两个进程同时打开同一个 probe.db 并同时迁移
//	04-4 `journal_size_limit` / `auto_vacuum` / `secure_delete` 的当前取值，
//	     以及「删掉的行仍然留在主库 / -wal 里」这件事本身
//
// 本文件在 round4 只加测试：产品代码一行未改（internal/store 下的非 _test.go 文件
// 逐字节未动）。round5 打破了这个前提 —— 04-2 与 04-4 两条用例随产品代码的行为变更
// 一起翻面（B2：溢出时返回能读懂的错；C11：dsn 打开 secure_delete(1)），
// 见 _audit/ROUND5-BACKEND.md。

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
//	   第一次写进去、读回精确的 2⁶²；第二次的加法结果越出 int64 ⇒ `FlushTraffic`
//	   **报错**，整个事务回滚 ⇒ 之前的 2⁶² 一个字节没变。
//	   也就是说**不是静默回绕/静默精度丢失，而是这台节点自己的流量写入开始报错**
//	   （代价见 ROUND4-GAPS.md：该节点的日流量会一直写不进去，直到人为清掉那一行）。
//
//	   ⚠️ round5：这句报错**换人了**。round4 实测的是驱动层那句英文
//	   （`constraint failed: cannot store REAL value in INTEGER column traffic_daily.rx (3091)`）；
//	   B2 之后由本程序在写之前拦下（traffic.go 的 trafficSumOverflow），报错变成一句
//	   带节点与日期、说明"计数已达上限、要人工清零"的中文。下面 B 段的断言随之翻面。
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
	// round5 起这句报错是**本程序**给的（B2 的上界保护）：一句能读懂的中文，
	// 带节点与日期。驱动那句 `cannot store REAL value in INTEGER column` 不再出现
	// —— 越界的那一步根本没发给 SQLite（见 traffic.go 的 trafficSumOverflow）。
	if strings.Contains(err.Error(), "REAL") || strings.Contains(err.Error(), "INTEGER") {
		t.Errorf("报错还是驱动层那句英文（上界保护没生效）：%v", err)
	}
	for _, want := range []string{"节点 2", day, "上限"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里缺少 %q，运维据此认不出是哪台机器/哪一天：%v", want, err)
		}
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
// 04-4：三个 pragma 的取值 + 删掉的行会不会留在文件里
// ---------------------------------------------------------------------------
//
// ⚠️ round5 起这条用例的含义变了：产品代码在 dsn 里打开了 secure_delete(1)
// （见 store.go），于是它从"钉住当前实现的**特征**（删掉的行还留在主库/-wal 里）"
// 翻面成"钉住**期望行为**（删掉的行立刻被抹掉）"。原函数名是
// TestDeletedSettingBytesRemainInDatabaseFiles；改名、断言翻转与代价都记在
// _audit/ROUND5-BACKEND.md。
//
// TestDeletedSettingBytesAreScrubbedFromDatabaseFiles 覆盖 04-4（round5 行为断言）。
//
// 它现在钉住三件事：
//
//	① 三个 pragma 的取值：secure_delete=1（本轮开的），auto_vacuum=0 与
//	   journal_size_limit=-1（**没动**，各有各的理由，见 ① 段注释）；
//	② 标记确实写进过主库字节（checkpoint 之后能搜到）——没有这一步，
//	   第 ③ 段的"搜不到"可能只是因为它压根没落进主库；
//	③ DELETE + `wal_checkpoint(TRUNCATE)` 之后，主库与 -wal 里**都**搜不到那 80 字节，
//	   而库还能正常读写、页数不变、文件不变小（secure_delete 只清零，不缩文件）。
//
// 反向验证（已实测，见 ROUND5-BACKEND.md）：把 dsn 里的 secure_delete(1) 撤掉 →
// 红在 ① 段的 "secure_delete = 0，期望 1" 与 ③ 段的 "主库…里仍然搜得到标记"。
func TestDeletedSettingBytesAreScrubbedFromDatabaseFiles(t *testing.T) {
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
	if secureDelete != 1 {
		t.Errorf("secure_delete = %d，期望 1（开）：删除的行必须立刻被抹掉（round5 的有意行为变更，"+
			"见 store.go 的 dsn）", secureDelete)
	}
	// 下面两个**必须**保持原值：它们是本轮刻意没动的两个 pragma。
	//   - auto_vacuum 是"建库时"写进文件头的设置，对已存在的库改它等于要求 VACUUM
	//     重建整个文件（生产库上的危险操作），而且此后每次删除都要移动页；
	//   - journal_size_limit 管的是 WAL 回卷后的文件大小，与"删掉的字节还在不在"无关。
	if autoVacuum != 0 {
		t.Errorf("auto_vacuum = %d，期望 0（本轮不许动它：改它要 VACUUM 重建文件）", autoVacuum)
	}
	if journalLimit != -1 {
		t.Errorf("journal_size_limit = %d，期望 -1（本轮不许动它：它管 WAL 大小）", journalLimit)
	}

	// ② 写一个 TOTP 种子形态的标记 → checkpoint → 主库字节里必须能搜到。
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

	// checkpoint 之前 -wal 里就有明文：这是 WAL 的语义（已提交的最近页先写日志），
	// secure_delete 管的是"删除时抹掉"，不是"写入时不落盘"。
	walBytes := readFileIfExists(t, path+"-wal")
	if !bytes.Contains(walBytes, []byte(marker)) {
		t.Errorf("checkpoint 之前 -wal 里就该有明文标记（审计的时序修正）：没搜到（-wal %d 字节）",
			len(walBytes))
	}
	if _, err := db.Writer().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("wal_checkpoint（删除前）: %v", err)
	}
	// 前提：标记真的进了主库。这一条不能省 —— 少了它，"③ 里搜不到"可能只是因为
	// 标记从来没写进主库，用例就变成了永远绿的。
	mainWithMarker := readFileIfExists(t, path)
	if !bytes.Contains(mainWithMarker, []byte(marker)) {
		t.Fatalf("checkpoint 之后主库（%d 字节）里搜不到标记：本用例的前提不成立（标记没落进主库），"+
			"后面的断言失去判别力", len(mainWithMarker))
	}
	pagesBefore := readPragma("page_count")

	// ③ 删掉 → 程序读不到 → checkpoint → 主库与 -wal 里都必须搜不到。
	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, "totp_secret"); err != nil {
		t.Fatalf("删除标记行: %v", err)
	}
	if v, ok, err := db.GetSetting(ctx, "totp_secret"); err != nil {
		t.Fatalf("删除后读回: %v", err)
	} else if ok {
		t.Fatalf("删除后仍然读得到 %q：删除没有生效，本用例失去判别力", v)
	}
	if _, err := db.Writer().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("wal_checkpoint（删除后）: %v", err)
	}

	mainBytes := readFileIfExists(t, path)
	if bytes.Contains(mainBytes, []byte(marker)) {
		t.Errorf("删除 + checkpoint(TRUNCATE) 之后，主库（%d 字节）里仍然搜得到那 80 字节标记 —— "+
			"secure_delete 没生效（它一旦被撤掉，删掉的 TOTP 种子/会话就会以旧页形式留在库里）",
			len(mainBytes))
	}
	if walAfter := readFileIfExists(t, path+"-wal"); bytes.Contains(walAfter, []byte(marker)) {
		t.Errorf("删除 + checkpoint(TRUNCATE) 之后，-wal（%d 字节）里仍然搜得到标记", len(walAfter))
	}

	// 清零 ≠ 缩文件：页还在（page_count 不变）、文件不变小。这两条是 secure_delete
	// 与 VACUUM 的区别，写出来免得有人以为"打开了它，删掉数据文件就会变小"。
	if pagesAfter := readPragma("page_count"); pagesAfter != pagesBefore {
		t.Errorf("page_count 从 %d 变成 %d：删除不该释放/移动页（那是 auto_vacuum 的行为）",
			pagesBefore, pagesAfter)
	}
	if len(mainBytes) != len(mainWithMarker) {
		t.Errorf("主库大小从 %d 变成 %d：secure_delete 只把内容清零，不会缩小文件",
			len(mainWithMarker), len(mainBytes))
	}
	// 清零没有把页写坏：库还能正常读写。
	if err := db.SetSetting(ctx, "after_delete", "ok"); err != nil {
		t.Fatalf("删除之后写入新设置: %v", err)
	}
	if v, ok, err := db.GetSetting(ctx, "after_delete"); err != nil || !ok || v != "ok" {
		t.Errorf("删除之后读写不正常：v=%q ok=%v err=%v", v, ok, err)
	}

	// ④ 再走一遍"整页被释放"的形态：登出删掉的会话、被裁剪的审计行都是整行整页地走。
	// 单行删除只证明"被删的那几个字节被清零"，证明不了"已经进 freelist 的页也被清零"
	// —— 而后者才是「文件权限即边界」当初最要紧的一半：整页残留意味着一次 grep 就能
	// 捞回整段历史。这里写 200 行（每行一个 200 字节的唯一标记）→ 全删 → checkpoint，
	// 要求一个标记都不剩，同时 freelist_count > 0（页确实进了空闲表，不是被顺手重写）。
	//
	// 反向验证（已实测，见 ROUND5-BACKEND.md）：撤掉 secure_delete(1) → 这里 200 个
	// 标记能剩下 166 个（同一台机器、同一份用例，只差那个 pragma）。
	const scrubRows = 200
	scrubMarkers := make([]string, scrubRows)
	for i := 0; i < scrubRows; i++ {
		scrubMarkers[i] = fmt.Sprintf("SCRUB%04d-", i) + strings.Repeat("k", 180)
		if err := db.SetSetting(ctx, fmt.Sprintf("scrub_%04d", i), scrubMarkers[i]); err != nil {
			t.Fatalf("写入第 %d 个整页标记: %v", i, err)
		}
	}
	if _, err := db.Writer().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("wal_checkpoint（整页场景，删除前）: %v", err)
	}
	present := 0
	for _, m := range scrubMarkers {
		if bytes.Contains(readFileIfExists(t, path), []byte(m)) {
			present++
		}
	}
	if present != scrubRows {
		t.Fatalf("删除前主库里只有 %d/%d 个整页标记：本段的前提不成立（标记没落进主库）",
			present, scrubRows)
	}
	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key LIKE 'scrub\_%' ESCAPE '\'`); err != nil {
		t.Fatalf("删除整页标记: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("wal_checkpoint（整页场景，删除后）: %v", err)
	}
	scrubbed, left := readFileIfExists(t, path), 0
	for _, m := range scrubMarkers {
		if bytes.Contains(scrubbed, []byte(m)) {
			left++
		}
	}
	if left != 0 {
		t.Errorf("整页被释放之后，主库（%d 字节）里仍然搜得到 %d/%d 个标记 —— "+
			"secure_delete 没有覆盖「进 freelist 的整页」这一半", len(scrubbed), left, scrubRows)
	}
	if free := readPragma("freelist_count"); free <= 0 {
		t.Errorf("freelist_count = %d：本段要测的正是「整页进空闲表」的形态，页没进空闲表就没有判别力", free)
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
