package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// copyFixtureRoot 把只读的 fixture 快照复制到临时目录，便于测试中途修改。
func copyFixtureRoot(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "root")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("复制 fixture 失败: %v", err)
	}
	return dst
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", rel, err)
	}
}

func removeFile(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("删除 %s 失败: %v", rel, err)
	}
}

// netDevLine / netDevFile 生成 /proc/net/dev 内容（列数与真实文件一致）。
func netDevLine(iface string, rx, tx uint64) string {
	return fmt.Sprintf("  %s: %d 1 0 0 0 0 0 0 %d 1 0 0 0 0 0 0\n", iface, rx, tx)
}

func netDevFile(lines ...string) string {
	return "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		strings.Join(lines, "")
}

func containsWarning(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestCollectorInfoFromSnapshot(t *testing.T) {
	c := New(filepath.Join("testdata", "root"), "", "/", nil)
	info, err := c.Info()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.OS.Name != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("系统名 = %q", info.OS.Name)
	}
	if info.OS.Kernel != "6.1.0-13-amd64" {
		t.Errorf("内核 = %q", info.OS.Kernel)
	}
	if info.OS.Arch != runtime.GOARCH {
		t.Errorf("架构 = %q", info.OS.Arch)
	}
	if info.CPU.Cores != 2 || !strings.Contains(info.CPU.Model, "Xeon") {
		t.Errorf("CPU = %+v", info.CPU)
	}
	if info.BootID != "9f1c2b3a-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("boot_id = %q", info.BootID)
	}
	if info.UptimeSec != 1234567 {
		t.Errorf("uptime = %d", info.UptimeSec)
	}
	want := struct {
		name    string
		ifindex int
		mac     string
	}{"eth0", 2, "52:54:00:aa:bb:cc"}
	if info.Iface.Name != want.name || info.Iface.IfIndex != want.ifindex || info.Iface.MAC != want.mac {
		t.Errorf("网卡身份 = %+v，期望 %+v", info.Iface, want)
	}
	if info.AgentVersion == "" {
		t.Error("Agent 版本不应为空")
	}
}

func TestCollectorSampleAndRates(t *testing.T) {
	root := copyFixtureRoot(t)
	traffic, warn, err := LoadTraffic("") // 仅内存：诊断模式不落盘
	if err != nil || warn != "" {
		t.Fatalf("LoadTraffic: %v %q", err, warn)
	}
	c := New(root, "", "/", traffic)
	t0 := time.Unix(1_700_000_000, 0)

	m1, warnings1, err := c.Sample(t0)
	if err != nil {
		t.Fatalf("采样 1: %v", err)
	}
	if m1.CPUPct != 0 {
		t.Errorf("首次采样没有 CPU 基线，应为 0，实际 %v", m1.CPUPct)
	}
	if m1.CPUCores != 2 {
		t.Errorf("核心数 = %d", m1.CPUCores)
	}
	if m1.Mem.Total != 2048000*1024 || m1.Mem.Used != uint64(2048000-987654)*1024 {
		t.Errorf("内存 = %+v", m1.Mem)
	}
	if math.Abs(m1.Mem.Pct-51.774) > 0.01 {
		t.Errorf("内存使用率 = %v，期望约 51.77", m1.Mem.Pct)
	}
	if m1.Swap.Total != 1048576*1024 || m1.Swap.Used != 262144*1024 || math.Abs(m1.Swap.Pct-25) > 1e-9 {
		t.Errorf("swap = %+v", m1.Swap)
	}
	if m1.Load.L1 != 0.15 || m1.Load.L5 != 0.08 || m1.Load.L15 != 0.03 {
		t.Errorf("负载 = %+v", m1.Load)
	}
	if m1.UptimeSec != 1234567 {
		t.Errorf("uptime = %d", m1.UptimeSec)
	}
	if m1.Net.Iface != "eth0" || m1.Net.RxRaw != 9876543210 || m1.Net.TxRaw != 1234567890 {
		t.Errorf("网卡计数 = %+v", m1.Net)
	}
	if m1.Net.RxTotal != 0 || m1.Net.TxTotal != 0 {
		t.Errorf("首次采样累计值应为 0: %+v", m1.Net)
	}
	if m1.Net.CkptAgeS != -1 {
		t.Errorf("仅内存模式的 ckpt_age_s 应为 -1，实际 %d", m1.Net.CkptAgeS)
	}
	if len(m1.Disk) != 0 {
		t.Errorf("快照模式应跳过磁盘统计，实际 %+v", m1.Disk)
	}
	if !containsWarning(warnings1, "快照模式") {
		t.Errorf("应当提示跳过磁盘统计: %v", warnings1)
	}

	// 10 秒后：CPU 25%，rx +10 MiB，tx +5 MiB。
	writeFile(t, root, "proc/stat", "cpu  1234767 8901 456789 98766032 12345 0 6789 23456 0 0\n")
	writeFile(t, root, "proc/net/dev", netDevFile(
		netDevLine("lo", 123456789, 123456789),
		netDevLine("eth0", 9876543210+10*1024*1024, 1234567890+5*1024*1024),
	))

	m2, warnings2, err := c.Sample(t0.Add(10 * time.Second))
	if err != nil {
		t.Fatalf("采样 2: %v", err)
	}
	if math.Abs(m2.CPUPct-25) > 1e-9 {
		t.Errorf("CPU 使用率 = %v，期望 25", m2.CPUPct)
	}
	if math.Abs(m2.Net.RxRate-1048576) > 1e-9 || math.Abs(m2.Net.TxRate-524288) > 1e-9 {
		t.Errorf("速率 = rx %v tx %v，期望 1048576 / 524288", m2.Net.RxRate, m2.Net.TxRate)
	}
	if m2.Net.RxTotal != 10*1024*1024 || m2.Net.TxTotal != 5*1024*1024 {
		t.Errorf("累计值 = rx %d tx %d", m2.Net.RxTotal, m2.Net.TxTotal)
	}
	if containsWarning(warnings2, "基线已重设") {
		t.Errorf("正常采样不应重设基线: %v", warnings2)
	}
}

