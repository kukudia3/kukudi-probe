// Package qr 是一个"够用就好"的 QR 码编码器：只用 Go 标准库，不联网、不引依赖。
//
// 为什么要自己写：面板要显示的是 otpauth:// 链接，里面装着用户的 TOTP 种子。
// 把它发给任何**在线**二维码服务，等于把第二因素交给第三方（也违反本仓库
// "前端禁止任何外部 URL"的硬约束）。所以二维码只能在服务端自己算出来。
//
// 覆盖范围是**刻意**划小的（而不是能力不足）：
//   - 只做字节模式 —— otpauth:// 链接就是一段 ASCII/URL 文本；
//   - 只做版本 1~10 —— 一条 otpauth:// 链接约 100 字节，版本 5（M 级）就装得下，
//     留了一倍余量；版本 11~40 的表格永远不会被执行到，却要有人去核对；
//   - 只做 L/M 两级纠错 —— M 是行业默认（约 15% 冗余），L 是给更长链接留的后备；
//   - 不做数字/字母数字/汉字模式、不做结构化追加（那些需要多段优化，属于另一个量级）。
//
// 正确性靠三样东西撑着（见 qr_test.go）：
//  1. 结构断言：定位/定时/对齐图案、格式信息（BCH(15,5)）、版本信息（BCH(18,6)）
//     都按 ISO/IEC 18004 的固定比特写死比对 —— 这些是标准里的常数，没有解释空间；
//  2. 往返断言：把矩阵**按标准反着读回来**（解掩码、去交错、逐块算 RS 校验子），
//     还原出的字节必须等于原始文本；
//  3. 交叉验证：与一个完全独立的实现（Python 的 qrcode 库）逐模块比对矩阵。
//     这一条不在 go test 里跑（不能给仓库引入第三方依赖），做法见 qr_test.go 顶部。
package qr

import (
	"errors"
	"fmt"
	"math"
)

// Level 是纠错等级。只支持 L 与 M（见包注释）。
type Level int

const (
	// LevelL 约能纠正 7% 的损坏。
	LevelL Level = iota
	// LevelM 约能纠正 15% 的损坏，是二维码的行业默认档。
	LevelM
)

func (l Level) String() string {
	switch l {
	case LevelL:
		return "L"
	case LevelM:
		return "M"
	default:
		return fmt.Sprintf("Level(%d)", int(l))
	}
}

// formatBits 返回纠错等级在格式信息里的 2 位编码（ISO/IEC 18004 表 25）：
// L=01、M=00（Q=11、H=10 本包不支持）。
func (l Level) formatBits() int {
	if l == LevelL {
		return 0b01
	}
	return 0b00
}

// maxVersion 是本包支持的版本上限。
const maxVersion = 10

// totalCodewords 是版本 1~10 的码字总数（数据码字 + 纠错码字）。
//
// 它同时是分块表的一致性标尺：把 ecSpecs 里每块的 (数据+纠错) 加起来必须等于它，
// 否则说明表抄错了（见 TestEcSpecsMatchTotalCodewords）。
var totalCodewords = [maxVersion + 1]int{
	0, 26, 44, 70, 100, 134, 172, 196, 242, 292, 346,
}

// remainderBits 是每个版本在最后一个码字之后补的"剩余位"（ISO/IEC 18004 表 1），
// 下标 0 是占位、下标 1~10 对应版本 1~10，数值原样照抄标准：
//
//	[0, 0, 7, 7, 7, 7, 7, 0, 0, 0, 0]
//
// 它们不是数据、也不是纠错，就是为了把矩阵铺满；编码时必须按 0 参与掩码，
// 否则数据落点会整体错位。
//
// 这张表当前**不参与计算**，只作标准数值的对照记录："补 0 并照样套掩码"这条规则
// 已经内建在 drawData 里（bitIndex >= total 时保持 dark=false，但仍旧过一遍掩码），
// 所以没有任何代码需要读它 —— 留在这里是为了让上面那条规则有出处可查。

// alignPositions 是版本 1~10 的对齐图案中心坐标（ISO/IEC 18004 附录 E）。
//
// 写死成表而不是按公式算：这张表只有 10 行，公式却要处理 version==32 的特例，
// 而"对齐图案坐标错了"的表现是"扫不出来"，没有任何提示。
var alignPositions = [maxVersion + 1][]int{
	nil,
	nil,
	{6, 18},
	{6, 22},
	{6, 26},
	{6, 30},
	{6, 34},
	{6, 22, 38},
	{6, 24, 42},
	{6, 26, 46},
	{6, 28, 50},
}

