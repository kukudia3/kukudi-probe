package protocol

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 探测目标的 JSON 形状是**冻结的接口契约**（前端按字段名解析、服务端按同一份
// 定义存库），所以这里逐字段钉死字段名，而不是只测"能往返"。
func TestPingTargetConfigRoundTrip(t *testing.T) {
	cfg := Config{
		ConfigVersion:   7,
		IntervalSec:     1,
		PingIntervalSec: 60,
		PingTargets: []PingTarget{
			{ID: 1, Type: PingTypeTCP, Host: "1.1.1.1", Port: 443},
			{ID: 2, Type: PingTypeICMP, Host: "2001:db8::1"},
		},
	}
	frame, err := New(TypeConfig, cfg)
	if err != nil {
		t.Fatalf("构造 config 帧: %v", err)
	}
	raw, err := frame.Encode()
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	for _, needle := range []string{
		`"config_version":7`, `"interval_sec":1`, `"ping_interval_sec":60`,
		`"ping_targets":[`, `"id":1`, `"type":"tcp"`, `"host":"1.1.1.1"`, `"port":443`,
		`"type":"icmp"`, `"host":"2001:db8::1"`,
	} {
		if !strings.Contains(string(raw), needle) {
			t.Errorf("config 帧里缺少 %s：%s", needle, raw)
		}
	}
	// icmp 目标没有端口，omitempty 让它不要出现在帧里（免得看的人以为它有用）。
	if strings.Contains(string(raw), `"port":0`) {
		t.Errorf("icmp 目标不该出现 port：%s", raw)
	}

	env, err := Decode(raw)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	var got Config
	if err := env.Bind(&got); err != nil {
		t.Fatalf("绑定: %v", err)
	}
	if err := ValidateConfig(got); err != nil {
		t.Fatalf("往返后的配置不合法: %v", err)
	}
	if len(got.PingTargets) != 2 || got.PingTargets[0].Port != 443 || got.PingTargets[1].Host != "2001:db8::1" {
		t.Fatalf("往返后内容不一致: %+v", got)
	}
}

func TestValidateConfigAcceptsMinimal(t *testing.T) {
	// 只有版本号（0 间隔＝"本次不改"）也不该被拒：config 帧可以只用来宣告版本。
	if err := ValidateConfig(Config{ConfigVersion: 1}); err != nil {
		t.Fatalf("最小配置被拒绝: %v", err)
	}
	full := Config{
		ConfigVersion:   2,
		IntervalSec:     5,
		Iface:           "eth0",
		PingIntervalSec: DefaultPingIntervalSec,
		PingTargets:     []PingTarget{{ID: 1, Type: PingTypeICMP, Host: "example.com"}},
	}
	if err := ValidateConfig(full); err != nil {
		t.Fatalf("完整配置被拒绝: %v", err)
	}
	// 目标数量正好到上限应当接受。
	maxed := Config{ConfigVersion: 1, PingTargets: make([]PingTarget, MaxPingTargets)}
	for i := range maxed.PingTargets {
		maxed.PingTargets[i] = PingTarget{ID: int64(i + 1), Type: PingTypeICMP, Host: "1.1.1.1"}
	}
	if err := ValidateConfig(maxed); err != nil {
		t.Fatalf("满员配置被拒绝: %v", err)
	}
}

