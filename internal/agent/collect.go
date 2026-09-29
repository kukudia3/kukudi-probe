package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"probe/internal/protocol"
)

// 本文件只做"读文件 + 解析文本"，不含任何 Linux 专有系统调用，
// 因此可以在任何平台上对着 /proc 快照做单元测试。
// 唯一需要系统调用的磁盘容量在 disk_linux.go / disk_other.go 里。
//
// Collector 的 root 是一个"类 FHS 根目录"（默认 /）：
//
//	<root>/proc/...   进程与内核信息
//	<root>/sys/...    网卡身份（ifindex / MAC / operstate）
//	<root>/etc/os-release
//
// 把 root 指向一份快照，就能在别的机器上离线复现线上问题。

// cpuTimes 是 /proc/stat 里 CPU 的累计时间片（单位 jiffies）。
type cpuTimes struct {
	busy  uint64
	total uint64
}

// readFileTrim 读取一个小文件并去掉首尾空白。
func readFileTrim(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// parseCPUStat 解析 /proc/stat 的 "cpu" 汇总行。
//
// 字段依次是 user nice system idle iowait irq softirq steal guest guest_nice；
// guest 已经包含在 user 里，因此只累加前 8 个，避免重复计数。
// busy 不包含 idle 与 iowait：等待 IO 的时间不算 CPU 忙。
func parseCPUStat(line string) (cpuTimes, error) {
	fields := strings.Fields(line)
	if len(fields) < 9 || fields[0] != "cpu" {
		return cpuTimes{}, fmt.Errorf("无法解析 /proc/stat 的首行: %q", line)
	}
	var vals [8]uint64
	for i := 0; i < 8; i++ {
		v, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return cpuTimes{}, fmt.Errorf("解析 /proc/stat 字段 %d 失败: %w", i, err)
		}
		vals[i] = v
	}
	var total uint64
	for _, v := range vals {
		total += v
	}
	idle := vals[3] + vals[4] // idle + iowait
	if idle > total {
		return cpuTimes{}, fmt.Errorf("/proc/stat 数值异常：idle+iowait 大于总和")
	}
	return cpuTimes{busy: total - idle, total: total}, nil
}

// cpuUsage 计算两次采样之间的 CPU 使用率（百分比，0-100）。
func cpuUsage(prev, cur cpuTimes) float64 {
	if cur.total <= prev.total {
		return 0
	}
	totalDelta := float64(cur.total - prev.total)
	busyDelta := float64(0)
	if cur.busy > prev.busy {
		busyDelta = float64(cur.busy - prev.busy)
	}
	pct := busyDelta / totalDelta * 100
	return clampPct(pct)
}

func clampPct(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

// memInfo 是 /proc/meminfo 里我们关心的字段（字节）。
type memInfo struct {
	total     uint64
	free      uint64
	available uint64
	buffers   uint64
	cached    uint64
	swapTotal uint64
	swapFree  uint64
}

// parseMemInfo 解析 /proc/meminfo。数值单位通常是 kB，按字节换算。
func parseMemInfo(data string) memInfo {
	var mi memInfo
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		unit := uint64(1)
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			unit = 1024
		}
		bytes := n * unit
		switch key {
		case "MemTotal":
			mi.total = bytes
		case "MemFree":
			mi.free = bytes
		case "MemAvailable":
			mi.available = bytes
		case "Buffers":
			mi.buffers = bytes
		case "Cached":
			mi.cached = bytes
		case "SwapTotal":
			mi.swapTotal = bytes
		case "SwapFree":
			mi.swapFree = bytes
		}
	}
	return mi
}

// used 返回已用内存：优先用 MemAvailable（内核估算的可用量），
// 老内核没有该字段时退回 MemFree+Buffers+Cached。
func (mi memInfo) used() uint64 {
	if mi.available > 0 && mi.available <= mi.total {
		return mi.total - mi.available
	}
	reclaimable := mi.free + mi.buffers + mi.cached
	if reclaimable >= mi.total {
		return 0
	}
	return mi.total - reclaimable
}

// parseLoadAvg 解析 /proc/loadavg 的前三个数。
func parseLoadAvg(data string) (protocol.Load, error) {
	fields := strings.Fields(data)
	if len(fields) < 3 {
		return protocol.Load{}, fmt.Errorf("无法解析 /proc/loadavg: %q", strings.TrimSpace(data))
	}
	var load protocol.Load
	targets := []*float64{&load.L1, &load.L5, &load.L15}
	for i, p := range targets {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return protocol.Load{}, fmt.Errorf("解析 /proc/loadavg 字段 %d 失败: %w", i, err)
		}
		*p = v
	}
	return load, nil
}

