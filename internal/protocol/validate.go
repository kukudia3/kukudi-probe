package protocol

import (
	"fmt"
	"math"
	"net"
	"strings"
)

// 校验上限。取值原则：够用即可，宁可拒绝可疑数据，也不让它进入数据库。
const (
	maxUint53        = uint64(1) << 53
	maxVersionLen    = 64
	maxHostnameLen   = 255
	maxOSFieldLen    = 128
	maxModelLen      = 128
	maxBootIDLen     = 64
	maxIfaceLen      = 32
	maxMACLen        = 32
	maxMountLen      = 128
	maxFilesystemLen = 32
	maxDiskEntries   = 8
	maxLoadValue     = 100000
	maxAgentCores    = 4096
	// maxIPLen 是 IP 文本的硬上限：IPv6 最长 45 字符（含 IPv4 映射写法），
	// 给到 64 已经足够宽松。先卡长度再 ParseIP，避免超长字符串白跑解析。
	maxIPLen = 64
)

// ValidateHello 校验 hello 负载。
func ValidateHello(h Hello) error {
	if err := checkLen("agent_version", h.AgentVersion, maxVersionLen, true); err != nil {
		return err
	}
	if err := checkLen("hostname", h.Hostname, maxHostnameLen, false); err != nil {
		return err
	}
	if err := checkLen("os.name", h.OS.Name, maxOSFieldLen, false); err != nil {
		return err
	}
	if err := checkLen("os.kernel", h.OS.Kernel, maxOSFieldLen, false); err != nil {
		return err
	}
	if err := checkLen("os.arch", h.OS.Arch, maxOSFieldLen, false); err != nil {
		return err
	}
	if err := checkLen("cpu.model", h.CPU.Model, maxModelLen, false); err != nil {
		return err
	}
	if h.CPU.Cores < 0 || h.CPU.Cores > maxAgentCores {
		return fmt.Errorf("cpu.cores %d 超出范围", h.CPU.Cores)
	}
	if err := checkLen("boot_id", h.BootID, maxBootIDLen, false); err != nil {
		return err
	}
	if h.UptimeSec >= maxUint53 {
		return fmt.Errorf("uptime_sec 过大")
	}
	if err := checkLen("iface.name", h.Iface.Name, maxIfaceLen, false); err != nil {
		return err
	}
	if h.Iface.IfIndex < 0 {
		return fmt.Errorf("iface.ifindex 不能为负")
	}
	if err := checkLen("iface.mac", h.Iface.MAC, maxMACLen, false); err != nil {
		return err
	}
	// 本机地址是可选字段：空串表示"这台机器取不到"，是正常情况（例如纯 IPv6
	// 或纯 IPv4 主机）。但一旦给了值就必须是**对应族**的合法 IP —— 把 IPv6 塞进
	// local_ip 会让服务端把它当 IPv4 展示，这种"看着有值其实错了"最难排查。
	if err := checkIP("local_ip", h.LocalIP, false); err != nil {
		return err
	}
	if err := checkIP("local_ip6", h.LocalIP6, true); err != nil {
		return err
	}
	if h.IntervalSec != 0 && (h.IntervalSec < MinIntervalSec || h.IntervalSec > MaxIntervalSec) {
		return fmt.Errorf("interval_sec %d 超出 %d-%d", h.IntervalSec, MinIntervalSec, MaxIntervalSec)
	}
	return nil
}

// ValidateMetrics 校验实时指标。
//
// 原则：任何一项不合法就整帧拒绝——半真半假的数据比没有数据更危险
// （例如把 0 当成真实的流量累计值，会让服务端重设流量基线）。
func ValidateMetrics(m Metrics) error {
	if err := checkPct("cpu_pct", m.CPUPct); err != nil {
		return err
	}
	if m.CPUCores < 0 || m.CPUCores > maxAgentCores {
		return fmt.Errorf("cpu_cores %d 超出范围", m.CPUCores)
	}
	if err := validateMem("mem", m.Mem); err != nil {
		return err
	}
	if err := validateMem("swap", m.Swap); err != nil {
		return err
	}
	if len(m.Disk) > maxDiskEntries {
		return fmt.Errorf("disk 条目 %d 超过上限 %d", len(m.Disk), maxDiskEntries)
	}
	for i, d := range m.Disk {
		if err := checkLen(fmt.Sprintf("disk[%d].mount", i), d.Mount, maxMountLen, true); err != nil {
			return err
		}
		if err := checkLen(fmt.Sprintf("disk[%d].fs", i), d.FS, maxFilesystemLen, false); err != nil {
			return err
		}
		if d.Used > d.Total && d.Total > 0 {
			return fmt.Errorf("disk[%d] 已用 %d 大于总量 %d", i, d.Used, d.Total)
		}
		if err := checkPct(fmt.Sprintf("disk[%d].pct", i), d.Pct); err != nil {
			return err
		}
	}
	if err := validateLoad(m.Load); err != nil {
		return err
	}
	if err := validateNet(m.Net); err != nil {
		return err
	}
	if err := checkFinite("lat_ms", m.LatMS, 0, 600000); err != nil {
		return err
	}
	if err := validatePings(m.Pings); err != nil {
		return err
	}
	if m.UptimeSec >= maxUint53 {
		return fmt.Errorf("uptime_sec 过大")
	}
	if m.Dropped >= maxUint53 || m.Gap >= maxUint53 {
		return fmt.Errorf("dropped/gap 过大")
	}
	return nil
}

