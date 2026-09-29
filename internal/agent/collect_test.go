package agent

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture 读取随仓库提交的真实 /proc 快照（testdata/root）。
func readFixture(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "root", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", rel, err)
	}
	return string(data)
}

func TestParseCPUStatAndUsage(t *testing.T) {
	prev, err := parseCPUStat("cpu  100 0 100 800 0 0 0 0 0 0")
	if err != nil {
		t.Fatalf("解析 prev: %v", err)
	}
	cur, err := parseCPUStat("cpu  200 0 200 1400 0 0 0 0 0 0")
	if err != nil {
		t.Fatalf("解析 cur: %v", err)
	}
	if prev.total != 1000 || prev.busy != 200 {
		t.Fatalf("prev = %+v，期望 total=1000 busy=200", prev)
	}
	if cur.total != 1800 || cur.busy != 400 {
		t.Fatalf("cur = %+v，期望 total=1800 busy=400", cur)
	}
	if got := cpuUsage(prev, cur); math.Abs(got-25) > 1e-9 {
		t.Fatalf("CPU 使用率 = %v，期望 25", got)
	}
}

func TestCPUUsageEdgeCases(t *testing.T) {
	base := cpuTimes{busy: 100, total: 1000}
	cases := []struct {
		name     string
		prev, cu cpuTimes
		want     float64
	}{
		{"两次采样相同", base, base, 0},
		{"计数器回退", cpuTimes{busy: 500, total: 2000}, base, 0},
		{"busy 变小", base, cpuTimes{busy: 50, total: 1100}, 0},
		{"全程忙", cpuTimes{busy: 0, total: 100}, cpuTimes{busy: 100, total: 200}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cpuUsage(tc.prev, tc.cu); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("CPU 使用率 = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestParseCPUStatRejectsBadInput(t *testing.T) {
	inputs := []string{
		"",
		"cpu 1 2 3",
		"cpu a b c d e f g h",
		"cpu0 1 2 3 4 5 6 7 8",
	}
	for _, in := range inputs {
		if _, err := parseCPUStat(in); err == nil {
			t.Errorf("parseCPUStat(%q) 应当报错", in)
		}
	}
}

func TestParseMemInfo(t *testing.T) {
	mi := parseMemInfo(readFixture(t, "proc/meminfo"))
	if mi.total != 2048000*1024 {
		t.Fatalf("MemTotal = %d", mi.total)
	}
	if mi.available != 987654*1024 {
		t.Fatalf("MemAvailable = %d", mi.available)
	}
	if got, want := mi.used(), uint64(2048000-987654)*1024; got != want {
		t.Fatalf("已用内存 = %d，期望 %d", got, want)
	}
	if mi.swapTotal != 1048576*1024 || mi.swapFree != 786432*1024 {
		t.Fatalf("swap 解析错误: total=%d free=%d", mi.swapTotal, mi.swapFree)
	}

	// 老内核没有 MemAvailable 时退回 MemFree+Buffers+Cached。
	old := parseMemInfo("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 250 kB\n")
	if got, want := old.used(), uint64(600)*1024; got != want {
		t.Fatalf("回退算法的已用内存 = %d，期望 %d", got, want)
	}
}

func TestParseLoadAvg(t *testing.T) {
	load, err := parseLoadAvg("0.15 0.08 0.03 1/128 9876")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if load.L1 != 0.15 || load.L5 != 0.08 || load.L15 != 0.03 {
		t.Fatalf("负载解析错误: %+v", load)
	}
	for _, bad := range []string{"", "0.15 0.08", "a b c"} {
		if _, err := parseLoadAvg(bad); err == nil {
			t.Errorf("parseLoadAvg(%q) 应当报错", bad)
		}
	}
}

func TestParseUptime(t *testing.T) {
	got, err := parseUptime("1234567.89 9876543.21")
	if err != nil || got != 1234567 {
		t.Fatalf("uptime = %d, err = %v", got, err)
	}
	for _, bad := range []string{"", "-5 0", "abc"} {
		if _, err := parseUptime(bad); err == nil {
			t.Errorf("parseUptime(%q) 应当报错", bad)
		}
	}
}

func TestParseNetDev(t *testing.T) {
	data := readFixture(t, "proc/net/dev")
	nc, ok := parseNetDev(data, "eth0")
	if !ok {
		t.Fatal("没有解析到 eth0")
	}
	if nc.rx != 9876543210 || nc.tx != 1234567890 {
		t.Fatalf("eth0 计数 = rx %d tx %d", nc.rx, nc.tx)
	}
	if _, ok := parseNetDev(data, "eth9"); ok {
		t.Error("不存在的网卡不应解析成功")
	}
	if _, ok := parseNetDev("eth0: 1 2 3", "eth0"); ok {
		t.Error("字段不足时不应解析成功")
	}
	if _, ok := parseNetDev("eth0: a b c d e f g h i", "eth0"); ok {
		t.Error("非数字时不应解析成功")
	}

	names := listNetDevIfaces(data)
	if len(names) != 2 || names[0] != "eth0" || names[1] != "veth1a2b" {
		t.Fatalf("网卡列表 = %v，期望 [eth0 veth1a2b]（不含 lo 与表头）", names)
	}
}

func TestParseDefaultRoutes(t *testing.T) {
	if got := parseDefaultRoute(readFixture(t, "proc/net/route")); got != "eth0" {
		t.Fatalf("IPv4 默认路由网卡 = %q，期望 eth0", got)
	}
	if got := parseDefaultRoute("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n"); got != "" {
		t.Fatalf("没有默认路由时应当返回空，实际 %q", got)
	}
	if got := parseDefaultRouteV6(readFixture(t, "proc/net/ipv6_route")); got != "eth0" {
		t.Fatalf("IPv6 默认路由网卡 = %q，期望 eth0", got)
	}
	if got := parseDefaultRouteV6(""); got != "" {
		t.Fatalf("空文件应当返回空，实际 %q", got)
	}
}

func TestParseCPUInfo(t *testing.T) {
	model, cores := parseCPUInfo(readFixture(t, "proc/cpuinfo"))
	if !strings.Contains(model, "Xeon") {
		t.Fatalf("CPU 型号 = %q", model)
	}
	if cores != 2 {
		t.Fatalf("核心数 = %d，期望 2", cores)
	}

	// ARM 风格：没有 model name 行，退回到 Processor/Hardware。
	arm := "Processor\t: ARMv7 Processor rev 4 (v7l)\nHardware\t: BCM2835\n"
	model, cores = parseCPUInfo(arm)
	if !strings.Contains(model, "ARMv7") {
		t.Fatalf("ARM 型号 = %q", model)
	}
	if cores != 0 {
		t.Fatalf("ARM fixture 没有 processor 行，核心数应为 0，实际 %d", cores)
	}
}

func TestParseOSRelease(t *testing.T) {
	if got := parseOSRelease(readFixture(t, "etc/os-release")); got != "Debian GNU/Linux 12 (bookworm)" {
		t.Fatalf("系统名 = %q", got)
	}
	if got := parseOSRelease(`NAME="Alpine Linux"`); got != "Alpine Linux" {
		t.Fatalf("退回 NAME 失败: %q", got)
	}
	if got := parseOSRelease(""); got != "" {
		t.Fatalf("空内容应当返回空: %q", got)
	}
}

func TestParseMountsAndPickPrimary(t *testing.T) {
	mounts := parseMounts(readFixture(t, "proc/mounts"))
	if len(mounts) != 4 {
		t.Fatalf("挂载点数量 = %d，期望 4", len(mounts))
	}

	cases := []struct {
		path      string
		wantMount string
		wantFS    string
	}{
		{"/", "/", "ext4"},
		{"/data", "/data", "ext4"},
		{"/data/mysql/data", "/data", "ext4"},
		{"/var/lib/docker/overlay2/6f0f/merged/etc", "/var/lib/docker/overlay2/6f0f/merged", "overlay"},
		{"/home/unknown", "/", "ext4"}, // 未知路径回退到根文件系统
	}
	for _, tc := range cases {
		got, ok := pickPrimaryMount(mounts, tc.path)
		if !ok {
			t.Fatalf("pickPrimaryMount(%q) 没找到挂载点", tc.path)
		}
		if got.mount != tc.wantMount || got.fs != tc.wantFS {
			t.Errorf("pickPrimaryMount(%q) = %s(%s)，期望 %s(%s)", tc.path, got.mount, got.fs, tc.wantMount, tc.wantFS)
		}
	}

	if _, ok := pickPrimaryMount(nil, "/"); ok {
		t.Error("空挂载表不应找到挂载点")
	}
}

func TestUnescapeMount(t *testing.T) {
	mounts := parseMounts(`/dev/sda1 /mnt/my\040disk ext4 rw 0 0`)
	if len(mounts) != 1 || mounts[0].mount != "/mnt/my disk" {
		t.Fatalf("八进制转义还原失败: %+v", mounts)
	}
}

func TestPickExtraMounts(t *testing.T) {
	mounts := parseMounts(readFixture(t, "proc/mounts"))
	extras := pickExtraMounts(mounts, "/", 7)
	if len(extras) != 1 || extras[0].mount != "/data" {
		t.Fatalf("附加磁盘 = %+v，期望只有 /data（tmpfs 与 overlay 被排除）", extras)
	}
	if got := pickExtraMounts(mounts, "/", 0); got != nil {
		t.Fatalf("limit<=0 时应返回空，实际 %+v", got)
	}
}
