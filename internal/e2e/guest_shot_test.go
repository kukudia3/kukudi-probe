package e2e

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 「访客只读模式」与"复制按钮"的人工核对截图。
//
// 自动化断言（见 guest_browser_test.go）能证明"DOM 里有没有那个元素、页面文本里
// 有没有那个地址、剪贴板里拿到了什么"，证明不了"人看起来对不对"：只读提示条是不是
// 一眼能看见、顶栏少了三个按钮之后是不是显得空、价格那一排对访客是不是照旧、
// 设置页那个新开关与说明读起来是不是清楚、创建成功对话框里那两个复制按钮是不是
// 一眼能分清、命令块有没有被按钮挤窄或盖住。这些只有看图才知道。
//
// 成对出现，方便对比：
//
//	guest-home / guest-detail      未登录（只读）
//	admin-detail                   登录后的同一个详情页
//	admin-guest-switch             设置页「访客访问」那一栏
//	admin-token-dialog             创建成功对话框（两个复制按钮 + 命令块）
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）：要看图时设
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestGuestModeScreenshots -v
func TestGuestModeScreenshots(t *testing.T) {
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

	h := startGuestShotFixture(t)

	shots := []struct {
		name  string
		shot  string
		theme string
		w, h  int
		hash  string
	}{
		{"guest-home", "guest-home", "", 1500, 1000, "/"},
		{"guest-detail", "guest-detail", "", 1500, 1100, "/#/n/1"},
		{"admin-detail", "admin-detail", "", 1500, 1100, "/#/n/1"},
		{"admin-detail-dark", "admin-detail", "dark", 1500, 1100, "/#/n/1"},
		{"admin-guest-switch", "guest-switch", "", 1500, 1000, "/#/settings/guest"},
		// 创建成功那个对话框：Token 的复制按钮 + 命令块自己的复制按钮。
		// 命令块是可横向滚动的，按钮必须放在标题行里（不盖住命令、也不把命令挤窄）。
		{"admin-token-dialog", "token-dialog", "", 1500, 900, "/"},
		{"admin-token-dialog-dark", "token-dialog", "dark", 1500, 900, "/"},
		// 退出登录**之后**那一屏：人工核对"还看不看得见上一位登录者的东西"。
		{"logout-done", "logout-done", "", 1500, 1100, "/"},
		// 访客详情页的「延迟」卡（探测目标卡片）：名字应当是「目标 #id」而不是地址。
		// 窗口开得很高：无头 Chrome 的 --screenshot 只截**视口**，而这一页很长，
		// 矮窗口下卡片落在折叠线以下（scrollIntoView 不会改变截图的可视区域）。
		{"guest-latency", "guest-latency", "", 1200, 2600, "/#/n/1"},
	}

	for _, s := range shots {
		// PROBE_SHOT_ONLY 只在调试时用：它的值里必须**含有**要跑的那张的名字
		// （例如 `token` 同时选中 admin-token-dialog 与 admin-token-dialog-dark），
		// 留空 = 全部。
		if only := os.Getenv("PROBE_SHOT_ONLY"); only != "" && !strings.Contains(only, s.name) {
			continue
		}
		cfg := tzHarnessConfig{
			NodeID: 1, NodeName: "guest-01", Scenario: "guest", Shot: s.shot, Theme: s.theme,
			User: "admin", Pass: "a-very-good-password",
		}
		mock := newMockServer(t, newShotProxyWith(t, "http://"+h.addr, cfg, guestHarnessJS))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把界面开到目标状态。
		// （自动化断言那条路径故意不用它：长连接 SSE 会让虚拟时间暂停，
		//  所以这里也把 EventSource 换成了不联网的替身，见 guestHarnessJS。）
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

// startGuestShotFixture 起一个"什么都有"的服务端：一台带价格/到期日/三个地址字段
// 的机器，并且**打开**了「允许访客查看」。
//
// 截图里的每个数字都来自这里，所以四张图之间可以直接对比：
// 访客那两张里不该出现 203.0.113.9 / 10.0.0.5 / fd00::5，
// 而登录后那张里必须有它们。
func startGuestShotFixture(t *testing.T) *harness {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	nodeID, _ := createNodeViaAPI(t, br, "guest-01")
	status, body := br.do("PATCH", "/api/v1/nodes/"+strconv.FormatInt(nodeID, 10), map[string]any{
		"name": "guest-01", "group_name": "香港", "region": "香港", "interval_sec": 1,
		"note":        "备注里可以写任何东西：root@203.0.113.7:22",
		"price_cents": 7121, "currency": "CNY", "billing_months": 12,
		"expires_at": time.Now().Add(30 * 24 * time.Hour).Unix(),
		"reset_day":  1, "traffic_warn_pct": 80,
	}, true)
	if status != 200 {
		t.Fatalf("改节点失败: %d %v", status, body)
	}
	seedGuestNodeState(t, h, nodeID)
	// 探测目标三个（第三个名字留空 —— 存储层用 host 兜底填 label）。
	// 不配的话延迟卡上是"还没有配置探测目标"，那张 guest-latency 截图就什么也证明不了。
	seedGuestPingTargets(t, br)

	// 打开访客开关（走真实接口）。
	if status, body := br.do("PUT", "/api/v1/settings/guest", map[string]any{"enabled": true}, true); status != 200 {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}
	return h
}
