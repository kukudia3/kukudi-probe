package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"probe/internal/store"
)

// 这个文件是「允许访客查看」的全部实现：开关、路由保护、脱敏白名单、粗限流。
//
// 三条设计决定，都不是随手选的：
//
//  1. **默认关**。开关存在 settings 表里（store.KeyGuestAccess），"没有这一行"就是关。
//     默认打开等于"升级即泄露"：老用户一升级，面板就自动挂到公网上了，而他们
//     根本不知道这件事发生过 —— 这种默认值不能接受。代价只是"想开的人自己去点一下"。
//
//  2. **脱敏在服务端，而且是白名单**。访客的响应里私有字段**根本不存在** ——
//     不是前端 display:none，也不是"值是空串"，是那个 JSON 键压根没有。
//     白名单的意思是"我列出来的才公开"：以后 nodeDTO 加了新字段，它默认是**私有**的，
//     想公开必须有人显式写进 guestPublicNodeFields。黑名单（"我列出的要藏"）在这里
//     不能用：加字段的人根本不会想到"还得去藏匿名单补一笔"，于是新字段默认公开。
//     另有一条测试盯着"每个字段都被显式分过类"（TestNodeDTOFieldsAreClassified）。
//
//  3. **HTTP 与 SSE 走同一个脱敏函数**。首页每秒靠 SSE 更新，如果只给 HTTP 响应
//     脱敏，IP 会从每秒推送的快照里漏出去 —— 这是这个功能里最容易漏的一条路径。
//     所以两条路都调 guestNodeJSON（见 api_stream.go 与 hub.go 的按角色广播）。

// guestAccessOn 是 settings 表里"开"的取值；别的值（含"这一行不存在"）一律按关处理。
const guestAccessOn = "1"

// guestReadLimit / guestReadWindow 是访客读接口的粗限流：每个来源 IP 每分钟最多这么多次。
//
// 为什么要有它：面板一旦公开，读接口就成了机器人扫描的靶子，而每个访客请求都要
// 查库（会话查不到就查开关、再查节点/曲线）。300 是"远远高于真人使用"的量级：
// 一个开着详情页的浏览器每分钟大约 30~60 次请求（总览 1 次/分钟、详情刷新
// 1 次/30 秒、切一次档位成串发几条曲线），而扫描器一分钟能发几千次 —— 卡在 300
// 挡的全是后者。
//
// 只对**未登录**的访客生效：有会话的管理员走另一条分支，永远不受这个限流影响。
const (
	guestReadLimit  = 300
	guestReadWindow = time.Minute
)

// guestAccessEnabled 报告「允许访客查看」是否打开。
//
// 带进程内缓存：这个值每个访客请求都要读一次，而它一天也变不了一次 —— 每次都查库
// 等于给公开的读接口白加一次查询。缓存在 setGuestAccess 里同步更新，所以同一个
// 进程里绝不会读到旧值（也正因为如此，不需要重新读取数据库的那条刷新路径）。
//
// 读库失败时返回 false（**按关处理**）：在"要不要把数据发出去"这条路上，
// 取不到开关时唯一安全的方向是"当它没开"。
func (s *Server) guestAccessEnabled(ctx context.Context) bool {
	s.guestMu.Lock()
	defer s.guestMu.Unlock()
	if s.guestOn != nil {
		return *s.guestOn
	}
	raw, ok, err := s.db.GetSetting(ctx, store.KeyGuestAccess)
	if err != nil {
		s.log.Warn("读取「允许访客查看」开关失败，本次按关闭处理", "err", err)
		return false
	}
	on := ok && raw == guestAccessOn
	s.guestOn = &on
	return on
}

// setGuestAccess 写入开关并同步缓存。
//
// 先写库再改缓存：写失败时缓存保持原样，界面看到的就还是"实际生效的那个值"。
func (s *Server) setGuestAccess(ctx context.Context, on bool) error {
	value := "0"
	if on {
		value = guestAccessOn
	}
	if err := s.db.SetSetting(ctx, store.KeyGuestAccess, value); err != nil {
		return err
	}
	s.guestMu.Lock()
	s.guestOn = &on
	s.guestMu.Unlock()
	return nil
}

// guestViewerKey 是"这次请求按访客脱敏"的上下文标记。
//
// 为什么用 context 而不是把 guest 当参数一路传下去：需要脱敏的地方分布在好几个
// handler 里（节点列表、详情、总览、延迟、SSE），而"来的人是不是访客"这件事在
// 路由那一层就已经判完了。放进 context 之后，handler 只问一句 isGuestView(ctx)，
// 不必知道鉴权的来龙去脉。
type guestViewerKey struct{}

