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

// Y 轴改动的人工核对截图：延迟图近景一张。
//
// 自动化断言能证明"刻度是 0/200/400/600/800"，证明不了"曲线与刻度贴合得好看"——
// 上面还剩多少空白、刻度会不会挤在一起、三条曲线分不分得开，只有看图才知道。
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）。要看图时设：
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestYAxisScreenshot -v
//
// 数据与 TestYAxisTopMatchesStepRuleInRealBrowser 用的是**同一份**
// （startYAxisFixture）：三条延迟曲线 220…550ms，截图里那张图的轴顶应当是 800。
func TestYAxisScreenshot(t *testing.T) {
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

	h, nodeID := startYAxisFixture(t)
	proxy := newShotProxyWith(t, "http://"+h.addr, tzHarnessConfig{
		NodeID: nodeID, NodeName: "yaxis-01", Scenario: "yaxis", Shot: "lat",
		User: "admin", Pass: "a-very-good-password",
	}, yaxisHarnessJS)
	mock := newMockServer(t, proxy)
	out := filepath.Join(outDir, "yaxis-latency-550.png")

	// 视口只有 420px 高：近景里只留「延迟」那一张卡（其它块由自检脚本隐藏）。
	args := []string{
		"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
		"--hide-scrollbars",
		"--user-data-dir=" + t.TempDir(),
		"--window-size=1500,420",
		"--virtual-time-budget=60000",
		"--screenshot=" + out,
		mock.URL + "/",
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
		t.Fatalf("截图超时。Chrome 输出尾部：\n%s", tail(buf.String(), 800))
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("截图没有生成: %v\nChrome 输出尾部：\n%s", err, tail(buf.String(), 800))
	}
	t.Logf("yaxis-latency-550 → %s（%s 字节，窗口 %s）", out, strconv.FormatInt(st.Size(), 10), "1500x420")
}