// ecSpec 是一个"版本 + 纠错等级"的分块方案。
//
// dataPerBlock 是**按块展开**的数据码字数（短的块在前，与标准里的交错顺序一致），
// 而不是标准表里的 (块数, 每块数据数) 分组写法：交错那一步要按块逐个取字节，
// 展开之后不需要再判断"这个块属于哪一组"。
type ecSpec struct {
	ecPerBlock   int
	dataPerBlock []int
}

// ecSpecs 是版本 1~10 在 L/M 两级下的分块方案（ISO/IEC 18004 表 9）。
var ecSpecs = map[Level][maxVersion + 1]ecSpec{
	LevelL: {
		{},
		{7, []int{19}},
		{10, []int{34}},
		{15, []int{55}},
		{20, []int{80}},
		{26, []int{108}},
		{18, []int{68, 68}},
		{20, []int{78, 78}},
		{24, []int{97, 97}},
		{30, []int{116, 116}},
		{18, []int{68, 68, 69, 69}},
	},
	LevelM: {
		{},
		{10, []int{16}},
		{16, []int{28}},
		{26, []int{44}},
		{18, []int{32, 32}},
		{24, []int{43, 43}},
		{16, []int{27, 27, 27, 27}},
		{18, []int{31, 31, 31, 31}},
		{22, []int{38, 38, 39, 39}},
		{22, []int{36, 36, 36, 37, 37}},
		{26, []int{43, 43, 43, 43, 44}},
	},
}

// ErrTooLong 表示内容超过了本包支持的最大容量（版本 10 + L 级）。
var ErrTooLong = errors.New("内容太长：超出二维码版本 10 的容量")

// Code 是一个编码完成的二维码。
type Code struct {
	// Size 是矩阵边长（模块数，不含静区）。
	Size int
	// Version 是二维码版本（1~10）。
	Version int
	// Level 是纠错等级。
	Level Level
	// Mask 是最终选中的掩码编号（0~7）。
	Mask int
	// Modules 是模块矩阵，[行][列]，true 表示深色。
	Modules [][]bool
}

// Encode 把文本编码成二维码（字节模式）。
func Encode(text string, level Level) (*Code, error) {
	data := []byte(text)
	version, err := pickVersion(len(data), level)
	if err != nil {
		return nil, err
	}
	spec := ecSpecs[level][version]

	final, err := buildCodewords(data, version, spec)
	if err != nil {
		return nil, err
	}

	base := newBaseMatrix(version)
	best := -1
	bestScore := 0
	var bestModules [][]bool
	for mask := 0; mask < 8; mask++ {
		modules := buildWithMask(base, level, final, mask)
		score := penalty(modules)
		// 掩码选择规则里"分数相同取编号小的"（ISO/IEC 18004 8.8.2），
		// 所以这里用严格小于。
		if best < 0 || score < bestScore {
			best, bestScore, bestModules = mask, score, modules.modules
		}
	}
	return &Code{
		Size:    base.size,
		Version: version,
		Level:   level,
		Mask:    best,
		Modules: bestModules,
	}, nil
}

// buildWithMask 生成"铺好数据 + 套上该掩码 + 写好格式信息"的完整矩阵。
func buildWithMask(base *matrix, level Level, final []byte, mask int) *matrix {
	modules := base.clone()
	drawData(modules, base.isFunc, final, mask)
	drawFormatBits(modules, level, mask)
	return modules
}

// charCountBits 是字节模式的字符计数指示符长度。
func charCountBits(version int) int {
	if version <= 9 {
		return 8
	}
	return 16
}

// pickVersion 选能装下这份数据的最小版本。
func pickVersion(dataLen int, level Level) (int, error) {
	specs, ok := ecSpecs[level]
	if !ok {
		return 0, fmt.Errorf("不支持的纠错等级 %v", level)
	}
	for version := 1; version <= maxVersion; version++ {
		spec := specs[version]
		capacity := 0
		for _, n := range spec.dataPerBlock {
			capacity += n
		}
		// 4 位模式指示符 + 字符计数 + 数据本体。终止符与填充是"剩余空间"的事，
		// 不参与容量判断。
		if 4+charCountBits(version)+8*dataLen <= capacity*8 {
			return version, nil
		}
	}
	return 0, ErrTooLong
}

