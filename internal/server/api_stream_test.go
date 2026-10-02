package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
)

func TestHubBroadcastKeepsOnlyLatestForSlowClient(t *testing.T) {
	h := newHub(slog.New(slog.DiscardHandler))
	c := h.add("10.0.0.1", false, nil)
	if c == nil {
		t.Fatal("第一条连接应当被接受")
	}

	h.broadcastTo(false, []byte("first"))
	h.broadcastTo(false, []byte("second"))
	h.broadcastTo(false, []byte("third"))

	select {
	case got := <-c.ch:
		if string(got) != "third" {
			t.Fatalf("慢客户端应当只保留最新值，实际 %q", got)
		}
	default:
		t.Fatal("槽里应当有值")
	}
	select {
	case got := <-c.ch:
		t.Fatalf("槽容量必须是 1，却读到了第二个值 %q", got)
	default:
	}

	if h.count() != 1 {
		t.Fatalf("客户端数 = %d", h.count())
	}
	h.remove(c)
	if h.count() != 0 {
		t.Fatalf("移除后客户端数 = %d", h.count())
	}
	// 移除后配额也要还回去，否则同一个 IP 会被永久卡住。
	if again := h.add("10.0.0.1", false, nil); again == nil {
		t.Fatal("移除后应当还能再连")
	}

	// 没有客户端时广播不应 panic。
	h.broadcastTo(false, []byte("nobody"))
}

func TestBroadcastDoesNotBlockOnStuckClient(t *testing.T) {
	h := newHub(slog.New(slog.DiscardHandler))
	stuck := h.add("10.0.0.1", false, nil)
	fast := h.add("10.0.0.2", false, nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.broadcastTo(false, []byte("payload"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("广播被卡住的客户端阻塞了")
	}

	// 卡住的客户端槽里是最新值；正常客户端也能拿到值。
	select {
	case <-stuck.ch:
	default:
		t.Fatal("卡住的客户端槽里也应当有最新值")
	}
	select {
	case <-fast.ch:
	default:
		t.Fatal("正常客户端应当收到数据")
	}
}

// sseReader 读 SSE 流并把每个 data 负载推给 channel。
type sseReader struct {
	events chan []byte
	errors chan error
}

func startSSE(t *testing.T, h *authHarness) (*sseReader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.ts.URL+"/api/v1/stream", nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("连接 SSE 失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("SSE 状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("SSE Content-Type = %q", ct)
	}

	r := &sseReader{events: make(chan []byte, 16), errors: make(chan error, 1)}
	go func() {
		defer close(r.events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			r.events <- []byte(strings.TrimPrefix(line, "data: "))
		}
		if err := scanner.Err(); err != nil {
			r.errors <- err
		}
	}()

	stop := func() {
		cancel()
		_ = resp.Body.Close()
	}
	return r, stop
}

func (r *sseReader) next(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case payload, ok := <-r.events:
		if !ok {
			t.Fatal("SSE 流已关闭")
		}
		var out map[string]any
		if err := json.Unmarshal(payload, &out); err != nil {
			t.Fatalf("解析 SSE 数据失败: %v（%s）", err, payload)
		}
		return out
	case err := <-r.errors:
		t.Fatalf("SSE 读取失败: %v", err)
	case <-time.After(timeout):
		t.Fatal("等待 SSE 事件超时")
	}
	return nil
}

// nextMatching 持续读取事件直到满足条件：SSE 是异步的（1 Hz 广播），
// 用"等到出现目标状态"代替"读数第几个事件"才不会偶发失败。
func (r *sseReader) nextMatching(t *testing.T, timeout time.Duration, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		payload := r.next(t, time.Until(deadline))
		if match(payload) {
			return payload
		}
	}
	t.Fatalf("等待 SSE 事件超时: %s", what)
	return nil
}

// firstNode 取出负载里的第一个节点。
func firstNode(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	nodes, _ := payload["nodes"].([]any)
	if len(nodes) == 0 {
		return nil
	}
	node, _ := nodes[0].(map[string]any)
	return node
}

