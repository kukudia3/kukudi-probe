package qr

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 这个文件是 internal/qr 的验收。三层，缺一层都不够：
//
//  1. **结构断言**：定位/定时/对齐图案、暗模块、格式信息（BCH(15,5)）、
//     版本信息（BCH(18,6)）都是标准里的常数，写死比对 —— 没有解释空间。
//  2. **往返断言**（roundTrip）：把矩阵**按标准反着读回来**。读的那一侧是
//     照着 ISO/IEC 18004 重新写的一份（功能模块图、之字形取位、去交错、
//     RS 校验子），不复用编码器的任何函数 —— 否则"同一个错写两遍"也会全绿。
//  3. **参考实现比对**：与一个完全独立的实现（Python 的 qrcode 库）逐模块比。
//     它**不在** go test 里跑（给仓库引第三方依赖是被禁止的），做法是：
//
//	$env:QR_DUMP_FILE="$env:TEMP\qr-dump.json"; go test ./internal/qr/ -run TestDumpForReference -v
//
//	然后用 qrcode 库生成同一批文本的矩阵，比对 size/version/mask/每个模块。
//	命令行与脚本见本文件末尾的注释。

// 现场：一段真实形状的 otpauth:// 链接（含小写、大写、数字与符号）+ 边界长度。
var sampleTexts = []string{
	"otpauth://totp/Probe:admin?secret=JBSWY3DPEHPK3PXP&issuer=Probe&algorithm=SHA1&digits=6&period=30",
	"otpauth://totp/Probe:admin?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Probe",
	"a",
	"hello world",
	strings.Repeat("x", 60),
	strings.Repeat("y", 120),
	strings.Repeat("z", 200),
	strings.Repeat("w", 271), // 版本 10 + L 的字节容量上限
}

// TestEcSpecsMatchTotalCodewords 钉住分块表与码字总数一致。
//
// 这两张表一个是"每块多少数据"，一个是"一共多少码字"，任何一处抄错都会
// 让交错出来的比特流整体错位 —— 而表现只是"扫不出来"。
func TestEcSpecsMatchTotalCodewords(t *testing.T) {
	for _, level := range []Level{LevelL, LevelM} {
		for version := 1; version <= maxVersion; version++ {
			spec := ecSpecs[level][version]
			sum := 0
			for _, n := range spec.dataPerBlock {
				sum += n + spec.ecPerBlock
			}
			if sum != totalCodewords[version] {
				t.Errorf("%s 级版本 %d：分块表合计 %d 个码字，标准总数是 %d",
					level, version, sum, totalCodewords[version])
			}
			// 短块必须排在前面：交错时前几轮只有短块在出字节。
			for i := 1; i < len(spec.dataPerBlock); i++ {
				if spec.dataPerBlock[i] < spec.dataPerBlock[i-1] {
					t.Errorf("%s 级版本 %d：数据块长度不是非递减（%v）—— 交错顺序会错",
						level, version, spec.dataPerBlock)
				}
			}
		}
	}
}

// TestEncodeStructure 检查功能图案与格式/版本信息的固定比特。
func TestEncodeStructure(t *testing.T) {
	for _, level := range []Level{LevelL, LevelM} {
		code, err := Encode(sampleTexts[0], level)
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		if code.Size != code.Version*4+17 {
			t.Fatalf("边长 %d 与版本 %d 不符", code.Size, code.Version)
		}
		size := code.Size

		// 定位图案：三个角的 7×7 同心方框（外圈深、次圈浅、内 3×3 深）。
		for _, origin := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
			ox, oy := origin[0], origin[1]
			for dy := 0; dy < 7; dy++ {
				for dx := 0; dx < 7; dx++ {
					dist := max(abs(dx-3), abs(dy-3))
					want := dist != 2
					if got := code.Modules[oy+dy][ox+dx]; got != want {
						t.Fatalf("%s：定位图案 (%d,%d) 的模块 = %v，期望 %v",
							level, ox+dx, oy+dy, got, want)
					}
				}
			}
		}
		// 分隔符：定位图案外侧一圈必须是浅色。
		for i := 0; i < 8; i++ {
			if code.Modules[7][i] || code.Modules[i][7] ||
				code.Modules[7][size-1-i] || code.Modules[size-1-i][7] {
				t.Fatalf("%s：定位图案的分隔符不是浅色（第 7 行/列）", level)
			}
		}
		// 定时图案：第 6 行与第 6 列在定位图案之间交替，偶数格深色。
		for i := 8; i < size-8; i++ {
			if want := i%2 == 0; code.Modules[6][i] != want || code.Modules[i][6] != want {
				t.Fatalf("%s：定时图案在 %d 处 = %v/%v，期望 %v",
					level, i, code.Modules[6][i], code.Modules[i][6], want)
			}
		}
		// 固定深色模块。
		if !code.Modules[size-8][8] {
			t.Fatalf("%s：固定深色模块 (8,%d) 不是深色", level, size-8)
		}
		// 格式信息：两份拷贝必须一致，且能独立解出等级与掩码。
		gotLevel, gotMask := readFormat(t, code)
		if gotLevel != level || gotMask != code.Mask {
			t.Fatalf("%s：格式信息解出 level=%v mask=%d，期望 level=%v mask=%d",
				level, gotLevel, gotMask, level, code.Mask)
		}
		// 版本信息（版本 7 起）：两份拷贝一致。
		if code.Version >= 7 {
			if v := readVersion(t, code); v != code.Version {
				t.Fatalf("版本信息解出 %d，期望 %d", v, code.Version)
			}
		}
	}
}

