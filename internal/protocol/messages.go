// Package protocol 定义 Agent ↔ Server 的消息结构，两个二进制共用。
//
// 全量规格见 docs/PROTOCOL.md。规则：
//   - 只允许"加可选字段"的演进；删字段、改语义、改类型必须升 Version。
//   - 未知字段忽略（向前兼容），已知字段严格校验。
//   - 本包只描述数据；校验逻辑在服务端接收侧（Phase 4）。
package protocol

// Version 是当前协议主版本，对应信封里的 "v"。
const Version = 1

// Metrics 是每 interval 上报一次的实时指标（PROTOCOL.md §5.3）。
//
// 关键约定：net.rx_total / net.tx_total 是 Agent 自己维护的**长期单调累计**，
// 不是内核 counter；服务端用它与自己保存的基线做差，因此增量天然幂等
// （见 docs/DESIGN.md §9）。
type Metrics struct {
	CPUPct    float64 `json:"cpu_pct"`
	CPUCores  int     `json:"cpu_cores"`
	Mem       Mem     `json:"mem"`
	Swap      Mem     `json:"swap"`
	Disk      []Disk  `json:"disk"`
	Load      Load    `json:"load"`
	Net       Net     `json:"net"`
	LatMS     float64 `json:"lat_ms"`
	UptimeSec uint64  `json:"uptime_sec"`
	Dropped   uint64  `json:"dropped"`
	Gap       uint64  `json:"gap"`

	// Pings 是各探测目标**最近一次**的结果（见 PingResult 的语义说明）。
	//
	// 为什么放在每秒的 metrics 里，而不是另开一种帧：曲线要按时间轴画，必须带时间戳；
	// 复用 metrics 帧就自动继承了它已有的时间戳、序号、限流与落盘路径，
	// 不必再为"每隔一分钟才有的数据"设计一套新的传输与校验。
	Pings []PingResult `json:"pings,omitempty"`
}

// PingTarget 是一个延迟探测目标（服务端 → Agent 下发）。
//
// ID 由服务端分配，**创建后永不变更**：它是延迟曲线的身份标识，改名字、换标签、
// 甚至换探测方式都不能让历史断掉（前端靠 ID 把库里的历史点与当前配置对上）。
type PingTarget struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

// PingResult 是 Agent 对某个目标最近一次探测的结果。
//
// **语义（重要）**：探测按 ping_interval_sec 进行（默认 60 秒），而 metrics 是每秒一帧，
// 所以每一帧都带上"各目标最近一次的结果"—— 同一个值会连续出现在多帧里，这是**有意的**：
// 服务端按每分钟一行落盘，曲线因此呈阶梯状，前端不需要自己补点。
// 从未探到过结果的目标（例如 ICMP 没有权限而被留空）不出现在数组里。
type PingResult struct {
	TargetID int64   `json:"target_id"`
	AvgMS    float64 `json:"avg_ms"`
	MinMS    float64 `json:"min_ms"`
	MaxMS    float64 `json:"max_ms"`
	LossPct  float64 `json:"loss_pct"`
}

// IsPingType 判断探测方式是否合法（只有 ICMP 与 TCP 两种）。
func IsPingType(t string) bool {
	return t == PingTypeICMP || t == PingTypeTCP
}

// Mem 描述内存或 swap 的用量。
type Mem struct {
	Total uint64  `json:"total"`
	Used  uint64  `json:"used"`
	Pct   float64 `json:"pct"`
}

// Disk 描述一个文件系统。数组第一项是 --disk 指定的主文件系统。
type Disk struct {
	Mount string  `json:"mount"`
	FS    string  `json:"fs"`
	Total uint64  `json:"total"`
	Used  uint64  `json:"used"`
	Pct   float64 `json:"pct"`
}

// Load 是 1/5/15 分钟平均负载。
type Load struct {
	L1  float64 `json:"l1"`
	L5  float64 `json:"l5"`
	L15 float64 `json:"l15"`
}

// Net 描述被监控网卡的流量。
type Net struct {
	Iface string `json:"iface"`
	// RxTotal/TxTotal 是探针长期累计（权威值，服务端据此做增量）。
	RxTotal uint64 `json:"rx_total"`
	TxTotal uint64 `json:"tx_total"`
	// RxRaw/TxRaw 是内核 counter 快照，仅用于诊断与审计。
	RxRaw uint64 `json:"rx_raw"`
	TxRaw uint64 `json:"tx_raw"`
	// RxRate/TxRate 是本次采样的瞬时速率，单位 bytes/s。
	RxRate float64 `json:"rx_rate"`
	TxRate float64 `json:"tx_rate"`
	// BootID 变化说明机器重启过；CkptAgeS 是上次落盘距今的秒数（-1 = 未持久化）。
	BootID   string `json:"boot_id"`
	CkptAgeS int64  `json:"ckpt_age_s"`
}

// Info 是 Agent 连接后上报的静态信息（PROTOCOL.md §5.1 的 hello 负载）。
type Info struct {
	AgentVersion string    `json:"agent_version"`
	Hostname     string    `json:"hostname"`
	OS           OSInfo    `json:"os"`
	CPU          CPUInfo   `json:"cpu"`
	BootID       string    `json:"boot_id"`
	UptimeSec    uint64    `json:"uptime_sec"`
	Iface        IfaceInfo `json:"iface"`
}

// OSInfo 描述操作系统。
type OSInfo struct {
	Name   string `json:"name"`
	Kernel string `json:"kernel"`
	Arch   string `json:"arch"`
}

// CPUInfo 描述 CPU。
type CPUInfo struct {
	Model string `json:"model"`
	Cores int    `json:"cores"`
}

// IfaceInfo 是被监控网卡的身份信息：ifindex 与 MAC 变化说明网卡被重建过，
// 此时流量基线必须重设（docs/DESIGN.md §9.2）。
type IfaceInfo struct {
	Name    string `json:"name"`
	IfIndex int    `json:"ifindex"`
	MAC     string `json:"mac"`
}
