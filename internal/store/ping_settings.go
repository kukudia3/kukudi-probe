package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"probe/internal/protocol"
)

// 延迟探测设置用到的键。
const (
	// KeyPingTargets 是探测目标列表（JSON 数组）。
	KeyPingTargets = "ping_targets"
	// KeyPingIntervalSec 是探测间隔（秒，字符串形式的整数）。
	KeyPingIntervalSec = "ping_interval_sec"
	// KeyPingTargetSeq 是目标 ID 的自增计数。
	//
	// 为什么单独存一个键，而不是每次用 max(id)+1 算：目标被删掉后 ID 绝不能回收，
	// 否则"删掉第 2 个目标、再新建一个"会让新目标拿到旧 ID，前端（按 ID 认曲线）
	// 会把两段毫不相干的历史接成一条曲线。计数器只增不减就能杜绝这件事。
	KeyPingTargetSeq = "ping_target_seq"
)

// ErrInvalidPing 表示延迟探测设置不合法（服务端据此回 400）。
var ErrInvalidPing = errors.New("延迟探测设置不合法")

// PingTarget 是一个探测目标。
//
// 它既是设置项（存 settings 表的 JSON），也是 GET /api/v1/settings 与
// GET /api/v1/nodes/{id}/ping 里 targets[] 的形状 —— 只有这一份定义，
// 前端拿到的字段名不会因为走哪个接口而变。
//
// ID 由服务端分配、创建后永不变更（见 KeyPingTargetSeq）。
type PingTarget struct {
	ID      int64  `json:"id"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Enabled bool   `json:"enabled"`
}

// PingSettings 是一个节点的延迟探测配置（目标 + 间隔）。
type PingSettings struct {
	Targets     []PingTarget
	IntervalSec int
}

// PingTargets 读取探测目标列表。
//
// 缺省（键不存在）＝ 空列表（不探测任何目标，与老版本行为一致）。
// 脏数据一律回退，绝不让首页/设置页因为一行设置解析失败就白掉：
//   - 整个 JSON 解析不了 → 空列表；
//   - 单条记录不合法（类型不认识、host 为空、ID 重复…）→ 只丢掉那一条。
func (d *DB) PingTargets(ctx context.Context) ([]PingTarget, error) {
	raw, ok, err := d.GetSetting(ctx, KeyPingTargets)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []PingTarget{}, nil
	}

	var parsed []PingTarget
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return []PingTarget{}, nil
	}
	out := make([]PingTarget, 0, len(parsed))
	seen := make(map[int64]bool, len(parsed))
	for _, t := range parsed {
		// 先归一化再校验：库里可能存着"当时合法、现在越界"的值（例如早期版本
		// 没有把 icmp 的端口清零），归一化能救回来的就不必丢。
		t = t.normalized()
		if t.ID <= 0 {
			// ID 是曲线的身份标识，没有 ID 的记录无法与任何历史对应：
			// 只有**请求**里才允许 ID=0（表示"请服务端分配"），存库的目标必须有 ID。
			continue
		}
		if err := t.validate(); err != nil {
			continue
		}
		if seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		out = append(out, t)
		if len(out) >= protocol.MaxPingTargets {
			break
		}
	}
	return out, nil
}

// PingIntervalSec 读取探测间隔（秒）。
//
// 缺省与脏数据都回退到 protocol.DefaultPingIntervalSec：间隔是一行配置，
// 读不出来绝不能让探测功能整体不可用。
func (d *DB) PingIntervalSec(ctx context.Context) (int, error) {
	raw, ok, err := d.GetSetting(ctx, KeyPingIntervalSec)
	if err != nil {
		return 0, err
	}
	if !ok {
		return protocol.DefaultPingIntervalSec, nil
	}
	sec, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || !validPingInterval(sec) {
		return protocol.DefaultPingIntervalSec, nil
	}
	return sec, nil
}

// PingSettings 一次读出目标与间隔（下发 config 时用）。
func (d *DB) PingSettings(ctx context.Context) (PingSettings, error) {
	targets, err := d.PingTargets(ctx)
	if err != nil {
		return PingSettings{}, err
	}
	interval, err := d.PingIntervalSec(ctx)
	if err != nil {
		return PingSettings{}, err
	}
	return PingSettings{Targets: targets, IntervalSec: interval}, nil
}

// SetPingSettings 保存探测目标与间隔，返回分配好 ID 的完整列表。
//
// 规则（与接口契约一致）：
//   - 请求里带了 ID 且该 ID 在**当前已存列表**里 → 保留（前端编辑时回传，历史不断）；
//   - 没带 ID（0）、或带了一个当前不存在的 ID → 分配新 ID；
//   - 相同 type+host+port 视为同一个目标 → **自动去重**（保留先出现的那个）。
//     选自动去重而不是报 400：用户从别处粘一份列表过来、或手滑点了两次"添加"时，
//     静默合并远好过让他自己去两行相似的配置里找哪一行重复了；返回体里带回
//     去重后的完整列表，界面刷新即所见即所得。
//
// 整个"读旧列表 → 分配 ID → 写入"在一个写事务里完成：并发保存（双击提交）
// 不会分配出重复 ID。
func (d *DB) SetPingSettings(ctx context.Context, targets []PingTarget, intervalSec int) (PingSettings, error) {
	if !validPingInterval(intervalSec) {
		return PingSettings{}, fmt.Errorf("%w：探测间隔 %d 必须在 %d-%d 秒之间",
			ErrInvalidPing, intervalSec, protocol.MinPingIntervalSec, protocol.MaxPingIntervalSec)
	}

	// 校验顺序说明：label 长度用**原始输入**判断（超长要如实报错，不能被
	// 归一化的"空 label 用 host 兜底 + 截断"掩盖），其余字段先归一化再校验
	// （大小写、首尾空白属于输入习惯，不该让用户重填）。
	clean := make([]PingTarget, 0, len(targets))
	for i, t := range targets {
		if n := utf8.RuneCountInString(strings.TrimSpace(t.Label)); n > protocol.MaxPingLabelLen {
			return PingSettings{}, fmt.Errorf("%w：第 %d 个目标的名称长度 %d 超过上限 %d",
				ErrInvalidPing, i+1, n, protocol.MaxPingLabelLen)
		}
		t = t.normalized()
		if err := t.validate(); err != nil {
			return PingSettings{}, fmt.Errorf("%w：第 %d 个目标%v", ErrInvalidPing, i+1, err)
		}
		if _, dup := findDuplicate(clean, t); dup {
			continue // 相同 type+host+port：只保留先出现的那个
		}
		clean = append(clean, t)
	}
	if len(clean) > protocol.MaxPingTargets {
		return PingSettings{}, fmt.Errorf("%w：最多只能配置 %d 个目标（当前 %d 个）",
			ErrInvalidPing, protocol.MaxPingTargets, len(clean))
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return PingSettings{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// 读出已有 ID（判断"请求里的 ID 是否已存在"）与自增计数。
	existing, err := pingTargetIDs(ctx, tx)
	if err != nil {
		return PingSettings{}, err
	}
	seq, err := pingTargetSeq(ctx, tx)
	if err != nil {
		return PingSettings{}, err
	}
	// 老库没有计数键、或键被写坏时，用已有 ID 的最大值兜底：
	// 宁可跳号，也绝不回退到 0 造成 ID 复用。
	for id := range existing {
		if id > seq {
			seq = id
		}
	}

	for i := range clean {
		id := clean[i].ID
		// 前端回传的既有 ID 原样保留；其余情况（没带、带了不存在的、带了前面
		// 已经用过的）一律重新分配，避免同一个 ID 落到两个目标身上。
		if id > 0 && existing[id] && !assigned(clean[:i], id) {
			continue
		}
		seq++
		clean[i].ID = seq
	}

	if err := writePingSettings(ctx, tx, clean, intervalSec, seq); err != nil {
		return PingSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return PingSettings{}, fmt.Errorf("提交延迟探测设置失败: %w", err)
	}
	return PingSettings{Targets: clean, IntervalSec: intervalSec}, nil
}

// assigned 判断前面已经处理过的目标里是否已经用了这个 ID
// （请求里两个不同的目标带同一个 ID 时，后一个必须重新分配）。
func assigned(prev []PingTarget, id int64) bool {
	for _, t := range prev {
		if t.ID == id {
			return true
		}
	}
	return false
}

// findDuplicate 判断 target 是否与已保留的某个目标重合（type+host+port 相同）。
func findDuplicate(kept []PingTarget, target PingTarget) (PingTarget, bool) {
	for _, t := range kept {
		if t.Type == target.Type && t.Host == target.Host && t.Port == target.Port {
			return t, true
		}
	}
	return PingTarget{}, false
}

// writePingSettings 在事务里写入三个设置键。
func writePingSettings(ctx context.Context, tx *sql.Tx, targets []PingTarget, intervalSec int, seq int64) error {
	encoded, err := json.Marshal(targets)
	if err != nil {
		return fmt.Errorf("序列化探测目标失败: %w", err)
	}
	now := time.Now().Unix()
	values := map[string]string{
		KeyPingTargets:     string(encoded),
		KeyPingIntervalSec: strconv.Itoa(intervalSec),
		KeyPingTargetSeq:   strconv.FormatInt(seq, 10),
	}
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, now); err != nil {
			return fmt.Errorf("写入设置 %s 失败: %w", key, err)
		}
	}
	return nil
}

// pingTargetIDs 读出已存目标的 ID 集合（含已停用的目标）。
func pingTargetIDs(ctx context.Context, tx *sql.Tx) (map[int64]bool, error) {
	out := map[int64]bool{}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, KeyPingTargets).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取探测目标失败: %w", err)
	}
	var parsed []PingTarget
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// 脏数据当作"没有既有目标"：此时所有请求里的 ID 都会被重新分配，
		// 新 ID 从旧计数继续，因此仍然不会与历史曲线撞号。
		return out, nil
	}
	for _, t := range parsed {
		if t.ID > 0 {
			out[t.ID] = true
		}
	}
	return out, nil
}

// pingTargetSeq 读出 ID 计数器（缺失或脏数据时为 0，由调用方用 max(ID) 兜底）。
func pingTargetSeq(ctx context.Context, tx *sql.Tx) (int64, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, KeyPingTargetSeq).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取探测目标计数失败: %w", err)
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seq < 0 {
		return 0, nil
	}
	return seq, nil
}

// normalized 归一化：去空白、小写化类型、icmp 清端口、空 label 用 host 兜底。
//
// label 兜底时按上限截断（host 最长 253 字符，而 label 只允许 32）：
// 这是**派生值**，截断比报错合理；用户在请求里显式写超长 label 则会被
// validate 拒掉（见 SetPingSettings）。
//
// 兜底这一步与 labelDerivedFromHost 共用同一个函数（derivedLabel）：两处各写一遍
// 的话，"兜底怎么算"和"怎么认出兜底"迟早会分叉，而分叉的表现是地址从访客的
// 公开字段 label 里漏出去（见 LabelDerivedFromHost）。
func (t PingTarget) normalized() PingTarget {
	t.Label = strings.TrimSpace(t.Label)
	t.Host = strings.TrimSpace(t.Host)
	t.Type = strings.ToLower(strings.TrimSpace(t.Type))
	if t.Type == protocol.PingTypeICMP {
		t.Port = 0 // icmp 没有端口概念，留着只会让前端以为它有用
	}
	if t.Label == "" {
		t.Label = derivedLabel(t.Host)
	} else if utf8.RuneCountInString(t.Label) > protocol.MaxPingLabelLen {
		t.Label = truncate(t.Label, protocol.MaxPingLabelLen)
	}
	return t
}

// derivedLabel 是"空 label 用 host 兜底"这一步的**唯一**实现：去空白 + 按上限截断。
//
// 单独抽出来是为了让 normalized()（写库与读库都走它）与
// LabelDerivedFromHost（出口处认派生值）用的是同一套规则 —— 见后者的注释。
func derivedLabel(host string) string {
	label := strings.TrimSpace(host)
	if utf8.RuneCountInString(label) > protocol.MaxPingLabelLen {
		label = truncate(label, protocol.MaxPingLabelLen)
	}
	return label
}

// LabelDerivedFromHost 报告 label 是不是"由 host 派生出来的"。
//
// 为什么要问这个问题：normalized() 在 label 留空时用 host 兜底，而**写入路
// （SetPingSettings 落库前）与读取路（PingTargets）都调用它** —— 于是库里存的
// label 本身就是地址，任何调用方拿到的 Label 都是 `1.1.1.1` / `nas.home.lan`
// 这类地址。而面板的访客白名单里 label 是公开的、host 是私有的
// （见 internal/server/guest.go 的 guestPublicPingTargetFields /
// guestPrivateTargetFields）：地址会顺着公开字段漏出去。这不是边角情况 ——
// 前端新建目标时 label 的 placeholder 就写着"留空则显示地址"。
//
// 判定与 derivedLabel 严格同源（同一套去空白 + 截断）：`nas.home.lan` 这类
// 32 字符以内的 host，派生结果就是 host 本身；更长的 host 派生出来的是它的
// 前 32 个字符 —— 两种都要认得出来。
//
// 代价不对称，所以宁可多抹：用户完全可能**故意**把 label 写成与 host 一样的值
// （拿 "1.1.1.1" 当名字），那种情况也会被判成派生。抹掉的代价只是访客那边的
// 名字没了（前端回落到「目标 #id」，见 app.js 的 pingTargetLabel），
// 而漏掉的代价是把地址发给了不该看到它的人。
//
// 注意判定只看"label == host 的派生值"，不做任何存储层改写：存储层的兜底是
// 管理员侧 API 的既有行为，动它等于改管理员的输出；抹的动作放在访客出口
// （internal/server/guest.go），对存量数据与新增数据同时有效。
//
// 参数是 (label, host) 两个字符串而不是 PingTarget：除了 store.PingTarget，
// 面板侧的 pingTargetSeries / OverviewPingTarget 也是同一形状（同样是
// "label 公开、host 私有"），判定要能用在它们身上。
func LabelDerivedFromHost(label, host string) bool {
	if strings.TrimSpace(host) == "" {
		return false
	}
	return strings.TrimSpace(label) == derivedLabel(host)
}

// validate 校验一个目标（错误信息直接面向用户，所以写清楚哪里不对）。
func (t PingTarget) validate() error {
	if !protocol.IsPingType(t.Type) {
		return fmt.Errorf("的探测方式 %q 不支持（只能是 %s 或 %s）",
			t.Type, protocol.PingTypeICMP, protocol.PingTypeTCP)
	}
	if t.ID < 0 {
		return errors.New("的 ID 不能为负")
	}
	if t.Host == "" {
		return errors.New("的主机不能为空")
	}
	if utf8.RuneCountInString(t.Host) > protocol.MaxPingHostLen {
		return fmt.Errorf("的主机名长度 %d 超过上限 %d",
			utf8.RuneCountInString(t.Host), protocol.MaxPingHostLen)
	}
	for _, r := range t.Host {
		if r <= ' ' || r == 0x7f {
			return errors.New("的主机名含空白或控制字符")
		}
	}
	if utf8.RuneCountInString(t.Label) > protocol.MaxPingLabelLen {
		return fmt.Errorf("的名称长度 %d 超过上限 %d",
			utf8.RuneCountInString(t.Label), protocol.MaxPingLabelLen)
	}
	if t.Port < 0 || t.Port > protocol.MaxPingPort {
		return fmt.Errorf("的端口 %d 超出 0-%d", t.Port, protocol.MaxPingPort)
	}
	if t.Type == protocol.PingTypeTCP && t.Port < 1 {
		return fmt.Errorf("用 TCP 探测时必须填端口（1-%d）", protocol.MaxPingPort)
	}
	return nil
}

// validPingInterval 判断探测间隔是否合法。
func validPingInterval(sec int) bool {
	return sec >= protocol.MinPingIntervalSec && sec <= protocol.MaxPingIntervalSec
}
