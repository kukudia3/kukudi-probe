// Package version 保存构建信息。
//
// 这三个变量由 Makefile 通过 -ldflags -X 覆盖；直接用 go build 时保留默认值。
package version

var (
	// Version 是语义化版本号。
	Version = "0.1.0-dev"
	// Commit 是构建时的提交号。
	Commit = "unknown"
	// BuildTime 是构建时间（UTC）。
	BuildTime = "unknown"
)
