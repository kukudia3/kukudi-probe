package chart

// 这一份是"自己起 Chrome + 走 DevTools 协议"的最小装置，只给**延迟图例**那条用例用
// （见 legend_loss_browser_test.go）。
//
// 为什么不走仓库里另外两套：
//   - RunChromeForResult 那套要页面自己 POST 结果回来，观测值只能来自注入脚本 ——
//     而这条用例要量的是**布局几何**（统计行占了几行、有没有横向溢出），还要按
//     元素外接矩形裁图。这两件事都得由外层在真浏览器的**精确视口**下做；
//   - --virtual-time-budget + --screenshot 那套（见 cardstate_shot_test.go）在
//     本例不可用：详情页挂着一路 SSE，虚拟时间会一直暂停，预算永远耗不完。
//
// 做法与 internal/e2e/ui 的 cdpShot / billingNarrowProbe 完全一致（同一套
// Emulation.setDeviceMetricsOverride + Page.captureScreenshot），只是把
// "连上 → 调用 → 截图"收成一个 session：调用方不必自己数 DevTools 的调用 id。
// ui 那边有四个文件在用自己那份实现，这一轮**不动它们**（只加不改）。
//
// 定死视口是这条用例的前提：--window-size 在无头模式下拿到的布局视口会被窗口
// 装饰吃掉十几像素（见 billingNarrowProbe 的说明），而"三张卡片会不会挤"正好
// 卡在 640 / 700 这些断点附近，差十几像素就是另一个结论。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"probe/internal/e2e/harness"
)

// cdpSession 是一趟"自己起的 Chrome + 一条 DevTools 连接"。
type cdpSession struct {
	t    *testing.T
	ctx  context.Context
	conn *websocket.Conn
	id   int
	buf  *harness.SyncBuffer
}

// startCDPSession 起一个无头 Chrome、连上它的页面目标，并把布局视口定死成 w×h。
//
// 定死之后**重新导航一次**（与 billingNarrowProbe 同一个理由）：让页面从第一帧
// 就跑在这个视口里，而不是先按窗口宽度排一版、再被重排。
func startCDPSession(t *testing.T, chrome, pageURL string, w, h int) *cdpSession {
	t.Helper()
	profile, err := os.MkdirTemp("", "probe-legend-cdp-")
	if err != nil {
		t.Fatalf("创建 Chrome 画像目录: %v", err)
	}
	args := []string{
		"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--disable-extensions", "--hide-scrollbars",
		"--remote-debugging-port=0", "--remote-allow-origins=*",
		"--user-data-dir=" + profile,
		"--window-size=" + strconv.Itoa(w) + "," + strconv.Itoa(h),
		pageURL,
	}
	cmd := exec.Command(chrome, args...)
	buf := &harness.SyncBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		t.Fatalf("启动 Chrome: %v", err)
	}
	t.Cleanup(func() {
		harness.KillChrome(cmd)
		_ = cmd.Wait()
		_ = os.RemoveAll(profile)
	})

	// Chrome 把真正选中的调试端口写在画像目录里的 DevToolsActivePort（第一行）。
	portFile := filepath.Join(profile, "DevToolsActivePort")
	var port string
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(portFile); err == nil {
			if line, _, _ := strings.Cut(string(raw), "\n"); strings.TrimSpace(line) != "" {
				port = strings.TrimSpace(line)
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if port == "" {
		t.Fatalf("等不到 DevToolsActivePort。Chrome 输出尾部：\n%s", harness.Tail(buf.String(), 800))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)
	s := &cdpSession{t: t, ctx: ctx, buf: buf}
	s.conn = dialCDP(t, ctx, port)
	t.Cleanup(func() { s.conn.CloseNow() })

	s.mustCall("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false,
	})
	s.mustCall("Page.navigate", map[string]any{"url": pageURL})
	return s
}

// setViewport 换一个**精确**的布局视口（CSS 像素），与窗口装饰、DPI 无关。
func (s *cdpSession) setViewport(w, h int) {
	s.t.Helper()
	s.mustCall("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false,
	})
}

func (s *cdpSession) nextID() int {
	s.id++
	return s.id
}

func (s *cdpSession) mustCall(method string, params any) json.RawMessage {
	s.t.Helper()
	res, err := cdpCall(s.ctx, s.conn, s.nextID(), method, params)
	if err != nil {
		s.t.Fatalf("%s: %v", method, err)
	}
	return res
}

// eval 求一个 JS 表达式的值（returnByValue）。
func (s *cdpSession) eval(expr string) any {
	s.t.Helper()
	v, err := cdpEval(s.ctx, s.conn, s.nextID(), expr)
	if err != nil {
		s.t.Fatalf("求值失败（%s）：%v", harness.Tail(expr, 160), err)
	}
	return v
}

