package server

import (
	"bytes"
	"log/slog"
	"sync"
	"sync/atomic"
)

// 每个浏览器窗口开 1~2 条 SSE 连接就够用了；上限存在的意义是"别让一条
// 泄漏的会话（或故意捣乱的人）用几千条连接把内存和文件描述符吃光"。
//
// 管理员与访客是**两份互不挤占的配额**（理由见 hub.add）。
const (
	maxSSEClients      = 32
	maxSSEClientsPerIP = 8

	// maxGuestSSEClients / maxGuestSSEClientsPerIP 是访客那一份，明显更小。
	//
	// 为什么要分开而不是共用：访客分支不做同源校验（curl 一行就能连），
	// 也就是说**任何人都能把访客配额占满**；共用一份时，占满的直接后果是
	// 管理员自己被 503 拒之门外 —— 一个纯 curl 就能让面板失去实时视图。
	// 而访客的请求限流（300 次/分钟）对"建连后长挂"完全无效：它数的是请求数。
	//
	// 20 / 2 这两个数怎么来的：真人访客一个标签页只开 1 条流（首页与详情页共用
	// 同一条，见 app.js 的 connectStream），2 条/IP 容得下"开两个标签页"；
	// 全局 20 意味着就算被爬虫盯上，最多也只牺牲 20 条访客名额，
	// 管理员那 32 条一条都不会少。
	maxGuestSSEClients      = 20
	maxGuestSSEClientsPerIP = 2
)

// hubClient 是一条 SSE 连接的服务端句柄。
//
// ch 的容量固定为 1：这是"最新值槽"，不是队列。客户端慢的时候
// 新数据直接覆盖还没发出去的数据——内存有界，且永远不会拖慢广播方。
type hubClient struct {
	id uint64
	ip string
	ch chan []byte
	// guest 为真表示这条连接是**访客**（没有会话、而「允许访客查看」开着）。
	//
	// 为什么角色记在连接上：广播的那一份负载是**按角色编码**的（访客那份过了
	// 白名单，见 guest.go），而 hub 是"一份负载发给所有人"的结构。把角色放在
	// 客户端上，广播时才能把对应的那一份发给对应的人 —— 否则 IP 会顺着每秒
	// 推送的快照漏给访客（这是这个功能里最容易漏的一条路径）。配额也按它分。
	guest bool
	// session 是这条连接所属会话的 Token **哈希**（访客连接为 nil）。
	//
	// 为什么记哈希而不是原文：连接上只需要"这条流属于哪个会话"这个身份，
	// 而登出/改密时手里也只有哈希（会话表存的就是哈希）。存哈希还顺带保证
	// "连接对象里没有任何能直接冒充会话的凭据"。
	session []byte
	// closed 在服务端退出（hub.shutdown）或身份被撤销（登出/改密/关掉访客开关）
	// 时被关闭，处理函数据此立刻收工。**只能经 close() 关**。
	closed chan struct{}
	// closeOnce 让 close() 幂等（见 close 的注释）。
	closeOnce sync.Once
	// dropped 标记"这台客户端漏掉过变更"。因为推的是**变更集**而不是全量，
	// 漏一帧就可能永久丢掉一次状态翻转（例如离线），所以漏过就让它重连拿全量。
	dropped atomic.Bool
}

// close 让这条连接立刻结束（幂等）。
//
// 为什么必须幂等：关闭有两个来源 —— 服务端退出（shutdown）与身份被撤销
// （登出/改密/关掉访客开关），两者完全可能撞在同一时刻（管理员一边点退出、
// 服务端一边在关机）。而 `close(chan)` 关第二次会 panic，所以全部路径都收口到
// 这一个方法上，由 sync.Once 保证只关一次。
func (c *hubClient) close() {
	c.closeOnce.Do(func() { close(c.closed) })
}

// hub 负责把所有在线浏览器的 SSE 连接聚合起来做广播。
type hub struct {
	log *slog.Logger

	mu      sync.Mutex
	clients map[uint64]*hubClient
	// perIP / guestPerIP 是**分开的两份**每 IP 账，admins / guests 是两份总数：
	// 减的时候必须与加的时候落在同一份账上（见 remove）。
	perIP      map[string]int
	guestPerIP map[string]int
	admins     int
	guests     int
	nextID     atomic.Uint64
	// shuttingDown 为真时不再接受新连接（服务端正在退出）。
	shuttingDown bool
}

func newHub(log *slog.Logger) *hub {
	return &hub{
		log:        log,
		clients:    make(map[uint64]*hubClient),
		perIP:      make(map[string]int),
		guestPerIP: make(map[string]int),
	}
}

