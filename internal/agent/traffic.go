package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Checkpoint 是 Agent 本地持久化的流量基线（docs/DESIGN.md §9.2）。
//
// 它同时保存两类值：
//   - Total*：探针自己的长期单调累计（权威，服务端据此做增量）
//   - Raw*：内核 counter 的快照（用于下次开机算差值与检测清零/回绕）
//
// 以及网卡与开机身份：ifindex/MAC/boot_id 任一变化都必须重设基线。
type Checkpoint struct {
	Iface   string `json:"iface"`
	IfIndex int    `json:"ifindex"`
	MAC     string `json:"mac"`
	BootID  string `json:"boot_id"`
	TotalRx uint64 `json:"total_rx"`
	TotalTx uint64 `json:"total_tx"`
	RawRx   uint64 `json:"raw_rx"`
	RawTx   uint64 `json:"raw_tx"`
	SavedAt int64  `json:"saved_at"`
}

const (
	// checkpointInterval 是两次落盘之间的最短间隔，防止写放大。
	checkpointInterval = 10 * time.Second
	// checkpointDelta 是触发立即落盘的未持久化增量（字节）。
	checkpointDelta = 64 << 20
	// wrap32 用于识别 32 位内核计数器的回绕。
	wrap32 = uint64(1) << 32
	// wrap32Threshold 是判定"这是回绕而不是清零"的分界。
	wrap32Threshold = uint64(1) << 31
	// checkpointMode 是 checkpoint 文件的权限。
	checkpointMode = 0o600
)

// Delta 是一次采样算出的流量增量。
type Delta struct {
	Rx uint64
	Tx uint64
	// Reset 非空表示本拍重设了基线（增量为 0），值说明原因：
	// init / iface_changed / reboot / counter_reset。
	Reset string
}

// Traffic 维护流量累计与本地 checkpoint。
//
// path 为空时退化为"仅内存"：不落盘、CkptAge 返回 -1。
// 这用于 --print-json 自检，避免诊断命令在磁盘上留下状态文件。
//
// 所有导出方法（Checkpoint / CkptAge / Apply / MaybeSave）都持 mu：
// 同一个 Traffic 可能被两个 goroutine 同时使用 —— 读循环失败后 session 只等
// 写循环 2 秒就返回（见 Client.session），被遗弃的旧写循环会与新写循环并发调用
// Apply/MaybeSave，还会与退出时 main 的强制落盘撞上。没有这把锁时它们同时改
// cp / pending* / dirty / savedAt，并且同时写同一个 .tmp 文件（真正的数据竞争，
// 累计值可能丢失、checkpoint 可能被写坏）。
// save 由 MaybeSave 在持锁状态下调用，自己不重复加锁。
type Traffic struct {
	mu        sync.Mutex
	path      string
	cp        Checkpoint
	pendingRx uint64
	pendingTx uint64
	dirty     bool
	persisted bool
	savedAt   time.Time
}

// LoadTraffic 读取 checkpoint。
//
// 文件不存在是正常情况（首次安装），返回空基线。
// 文件损坏时返回可用的空基线与一条说明，让调用方记一条警告后继续
// ——统计宁可少算，也不能因为一个坏文件让 Agent 起不来。
func LoadTraffic(path string) (*Traffic, string, error) {
	t := &Traffic{path: path}
	if path == "" {
		return t, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return t, "", nil
		}
		return t, "", fmt.Errorf("读取流量 checkpoint %s 失败: %w", path, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return t, fmt.Sprintf("流量 checkpoint 损坏（%v），已按新基线重新开始统计", err), nil
	}
	t.cp = cp
	t.persisted = true
	if cp.SavedAt > 0 {
		t.savedAt = time.Unix(cp.SavedAt, 0)
	}
	return t, "", nil
}

// Checkpoint 返回当前基线副本。
func (t *Traffic) Checkpoint() Checkpoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cp
}

// CkptAge 返回距上次落盘的秒数；未持久化时返回 -1。
func (t *Traffic) CkptAge(now time.Time) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.persisted {
		return -1
	}
	return int64(now.Sub(t.savedAt).Seconds())
}

