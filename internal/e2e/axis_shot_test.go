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
	"strings"
	"testing"
	"time"
)

// 改动 A/B 的人工核对截图：六档 X 轴的近景各一张 + 首页分组筛选的三个状态。
//
// 自动化断言（见 axis_browser_test.go / groupfilter_browser_test.go）能证明
// "字是什么、有几个"，证明不了"人看起来对不对"：标签是不是真的分得开、
// 日期那一格会不会被挤掉半个字、chip 那一排的选中态一眼能不能看出来、
// 窄屏下有没有把布局撑破 —— 这些只有看图才知道。
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）：要看图时设
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestAxisAndGroupScreenshots -v
//
// 产生的九张图（X 轴那六张是近景：视口只有 420px 高，页面把延迟图滚到中间）：
// axis-1h / axis-6h / axis-12h / axis-1d / axis-3d / axis-7d /
// group-all / group-hk / group-none。
func TestAxisAndGroupScreenshots(t *testing.T) {
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

	axisH, axisNode := startAxisFixture(t, loc)

	// 首页分组那三张要的是"分组 + 未分组"同时存在的一屏，另起一个服务端。
	groupH := startGroupFixture(t)

	shots := []struct {
		name    string
		harness string
		base    string
		cfg     tzHarnessConfig
		w, h    int
		hash    string
	}{
		{"axis-1h", axisHarnessJS, "http://" + axisH.addr,
			tzHarnessConfig{NodeID: axisNode, NodeName: "axis-01", Scenario: "axis", Shot: "1h", User: "admin", Pass: "a-very-good-password"}, 1500, 420, "/"},
		{"axis-6h", axisHarnessJS, "http://" + axisH.addr,
			tzHarnessConfig{NodeID: axisNode, NodeName: "axis-01", Scenario: "axis", Shot: "6h", User: "admin", Pass: "a-very-good-password"}, 1500, 420, "/"},
		{"axis-12h", axisHarnessJS, "http://" + axisH.addr,
			tzHarnessConfig{NodeID: axisNode, NodeName: "axis-01", Scenario: "axis", Shot: "12h", User: "admin", Pass: "a-very-good-password"}, 1500, 420, "/"},
		{"axis-1d", axisHarnessJS, "http://" + axisH.addr,
			tzHarnessConfig{NodeID: axisNode, NodeName: "axis-01", Scenario: "axis", Shot: "1d", User: "admin", Pass: "a-very-good-password"}, 1500, 420, "/"},
		{"axis-3d", axisHarnessJS, "http://" + axisH.addr,
			tzHarnessConfig{NodeID: axisNode, NodeName: "axis-01", Scenario: "axis", Shot: "3d", User: "admin", Pass: "a-very-good-password"}, 1500, 420, "/"},
		{"axis-7d", axisHarnessJS, "http://" + axisH.addr,
			tzHarnessConfig{NodeID: axisNode, NodeName: "axis-01", Scenario: "axis", Shot: "7d", User: "admin", Pass: "a-very-good-password"}, 1500, 420, "/"},
		{"group-all", groupHarnessJS, "http://" + groupH.addr,
			tzHarnessConfig{Scenario: "filter", Shot: "group-all", User: "admin", Pass: "a-very-good-password"}, 1500, 1000, "/"},
		{"group-hk", groupHarnessJS, "http://" + groupH.addr,
			tzHarnessConfig{Scenario: "filter", Shot: "group-hk", User: "admin", Pass: "a-very-good-password"}, 1500, 1000, "/"},
		{"group-none", groupHarnessJS, "http://" + groupH.addr,
			tzHarnessConfig{Scenario: "filter", Shot: "group-none", User: "admin", Pass: "a-very-good-password"}, 1500, 1000, "/"},
	}

	for _, s := range shots {
		// PROBE_SHOT_ONLY 只在调试时用：挑一张（或几张）先跑，留空 = 全部九张。
		if only := os.Getenv("PROBE_SHOT_ONLY"); only != "" && !strings.Contains(only, s.name) {
			continue
		}
		mock := newMockServer(t, newShotProxyWith(t, s.base, s.cfg, s.harness))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把界面开到目标状态。
		// （自动化断言那条路径故意不用它：长连接 SSE 会让虚拟时间暂停。）
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + s.hash,
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

// startGroupFixture 起一个装了四个分组的服务端（香港 2 / 美国 1 / 未分组 1）：
// 与分组那条浏览器用例同一份数据，截图里看到的 chip 名单就是被测出来的那一份。
func startGroupFixture(t *testing.T) *harness {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	createNodeWithGroup(t, br, "hk-01", "香港")
	createNodeWithGroup(t, br, "us-01", "美国")
	createNodeWithGroup(t, br, "none-01", "")
	createNodeWithGroup(t, br, "hk-02", "香港")
	return h
}

// newShotProxyWith 与 tz_shot_test.go 的 newShotProxy 同一套（反代 + 只改首页），
// 只是注入哪一份自检脚本由调用方给：X 轴那几张与首页分组那三张观测的东西完全不同。
func newShotProxyWith(t *testing.T, base string, cfg tzHarnessConfig, harnessJS string) *tzProxy {
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
		page:   []byte(injectHarness(string(body), string(raw), harnessJS)),
		result: make(chan []byte, 1),
	}
}