// anyNodeKey 报告负载里的节点有没有带着某个键（不依赖 *testing.T，
// 好在"检查一批帧"的循环里用）。
func anyNodeKey(payload map[string]any, key string) bool {
	nodes, _ := payload["nodes"].([]any)
	for _, raw := range nodes {
		node, _ := raw.(map[string]any)
		if _, ok := node[key]; ok {
			return true
		}
	}
	return false
}

// streamPrivateKeys 是管理员那一份**独有的**字段：一条已经不该存在的流继续
// 收到它们，就是"身份撤销没生效"的直接证据。
var streamPrivateKeys = []string{"observed_ip", "local_ip", "local_ip6", "note", "boot_id"}

// tryNext 尝试读一个事件；超时或流已关闭时返回 false（不 Fatal）。
func (r *sseReader) tryNext(timeout time.Duration) (map[string]any, bool) {
	select {
	case payload, ok := <-r.events:
		if !ok {
			return nil, false
		}
		var out map[string]any
		if err := json.Unmarshal(payload, &out); err != nil {
			return nil, false
		}
		return out, true
	case <-r.errors:
		return nil, false
	case <-time.After(timeout):
		return nil, false
	}
}

// waitQuiet 读到"连续 quiet 时间内没有新帧"为止 —— 把已经排上队的在途帧读干净。
//
// 为什么需要它：要断言的是"撤销之后不再有**新的**帧"，而撤销那一刻可能正好有
// 一帧在路上（它是撤销之前合法发出的）。先等一段安静期，断言才不会偶发变红。
// quiet 必须大于服务端 1 Hz 的推送节奏，否则会在两拍之间"误判安静"。
func (r *sseReader) waitQuiet(quiet, maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if _, ok := r.tryNext(quiet); !ok {
			return
		}
	}
}

// collectUntilClosed 一直读事件直到**服务端把流关掉**（events 通道关闭），
// 返回关掉之前收到的全部事件。超过 timeout 还没关就 Fatal。
//
// 注意 scanner 在服务端关闭连接时可能报 "unexpected EOF"，那一份会进 errors
// 通道：那是**预期**的，不能当成失败，所以把它置 nil（nil channel 永远阻塞）。
func (r *sseReader) collectUntilClosed(t *testing.T, timeout time.Duration) []map[string]any {
	t.Helper()
	var out []map[string]any
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("等待实时连接被服务端关闭超时（%s）；期间收到 %d 个事件（撤销没生效？）",
				timeout, len(out))
		}
		select {
		case payload, ok := <-r.events:
			if !ok {
				return out
			}
			var parsed map[string]any
			if err := json.Unmarshal(payload, &parsed); err != nil {
				t.Fatalf("解析 SSE 数据失败: %v（%s）", err, payload)
			}
			out = append(out, parsed)
		case <-r.errors:
			r.errors = nil
		case <-time.After(remaining):
			t.Fatalf("等待实时连接被服务端关闭超时（%s）；期间收到 %d 个事件（撤销没生效？）",
				timeout, len(out))
		}
	}
}

// assertNoPrivateFrames 断言一串 SSE 帧里没有一帧带着私有字段。
func assertNoPrivateFrames(t *testing.T, what string, frames []map[string]any) {
	t.Helper()
	for i, payload := range frames {
		for _, key := range streamPrivateKeys {
			if anyNodeKey(payload, key) {
				t.Errorf("%s：第 %d 个帧里还有 %s（身份已撤销，推送却还在继续）：%v",
					what, i+1, key, payload)
			}
		}
	}
}

// loginSecondDevice 在一个**独立**的客户端上再登录一次（第二条会话）。
//
// 用途：需要"管理员"与"访客"同时在线时，两边的 Cookie 必须互不干扰
// —— h.anonymousClient 换掉的是同一个字段，只能有一个身份。
func loginSecondDevice(t *testing.T, h *authHarness) *authHarness {
	t.Helper()
	second := h.cloneClient(t)
	status, body := second.post(t, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": h.password,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("第二台设备登录失败: %d %v", status, body)
	}
	second.csrf, _ = body["csrf_token"].(string)
	if second.csrf == "" {
		t.Fatal("第二台设备登录后没有 CSRF Token")
	}
	return second
}

