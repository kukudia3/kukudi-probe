package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// pingMeta 是延迟曲线的档位信息（前端不做算术，与 /series 的 meta 同样的思路）。
//
// 字段只有这四个 + 基准间隔：ping 曲线没有"源表"的概念（桶宽就是查询粒度，
// 曲线由前端按 ts 自己铺时间轴），多给字段只会让前端多一套没人用的分支。
//
// 为什么要给 TickBaseSec：X 轴的标签锚点按它铺（1h 档每 1 分钟一个锚点），
// 放不下时由前端按标签实际宽度自动稀疏。没有它前端就只能**猜**一个刻度，
// 或者去读 /nodes/{id} 里另一张表的同名字段 —— 两张表各有一份刻度，
// 拿错了这一档的标签就落在错的网格上（store 的测试钉住两者一致）。
type pingMeta struct {
	Key       string `json:"key"`
	Seconds   int64  `json:"seconds"`
	BucketSec int64  `json:"bucket_sec"`
	Points    int    `json:"points"`
	// TickBaseSec 是 X 轴**基准**间隔（秒），不是屏幕上实际的标签间隔。
	TickBaseSec int64 `json:"tick_base_sec"`
}

func pingMetaOf(r store.PingRange) pingMeta {
	return pingMeta{
		Key:         r.Key,
		Seconds:     int64(r.Window.Seconds()),
		BucketSec:   r.Bucket,
		Points:      r.Points(),
		TickBaseSec: r.TickBaseSec,
	}
}

// pingTargetSeries 是一个目标在某个档位下的曲线。
//
// enabled 与 label 都带上：前端要列出每个目标的**卡片**，就得知道哪些目标当前是
// 关闭的（关闭的目标不会有新数据，显示成空的才不会让人以为探针坏了）。
type pingTargetSeries struct {
	ID      int64   `json:"id"`
	Label   string  `json:"label"`
	Type    string  `json:"type"`
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Enabled bool    `json:"enabled"`
	HasData bool    `json:"has_data"`
	LossPct float64 `json:"loss_pct"`
	// AvgMS 是该档位内的整体平均延迟（按成功探测次数加权，无数据时为 0）。
	//
	// 为什么由服务端给：卡片上要显示"这个目标这一小时平均多少毫秒"，
	// 而前端手里只有画曲线用的分桶点 —— 让它自己把桶平均一遍，就得在
	// 前端重复一遍加权规则（见 store.QueryPingSeries），两处口径迟早分叉。
	AvgMS float64 `json:"avg_ms"`

	// PeakMS 是该档位内的峰值延迟，也就是曲线用的那批桶里 max 的最大值
	// （无数据 / 整段全丢时为 0，见 store.PingSeries.PeakMS）。
	//
	// 与 AvgMS 同一个理由由服务端给：卡片上要写「峰值 260 ms」，而悬浮读数里那一行
	// 用的是 points[i][2] —— 前端自己遍历一遍就是把这个统计再做一次（前端不做统计），
	// 而且"卡片上写的峰值"与"悬浮里那一行峰值"必须来自同一批点。
	// （峰值线本身已经不再画了：延迟图的 showMax 写死 false，见 web/app.js。）
	PeakMS float64 `json:"peak_ms"`

	// 这里曾经还有慢判定的三件套（baseline_ms / threshold_ms / slow_pct）：
	// 用户明确不要"慢"这个概念了（延迟图上不再有红色慢段、卡片上不再有「慢 X%」），
	// 所以字段、判定规则与前端渲染一起删干净了。LossPct 的含义一个字都没变：
	// 它永远只统计**真丢包**（超时时间内没回来），与延迟高低无关。

	// Points 是 [ts, avg, max, loss] 四元组：前三个与 /series 的点完全一致
	// （前端读 p[1]/p[2]），第 4 个是这个桶的丢包率（0-100）。
	//
	// 为什么把它塞进点里：targets[].loss_pct 只说"整段丢了多少"，画不出
	// "什么时候丢的" —— 而"这里丢过包"恰恰是延迟图上最该一眼看到的信息。
	Points [][4]float64 `json:"points"`
}

