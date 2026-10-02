package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCounterDelta(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur uint64
		wantDelta uint64
		wantReset bool
	}{
		{"正常增长", 1000, 1500, 500, false},
		{"没有变化", 1000, 1000, 0, false},
		{"清零", 1000, 10, 0, true},
		{"32 位回绕", wrap32 - 100, 50, 150, false},
		{"回绕分界以下按清零处理", wrap32Threshold - 1, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, reset := counterDelta(tc.prev, tc.cur)
			if delta != tc.wantDelta || reset != tc.wantReset {
				t.Fatalf("counterDelta(%d, %d) = (%d, %v)，期望 (%d, %v)",
					tc.prev, tc.cur, delta, reset, tc.wantDelta, tc.wantReset)
			}
		})
	}
}

func newMemTraffic(t *testing.T) *Traffic {
	t.Helper()
	tr, warn, err := LoadTraffic("")
	if err != nil || warn != "" {
		t.Fatalf("LoadTraffic(\"\") = %v, %q", err, warn)
	}
	return tr
}

func TestTrafficFirstApplyIsBaselineOnly(t *testing.T) {
	tr := newMemTraffic(t)
	now := time.Unix(1_700_000_000, 0)

	delta := tr.Apply("eth0", 2, "52:54:00:aa:bb:cc", "boot-1", 5_000_000, 1_000_000, now)
	if delta.Reset != "init" || delta.Rx != 0 || delta.Tx != 0 {
		t.Fatalf("首次采样应当只建基线，实际 %+v", delta)
	}
	cp := tr.Checkpoint()
	if cp.TotalRx != 0 || cp.TotalTx != 0 {
		t.Fatalf("首次采样后累计值应为 0，实际 rx=%d tx=%d", cp.TotalRx, cp.TotalTx)
	}
	if cp.RawRx != 5_000_000 || cp.Iface != "eth0" || cp.BootID != "boot-1" {
		t.Fatalf("基线内容错误: %+v", cp)
	}
}

func TestTrafficAccumulatesDeltas(t *testing.T) {
	tr := newMemTraffic(t)
	base := time.Unix(1_700_000_000, 0)
	tr.Apply("eth0", 2, "mac", "boot-1", 1_000_000, 500_000, base)

	delta := tr.Apply("eth0", 2, "mac", "boot-1", 1_500_000, 700_000, base.Add(10*time.Second))
	if delta.Rx != 500_000 || delta.Tx != 200_000 || delta.Reset != "" {
		t.Fatalf("增量计算错误: %+v", delta)
	}
	cp := tr.Checkpoint()
	if cp.TotalRx != 500_000 || cp.TotalTx != 200_000 {
		t.Fatalf("累计值错误: rx=%d tx=%d", cp.TotalRx, cp.TotalTx)
	}
	if tr.CkptAge(base.Add(10*time.Second)) != -1 {
		t.Fatal("仅内存模式的 CkptAge 必须是 -1")
	}
}

func TestTrafficRebootResetsBaselineWithoutCounting(t *testing.T) {
	tr := newMemTraffic(t)
	base := time.Unix(1_700_000_000, 0)
	tr.Apply("eth0", 2, "mac", "boot-1", 900_000_000_000, 500_000_000_000, base)
	tr.Apply("eth0", 2, "mac", "boot-1", 900_000_100_000, 500_000_100_000, base.Add(time.Second))

	before := tr.Checkpoint()
	// 机器重启：boot_id 变了，内核计数器从 0 开始。
	delta := tr.Apply("eth0", 2, "mac", "boot-2", 1_234_567, 765_432, base.Add(2*time.Second))
	if delta.Reset != "reboot" || delta.Rx != 0 || delta.Tx != 0 {
		t.Fatalf("重启后应当只重设基线，实际 %+v", delta)
	}
	after := tr.Checkpoint()
	if after.TotalRx != before.TotalRx || after.TotalTx != before.TotalTx {
		t.Fatalf("重启不得改变累计值：之前 %d/%d，之后 %d/%d",
			before.TotalRx, before.TotalTx, after.TotalRx, after.TotalTx)
	}
	if after.RawRx != 1_234_567 || after.BootID != "boot-2" {
		t.Fatalf("重启后基线未更新: %+v", after)
	}
}

func TestTrafficIfaceChangeResetsBaseline(t *testing.T) {
	tr := newMemTraffic(t)
	base := time.Unix(1_700_000_000, 0)
	tr.Apply("eth0", 2, "mac-a", "boot-1", 10_000, 20_000, base)

	cases := []struct {
		name    string
		iface   string
		ifindex int
		mac     string
	}{
		{"换网卡名", "eth1", 3, "mac-a"},
		{"网卡重建（ifindex 变化）", "eth0", 5, "mac-a"},
		{"网卡重建（MAC 变化）", "eth0", 2, "mac-b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fresh := newMemTraffic(t)
			fresh.Apply("eth0", 2, "mac-a", "boot-1", 10_000, 20_000, base)
			fresh.Apply("eth0", 2, "mac-a", "boot-1", 30_000, 40_000, base.Add(time.Second))
			total := fresh.Checkpoint().TotalRx

			delta := fresh.Apply(tc.iface, tc.ifindex, tc.mac, "boot-1", 100, 200, base.Add(2*time.Second))
			if delta.Reset != "iface_changed" || delta.Rx != 0 {
				t.Fatalf("期望 iface_changed，实际 %+v", delta)
			}
			if fresh.Checkpoint().TotalRx != total {
				t.Fatal("网卡变化时不得累加流量")
			}
		})
	}
}

