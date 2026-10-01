package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"probe/internal/store"
)

// 六档的 X 轴"基准间隔 → 屏幕上**真实**画出来的标签数"，用真 Chrome 跑一遍
// web/chart.js 来量。
//
// 为什么必须有这一条（而不是只做静态断言）：基准间隔这次被改细了
// （6h/12h 从 2/3 分钟改成 1 分钟、3d 从 15 分钟改成 5 分钟、7d 从 30 分钟改成
// 15 分钟）。改细本身不会让画面变糊 —— 放不下时 chart.js 会按标签文本的**实际
// 像素宽度**（measureText + X_LABEL_MIN_GAP）自动按整齐倍数稀疏 ——
// 但"会自动稀疏"这件事只有真的量一次才算验证过：静态断言只能证明代码里有
// measureText，证明不了"这一档画出来是 12 个标签，而不是 672 个挤成一团"。
//
// 三条断言（每条都对应一种"页面上看得出来、测试却抓不到"的坏法）：
//  1. 基准值：六档的 tick_base_sec 就是用户定稿的那张表（1m/1m/1m/2m/5m/15m）；
//  2. 稀疏确实发生了：真实标签数 **小于** "按基准间隔铺满"的条数
//     （1h 档 60 条、7d 档 672 条 —— 不稀疏的话就是一片糊在一起的黑块）；
//  3. 稀疏没过头：标签数落在 [4, 24] —— 少到读不出时间、多到没人会逐个读，
//     都在这个区间之外（X_LABEL_MIN_GAP=28px 的取值依据见 chart.js 注释）。
//
// 跳过条件：找不到 Chrome。CI 镜像里通常没有 —— 那种环境下这条用例会 Skip，
// 而不是把整套测试拖红。本机（含开发机）装了 Chrome 就一定会真跑。
// 可用 PROBE_CHROME 指定可执行文件路径。
func TestXAxisLabelCountsInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	chartJS, err := os.ReadFile(filepath.Join("..", "..", "web", "chart.js"))
	if err != nil {
		t.Fatalf("读取 web/chart.js: %v", err)
	}

	// 夹具直接来自**后端那张表**（store.PingRanges）：这条用例要量的正是
	// "后端给的基准间隔在真实浏览器里画成几个标签"，两边必须是同一份数据。
	want := map[string]int64{"1h": 60, "6h": 60, "12h": 60, "1d": 120, "3d": 300, "7d": 900}
	type fixture struct {
		Key    string       `json:"key"`
		Base   int64        `json:"base"`
		Bucket int64        `json:"bucket"`
		Window int64        `json:"window"`
		Points [][3]float64 `json:"points"`
	}
	var fixtures []fixture
	for _, r := range store.PingRanges() {
		points := make([][3]float64, 0, r.Points())
		// 起点对齐到桶网格（与 Range.window 同一套对齐：每个点代表一个完整桶）。
		start := int64(1700000000)
		start -= start % r.Bucket
		for i := int64(0); i < int64(r.Points()); i++ {
			ts := float64(start + i*r.Bucket)
			points = append(points, [3]float64{ts, float64(20 + (i%7)*3), float64(30 + (i%11)*4)})
		}
		fixtures = append(fixtures, fixture{
			Key: r.Key, Base: r.TickBaseSec, Bucket: r.Bucket,
			Window: int64(r.Window.Seconds()), Points: points,
		})
	}
	if len(fixtures) != len(want) {
		t.Fatalf("档位数量 = %d，期望 %d", len(fixtures), len(want))
	}
	for _, f := range fixtures {
		if w, ok := want[f.Key]; !ok || f.Base != w {
			t.Fatalf("%s 档基准间隔 = %d，期望 %d（用户定稿的那张表）", f.Key, f.Base, w)
		}
	}

	page := buildXAxisPage(string(chartJS), fixtures)
	dir := t.TempDir()
	html := filepath.Join(dir, "xaxis.html")
	if err := os.WriteFile(html, []byte(page), 0o644); err != nil {
		t.Fatalf("写入测试页面: %v", err)
	}

	dom := runChromeDump(t, chrome, dir, html)

	const marker = "XAXIS:"
	at := strings.Index(dom, marker)
	if at < 0 {
		t.Fatalf("页面没有产出 %s 结果（Chrome 的输出尾部：%s）", marker, tail(dom, 400))
	}
	raw := dom[at+len(marker):]
	if end := strings.IndexAny(raw, "<\n"); end >= 0 {
		raw = raw[:end]
	}
	raw = strings.ReplaceAll(raw, "&quot;", `"`)
	var got map[string]int
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("解析浏览器结果 %q: %v", raw, err)
	}

	for _, f := range fixtures {
		labels, ok := got[f.Key]
		if !ok {
			t.Errorf("%s 档没有量到标签数", f.Key)
			continue
		}
		naive := int(f.Window / f.Base)
		// 把量到的数打出来（-v 可见）：这些数字是"稀疏到什么程度"的唯一证据，
		// 失败时也要靠它分辨是"没稀疏"还是"稀疏过头"。
		t.Logf("%s 档：基准 %d 秒 → 画面上 %d 个标签（按基准铺满是 %d 条）",
			f.Key, f.Base, labels, naive)
		if labels >= naive {
			t.Errorf("%s 档标签数 = %d，按基准间隔（%d 秒）铺满应该是 %d 条 —— 说明自动稀疏没生效（画面上就是一片糊在一起的黑块）",
				f.Key, labels, f.Base, naive)
		}
		if labels < 4 || labels > 24 {
			t.Errorf("%s 档标签数 = %d，超出 [4, 24]：少于 4 个读不出时间轴，多于 24 个没人会逐个读（见 chart.js 的 X_LABEL_MIN_GAP）",
				f.Key, labels)
		}
	}
}

