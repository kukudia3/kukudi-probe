package e2e

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
)

// 设置页改版的人工核对截图：八栏改后各一张（浅色）+ 深色 + 窄屏。
//
// 自动化断言（见 settings_browser_test.go）能证明"计算出来的字号/颜色是那七档、
// 八栏都不横向溢出"，证明不了"人看起来对不对"：分组之间的呼吸感够不够、
// 动作条是不是真的落在卡片底部、K 线一样密的信息有没有读起来更省力 ——
// 这些只有看图才知道。
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）：要看图时设
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestSettingsScreenshots -v
//
// 产生的图（前八张是浅色的八栏，后面是深色与窄屏）：
//
//	settings-notify / settings-alert / settings-dashboard / settings-ping /
//	settings-nodes / settings-security / settings-server / settings-audit /
//	settings-notify-dark / settings-server-dark /
//	settings-notify-narrow / settings-ping-narrow
//
// 宽度 1664 = 内容列上限 1600 + body 左右各 16px 内边距；再乘 1.5 的缩放，
// 12px 的小字在图上直接可读（与设计稿 _settings_mock 的截图参数一致）。
func TestSettingsScreenshots(t *testing.T) {
	outDir := os.Getenv("PROBE_SHOT_DIR")
	if outDir == "" {
		t.Skip("没设 PROBE_SHOT_DIR：截图用例默认不跑（它只产出人工核对的 PNG）")
	}
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("创建截图目录: %v", err)
	}

	h := startSettingsShotFixture(t)

	shots := []struct {
		name  string
		pane  string
		theme string
		w, h  int
		scale string
	}{
		{"settings-notify", "notify", "", 1664, 900, "1.5"},
		{"settings-alert", "alert", "", 1664, 700, "1.5"},
		{"settings-dashboard", "dashboard", "", 1664, 760, "1.5"},
		{"settings-ping", "ping", "", 1664, 1000, "1.5"},
		{"settings-nodes", "nodes", "", 1664, 700, "1.5"},
		{"settings-security", "security", "", 1664, 800, "1.5"},
		{"settings-server", "server", "", 1664, 900, "1.5"},
		{"settings-audit", "audit", "", 1664, 1000, "1.5"},
		// 深色只出两张（与设计稿一致）：一栏字段多、一栏只读网格。
		{"settings-notify-dark", "notify", "dark", 1664, 900, "1.5"},
		{"settings-server-dark", "server", "dark", 1664, 900, "1.5"},
		// 窄屏：Windows 上 Chrome 的窗口有最小宽度（实测 485px），
		// 所以用 2 倍缩放 + 760 的窗口拿到真的 380px CSS 视口（与 fx 那套一致）。
		{"settings-notify-narrow", "notify", "", 760, 1700, "2"},
		{"settings-ping-narrow", "ping", "", 760, 1700, "2"},
	}
	for _, s := range shots {
		// PROBE_SHOT_ONLY 只在调试时用：挑一张（或几张）先跑，留空 = 全部。
		if only := os.Getenv("PROBE_SHOT_ONLY"); only != "" && !strings.Contains(only, s.name) {
			continue
		}
		cfg := tzHarnessConfig{
			User: "admin", Pass: "a-very-good-password",
			Shot: s.pane, Theme: s.theme,
		}
		mock := newMockServer(t, newShotProxyWith(t, "http://"+h.addr, cfg, settingsHarnessJS))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把界面开到目标状态。
		// （自动化断言那条路径故意不用它：长连接 SSE 会让虚拟时间暂停。）
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--force-device-scale-factor=" + s.scale,
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + "/#/settings/" + s.pane,
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
		case <-time.After(120 * time.Second):
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

// startSettingsShotFixture 起一个"八栏都有东西可看"的服务端：
// 三台机器（含一台不足 1 天到期、一台已过期）、一个探测目标、几条操作记录。
//
// 为什么这些数据要先经 API 落库、而不是让页面自己造：截图上看到的必须是
// **页面从服务端读回来**的东西（服务器列表的行、服务器信息的十项、探测目标行
// 都是 app.js 动态生成的），注进去的假 DOM 不能证明这些行真的渲染得出来。
func startSettingsShotFixture(t *testing.T) *harness {
	t.Helper()
	logs := &captureHandler{}
	// 关掉汇率自动获取：截图不该依赖外网（跑得通跑不通取决于当时能不能连上
	// 公开汇率源），而这一栏要显示的"有没有取到、从哪取的"两种状态里，
	// 兜底那一种反而是更需要人眼核对文案的（它写着"不是实时值"）。
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
		func(cfg *config.Server) { cfg.FX = false })
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	now := time.Now()
	createPricedNode(t, br, "HK-01", 4200, "CNY", 1, now.Add(28*24*time.Hour+6*time.Hour).Unix())
	createPricedNode(t, br, "JP-02", 1850, "USD", 3, now.Add(5*time.Hour+30*time.Minute).Unix())
	createPricedNode(t, br, "SG-03", 3000, "CNY", 1, now.Add(-72*time.Hour-time.Minute).Unix())

	if status, body := br.do(http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"targets": []map[string]any{
			{"id": 0, "label": "香港出口", "type": "tcp", "host": "hk.example.com", "port": 443, "enabled": true},
			{"id": 0, "label": "东京节点", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"id": 0, "label": "本地网关", "type": "icmp", "host": "192.168.1.1", "port": 0, "enabled": true},
		},
		"interval_sec": 60,
	}, true); status != http.StatusOK {
		t.Fatalf("预置探测目标失败: %d %v", status, body)
	}
	// 操作记录那一栏要有行可看（新建节点与改设置已经写了几条，这里再补一条登录）。
	if status, body := br.do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": "a-very-good-password",
	}, false); status != http.StatusOK {
		t.Fatalf("补一条登录记录失败: %d %v", status, body)
	}
	return h
}