func isGuestView(ctx context.Context) bool {
	guest, _ := ctx.Value(guestViewerKey{}).(bool)
	return guest
}

func withGuestView(ctx context.Context) context.Context {
	return context.WithValue(ctx, guestViewerKey{}, true)
}

// guestOrAdmin 包装"访客也能读"的接口（只用于读接口，见 routes() 的标注）。
//
//   - 有会话 → 管理员：原样通过，响应里带全部字段；
//   - 没会话 + 开关关 → 401：与这个功能加进来**之前完全一样**；
//   - 没会话 + 开关开 → 访客：先过一道粗限流（面板公开后会被机器人扫），
//     再在 context 里打上"这是访客"的标记，handler 据此脱敏。
//
// 这里**故意不按请求方法拒绝**：如果哪天有人把一个写接口错标成"访客可读"，
// 它会在枚举测试里当场变红（写方法 + 无会话必须 401，见 guest_test.go），
// 而不是被这一层的兜底悄悄挡住、让错误标注一直留在代码里。
func (s *Server) guestOrAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 同源校验放在**最前面**：它必须覆盖这个包装器的每一个分支。
		//
		// 这里曾经只在"有会话"那一支里做校验（与 auth.Require 一样），于是
		// **访客分支完全不校验** —— 别人家的页面可以直接跨站读你的公开面板：
		// 请求会被受理、会消耗限额、会连上 SSE 长连接，而注释里写的是
		// "同源校验对所有 /api/ 请求都生效"。校验与"有没有会话"无关：
		// 它管的是"这个请求该不该由浏览器替你发出来"。
		if err := checkSameOrigin(r); err != nil {
			s.writeJSON(w, http.StatusForbidden, errorEnvelope{Error: apiError{
				Code: "bad_origin", Message: err.Error()}})
			return
		}
		user, err := s.auth.authenticate(r)
		if err == nil {
			next(w, r.WithContext(context.WithValue(r.Context(), authUserKey{}, user)))
			return
		}
		if !s.guestAccessEnabled(r.Context()) {
			s.auth.unauthorized(w, err)
			return
		}
		// 限流按来源 IP：面板公开之后，"一个 IP 每分钟几千次"是扫描器的形状，
		// 而真人看面板远远到不了这个量级。
		if ok, retry := s.guestReads.allowed(clientIP(r), time.Now()); !ok {
			s.auth.tooMany(w, retry)
			return
		}
		next(w, r.WithContext(withGuestView(r.Context())))
	}
}

// ---------------------------------------------------------------- 字段白名单
//
// 下面几份清单是**唯一**决定"访客能看到什么"的地方。加字段时改这里，
// 不要在 handler 里逐个判断。

// guestPublicNodeFields 是访客**能看**的节点字段（JSON 键名）。
//
// 分组说明（与 index.html 的「访客访问」一栏里那段说明对应）：
//   - 身份与分组：id / name / group_name / region / tags / enabled / interval_sec / iface
//   - 状态与资源：status / connected / last_seen / cpu_* / mem_* / swap_pct / disk_pct /
//     load* / rx_* / tx_* / disks / gap / dropped / lat_ms / uptime_sec / online_sec
//   - 系统信息：os_name / kernel / cpu_model / cpu_cores / agent_version
//   - 流量与计费：traffic_* / cycle_* / traffic_limit / reset_day / expires_at /
//     expires_text / expired / remaining_days / price_cents / monthly_cents /
//     remaining_value_cents / currency / billing_months / *_cny_cents / cny_converted
//
// **不在**这份清单里的节点字段就是私有的（例如本机地址、来源 IP、备注、boot_id），
// 见 guestPrivateNodeFields 与 guest_test.go 的分类断言。
var guestPublicNodeFields = []string{
	"id",
	"name",
	"group_name",
	"region",
	"tags",
	"enabled",
	"interval_sec",
	"iface",

	"status",
	"connected",
	"last_seen",

	"cpu_pct",
	"cpu_cores",
	"cpu_model",
	"load1",
	"load5",
	"load15",
	"mem_pct",
	"mem_used",
	"mem_total",
	"swap_pct",
	"disk_pct",
	"disks",
	"rx_rate",
	"tx_rate",
	"rx_total",
	"tx_total",
	"lat_ms",
	"uptime_sec",
	"online_sec",
	"gap",
	"dropped",

	"os_name",
	"kernel",
	"agent_version",

	"traffic_limit",
	"traffic_warn_pct",
	"reset_day",
	"traffic_today_rx",
	"traffic_today_tx",
	"traffic_today_total",
	"traffic_today_pct",
	"traffic_week_rx",
	"traffic_week_tx",
	"traffic_week_total",
	"traffic_week_pct",
	"traffic_cycle_rx",
	"traffic_cycle_tx",
	"traffic_cycle_total",
	"traffic_total_rx",
	"traffic_total_tx",
	"traffic_pct",
	"cycle_start",
	"cycle_end",

	"expires_at",
	"expires_text",
	"expired",
	"remaining_days",
	"price_cents",
	"monthly_cents",
	"remaining_value_cents",
	"currency",
	"billing_months",
	"price_cny_cents",
	"monthly_cny_cents",
	"remaining_value_cny_cents",
	"cny_converted",
}

