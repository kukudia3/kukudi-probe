package server

import (
	"sync"
	"time"

	"probe/internal/state"
)

// onlineTracker 记「每个节点最近一次进入 online 状态的时刻」，用来算「连续在线时长」。
//
// 它与另外两个时长概念是三回事，别混：
//   - uptime_sec（开机时长）：来自 /proc/uptime，机器重启就归零，而这期间节点
//     一秒都没掉线；反过来机器一年没重启、中途断网三天，它照样显示一年；
//   - 可用率（详情页的 24h/7d）：一段时间里"上报有多完整"的比例；
//   - 这里：**当下**这一段连续在线从什么时候开始，掉线就清零重来。
//
// 判定只有一处：状态由 buildNodeDTO 按 last_seen + StaleAfter/OfflineAfter 算出
// （见 dto.go 的 statusOf），本类型只消费那个结果 —— 不另起一套判定，否则界面上
// 会出现"卡片说在线、时长却在清零"这种自相矛盾。
//
// 起点存库（node_runtime.online_since，迁移 0005）：服务端重启后如果那个节点
// 仍然在线，接着原来的起点累加，而不是从零开始。
type onlineTracker struct {
	mu sync.Mutex
	// since[nodeID] = 进入在线状态的 Unix 秒。**没有键**表示"当前不在线"，
	// 而不是存一个 0：两种写法混用迟早会有一处忘了判断。
	since map[int64]int64
}

func newOnlineTracker() *onlineTracker {
	return &onlineTracker{since: make(map[int64]int64)}
}

// observe 用一次观测到的状态推进记账，返回该节点此刻的连续在线秒数（不在线为 0）。
//
// 语义：
//   - online 且还没有起点 → 起点 = 现在。此前它在线多久无从得知，只能从"被观测到"
//     开始算：往前追溯是猜，猜出来的"在线 18 天"比从 0 开始更糟；
//   - online 且已有起点 → 起点不动，时长继续累加；
//   - 非 online（stale/offline/unknown）→ 起点清零，"连续"就此中断；
//     下次再上线时从那一刻重新起算。
//
// 1 Hz 的实时循环每秒都会经 currentNodes → dtoFor 调到这里，所以这里只做
// map 查找与整数比较：不分配、不写库（写库跟着每分钟的运行态落盘走，
// 见 pipeline.go 的 flushRuntime）。
func (t *onlineTracker) observe(nodeID int64, status state.Status, now time.Time) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	if status != state.StatusOnline {
		delete(t.since, nodeID)
		return 0
	}
	nowSec := now.Unix()
	since, ok := t.since[nodeID]
	// 起点晚于现在（系统时钟被往回校过、或库里的值被改坏）时重新起算：
	// 不重算的话时长会一直卡在 0，直到墙钟追上那个未来时刻。
	if !ok || since <= 0 || since > nowSec {
		since = nowSec
		t.since[nodeID] = since
	}
	if sec := nowSec - since; sec > 0 {
		return uint64(sec)
	}
	return 0
}

// seed 在服务端启动时恢复一个节点的在线起点。
//
// 只有"重启后**仍然在线**"的节点才继承起点：status 由调用方按同一套判定算好
// （last_seen + 阈值），已经抖动/离线的节点一律不恢复 —— 否则重启会把停机那段时间
// 也算成"在线"，而那正是这个数字要如实反映的东西。
func (t *onlineTracker) seed(nodeID int64, status state.Status, since int64) {
	if status != state.StatusOnline || since <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.since[nodeID] = since
}

// sinceOf 返回一个节点的在线起点（0 = 当前不在线）。给运行态落盘用。
func (t *onlineTracker) sinceOf(nodeID int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.since[nodeID]
}

// forget 丢弃一个节点的记账（节点被删除时调用，避免内存里留下永远不用的键）。
func (t *onlineTracker) forget(nodeID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.since, nodeID)
}
