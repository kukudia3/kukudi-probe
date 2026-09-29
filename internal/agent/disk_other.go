//go:build !linux

package agent

import "errors"

// statFS 在非 Linux 平台不可用：Agent 正式运行只支持 Linux，
// 其它平台仅能通过 --print-json 检查解析逻辑（磁盘部分会被跳过并给出提示）。
func statFS(string) (total, used, avail uint64, err error) {
	return 0, 0, 0, errors.New("当前平台不支持 statfs")
}
