package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// pingMeta 是延迟曲线的档位信息（前端不做算术，与 /series 的 meta 同样的思路）。
//
// 只保留这四个字段：ping 曲线没有"源表""刻度"的概念（桶宽就是查询粒度，
// 曲线由前端按 ts 自己铺时间轴），多给字段只会让前端多一套没人用的分支。
type pingMeta struct {
	Key       string `json:"key"`
	Seconds   int64  `json:"seconds"`
	BucketSec int64  `json:"bucket_sec"`
	Points    int    `json:"points"`
}

func pingMetaOf(r store.PingRange) pingMeta {
	return pingMeta{
		Key:       r.Key,
		Seconds:   int64(r.Window.Seconds()),
		BucketSec: r.Bucket,
		Points:    r.Points(),
	}
}

// pingTargetSeries 是一个目标在某个档位下的曲线。
//
// enabled 与 label 都带上：前端要列出"勾选框"，就得知道哪些目标当前是关闭的
// （关闭的目标不会有新数据，显示成空的才不会让人以为探针坏了）。
type pingTargetSeries struct {
	ID      int64   `json:"id"`
	Label   string  `json:"label"`
	Type    string  `json:"type"`
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Enabled bool    `json:"enabled"`
	HasData bool    `json:"has_data"`
	LossPct float64 `json:"loss_pct"`

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
// 这样前端一进详情页就能把勾选框列全，不必再去拉一次设置。
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
	out := make([]pingTargetSeries, 0, len(targets))
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
		out = append(out, pingTargetSeries{
			ID: t.ID, Label: t.Label, Type: t.Type, Host: t.Host, Port: t.Port,
			Enabled: t.Enabled, HasData: series.HasData, LossPct: series.LossPct,
			Points: points,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"meta":    pingMetaOf(rg),
		"targets": out,
	})
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
		fmt.Sprintf("修改延迟探测目标（%d 个，间隔 %d 秒）", len(saved.Targets), saved.IntervalSec))
	s.log.Info("已更新延迟探测设置",
		"targets", len(saved.Targets), "interval_sec", saved.IntervalSec)

	// 立刻下发：设置页点保存后马上生效，不必等 Agent 重连。
	s.agents.PushConfig()

	s.writeJSON(w, http.StatusOK, pingSettingsDTOOf(saved))
}