// buildCodewords 生成最终的码字序列（已按标准交错）。
func buildCodewords(data []byte, version int, spec ecSpec) ([]byte, error) {
	total := 0
	for _, n := range spec.dataPerBlock {
		total += n
	}

	bits := make([]bool, 0, total*8)
	appendBits := func(value, length int) {
		for i := length - 1; i >= 0; i-- {
			bits = append(bits, (value>>uint(i))&1 != 0)
		}
	}
	appendBits(0b0100, 4) // 字节模式
	appendBits(len(data), charCountBits(version))
	for _, b := range data {
		appendBits(int(b), 8)
	}
	capacity := total * 8
	// 终止符：最多 4 个 0，且不能越过容量。
	for i := 0; i < 4 && len(bits) < capacity; i++ {
		bits = append(bits, false)
	}
	// 补到字节边界。
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	codewords := make([]byte, 0, total)
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			if bits[i+j] {
				b |= 1 << uint(7-j)
			}
		}
		codewords = append(codewords, b)
	}
	// 填充码字：0xEC / 0x11 交替（ISO/IEC 18004 8.4.9）。
	for i := 0; len(codewords) < total; i++ {
		if i%2 == 0 {
			codewords = append(codewords, 0xEC)
		} else {
			codewords = append(codewords, 0x11)
		}
	}

	blocks := make([][]byte, len(spec.dataPerBlock))
	eccs := make([][]byte, len(spec.dataPerBlock))
	offset := 0
	for i, n := range spec.dataPerBlock {
		blocks[i] = codewords[offset : offset+n]
		offset += n
		eccs[i] = rsEncode(blocks[i], spec.ecPerBlock)
	}

	maxData := 0
	for _, n := range spec.dataPerBlock {
		if n > maxData {
			maxData = n
		}
	}
	final := make([]byte, 0, totalCodewords[version])
	for i := 0; i < maxData; i++ {
		for _, blk := range blocks {
			if i < len(blk) {
				final = append(final, blk[i])
			}
		}
	}
	for i := 0; i < spec.ecPerBlock; i++ {
		for _, ecc := range eccs {
			final = append(final, ecc[i])
		}
	}
	want := totalCodewords[version]
	if len(final) != want {
		return nil, fmt.Errorf("内部错误：版本 %d 的码字数 = %d，期望 %d", version, len(final), want)
	}
	return final, nil
}

// ---------------------------------------------------------------- GF(256) 与 RS

// GF(256) 的指数/对数表（本原多项式 0x11D，本原元 α=2）。
var (
	gfExp [512]byte
	gfLog [256]int
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

// rsGenerator 返回 degree 次的 RS 生成多项式 ∏(x-α^i)，i=0..degree-1。
//
// 系数按**从高次到低次**排列，最高次项恒为 1（所以返回长度是 degree+1）。
func rsGenerator(degree int) []byte {
	gen := []byte{1}
	for i := 0; i < degree; i++ {
		next := make([]byte, len(gen)+1)
		for j, c := range gen {
			next[j] ^= c                    // 乘 x
			next[j+1] ^= gfMul(c, gfExp[i]) // 乘 α^i
		}
		gen = next
	}
	return gen
}

// rsEncode 计算 data 的 ecLen 个纠错码字（多项式长除的余式）。
func rsEncode(data []byte, ecLen int) []byte {
	gen := rsGenerator(ecLen)
	rem := make([]byte, ecLen)
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[ecLen-1] = 0
		for i := 0; i < ecLen; i++ {
			rem[i] ^= gfMul(gen[i+1], factor)
		}
	}
	return rem
}

// ---------------------------------------------------------------- 矩阵

type matrix struct {
	size    int
	modules [][]bool
	isFunc  [][]bool
}

func (m *matrix) clone() *matrix {
	out := &matrix{
		size:    m.size,
		modules: make([][]bool, m.size),
		isFunc:  make([][]bool, m.size),
	}
	for i := 0; i < m.size; i++ {
		out.modules[i] = make([]bool, m.size)
		out.isFunc[i] = make([]bool, m.size)
		copy(out.modules[i], m.modules[i])
		copy(out.isFunc[i], m.isFunc[i])
	}
	return out
}