// TestAdminStreamIsRevokedOnLogout 钉住必修 2 的第一层（主动撤销）。
//
// 场景：管理员开着面板（SSE 每秒推完整 nodeDTO），在别处/本处点了退出登录。
// 会话没了，但那条**已经建立**的流不会再经过一次鉴权 —— 不主动关，它会继续
// 每秒把 observed_ip / local_ip / note / boot_id 推给一个已经不是管理员的浏览器。
func TestAdminStreamIsRevokedOnLogout(t *testing.T) {
	h := newAuthHarness(t)
	guestSample(t, h)

	reader, stop := startSSE(t, h)
	defer stop()

	// 快照里必须有私有字段：这是"这条流确实在发 IP"的证据。少了它，
	// 下面"关掉之后不再收到"就成了一条空断言。
	snapshot := reader.next(t, 5*time.Second)
	if !anyNodeKey(snapshot, "observed_ip") || !anyNodeKey(snapshot, "note") {
		t.Fatalf("管理员的快照里应当有 observed_ip / note：%v", snapshot)
	}
	// 把在途的帧读干净（撤销之前合法发出的那些不算"撤销没生效"）。
	reader.waitQuiet(1300*time.Millisecond, 6*time.Second)
	if admins, _ := h.srv.hub.countByRole(); admins != 1 {
		t.Fatalf("hub 里的管理员连接数 = %d，期望 1（上面等安静期时流意外断了？）", admins)
	}

	status, _ := h.post(t, "/api/v1/auth/logout", nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("登出状态码 = %d，期望 204", status)
	}

	// **2 秒内**被服务端断开，且断开之前一个带私有字段的帧都不能再来。
	assertNoPrivateFrames(t, "登出之后", reader.collectUntilClosed(t, 2*time.Second))

	// 断开只是"这一条没了"；真正要钉的是"这个会话再也拿不到流"。
	// 浏览器会立刻自动重连一次，那次必须被 401 挡住。
	if code := probeStatus(t, h.client, http.MethodGet, h.ts.URL+"/api/v1/stream"); code != http.StatusUnauthorized {
		t.Errorf("登出之后重连实时流应当 401，实际 %d", code)
	}
	// 名额也要还回去：泄漏的话页面全关了也连不上新的。
	if n := h.srv.hub.count(); n != 0 {
		t.Errorf("流关闭之后 hub 里还剩 %d 条连接（remove 没跑到？）", n)
	}
}

// TestGuestStreamIsRevokedWhenSwitchTurnsOff 钉住必修 2 的另一半：
// 关掉「允许访客查看」之后，**已经连上**的访客流也必须当场断开。
func TestGuestStreamIsRevokedWhenSwitchTurnsOff(t *testing.T) {
	h := newAuthHarness(t)
	guestSample(t, h)
	guestOn(t, h)

	admin := loginSecondDevice(t, h)
	h.anonymousClient(t)
	reader, stop := startSSE(t, h)
	defer stop()
	if n := firstNode(t, reader.next(t, 5*time.Second)); n == nil {
		t.Fatal("访客的快照里没有节点")
	}
	if _, guests := h.srv.hub.countByRole(); guests != 1 {
		t.Fatalf("hub 里的访客连接数 = %d，期望 1", guests)
	}

	status, body := admin.put(t, "/api/v1/settings/guest", map[string]any{"enabled": false})
	if status != http.StatusOK {
		t.Fatalf("关闭访客开关失败: %d %v", status, body)
	}

	reader.collectUntilClosed(t, 2*time.Second)
	if code := probeStatus(t, h.client, http.MethodGet, h.ts.URL+"/api/v1/stream"); code != http.StatusUnauthorized {
		t.Errorf("关掉开关之后访客重连实时流应当 401，实际 %d", code)
	}
}

