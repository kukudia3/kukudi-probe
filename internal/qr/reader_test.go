package qr

import (
	"bytes"
	"image/png"
	"testing"
)

// 这个文件是**读**的那一侧：照 ISO/IEC 18004 重写一遍，专门用来验证编码器。
//
// 为什么值得再写一份：如果"读"复用编码器的函数（功能模块图、掩码、取位顺序），
// 那么同一个理解错误会在两边同时出现，往返断言照样全绿 —— 而二维码扫不出来。
// 所以这里的四个关键环节都是独立实现：
//
//	functionMap   功能模块图（从标准正文重写，不复用 newBaseMatrix）
//	maskAt        8 种掩码条件（用另一种写法表达同一组公式）
//	readBits      之字形取位 + 解掩码
//	deinterleave  去交错（按"短块在前"逐轮取字节）
//
// 另外它还做一件编码器做不到的事：**逐块算 RS 校验子**。校验子全零等价于
// "这些纠错码字真的是 RS 编码的结果"，比"我算的和我算的一样"强得多。

// functionMap 独立算出的功能模块图。
func functionMap(version int) [][]bool {
	size := version*4 + 17
	f := make([][]bool, size)
	for i := range f {
		f[i] = make([]bool, size)
	}
	mark := func(x, y int) {
		if x >= 0 && y >= 0 && x < size && y < size {
			f[y][x] = true
		}
	}

	// 定位图案 + 分隔符：三个角各 8×8。
	for dy := 0; dy < 8; dy++ {
		for dx := 0; dx < 8; dx++ {
			mark(dx, dy)
			mark(size-1-dx, dy)
			mark(dx, size-1-dy)
		}
	}
	// 定时图案：整行 6 与整列 6（重叠部分重复标记无妨）。
	for i := 0; i < size; i++ {
		mark(i, 6)
		mark(6, i)
	}
	// 对齐图案：5×5，三个角上的与定位图案重叠，跳过。
	pos := alignPositions[version]
	for i, cy := range pos {
		for j, cx := range pos {
			corner := (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0)
			if corner {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					mark(cx+dx, cy+dy)
				}
			}
		}
	}
	// 格式信息：左上角两条边 + 右上 8 格 + 左下 8 格（含固定深色模块）。
	for i := 0; i <= 8; i++ {
		mark(8, i)
		mark(i, 8)
	}
	for i := 0; i < 8; i++ {
		mark(size-1-i, 8)
		mark(8, size-1-i)
	}
	// 版本信息（版本 7 起）：两块 3×6。
	if version >= 7 {
		for i := 0; i < 18; i++ {
			a := size - 11 + i%3
			b := i / 3
			mark(a, b)
			mark(b, a)
		}
	}
	return f
}

// maskAt 是 8 种掩码条件（另一种写法，避免与编码器共用同一个实现）。
func maskAt(mask, i, j int) bool {
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
	case 7:
		return ((i*j)%3+(i+j)%2)%2 == 0
	default:
		panic("掩码编号越界")
	}
}

// readBits 按之字形读出数据区的比特（已解掩码），顺序与标准一致。
func readBits(c *Code) []bool {
	f := functionMap(c.Version)
	bits := make([]bool, 0, c.Size*c.Size)
	for right := c.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < c.Size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = c.Size - 1 - vert
				}
				if f[y][x] {
					continue
				}
				dark := c.Modules[y][x]
				if maskAt(c.Mask, y, x) {
					dark = !dark
				}
				bits = append(bits, dark)
			}
		}
	}
	return bits
}

// packCodewords 把比特流按 8 位一字节打包（高位在前）。
func packCodewords(bits []bool) []byte {
	out := make([]byte, 0, len(bits)/8)
	for i := 0; i+8 <= len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			if bits[i+j] {
				b |= 1 << uint(7-j)
			}
		}
		out = append(out, b)
	}
	return out
}

// deinterleave 把码字序列拆回各块的数据部分与纠错部分。
func deinterleave(t *testing.T, cw []byte, spec ecSpec) (data, eccs [][]byte) {
	t.Helper()
	n := len(spec.dataPerBlock)
	data = make([][]byte, n)
	eccs = make([][]byte, n)
	maxData := 0
	for _, k := range spec.dataPerBlock {
		if k > maxData {
			maxData = k
		}
	}
	at := 0
	take := func() byte {
		if at >= len(cw) {
			t.Fatalf("码字不够：读到第 %d 个就越界（共 %d 个）", at, len(cw))
		}
		b := cw[at]
		at++
		return b
	}
	for round := 0; round < maxData; round++ {
		for b := 0; b < n; b++ {
			if round < spec.dataPerBlock[b] {
				data[b] = append(data[b], take())
			}
		}
	}
	for round := 0; round < spec.ecPerBlock; round++ {
		for b := 0; b < n; b++ {
			eccs[b] = append(eccs[b], take())
		}
	}
	if at != len(cw) {
		t.Fatalf("码字有剩余：只读了 %d 个，共 %d 个", at, len(cw))
	}
	return data, eccs
}

