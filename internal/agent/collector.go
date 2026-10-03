package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"probe/internal/protocol"
	"probe/internal/version"
)

// bootIDTTL 是 boot_id 缓存的有效期。
//
// boot_id 在一次开机内恒定，本该按 Info() 的做法永久缓存；但 --root 允许把根指向
// "会跨越宿主重启的实时 /proc"（容器内监控宿主机），那种部署下永久缓存会漏掉重启
// 检测（流量基线不重设、多算一段）。60 秒的 TTL 让那种部署最迟一分钟后发现重启，
// 同时把日常的每秒一次文件读取降到每分钟一次。
const bootIDTTL = time.Minute

// Collector 采集一台机器的指标。
//
// 它是有状态的：CPU 使用率需要两次采样求差，网卡速率同理，
// 流量累计则来自 Traffic（本地 checkpoint）。
type Collector struct {
	root    string
	iface   string // 显式指定的网卡，空表示自动探测
	disk    string // 主文件系统路径
	traffic *Traffic

	// mu 保护下面这些跨拍状态与缓存。
	//
	// 为什么需要：读循环失败后 session 只等写循环 2 秒就返回（见 Client.session），
	// 被遗弃的旧写循环会与新写循环同时调用 Sample —— 那时两边并发读写
	// prev*/ifaceReady/infoLoaded，是真正的数据竞争。
	//
	// 只把**状态**放进锁里，statfs 这类 I/O 留在锁外：旧写循环卡在 statfs 时
	// 不该把新写循环一起拖住（那会让 Agent 彻底停止上报，比竞争更糟）。
	mu sync.Mutex

	info       protocol.Info
	infoLoaded bool

	// cachedBootID 是 proc/sys/kernel/random/boot_id 的缓存（TTL 见 bootIDTTL）。
	cachedBootID string
	bootIDAt     time.Time

	ifaceInfo  protocol.IfaceInfo
	ifaceReady bool

	prevCPU   cpuTimes
	hasCPU    bool
	prevNet   netCounters
	prevNetAt time.Time
	hasNet    bool
}

// New 构造采集器。traffic 为 nil 时使用仅内存的流量统计。
func New(root, iface, disk string, traffic *Traffic) *Collector {
	if traffic == nil {
		traffic = &Traffic{}
	}
	return &Collector{root: root, iface: iface, disk: disk, traffic: traffic}
}

func (c *Collector) path(parts ...string) string {
	return filepath.Join(append([]string{c.root}, parts...)...)
}

// Info 返回静态信息（首次读取后缓存）。
//
// 整个过程持锁：它只在首次调用时读文件（procfs/sysfs 都是内存文件系统），
// 之后永远命中缓存；持锁换来的是"两个 goroutine 同时首次调用"也不会各写一份。
func (c *Collector) Info() (protocol.Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.infoLoaded {
		return c.info, nil
	}
	info := protocol.Info{
		AgentVersion: version.Version,
		OS:           protocol.OSInfo{Arch: runtime.GOARCH},
	}
	if hostname, err := os.Hostname(); err == nil {
		info.Hostname = hostname
	}
	if kernel, err := readFileTrim(c.path("proc/sys/kernel/osrelease")); err == nil {
		info.OS.Kernel = kernel
	}
	if data, err := os.ReadFile(c.path("etc/os-release")); err == nil {
		info.OS.Name = parseOSRelease(string(data))
	}
	if data, err := os.ReadFile(c.path("proc/cpuinfo")); err == nil {
		info.CPU.Model, info.CPU.Cores = parseCPUInfo(string(data))
	}
	if uptime, err := c.readUptime(); err == nil {
		info.UptimeSec = uptime
	}
	info.BootID = c.bootIDLocked()
	if iface, err := c.resolveIfaceLocked(); err == nil {
		info.Iface = iface
	}
	c.info, c.infoLoaded = info, true
	return info, nil
}

