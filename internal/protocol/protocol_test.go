package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	frame, err := New(TypeMetrics, Metrics{CPUPct: 12.5, Net: Net{Iface: "eth0", RxTotal: 123}})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	frame.Seq = 7
	frame.TS = 1_700_000_000

	data, err := frame.Encode()
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	if got.V != Version || got.T != TypeMetrics || got.Seq != 7 || got.TS != 1_700_000_000 {
		t.Fatalf("信封内容不一致: %+v", got)
	}
	var m Metrics
	if err := got.Bind(&m); err != nil {
		t.Fatalf("绑定负载: %v", err)
	}
	if m.CPUPct != 12.5 || m.Net.Iface != "eth0" || m.Net.RxTotal != 123 {
		t.Fatalf("负载内容不一致: %+v", m)
	}
}

func TestEnvelopeIgnoresUnknownFields(t *testing.T) {
	raw := []byte(`{"v":1,"t":"metrics","future_field":1,"d":{"cpu_pct":1,"next_version_field":"x"}}`)
	env, err := Decode(raw)
	if err != nil {
		t.Fatalf("未知字段必须被忽略: %v", err)
	}
	var m Metrics
	if err := env.Bind(&m); err != nil {
		t.Fatalf("绑定: %v", err)
	}
	if m.CPUPct != 1 {
		t.Fatalf("cpu_pct = %v", m.CPUPct)
	}
}

func TestDecodeRejectsBadFrames(t *testing.T) {
	isVersionMismatch := func(err error) bool { return errors.Is(err, ErrVersionMismatch) }
	cases := []struct {
		name  string
		raw   []byte
		check func(error) bool
	}{
		{"不是 JSON", []byte("{"), nil},
		{"缺少版本", []byte(`{"t":"metrics"}`), isVersionMismatch},
		{"版本过高", []byte(`{"v":99,"t":"metrics"}`), isVersionMismatch},
		{"缺少类型", []byte(`{"v":1}`), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.raw)
			if err == nil {
				t.Fatal("应当报错")
			}
			if tc.check != nil && !tc.check(err) {
				t.Fatalf("错误类型不符: %v", err)
			}
		})
	}
}

func TestDecodeRejectsOversizeFrame(t *testing.T) {
	if _, err := Decode(bytes.Repeat([]byte("a"), MaxFrame+1)); err == nil {
		t.Fatal("超过帧上限应当报错")
	}
}