// TestStreamHeartbeatRechecksAdminSession 钉住必修 2 的第二层（心跳兜底复查）。
//
// 主动撤销覆盖不了全部情况：会话**自然过期**、被别处直接删掉（运维脚本、
// 另一个进程）时没有任何人通知 hub。心跳里那次只读复查是唯一能兜住它们的。
//
// 这里刻意**绕过** HTTP 处理函数直接删会话行 —— 走登出接口的话，主动撤销
// 那一层会先把流关掉，这条用例就测不到复查了。
func TestStreamHeartbeatRechecksAdminSession(t *testing.T) {
	h := newAuthHarness(t)
	guestSample(t, h)
	// 心跳调到 50ms：这一条验的就是心跳里的那段逻辑，而默认 15 秒没法在用例里等。
	h.srv.streamPingEvery = 50 * time.Millisecond

	reader, stop := startSSE(t, h)
	defer stop()
	if !anyNodeKey(reader.next(t, 5*time.Second), "observed_ip") {
		t.Fatal("管理员的快照里应当有 observed_ip")
	}

	if _, err := h.srv.db.DeleteSessionsExcept(context.Background(), nil); err != nil {
		t.Fatalf("直接删除会话: %v", err)
	}

	// 会话没了 → 下一次心跳复查就该断开，并且期间不再有带私有字段的帧。
	assertNoPrivateFrames(t, "会话消失之后", reader.collectUntilClosed(t, 2*time.Second))
}

// TestStreamHeartbeatRechecksGuestSwitch 同上，访客那一侧：开关被关掉时
// 心跳复查必须发现（这里同样绕过处理函数直接改开关，见 setGuestAccess）。
func TestStreamHeartbeatRechecksGuestSwitch(t *testing.T) {
	h := newAuthHarness(t)
	guestSample(t, h)
	guestOn(t, h)
	h.anonymousClient(t)
	h.srv.streamPingEvery = 50 * time.Millisecond

	reader, stop := startSSE(t, h)
	defer stop()
	if firstNode(t, reader.next(t, 5*time.Second)) == nil {
		t.Fatal("访客的快照里没有节点")
	}

	// setGuestAccess 只写库 + 更新缓存，**不**撤销连接（撤销在 HTTP 处理函数里）——
	// 于是只有心跳复查能发现这件事。
	if err := h.srv.setGuestAccess(context.Background(), false); err != nil {
		t.Fatalf("直接关闭访客开关: %v", err)
	}
	reader.collectUntilClosed(t, 2*time.Second)
}

// TestRepeatedShutdownDoesNotPanic 钉住"关连接只能关一次"。
//
// closed 通道有两个关闭来源（服务端退出、身份撤销），两者完全可能撞在一起。
// 全部路径都走 hubClient.close（sync.Once），所以这里连着关三次也不该 panic ——
// 裸 close(c.closed) 的话第一次就炸了。
func TestRepeatedShutdownDoesNotPanic(t *testing.T) {
	h := newHub(slog.New(slog.DiscardHandler))
	admin := h.add("10.0.0.1", false, []byte("session-a"))
	guest := h.add("10.0.0.2", true, nil)
	if admin == nil || guest == nil {
		t.Fatal("两条连接都应当被接受")
	}

	h.revokeSession("logout", []byte("session-a"))
	admin.close() // 撤销与关闭撞在一起
	h.shutdown()
	h.shutdown()

	select {
	case <-admin.closed:
	default:
		t.Fatal("被撤销的连接应当已经关闭")
	}
	select {
	case <-guest.closed:
	default:
		t.Fatal("服务端退出时访客连接也应当关闭")
	}
	// shutdown 之后不再接受新连接。
	if c := h.add("10.0.0.3", false, nil); c != nil {
		t.Fatal("正在退出的 hub 不该再接受新连接")
	}
}