// add 登记一条 SSE 连接；超过**该角色**的上限、或服务端正在退出时返回 nil
// （调用方直接拒绝）。
//
// guest 说明这条连接是不是访客（见 hubClient.guest），sessionHash 是它所属会话的
// Token 哈希（访客传 nil）—— 登出时靠它把"这一个会话"的流关掉。
//
// 配额为什么要按角色分：见 maxGuestSSEClients 的注释。
func (h *hub) add(ip string, guest bool, sessionHash []byte) *hubClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shuttingDown {
		return nil
	}
	if guest {
		if h.guests >= maxGuestSSEClients || h.guestPerIP[ip] >= maxGuestSSEClientsPerIP {
			return nil
		}
	} else if h.admins >= maxSSEClients || h.perIP[ip] >= maxSSEClientsPerIP {
		return nil
	}
	c := &hubClient{
		id: h.nextID.Add(1), ip: ip, guest: guest, session: sessionHash,
		ch: make(chan []byte, 1), closed: make(chan struct{}),
	}
	h.clients[c.id] = c
	if guest {
		h.guests++
		h.guestPerIP[ip]++
	} else {
		h.admins++
		h.perIP[ip]++
	}
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

	// 走 hubClient.close 而不是裸的 close(c.closed)：撤销路径（登出/改密/关开关）
	// 完全可能同时关掉同一批连接，裸 close 撞上就是二次关闭 panic。
	for _, c := range clients {
		c.close()
	}
	if len(clients) > 0 {
		h.log.Info("已关闭全部实时连接", "count", len(clients))
	}
}

// closeStreams 按条件关掉**已经建立**的连接，返回关掉的数量。
//
// 为什么需要它：SSE 只在**建连那一刻**判过一次身份（见 guest.go 的 guestOrAdmin），
// 之后每秒往外推数据。而身份不是一次性的 —— 会话会被登出、被改密撤销、会自然过期，
// 「允许访客查看」会被关掉。少了这一层，一条已经建立的管理员流会在登出之后
// **继续**每秒收到完整的 nodeDTO（含 observed_ip / local_ip / note / boot_id）。
//
// 关的是 closed 通道（经 hubClient.close，幂等），处理函数的 select 立刻返回，
// 它自己的 defer hub.remove 会把名额还回去。
func (h *hub) closeStreams(reason string, match func(*hubClient) bool) int {
	h.mu.Lock()
	targets := make([]*hubClient, 0, len(h.clients))
	for _, c := range h.clients {
		if match(c) {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	for _, c := range targets {
		c.close()
	}
	if len(targets) > 0 {
		h.log.Info("已撤销实时连接", "reason", reason, "count", len(targets))
	}
	return len(targets)
}

// revokeSession 关掉某个会话（按 Token 哈希认）已经建立的全部管理员连接（登出）。
func (h *hub) revokeSession(reason string, tokenHash []byte) int {
	if len(tokenHash) == 0 {
		return 0
	}
	return h.closeStreams(reason, func(c *hubClient) bool {
		return !c.guest && bytes.Equal(c.session, tokenHash)
	})
}

// revokeAdmins 关掉**全部**管理员连接（改密码）。
//
// 为什么不是"只关被注销的那些会话"：DeleteSessionsExcept 只告诉我们注销了几个，
// 拿不到它们的哈希，而"当前会话"是唯一知道哈希的那一条。多关一条当前会话的代价
// 只是它自动重连一次（Cookie 仍然有效，重连时会重新走完整鉴权），
// 而少关一条的代价是一条已经不该存在的流继续收 IP —— 两个方向不对称。
func (h *hub) revokeAdmins(reason string) int {
	return h.closeStreams(reason, func(c *hubClient) bool { return !c.guest })
}

// revokeGuests 关掉全部访客连接（「允许访客查看」被关掉时调用）。
func (h *hub) revokeGuests(reason string) int {
	return h.closeStreams(reason, func(c *hubClient) bool { return c.guest })
}

// remove 注销一条连接，并把它占的那份配额**对称地**还回去。
//
// 递减必须与 add 严格对称（同一个角色、同一份 perIP 账）：漏减哪一边，那一份
// 名额就永久泄漏 —— 表现是"页面全关了、重开却连不上"，而且只会随运行时间越来越
// 严重。所以这里按 c.guest 分成两支，各自减自己那一份；不在册的连接直接返回，
// 再减一次就是凭空多还一份配额。
func (h *hub) remove(c *hubClient) {
	if c == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c.id]; !ok {
		return
	}
	delete(h.clients, c.id)
	if c.guest {
		if h.guests > 0 {
			h.guests--
		}
		if h.guestPerIP[c.ip] > 0 {
			h.guestPerIP[c.ip]--
		}
		if h.guestPerIP[c.ip] == 0 {
			delete(h.guestPerIP, c.ip)
		}
		return
	}
	if h.admins > 0 {
		h.admins--
	}
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

// countByRole 报告当前管理员与访客各有多少条连接（只被测试用）。
func (h *hub) countByRole() (admins, guests int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.admins, h.guests
}

// hasGuest 报告当前有没有访客连接。
//
// realtimeLoop 用它决定"要不要额外编码一份脱敏负载"：没有访客连着时那次转换
// 纯属白做（而它每秒都要做一次）。
func (h *hub) hasGuest() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if c.guest {
			return true
		}
	}
	return false
}

// broadcastTo 把负载发给**某一类**客户端（guest 决定是访客还是管理员）。
//
// 为什么要按角色分开：同一个时刻可能既有管理员又有访客连着，而给访客的那份
// 必须已经脱敏（见 guest.go）。两份负载由调用方分别编码（realtimeLoop），
// 这里只负责"把哪一份发给谁"。
//
// 对每个客户端都是非阻塞的：槽里已有未发送的数据就丢掉旧的换成新的。
// 因此一个卡住的浏览器不会影响其它人，也不会让服务端内存增长。
func (h *hub) broadcastTo(guest bool, payload []byte) {
	h.mu.Lock()
	targets := make([]*hubClient, 0, len(h.clients))
	for _, c := range h.clients {
		if c.guest == guest {
			targets = append(targets, c)
		}
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