// handleNodePing 返回某节点全部探测目标的延迟曲线。
//
// 所有**配置了**的目标都会出现（没数据的 has_data=false、points=[]），
// 这样前端一进详情页就能把目标卡片列全，不必再去拉一次设置。
func (s *Server) handleNodePing(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if _, ok := s.loadNode(w, r, id); !ok {
		return
	}

	rangeKey := r.URL.Query().Get("range")
	if rangeKey == "" {
		rangeKey = "1h"
	}
	rg, ok := store.PingRangeByKey(rangeKey)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
			Code: "bad_range", Message: "不支持的时间范围，可选：" + pingRangeKeysHint()}})
		return
	}

	targets, err := s.db.PingTargets(r.Context())
	if err != nil {
		s.log.Error("读取探测目标失败", "err", err, "node_id", id)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	now := time.Now()
	guest := isGuestView(r.Context())
	// 元素类型是 any：访客拿到的每个目标是**白名单 map**（没有 host），
	// 管理员拿到的是完整结构体。两种都能直接序列化，不必分两条返回路径。
	out := make([]any, 0, len(targets))
	for _, t := range targets {
		// 每个目标一条查询（最多 16 条，都是主键区间扫描）。
		// 没有合成一条 SQL：目标数量有上限，而合成查询要么写 UNION ALL，
		// 要么在 SQL 里做 (ts/bucket) 与 target_id 的交叉分组 —— 都比这 16 次查询难读。
		series, err := s.db.QueryPingSeries(r.Context(), id, t.ID, rg, now)
		if err != nil {
			s.log.Error("查询延迟曲线失败", "err", err, "node_id", id, "target_id", t.ID)
			s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
				Code: "internal", Message: "服务端内部错误"}})
			return
		}
		points := make([][4]float64, 0, len(series.Points))
		for _, p := range series.Points {
			points = append(points, [4]float64{float64(p.TS), p.Avg, p.Max, p.Loss})
		}
		item := pingTargetSeries{
			ID: t.ID, Label: t.Label, Type: t.Type, Host: t.Host, Port: t.Port,
			Enabled: t.Enabled, HasData: series.HasData, LossPct: series.LossPct,
			AvgMS:  series.AvgMS,
			PeakMS: series.PeakMS,
			Points: points,
		}
		// 访客看不到 host（探测目标是**地址**，属于"能定位到具体主机/网络"那一类）。
		// label 与全部曲线数据照旧公开 —— 曲线本身就是访客能看的东西。
		if guest {
			out = append(out, guestPingTargetJSON(item))
			continue
		}
		out = append(out, item)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"meta":    pingMetaOf(rg),
		"targets": out,
	})
}

// pingTargetsAuditLine 把一份探测目标列表写成审计里看得见的一行（审计 03-A-8）。
//
// 为什么要把地址写进操作记录：这份列表就是**全体 Agent 的出站目的地** ——
// 谁能写它，谁就能让每一台被监控机从各自内网去连任意 host:port（慢速端口扫描 /
// 内网可达性测绘），而流量来自你自己的机器、来源看起来完全合法。只记"改了 N 个
// 目标"，事后翻操作记录**看不出改了哪些地址**，等于没有取证线索（对比
// api_admin.go 改标签时把新标签写进 detail 的写法）。
//
// 停用的目标不下发、不会被连（wirePingTargets 只发 enabled 的），所以标注出来：
// 它是"下一次可能被连的地址"，值得记，但别让事后读者以为它现在就在被连。
// 规模有协议侧上界（MaxPingTargets = 16 个 × host ≤ 253 字节），一行不会失控。
func pingTargetsAuditLine(targets []store.PingTarget) string {
	if len(targets) == 0 {
		return "：无目标"
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		addr := t.Host
		if t.Type == protocol.PingTypeTCP {
			// 与 Agent 实际 dial 的形态一致（net.JoinHostPort，IPv6 会带方括号）。
			addr = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		}
		if !t.Enabled {
			addr += "（停用）"
		}
		parts = append(parts, t.Type+" "+addr)
	}
	return "：" + strings.Join(parts, "、")
}