// Sample 采集一次实时指标。
//
// 返回的 error 表示"这一拍没有可信数据"，上层应当直接跳过本次上报
// ——宁可少一个点，也不能把零值当成真实流量发给服务端（那会重设流量基线）。
// warnings 是非致命问题（某个可选文件读不到、磁盘统计失败等）。
func (c *Collector) Sample(now time.Time) (protocol.Metrics, []string, error) {
	var (
		m        protocol.Metrics
		warnings []string
	)

	// CPU：解析总量也需要它，属于必需项。
	statData, err := os.ReadFile(c.path("proc/stat"))
	if err != nil {
		return m, nil, fmt.Errorf("读取 /proc/stat 失败: %w", err)
	}
	cpu, err := parseCPUStat(firstLine(string(statData)))
	if err != nil {
		return m, nil, err
	}
	c.mu.Lock()
	if c.hasCPU {
		m.CPUPct = cpuUsage(c.prevCPU, cpu)
	}
	c.prevCPU, c.hasCPU = cpu, true
	c.mu.Unlock()

	// 内存与 swap：必需项。
	memData, err := os.ReadFile(c.path("proc/meminfo"))
	if err != nil {
		return m, nil, fmt.Errorf("读取 /proc/meminfo 失败: %w", err)
	}
	mi := parseMemInfo(string(memData))
	if mi.total == 0 {
		return m, nil, errors.New("/proc/meminfo 里没有 MemTotal，无法计算内存使用率")
	}
	m.Mem = protocol.Mem{Total: mi.total, Used: mi.used(), Pct: pct(mi.used(), mi.total)}
	if mi.swapTotal > 0 {
		// SwapFree > SwapTotal 是内核（以及 lxcfs 这类容器里的 /proc/meminfo 替身）会给出的
		// 坏值：直接相减会在 uint64 下溢成 1.8e19，validateMem 于是每一拍都拒掉整帧
		// （探针永久静默，且 raw 那样"重启也不会好"——见 _audit/ROUND5-RAWCOUNT.md §5 的 T3）。
		// 下溢时按"没有用到 swap"（0）处理。另一个候选是取 total（=100%，显示成 swap 用满）：
		// 那会凭空造出"swap 打爆"的显示、甚至触发告警，比 0 危险，所以不取它。
		swapUsed := uint64(0)
		if mi.swapFree <= mi.swapTotal {
			swapUsed = mi.swapTotal - mi.swapFree
		}
		m.Swap = protocol.Mem{Total: mi.swapTotal, Used: swapUsed, Pct: pct(swapUsed, mi.swapTotal)}
	}

	if info, err := c.Info(); err == nil {
		m.CPUCores = info.CPU.Cores
	}

	// 负载与运行时长：读不到只告警，不影响上报。
	if data, err := os.ReadFile(c.path("proc/loadavg")); err != nil {
		warnings = append(warnings, fmt.Sprintf("读取 /proc/loadavg 失败: %v", err))
	} else if load, err := parseLoadAvg(string(data)); err != nil {
		warnings = append(warnings, err.Error())
	} else {
		m.Load = load
	}
	if uptime, err := c.readUptime(); err != nil {
		warnings = append(warnings, err.Error())
	} else {
		m.UptimeSec = uptime
	}

	// 网卡流量：必需项，拿不到就不能上报（否则会污染服务端基线）。
	netData, err := os.ReadFile(c.path("proc/net/dev"))
	if err != nil {
		return m, warnings, fmt.Errorf("读取 /proc/net/dev 失败: %w", err)
	}
	ifaceInfo, err := c.resolveIface()
	if err != nil {
		return m, warnings, err
	}
	counters, ok := parseNetDev(string(netData), ifaceInfo.Name)
	if !ok {
		if c.iface != "" {
			return m, warnings, fmt.Errorf("指定的网卡 %s 不在 /proc/net/dev 中", ifaceInfo.Name)
		}
		warnings = append(warnings, fmt.Sprintf("网卡 %s 消失，重新探测", ifaceInfo.Name))
		c.invalidateIface()
		if ifaceInfo, err = c.resolveIface(); err != nil {
			return m, warnings, err
		}
		if counters, ok = parseNetDev(string(netData), ifaceInfo.Name); !ok {
			return m, warnings, fmt.Errorf("重新探测后的网卡 %s 仍不在 /proc/net/dev 中", ifaceInfo.Name)
		}
	}
	if ifaceInfo.IfIndex == 0 {
		warnings = append(warnings, fmt.Sprintf("读不到网卡 %s 的 ifindex（/sys 未挂载？），网卡重建检测会变弱", ifaceInfo.Name))
	}

	delta := c.traffic.Apply(ifaceInfo.Name, ifaceInfo.IfIndex, ifaceInfo.MAC, c.bootID(), counters.rx, counters.tx, now)
	cp := c.traffic.Checkpoint()
	m.Net = protocol.Net{
		Iface:    ifaceInfo.Name,
		RxTotal:  cp.TotalRx,
		TxTotal:  cp.TotalTx,
		RxRaw:    counters.rx,
		TxRaw:    counters.tx,
		BootID:   cp.BootID,
		CkptAgeS: c.traffic.CkptAge(now),
	}
	c.mu.Lock()
	if c.hasNet && delta.Reset == "" {
		if dt := now.Sub(c.prevNetAt).Seconds(); dt > 0 {
			m.Net.RxRate = float64(delta.Rx) / dt
			m.Net.TxRate = float64(delta.Tx) / dt
		}
	}
	c.prevNet, c.prevNetAt, c.hasNet = counters, now, true
	c.mu.Unlock()
	if delta.Reset != "" && delta.Reset != "init" {
		warnings = append(warnings, "流量基线已重设（"+delta.Reset+"）")
	}
	if _, err := c.traffic.MaybeSave(now, delta.Reset != ""); err != nil {
		warnings = append(warnings, err.Error())
	}

	// 磁盘：需要 statfs 系统调用，快照模式下跳过。
	if cleanSlash(c.root) == "/" {
		disks, warns := c.collectDisks()
		m.Disk = disks
		warnings = append(warnings, warns...)
	} else {
		warnings = append(warnings, "快照模式：跳过磁盘统计（statfs 只能作用于本机真实路径）")
	}

	return m, warnings, nil
}

