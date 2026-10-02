package qr

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// 渲染参数的上限：它们最终会乘进图片边长，不能由调用方随便放大
// （一张 (177+8)*64 的图是 1.1 亿像素，足够把内存打满）。
const (
	maxScale = 32
	maxQuiet = 16
	// DefaultScale / DefaultQuiet 是面板实际用的档位：
	// 每个模块 8 像素 + 4 模块静区，手机扫屏与打印都够清晰，图约 2~4 KB。
	DefaultScale = 8
	DefaultQuiet = 4
)

// PNG 把二维码渲染成 PNG 字节。
//
// scale 是每个模块的像素边长，quiet 是四周的静区宽度（单位：模块）。
// 静区不是装饰：标准要求至少 4 个模块的空白，缺了它很多扫码器直接找不到定位图案。
func (c *Code) PNG(scale, quiet int) ([]byte, error) {
	if c == nil || c.Size <= 0 {
		return nil, errors.New("二维码为空")
	}
	if scale < 1 || scale > maxScale {
		return nil, fmt.Errorf("模块边长必须在 1~%d 像素之间", maxScale)
	}
	if quiet < 0 || quiet > maxQuiet {
		return nil, fmt.Errorf("静区必须在 0~%d 个模块之间", maxQuiet)
	}

	side := (c.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, side, side))
	// 底色一次性铺白，再只画深色模块：绝大多数模块是浅色的，
	// 逐格画两个颜色纯属浪费。
	white := color.Gray{Y: 0xFF}
	black := color.Gray{Y: 0x00}
	for i := range img.Pix {
		img.Pix[i] = white.Y
	}
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.Modules[y][x] {
				continue
			}
			x0 := (x + quiet) * scale
			y0 := (y + quiet) * scale
			for dy := 0; dy < scale; dy++ {
				// 这一行的起点必须加上 x0：写成 Pix[rowStart : rowStart+x0+scale]
				// 会把整行从左边一直涂到 x0+scale（切片长度是 x0+scale），
				// 画出来是一堆"从左边铺开的黑条" —— 而它**看起来仍像一张图**，
				// 只有数一遍黑白像素才发现（见 TestPNG 里的像素普查）。
				rowStart := (y0 + dy) * img.Stride
				row := img.Pix[rowStart+x0 : rowStart+x0+scale]
				for i := range row {
					row[i] = black.Y
				}
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("编码 PNG 失败: %w", err)
	}
	return buf.Bytes(), nil
}
