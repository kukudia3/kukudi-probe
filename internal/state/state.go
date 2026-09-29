// Package state 保存所有节点的最新状态。
//
// 这是"1 秒级"数据唯一落脚的地方：Agent 每 1 秒上报一次，只更新内存，
// 绝不每拍写数据库（见 docs/DESIGN.md §8）。历史数据由聚合器按 10s/1m 落库。
package state

import (
	"sync"
	"sync/atomic"
	"time"

	"probe/internal/protocol"
)

// Status 是节点对外展示的状态。
type Status string

const (
	StatusUnknown Status = "unknown"
	StatusOnline  Status = "online"
	StatusStale   Status = "stale"
	StatusOffline Status = "offline"
)

// StatusFor 根据最后一次有效通信时间推算状态。
//
// 纯函数：判定逻辑只有这一处，前端展示与告警都基于它，
// 因此"界面说在线、告警说离线"这种矛盾不可能出现。
func StatusFor(lastSeen, now time.Time, staleAfter, offlineAfter time.Duration) Status {
	if lastSeen.IsZero() {
		return StatusUnknown
	}
	silent := now.Sub(lastSeen)
	switch {
	case silent < staleAfter:
		return StatusOnline
	case silent < offlineAfter:
		return StatusStale
	default:
		return StatusOffline
	}
}

// Node 是一个节点的内存状态。
//
// Metrics 里的 Disk 切片是只读的（每次上报整体替换，不做原地修改）。
type Node struct {
	NodeID     int64
	Seq        uint64
	ConnID     uint64
	Connected  bool
	LastSeen   time.Time
	Info       protocol.Info
	Metrics    protocol.Metrics
	ObservedIP string
	// Gap 是服务端观测到的序号缺口（丢失的帧数），与 Agent 上报的 Dropped 互补。
	Gap uint64
}

// Store 是所有节点内存状态的容器。
type Store struct {
	mu    sync.RWMutex
	nodes map[int64]*Node
	conns atomic.Uint64
}

// New 构造空状态容器。
func New() *Store {
	return &Store{nodes: make(map[int64]*Node)}
}

// NextConnID 分配一个连接 ID，用于区分同一节点的不同连接。
func (s *Store) NextConnID() uint64 { return s.conns.Add(1) }

// Attach 记录一个 Agent 连接建立以及它的静态信息。
func (s *Store) Attach(nodeID int64, connID uint64, info protocol.Info, observedIP string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.ensure(nodeID)
	n.ConnID = connID
	n.Connected = true
	n.Info = info
	n.ObservedIP = observedIP
	n.LastSeen = now
}

// Detach 标记连接断开；只有当前连接才能清除在线标记，避免旧连接把新连接顶掉。
func (s *Store) Detach(nodeID int64, connID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[nodeID]
	if !ok || n.ConnID != connID {
		return
	}
	n.Connected = false
}

// Update 写入一次实时指标，返回该节点新的版本号（seq，供前端做变更检测）。
//
// connGap 是服务端在当前连接上观测到的序号缺口（累计），与 Agent 自报的
// Dropped 互补：一个说明"服务端丢了帧"，一个说明"Agent 来不及发"。
func (s *Store) Update(nodeID int64, connID uint64, m protocol.Metrics, connGap uint64, now time.Time) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.ensure(nodeID)
	n.Metrics = m
	n.LastSeen = now
	n.Connected = true
	n.Gap = connGap
	if connID != 0 {
		n.ConnID = connID
	}
	n.Seq++
	return n.Seq
}

// Seed 是把持久化的"最后状态"读回内存时的输入。
type Seed struct {
	NodeID   int64
	LastSeen time.Time
	Metrics  protocol.Metrics
	Info     protocol.Info
}

// Seed 恢复一个节点的最后状态。
//
// 恢复出来的节点 Connected=false：服务端刚起来时没有任何连接，
// 状态由 LastSeen 决定（离线就是离线），不会被"美化"成在线。
func (s *Store) Seed(in Seed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.ensure(in.NodeID)
	n.LastSeen = in.LastSeen
	n.Metrics = in.Metrics
	n.Info = in.Info
	n.Connected = false
}

// Get 返回节点状态副本。
func (s *Store) Get(nodeID int64) (Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[nodeID]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// Snapshot 返回全部节点状态副本。
func (s *Store) Snapshot() []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, *n)
	}
	return out
}

// Len 返回已记录状态的节点数。
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.nodes)
}

// Delete 丢弃某个节点的内存状态（删除节点时调用）。
func (s *Store) Delete(nodeID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, nodeID)
}

func (s *Store) ensure(nodeID int64) *Node {
	n, ok := s.nodes[nodeID]
	if !ok {
		n = &Node{NodeID: nodeID}
		s.nodes[nodeID] = n
	}
	return n
}