// ValidateConfig 校验服务端下发的配置帧。
//
// 与 hello/metrics 一样严格：配置决定 Agent 接下来做什么（探谁、多久探一次），
// 半真半假的配置比不收更危险 —— 例如 interval_sec 被写成 0 会让上报循环空转。
// IntervalSec / PingIntervalSec 为 0 表示"本次不改"，是合法值。
func ValidateConfig(c Config) error {
	if c.ConfigVersion < 0 || uint64(c.ConfigVersion) >= maxUint53 {
		return fmt.Errorf("config_version %d 超出范围", c.ConfigVersion)
	}
	if c.IntervalSec != 0 && (c.IntervalSec < MinIntervalSec || c.IntervalSec > MaxIntervalSec) {
		return fmt.Errorf("interval_sec %d 超出 %d-%d", c.IntervalSec, MinIntervalSec, MaxIntervalSec)
	}
	if err := checkLen("iface", c.Iface, maxIfaceLen, false); err != nil {
		return err
	}
	if c.PingIntervalSec != 0 && (c.PingIntervalSec < MinPingIntervalSec || c.PingIntervalSec > MaxPingIntervalSec) {
		return fmt.Errorf("ping_interval_sec %d 超出 %d-%d",
			c.PingIntervalSec, MinPingIntervalSec, MaxPingIntervalSec)
	}
	if len(c.PingTargets) > MaxPingTargets {
		return fmt.Errorf("ping_targets 条目 %d 超过上限 %d", len(c.PingTargets), MaxPingTargets)
	}
	seen := make(map[int64]bool, len(c.PingTargets))
	for i, t := range c.PingTargets {
		name := fmt.Sprintf("ping_targets[%d]", i)
		if err := validatePingTarget(name, t); err != nil {
			return err
		}
		if seen[t.ID] {
			return fmt.Errorf("%s 的目标 ID %d 重复", name, t.ID)
		}
		seen[t.ID] = true
	}
	return nil
}

// validatePings 校验一帧里各目标的最新探测结果。
//
// 数值范围、有限性、条目上限都要挡：这些数字会直接进库并画成曲线，
// NaN/Inf 会让整条曲线上所有点变成非法 JSON（前端解析直接失败）。
func validatePings(pings []PingResult) error {
	if len(pings) > MaxPingTargets {
		return fmt.Errorf("pings 条目 %d 超过上限 %d", len(pings), MaxPingTargets)
	}
	seen := make(map[int64]bool, len(pings))
	for i, p := range pings {
		name := fmt.Sprintf("pings[%d]", i)
		if p.TargetID <= 0 {
			return fmt.Errorf("%s.target_id 必须是正整数", name)
		}
		if seen[p.TargetID] {
			return fmt.Errorf("%s 的目标 ID %d 重复", name, p.TargetID)
		}
		seen[p.TargetID] = true
		for _, v := range []struct {
			field string
			val   float64
		}{{"avg_ms", p.AvgMS}, {"min_ms", p.MinMS}, {"max_ms", p.MaxMS}} {
			if err := checkFinite(name+"."+v.field, v.val, 0, MaxPingMS); err != nil {
				return err
			}
		}
		if err := checkPct(name+".loss_pct", p.LossPct); err != nil {
			return err
		}
		// min <= avg <= max 是探测端算出这三个值的定义本身。留 1e-6 的容差是为了
		// 吸收浮点求和误差，不是允许数据乱来。
		if p.MinMS > p.MaxMS+1e-6 {
			return fmt.Errorf("%s 的 min_ms %v 大于 max_ms %v", name, p.MinMS, p.MaxMS)
		}
		if p.AvgMS < p.MinMS-1e-6 || p.AvgMS > p.MaxMS+1e-6 {
			return fmt.Errorf("%s 的 avg_ms %v 不在 [min_ms, max_ms] 之间", name, p.AvgMS)
		}
	}
	return nil
}

