package server

// 第二轮安全审计（SECURITY-AUDIT-ROUND2.md）里属于数据 / 输入处理 / 聚合 / 流量
// 的 confirmed 条目在此逐条钉住。每条都写了"撤掉修复会红在哪一处"。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"probe/internal/alert"
	"probe/internal/config"
	"probe/internal/store"
)

// ---------------------------------------------------------------------------
// 02-3：singleLine 覆盖 U+2028 / U+2029 / 双向控制符
// ---------------------------------------------------------------------------

// 报告的不变量是"每行一台机器"：名字里任何能断行的字符都会把这台机器拆成两行，
// 后半截看起来就像另一台机器（而报告里还有 ↑/↓/合计 三个数据行可以仿造）。
func TestSingleLineCoversLineBreaksAndBidiControls(t *testing.T) {
	breaks := func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}

	// Cc（旧实现已覆盖）与 Zl/Zp（旧实现漏掉：U+2028/U+2029 是换行，但不是 Cc）。
	for _, in := range []string{
		"hk-01\n↑ 999 GB",
		"hk-01\r\n",
		"hk-01\u0085",
		"hk-01\u2028↑ 999 GB",
		"hk-01\u2029↑ 999 GB",
	} {
		got := singleLine(in)
		for _, r := range got {
			if breaks(r) {
				t.Errorf("singleLine(%q) = %q 里还留着能断行的 %U", in, got, r)
			}
		}
	}

	// 双向控制符不换行，但能把后面的字符**显示**成别的样子（U+202E RLO 之后整行
	// 反向），用来把 hk-01 伪装成另一台机器、或让两台不同的机器看起来同名。
	for _, in := range []string{
		"hk-01\u202E", "\u202Dhk-01", "hk-01\u2066x\u2069", "hk-01\u200F", "hk-01\u061C",
	} {
		got := singleLine(in)
		for _, r := range got {
			if isBidiControl(r) {
				t.Errorf("singleLine(%q) = %q 里还留着双向控制符 %U", in, got, r)
			}
		}
	}

	// 意图级：报告里那一行（名称（分组 · 地区））整体不能含断行字符 ——
	// 三段都要过清洗，漏掉分组或地区同样能凭空造出一行。
	node := store.Node{
		ID: 7, Name: "hk-01\u2028↑ 999 GB", GroupName: "g\u2029x", Region: "HK\u202E",
	}
	line := reportName(node)
	if strings.ContainsAny(line, "\n\r\u2028\u2029") {
		t.Errorf("报告的一行里出现了换行字符：%q", line)
	}
	// 清洗是"换成空格"，不是"删字符"：正常字符一个都不能动，否则 hk-01 与 hk011
	// 会撞成同一个名字。
	if !strings.HasPrefix(line, "hk-01 ↑ 999 GB") {
		t.Errorf("控制字符应当换成空格、其余字符原样保留：%q", line)
	}

	// 边界（刻意**不**清洗的）：Cf 里的零宽连接符 / 软连字符是合法字符，
	// 一律抹掉会把 emoji 序列与正常排版改样。残留风险见交付说明。
	for _, keep := range []string{"👨\u200D👩 hk-01", "hk\u00AD01"} {
		if got := singleLine(keep); got != keep {
			t.Errorf("不该误伤合法字符：singleLine(%q) = %q", keep, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 02·04-2：告警文案与报告侧同一套清洗口径（名字里的换行不能让正文多一行）
// ---------------------------------------------------------------------------

func TestAlertBodyKeepsNodeNameOnOneLine(t *testing.T) {
	cfg := alertTestConfig()
	cfg.AlertDebounce = 0
	h := newAuthHarnessWithConfig(t, cfg)
	recorder := newRecordingNotifier()
	h.srv.dispatch.SetNotifiers([]alert.Notifier{recorder})

	// 名字里带换行：管理员能原样写进库（存储层不限字符类别），而告警正文是
	// 纯文本（Telegram 请求体不带 parse_mode），\n 一定按换行渲染。
	node := nodeDTO{
		ID:       1,
		Name:     "hk-01\n↑ 999 GB  ↓ 0 B",
		Status:   "offline",
		LastSeen: time.Now().Add(-time.Hour).Unix(),
	}
	// 第一次评估只是记下"离线条件从此刻开始"（去抖），第二次才真的触发。
	h.srv.evaluateAlerts(context.Background(), []nodeDTO{node})
	h.srv.evaluateAlerts(context.Background(), []nodeDTO{node})

	got := recorder.wait(t, alert.RuleOffline, 10*time.Second)

	// 意图级：清洗后的名字必须**完整地占一行**。修复前这里的名字会被 \n 拆成
	// "hk-01" 与 "↑ 999 GB  ↓ 0 B" 两行，后半截看起来就像另一台机器 / 一行数据。
	want := "hk-01 ↑ 999 GB  ↓ 0 B"
	found := false
	for _, line := range strings.Split(got.Body, "\n") {
		if line == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("告警正文里没有一行是完整的节点名 —— 名字里的控制字符把它拆行了：\n%s", got.Body)
	}
	// 不能为了"压成一行"把名字整段删掉。
	if !strings.Contains(got.Body, want) {
		t.Errorf("告警正文里丢了节点名：\n%s", got.Body)
	}

	// 通知的**元数据**（NodeName）也必须是一行：它同时进服务端日志与通知器日志，
	// alert 包里的 DisplayName 只管正文、管不到这个字段 —— 服务端喂进引擎的
	// 快照值才是它的来源（见 internal/server/alert.go 的 evaluateAlerts）。
	if strings.ContainsAny(got.NodeName, "\n\r\u2028\u2029") {
		t.Errorf("通知里的节点名没有压成一行：%q", got.NodeName)
	}
	if got.NodeName != want {
		t.Errorf("通知里的节点名 = %q，期望 %q（清洗不能改动正常字符）", got.NodeName, want)
	}
}

// ---------------------------------------------------------------------------
// 02-2：四个告警参数被无锁并发读写（半新半旧的一组参数）
// ---------------------------------------------------------------------------

// 这一条没有 -race 也能量到：每个"代"把四个参数编码成互不相同的值，
// 只要一次读取拿到的四个值来自不同的代，就是修复前那种"读到半新半旧"的现象
// （审计员用它观测到 9 次混搭）。加锁之后四个字段整组换，读数必须始终一致。
func TestAlertSettingsConcurrentSaveAndReadStayConsistent(t *testing.T) {
	h := newAuthHarness(t)

	// 每个字段的"单位"：g = 值 / 单位，四者必须相等。
	units := map[string]time.Duration{
		"cooldown":       time.Second,
		"startup_grace":  time.Millisecond,
		"debounce":       time.Microsecond,
		"recover_stable": 10 * time.Millisecond,
	}
	const maxGen = 200
	payload := func(g int) map[string]any {
		return map[string]any{
			"cooldown":       (time.Duration(g) * time.Second).String(),
			"startup_grace":  (time.Duration(g) * time.Millisecond).String(),
			"debounce":       (time.Duration(g) * time.Microsecond).String(),
			"recover_stable": (time.Duration(g) * 10 * time.Millisecond).String(),
		}
	}
	genOf := func(v string, unit time.Duration) (int, bool) {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, false
		}
		g := int(d / unit)
		if g < 1 || g > maxGen {
			return 0, false // 还在默认值上（写者还没跑起来），这次读数不作数
		}
		return g, true
	}

	// 先落一代，避免读者在"默认值"上打转。
	if status, body := h.put(t, "/api/v1/settings/alert", payload(1)); status != http.StatusOK {
		t.Fatalf("前置保存失败: %d %v", status, body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	// 两个写者：并发的 POST 保存（设置页连点保存 / 多标签页）。
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ctx.Err() == nil; i++ {
				g := w*100 + i%100 + 1
				if err := h.putNoFatal("/api/v1/settings/alert", payload(g)); err != nil {
					return // 上下文取消时连接会断，不再计较
				}
			}
		}(w)
	}

	// 三个读者：直接读"GET /settings 里那四个参数"的那条取值路径（它才是
	// 数据竞争的那一侧）。tight loop 才能撞上写者那几百纳秒的四次赋值窗口。
	var (
		mu         sync.Mutex
		reads      int
		mixed      []string
		seenGens   = map[int]bool{}
		readErrors []string
	)
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				got := h.srv.currentAlertSettings()
				values := map[string]string{
					"cooldown": got.Cooldown, "startup_grace": got.StartupGrace,
					"debounce": got.Debounce, "recover_stable": got.RecoverStable,
				}
				gen, ok := -1, true
				for field, v := range values {
					g, valid := genOf(v, units[field])
					if !valid {
						ok = false
						break
					}
					if gen == -1 {
						gen = g
					} else if g != gen {
						ok = false
						break
					}
				}
				mu.Lock()
				if ok {
					reads++
					seenGens[gen] = true
				} else if gen != -1 {
					if len(mixed) < 5 {
						mixed = append(mixed, fmt.Sprintf("%v", values))
					}
				}
				mu.Unlock()
			}
		}()
	}

	time.Sleep(700 * time.Millisecond)
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if reads < 1000 {
		t.Fatalf("有效读数只有 %d 次，用例失去判别力（写者没跑起来？）", reads)
	}
	if len(seenGens) < 2 {
		t.Fatalf("只观察到 %d 个代次，写者没有与读者并发（用例失去判别力）", len(seenGens))
	}
	if len(mixed) > 0 {
		t.Errorf("读到了 %d 组「四个参数来自不同保存」的混搭值（修复前是数据竞争）：\n%s",
			len(mixed), strings.Join(mixed, "\n"))
	}
	for _, e := range readErrors {
		t.Errorf("%s", e)
	}
}