// guestPrivateNodeFields 是访客**看不到**的节点字段。
//
// 它**不参与过滤**（过滤只看上面那份白名单）—— 留着它是为了让"每个字段都被显式
// 分过类"这件事可验证：TestNodeDTOFieldsAreClassified 会反射 nodeDTO 的全部 JSON 键，
// 要求每一个都出现在这两份清单之一里。于是以后往 nodeDTO 里加字段时，作者必须
// 明确选一边；忘了选，测试就红，而不是"默认公开"。
var guestPrivateNodeFields = []string{
	// 本机地址与来源 IP：这两个是最直接的主机定位信息。
	//   observed_ip —— 服务端看到的 TCP 来源地址；
	//   local_ip / local_ip6 —— Agent 自报的本机地址。
	"observed_ip",
	"local_ip",
	"local_ip6",
	// note 是管理员自己写的自由文本。它可能**包含** IP、SSH 端口、商家后台地址 ——
	// 也就是说它不是"一个 IP 字段"，却能反推出 IP。面板上没有任何一处显示它
	// （app.js 里连 node.note 都没有引用），所以对访客隐藏它零成本。
	"note",
	// boot_id 是 Agent 每次启动生成的随机串。它不是地址，但它是**机器指纹**：
	// 同一串出现在别的地方时能把两份数据关联起来；而面板上同样没有任何一处显示它。
	"boot_id",
}

// guestPublicPingTargetFields 是访客能看的探测目标字段（/nodes/{id}/ping 的 targets[]）。
//
// host 不在里面：探测目标是**地址**（1.1.1.1、或者管理员自己另一台机器的域名），
// 它属于"能定位到具体主机/网络"那一类。
//
// label 在名单里，但**值不一定是配置里那个**：label 留空时存储层会用 host 兜底
// （写库与读库都走 normalized()，见 store.LabelDerivedFromHost），于是空名字目标的
// label 就是地址本身。那种 label 在出口被抹成空串（见 guestTargetLabel），
// 前端于是回落到「目标 #id」。所以这份名单公开的是"用户**自己起的名字**"，
// 不是"存储层兜底出来的地址"。
var guestPublicPingTargetFields = []string{
	"id", "label", "type", "port", "enabled",
	"has_data", "loss_pct", "avg_ms", "peak_ms", "points",
}

// guestPublicOverviewTargetFields 是访客能看的"总览按目标聚合"字段。
// 与上面同一套口径：id / label / 延迟 / 丢包公开，host 私有。
var guestPublicOverviewTargetFields = []string{
	"id", "label", "lat_ms", "avg_ms", "loss_pct", "has_data",
}

// guestPrivateTargetFields 是探测目标里**私有**的字段。
//
// 与 guestPrivateNodeFields 一样，它不参与过滤（过滤只看上面那几份白名单），
// 存在的意义是让"每个字段都被显式分过类"可验证（见 guest_test.go 的
// TestTargetDTONestedFieldsAreClassified）。
//
// host 为什么私有：探测目标是一个**地址**（1.1.1.1、或者管理员自己另一台机器的
// 域名 nas.home.lan）。它不属于任何被监控节点，但它同样能定位到具体主机/网络；
// 而面板上没有一处非要访客看到它不可 —— 图例与卡片显示的都是 label。
//
// label 本身不是私有字段，但它**可能是 host 的副本**：存储层对空名称的目标用
// host 兜底（见 store.LabelDerivedFromHost），那种 label 在访客出口被抹成空串
// （guestTargetLabel），前端回落到「目标 #id」（见 app.js 的 pingTargetLabel）。
// 也就是说"host 私有"这件事在出口处是靠两处一起兑现的：host 键不出现 +
// 派生 label 被抹掉。
var guestPrivateTargetFields = []string{"host"}

