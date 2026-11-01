//go:build linux

package main

import "syscall"

// diskUsage 返回指定路径所在文件系统的总量和已用量（MB）。
func diskUsage(path string) (totalMB, usedMB int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	total := uint64(st.Blocks) * bs
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * bs
	return int64(total / (1 << 20)), int64(used / (1 << 20))
}
