package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 首页卡片四种状态的人工核对截图：一屏四态（浅色）+ 深色 + 窄屏。
//
// 为什么要单独一条用例：自动化断言能证明"计算样式是什么"，证明不了"人看起来对不对"
// —— 环细到看不看得见、被糊的那一块边界在哪儿、覆盖层那两行字有没有压在迷你条上、
// 窄屏下卡片会不会被撑破，只有看图才知道。
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）：要看图时设
//
//	$env:PROBE_SHOT_DIR = "D:\_cardstate_shots"; go test ./internal/e2e/ -run TestCardStateScreenshots -v
//
// 产生的三张图：01-home-four-states.png / 02-home-four-states-dark.png / 03-home-narrow.png。
func TestCardStateScreenshots(t *testing.T) {
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

	f := startCardStateFixture(t, loc)

	shots := []struct {
		name  string
		theme string
		w, h  int
	}{
		// 一屏四态：宽屏一行放得下四张卡片（每张约 360px）。
		{"01-home-four-states", "", 1500, 1100},
		// 深色：环在深色底上必须看得见（浅色那一档的 20% 不透明度在这里是看不见的）。
		{"02-home-four-states-dark", "dark", 1500, 1100},
		// 窄屏：Chrome 无头模式最小窗口宽约 485px，拿不到 380；这一档仍然落在
		// <=640px 的移动断点上（卡片单列、卡片宽约 453px）。
		{"03-home-narrow", "", 500, 2200},
	}
	for _, s := range shots {
		cfg := tzHarnessConfig{
			NodeID: f.ids[cardOnlineName], NodeName: cardOnlineName,
			Shot: "cardstate", Theme: s.theme, Scenario: "shot",
			User: "admin", Pass: "a-very-good-password",
		}
		// 注入 + 反代：接口一条都不 mock，页面拿到的仍是真服务端的真数据
		// （首页注入点必须走这一层，所以首页是从服务端取回来再注入的）。
		mock := newMockServer(t, newHarnessProxy(t, "http://"+f.h.addr, cfg, cardStateHarnessJS))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把四态都开出来。
		// 它只在这一处用 —— 自动化断言那条路径故意不用它（长连接 SSE 会让
		// 虚拟时间暂停，大流程有可能卡住），结果改由页面 POST 回来。
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + "/#/",
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
