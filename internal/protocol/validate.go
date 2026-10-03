package protocol

import (
	"fmt"
	"math"
	"net"
	"strings"
)

// 校验上限。取值原则：够用即可，宁可拒绝可疑数据，也不让它进入数据库。
const (
	maxUint53 = uint64(1) << 53
	// maxInt64 是"能写进数据库的无符号上界"，2^63。它的名字取的是"int64 计数器的模"
	// （不是 math.MaxInt64，差 1）：node_runtime.rx_raw/tx_raw 是 SQLite 的 INTEGER
	// （有符号 64 位），而 Go 侧是 uint64，database/sql 对高位为 1 的值直接报
	// "uint64 values with high bit set are not supported"，并让**整批**运行态落盘回滚
	// ——所以内核计数快照的界卡在 2^63-1，既不是 2^53，也不能是 MaxUint64。
	maxInt64      = uint64(1) << 63
	maxVersionLen = 64

	// maxTextLen 是"由被监控机器决定、我们无法裁剪"的那一类字符串字段的统一上限，
	// **单位是字节**（checkLen 用的是 len()，Go 的 string 长度就是 UTF-8 字节数）——
	// 不是字符数：一个汉字 3 字节、一个 emoji 4 字节，所以 512 字节 ≈ 170 个汉字。
	//
	// 这一类字段是：hostname、os.name / os.kernel / os.arch、cpu.model、iface.mac、
	// disk[].mount、disk[].fs。它们的值来自 uname / etc/os-release / proc/cpuinfo /
	// sysfs / proc/mounts，长度不受我们控制，也**不该由我们截断**（截断会改掉面板上
	// 显示的身份信息，并让"同一台机器"在不同地方名字不一致）。
	//
	// 旧上限（hostname 255、os.*/cpu.model 128、mount 128、mac/fs 32）对其中几个字段
	// 紧贴甚至低于现实可能长度（os.name 的 128 字节只够 42 个汉字；iface.mac 的 32 字节
	// 连 IPoIB 的 59 字符硬件地址都放不下；mount 的 128 字节挡不住长挂载路径），
	// 而超限的后果不是"丢一条数据"，是**这台探针永久没有数据**：hello 被 ValidateHello
	// 拒 ⇒ 服务端 4400 关闭 ⇒ Agent 无限退避重连（值来自机器本身、不会变，Agent 既不
	// 截断也不会自愈）；disk[].mount 超限则是每一拍 metrics 都被整帧拒掉
	// （与 _audit/ROUND5-UNDERFLOW.md §1-§3 同一类"在线但无数据"）。
	//
	// 为什么定 512：
	//   - 现实分布：PRETTY_NAME 与 ARM/设备树的 CPU model 都是厂商自由文本，规范上
	//     没有长度约束，128 字节（≈42 个汉字）余量太薄；512 把余量抬到它们之上。
	//   - 仍然有界：最坏情况下一个合法 metrics 帧（8 块盘、每块 mount+fs 都顶到上限、
	//     外加 16 个 ping 目标）约 10.6 KiB，仍小于单帧上限 MaxFrame = 16 KiB，
	//     所以放宽不会造出"校验通过却编不进一帧"的新形态 —— 帧上限仍是外层兜底，
	//     单个字段的界只是防止"一个字段吃掉整帧"。没有去掉上限，DoS 面不变。
	//   - 有内核硬上限的字段维持原值，它们本来就 ≥ 硬上限的 2 倍，不可能是这个坑：
	//     iface.name（IFNAMSIZ=16）/ net.iface、boot_id（36 字符 UUID）、
	//     local_ip·local_ip6（IPv6 文本最长 45）。
	maxTextLen = 512

	maxBootIDLen   = 64
	maxIfaceLen    = 32
	maxDiskEntries = 8
	maxLoadValue   = 100000
	maxAgentCores  = 4096
	// maxIPLen 是 IP 文本的硬上限：IPv6 最长 45 字符（含 IPv4 映射写法），
	// 给到 64 已经足够宽松。先卡长度再 ParseIP，避免超长字符串白跑解析。
	maxIPLen = 64
)