func TestCollectorKeepsTrafficAcrossRestart(t *testing.T) {
	root := copyFixtureRoot(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	t0 := time.Unix(1_700_000_000, 0)

	tr1, _, err := LoadTraffic(statePath)
	if err != nil {
		t.Fatalf("加载: %v", err)
	}
	c1 := New(root, "", "/", tr1)
	if _, _, err := c1.Sample(t0); err != nil {
		t.Fatalf("采样 1: %v", err)
	}
	writeFile(t, root, "proc/net/dev", netDevFile(netDevLine("eth0", 9876543210+1000, 1234567890+2000)))
	if _, _, err := c1.Sample(t0.Add(2 * time.Second)); err != nil {
		t.Fatalf("采样 2: %v", err)
	}
	if saved, err := tr1.MaybeSave(t0.Add(20*time.Second), true); err != nil || !saved {
		t.Fatalf("落盘失败: saved=%v err=%v", saved, err)
	}

	// 模拟 Agent 重启：重新加载 checkpoint 后累计值必须延续。
	tr2, warn, err := LoadTraffic(statePath)
	if err != nil || warn != "" {
		t.Fatalf("重新加载: %v %q", err, warn)
	}
	c2 := New(root, "", "/", tr2)
	writeFile(t, root, "proc/net/dev", netDevFile(netDevLine("eth0", 9876543210+3000, 1234567890+5000)))
	m, _, err := c2.Sample(t0.Add(30 * time.Second))
	if err != nil {
		t.Fatalf("重启后采样: %v", err)
	}
	if m.Net.RxTotal != 3000 || m.Net.TxTotal != 5000 {
		t.Fatalf("重启后累计 = rx %d tx %d，期望 3000 / 5000", m.Net.RxTotal, m.Net.TxTotal)
	}
}

func TestCollectorReResolvesIfaceInAutoMode(t *testing.T) {
	root := copyFixtureRoot(t)
	c := New(root, "", "/", nil)
	t0 := time.Unix(1_700_000_000, 0)
	if _, _, err := c.Sample(t0); err != nil {
		t.Fatalf("采样 1: %v", err)
	}

	// 网卡被重建并改名：eth0 消失，eth1 出现，路由表还是旧的。
	removeFile(t, root, "proc/net/route")
	removeFile(t, root, "proc/net/ipv6_route")
	writeFile(t, root, "proc/net/dev", netDevFile(
		netDevLine("eth1", 5000, 6000),
		netDevLine("veth1a2b", 1, 2),
	))
	writeFile(t, root, "sys/class/net/eth1/ifindex", "9")
	writeFile(t, root, "sys/class/net/eth1/address", "52:54:00:11:22:33")
	writeFile(t, root, "sys/class/net/eth1/operstate", "up")

	m, warnings, err := c.Sample(t0.Add(time.Second))
	if err != nil {
		t.Fatalf("重新探测后仍然失败: %v", err)
	}
	if m.Net.Iface != "eth1" {
		t.Fatalf("应当切到 eth1，实际 %s", m.Net.Iface)
	}
	if !containsWarning(warnings, "消失") {
		t.Errorf("应当提示原网卡消失: %v", warnings)
	}
	if !containsWarning(warnings, "iface_changed") {
		t.Errorf("切网卡后应当重设流量基线: %v", warnings)
	}
}

func TestCollectorErrors(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)

	t.Run("缺少 /proc/stat", func(t *testing.T) {
		root := copyFixtureRoot(t)
		removeFile(t, root, "proc/stat")
		if _, _, err := New(root, "", "/", nil).Sample(t0); err == nil {
			t.Fatal("应当报错")
		}
	})

	t.Run("缺少 MemTotal", func(t *testing.T) {
		root := copyFixtureRoot(t)
		writeFile(t, root, "proc/meminfo", "MemFree: 100 kB\n")
		if _, _, err := New(root, "", "/", nil).Sample(t0); err == nil {
			t.Fatal("应当报错")
		}
	})

	t.Run("缺少 /proc/net/dev", func(t *testing.T) {
		root := copyFixtureRoot(t)
		removeFile(t, root, "proc/net/dev")
		_, _, err := New(root, "", "/", nil).Sample(t0)
		if err == nil || !strings.Contains(err.Error(), "net/dev") {
			t.Fatalf("应当因为 net/dev 报错，实际 %v", err)
		}
	})

	t.Run("指定的网卡不存在", func(t *testing.T) {
		root := copyFixtureRoot(t)
		_, _, err := New(root, "eth9", "/", nil).Sample(t0)
		if err == nil || !strings.Contains(err.Error(), "eth9") {
			t.Fatalf("应当提示网卡不存在，实际 %v", err)
		}
	})

	t.Run("只有 lo 时无法探测网卡", func(t *testing.T) {
		root := copyFixtureRoot(t)
		writeFile(t, root, "proc/net/dev", netDevFile(netDevLine("lo", 1, 2)))
		_, _, err := New(root, "", "/", nil).Sample(t0)
		if err == nil || !strings.Contains(err.Error(), "没有任何网卡") {
			t.Fatalf("应当提示没有可监控网卡，实际 %v", err)
		}
	})

	t.Run("文件为空时解析失败", func(t *testing.T) {
		root := copyFixtureRoot(t)
		writeFile(t, root, "proc/stat", "")
		if _, _, err := New(root, "", "/", nil).Sample(t0); err == nil {
			t.Fatal("应当报错")
		}
	})
}

