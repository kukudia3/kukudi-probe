package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrVersionMismatch 表示对端协议版本与本程序不兼容，调用方应当回
// upgrade_required 并以 4426 关闭连接。
var ErrVersionMismatch = errors.New("协议版本不匹配")

// 消息类型（docs/PROTOCOL.md §5）。
const (
	TypeHello   = "hello"
	TypeWelcome = "welcome"
	TypeMetrics = "metrics"
	TypePing    = "ping"
	TypePong    = "pong"
	TypeConfig  = "config"
	TypeAck     = "ack"
	TypeError   = "error"
)

// 错误码（docs/PROTOCOL.md §5.6）。
const (
	CodeBadFrame        = "bad_frame"
	CodeUnknownType     = "unknown_type"
	CodeRateLimited     = "rate_limited"
	CodeTooLarge        = "too_large"
	CodeUpgradeRequired = "upgrade_required"
	CodeUnauthorized    = "unauthorized"
	CodeNodeDisabled    = "node_disabled"
	CodeIntervalInvalid = "interval_invalid"
	CodeInternal        = "internal"
)

// WebSocket 关闭码（docs/PROTOCOL.md §5.7）。
//
// 这里用普通整数而不是 websocket.StatusCode，避免让协议包依赖具体的 WebSocket 实现。
const (
	CloseBadRequest      = 4400
	CloseUnauthorized    = 4401
	CloseNodeDisabled    = 4403
	CloseHelloTimeout    = 4408
	CloseUpgradeRequired = 4426
	CloseInternalError   = 1011
)

// 协议限制。
const (
	// MaxFrame 是单帧上限（Agent→Server；Server→Agent 也用它做统一保护）。
	MaxFrame = 16 << 10
	// MinIntervalSec / MaxIntervalSec 是允许的上报间隔。
	MinIntervalSec = 1
	MaxIntervalSec = 300
	// HelloTimeout 是连接建立后必须收到 hello 的时间。
	HelloTimeout = 10 // 秒
)

// 延迟探测（ping）的限制。
//
// 这些数字定义在协议层：服务端（校验设置、下发 config）与 Agent（校验收到的 config、
// 校验自己发出的 metrics）必须用同一组上限，否则会出现"服务端存得下、Agent 却拒收"
// 的漂移。存储层直接引用这里，不再抄一份。
const (
	// MinPingIntervalSec / MaxPingIntervalSec 是允许的探测间隔。
	MinPingIntervalSec = 10
	MaxPingIntervalSec = 3600
	// DefaultPingIntervalSec 是探测间隔的缺省值（设置键不存在时用它）。
	DefaultPingIntervalSec = 60
	// MaxPingTargets 是单节点的目标数量上限（一帧 config、一帧 metrics 都要装得下）。
	MaxPingTargets = 16
	// MaxPingLabelLen 是目标显示名上限（按字符计）。
	MaxPingLabelLen = 32
	// MaxPingHostLen 是主机名上限（DNS 的 253 字符）。
	MaxPingHostLen = 253
	// MaxPingMS 是单次探测耗时上限（10 分钟）：超过它的只可能是坏数据。
	MaxPingMS = 600000.0
	// MaxPingPort 是 TCP 端口上限。
	MaxPingPort = 65535
)

// 探测方式。
const (
	PingTypeICMP = "icmp"
	PingTypeTCP  = "tcp"
)

// Envelope 是所有消息的外层结构。
//
// 未知字段一律忽略（向前兼容）；已知字段严格校验，不合法整帧丢弃。
type Envelope struct {
	V   int             `json:"v"`
	T   string          `json:"t"`
	TS  int64           `json:"ts,omitempty"`
	Seq uint64          `json:"seq,omitempty"`
	D   json.RawMessage `json:"d,omitempty"`
}

// Hello 是 Agent 连接后的第一帧（PROTOCOL.md §5.1）。
type Hello struct {
	AgentVersion string     `json:"agent_version"`
	Hostname     string     `json:"hostname"`
	OS           OSInfo     `json:"os"`
	CPU          CPUInfo    `json:"cpu"`
	BootID       string     `json:"boot_id"`
	UptimeSec    uint64     `json:"uptime_sec"`
	Iface        IfaceInfo  `json:"iface"`
	IntervalSec  int        `json:"interval_sec"`
	State        *AgentStat `json:"state,omitempty"`

	// LocalIP / LocalIP6 是 Agent 自己采集的**本机地址**。
	//
	// 取法是"不发包的 UDP connect"：建一个 UDP 套接字 connect 到服务端地址，
	// 读 LocalAddr —— 内核会告诉我们"去服务端会用哪个源地址"，全程没有报文发出，
	// 所以既不依赖任何第三方服务，也不受 NAT/代理影响（拿到的是机器自己的地址）。
	//
	// 与 Welcome.ObservedIP（服务端看到的来源地址）互补：
	//   - 同一台机器上跑 Agent + Cloudflare Tunnel 时，ObservedIP 是 127.0.0.1；
	//   - 节点在 NAT 后面或走代理出站时，只有这两个字段才是机器自己的地址。
	//
	// 两个字段都是可选的（omitempty）：只有 IPv6 的机器只有 LocalIP6，反之亦然，
	// 一个都取不到时都为空 —— 这是附加信息，绝不能因为它让 Agent 连不上。
	LocalIP  string `json:"local_ip,omitempty"`
	LocalIP6 string `json:"local_ip6,omitempty"`
}