// putNoFatal 是 h.put 的并发安全版本：HTTP 出错时返回 error 而不是调用 t.Fatalf
// （t.Fatalf 只能从测试主 goroutine 调）。
func (h *authHarness) putNoFatal(path string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, h.ts.URL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CSRF-Token", h.csrf)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT %s 状态码 = %d", path, resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 02·L2：SSE 先占名额、后建全量快照
// ---------------------------------------------------------------------------

// 判据是"被配额拒绝的那条请求有没有付快照的代价"，而这件事有一个可观测的差别：
// 把库弄坏（Close）之后，先建快照的实现会在快照那一步报 500（节点查询失败），
// 先占名额的实现一个字节都不编码，直接按配额拒绝 → 503 too_many_streams。
//
// （用访客配额：它每个 IP 只有 2 条，占满最快；访客分支不需要会话，
// 所以库关掉之后中间件仍然放行。）
func TestStreamRejectedBeforeSnapshotIsBuilt(t *testing.T) {
	h := newAuthHarness(t)
	guestOn(t, h)
	h.anonymousClient(t)

	for i := 0; i < maxGuestSSEClientsPerIP; i++ {
		_, stop := startSSE(t, h)
		defer stop()
	}

	if err := h.srv.db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}

	status, body, _ := h.do(t, http.MethodGet, "/api/v1/stream", nil, false, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("配额已满时应当 503（先占名额、不建快照），实际 %d %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "too_many_streams" {
		t.Fatalf("错误码 = %v，期望 too_many_streams（说明请求是**先被配额闸门挡下**的）", errObj)
	}
}

// ---------------------------------------------------------------------------
// 02·L3：流量汇总失败不再静默降级成 0
// ---------------------------------------------------------------------------

// 汇总查询失败时，面板必须沿用**上一次成功**的那一份，而不是显示 0 ——
// 0 与"这台机器这个周期真的没跑流量"在界面上完全一样（而且它同时喂告警引擎，
// 静默填 0 会让"流量已超额"看起来恢复了）。
func TestTrafficAggregateFailureKeepsLastKnownValues(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "tr-01")

	// 灌一行真实的日流量（走 store 的写入路径，字段口径与线上一致）。
	day := store.FormatDay(time.Now().In(h.srv.loc))
	if _, err := h.srv.db.Writer().ExecContext(context.Background(),
		`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)`,
		nodeID, day, 3_000_000_000, 1_000_000_000); err != nil {
		t.Fatalf("写入日流量: %v", err)
	}

	readTotal := func(what string) (int64, int64) {
		t.Helper()
		status, body := h.get(t, "/api/v1/nodes")
		if status != http.StatusOK {
			t.Fatalf("%s: /api/v1/nodes 状态码 = %d", what, status)
		}
		nodes, _ := body["nodes"].([]any)
		if len(nodes) != 1 {
			t.Fatalf("%s: 节点数 = %d，期望 1", what, len(nodes))
		}
		node, _ := nodes[0].(map[string]any)
		pick := func(key string) int64 {
			v, _ := node[key].(float64)
			return int64(v)
		}
		return pick("traffic_total_rx"), pick("traffic_total_tx")
	}

	rxBefore, txBefore := readTotal("前置")
	if rxBefore != 3_000_000_000 || txBefore != 1_000_000_000 {
		t.Fatalf("前置条件不成立：累计流量 = %d/%d", rxBefore, txBefore)
	}

	// 模拟"流量刚落盘、缓存刚失效"（每分钟都会发生一次）之后汇总查询失败：
	// 把那张表删掉，其它读取（节点列表、实时指标）照常。
	h.srv.trafficCache.invalidate()
	if _, err := h.srv.db.Writer().ExecContext(context.Background(), `DROP TABLE traffic_daily`); err != nil {
		t.Fatalf("删除流量表: %v", err)
	}

	rxAfter, txAfter := readTotal("故障后")
	if rxAfter == 0 && txAfter == 0 {
		t.Errorf("汇总查询失败后流量显示成了 0 —— 与「真的没有流量」不可区分（修复前就是这样）")
	}
	if rxAfter != rxBefore || txAfter != txBefore {
		t.Errorf("故障后应当沿用上一次成功的汇总：%d/%d，期望 %d/%d",
			rxAfter, txAfter, rxBefore, txBefore)
	}
}