func (c *Collector) readUptime() (uint64, error) {
	data, err := os.ReadFile(c.path("proc/uptime"))
	if err != nil {
		return 0, fmt.Errorf("读取 /proc/uptime 失败: %w", err)
	}
	return parseUptime(string(data))
}

// bootID 返回缓存过的 boot_id（TTL 见 bootIDTTL）。
//
// 为什么值得缓存：它是一次开机内的常量，而 Sample 每一拍都要用它做重启检测。
// 之前每拍都重读一次 /proc/sys/kernel/random/boot_id（一次 open+read+close、
// 一次分配与一次 string 转换），每台机器每秒一次、永久。
func (c *Collector) bootID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bootIDLocked()
}

// bootIDLocked 是 bootID 的实现，调用方必须持锁。
func (c *Collector) bootIDLocked() string {
	// 用 time.Since（带单调时钟读数）而不是 now.Sub：NTP 把墙钟往回拨时
	// 不会让 TTL 判定失效。
	if !c.bootIDAt.IsZero() && time.Since(c.bootIDAt) < bootIDTTL {
		return c.cachedBootID
	}
	id, err := readFileTrim(c.path("proc/sys/kernel/random/boot_id"))
	if err != nil {
		id = ""
	}
	c.cachedBootID, c.bootIDAt = id, time.Now()
	return id
}

// invalidateIface 让下一次 resolveIface 重新探测（网卡消失时用）。
func (c *Collector) invalidateIface() {
	c.mu.Lock()
	c.ifaceReady = false
	c.mu.Unlock()
}

// resolveIface 确定被监控的网卡并缓存它的身份信息（ifindex / MAC）。
func (c *Collector) resolveIface() (protocol.IfaceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolveIfaceLocked()
}

// resolveIfaceLocked 是 resolveIface 的实现，调用方必须持锁。
func (c *Collector) resolveIfaceLocked() (protocol.IfaceInfo, error) {
	if c.ifaceReady {
		return c.ifaceInfo, nil
	}
	netData, err := os.ReadFile(c.path("proc/net/dev"))
	if err != nil {
		return protocol.IfaceInfo{}, fmt.Errorf("读取 /proc/net/dev 失败: %w", err)
	}
	avail := listNetDevIfaces(string(netData))
	if len(avail) == 0 {
		return protocol.IfaceInfo{}, errors.New("/proc/net/dev 里没有任何网卡（只有 lo？），请用 --iface 指定")
	}

	name := c.iface
	if name == "" {
		name = c.autoIface(string(netData), avail)
	}
	if name == "" {
		return protocol.IfaceInfo{}, fmt.Errorf("无法自动探测网卡，请用 --iface 指定（可选：%s）", strings.Join(avail, ", "))
	}
	if !containsString(avail, name) {
		return protocol.IfaceInfo{}, fmt.Errorf("网卡 %s 不在 /proc/net/dev 中（可选：%s）", name, strings.Join(avail, ", "))
	}

	info := protocol.IfaceInfo{Name: name}
	if v, err := readFileTrim(c.path("sys/class/net", name, "ifindex")); err == nil {
		if n, err := strconv.Atoi(v); err == nil {
			info.IfIndex = n
		}
	}
	if v, err := readFileTrim(c.path("sys/class/net", name, "address")); err == nil {
		info.MAC = v
	}
	c.ifaceInfo, c.ifaceReady = info, true
	return info, nil
}