// buildXAxisPage 造一个**自包含**的测试页：chart.js 内联、没有外部请求、没有 mock。
//
// 只测 labelFits/xLabelStep 这一条链路，所以不需要 app.js 与整个 index.html ——
// 每档一个固定宽度（900px，与真实的半宽图同量级）的画布，画完把每档真实画出来的
// 标签数写进 title（--dump-dom 会把 title 一起 dump 出来）。
func buildXAxisPage(chartJS string, fixtures any) string {
	data, err := json.Marshal(fixtures)
	if err != nil {
		panic(err)
	}
	return `<!doctype html><html><head><meta charset="utf-8">
<title>XAXIS:{}</title></head><body>
<script>` + chartJS + `</script>
<script>
// 真实标签是在 draw() 里 fillText 画出来的：把这一层的调用记下来，
// 按 Y 坐标（标签画在绘图区底部往下 4px，也就是 画布高-16）筛出 X 轴标签。
(function () {
  var proto = CanvasRenderingContext2D.prototype;
  var original = proto.fillText;
  window.__labels = {};
  window.__current = null;
  proto.fillText = function (text, x, y) {
    if (window.__current && Math.abs(y - window.__current.bottomY) < 0.51) {
      window.__labels[window.__current.key] = (window.__labels[window.__current.key] || 0) + 1;
    }
    return original.apply(this, arguments);
  };
})();

var fixtures = ` + string(data) + `;
var W = 900, H = 190;
function pad2(n) { return (n < 10 ? '0' : '') + n; }
function clockOf(ts) {
  var d = new Date(ts * 1000);
  return pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
function dateOf(ts) {
  var d = new Date(ts * 1000);
  return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
}
// 与 app.js 的 RANGE_X_FORMAT 一致（3d/7d 的格式取决于抽稀后的实际间隔）。
function xFormatFor(key) {
  if (key === '3d' || key === '7d') {
    return function (ts, step) { return step >= 86400 ? dateOf(ts) : dateOf(ts) + ' ' + clockOf(ts); };
  }
  return function (ts) { return clockOf(ts); };
}

fixtures.forEach(function (f) {
  var canvas = document.createElement('canvas');
  canvas.style.width = W + 'px';
  canvas.style.height = H + 'px';
  document.body.appendChild(canvas);
  var chart = window.ProbeChart.create(canvas, {
    tickBaseSec: f.base, bucketSec: f.bucket, xFormat: xFormatFor(f.key),
    unit: ' ms', yFormat: function (v) { return v.toFixed(0); }
  });
  window.__current = { key: f.key, bottomY: H - 16 };
  chart.setData([{ label: 'A', color: '#2563eb', points: f.points }], {});
  window.__current = null;
});

document.title = 'XAXIS:' + JSON.stringify(window.__labels);
</script></body></html>`
}

// runChromeDump 用 headless Chrome 打开页面并 dump 出最终 DOM。
func runChromeDump(t *testing.T, chrome, dir, page string) string {
	t.Helper()
	profile := filepath.Join(dir, "profile")
	args := []string{
		"--headless=new",
		"--no-proxy-server",
		"--disable-gpu",
		"--no-first-run",
		"--user-data-dir=" + profile,
		"--window-size=1000,900",
		"--virtual-time-budget=5000",
		"--dump-dom",
		"file:///" + filepath.ToSlash(page),
	}
	cmd := exec.Command(chrome, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Chrome 退出失败: %v\n%s", err, tail(string(out), 800))
	}
	return string(out)
}

// findChrome 找本机的 Chrome：先看 PROBE_CHROME，再看常见安装位置，最后 PATH。
func findChrome() string {
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

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("…%s", s[len(s)-n:])
}
