//go:build linux

package main

import "syscall"

// statfsBytes 返回挂载点所在文件系统的总字节数和已用字节数。
func statfsBytes(path string) (total, used uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	blocks := uint64(st.Blocks)
	free := uint64(st.Bfree)
	if free > blocks {
		free = blocks
	}
	return blocks * bs, (blocks - free) * bs, true
}