func pingRangeKeysHint() string {
	keys := ""
	for i, r := range store.PingRanges() {
		if i > 0 {
			keys += ", "
		}
		keys += r.Key
	}
	return keys
}

// pingSettingsDTO 是延迟探测设置：GET /api/v1/settings 与 PUT /api/v1/settings/ping
// 返回同一形状，前端一套解析逻辑就够。
//
// interval_min_sec / interval_max_sec 是**附加**字段（契约里只要求 targets /
// interval_sec / max_targets）：把合法区间一并下发，前端的输入框校验就不会与
// 服务端漂移 —— 否则每次改范围都要同时改两处代码。
type pingSettingsDTO struct {
	Targets        []store.PingTarget `json:"targets"`
	IntervalSec    int                `json:"interval_sec"`
	MaxTargets     int                `json:"max_targets"`
	IntervalMinSec int                `json:"interval_min_sec"`
	IntervalMaxSec int                `json:"interval_max_sec"`
}

func pingSettingsDTOOf(settings store.PingSettings) pingSettingsDTO {
	targets := settings.Targets
	if targets == nil {
		targets = []store.PingTarget{}
	}
	return pingSettingsDTO{
		Targets:        targets,
		IntervalSec:    settings.IntervalSec,
		MaxTargets:     protocol.MaxPingTargets,
		IntervalMinSec: protocol.MinPingIntervalSec,
		IntervalMaxSec: protocol.MaxPingIntervalSec,
	}
}

// currentPingSettings 读设置；读失败时退回"空列表 + 默认间隔"。
//
// 与图表可见性同样的理由：设置页里还挤着密码、通知、服务器信息，
// 不能因为一行设置读不出来就让整个设置对话框打不开。
func (s *Server) currentPingSettings(ctx context.Context) pingSettingsDTO {
	settings, err := s.db.PingSettings(ctx)
	if err != nil {
		s.log.Warn("读取延迟探测设置失败，本次按「不探测」处理", "err", err)
		settings = store.PingSettings{Targets: []store.PingTarget{}, IntervalSec: protocol.DefaultPingIntervalSec}
	}
	return pingSettingsDTOOf(settings)
}

// pingSettingsRequest 是 PUT /api/v1/settings/ping 的请求体。
//
// IntervalSec 用指针：区分"没传"（保持现值）与"传了 0"（非法，要报 400）。
type pingSettingsRequest struct {
	Targets     []store.PingTarget `json:"targets"`
	IntervalSec *int               `json:"interval_sec"`
}

// handlePutPingSettings 保存延迟探测目标与间隔，并立刻推给所有在线 Agent。
func (s *Server) handlePutPingSettings(w http.ResponseWriter, r *http.Request) {
	var req pingSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	// targets 是整体替换语义，但**缺字段**不等于"清空"：缺字段多半是调用方写错了，
	// 悄悄把用户配好的目标全删掉（曲线从此断掉）比报一个 400 糟糕得多。
	// 真的要清空就显式传 []。
	if req.Targets == nil {
		s.badRequest(w, errors.New(`缺少 targets 字段（清空请显式传 "targets": []）`))
		return
	}

	ctx := r.Context()
	interval := s.currentPingSettings(ctx).IntervalSec
	if req.IntervalSec != nil {
		interval = *req.IntervalSec
	}

	saved, err := s.db.SetPingSettings(ctx, req.Targets, interval)
	if err != nil {
		if errors.Is(err, store.ErrInvalidPing) {
			s.badRequest(w, err)
			return
		}
		s.log.Error("保存延迟探测设置失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	s.audit(ctx, r, "settings_update", 0,
		fmt.Sprintf("修改延迟探测目标（%d 个，间隔 %d 秒）%s",
			len(saved.Targets), saved.IntervalSec, pingTargetsAuditLine(saved.Targets)))
	s.log.Info("已更新延迟探测设置",
		"targets", len(saved.Targets), "interval_sec", saved.IntervalSec)

	// 立刻下发：设置页点保存后马上生效，不必等 Agent 重连。
	s.agents.PushConfig()

	s.writeJSON(w, http.StatusOK, pingSettingsDTOOf(saved))
}
