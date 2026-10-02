package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"probe/web"
)

// webETagCache 缓存每个静态资源的内容指纹（进程内只算一次）。
var webETagCache sync.Map // name -> etag

// handleWeb 提供内嵌的前端静态资源。
//
// 前端没有构建步骤、没有 CDN、没有外链，因此用**协商缓存**：
// 每个文件带内容 ETag，浏览器下次请求带 If-None-Match，没变就只回 304。
//
// 为什么要自己做 ETag：go:embed 的文件没有修改时间，http.FileServerFS 只能发
// Last-Modified: 零值，浏览器无法协商，每次导航都会重新下载全部资源（约 77 KB）。
//
// 路由表里它挂在 "GET /"（"其它都没匹配上"的那一档），级别是 accessOpen：
// 前端资源本来就不需要会话（登录页的 HTML/JS/CSS 也得先发下去），
// 而它们里面不含任何节点数据。
func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")

	name := assetName(r.URL.Path)
	data, err := web.FS.ReadFile(name)
	if err != nil {
		// 目录或缺失文件：交给 FileServer 处理（它会回 404 或 index.html）。
		http.FileServerFS(web.FS).ServeHTTP(w, r)
		return
	}
	w.Header().Set("Etag", etagFor(name, data))
	// ServeContent 会用上面已经设置好的 Etag 处理 If-None-Match（回 304），
	// 顺带处理 Range 与 Content-Type 推断。
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// robotsText 是 /robots.txt 的内容。
//
// 面板是私人物业，不该被搜索引擎收录：一旦收录，任何人搜一下就能看见节点名称、
// 用量与价格（访客模式打开时甚至是整个只读面板）。index.html 里的
// <meta name="robots"> 只对"愿意执行到解析 HTML 那一步"的爬虫有效，而
// robots.txt 是爬虫在抓取**之前**就会读的那一份；再加上每个响应上的
// X-Robots-Tag（见 middleware.go），三道一起才算把这件事说清楚。
const robotsText = "User-agent: *\nDisallow: /\n"

// handleRobots 提供 robots.txt（会话无关：爬虫不会带 Cookie）。
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, robotsText)
}

// assetName 把 URL 路径映射成 go:embed 里的文件名（"/" → "index.html"）。
func assetName(urlPath string) string {
	cleaned := path.Clean("/" + urlPath)
	name := strings.TrimPrefix(cleaned, "/")
	if name == "" || name == "." {
		return "index.html"
	}
	return name
}

func etagFor(name string, data []byte) string {
	if cached, ok := webETagCache.Load(name); ok {
		return cached.(string)
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	webETagCache.Store(name, etag)
	return etag
}
