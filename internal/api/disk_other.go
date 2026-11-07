//go:build !linux && !windows

package api

// diskSpace 在没有对应系统调用的平台上如实报"未知"（-1）。这一页讲的是磁盘，
// 编一个 0 出来会被读成"盘满了"，那比什么都不知道更糟。
func diskSpace(string) (total, free int64) { return -1, -1 }