// ---------------------------------------------------------------------------
// 04-S-3：并发保存被记下来，但接口语义（后提交者赢）不变
// ---------------------------------------------------------------------------

// 真实交错（别人的写入正好落在 loadNode 与写入之间）在 HTTP 层复现不了 ——
// 那个窗口只有几微秒，所以直接驱动 handleUpdateNode 用的那个函数。
func TestStaleNodeSaveIsReportedAndStillWins(t *testing.T) {
	var logs bytes.Buffer
	h := newAuthHarnessWithConfigAndLogger(t, config.Default(), slog.New(slog.NewTextHandler(&logs, nil)))
	ctx := context.Background()

	nodeID, _ := createNodeOverHTTP(t, h, "stale-01")
	snapshot, err := h.srv.db.NodeByID(ctx, nodeID)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}

	// 另一个会话先保存（改地区）。
	// 时间戳必须**跨过整秒**：updated_at 是秒级的，同一秒内的两次写入在并发
	// 判据上分不开（这是 04-S-3 明说的已知边界，漏判方向是"照旧覆盖"）。
	byOther := snapshot
	byOther.Region = "SG"
	if err := h.srv.db.UpdateNode(ctx, byOther, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("另一个会话保存: %v", err)
	}

	// 我用手里的**陈旧快照**保存。
	mine := snapshot
	mine.Region = "JP"
	if err := h.srv.saveNodeWithStaleCheck(ctx, mine, snapshot.UpdatedAt, time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("并发保存不该失败（接口语义仍是「后提交者赢」）: %v", err)
	}

	got, err := h.srv.db.NodeByID(ctx, nodeID)
	if err != nil {
		t.Fatalf("读回节点: %v", err)
	}
	if got.Region != "JP" {
		t.Errorf("后提交者仍然应当赢（接口语义不变）：%+v", got)
	}
	if !strings.Contains(logs.String(), "被其它会话改过") {
		t.Errorf("被静默覆盖的那次修改必须留下可观测的记录，日志里没有：\n%s", logs.String())
	}
}