func TestTrafficCounterResetKeepsOtherDirection(t *testing.T) {
	tr := newMemTraffic(t)
	base := time.Unix(1_700_000_000, 0)
	tr.Apply("eth0", 2, "mac", "boot-1", 5_000, 5_000, base)

	// rx 计数器被清零，tx 正常增长：rx 记 0，tx 照常累加。
	delta := tr.Apply("eth0", 2, "mac", "boot-1", 100, 8_000, base.Add(time.Second))
	if delta.Reset != "counter_reset" {
		t.Fatalf("期望 counter_reset，实际 %+v", delta)
	}
	if delta.Rx != 0 || delta.Tx != 3_000 {
		t.Fatalf("单向清零的处理错误: %+v", delta)
	}
}

func TestTrafficSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(1_700_000_000, 0)

	tr, warn, err := LoadTraffic(path)
	if err != nil || warn != "" {
		t.Fatalf("首次加载 = %v, %q", err, warn)
	}
	tr.Apply("eth0", 2, "mac", "boot-1", 1_000_000, 500_000, now)
	tr.Apply("eth0", 2, "mac", "boot-1", 1_600_000, 800_000, now.Add(20*time.Second))

	saved, err := tr.MaybeSave(now.Add(20*time.Second), false)
	if err != nil || !saved {
		t.Fatalf("MaybeSave = %v, %v（超过 10s 间隔应当落盘）", saved, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("checkpoint 文件不存在: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("临时文件应当已被 rename")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("checkpoint 权限 = %o，期望 600", perm)
		}
	}

	// 重新加载：累计值必须原样恢复（Agent 重启不清零、不重复计）。
	tr2, warn, err := LoadTraffic(path)
	if err != nil || warn != "" {
		t.Fatalf("重新加载 = %v, %q", err, warn)
	}
	cp := tr2.Checkpoint()
	if cp.TotalRx != 600_000 || cp.TotalTx != 300_000 {
		t.Fatalf("重新加载的累计值 = rx %d tx %d，期望 600000/300000", cp.TotalRx, cp.TotalTx)
	}
	if cp.RawRx != 1_600_000 || cp.Iface != "eth0" || cp.BootID != "boot-1" {
		t.Fatalf("重新加载的基线错误: %+v", cp)
	}
	if age := tr2.CkptAge(now.Add(25 * time.Second)); age != 5 {
		t.Fatalf("CkptAge = %d，期望 5", age)
	}

	// 继续累加：不能把已统计的 600000 再加一遍。
	delta := tr2.Apply("eth0", 2, "mac", "boot-1", 1_700_000, 900_000, now.Add(30*time.Second))
	if delta.Rx != 100_000 || delta.Tx != 100_000 {
		t.Fatalf("重启后的增量错误: %+v", delta)
	}
}

func TestTrafficMaybeSaveThrottles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(1_700_000_000, 0)
	tr, _, err := LoadTraffic(path)
	if err != nil {
		t.Fatalf("加载: %v", err)
	}
	tr.Apply("eth0", 2, "mac", "boot-1", 0, 0, now)
	tr.Apply("eth0", 2, "mac", "boot-1", 1_000_000, 0, now.Add(time.Second))

	// 首次落盘（还没有任何 checkpoint）不节流。
	if saved, err := tr.MaybeSave(now.Add(2*time.Second), false); !saved || err != nil {
		t.Fatalf("首次落盘应当立即执行（saved=%v err=%v）", saved, err)
	}
	// 之后按 10s 间隔节流。
	tr.Apply("eth0", 2, "mac", "boot-1", 2_000_000, 0, now.Add(3*time.Second))
	if saved, err := tr.MaybeSave(now.Add(3*time.Second), false); saved || err != nil {
		t.Fatalf("距上次落盘不足 10s 且增量很小，不应落盘（saved=%v err=%v）", saved, err)
	}
	if saved, err := tr.MaybeSave(now.Add(3*time.Second), true); !saved || err != nil {
		t.Fatalf("force 应当立即落盘（saved=%v err=%v）", saved, err)
	}
	if saved, err := tr.MaybeSave(now.Add(4*time.Second), false); saved || err != nil {
		t.Fatal("没有新的增量时不应重复落盘")
	}
	// 超过间隔后自动落盘。
	tr.Apply("eth0", 2, "mac", "boot-1", 3_000_000, 0, now.Add(5*time.Second))
	if saved, err := tr.MaybeSave(now.Add(40*time.Second), false); !saved || err != nil {
		t.Fatalf("超过 10s 间隔应当落盘（saved=%v err=%v）", saved, err)
	}
}

func TestTrafficLargeDeltaTriggersSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(1_700_000_000, 0)
	tr, _, err := LoadTraffic(path)
	if err != nil {
		t.Fatalf("加载: %v", err)
	}
	tr.Apply("eth0", 2, "mac", "boot-1", 0, 0, now)
	tr.Apply("eth0", 2, "mac", "boot-1", checkpointDelta+1, 0, now.Add(time.Second))

	if saved, err := tr.MaybeSave(now.Add(time.Second), false); !saved || err != nil {
		t.Fatalf("未落盘增量超过阈值应当立即落盘（saved=%v err=%v）", saved, err)
	}
}

func TestTrafficCorruptCheckpointIsIgnoredWithWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}
	tr, warn, err := LoadTraffic(path)
	if err != nil {
		t.Fatalf("坏 checkpoint 不应让 Agent 起不来: %v", err)
	}
	if warn == "" {
		t.Fatal("应当返回一条警告说明 checkpoint 已按新基线处理")
	}
	if cp := tr.Checkpoint(); cp.Iface != "" || cp.TotalRx != 0 {
		t.Fatalf("坏 checkpoint 应当从空基线开始，实际 %+v", cp)
	}
}

func TestTrafficMissingDirectoriesAreCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "state.json")
	now := time.Unix(1_700_000_000, 0)
	tr, _, err := LoadTraffic(path)
	if err != nil {
		t.Fatalf("加载: %v", err)
	}
	tr.Apply("eth0", 2, "mac", "boot-1", 0, 0, now)
	if saved, err := tr.MaybeSave(now, true); !saved || err != nil {
		t.Fatalf("落盘失败: saved=%v err=%v", saved, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("文件内容不是合法 JSON: %v", err)
	}
	if cp.Iface != "eth0" {
		t.Fatalf("文件内容错误: %+v", cp)
	}
}

// assertBlocksWhileLocked 断言 fn 在锁被外界持有时不会返回，即它确实走同一把锁。
//
// 返回的 channel 在 fn 真正返回后关闭；调用方放锁后应当等它关闭，
// 避免用例结束时还有 goroutine 在跑。
func assertBlocksWhileLocked(t *testing.T, name string, fn func()) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		t.Errorf("%s 在锁被持有时仍然返回了：它没有走同一把锁（并发调用会同时改同一份状态）", name)
	case <-time.After(50 * time.Millisecond):
	}
	return done
}

// waitClosed 等一个 assertBlocksWhileLocked 留下的 goroutine 结束。
func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("放锁后调用仍然没有返回")
	}
}

// Traffic 的导出方法必须串行化。
//
// 为什么：读循环失败后 session 只等写循环 2 秒就返回（见 Client.session），
// 被遗弃的旧写循环会与新写循环、以及与退出时 main 的强制落盘并发调用这些方法。
// 没有锁时它们同时改 t.cp / t.dirty / t.savedAt，还会同时写同一个 .tmp 文件
// ——真正的数据竞争（本机没有可用的 -race，所以这里用"持锁时必须阻塞"来判定，
// 不依赖竞态调度的偶然性：去掉任何一处加锁，本用例立刻变红）。
func TestTrafficMethodsSerializeOnOneLock(t *testing.T) {
	tr := newMemTraffic(t)
	now := time.Unix(1_700_000_000, 0)

	tr.mu.Lock()
	calls := []struct {
		name string
		fn   func()
	}{
		{"Apply", func() { tr.Apply("eth0", 2, "mac", "boot-1", 100, 200, now) }},
		{"Checkpoint", func() { _ = tr.Checkpoint() }},
		{"CkptAge", func() { _ = tr.CkptAge(now) }},
		{"MaybeSave", func() { _, _ = tr.MaybeSave(now, true) }},
	}
	var pending []<-chan struct{}
	for _, call := range calls {
		pending = append(pending, assertBlocksWhileLocked(t, call.name, call.fn))
	}
	tr.mu.Unlock()
	for _, done := range pending {
		waitClosed(t, done)
	}
}

// 两个"写循环"并发落盘时，checkpoint 文件必须始终是一份完整可解析的 JSON。
//
// 修好之前两个 goroutine 会同时写同一个 <path>.tmp 再 rename：读到的可能是
// 半个 JSON（累计值就此丢失）。
func TestTrafficConcurrentMaybeSaveKeepsCheckpointValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(1_700_000_000, 0)
	tr, _, err := LoadTraffic(path)
	if err != nil {
		t.Fatalf("加载: %v", err)
	}
	tr.Apply("eth0", 2, "mac", "boot-1", 1_000, 2_000, now)                  // init：只设基线
	tr.Apply("eth0", 2, "mac", "boot-1", 1_500, 2_400, now.Add(time.Second)) // 累加 500/400

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := tr.MaybeSave(now.Add(time.Duration(i)*time.Second), true); err != nil {
					t.Errorf("并发落盘失败: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 checkpoint: %v", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("并发落盘后文件不是合法 JSON（写坏了自己唯一的累计值）: %v\n内容: %q", err, data)
	}
	if cp.Iface != "eth0" || cp.TotalRx == 0 {
		t.Fatalf("并发落盘后的 checkpoint 内容不对: %+v", cp)
	}
}
