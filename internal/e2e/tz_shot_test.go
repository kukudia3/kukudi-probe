package e2e

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 跨时区渲染的三张人工核对截图：操作记录（表头写没写时区）、
// 「近 7 天流量」的日轴、节点编辑框里的到期日。
//
// 为什么要单独一条用例：自动化断言（见 tz_browser_test.go）能证明"字是什么"，
// 证明不了"人看起来对不对" —— 表头那句话有没有被挤掉、日轴标签有没有叠在一起、
// 编辑框里那个日期是不是真的显示成 2026-10-01，只有看图才知道。
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）：要看图时设
//
//	$env:PROBE_SHOT_DIR = "D:\shots"; go test ./internal/e2e/ -run TestTimezoneScreenshots -v
//
// 产生的三张图：audit.png / traffic.png / expires.png。
func TestTimezoneScreenshots(t *testing.T) {
	outDir := os.Getenv("PROBE_SHOT_DIR")
	if outDir == "" {
		t.Skip("没设 PROBE_SHOT_DIR：截图用例默认不跑（它只产出人工核对的 PNG）")
	}
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置")
	}
	loc, err := time.LoadLocation(tzTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", tzTestZone, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("创建截图目录: %v", err)
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), loc, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "hz-01")
	seedTrafficDays(t, h, nodeID, loc)
	seedCPUSamples(t, h, nodeID)

	// 先把到期日按"服务端时区那一天的零点"写进去（等价于在界面上填过一次）。
	// 这样截图里那个 2026-10-01 就是**回读**出来的，而不是脚本刚敲进去的。
	status, body := br.do("PATCH", "/api/v1/nodes/"+strconv.FormatInt(nodeID, 10), map[string]any{
		"name": "hz-01", "group_name": "香港", "region": "HK", "interval_sec": 1,
		"expires_at": tzTestExpiresEpoch, "traffic_limit": 1_000_000_000_000,
		"reset_day": 1, "traffic_warn_pct": 80, "enabled": true,
	}, true)
	if status != 200 {
		t.Fatalf("预置到期日失败: %d %v", status, body)
	}

	base := tzHarnessConfig{
		NodeID: nodeID, NodeName: "hz-01", Date: tzTestDate,
		User: "admin", Pass: "a-very-good-password",
	}
	shots := []struct {
		name string
		mode string
		hash string
		w, h int
	}{
		// 操作记录：表头必须写着「时间（服务端时区 Asia/Tokyo）」，行里是服务端时区的时间。
		{"audit", "audit", "#/settings/audit", 1500, 1100},
		// 详情页整页 1900 高：「近 7 天流量」在最下面，窗口不够高就会把日轴裁掉。
		{"traffic", "chart", "#/n/" + strconv.FormatInt(nodeID, 10), 1500, 1900},
		// 节点编辑框：到期日那一格显示的是从服务端回读出来的日期。
		{"expires", "dialog", "#/n/" + strconv.FormatInt(nodeID, 10), 1500, 1200},
	}
	for _, s := range shots {
		cfg := base
		cfg.Shot = s.mode
		mock := newMockServer(t, newShotProxy(t, "http://"+h.addr, cfg))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把界面开到目标状态。
		// 它只在这一处用 —— 自动化断言那条路径故意不用它（长连接 SSE 会让
		// 虚拟时间暂停，大流程有可能卡住），结果改由页面 POST 回来。
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + "/" + s.hash,
		}
		cmd := exec.Command(chrome, args...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 Chrome: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			killChrome(cmd)
			t.Fatalf("%s: Chrome 超时。输出尾部：\n%s", s.name, tail(buf.String(), 800))
		}
		st, err := os.Stat(out)
		if err != nil {
			t.Fatalf("%s: 截图没有生成: %v\nChrome 输出尾部：\n%s", s.name, err, tail(buf.String(), 800))
		}
		t.Logf("%s → %s（%d 字节）", s.name, out, st.Size())
	}
}

// newShotProxy 与 tzProxy 同一套注入，只是首页直接从 web/ 目录读：
// 截图用例不校验"接口返回的 HTML 与磁盘一致"（那条由 tz_browser_test.go 盯着），
// 省掉一次取首页的往返。
func newShotProxy(t *testing.T, base string, cfg tzHarnessConfig) *tzProxy {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("解析服务端地址 %s: %v", base, err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = 50 * time.Millisecond
	body, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("读取 index.html: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化自检配置: %v", err)
	}
	return &tzProxy{
		proxy:  rp,
		page:   []byte(injectHarness(string(body), string(raw), tzHarnessJS)),
		result: make(chan []byte, 1),
	}
}