// readFormat 独立解出格式信息（15 位 BCH + 0x5412 掩码），并校验两份拷贝一致。
func readFormat(t *testing.T, c *Code) (Level, int) {
	t.Helper()
	size := c.Size
	pick := func(coords [][2]int) int {
		bits := 0
		for i, xy := range coords {
			if c.Modules[xy[1]][xy[0]] {
				bits |= 1 << uint(i)
			}
		}
		return bits
	}
	// 位置按标准：低位在前。
	first := make([][2]int, 0, 15)
	for i := 0; i <= 5; i++ {
		first = append(first, [2]int{8, i})
	}
	first = append(first, [2]int{8, 7}, [2]int{8, 8}, [2]int{7, 8})
	for i := 9; i < 15; i++ {
		first = append(first, [2]int{14 - i, 8})
	}
	second := make([][2]int, 0, 15)
	for i := 0; i < 8; i++ {
		second = append(second, [2]int{size - 1 - i, 8})
	}
	for i := 8; i < 15; i++ {
		second = append(second, [2]int{8, size - 15 + i})
	}

	a := pick(first) ^ 0x5412
	b := pick(second) ^ 0x5412
	if a != b {
		t.Fatalf("格式信息的两份拷贝不一致：%015b vs %015b", a, b)
	}
	// BCH(15,5)：整串必须能被生成多项式 0x537 整除。
	if rem := bchRemainder(a, 0x537, 10); rem != 0 {
		t.Fatalf("格式信息的 BCH 余式 = %d，期望 0", rem)
	}
	data := a >> 10
	level := LevelM
	if data>>3 == 1 {
		level = LevelL
	} else if data>>3 != 0 {
		t.Fatalf("格式信息里的纠错等级 = %d，本包只支持 L(1)/M(0)", data>>3)
	}
	return level, data & 0b111
}

// readVersion 独立解出版本信息（18 位 BCH）。
func readVersion(t *testing.T, c *Code) int {
	t.Helper()
	size := c.Size
	bits := 0
	for i := 0; i < 18; i++ {
		a := size - 11 + i%3
		b := i / 3
		if c.Modules[b][a] {
			bits |= 1 << uint(i)
		}
		if c.Modules[a][b] != c.Modules[b][a] {
			t.Fatalf("版本信息的两份拷贝在 %d 位不一致", i)
		}
	}
	if rem := bchRemainder(bits, 0x1F25, 12); rem != 0 {
		t.Fatalf("版本信息的 BCH 余式 = %d，期望 0", rem)
	}
	return bits >> 12
}

func bchRemainder(value, poly, degree int) int {
	rem := value
	for i := 17; i >= degree; i-- {
		if rem&(1<<uint(i)) != 0 {
			rem ^= poly << uint(i-degree)
		}
	}
	return rem
}

// TestRoundTrip 把矩阵按标准反着读回来，比对原始文本。
//
// 这一条覆盖的是"掩码、之字形取位、块交错、RS 纠错"这四件最容易写错的事：
// 读的那一份实现（functionMap / readCodewords / deinterleave / syndromesZero）
// 全部照标准重写，不复用编码器的函数。
func TestRoundTrip(t *testing.T) {
	for _, level := range []Level{LevelL, LevelM} {
		for _, text := range sampleTexts {
			code, err := Encode(text, level)
			if err != nil {
				// 超出该等级的容量（M 级装不下 271 字节那一条）：不是错，跳过长样本。
				continue
			}
			got := roundTrip(t, code)
			if got != text {
				t.Fatalf("%s：往返读回的内容不一样\n得到 %q\n期望 %q", level, got, text)
			}
		}
	}
}

// TestEncodeRejectsTooLong 超长内容必须报错，而不是静默截断。
func TestEncodeRejectsTooLong(t *testing.T) {
	if _, err := Encode(strings.Repeat("x", 272), LevelL); err == nil {
		t.Fatal("273 字节超过了版本 10 + L 的容量，应当报错")
	}
	// 少一个字节就必须成功（否则上面的断言可能只是"永远都失败"）。
	if _, err := Encode(strings.Repeat("x", 271), LevelL); err != nil {
		t.Fatalf("271 字节应当能编下: %v", err)
	}
}