func (s *cdpSession) evalString(expr string) string {
	s.t.Helper()
	v := s.eval(expr)
	out, _ := v.(string)
	return out
}

func (s *cdpSession) evalBool(expr string) bool {
	s.t.Helper()
	v := s.eval(expr)
	out, _ := v.(bool)
	return out
}

// evalJSON 把返回的对象/数组解进 out。
func (s *cdpSession) evalJSON(expr string, out any) {
	s.t.Helper()
	v := s.eval(expr)
	raw, err := json.Marshal(v)
	if err != nil {
		s.t.Fatalf("序列化浏览器返回值: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		s.t.Fatalf("解析浏览器返回值（%s）：%v\n原始：%s", harness.Tail(expr, 120), err, harness.Tail(string(raw), 400))
	}
}

// waitJS 轮询一个 JS 判据，直到它为真（超时即 Fatal）。
func (s *cdpSession) waitJS(what, expr string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.evalBool(expr) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("等不到「%s」（%s）。页面标题：%q\nChrome 输出尾部：\n%s",
		what, harness.Tail(expr, 160), s.evalString("document.title"), harness.Tail(s.buf.String(), 800))
}

// screenshot 按 clip（页面坐标，见 clipOf）截一张 PNG 并写到 out。
func (s *cdpSession) screenshot(out string, clip any) {
	s.t.Helper()
	params := map[string]any{"format": "png"}
	if clip != nil {
		params["clip"] = clip
	}
	res := s.mustCall("Page.captureScreenshot", params)
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &shot); err != nil {
		s.t.Fatalf("解析截图结果: %v", err)
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		s.t.Fatalf("解码截图: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		s.t.Fatalf("创建截图目录: %v", err)
	}
	if err := os.WriteFile(out, png, 0o644); err != nil {
		s.t.Fatalf("写截图: %v", err)
	}
	s.t.Logf("截图 → %s（%d 字节）", out, len(png))
}

// clipOf 把一段"算外接矩形的 JS"变成 clip 参数，并把元素滚进视口中间。
//
// 返回 nil 表示页面上没有那个元素（调用方决定是跳过还是报错）。
// 坐标要加上 scrollX/scrollY：clip 用的是**页面**坐标，而
// getBoundingClientRect 给的是视口坐标。
func (s *cdpSession) clipOf(scrollJS, rectJS string, pad float64) any {
	s.t.Helper()
	if scrollJS != "" {
		if !s.evalBool(scrollJS) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	var v any
	s.evalJSON(`(function () {
      var r = (`+rectJS+`);
      if (!r) return null;
      return { x: r.left + window.scrollX - `+trimFloat(pad)+`, y: r.top + window.scrollY - `+trimFloat(pad)+`,
               width: r.width + `+trimFloat(2*pad)+`, height: r.height + `+trimFloat(2*pad)+`, scale: 1 };
    })()`, &v)
	return v
}

// dialCDP 连上页面目标的 DevTools WebSocket（与 ui 那边同一套）。
func dialCDP(t *testing.T, ctx context.Context, port string) *websocket.Conn {
	t.Helper()
	var wsURL string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
		if err == nil {
			var list []struct {
				Type string `json:"type"`
				WS   string `json:"webSocketDebuggerUrl"`
			}
			if dec := json.NewDecoder(resp.Body); dec.Decode(&list) == nil {
				for _, e := range list {
					if e.Type == "page" && e.WS != "" {
						wsURL = e.WS
						break
					}
				}
			}
			_ = resp.Body.Close()
		}
		if wsURL != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if wsURL == "" {
		t.Fatal("拿不到页面目标的 webSocketDebuggerUrl")
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("连接 DevTools: %v", err)
	}
	// 一张 1500×1180 的 PNG 的 base64 有几百 KB，默认 32KB 的读上限装不下。
	conn.SetReadLimit(64 << 20)
	return conn
}

// cdpCall 发一次 DevTools 调用并等它自己的那条回包（事件通知直接跳过）。
func cdpCall(ctx context.Context, conn *websocket.Conn, id int, method string, params any) (json.RawMessage, error) {
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, err
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var got struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &got) != nil || got.ID != id {
			continue
		}
		if got.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, got.Error.Message)
		}
		return got.Result, nil
	}
}

// cdpEval 求一个 JS 表达式的值（returnByValue，所以字符串/布尔直接拿得到）。
func cdpEval(ctx context.Context, conn *websocket.Conn, id int, expr string) (any, error) {
	res, err := cdpCall(ctx, conn, id, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return out.Result.Value, nil
}
