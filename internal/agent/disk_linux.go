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
	// Bfree > Blocks 是文件系统（NFS/CIFS 等网络文件系统可能返回任意值）会给出的坏值：
	// 直接做 total - Bfree*bsize 会在 uint64 下溢成 1.8e19，validate.go 的 "已用大于总量"
	// 于是把每一拍都拒掉 —— 整台探针永久静默（_audit/ROUND5-RAWCOUNT.md §5 的 T4）。
	// 下溢时按"没有占用"（0）处理：另一个候选是取 total（显示成 100% 满），
	// 那会凭空造出"磁盘写满"的显示甚至触发告警，比 0 危险，所以不取它。
	used = 0
	if free := st.Bfree * bsize; free <= total {
		used = total - free
	}
	return total, used, avail, nil
}