// assertSyndromesZero 断言每一块的 (数据+纠错) 多项式在 α^0..α^(ec-1) 处取值全为 0。
//
// 这就是 RS 码的定义：校验子全零 ⇔ 这段码字是一个合法码字。
func assertSyndromesZero(t *testing.T, blocks [][]byte, ecLen int) {
	t.Helper()
	for bi, block := range blocks {
		for i := 0; i < ecLen; i++ {
			var acc byte
			for _, b := range block {
				acc = gfMul(acc, gfExp[i]) ^ b
			}
			if acc != 0 {
				t.Fatalf("第 %d 块的 RS 校验子 S%d = %d，期望 0", bi, i, acc)
			}
		}
	}
}

// roundTrip 把矩阵读回原始文本。
func roundTrip(t *testing.T, c *Code) string {
	t.Helper()
	spec := ecSpecs[c.Level][c.Version]
	cw := packCodewords(readBits(c))
	wantTotal := totalCodewords[c.Version]
	if len(cw) < wantTotal {
		t.Fatalf("读出的码字数 = %d，少于标准的 %d（矩阵没铺满？）", len(cw), wantTotal)
	}
	cw = cw[:wantTotal]
	data, eccs := deinterleave(t, cw, spec)

	blocks := make([][]byte, len(data))
	for i := range data {
		if len(data[i]) != spec.dataPerBlock[i] {
			t.Fatalf("第 %d 块的数据码字数 = %d，期望 %d", i, len(data[i]), spec.dataPerBlock[i])
		}
		if len(eccs[i]) != spec.ecPerBlock {
			t.Fatalf("第 %d 块的纠错码字数 = %d，期望 %d", i, len(eccs[i]), spec.ecPerBlock)
		}
		blocks[i] = append(append([]byte{}, data[i]...), eccs[i]...)
	}
	assertSyndromesZero(t, blocks, spec.ecPerBlock)

	// 数据流：模式 4 位 + 计数 + 字节。
	stream := make([]byte, 0, len(data)*64)
	for _, blk := range data {
		stream = append(stream, blk...)
	}
	bitAt := 0
	read := func(n int) int {
		v := 0
		for i := 0; i < n; i++ {
			byteIdx := bitAt >> 3
			if byteIdx >= len(stream) {
				t.Fatalf("数据流不足：想读第 %d 位", bitAt)
			}
			bit := (stream[byteIdx] >> uint(7-(bitAt&7))) & 1
			v = v<<1 | int(bit)
			bitAt++
		}
		return v
	}
	if mode := read(4); mode != 0b0100 {
		t.Fatalf("模式指示符 = %04b，期望 0100（字节模式）", mode)
	}
	countBits := 8
	if c.Version >= 10 {
		countBits = 16
	}
	count := read(countBits)
	out := make([]byte, count)
	for i := 0; i < count; i++ {
		out[i] = byte(read(8))
	}
	return string(out)
}

func pngSide(t *testing.T, data []byte) int {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码 PNG: %v", err)
	}
	b := img.Bounds()
	return b.Dx()
}

// pngSymbolDark 数出"符号区"（不含静区）里的深色像素，并算出它**应该**是多少：
// 深色模块数 × scale²。
//
// 这是 PNG 渲染的**唯一**有效断言：尺寸、静区、PNG 合法性都能在图案完全错位时
// 依然成立（本仓库真的这么错过一次：每一行都从左边涂到某个位置，图看着像那么
// 回事，扫不出来）。quietDark 顺带报告静区里有没有被涂黑的像素 —— 静区必须是
// 纯白，否则一些扫码器找不到定位图案。
func pngSymbolDark(t *testing.T, data []byte, code *Code, scale, quiet int) (drawn, want, quietDark int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码 PNG: %v", err)
	}
	b := img.Bounds()
	darkAt := func(x, y int) bool {
		r, _, _, _ := img.At(x, y).RGBA()
		return r < 0x8000
	}
	x0 := quiet * scale
	x1 := (quiet + code.Size) * scale
	for y := x0; y < x1; y++ {
		for x := x0; x < x1; x++ {
			if darkAt(x, y) {
				drawn++
			}
		}
	}
	darkCells := 0
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Modules[y][x] {
				darkCells++
			}
		}
	}
	want = darkCells * scale * scale
	// 静区（符号区之外的全部像素）：一个深色像素都不许有。
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if x >= x0 && x < x1 && y >= x0 && y < x1 {
				continue
			}
			if darkAt(x, y) {
				quietDark++
			}
		}
	}
	return drawn, want, quietDark
}