// validatePingTarget 校验一个探测目标。
func validatePingTarget(name string, t PingTarget) error {
	if t.ID <= 0 {
		return fmt.Errorf("%s.id 必须是正整数", name)
	}
	if !IsPingType(t.Type) {
		return fmt.Errorf("%s.type %q 不是 %s/%s", name, t.Type, PingTypeICMP, PingTypeTCP)
	}
	// 端口对 icmp 没有意义（设置侧的归一化会把它清零），但取值范围仍然要挡：
	// 越界的数字一旦流到 net.JoinHostPort，会变成一个看着像端口、其实不是的东西。
	if t.Port < 0 || t.Port > MaxPingPort {
		return fmt.Errorf("%s.port %d 超出 0-%d", name, t.Port, MaxPingPort)
	}
	if t.Type == PingTypeTCP && t.Port < 1 {
		return fmt.Errorf("%s.port 对 tcp 目标是必需的（1-%d）", name, MaxPingPort)
	}
	return checkPingHost(name+".host", t.Host)
}

// checkPingHost 校验探测目标的主机。
//
// 只挡"一眼就知道不可能解析成功"的输入（空白、控制字符、超长），不在这里做
// DNS 语法校验：解析失败会由探测侧如实记成丢包，那才是用户看得懂的反馈。
func checkPingHost(name, host string) error {
	if err := checkLen(name, host, MaxPingHostLen, true); err != nil {
		return err
	}
	if strings.TrimSpace(host) != host {
		return fmt.Errorf("%s 首尾不能有空白", name)
	}
	for _, r := range host {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%s 含空白或控制字符", name)
		}
	}
	return nil
}

func validateMem(name string, m Mem) error {
	if m.Total >= maxUint53 || m.Used >= maxUint53 {
		return fmt.Errorf("%s 数值过大", name)
	}
	if m.Used > m.Total {
		return fmt.Errorf("%s 已用 %d 大于总量 %d", name, m.Used, m.Total)
	}
	return checkPct(name+".pct", m.Pct)
}

func validateLoad(l Load) error {
	for _, v := range []struct {
		name string
		val  float64
	}{{"load.l1", l.L1}, {"load.l5", l.L5}, {"load.l15", l.L15}} {
		if err := checkFinite(v.name, v.val, 0, maxLoadValue); err != nil {
			return err
		}
	}
	return nil
}

func validateNet(n Net) error {
	if err := checkLen("net.iface", n.Iface, maxIfaceLen, true); err != nil {
		return err
	}
	if err := checkLen("net.boot_id", n.BootID, maxBootIDLen, false); err != nil {
		return err
	}
	if n.RxTotal >= maxUint53 || n.TxTotal >= maxUint53 || n.RxRaw >= maxUint53 || n.TxRaw >= maxUint53 {
		return fmt.Errorf("net 累计字节数过大")
	}
	if err := checkFinite("net.rx_rate", n.RxRate, 0, float64(maxUint53)); err != nil {
		return err
	}
	if err := checkFinite("net.tx_rate", n.TxRate, 0, float64(maxUint53)); err != nil {
		return err
	}
	if n.CkptAgeS < -1 || n.CkptAgeS > 86400*365 {
		return fmt.Errorf("net.ckpt_age_s %d 超出范围", n.CkptAgeS)
	}
	return nil
}

func checkLen(name, value string, limit int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s 不能为空", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s 长度 %d 超过上限 %d", name, len(value), limit)
	}
	return nil
}

func checkPct(name string, v float64) error {
	return checkFinite(name, v, 0, 100)
}

// checkIP 校验一个可选的 IP 文本。
//
// wantV6=false 时只接受 IPv4，true 时只接受 IPv6：校验的是"文本能不能解析成
// 该族的地址"，而不是"长得像不像"。空串一律放行（可选字段）。
func checkIP(name, value string, wantV6 bool) error {
	if value == "" {
		return nil
	}
	if err := checkLen(name, value, maxIPLen, false); err != nil {
		return err
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return fmt.Errorf("%s %q 不是合法 IP", name, value)
	}
	isV4 := ip.To4() != nil
	if wantV6 {
		// ParseIP 把 ::ffff:1.2.3.4 这种 IPv4 映射写法也归到 IPv4（To4() 非 nil），
		// 所以这里直接拒掉：写进 local_ip6 会被前端当成 IPv6 展示，看着有值其实错了。
		if isV4 {
			return fmt.Errorf("%s %q 不是 IPv6", name, value)
		}
		return nil
	}
	// IPv4 分支额外要求文本里没有冒号：带冒号的一定是 v6 写法（含映射写法），
	// 混进 local_ip 会让服务端按 IPv4 展示一个 v6 地址。
	if !isV4 || strings.Contains(value, ":") {
		return fmt.Errorf("%s %q 不是 IPv4", name, value)
	}
	return nil
}

// checkFinite 拒绝 NaN / ±Inf 与越界值。
func checkFinite(name string, v, min, max float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s 不是有限数值", name)
	}
	if v < min || v > max {
		return fmt.Errorf("%s = %v 超出 [%v, %v]", name, v, min, max)
	}
	return nil
}
