package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"probe/web"
)

// webETagCache 缓存每个静态资源的内容指纹（进程内只算一次）。
var webETagCache sync.Map // name -> etag

// registerWeb 挂载内嵌的前端静态资源。
//
// 前端没有构建步骤、没有 CDN、没有外链，因此用**协商缓存**：
// 每个文件带内容 ETag，浏览器下次请求带 If-None-Match，没变就只回 304。
//
// 为什么要自己做 ETag：go:embed 的文件没有修改时间，http.FileServerFS 只能发
// Last-Modified: 零值，浏览器无法协商，每次导航都会重新下载全部资源（约 77 KB）。
func (s *Server) registerWeb(mux *http.ServeMux) {
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
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