func TestValidateConfigRejectsBadPayload(t *testing.T) {
	base := func() Config {
		return Config{
			ConfigVersion:   3,
			IntervalSec:     1,
			PingIntervalSec: 60,
			PingTargets:     []PingTarget{{ID: 1, Type: PingTypeTCP, Host: "1.1.1.1", Port: 443}},
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"版本号为负", func(c *Config) { c.ConfigVersion = -1 }},
		{"上报间隔越界", func(c *Config) { c.IntervalSec = 301 }},
		{"上报间隔为负", func(c *Config) { c.IntervalSec = -1 }},
		{"探测间隔过小", func(c *Config) { c.PingIntervalSec = MinPingIntervalSec - 1 }},
		{"探测间隔过大", func(c *Config) { c.PingIntervalSec = MaxPingIntervalSec + 1 }},
		{"目标类型非法", func(c *Config) { c.PingTargets[0].Type = "udp" }},
		{"目标类型为空", func(c *Config) { c.PingTargets[0].Type = "" }},
		{"目标 ID 为 0", func(c *Config) { c.PingTargets[0].ID = 0 }},
		{"目标 ID 为负", func(c *Config) { c.PingTargets[0].ID = -5 }},
		{"tcp 缺端口", func(c *Config) { c.PingTargets[0].Port = 0 }},
		{"端口越界", func(c *Config) { c.PingTargets[0].Port = 65536 }},
		{"端口为负", func(c *Config) { c.PingTargets[0].Port = -1 }},
		{"主机为空", func(c *Config) { c.PingTargets[0].Host = "" }},
		{"主机只有空白", func(c *Config) { c.PingTargets[0].Host = "   " }},
		{"主机含空格", func(c *Config) { c.PingTargets[0].Host = "1.1.1.1 x" }},
		{"主机含控制字符", func(c *Config) { c.PingTargets[0].Host = "1.1.1.1\n" }},
		{"主机过长", func(c *Config) { c.PingTargets[0].Host = strings.Repeat("a", MaxPingHostLen+1) }},
		{"目标 ID 重复", func(c *Config) {
			c.PingTargets = append(c.PingTargets, PingTarget{ID: 1, Type: PingTypeICMP, Host: "9.9.9.9"})
		}},
		{"目标过多", func(c *Config) { c.PingTargets = make([]PingTarget, MaxPingTargets+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			if err := ValidateConfig(cfg); err == nil {
				t.Fatalf("非法配置被接受: %+v", cfg)
			}
		})
	}
}

// icmp 的端口没有意义：设置侧会清零，协议侧只挡越界值。
func TestValidateConfigIgnoresICMPPort(t *testing.T) {
	cfg := Config{ConfigVersion: 1, PingTargets: []PingTarget{
		{ID: 1, Type: PingTypeICMP, Host: "1.1.1.1", Port: 443},
	}}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("icmp 目标带了端口不该被拒（应当被忽略）: %v", err)
	}
}

func TestMetricsPingsRoundTrip(t *testing.T) {
	m := validMetrics()
	m.Pings = []PingResult{
		{TargetID: 1, AvgMS: 23.4, MinMS: 20.1, MaxMS: 31.2, LossPct: 0},
		{TargetID: 2, AvgMS: 0, MinMS: 0, MaxMS: 0, LossPct: 100},
		{TargetID: 3, AvgMS: 41, MinMS: 40, MaxMS: 42, LossPct: 33.333},
	}
	if err := ValidateMetrics(m); err != nil {
		t.Fatalf("合法探测结果被拒绝: %v", err)
	}
	frame, err := New(TypeMetrics, m)
	if err != nil {
		t.Fatalf("构造 metrics 帧: %v", err)
	}
	raw, err := frame.Encode()
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	if !strings.Contains(string(raw), `"pings":[{"target_id":1,"avg_ms":23.4`) {
		t.Fatalf("pings 字段形状不对: %s", raw)
	}
	env, err := Decode(raw)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	var got Metrics
	if err := env.Bind(&got); err != nil {
		t.Fatalf("绑定: %v", err)
	}
	if len(got.Pings) != 3 || got.Pings[1].LossPct != 100 || got.Pings[2].MaxMS != 42 {
		t.Fatalf("往返后内容不一致: %+v", got.Pings)
	}

	// 没有探测结果时整个字段不该出现（老服务端/老 Agent 互操作时的形状）。
	empty, err := New(TypeMetrics, validMetrics())
	if err != nil {
		t.Fatalf("构造 metrics 帧: %v", err)
	}
	rawEmpty, err := empty.Encode()
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	if strings.Contains(string(rawEmpty), "pings") {
		t.Fatalf("没有探测结果时不该出现 pings 字段: %s", rawEmpty)
	}
}