func (m *matrix) setFunction(x, y int, dark bool) {
	if x < 0 || y < 0 || x >= m.size || y >= m.size {
		return
	}
	m.modules[y][x] = dark
	m.isFunc[y][x] = true
}

// newBaseMatrix 铺好全部功能图案（定位、分隔、定时、对齐、暗模块、格式/版本信息位）。
func newBaseMatrix(version int) *matrix {
	size := version*4 + 17
	m := &matrix{
		size:    size,
		modules: make([][]bool, size),
		isFunc:  make([][]bool, size),
	}
	for i := 0; i < size; i++ {
		m.modules[i] = make([]bool, size)
		m.isFunc[i] = make([]bool, size)
	}

	// 定时图案：第 6 行与第 6 列，从定位图案之间交替。
	for i := 0; i < size; i++ {
		m.setFunction(6, i, i%2 == 0)
		m.setFunction(i, 6, i%2 == 0)
	}
	// 三个定位图案（含分隔符：drawFinderPattern 画的是 9×9，第 3 圈是空白分隔）。
	m.drawFinder(3, 3)
	m.drawFinder(size-4, 3)
	m.drawFinder(3, size-4)

	// 对齐图案：所有坐标组合，除了与三个定位图案重叠的那三个角。
	pos := alignPositions[version]
	for i, y := range pos {
		for j, x := range pos {
			corner := (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0)
			if corner {
				continue
			}
			m.drawAlignment(x, y)
		}
	}

	// 格式信息与版本信息的**位置**必须先占住（值先写 0，选好掩码后再重写）：
	// 数据填充靠 isFunc 跳过它们，漏一个就是整条数据流错位。
	drawFormatBits(m, LevelL, 0)
	m.drawVersion(version)
	return m
}

func (m *matrix) drawFinder(x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			dist := abs(dx)
			if abs(dy) > dist {
				dist = abs(dy)
			}
			m.setFunction(x+dx, y+dy, dist != 2 && dist != 4)
		}
	}
}

func (m *matrix) drawAlignment(x, y int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			dist := abs(dx)
			if abs(dy) > dist {
				dist = abs(dy)
			}
			m.setFunction(x+dx, y+dy, dist != 1)
		}
	}
}

// drawVersion 写版本信息（版本 7 以上才有）：6 位版本 + 12 位 BCH(18,6)。
func (m *matrix) drawVersion(version int) {
	if version < 7 {
		return
	}
	rem := version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := version<<12 | rem
	for i := 0; i < 18; i++ {
		dark := (bits>>uint(i))&1 != 0
		a := m.size - 11 + i%3
		b := i / 3
		m.setFunction(a, b, dark)
		m.setFunction(b, a, dark)
	}
}

// drawFormatBits 写格式信息：2 位纠错等级 + 3 位掩码 + 10 位 BCH(15,5)，再异或 0x5412。
//
// 它有两份拷贝（左上角一份、右上+左下各半份），这是标准要求的冗余：
// 扫到哪一份都能读出掩码。
func drawFormatBits(m *matrix, level Level, mask int) {
	data := level.formatBits()<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412

	bit := func(i int) bool { return (bits>>uint(i))&1 != 0 }

	for i := 0; i <= 5; i++ {
		m.setFunction(8, i, bit(i))
	}
	m.setFunction(8, 7, bit(6))
	m.setFunction(8, 8, bit(7))
	m.setFunction(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		m.setFunction(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		m.setFunction(m.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		m.setFunction(8, m.size-15+i, bit(i))
	}
	// 固定深色模块（第 8 行第 size-8 列，就在左下那份格式信息上方）。
	m.setFunction(8, m.size-8, true)
}

// drawData 按标准的"之字形"把码字铺进矩阵，然后套用掩码。
func drawData(m *matrix, isFunc [][]bool, codewords []byte, mask int) {
	bitIndex := 0
	total := len(codewords) * 8
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // 第 6 列是竖向定时图案，整列跳过
		}
		for vert := 0; vert < m.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = m.size - 1 - vert
				}
				if isFunc[y][x] {
					continue
				}
				dark := false
				if bitIndex < total {
					dark = (codewords[bitIndex>>3]>>uint(7-(bitIndex&7)))&1 != 0
					bitIndex++
				}
				// 剩余位（remainderBits）不写数据，但**照样参与掩码**：
				// 它们也是数据区的一部分，漏掉会让掩码与实际图案对不上。
				if maskApplies(mask, y, x) {
					dark = !dark
				}
				m.modules[y][x] = dark
			}
		}
	}
}