func TestCollectorAutoIfaceFallbackSkipsVirtual(t *testing.T) {
	root := copyFixtureRoot(t)
	removeFile(t, root, "proc/net/route")
	removeFile(t, root, "proc/net/ipv6_route")
	// veth 的流量远大于物理网卡，但兜底逻辑必须跳过虚拟网卡。
	writeFile(t, root, "proc/net/dev", netDevFile(
		netDevLine("veth1a2b", 1<<40, 1<<40),
		netDevLine("ens3", 1000, 2000),
	))
	writeFile(t, root, "sys/class/net/ens3/ifindex", "5")
	writeFile(t, root, "sys/class/net/ens3/address", "52:54:00:99:88:77")
	writeFile(t, root, "sys/class/net/ens3/operstate", "up")

	m, _, err := New(root, "", "/", nil).Sample(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}
	if m.Net.Iface != "ens3" {
		t.Fatalf("应当选中 ens3，实际 %s", m.Net.Iface)
	}
}

func TestDisksFromMounts(t *testing.T) {
	mountsData := readFixture(t, "proc/mounts")
	statfs := func(path string) (uint64, uint64, uint64, error) {
		switch path {
		case "/":
			return 100 << 30, 50 << 30, 50 << 30, nil
		case "/data":
			return 200 << 30, 100 << 30, 100 << 30, nil
		default:
			return 0, 0, 0, errors.New("不存在的挂载点")
		}
	}

	disks, warnings := disksFromMounts(mountsData, "/", statfs)
	if len(warnings) != 0 {
		t.Fatalf("不应有警告: %v", warnings)
	}
	if len(disks) != 2 {
		t.Fatalf("磁盘数量 = %d，期望 2", len(disks))
	}
	if disks[0].Mount != "/" || disks[0].FS != "ext4" || math.Abs(disks[0].Pct-50) > 1e-9 {
		t.Fatalf("主文件系统 = %+v", disks[0])
	}
	if disks[1].Mount != "/data" {
		t.Fatalf("附加磁盘 = %+v", disks[1])
	}

	// 主文件系统 statfs 失败时：给出警告，但仍然返回可用的附加磁盘。
	disks, warnings = disksFromMounts(mountsData, "/", func(path string) (uint64, uint64, uint64, error) {
		if path == "/" {
			return 0, 0, 0, errors.New("权限不足")
		}
		return 10 << 30, 5 << 30, 5 << 30, nil
	})
	if len(disks) != 1 || disks[0].Mount != "/data" {
		t.Fatalf("磁盘列表 = %+v", disks)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "权限不足") {
		t.Fatalf("警告 = %v", warnings)
	}

	// 挂载表为空：给出警告且不 panic。
	disks, warnings = disksFromMounts("", "/", statfs)
	if len(disks) != 0 || len(warnings) != 1 {
		t.Fatalf("空挂载表应当只给一条警告，实际 disks=%v warnings=%v", disks, warnings)
	}
}

