// Package web 内嵌前端静态资源。
//
// 资源随二进制一起发布：没有 CDN、没有外链、没有构建步骤，离线也能用。
// 新增文件必须写进下面的 go:embed 列表（漏了会编译失败，这是有意的）。
package web

import "embed"

// FS 是前端资源文件系统，根目录即 web/。
//
//go:embed index.html style.css app.js chart.js
var FS embed.FS