// maskApplies 是 8 种掩码条件（ISO/IEC 18004 表 10），i=行、j=列。
func maskApplies(mask, i, j int) bool {
	switch mask {
	case 0:
		return (i+j)%2 == 0
	case 1:
		return i%2 == 0
	case 2:
		return j%3 == 0
	case 3:
		return (i+j)%3 == 0
	case 4:
		return (i/2+j/3)%2 == 0
	case 5:
		return (i*j)%2+(i*j)%3 == 0
	case 6:
		return ((i*j)%2+(i*j)%3)%2 == 0
	default:
		return ((i+j)%2+(i*j)%3)%2 == 0
	}
}

// ---------------------------------------------------------------- 掩码评分

// penalty 是四条掩码惩罚规则的总分（分越低越好）。
func penalty(m *matrix) int {
	r1, r2, r3, r4 := penaltyParts(m)
	return r1 + r2 + r3 + r4
}

// penaltyParts 分规则返回四条惩罚分。
//
// 为什么要把四条规则拆开返回：掩码选错时的表现是"扫不出来"，而**总分对不上**
// 是排查它的唯一线索（四条规则里只有一条算错时，总分只差几十，肉眼看不出是哪条）。
// 与参考实现比对时（见 qr_test.go 的 TestDumpForReference）逐条比，
// 一次就能指出是哪条规则的问题。
func penaltyParts(m *matrix) (r1, r2, r3, r4 int) {
	size := m.size

	// 规则 1：同色连续 5 个以上 → 3 + (长度-5)。
	for i := 0; i < size; i++ {
		r1 += runPenalty(func(j int) bool { return m.modules[i][j] }, size)
		r1 += runPenalty(func(j int) bool { return m.modules[j][i] }, size)
	}

	// 规则 2：2×2 同色块 → 每块 3 分。
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			c := m.modules[y][x]
			if m.modules[y][x+1] == c && m.modules[y+1][x] == c && m.modules[y+1][x+1] == c {
				r2 += 3
			}
		}
	}

	// 规则 3：出现 1:1:3:1:1 的"类定位图案"（一侧带 4 个浅色）→ 每处 40 分。
	const (
		patA = 0b00001011101
		patB = 0b10111010000
	)
	for y := 0; y < size; y++ {
		bits := 0
		for x := 0; x < size; x++ {
			bits = ((bits << 1) & 0x7FF)
			if m.modules[y][x] {
				bits |= 1
			}
			if x >= 10 && (bits == patA || bits == patB) {
				r3 += 40
			}
		}
	}
	for x := 0; x < size; x++ {
		bits := 0
		for y := 0; y < size; y++ {
			bits = ((bits << 1) & 0x7FF)
			if m.modules[y][x] {
				bits |= 1
			}
			if y >= 10 && (bits == patA || bits == patB) {
				r3 += 40
			}
		}
	}

	// 规则 4：深色比例每偏离 50% 达 5% → 10 分。
	//
	// 这里**故意**用浮点而不是整数运算：标准写的是"深色模块的百分比"，
	// 整数近似会在边界上取到不同的档位。浮点结果与其它实现（同样按 IEEE754
	// double 算）逐位一致，交叉验证时不会因为这个产生假差异。
	dark := 0
	for _, row := range m.modules {
		for _, v := range row {
			if v {
				dark++
			}
		}
	}
	percent := float64(dark) / float64(size*size)
	r4 = int(math.Abs(percent*100-50)/5) * 10
	return r1, r2, r3, r4
}

// runPenalty 是规则 1 在一行/一列上的实现。get 按顺序取这一行（列）的第 i 个模块。
func runPenalty(get func(int) bool, size int) int {
	score := 0
	run := 0
	var prev bool
	for i := 0; i < size; i++ {
		cur := get(i)
		if i == 0 || cur == prev {
			run++
		} else {
			if run >= 5 {
				score += 3 + (run - 5)
			}
			run = 1
		}
		prev = cur
	}
	if run >= 5 {
		score += 3 + (run - 5)
	}
	return score
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