// Collector 的跨拍状态与缓存必须走同一把锁。
//
// 为什么：读循环失败后 session 只等写循环 2 秒就返回（见 Client.session），
// 被遗弃的旧写循环如果卡在 statfs/fsync 里，会与新写循环并发调用 Sample ——
// prevCPU/prevNet/ifaceReady/infoLoaded 就会被两个 goroutine 同时读写。
// 判定方式与 Traffic 那边一致："持锁时调用必须阻塞"，不依赖竞态调度的偶然性。
func TestCollectorStateAccessSerializesOnOneLock(t *testing.T) {
	c := New(filepath.Join("testdata", "root"), "", "/", nil)
	// 先确认快照能正常采样：否则下面"阻塞"的判定会因为提前返回而误报。
	if _, _, err := c.Sample(time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("fixture 采样失败: %v", err)
	}

	c.mu.Lock()
	pending := []<-chan struct{}{
		assertBlocksWhileLocked(t, "Info", func() { _, _ = c.Info() }),
		assertBlocksWhileLocked(t, "resolveIface", func() { _, _ = c.resolveIface() }),
		assertBlocksWhileLocked(t, "bootID", func() { _ = c.bootID() }),
		assertBlocksWhileLocked(t, "invalidateIface", func() { c.invalidateIface() }),
		assertBlocksWhileLocked(t, "Sample", func() {
			_, _, _ = c.Sample(time.Unix(1_700_000_001, 0))
		}),
	}
	c.mu.Unlock()
	for _, done := range pending {
		waitClosed(t, done)
	}
}

// boot_id 按 TTL 缓存：一次开机内恒定，不必每拍重读一次
// /proc/sys/kernel/random/boot_id（每台机器每秒一次、永久）。
//
// TTL 而不是永久缓存，是为了 --root 指向"会跨越宿主重启的实时 /proc"的部署
// （容器内监控宿主机）：那种部署下永久缓存会漏掉重启检测。
func TestCollectorCachesBootIDWithinTTL(t *testing.T) {
	root := copyFixtureRoot(t)
	c := New(root, "", "/", nil)

	first := c.bootID()
	if first == "" {
		t.Fatal("fixture 里的 boot_id 应当能读到")
	}

	// TTL 内改了文件也必须拿缓存值：说明没有每拍重读。
	writeFile(t, root, "proc/sys/kernel/random/boot_id", "aaaaaaaa-1111-2222-3333-444444444444\n")
	if got := c.bootID(); got != first {
		t.Fatalf("TTL 内应当命中缓存：得到 %q，期望 %q", got, first)
	}

	// 超过 TTL 后必须重读（重启检测的兜底）。
	c.mu.Lock()
	c.bootIDAt = time.Now().Add(-2 * bootIDTTL)
	c.mu.Unlock()
	if got := c.bootID(); got != "aaaaaaaa-1111-2222-3333-444444444444" {
		t.Fatalf("TTL 过期后应当重读文件，实际 %q", got)
	}
}