// TestGuestStreamsHaveTheirOwnQuota 钉住必修 3 的配额算术（hub 层，不经过 HTTP）。
//
// httptest 里所有连接的来源都是 127.0.0.1，造不出 20 个不同的 IP，所以
// "全局访客配额"只能在 hub 这一层验；走 HTTP 的那部分见
// TestGuestStreamFloodDoesNotLockOutAdmin。
func TestGuestStreamsHaveTheirOwnQuota(t *testing.T) {
	h := newHub(slog.New(slog.DiscardHandler))

	// 每个 IP 的访客上限。
	first := h.add("198.51.100.1", true, nil)
	second := h.add("198.51.100.1", true, nil)
	if first == nil || second == nil {
		t.Fatalf("同一个 IP 的前 %d 条访客连接都应当被接受", maxGuestSSEClientsPerIP)
	}
	if rejected := h.add("198.51.100.1", true, nil); rejected != nil {
		t.Fatalf("同一个 IP 的第 %d 条访客连接应当被拒（上限 %d）",
			maxGuestSSEClientsPerIP+1, maxGuestSSEClientsPerIP)
	}
	// 换一个 IP 还能连 —— 说明上面那条是 perIP 上限，不是全局上限。
	if other := h.add("198.51.100.2", true, nil); other == nil {
		t.Fatal("换一个来源 IP 应当还能连访客流")
	}

	// 移除之后名额要**对称**还回去，否则名额永久泄漏。
	h.remove(nil)                 // 空句柄：什么都不该发生
	h.remove(&hubClient{id: 999}) // 不在册：不该凭空多还一份配额
	h.remove(first)
	if again := h.add("198.51.100.1", true, nil); again == nil {
		t.Fatal("移除之后同一个 IP 应当还能再连（perIP 名额要还回去）")
	}

	// 占满全局访客配额，管理员仍应能连。
	_, current := h.countByRole()
	for i := current; i < maxGuestSSEClients; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i)
		if c := h.add(ip, true, nil); c == nil {
			t.Fatalf("第 %d 条访客连接（全局配额 %d 之内）不该被拒", i+1, maxGuestSSEClients)
		}
	}
	if _, guests := h.countByRole(); guests != maxGuestSSEClients {
		t.Fatalf("访客连接数 = %d，期望 %d", guests, maxGuestSSEClients)
	}
	if extra := h.add("203.0.113.250", true, nil); extra != nil {
		t.Fatalf("超过全局访客配额应当被拒（%d）", maxGuestSSEClients)
	}

	admin := h.add("203.0.113.251", false, nil)
	if admin == nil {
		t.Fatal("访客配额占满不该影响管理员建连（两份配额是分开的）")
	}
	// 而且管理员确实收得到自己的那一份负载。
	h.broadcastTo(false, []byte(`{"type":"nodes"}`))
	select {
	case got := <-admin.ch:
		if string(got) != `{"type":"nodes"}` {
			t.Fatalf("管理员收到的负载不对：%q", got)
		}
	default:
		t.Fatal("管理员应当收到广播")
	}
	// 广播不会串台：访客那一份发不到管理员这里。
	h.broadcastTo(true, []byte(`{"type":"guest"}`))
	select {
	case got := <-admin.ch:
		t.Fatalf("管理员收到了访客那一份负载：%q", got)
	default:
	}
}