func TestValidateMetricsRejectsBadPings(t *testing.T) {
	base := func() Metrics {
		m := validMetrics()
		m.Pings = []PingResult{{TargetID: 1, AvgMS: 23, MinMS: 20, MaxMS: 31, LossPct: 0}}
		return m
	}
	cases := []struct {
		name   string
		mutate func(*Metrics)
	}{
		{"目标 ID 为 0", func(m *Metrics) { m.Pings[0].TargetID = 0 }},
		{"目标 ID 为负", func(m *Metrics) { m.Pings[0].TargetID = -1 }},
		{"目标重复", func(m *Metrics) { m.Pings = append(m.Pings, m.Pings[0]) }},
		{"条目过多", func(m *Metrics) { m.Pings = make([]PingResult, MaxPingTargets+1) }},
		{"平均值为 NaN", func(m *Metrics) { m.Pings[0].AvgMS = math.NaN() }},
		{"最小值为 Inf", func(m *Metrics) { m.Pings[0].MinMS = math.Inf(1) }},
		{"最大值为 -Inf", func(m *Metrics) { m.Pings[0].MaxMS = math.Inf(-1) }},
		{"平均值为负", func(m *Metrics) { m.Pings[0].AvgMS = -1 }},
		{"耗时越界", func(m *Metrics) { m.Pings[0].MaxMS = MaxPingMS + 1 }},
		{"丢包率为负", func(m *Metrics) { m.Pings[0].LossPct = -0.1 }},
		{"丢包率超过 100", func(m *Metrics) { m.Pings[0].LossPct = 100.1 }},
		{"丢包率为 NaN", func(m *Metrics) { m.Pings[0].LossPct = math.NaN() }},
		{"min 大于 max", func(m *Metrics) { m.Pings[0].MinMS = 40; m.Pings[0].AvgMS = 41 }},
		{"avg 不在 min/max 之间", func(m *Metrics) { m.Pings[0].AvgMS = 100 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(&m)
			if err := ValidateMetrics(m); err == nil {
				t.Fatalf("非法探测结果被接受: %+v", m.Pings)
			}
		})
	}
}

// 满员配置 + 满员探测结果都必须装得进 16 KB 的帧上限。
//
// 这个上限是硬约束（超了 Agent 根本发不出去，而用户只会看到"延迟图一直空着"），
// 所以用最坏情况（16 个目标、主机名顶到 253 字符）钉死。
func TestMaxPingPayloadFitsFrame(t *testing.T) {
	cfg := Config{ConfigVersion: 1, IntervalSec: 1, PingIntervalSec: MaxPingIntervalSec}
	host := strings.Repeat("a", MaxPingHostLen)
	for i := 0; i < MaxPingTargets; i++ {
		cfg.PingTargets = append(cfg.PingTargets, PingTarget{ID: int64(i + 1), Type: PingTypeTCP, Host: host, Port: 65535})
	}
	frame, err := New(TypeConfig, cfg)
	if err != nil {
		t.Fatalf("构造 config 帧: %v", err)
	}
	raw, err := frame.Encode()
	if err != nil {
		t.Fatalf("满员 config 帧超过上限: %v", err)
	}
	if len(raw) > MaxFrame/2 {
		t.Errorf("满员 config 帧 %d 字节，已经用掉一半以上的帧上限，需要重新评估上限", len(raw))
	}

	m := validMetrics()
	for i := 0; i < MaxPingTargets; i++ {
		m.Pings = append(m.Pings, PingResult{
			TargetID: int64(i + 1), AvgMS: MaxPingMS, MinMS: MaxPingMS, MaxMS: MaxPingMS, LossPct: 100,
		})
	}
	mFrame, err := New(TypeMetrics, m)
	if err != nil {
		t.Fatalf("构造 metrics 帧: %v", err)
	}
	if _, err := mFrame.Encode(); err != nil {
		t.Fatalf("满员 metrics 帧超过上限: %v", err)
	}
}

// 未知字段忽略：老 Agent 收到带新字段的 config 时不该报错（向前兼容）。
func TestConfigIgnoresUnknownFields(t *testing.T) {
	raw := []byte(`{"v":1,"t":"config","d":{"config_version":5,"interval_sec":2,"ping_future":"x"}}`)
	env, err := Decode(raw)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	var cfg Config
	if err := env.Bind(&cfg); err != nil {
		t.Fatalf("绑定: %v", err)
	}
	if cfg.ConfigVersion != 5 || cfg.IntervalSec != 2 {
		t.Fatalf("已知字段没解析出来: %+v", cfg)
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("不该因为未知字段报错: %v", err)
	}
	// 直接构造一个 JSON 里带空数组的 config：空数组与"字段缺失"都表示没有目标。
	var withEmpty Config
	if err := json.Unmarshal([]byte(`{"config_version":6,"interval_sec":1,"ping_targets":[],"ping_interval_sec":60}`), &withEmpty); err != nil {
		t.Fatalf("解析空数组: %v", err)
	}
	if err := ValidateConfig(withEmpty); err != nil {
		t.Fatalf("空目标数组应当合法: %v", err)
	}
	if len(withEmpty.PingTargets) != 0 {
		t.Fatalf("空数组不该被解析成目标: %+v", withEmpty.PingTargets)
	}
}