// AgentStat 是 hello 里携带的本地流量状态，让服务端一眼看出 Agent 的基线。
type AgentStat struct {
	CkptAgeS int64  `json:"ckpt_age_s"`
	TotalRx  uint64 `json:"rx_total"`
	TotalTx  uint64 `json:"tx_total"`
}

// Welcome 是服务端对 hello 的应答（PROTOCOL.md §5.2）。
type Welcome struct {
	NodeID        int64  `json:"node_id"`
	Name          string `json:"name"`
	IntervalSec   int    `json:"interval_sec"`
	ServerTime    int64  `json:"server_time"`
	ObservedIP    string `json:"observed_ip"`
	ConfigVersion int64  `json:"config_version"`
	Notes         string `json:"notes,omitempty"`
}

// Ping 用于测量 RTT 与保活；Pong 原样回 ts_us（PROTOCOL.md §5.4）。
type Ping struct {
	TsUS int64 `json:"ts_us"`
}

// Pong 与 Ping 结构相同。
type Pong = Ping

// Config 是服务端下发的配置变更（PROTOCOL.md §5.5）。
//
// 它同时承担两个职责，靠 ConfigVersion 区分新旧：
//   - 连接建立时紧跟 welcome 下发一帧，把该节点的完整配置交给 Agent；
//   - 设置变化时主动再推一帧，不必等 Agent 重连。
type Config struct {
	ConfigVersion int64 `json:"config_version"`
	IntervalSec   int   `json:"interval_sec"`
	// Iface 是"按节点指定监控网卡"的**预留**字段：服务端目前从不下发它
	// （internal/server/agentconn.go 的 configFrame 不填），Agent 也从不应用它
	// ——实际被监控的网卡只由 Agent 的 --iface / 自动探测决定。
	//
	// 保留而不删除是刻意的：删字段属于协议变更（PROTOCOL.md §9），必须升版本并同时改两端。
	// 但任何调用方都**不要**把它当成已生效的配置：面板上的"监控网卡"优先显示节点配置里
	// 的这个名字，它可能与真正被监控的网卡不一致（这是诊断时最容易误判的那类问题）。
	Iface  string `json:"iface,omitempty"`
	Reload bool   `json:"reload,omitempty"`

	// PingTargets 是**已启用**的探测目标（enabled=false 的目标不下发）。
	// PingIntervalSec 是探测间隔；0 表示"本次不改探测间隔"。
	PingTargets     []PingTarget `json:"ping_targets,omitempty"`
	PingIntervalSec int          `json:"ping_interval_sec,omitempty"`
}

// Ack 是 Agent 对 config 的确认。
type Ack struct {
	ConfigVersion int64 `json:"config_version"`
}

// ErrorPayload 是双向错误消息。
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal,omitempty"`
}

// New 构造一个信封，payload 为 nil 时省略 d。
func New(msgType string, payload any) (Envelope, error) {
	e := Envelope{V: Version, T: msgType}
	if payload == nil {
		return e, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("序列化 %s 负载失败: %w", msgType, err)
	}
	e.D = raw
	return e, nil
}

// Encode 把信封序列化成帧内容。
func (e Envelope) Encode() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("序列化 %s 帧失败: %w", e.T, err)
	}
	if len(data) > MaxFrame {
		return nil, fmt.Errorf("帧过大：%d 字节，上限 %d", len(data), MaxFrame)
	}
	return data, nil
}

// Decode 解析并校验信封本身。负载的校验由对应类型的 Validate 函数负责。
func Decode(data []byte) (Envelope, error) {
	if len(data) > MaxFrame {
		return Envelope{}, fmt.Errorf("帧过大：%d 字节，上限 %d", len(data), MaxFrame)
	}
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, fmt.Errorf("解析帧失败: %w", err)
	}
	if e.V != Version {
		return Envelope{}, fmt.Errorf("%w：对端 %d，本程序支持 %d", ErrVersionMismatch, e.V, Version)
	}
	if e.T == "" {
		return Envelope{}, fmt.Errorf("帧缺少消息类型")
	}
	return e, nil
}

// Bind 把负载解析到目标结构（未知字段忽略）。
func (e Envelope) Bind(v any) error {
	if len(e.D) == 0 {
		return fmt.Errorf("%s 帧缺少负载", e.T)
	}
	if err := json.Unmarshal(e.D, v); err != nil {
		return fmt.Errorf("解析 %s 负载失败: %w", e.T, err)
	}
	return nil
}

// ErrorEnvelope 构造错误帧。
func ErrorEnvelope(code, message string, fatal bool) Envelope {
	e, err := New(TypeError, ErrorPayload{Code: code, Message: message, Fatal: fatal})
	if err != nil {
		// 错误负载是固定结构，序列化不可能失败；真失败了也只能返回空帧。
		return Envelope{V: Version, T: TypeError}
	}
	return e
}