// TestGuestStreamFloodDoesNotLockOutAdmin 是必修 3 的端到端版本：
// 访客把配额占满之后，**管理员自己的实时视图**必须仍然能用（不被 503 拒绝）。
func TestGuestStreamFloodDoesNotLockOutAdmin(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "quota-01")
	guestOn(t, h)

	admin := h.client // 带会话的那个客户端，下面要换回来
	h.anonymousClient(t)

	var stops []func()
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()

	// ① 同一个来源 IP 的访客配额（真实 HTTP）。
	for i := 0; i < maxGuestSSEClientsPerIP; i++ {
		_, stop := startSSE(t, h)
		stops = append(stops, stop)
	}
	if code := probeStatus(t, h.client, http.MethodGet, h.ts.URL+"/api/v1/stream"); code != http.StatusServiceUnavailable {
		t.Errorf("同一个 IP 的第 %d 条访客流应当 503，实际 %d", maxGuestSSEClientsPerIP+1, code)
	}

	// ② 全局访客配额：httptest 里来源 IP 都是 127.0.0.1，造不出 20 个不同 IP，
	// 所以剩下的名额直接打 hub —— 这一条测的是配额算术，不是 HTTP 栈。
	for i := 0; i < maxGuestSSEClients-maxGuestSSEClientsPerIP; i++ {
		if c := h.srv.hub.add(fmt.Sprintf("198.51.100.%d", i), true, nil); c == nil {
			t.Fatalf("第 %d 条访客连接（全局配额内）不该被拒", i+1)
		}
	}
	if _, guests := h.srv.hub.countByRole(); guests != maxGuestSSEClients {
		t.Fatalf("访客连接数 = %d，期望 %d", guests, maxGuestSSEClients)
	}
	if code := probeStatus(t, h.client, http.MethodGet, h.ts.URL+"/api/v1/stream"); code != http.StatusServiceUnavailable {
		t.Errorf("访客全局配额占满后新访客应当 503，实际 %d", code)
	}

	// ③ 管理员：仍然连得上，而且拿到的还是**完整**视图。
	h.client = admin
	reader, stop := startSSE(t, h)
	stops = append(stops, stop)
	snapshot := reader.next(t, 5*time.Second)
	if !anyNodeKey(snapshot, "observed_ip") {
		t.Errorf("管理员应当仍然拿到完整快照（含 observed_ip）：%v", snapshot)
	}
}

func TestStreamSendsSnapshotThenChanges(t *testing.T) {
	h := newAuthHarness(t)
	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "sse-01", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	reader, stop := startSSE(t, h)
	defer stop()

	// 1) 连上后先收到全量快照。
	first := reader.next(t, 5*time.Second)
	if first["type"] != "nodes" {
		t.Fatalf("首个事件类型 = %v", first["type"])
	}
	if nodes, _ := first["nodes"].([]any); len(nodes) != 1 {
		t.Fatalf("快照里节点数 = %d", len(nodes))
	}

	// 2) Agent 上报后，一秒内应当收到这个节点的变更。
	h.srv.State().Update(id, 1, protocol.Metrics{
		CPUPct: 42,
		Mem:    protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Net:    protocol.Net{Iface: "eth0", RxRate: 2048, TxRate: 1024},
		LatMS:  10, UptimeSec: 60,
	}, 0, time.Now())

	changed := reader.nextMatching(t, 5*time.Second, "带有 cpu_pct=42 的变更", func(p map[string]any) bool {
		n := firstNode(t, p)
		return n != nil && n["cpu_pct"] == 42.0
	})
	dto := firstNode(t, changed)
	if dto["rx_rate"] != 2048.0 || dto["status"] != "online" {
		t.Fatalf("变更集内容不对: %v", dto)
	}

	// 3) 汇总始终是全量的。
	summary, _ := changed["summary"].(map[string]any)
	if summary["total"] != float64(1) || summary["online"] != float64(1) {
		t.Fatalf("汇总不对: %v", summary)
	}
}

func TestStreamPushesStatusChangeWithoutNewMetrics(t *testing.T) {
	// 短阈值：让"时间流逝导致掉线"在测试里真的发生。
	cfg := config.Default()
	cfg.StaleAfter = 60 * time.Millisecond
	cfg.OfflineAfter = 120 * time.Millisecond
	h := newAuthHarnessWithConfig(t, cfg)

	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "sse-02", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	// 先让节点"在线"。
	h.srv.State().Update(id, 1, protocol.Metrics{Net: protocol.Net{Iface: "eth0"}}, 0, time.Now())

	reader, stop := startSSE(t, h)
	defer stop()
	_ = reader.next(t, 5*time.Second) // 快照

	// 之后不再上报，只让时间流逝：状态变化必须被推送出去。
	changed := reader.nextMatching(t, 6*time.Second, "状态变为 offline", func(p map[string]any) bool {
		n := firstNode(t, p)
		return n != nil && n["status"] == "offline"
	})
	dto := firstNode(t, changed)
	if dto["last_seen"] == float64(0) {
		t.Fatal("掉线推送里应当带最后通信时间")
	}
}
