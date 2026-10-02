package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"probe/internal/store"
)

const (
	// sseWriteTimeout 是单次写 SSE 事件的超时：超时就断开这条浏览器连接。
	sseWriteTimeout = 10 * time.Second
	// ssePingInterval 是心跳注释间隔（保持中间代理不掐连接）。
	ssePingInterval = 15 * time.Second
	// sseRetryHint 告诉浏览器断线后重连的间隔。
	sseRetryHint = 3000
)

type streamPayload struct {
	Type    string       `json:"type"`
	TS      int64        `json:"ts"`
	Summary stateSummary `json:"summary"`
	// Nodes 是本次要发的节点：管理员那份是 []nodeDTO，访客那份是白名单 map 的切片
	// （见 guest.go 的 guestNodesJSON）。两条路共用这同一个信封，前端拿到的形状
	// 完全一样，只是访客少几个键 —— 所以这里必须是 any。
	Nodes any `json:"nodes"`
}

// handleStream 是浏览器侧的实时通道：SSE，1 Hz 推变更集。
//
// 只推"变了的节点"：50 个节点的集群在稳态下每拍只发几百字节。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	// 让 nginx 不要缓冲（否则事件会被攒起来一起发）。
	header.Set("X-Accel-Buffering", "no")

	// 这条连接是不是访客（由 guestOrAdmin 判好放在 context 里）：初始快照与
	// 后续每秒的推送都必须用同一份脱敏视图，否则 IP 会从流里漏出去。
	guest := isGuestView(r.Context())

	initial, err := s.snapshotPayload(r.Context(), guest)
	if err != nil {
		s.log.Error("构造初始快照失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	// 先占一个连接名额：超上限（或服务端正在退出）时在写任何响应之前就拒绝，
	// 避免"写了 200 再改口"。名额分角色（访客那一份更小，见 hub.add）——
	// 访客把配额占满不该让管理员连不上。
	//
	// 登记会话哈希是为了"登出之后把这条流关掉"（见 hub.revokeSession）：
	// 建连时判过的身份会在登出/改密之后失效，而这条流不会自己停下来。
	client := s.hub.add(clientIP(r), guest, s.streamSessionHash(r))
	if client == nil {
		s.log.Warn("拒绝新的实时连接（超上限或服务端正在退出）",
			"clients", s.hub.count(), "guest", guest, "ip", clientIP(r))
		s.writeJSON(w, http.StatusServiceUnavailable, errorEnvelope{Error: apiError{
			Code: "too_many_streams", Message: "实时连接数已达上限，请关闭多余的页面后重试"}})
		return
	}
	defer s.hub.remove(client)

	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", sseRetryHint); err != nil {
		return
	}
	if !s.writeSSE(w, rc, initial) {
		return
	}

	s.log.Debug("SSE 客户端已连接", "clients", s.hub.count(), "guest", guest)

	ping := time.NewTicker(s.streamPingEvery)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			s.log.Debug("SSE 客户端断开", "clients", s.hub.count())
			return
		case <-client.closed:
			// 服务端退出，或这条流的身份被撤销（登出/改密/关掉访客开关）：
			// 主动收工，浏览器收到断线后会自己重连并重新走一遍鉴权。
			s.log.Debug("实时连接被关闭", "guest", guest)
			return
		case payload := <-client.ch:
			if !s.writeSSE(w, rc, payload) {
				return
			}
			// 这台客户端之前漏过帧（变更集是增量的，漏了就可能永久缺一次状态翻转）：
			// 断开让它重连，重连时会拿到全量快照。
			if client.dropped.Swap(false) {
				s.log.Warn("实时客户端跟不上推送，断开让它重连拿全量快照")
				return
			}
		case <-ping.C:
			// 兜底复查：建连时判过的身份**会在连接存活期间失效**，而这条流
			// 不会自己停下来。主动撤销（hub.revokeSession / revokeGuests）是第一层，
			// 这里是第二层 —— 它覆盖撤销与建连之间的竞态，也是唯一能兜住
			// "会话自然过期"的一层。
			//
			// 复查是**只读**的（见 auth.SessionAlive）：心跳绝不能顺带给会话续期，
			// 否则一条挂着的长连接就成了"永不过期"的会话。
			if !s.streamIdentityValid(r, guest) {
				s.log.Debug("实时连接的身份已失效，断开", "guest", guest)
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// streamSessionHash 返回这条 SSE 请求所属会话的 Token 哈希（无会话或访客时为 nil）。
//
// 存的是哈希而不是 Cookie 原文：撤销时能拿到的也只有哈希（会话表里存的就是它），
// 而且连接对象里因此不留任何能直接冒充会话的凭据。
func (s *Server) streamSessionHash(r *http.Request) []byte {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" || len(cookie.Value) > maxTokenLen {
		return nil
	}
	return store.HashToken(cookie.Value)
}

// streamIdentityValid 重新验证一条**已建立**的 SSE 连接的身份是否仍然成立。
//
//   - 访客：开关还开着吗（关掉之后未登录的人不该再收到任何推送）；
//   - 管理员：会话还在吗（登出、改密、过期都会让它消失）。
//
// 两条都走只读查询：心跳每 15 秒一次，顺手续期等于把会话的 7 天有效期取消掉
// （见 auth.SessionAlive）。查不动时按**失效**处理（fail closed）——
// 在"要不要继续把 IP 推出去"这条路上，取不到答案时唯一安全的方向是断开，
// 而断开的代价只是浏览器自动重连一次。
func (s *Server) streamIdentityValid(r *http.Request, guest bool) bool {
	if guest {
		return s.guestAccessEnabled(r.Context())
	}
	return s.auth.SessionAlive(r)
}

// writeSSE 写一个事件并立刻 flush；返回 false 表示这条连接该结束了。
func (s *Server) writeSSE(w http.ResponseWriter, rc *http.ResponseController, payload []byte) bool {
	// 单次写超时：慢客户端在这里被断开，不会拖住服务端。
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprintf(w, "event: nodes\ndata: %s\n\n", payload); err != nil {
		return false
	}
	if err := rc.Flush(); err != nil {
		return false
	}
	return true
}

// snapshotPayload 构造全量快照（新客户端连上时先发这一份）。
//
// guest 决定用哪一份视图：访客那份过白名单（guestNodesJSON），
// **与每秒推送用的是同一个函数** —— 两条路各写一遍迟早会分叉，
// 而分叉的表现就是"IP 只在重连时露一下"。
func (s *Server) snapshotPayload(ctx context.Context, guest bool) ([]byte, error) {
	nodes, err := s.currentNodes(ctx)
	if err != nil {
		return nil, err
	}
	if guest {
		return encodePayload(guestNodesJSON(nodes), summarize(nodes))
	}
	return encodePayload(nodes, summarize(nodes))
}

// realtimeLoop 是服务端唯一的 1 Hz 循环：评估告警 + （有浏览器时）推送变更集。
//
// 两者共用同一份 currentNodes() 结果，因此即使既开着告警又开着页面，
// 每秒也只做一次"查配置 + 汇总流量"的读取。
func (s *Server) realtimeLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	seen := make(map[int64]uint64)
	seenStatus := make(map[int64]string)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		nodes, err := s.currentNodes(ctx)
		if err != nil {
			s.log.Error("读取节点视图失败", "err", err)
			continue
		}

		// 告警评估：纯内存判断，只有状态变化才写库。
		s.evaluateAlerts(ctx, nodes)

		if s.hub.count() == 0 {
			continue
		}

		changed := make([]nodeDTO, 0, len(nodes))
		for _, n := range nodes {
			st, ok := s.state.Get(n.ID)
			seq := uint64(0)
			if ok {
				seq = st.Seq
			}
			prevSeq, seenBefore := seen[n.ID]
			// 状态是随时间变化的（在线→抖动→离线），即使没有新上报也要推。
			if !seenBefore || seq > prevSeq || seenStatus[n.ID] != n.Status {
				changed = append(changed, n)
			}
			seen[n.ID] = seq
			seenStatus[n.ID] = n.Status
		}
		if len(changed) == 0 {
			continue
		}
		// 汇总始终基于全部节点，前端直接覆盖显示即可。
		//
		// 管理员与访客各编码一份：**同一批变更集**，只是访客那份过了白名单
		// （见 guest.go 的 guestNodesJSON）。只有真的有人要那一份时才做这次转换
		// —— 没人连着的时候每秒白编码一次是纯浪费。
		summary := summarize(nodes)
		if s.hub.hasGuest() {
			guestPayload, err := encodePayload(guestNodesJSON(changed), summary)
			if err != nil {
				s.log.Error("编码访客实时推送失败", "err", err)
			} else {
				s.hub.broadcastTo(true, guestPayload)
			}
		}
		payload, err := encodePayload(changed, summary)
		if err != nil {
			s.log.Error("编码实时推送失败", "err", err)
			continue
		}
		s.hub.broadcastTo(false, payload)
	}
}

// encodePayload 组装一条 SSE 数据（nodes 为本次要发的节点，summary 为全量汇总）。
//
// nodes 是 any：管理员那份是 []nodeDTO，访客那份是白名单 map 的切片。
func encodePayload(nodes any, summary stateSummary) ([]byte, error) {
	return json.Marshal(streamPayload{
		Type:    "nodes",
		TS:      time.Now().Unix(),
		Summary: summary,
		Nodes:   nodes,
	})
}