// parseUptime 解析 /proc/uptime 的秒数。
func parseUptime(data string) (uint64, error) {
	fields := strings.Fields(data)
	if len(fields) == 0 {
		return 0, fmt.Errorf("无法解析 /proc/uptime")
	}
	sec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || sec < 0 {
		return 0, fmt.Errorf("解析 /proc/uptime 失败: %q", fields[0])
	}
	return uint64(sec), nil
}

// netCounters 是单个网卡的内核累计字节数。
type netCounters struct {
	rx uint64
	tx uint64
}

// parseNetDev 从 /proc/net/dev 中取出指定网卡的累计字节数。
func parseNetDev(data, iface string) (netCounters, bool) {
	for _, line := range strings.Split(data, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(name) != iface {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			return netCounters{}, false
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			return netCounters{}, false
		}
		return netCounters{rx: rx, tx: tx}, true
	}
	return netCounters{}, false
}

// listNetDevIfaces 返回 /proc/net/dev 里的所有网卡名（去掉 lo）。
func listNetDevIfaces(data string) []string {
	var names []string
	for _, line := range strings.Split(data, "\n") {
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" || strings.Contains(name, "|") {
			continue
		}
		names = append(names, name)
	}
	return names
}

// parseDefaultRoute 从 /proc/net/route 里找出默认路由的网卡（IPv4）。
func parseDefaultRoute(data string) string {
	for i, line := range strings.Split(data, "\n") {
		if i == 0 {
			continue // 表头
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		// Destination 与 Mask 全 0 且接口已启用（Flags 含 RTF_UP）即为默认路由。
		if fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x1 == 0 {
			continue
		}
		if fields[0] != "" {
			return fields[0]
		}
	}
	return ""
}

// parseDefaultRouteV6 从 /proc/net/ipv6_route 里找出默认路由的网卡。
func parseDefaultRouteV6(data string) string {
	const zero = "00000000000000000000000000000000"
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[0] != zero || fields[1] != "00" {
			continue
		}
		return fields[9]
	}
	return ""
}

// parseCPUInfo 返回 CPU 型号与核心数。
func parseCPUInfo(data string) (model string, cores int) {
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "processor" {
			cores++
			continue
		}
		if model != "" {
			continue
		}
		switch key {
		case "model name", "Model", "Hardware", "cpu model", "Processor":
			model = value
		}
	}
	return model, cores
}

// parseOSRelease 从 /etc/os-release 里取 PRETTY_NAME（没有则取 NAME）。
func parseOSRelease(data string) string {
	var name string
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "PRETTY_NAME":
			if value != "" {
				return value
			}
		case "NAME":
			if name == "" {
				name = value
			}
		}
	}
	return name
}

// mountEntry 是 /proc/mounts 的一行。
type mountEntry struct {
	source string
	mount  string
	fs     string
}

// parseMounts 解析 /proc/mounts，并把 \040 之类的八进制转义还原。
func parseMounts(data string) []mountEntry {
	var entries []mountEntry
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		entries = append(entries, mountEntry{
			source: unescapeMount(fields[0]),
			mount:  unescapeMount(fields[1]),
			fs:     fields[2],
		})
	}
	return entries
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// pickPrimaryMount 找出包含指定路径的文件系统（最长前缀匹配）。
//
// 这里用 path 而不是 filepath：挂载点永远是斜杠分隔的 Linux 路径，
// 而 filepath 会按运行平台的分隔符解释（在 Windows 上会把 "/" 变成 "\"）。
func pickPrimaryMount(mounts []mountEntry, diskPath string) (mountEntry, bool) {
	clean := cleanSlash(diskPath)
	best := mountEntry{}
	found := false
	for _, m := range mounts {
		mp := cleanSlash(m.mount)
		if clean != mp && !strings.HasPrefix(clean, strings.TrimSuffix(mp, "/")+"/") {
			continue
		}
		if !found || len(mp) > len(cleanSlash(best.mount)) {
			best = m
			found = true
		}
	}
	return best, found
}

// cleanSlash 用斜杠语义规范化路径（与运行平台无关）。
func cleanSlash(p string) string {
	return path.Clean(filepath.ToSlash(p))
}

// realFS 是"真实磁盘文件系统"白名单：只用于挑选详情页里的附加磁盘，
// 主文件系统由 --disk 决定，不受此表影响（容器里的 overlay 也能正常显示）。
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"f2fs": true, "zfs": true, "jfs": true, "reiserfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
}

// pickExtraMounts 返回除主文件系统外的其它真实磁盘，最多 limit 个。
func pickExtraMounts(mounts []mountEntry, primary string, limit int) []mountEntry {
	if limit <= 0 {
		return nil
	}
	var extras []mountEntry
	seen := map[string]bool{}
	for _, m := range mounts {
		if m.mount == primary || seen[m.mount] {
			continue
		}
		if !realFS[m.fs] || !strings.HasPrefix(m.source, "/dev/") {
			continue
		}
		seen[m.mount] = true
		extras = append(extras, m)
		if len(extras) >= limit {
			break
		}
	}
	return extras
}
