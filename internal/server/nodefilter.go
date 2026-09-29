package server

import "sync"

// nodeFilter 维护"哪些节点的数据可以入库"。
//
// 为什么需要它：samples_10s / samples_1m / traffic_daily 都有指向 nodes(id) 的外键。
// 节点被删除后，内存里可能还残留着它的一帧数据（连接刚被断开、或一份快照早已取走），
// 这些数据一旦进库就会触发 FOREIGN KEY 约束失败，让**整批**落盘事务回滚——
// 表现是"删掉一个节点之后，所有节点的历史与流量都不再更新"。
//
// 两套集合：
//   - known：数据库里当前存在的节点（每次读出节点列表后整体替换）；
//   - denied：明确被删除的节点。删除是立刻生效的，即使还没读到过完整列表。
//
// known 为 nil 表示"还没拿到节点列表"，此时除 denied 之外一律放行（避免启动瞬间丢数据）。
type nodeFilter struct {
	mu     sync.RWMutex
	known  map[int64]bool
	denied map[int64]bool
}

// set 用一份完整的节点列表替换集合。
func (f *nodeFilter) set(ids []int64) {
	if len(ids) == 0 {
		return
	}
	known := make(map[int64]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	f.mu.Lock()
	f.known = known
	f.mu.Unlock()
}

// allow 让单个节点立刻通过过滤（新建节点时调用，不必等下一次列表刷新）。
func (f *nodeFilter) allow(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.denied, id)
	if f.known == nil {
		// 还没有完整列表：保持"不过滤"，否则会把其它节点的数据全挡掉。
		return
	}
	f.known[id] = true
}

// drop 立刻把某个节点拉黑（删除节点时调用）。
func (f *nodeFilter) drop(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.known, id)
	if f.denied == nil {
		f.denied = make(map[int64]bool)
	}
	f.denied[id] = true
}

// allows 报告某个节点的数据是否可以入库。
func (f *nodeFilter) allows(id int64) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.denied[id] {
		return false
	}
	if f.known == nil {
		return true
	}
	return f.known[id]
}

// prune 从给定的映射里删除"不允许"的键（用于清理内存里的残留状态）。
func (f *nodeFilter) prune[T any](m map[int64]T) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for id := range m {
		if f.denied[id] {
			delete(m, id)
			continue
		}
		if f.known != nil && !f.known[id] {
			delete(m, id)
		}
	}
}