// autoIface 的探测顺序：IPv4 默认路由 → IPv6 默认路由 → 排除虚拟网卡后流量最大的一个。
func (c *Collector) autoIface(netData string, avail []string) string {
	if data, err := os.ReadFile(c.path("proc/net/route")); err == nil {
		if name := parseDefaultRoute(string(data)); name != "" && containsString(avail, name) {
			return name
		}
	}
	if data, err := os.ReadFile(c.path("proc/net/ipv6_route")); err == nil {
		if name := parseDefaultRouteV6(string(data)); name != "" && containsString(avail, name) {
			return name
		}
	}
	best, bestBytes := "", uint64(0)
	for _, name := range avail {
		if isVirtualIface(name) {
			continue
		}
		if state, err := readFileTrim(c.path("sys/class/net", name, "operstate")); err == nil && state != "up" {
			continue
		}
		if nc, ok := parseNetDev(netData, name); ok {
			if total := nc.rx + nc.tx; total > bestBytes {
				best, bestBytes = name, total
			}
		}
	}
	if best != "" {
		return best
	}
	// 兜底：放宽 operstate 限制，只排除最常见的虚拟网卡。
	for _, name := range avail {
		if strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") {
			continue
		}
		if nc, ok := parseNetDev(netData, name); ok {
			if total := nc.rx + nc.tx; total > bestBytes {
				best, bestBytes = name, total
			}
		}
	}
	return best
}

// collectDisks 返回主文件系统（--disk 所在）与若干真实磁盘的占用情况。
func (c *Collector) collectDisks() ([]protocol.Disk, []string) {
	data, err := os.ReadFile(c.path("proc/mounts"))
	if err != nil {
		return nil, []string{fmt.Sprintf("读取 /proc/mounts 失败: %v", err)}
	}
	return disksFromMounts(string(data), c.disk, statFS)
}

// disksFromMounts 是磁盘统计的纯函数部分：挂载表 + statfs 实现 → 磁盘列表。
// statfs 作为参数传入，便于用假实现做单元测试（生产代码里永远是 statFS）。
func disksFromMounts(mountsData, diskPath string, statfs func(string) (uint64, uint64, uint64, error)) ([]protocol.Disk, []string) {
	mounts := parseMounts(mountsData)
	primary, ok := pickPrimaryMount(mounts, diskPath)
	if !ok {
		return nil, []string{fmt.Sprintf("找不到包含 %s 的挂载点", diskPath)}
	}

	var warnings []string
	disks := make([]protocol.Disk, 0, 8)

	diskOf := func(entry mountEntry) (protocol.Disk, error) {
		total, used, avail, err := statfs(entry.mount)
		if err != nil {
			return protocol.Disk{}, fmt.Errorf("statfs %s 失败: %w", entry.mount, err)
		}
		return protocol.Disk{
			Mount: entry.mount,
			FS:    entry.fs,
			Total: total,
			Used:  used,
			Pct:   pct(used, used+avail),
		}, nil
	}

	if d, err := diskOf(primary); err != nil {
		warnings = append(warnings, err.Error())
	} else {
		disks = append(disks, d)
	}

	var extras []protocol.Disk
	for _, entry := range pickExtraMounts(mounts, primary.mount, 7) {
		d, err := diskOf(entry)
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		extras = append(extras, d)
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Used > extras[j].Used })
	return append(disks, extras...), warnings
}

func pct(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return clampPct(float64(part) / float64(whole) * 100)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// isVirtualIface 判断常见的虚拟网卡前缀，只在"自动探测兜底"时用来降低误选概率。
func isVirtualIface(name string) bool {
	for _, prefix := range []string{"veth", "docker", "br-", "virbr", "vmbr", "tap", "dummy"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
