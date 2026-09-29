package server

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// 每个浏览器窗口开 1~2 条 SSE 连接就够用了；上限存在的意义是"别让一条
// 泄漏的会话（或故意捣乱的人）用几千条连接把内存和文件描述符吃光"。
const (
	maxSSEClients      = 32
	maxSSEClientsPerIP = 8
)

// hubClient 是一条 SSE 连接的服务端句柄。
//
// ch 的容量固定为 1：这是"最新值槽"，不是队列。客户端慢的时候
// 新数据直接覆盖还没发出去的数据——内存有界，且永远不会拖慢广播方。
type hubClient struct {
	id uint64
	ip string
	ch chan []byte
	// closed 在服务端退出（hub.shutdown）时被关闭，处理函数据此立刻收工。
	closed chan struct{}
	// dropped 标记"这台客户端漏掉过变更"。因为推的是**变更集**而不是全量，
	// 漏一帧就可能永久丢掉一次状态翻转（例如离线），所以漏过就让它重连拿全量。
	dropped atomic.Bool
}

// hub 负责把所有在线浏览器的 SSE 连接聚合起来做广播。
type hub struct {
	log *slog.Logger

	mu      sync.Mutex
	clients map[uint64]*hubClient
	perIP   map[string]int
	nextID  atomic.Uint64
	// shuttingDown 为真时不再接受新连接（服务端正在退出）。
	shuttingDown bool
}

func newHub(log *slog.Logger) *hub {
	return &hub{log: log, clients: make(map[uint64]*hubClient), perIP: make(map[string]int)}
}

// add 登记一条 SSE 连接；超过上限或服务端正在退出时返回 nil（调用方直接拒绝）。
func (h *hub) add(ip string) *hubClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shuttingDown || len(h.clients) >= maxSSEClients || h.perIP[ip] >= maxSSEClientsPerIP {
		return nil
	}
	c := &hubClient{id: h.nextID.Add(1), ip: ip, ch: make(chan []byte, 1), closed: make(chan struct{})}
	h.clients[c.id] = c
	h.perIP[ip]++
	return c
}

// shutdown 让所有 SSE 连接立刻结束（服务端退出时调用）。
//
// 先置位 shuttingDown 再关连接：否则关掉之后新来的请求还会登记进来，
// 那个客户端的 closed 永远不会被关闭，http.Shutdown 依旧要等满宽限期。
func (h *hub) shutdown() {
	h.mu.Lock()
	h.shuttingDown = true
	clients := make([]*hubClient, 0, len(h.clients))
	for _, c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		close(c.closed)
	}
	if len(clients) > 0 {
		h.log.Info("已关闭全部实时连接", "count", len(clients))
	}
}

func (h *hub) remove(c *hubClient) {
	if c == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c.id)
	if h.perIP[c.ip] > 0 {
		h.perIP[c.ip]--
	}
	if h.perIP[c.ip] == 0 {
		delete(h.perIP, c.ip)
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// broadcast 把同一份负载发给所有客户端。
//
// 对每个客户端都是非阻塞的：槽里已有未发送的数据就丢掉旧的换成新的。
// 因此一个卡住的浏览器不会影响其它人，也不会让服务端内存增长。
func (h *hub) broadcast(payload []byte) {
	h.mu.Lock()
	targets := make([]*hubClient, 0, len(h.clients))
	for _, c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.Unlock()

	for _, c := range targets {
		select {
		case c.ch <- payload:
		default:
			// 槽满：丢弃旧值，放入最新值，并记下"这台客户端漏过东西"。
			// 推的是增量变更集，漏一帧可能永久丢掉一次状态翻转，所以
			// 处理函数看到 dropped 就会主动断开，让浏览器重连拿全量快照。
			c.dropped.Store(true)
			select {
			case <-c.ch:
			default:
			}
			select {
			case c.ch <- payload:
			default:
			}
		}
	}
}