// ValidateHello 校验 hello 负载。
func ValidateHello(h Hello) error {
	if err := checkLen("agent_version", h.AgentVersion, maxVersionLen, true); err != nil {
		return err
	}
	if err := checkLen("hostname", h.Hostname, maxTextLen, false); err != nil {
		return err
	}
	if err := checkLen("os.name", h.OS.Name, maxTextLen, false); err != nil {
		return err
	}
	if err := checkLen("os.kernel", h.OS.Kernel, maxTextLen, false); err != nil {
		return err
	}
	if err := checkLen("os.arch", h.OS.Arch, maxTextLen, false); err != nil {
		return err
	}
	if err := checkLen("cpu.model", h.CPU.Model, maxTextLen, false); err != nil {
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
	if err := checkLen("iface.mac", h.Iface.MAC, maxTextLen, false); err != nil {
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
	// hello.state 是 Agent 自报的本地流量状态：服务端至今不消费它，但它是外部输入。
	//
	// 这里**只**校验两个累计字节数，口径与 validateNet 的 rx_total/tx_total 完全一致
	// （>= 2^53 拒绝）。越界的探针今天已经每一拍都被 Agent 自己的 ValidateMetrics 自查
	// 拦下（client.go 的 reportOnce）、永远没有数据进来，所以这里只是把"连上了但没数据"
	// 提前成"握手被拒"；不存在"今天工作正常的探针被拒掉"。
	//
	// **刻意不校验 ckpt_age_s**（它不是漏掉）：它的合法取值包含"陈旧 / 未来 / 缺失"
	// 三种 —— traffic.go 的 LoadTraffic 对 SavedAt <= 0 不报错、CkptAge 对未持久化返回
	// -1，都是有意容忍 —— 而唯一会刷新 savedAt 的那次采集
	// （collector.Sample → MaybeSave）发生在**握手之后**。握手是刷新之前的一道门，
	// 按 metrics 的口径拒掉它就会变成不可恢复的断连：
	//   - saved_at 陈旧 > 365 天（机器停了一年多）：年龄只增不减 ⇒ 永久连不上；
	//   - saved_at 缺失 / 为 0（旧 state.json）：年龄 ≈ 9.2e9 ⇒ 永久连不上；
	//   - saved_at 在未来（时钟回拨）：拒绝到墙钟追上为止（实测回拨 300 秒 ⇒ 掉线约 5 分钟）。
	// 这三种探针今天都能正常握手（第一拍 metrics 被自查拦下、savedAt 随即刷新，第二拍起
	// 一切正常），而没有任何消费者会因此受益。四个场景的实测见 _audit/ROUND5-B1.md。
	if h.State != nil && (h.State.TotalRx >= maxUint53 || h.State.TotalTx >= maxUint53) {
		return fmt.Errorf("state 累计字节数过大")
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
		if err := checkLen(fmt.Sprintf("disk[%d].mount", i), d.Mount, maxTextLen, true); err != nil {
			return err
		}
		if err := checkLen(fmt.Sprintf("disk[%d].fs", i), d.FS, maxTextLen, false); err != nil {
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
	if n.RxTotal >= maxUint53 || n.TxTotal >= maxUint53 {
		return fmt.Errorf("net 累计字节数过大")
	}
	// rx_raw/tx_raw 与 total 不同口径：它们是 /proc/net/dev 的 64 位内核计数快照，
	// 不进前端 DTO、不参与任何算术（消费者只有"拿 sqlite3 打开 probe.db 查这一列的人"，
	// 见 _audit/ROUND5-RAWCOUNT.md §2），所以"JSON 数字要能被 JS double 精确表示"这条
	// 2^53 规则对它们没有保护对象；它们的合法取值就是内核计数器的全域，真正的上界在
	// 存储层（见 maxInt64 的注释）。卡 2^53 的后果不是"丢精度"而是"探针永久静默"：
	// 一块累计搬过 9.007 PB 的网卡（10 Gbps 跑满约 83 天）每一拍都会被拒，且 raw 是每拍
	// 现读的内核值，重启探针也不会恢复（实测见 _audit/ROUND5-RAWCOUNT.md §4）。
	if n.RxRaw >= maxInt64 || n.TxRaw >= maxInt64 {
		return fmt.Errorf("net 累计字节数过大") // 刻意沿用同一句话：不改日志/错误文案
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

// checkLen 校验字符串长度。limit 的单位是**字节**（len()），不是字符数：
// 一个汉字 3 字节、一个 emoji 4 字节（见 maxTextLen 的注释）。
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