// TestPNG 渲染出的字节要是一张真 PNG，尺寸与参数一致。
func TestPNG(t *testing.T) {
	code, err := Encode(sampleTexts[0], LevelM)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	data, err := code.PNG(DefaultScale, DefaultQuiet)
	if err != nil {
		t.Fatalf("渲染 PNG 失败: %v", err)
	}
	if len(data) < 8 || string(data[1:4]) != "PNG" {
		t.Fatalf("不是 PNG（前 8 字节 %v）", data[:min(8, len(data))])
	}
	want := (code.Size + 2*DefaultQuiet) * DefaultScale
	if got := pngSide(t, data); got != want {
		t.Fatalf("PNG 边长 = %d，期望 %d", got, want)
	}
	// 像素普查：符号区里的深色像素必须恰好等于"深色模块数 × scale²"。
	//
	// 这条断言是被一次真实事故逼出来的：渲染代码里的切片起点少加了一个偏移，
	// 于是每一行都从左边涂到某个位置 —— 图看着"像那么回事"（尺寸对、PNG 合法、
	// 有静区），但扫不出来，而当时所有结构断言都是绿的。
	drawn, wantDark, quietDark := pngSymbolDark(t, data, code, DefaultScale, DefaultQuiet)
	if drawn != wantDark {
		t.Fatalf("符号区里的深色像素 = %d，期望 %d（渲染出来的图案与矩阵不一致）", drawn, wantDark)
	}
	if quietDark != 0 {
		t.Fatalf("静区里有 %d 个深色像素（静区必须是纯白）", quietDark)
	}
	if _, err := code.PNG(0, 4); err == nil {
		t.Fatal("scale=0 应当报错")
	}
	if _, err := code.PNG(DefaultScale, -1); err == nil {
		t.Fatal("quiet=-1 应当报错")
	}
	// 诊断钩子：把这张图写到指定路径（用手机扫一下、或者肉眼看一眼）。
	if out := os.Getenv("QR_PNG_OUT"); out != "" {
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatalf("写 %s: %v", out, err)
		}
		t.Logf("已写出二维码 PNG：%s（%d 字节）", out, len(data))
	}
}

// TestDumpForReference 把矩阵写成 JSON，供独立的参考实现比对（默认跳过）。
//
// 为什么不在测试里直接调参考实现：那需要给仓库加一个第三方依赖（本仓库明令禁止）。
// 所以这一条只在显式给了 QR_DUMP_FILE 时才写文件，核对在仓库外做一次。
func TestDumpForReference(t *testing.T) {
	out := os.Getenv("QR_DUMP_FILE")
	if out == "" {
		t.Skip("没设 QR_DUMP_FILE：参考实现比对默认不跑（见文件头注释）")
	}
	type dump struct {
		Text    string  `json:"text"`
		Level   string  `json:"level"`
		Version int     `json:"version"`
		Mask    int     `json:"mask"`
		Size    int     `json:"size"`
		Modules [][]int `json:"modules"`
		// Masks 是**每一个**掩码（0~7）下的完整矩阵。
		//
		// 为什么要给全部 8 份而不是只给选中的那份：参考实现的评分口径与标准有
		// 一处已知差异（它在"格式信息还没写"的矩阵上评分，本包按标准在**完整**
		// 符号上评分），于是同一份内容可能选出不同的掩码。把 8 份都给出去，
		// 比对就与"选哪个掩码"完全无关 —— 同一个掩码下必须逐模块一致。
		Masks [][][]int `json:"masks"`
		// Scores 是 8 个掩码各自的四条规则得分 [r1 r2 r3 r4]（总分 = 四者之和）。
		Scores [][4]int `json:"scores"`
	}
	var all []dump
	for _, level := range []Level{LevelL, LevelM} {
		for _, text := range sampleTexts {
			code, err := Encode(text, level)
			if err != nil {
				continue // 超过该等级的容量：参考实现那边也不会有
			}
			version, err := pickVersion(len([]byte(text)), level)
			if err != nil {
				t.Fatalf("选版本: %v", err)
			}
			base := newBaseMatrix(version)
			final, err := buildCodewords([]byte(text), version, ecSpecs[level][version])
			if err != nil {
				t.Fatalf("生成码字: %v", err)
			}
			scores := make([][4]int, 8)
			masks := make([][][]int, 8)
			for mask := 0; mask < 8; mask++ {
				m := buildWithMask(base, level, final, mask)
				r1, r2, r3, r4 := penaltyParts(m)
				scores[mask] = [4]int{r1, r2, r3, r4}
				masks[mask] = rowsOf(m.modules)
			}
			all = append(all, dump{
				Text: text, Level: level.String(), Version: code.Version,
				Mask: code.Mask, Size: code.Size, Modules: rowsOf(code.Modules),
				Masks: masks, Scores: scores,
			})
		}
	}
	raw, err := json.MarshalIndent(all, "", " ")
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		t.Fatalf("写 %s: %v", out, err)
	}
	t.Logf("已写出 %d 个矩阵到 %s", len(all), out)
}

// rowsOf 把模块矩阵转成 0/1 的二维整数（便于序列化给参考实现比对）。
func rowsOf(modules [][]bool) [][]int {
	rows := make([][]int, len(modules))
	for y := range modules {
		rows[y] = make([]int, len(modules[y]))
		for x, v := range modules[y] {
			if v {
				rows[y][x] = 1
			}
		}
	}
	return rows
}
