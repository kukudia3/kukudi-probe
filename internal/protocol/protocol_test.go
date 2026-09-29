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
		{"负载为 NaN", func(m *Metrics) { m.Load.L1 = math.NaN() }},
		{"负载过大", func(m *Metrics) { m.Load.L15 = 1e9 }},
		{"网卡名为空", func(m *Metrics) { m.Net.Iface = "" }},
		{"网卡名过长", func(m *Metrics) { m.Net.Iface = strings.Repeat("e", maxIfaceLen+1) }},
		{"累计流量过大", func(m *Metrics) { m.Net.RxTotal = 1 << 53 }},
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

func TestValidateHello(t *testing.T) {
	good := Hello{
		AgentVersion: "0.1.0",
		Hostname:     "hk-01",
		OS:           OSInfo{Name: "Debian", Kernel: "6.1.0", Arch: "amd64"},
		CPU:          CPUInfo{Model: "Xeon", Cores: 4},
		BootID:       "boot",
		Iface:        IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		IntervalSec:  1,
	}
	if err := ValidateHello(good); err != nil {
		t.Fatalf("合法 hello 被拒绝: %v", err)
	}

	bad := []struct {
		name   string
		mutate func(*Hello)
	}{
		{"缺少 Agent 版本", func(h *Hello) { h.AgentVersion = "" }},
		{"主机名过长", func(h *Hello) { h.Hostname = strings.Repeat("h", maxHostnameLen+1) }},
		{"核心数异常", func(h *Hello) { h.CPU.Cores = 99999 }},
		{"间隔越界", func(h *Hello) { h.IntervalSec = 301 }},
		{"网卡名为空且必填", func(h *Hello) { h.Iface.Name = strings.Repeat("e", maxIfaceLen+1) }},
		{"ifindex 为负", func(h *Hello) { h.Iface.IfIndex = -1 }},
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