// Apply 用本拍的网卡身份与内核 counter 更新累计值，返回本拍增量。
func (t *Traffic) Apply(iface string, ifindex int, mac, bootID string, rawRx, rawTx uint64, now time.Time) Delta {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch {
	case t.cp.Iface == "":
		return t.reset(iface, ifindex, mac, bootID, rawRx, rawTx, "init")
	case iface != t.cp.Iface || ifindex != t.cp.IfIndex || mac != t.cp.MAC:
		return t.reset(iface, ifindex, mac, bootID, rawRx, rawTx, "iface_changed")
	case bootID != "" && t.cp.BootID != "" && bootID != t.cp.BootID:
		// 机器重启：内核 counter 已归零，绝不能用差值。
		return t.reset(iface, ifindex, mac, bootID, rawRx, rawTx, "reboot")
	}

	rx, rxReset := counterDelta(t.cp.RawRx, rawRx)
	tx, txReset := counterDelta(t.cp.RawTx, rawTx)
	reason := ""
	if rxReset || txReset {
		reason = "counter_reset"
	}

	t.cp.Iface, t.cp.IfIndex, t.cp.MAC = iface, ifindex, mac
	t.cp.BootID = bootID
	t.cp.RawRx, t.cp.RawTx = rawRx, rawTx
	t.cp.TotalRx += rx
	t.cp.TotalTx += tx
	t.pendingRx += rx
	t.pendingTx += tx
	t.dirty = true

	return Delta{Rx: rx, Tx: tx, Reset: reason}
}

// reset 重设基线：累计值保持不变，只把"当前 raw"当作新起点。
// 只由 Apply 在持锁状态下调用，自己不重复加锁。
func (t *Traffic) reset(iface string, ifindex int, mac, bootID string, rawRx, rawTx uint64, reason string) Delta {
	t.cp.Iface, t.cp.IfIndex, t.cp.MAC = iface, ifindex, mac
	t.cp.BootID = bootID
	t.cp.RawRx, t.cp.RawTx = rawRx, rawTx
	t.dirty = true
	return Delta{Reset: reason}
}

// counterDelta 计算单方向计数器的增量，并识别清零与 32 位回绕。
func counterDelta(prev, cur uint64) (delta uint64, reset bool) {
	switch {
	case cur >= prev:
		return cur - prev, false
	case prev > wrap32Threshold && cur < wrap32Threshold:
		// 32 位计数器回绕：老内核或 32 位平台。
		return (wrap32 - prev) + cur, false
	default:
		return 0, true
	}
}

// MaybeSave 按策略落盘：距上次落盘超过 checkpointInterval，
// 或未持久化增量超过 checkpointDelta，或 force 为真。
//
// 返回是否真的写了盘。写失败不影响采集：内存里的累计值仍然正确，
// 只是崩溃时可能少算这一小段（宁可少算，不可多算）。
func (t *Traffic) MaybeSave(now time.Time, force bool) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.path == "" {
		// 仅内存模式（--print-json 自检）：不落盘，也不在磁盘上留状态文件。
		if t.dirty {
			t.savedAt = now
			t.dirty = false
			t.pendingRx, t.pendingTx = 0, 0
		}
		return false, nil
	}
	if !t.dirty {
		return false, nil
	}
	// savedAt 为零说明本进程还没落过盘（首次运行）：不节流，直接写。
	// 时间回退（elapsed < 0，例如 NTP 校准）也视为到期，避免长时间不落盘。
	elapsed := now.Sub(t.savedAt)
	dueByTime := t.savedAt.IsZero() || elapsed >= checkpointInterval || elapsed < 0
	if !force && !dueByTime && t.pendingRx+t.pendingTx < checkpointDelta {
		return false, nil
	}
	if err := t.save(now); err != nil {
		return false, err
	}
	return true, nil
}

// save 原子写入 checkpoint：临时文件 → fsync → rename。
// 调用方通过 MaybeSave 保证 path 非空**且已持锁**（它自己不加锁）。
func (t *Traffic) save(now time.Time) error {
	t.cp.SavedAt = now.Unix()

	data, err := json.Marshal(t.cp)
	if err != nil {
		return fmt.Errorf("序列化流量 checkpoint 失败: %w", err)
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建状态目录 %s 失败: %w", dir, err)
	}

	// 原子替换：先写临时文件并 fsync，再 rename，避免断电留下半个 JSON。
	tmp := t.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, checkpointMode)
	if err != nil {
		return fmt.Errorf("写入流量 checkpoint 失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("写入流量 checkpoint 失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync 流量 checkpoint 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭流量 checkpoint 失败: %w", err)
	}
	if err := os.Rename(tmp, t.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换流量 checkpoint 失败: %w", err)
	}
	syncDir(dir)

	t.savedAt = now
	t.persisted = true
	t.dirty = false
	t.pendingRx, t.pendingTx = 0, 0
	return nil
}

// syncDir 让 rename 本身也落到磁盘。Windows 上打开目录会失败，忽略即可。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	_ = d.Sync()
}
