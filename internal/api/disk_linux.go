//go:build linux

package api

import "syscall"

// diskSpace 问的是"这块盘还能写多少"，所以剩余用 Bavail 而不是 Bfree：
// Bfree 里包含只有 root 拿得到的保留块，写满到那一段时普通用户已经写不进去了，
// 而页面要在那之前先让人看见。
func diskSpace(path string) (total, free int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1, -1
	}
	bsize := int64(st.Bsize)
	return int64(st.Blocks) * bsize, int64(st.Bavail) * bsize
}