func TestEncodeRejectsOversizePayload(t *testing.T) {
	frame, err := New(TypeHello, Hello{Hostname: strings.Repeat("x", MaxFrame)})
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	if _, err := frame.Encode(); err == nil {
		t.Fatal("编码超过帧上限应当报错")
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	env := ErrorEnvelope(CodeUnauthorized, "token 无效", true)
	data, err := env.Encode()
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	var decoded struct {
		T string `json:"t"`
		D struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Fatal   bool   `json:"fatal"`
		} `json:"d"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if decoded.T != TypeError || decoded.D.Code != CodeUnauthorized || !decoded.D.Fatal {
		t.Fatalf("错误帧结构不符: %s", data)
	}
}

// validMetrics 返回一份可通过校验的指标，测试里按需改坏其中一项。
func validMetrics() Metrics {
	return Metrics{
		CPUPct:    12.5,
		CPUCores:  4,
		Mem:       Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Swap:      Mem{Total: 1 << 28, Used: 0, Pct: 0},
		Disk:      []Disk{{Mount: "/", FS: "ext4", Total: 1 << 40, Used: 1 << 39, Pct: 50}},
		Load:      Load{L1: 0.5, L5: 0.4, L15: 0.3},
		Net:       Net{Iface: "eth0", RxTotal: 1 << 40, TxTotal: 1 << 39, RxRaw: 1 << 41, TxRaw: 1 << 40, RxRate: 1024, TxRate: 512, BootID: "abc", CkptAgeS: 3},
		LatMS:     23.4,
		UptimeSec: 123456,
	}
}

func TestValidateMetricsAcceptsGoodPayload(t *testing.T) {
	if err := ValidateMetrics(validMetrics()); err != nil {
		t.Fatalf("合法指标被拒绝: %v", err)
	}
	// 没有测量到延迟、没有磁盘、没有 swap 也应当接受。
	m := validMetrics()
	m.LatMS = 0
	m.Disk = nil
	m.Swap = Mem{}
	if err := ValidateMetrics(m); err != nil {
		t.Fatalf("边界情况被拒绝: %v", err)
	}
}

func TestValidateMetricsRejectsBadPayload(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Metrics)
	}{
		{"CPU 百分比越界", func(m *Metrics) { m.CPUPct = 101 }},
		{"CPU 为 NaN", func(m *Metrics) { m.CPUPct = math.NaN() }},
		{"CPU 为 Inf", func(m *Metrics) { m.CPUPct = math.Inf(1) }},
		{"CPU 核心数异常", func(m *Metrics) { m.CPUCores = 99999 }},
		{"内存已用大于总量", func(m *Metrics) { m.Mem.Used = m.Mem.Total + 1 }},
		{"内存百分比越界", func(m *Metrics) { m.Mem.Pct = -1 }},
		{"磁盘条目过多", func(m *Metrics) { m.Disk = make([]Disk, maxDiskEntries+1) }},
		{"磁盘挂载点为空", func(m *Metrics) { m.Disk[0].Mount = "" }},
		{"磁盘已用大于总量", func(m *Metrics) { m.Disk[0].Used = m.Disk[0].Total + 1 }},
		// disk_linux.go 的 used = total - Bfree*bsize 在 Bfree > Blocks 时的下溢量级
		// （2^64-4096）：钉住"下溢产物必然被整帧拒绝"，这就是采集侧必须兜住它的理由。
		{"磁盘下溢产物（2^64-4096）", func(m *Metrics) { m.Disk[0].Used = math.MaxUint64 - 4095 }},
		{"负载为 NaN", func(m *Metrics) { m.Load.L1 = math.NaN() }},
		{"负载过大", func(m *Metrics) { m.Load.L15 = 1e9 }},
		{"网卡名为空", func(m *Metrics) { m.Net.Iface = "" }},
		{"网卡名过长", func(m *Metrics) { m.Net.Iface = strings.Repeat("e", maxIfaceLen+1) }},
		{"累计流量过大", func(m *Metrics) { m.Net.RxTotal = 1 << 53 }},
		// raw 是内核计数快照，界与 total 不同：卡在存储层能承载的 2^63。
		{"raw rx 到达 2^63", func(m *Metrics) { m.Net.RxRaw = 1 << 63 }},
		{"raw tx 超过 2^63", func(m *Metrics) { m.Net.TxRaw = math.MaxUint64 }},
		{"速率为负", func(m *Metrics) { m.Net.RxRate = -1 }},
		{"速率不是有限值", func(m *Metrics) { m.Net.TxRate = math.Inf(-1) }},
		{"checkpoint 时间异常", func(m *Metrics) { m.Net.CkptAgeS = -100 }},
		{"延迟越界", func(m *Metrics) { m.LatMS = 600001 }},
		{"运行时长过大", func(m *Metrics) { m.UptimeSec = 1 << 53 }},
		{"dropped 过大", func(m *Metrics) { m.Dropped = 1 << 53 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMetrics()
			tc.mutate(&m)
			if err := ValidateMetrics(m); err == nil {
				t.Fatal("非法指标应当被拒绝")
			}
		})
	}
}

// TestValidateMetricsRawKernelCounterBounds 钉住 rx_raw/tx_raw 与 rx_total/tx_total
// 的**两套**边界，两边都要钉：只钉"2^53 放行"会漏掉"谁把界放回 2^53"的回退，只钉
// "2^63 拒绝"会漏掉"谁把界抬到 MaxUint64"的回退（后者会让 ≥2^63 的探针把
// UpsertRuntime 的整批事务打回滚，实测见 _audit/ROUND5-RAWCOUNT.md §3.2）。
//
// 口径（见 validateNet 的注释与 docs/PROTOCOL.md §3 的例外说明）：
//   - raw：内核 64 位计数快照，只作诊断，界 = 存储层能承载的 2^63；
//   - total：Agent 自己统计、会进前端，界 = JS 安全整数 2^53，**不随 raw 放宽**。
func TestValidateMetricsRawKernelCounterBounds(t *testing.T) {
	accepted := []struct {
		name   string
		mutate func(*Metrics)
	}{
		{"raw rx 恰好 2^53-1", func(m *Metrics) { m.Net.RxRaw = 1<<53 - 1 }},
		{"raw rx 恰好 2^53（今天被拒的那个）", func(m *Metrics) { m.Net.RxRaw = 1 << 53 }},
		{"raw rx 2^63-1（SQLite INTEGER 的上限）", func(m *Metrics) { m.Net.RxRaw = math.MaxInt64 }},
		{"raw tx 2^63-1", func(m *Metrics) { m.Net.TxRaw = math.MaxInt64 }},
		{"total 刚好低于 2^53", func(m *Metrics) { m.Net.RxTotal = 1<<53 - 1; m.Net.TxTotal = 1<<53 - 1 }},
	}
	for _, tc := range accepted {
		t.Run("放行 "+tc.name, func(t *testing.T) {
			m := validMetrics()
			tc.mutate(&m)
			if err := ValidateMetrics(m); err != nil {
				t.Fatalf("必须放行（口径见 validateNet 的注释）: %v", err)
			}
		})
	}

	rejected := []struct {
		name   string
		mutate func(*Metrics)
	}{
		{"raw rx 到达 2^63", func(m *Metrics) { m.Net.RxRaw = 1 << 63 }},
		{"raw tx 到达 2^63", func(m *Metrics) { m.Net.TxRaw = 1 << 63 }},
		{"raw rx 为 MaxUint64", func(m *Metrics) { m.Net.RxRaw = math.MaxUint64 }},
		{"total rx 到达 2^53（不随 raw 放宽）", func(m *Metrics) { m.Net.RxTotal = 1 << 53 }},
		{"total tx 到达 2^53（不随 raw 放宽）", func(m *Metrics) { m.Net.TxTotal = 1 << 53 }},
	}
	for _, tc := range rejected {
		t.Run("拒绝 "+tc.name, func(t *testing.T) {
			m := validMetrics()
			tc.mutate(&m)
			if err := ValidateMetrics(m); err == nil {
				t.Fatal("越界值应当被拒绝")
			}
		})
	}
}

// TestValidateMetricsMachineTextBounds 钉住 metrics 里"机器自报文本"
// （disk[].mount / disk[].fs）的长度边界，规则见 maxTextLen 的注释。
//
// 这两个字段的值来自 /proc/mounts（挂载点与文件系统类型都由机器/文件系统决定），
// 而它们超限的后果是**每一拍整帧被拒**：这台探针会一直"在线但没有数据"
// （与 _audit/ROUND5-UNDERFLOW.md §1-§3 的 raw/swap/disk 下溢同一类）。
//
// 两边都要钉：旧上限（128 / 32）仍然放行证明这是**纯放宽**，新上限 +1 拒绝证明
// 上限还在（没有变成 DoS 面）。
func TestValidateMetricsMachineTextBounds(t *testing.T) {
	mountOld := "/" + strings.Repeat("m", 127) // 128 字节：旧上限
	mountNew := "/" + strings.Repeat("m", 511) // 512 字节：新上限
	fsNew := strings.Repeat("f", maxTextLen)

	cases := []struct {
		name   string
		mutate func(*Metrics)
		ok     bool
	}{
		// 旧上限仍然放行（以前能过的值一个没变）。
		{"mount 128 字节（旧上限）", func(m *Metrics) { m.Disk[0].Mount = mountOld }, true},
		{"fs 32 字节（旧上限）", func(m *Metrics) { m.Disk[0].FS = strings.Repeat("f", 32) }, true},
		// 旧上限 +1 今天被拒，放宽后必须接受。
		{"mount 129 字节（旧上限 +1，今天被拒）", func(m *Metrics) { m.Disk[0].Mount = mountOld + "m" }, true},
		{"fs 33 字节（旧上限 +1，今天被拒）", func(m *Metrics) { m.Disk[0].FS = strings.Repeat("f", 33) }, true},
		// 新上限与 +1。
		{"mount 恰好 512 字节（新上限）", func(m *Metrics) { m.Disk[0].Mount = mountNew }, true},
		{"mount 513 字节（新上限 +1）", func(m *Metrics) { m.Disk[0].Mount = mountNew + "m" }, false},
		{"fs 恰好 512 字节（新上限）", func(m *Metrics) { m.Disk[0].FS = fsNew }, true},
		{"fs 513 字节（新上限 +1）", func(m *Metrics) { m.Disk[0].FS = fsNew + "f" }, false},
		// 多字节：长度按**字节**算，不按 rune 算（200 个汉字 = 600 字节，只有 200 rune）。
		{"挂载点 170 个汉字（510 字节）", func(m *Metrics) { m.Disk[0].Mount = strings.Repeat("挂", 170) }, true},
		{"挂载点 200 个汉字（600 字节 / 200 rune）", func(m *Metrics) { m.Disk[0].Mount = strings.Repeat("挂", 200) }, false},
		// 既有行为不变：mount 是必填字段，空串照样拒绝。
		{"mount 空串（必填，既有行为）", func(m *Metrics) { m.Disk[0].Mount = "" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMetrics()
			tc.mutate(&m)
			err := ValidateMetrics(m)
			if tc.ok && err != nil {
				t.Fatalf("必须放行（口径见 maxTextLen 的注释）: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("超长/非法值应当被拒绝")
			}
		})
	}
}

// TestMachineTextWorstCaseFitsOneFrame 钉住"放宽不会造出校验通过却编不进一帧"这个
// 前提（maxTextLen 的注释里那段算术）：8 块盘每块 mount 与 fs 都顶到新上限、
// 外加满员的 16 个 ping 目标，ValidateMetrics 必须放行、Encode 必须成功。
//
// 帧上限 MaxFrame=16 KiB 是外层兜底，单字段上限只负责"别让一个字段吃掉整帧"；
// 如果哪天有人把 maxTextLen 抬到 4096 这种量级，这条会红 —— 那时就不是"纯放宽"了，
// 而是新造出"校验过了却发不出去"的形态（探针同样会静默）。
func TestMachineTextWorstCaseFitsOneFrame(t *testing.T) {
	m := validMetrics()
	m.Disk = make([]Disk, maxDiskEntries)
	for i := range m.Disk {
		m.Disk[i] = Disk{
			Mount: "/" + strings.Repeat("m", maxTextLen-1),
			FS:    strings.Repeat("f", maxTextLen),
			Total: 1 << 40,
			Used:  1 << 39,
			Pct:   50,
		}
	}
	m.Pings = make([]PingResult, MaxPingTargets)
	for i := range m.Pings {
		m.Pings[i] = PingResult{TargetID: int64(i + 1), AvgMS: MaxPingMS, MinMS: MaxPingMS, MaxMS: MaxPingMS, LossPct: 100}
	}
	if err := ValidateMetrics(m); err != nil {
		t.Fatalf("顶到上限的合法指标必须放行: %v", err)
	}
	frame, err := New(TypeMetrics, m)
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	raw, err := frame.Encode()
	if err != nil {
		t.Fatalf("顶到上限的一帧必须编得进 MaxFrame=%d: %v", MaxFrame, err)
	}
	t.Logf("最坏情况帧大小 = %d 字节（上限 %d）", len(raw), MaxFrame)

	// hello 侧同样的道理：所有机器自报字段都顶到上限时也要编得进一帧。
	text := strings.Repeat("t", maxTextLen)
	h := Hello{
		AgentVersion: strings.Repeat("v", maxVersionLen),
		Hostname:     text,
		OS:           OSInfo{Name: text, Kernel: text, Arch: text},
		CPU:          CPUInfo{Model: text, Cores: maxAgentCores},
		BootID:       strings.Repeat("b", maxBootIDLen),
		Iface:        IfaceInfo{Name: strings.Repeat("e", maxIfaceLen), IfIndex: 2, MAC: text},
		IntervalSec:  1,
		LocalIP:      "203.0.113.5",
		LocalIP6:     "2001:db8::1",
		State:        &AgentStat{CkptAgeS: -1, TotalRx: maxUint53 - 1, TotalTx: maxUint53 - 1},
	}
	if err := ValidateHello(h); err != nil {
		t.Fatalf("顶到上限的合法 hello 必须放行: %v", err)
	}
	hFrame, err := New(TypeHello, h)
	if err != nil {
		t.Fatalf("构造 hello 帧: %v", err)
	}
	hRaw, err := hFrame.Encode()
	if err != nil {
		t.Fatalf("顶到上限的 hello 必须编得进 MaxFrame=%d: %v", MaxFrame, err)
	}
	t.Logf("最坏情况 hello 帧大小 = %d 字节（上限 %d）", len(hRaw), MaxFrame)
}

func TestValidateHello(t *testing.T) {
	good := Hello{
		AgentVersion: "0.1.0",
		Hostname:     "hk-01",
		OS:           OSInfo{Name: "Debian", Kernel: "6.1.0", Arch: "amd64"},
		CPU:          CPUInfo{Model: "Xeon", Cores: 4},
		BootID:       "boot",
		Iface:        IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		IntervalSec:  1,
		LocalIP:      "203.0.113.5",
		LocalIP6:     "2001:db8::1",
	}
	if err := ValidateHello(good); err != nil {
		t.Fatalf("合法 hello 被拒绝: %v", err)
	}

	bad := []struct {
		name   string
		mutate func(*Hello)
	}{
		{"缺少 Agent 版本", func(h *Hello) { h.AgentVersion = "" }},
		{"主机名过长", func(h *Hello) { h.Hostname = strings.Repeat("h", maxTextLen+1) }},
		{"核心数异常", func(h *Hello) { h.CPU.Cores = 99999 }},
		{"间隔越界", func(h *Hello) { h.IntervalSec = 301 }},
		{"网卡名为空且必填", func(h *Hello) { h.Iface.Name = strings.Repeat("e", maxIfaceLen+1) }},
		{"ifindex 为负", func(h *Hello) { h.Iface.IfIndex = -1 }},
		{"本机 IPv4 不是 IP", func(h *Hello) { h.LocalIP = "not-an-ip" }},
		{"本机 IPv4 塞了 IPv6", func(h *Hello) { h.LocalIP = "2001:db8::1" }},
		{"本机 IPv4 超出长度上限", func(h *Hello) { h.LocalIP = strings.Repeat("1", maxIPLen+1) }},
		{"本机 IPv6 不是 IP", func(h *Hello) { h.LocalIP6 = "10.0.0.1/8" }},
		{"本机 IPv6 塞了 IPv4", func(h *Hello) { h.LocalIP6 = "203.0.113.5" }},
		{"本机 IPv6 是 IPv4 映射写法", func(h *Hello) { h.LocalIP6 = "::ffff:203.0.113.5" }},
		{"state 累计 rx 到达上限", func(h *Hello) { h.State = &AgentStat{TotalRx: maxUint53} }},
		{"state 累计 tx 超过上限", func(h *Hello) { h.State = &AgentStat{TotalTx: maxUint53 + 1} }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			h := good
			tc.mutate(&h)
			if err := ValidateHello(h); err == nil {
				t.Fatal("非法 hello 应当被拒绝")
			}
		})
	}
}

// TestValidateHelloMachineTextBounds 钉住 hello 里"机器自报文本"的统一长度上限
// （hostname / os.name / os.kernel / os.arch / cpu.model / iface.mac，口径见 maxTextLen）。
//
// 这类字段的值来自被监控机器，长度不受我们控制；超限的后果不是"丢一条数据"而是
// **永久连不上**：ValidateHello 拒 ⇒ 服务端 4400 关闭 ⇒ Agent 无限退避重连
// （Agent 既不截断、值来自机器本身也不会变）。所以口径是"只放宽、不截断"。
//
// 长度按**字节**算（一个汉字 3 字节、一个 emoji 4 字节）：所以同一句"43 个汉字"在旧口径
// 下就被拒，而 512 字节 ≈ 170 个汉字。三类边界都钉住：
//   - 旧上限（255 / 128 / 32）仍然放行 —— 证明这是**纯放宽**，"以前能过的"一个没变；
//   - 新上限放行、新上限 +1 拒绝 —— 证明上限还在，没有变成 DoS 面；
//   - 多字节：汉字 / emoji 在旧上限与新上限处各一条；另外"rune 数没超、字节数超了"
//     （200 个汉字 = 200 rune / 600 字节）必须仍然被拒 —— 谁把 checkLen 改成按 rune
//     计数，这一条会红。
func TestValidateHelloMachineTextBounds(t *testing.T) {
	good := Hello{
		AgentVersion: "0.1.0",
		Hostname:     "hk-01",
		OS:           OSInfo{Name: "Debian GNU/Linux 12 (bookworm)", Kernel: "6.1.0-13-amd64", Arch: "amd64"},
		CPU:          CPUInfo{Model: "AMD EPYC 7542", Cores: 4},
		BootID:       "9f1c",
		Iface:        IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		IntervalSec:  1,
	}

	cases := []struct {
		name   string
		mutate func(*Hello)
		ok     bool
	}{
		// ---- 旧上限仍然放行（纯放宽的证据）----
		{"hostname 255 字节 ASCII（旧上限）", func(h *Hello) { h.Hostname = strings.Repeat("h", 255) }, true},
		{"hostname 85 个汉字（255 字节，旧上限）", func(h *Hello) { h.Hostname = strings.Repeat("机", 85) }, true},
		{"os.name 128 字节 ASCII（旧上限）", func(h *Hello) { h.OS.Name = strings.Repeat("o", 128) }, true},
		{"os.name 42 个汉字（126 字节，旧上限内）", func(h *Hello) { h.OS.Name = strings.Repeat("机", 42) }, true},
		{"os.kernel 128 字节（旧上限）", func(h *Hello) { h.OS.Kernel = strings.Repeat("k", 128) }, true},
		{"os.arch 128 字节（旧上限）", func(h *Hello) { h.OS.Arch = strings.Repeat("a", 128) }, true},
		{"cpu.model 128 字节（旧上限）", func(h *Hello) { h.CPU.Model = strings.Repeat("m", 128) }, true},
		{"iface.mac 32 字节（旧上限）", func(h *Hello) { h.Iface.MAC = strings.Repeat("a", 32) }, true},
		// ---- 旧上限 +1：今天被拒，放宽后必须接受 ----
		{"hostname 256 字节（旧上限 +1，今天被拒）", func(h *Hello) { h.Hostname = strings.Repeat("h", 256) }, true},
		{"hostname 64 个 emoji（256 字节，今天被拒）", func(h *Hello) { h.Hostname = strings.Repeat("👋", 64) }, true},
		{"os.name 129 字节（今天被拒）", func(h *Hello) { h.OS.Name = strings.Repeat("o", 129) }, true},
		{"os.name 43 个汉字（129 字节，今天被拒）", func(h *Hello) { h.OS.Name = strings.Repeat("机", 43) }, true},
		{"cpu.model 129 字节（今天被拒）", func(h *Hello) { h.CPU.Model = strings.Repeat("m", 129) }, true},
		{"iface.mac 33 字节（今天被拒）", func(h *Hello) { h.Iface.MAC = strings.Repeat("a", 33) }, true},
		// IPoIB（InfiniBand）的硬件地址是 20 字节，sysfs 文本 59 字符：旧上限 32 会让
		// 这类机器**永久连不上**（hello.iface.mac 超限），放宽后必须接受。
		{"iface.mac IPoIB 20 字节地址（59 字符，今天被拒）", func(h *Hello) {
			h.Iface.MAC = "80:00:00:48:fe:80:00:00:00:00:00:00:00:00:00:00:00:00:00:00"
		}, true},
		// ---- 新上限放行 / 新上限 +1 拒绝 ----
		{"hostname 恰好 512 字节（新上限）", func(h *Hello) { h.Hostname = strings.Repeat("h", maxTextLen) }, true},
		{"hostname 513 字节（新上限 +1）", func(h *Hello) { h.Hostname = strings.Repeat("h", maxTextLen+1) }, false},
		{"hostname 170 个汉字（510 字节）", func(h *Hello) { h.Hostname = strings.Repeat("机", 170) }, true},
		{"hostname 171 个汉字（513 字节）", func(h *Hello) { h.Hostname = strings.Repeat("机", 171) }, false},
		{"hostname 128 个 emoji（512 字节，新上限）", func(h *Hello) { h.Hostname = strings.Repeat("👋", 128) }, true},
		{"hostname 129 个 emoji（516 字节，新上限 +1）", func(h *Hello) { h.Hostname = strings.Repeat("👋", 129) }, false},
		{"os.name 恰好 512 字节", func(h *Hello) { h.OS.Name = strings.Repeat("o", maxTextLen) }, true},
		{"os.name 170 个汉字 + 2 个 ASCII（恰好 512 字节）", func(h *Hello) {
			h.OS.Name = strings.Repeat("机", 170) + "ok"
		}, true},
		{"os.name 513 字节（新上限 +1）", func(h *Hello) { h.OS.Name = strings.Repeat("o", maxTextLen+1) }, false},
		{"os.kernel 恰好 512 字节", func(h *Hello) { h.OS.Kernel = strings.Repeat("k", maxTextLen) }, true},
		{"os.kernel 513 字节（新上限 +1）", func(h *Hello) { h.OS.Kernel = strings.Repeat("k", maxTextLen+1) }, false},
		{"os.arch 恰好 512 字节", func(h *Hello) { h.OS.Arch = strings.Repeat("a", maxTextLen) }, true},
		{"os.arch 513 字节（新上限 +1）", func(h *Hello) { h.OS.Arch = strings.Repeat("a", maxTextLen+1) }, false},
		{"cpu.model 恰好 512 字节", func(h *Hello) { h.CPU.Model = strings.Repeat("m", maxTextLen) }, true},
		{"cpu.model 513 字节（新上限 +1）", func(h *Hello) { h.CPU.Model = strings.Repeat("m", maxTextLen+1) }, false},
		{"iface.mac 恰好 512 字节", func(h *Hello) { h.Iface.MAC = strings.Repeat("a", maxTextLen) }, true},
		{"iface.mac 513 字节（新上限 +1）", func(h *Hello) { h.Iface.MAC = strings.Repeat("a", maxTextLen+1) }, false},
		// ---- 单位是字节，不是 rune：200 个汉字只有 200 rune，但 600 字节必须被拒 ----
		{"os.name 200 个汉字（600 字节 / 200 rune）", func(h *Hello) { h.OS.Name = strings.Repeat("机", 200) }, false},
		// ---- 空串 / 极端超长：既有行为不变 ----
		{"hostname 空串（可选字段，既有行为）", func(h *Hello) { h.Hostname = "" }, true},
		{"os.name 空串（可选字段，既有行为）", func(h *Hello) { h.OS.Name = "" }, true},
		{"os.kernel 空串（可选字段，既有行为）", func(h *Hello) { h.OS.Kernel = "" }, true},
		{"cpu.model 空串（可选字段，既有行为）", func(h *Hello) { h.CPU.Model = "" }, true},
		{"iface.mac 空串（可选字段，既有行为）", func(h *Hello) { h.Iface.MAC = "" }, true},
		{"os.name 极端超长 4096 字节", func(h *Hello) { h.OS.Name = strings.Repeat("o", 4096) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := good
			tc.mutate(&h)
			err := ValidateHello(h)
			if tc.ok && err != nil {
				t.Fatalf("必须放行（口径见 maxTextLen 的注释）: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("超长值应当被拒绝")
			}
		})
	}
}

// TestValidateHelloStateRanges 钉住 hello.state 的校验边界（审计 03-4 / B1）。
//
// 规则（理由见 ValidateHello 的注释与 _audit/ROUND5-B1.md 的实测）：
//   - 累计字节数：>= 2^53 拒绝（与 validateNet 的 rx_total/tx_total 同口径）；
//   - ckpt_age_s：**刻意不校验**。-1（未持久化）、0、正常值、陈旧（> 365 天）、
//     未来（时钟回拨）、以及 saved_at 缺失导致的 ≈9.2e9 都必须放行 —— 握手发生在
//     "刷新 savedAt 的那次采集"之前，按 metrics 的口径拒掉它会让这些探针**永久**连不上。
//
// 这一组"必须放行"的断言是有意的护栏：将来谁想"顺手"把 ckpt_age_s 也校验上，
// 这里会红；红是提醒先去读 ValidateHello 的注释，而不是把断言删掉。
func TestValidateHelloStateRanges(t *testing.T) {
	good := Hello{
		AgentVersion: "0.1.0",
		Hostname:     "hk-01",
		OS:           OSInfo{Name: "Debian", Kernel: "6.1.0", Arch: "amd64"},
		CPU:          CPUInfo{Model: "Xeon", Cores: 4},
		BootID:       "boot",
		Iface:        IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		IntervalSec:  1,
		LocalIP:      "203.0.113.5",
		LocalIP6:     "2001:db8::1",
	}

	accepted := []struct {
		name  string
		state *AgentStat
	}{
		{"state 缺省（老 Agent 不发这个字段）", nil},
		{"全部为零（旧 state.json 的默认值）", &AgentStat{}},
		{"未持久化的 -1", &AgentStat{CkptAgeS: -1}},
		{"刚存过盘", &AgentStat{CkptAgeS: 0}},
		{"正常值", &AgentStat{CkptAgeS: 50, TotalRx: 1 << 40, TotalTx: 1 << 39}},
		{"陈旧 400 天（机器停了一年多）", &AgentStat{CkptAgeS: 400 * 24 * 3600}},
		{"未来 -299（时钟回拨 300 秒）", &AgentStat{CkptAgeS: -299}},
		{"saved_at 缺失导致的大年龄", &AgentStat{CkptAgeS: 9223372036}},
		{"int64 最小值的极端年龄（钉子用例里那个）", &AgentStat{CkptAgeS: math.MinInt64}},
		{"累计值刚好低于 2^53", &AgentStat{TotalRx: maxUint53 - 1, TotalTx: maxUint53 - 1}},
	}
	for _, tc := range accepted {
		t.Run("放行 "+tc.name, func(t *testing.T) {
			h := good
			h.State = tc.state
			if err := ValidateHello(h); err != nil {
				t.Fatalf("必须放行（理由见 ValidateHello 的注释）: %v", err)
			}
		})
	}

	rejected := []struct {
		name  string
		state *AgentStat
	}{
		{"rx_total 到达 2^53", &AgentStat{TotalRx: maxUint53}},
		{"tx_total 到达 2^53", &AgentStat{TotalTx: maxUint53}},
		{"两个累计值都是 MaxUint64", &AgentStat{TotalRx: math.MaxUint64, TotalTx: math.MaxUint64}},
	}
	for _, tc := range rejected {
		t.Run("拒绝 "+tc.name, func(t *testing.T) {
			h := good
			h.State = tc.state
			if err := ValidateHello(h); err == nil {
				t.Fatal("累计字节数 >= 2^53 应当被拒绝")
			}
		})
	}
}

// 两个本机地址都是可选字段：空串是**正常**值（纯 IPv4/纯 IPv6 主机、取不到
// 路由的机器都会留空）。把空串判成非法会让这类 Agent 直接连不上服务端。
func TestValidateHelloAllowsEmptyLocalIPs(t *testing.T) {
	h := Hello{
		AgentVersion: "0.1.0",
		Hostname:     "hk-01",
		OS:           OSInfo{Name: "Debian", Kernel: "6.1.0", Arch: "amd64"},
		CPU:          CPUInfo{Model: "Xeon", Cores: 4},
		Iface:        IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		IntervalSec:  1,
	}
	if err := ValidateHello(h); err != nil {
		t.Fatalf("两个本机地址都为空时应当通过: %v", err)
	}
	// 只有一边也是正常的。
	h.LocalIP = "203.0.113.5"
	if err := ValidateHello(h); err != nil {
		t.Fatalf("只有 IPv4 时应当通过: %v", err)
	}
	h.LocalIP, h.LocalIP6 = "", "2001:db8::1"
	if err := ValidateHello(h); err != nil {
		t.Fatalf("只有 IPv6 时应当通过: %v", err)
	}
}

// 本机地址要能原样过一遍 JSON：字段名与 omitempty 行为都属于协议的一部分，
// 前端依赖 local_ip / local_ip6 这两个键。
func TestHelloLocalIPRoundTrip(t *testing.T) {
	h := Hello{
		AgentVersion: "0.1.0",
		Hostname:     "hk-01",
		OS:           OSInfo{Name: "Debian", Kernel: "6.1.0", Arch: "amd64"},
		CPU:          CPUInfo{Model: "Xeon", Cores: 4},
		Iface:        IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		IntervalSec:  1,
		LocalIP:      "203.0.113.5",
		LocalIP6:     "2001:db8::1",
	}
	env, err := New(TypeHello, h)
	if err != nil {
		t.Fatalf("构造 hello 帧: %v", err)
	}
	raw, err := env.Encode()
	if err != nil {
		t.Fatalf("编码 hello 帧: %v", err)
	}
	if !strings.Contains(string(raw), `"local_ip":"203.0.113.5"`) ||
		!strings.Contains(string(raw), `"local_ip6":"2001:db8::1"`) {
		t.Fatalf("帧里的本机地址字段名不对: %s", raw)
	}

	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("解析帧: %v", err)
	}
	var got Hello
	if err := decoded.Bind(&got); err != nil {
		t.Fatalf("绑定 hello: %v", err)
	}
	if got.LocalIP != h.LocalIP || got.LocalIP6 != h.LocalIP6 {
		t.Fatalf("本机地址往返后变了: %+v", got)
	}
	if err := ValidateHello(got); err != nil {
		t.Fatalf("往返后的 hello 应当合法: %v", err)
	}

	// 空值必须真的从 JSON 里消失：老服务端不认识这两个字段，
	// 多发两个空串只会白白占帧宽（帧上限 16 KiB）。
	empty, err := New(TypeHello, Hello{AgentVersion: "0.1.0", Iface: IfaceInfo{Name: "eth0"}})
	if err != nil {
		t.Fatalf("构造空值 hello: %v", err)
	}
	emptyRaw, err := empty.Encode()
	if err != nil {
		t.Fatalf("编码空值 hello: %v", err)
	}
	if strings.Contains(string(emptyRaw), "local_ip") {
		t.Fatalf("本机地址为空时不应当出现这两个键: %s", emptyRaw)
	}
}
