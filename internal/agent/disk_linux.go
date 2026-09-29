//go:build linux

package agent

import "syscall"

// statFS 返回文件系统的总容量、已用、可用字节数，口径与 df 一致。
func statFS(path string) (total, used, avail uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	avail = st.Bavail * bsize
	used = total - st.Bfree*bsize
	return total, used, avail, nil
}