// ---------------------------------------------------------------- 脱敏

// guestJSON 把一个带 JSON 标签的值转成 map，并且**只保留白名单里的键**。
//
// 实现上先 Marshal 再 Unmarshal 成 map：这些 DTO 全是标量与切片，两次编码的开销
// 可以忽略（而且只有访客请求才会走到这里），换来的是"白名单之外一个键都不会出现"
// 这条性质**由构造过程本身保证** —— 不依赖任何手写的字段拷贝，也就不会出现
// "新加了字段、拷贝函数忘了同步"这种错。
//
// 编码失败时返回**空 map**（而不是原值）：宁可让访客少看到东西，也不能在
// 脱敏失败时把完整 DTO 发出去。
func guestJSON(v any, allow []string) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(allow))
	for _, key := range allow {
		if value, ok := all[key]; ok {
			out[key] = value
		}
	}
	return out
}

// guestNodeJSON 是节点的访客视图（HTTP 与 SSE 共用这一个函数）。
func guestNodeJSON(dto nodeDTO) map[string]any {
	return guestJSON(dto, guestPublicNodeFields)
}

// guestNodesJSON 批量版本（列表接口与 SSE 的变更集）。
func guestNodesJSON(nodes []nodeDTO) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, dto := range nodes {
		out = append(out, guestNodeJSON(dto))
	}
	return out
}

// guestOverviewNodesJSON 把总览里"每节点探测分桶"整块转成访客版本。
//
// 这一块只有两处私有：targets[].host，以及**由 host 派生出来的** targets[].label
// （见 guestTargetLabel）。外层（lat/loss 分桶、整段延迟与丢包）全部公开 ——
// 它们是"线路怎么样"，不是"机器在哪"。
func guestOverviewNodesJSON(in map[int64]store.OverviewPing) map[int64]any {
	out := make(map[int64]any, len(in))
	for id, p := range in {
		targets := make([]map[string]any, 0, len(p.Targets))
		for _, t := range p.Targets {
			item := guestJSON(t, guestPublicOverviewTargetFields)
			item["label"] = guestTargetLabel(t.Label, t.Host)
			targets = append(targets, item)
		}
		out[id] = map[string]any{
			"lat_ms":   p.LatMS,
			"loss_pct": p.LossPct,
			"lat":      p.Lat,
			"loss":     p.Loss,
			"targets":  targets,
		}
	}
	return out
}

// guestPingTargetJSON 是延迟曲线的目标元信息访客版本（points 原样保留：
// 曲线本身就是访客能看的东西）。
func guestPingTargetJSON(t pingTargetSeries) map[string]any {
	out := guestJSON(t, guestPublicPingTargetFields)
	out["label"] = guestTargetLabel(t.Label, t.Host)
	return out
}

// guestTargetLabel 是探测目标 label 的**访客**版本。
//
// label 在访客白名单里是公开的（图例与卡片显示的就是它），但 store 的
// normalized() 会把留空的 label 用 host 兜底（写库与读库都走它，见
// store.LabelDerivedFromHost），于是空名字目标的 label **就是地址本身**：
// `1.1.1.1`、`nas.home.lan`。而 host 是私有的 —— 地址会顺着这个公开字段漏出去。
// 这不是边角情况：前端新建目标时 label 的 placeholder 写着「留空则显示地址」，
// 用户基本不填。
//
// 所以出口处把派生 label 抹成空串。三个决定：
//
//  1. **抹成空串，不是删掉 label 键**：前端 pingTargetLabel 就是
//     `t.label || t.host || ('目标 #' + t.id)`，抹空之后访客自然落到「目标 #id」
//     —— 那正是这段代码一直声称的行为（guestPublicPingTargetFields 的注释里
//     写的就是它）。键不存在反而要多一条分支。
//  2. **在出口抹，不动存储层的回填**：存储层那份 label 是管理员侧 API 的既有
//     输出（设置页要靠它回填输入框），改它会动管理员看到的东西；而且存量数据
//     库里已经是地址了，只有抹在出口才对**存量与增量**同时有效。
//  3. **判定共享 store.LabelDerivedFromHost**：多抹一点（用户故意把 label 写成
//     地址）的代价是访客那边少个名字，漏一点的代价是地址泄露 —— 两个方向不
//     对称，所以判定宁可宽。
func guestTargetLabel(label, host string) string {
	if store.LabelDerivedFromHost(label, host) {
		return ""
	}
	return label
}
