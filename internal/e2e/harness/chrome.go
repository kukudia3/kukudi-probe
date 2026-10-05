package harness

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// FindChrome 找本机的 Chrome：先看 PROBE_CHROME，再看常见安装位置，最后 PATH。
func FindChrome() string {
	if p := os.Getenv("PROBE_CHROME"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			filepath.Join(os.Getenv("LOCALAPPDATA"), `Google\Chrome\Application\chrome.exe`),
		}
	case "darwin":
		candidates = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	default:
		candidates = []string{"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"}
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"chrome", "google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// RunChromeForResult 打开 pageURL，等页面自己把结果 POST 回来（或超时）。
// windowSize 为空时用默认窗口；窄屏用例靠它落进 (max-width: 640px) 那一档。
//
// extraArgs 是给个别用例补的开关（必须排在 URL 前面）。目前只有一个调用方用它：
// 「跨标签页登录」那条用例要在页面里 window.open 出第二个标签，而无头 Chrome
// 默认会把它当弹窗拦掉（--disable-popup-blocking）。
func RunChromeForResult(t *testing.T, chrome, pageURL string, result chan []byte, timeout time.Duration, windowSize string, extraArgs ...string) []byte {
	t.Helper()
	// 画像目录单独开在系统临时目录里：Chrome 被强杀时子进程可能还握着里面的文件，
	// 放在 t.TempDir() 里会让"清理失败"变成一条测试失败（与被测代码无关）。
	profile, err := os.MkdirTemp("", "probe-tz-chrome-")
	if err != nil {
		t.Fatalf("创建 Chrome 画像目录: %v", err)
	}
	defer func() { _ = os.RemoveAll(profile) }()
	if windowSize == "" {
		windowSize = "1500,1100"
	}
	args := []string{
		"--headless=new",
		"--no-proxy-server",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--user-data-dir=" + profile,
		"--window-size=" + windowSize,
	}
	args = append(args, extraArgs...)
	args = append(args, pageURL)
	cmd := exec.Command(chrome, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 Chrome: %v", err)
	}
	// 这里**故意不用 --dump-dom + --virtual-time-budget**：页面连着一路 SSE，
	// 长连接会让虚拟时间一直暂停，预算可能永远耗不完（整套流程就卡死了）。
	// 结果通道改成"页面自己 POST 回 mock"（也就是本仓库惯用的那一套），
	// 拿到结果之后由我们结束 Chrome。
	killed := false
	defer func() {
		if !killed {
			KillChrome(cmd)
		}
		_ = cmd.Wait()
	}()

	select {
	case raw := <-result:
		killed = true
		KillChrome(cmd)
		return raw
	case <-time.After(timeout):
		t.Fatalf("等浏览器回传结果超时（%s）。Chrome 输出尾部：\n%s", timeout, Tail(out.String(), 1500))
		return nil
	}
}

// KillChrome 结束 Chrome。Windows 上只杀主进程会留下渲染/GPU 子进程攥着画像目录，
// 所以优先用 taskkill /T 整棵树一起收。
func KillChrome(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if cmd.Process.Pid > 0 {
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}

func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("…%s", s[len(s)-n:])
}
