//go:build windows

package api

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// diskSpace 在 Windows 上问的是卷的那层根目录：GetDiskFreeSpaceEx 要的是一个卷上的路径，
// 给它一个子目录虽然也能用，但把 "C:\Users\…" 换成 "C:\" 之后，读到的数字和这个盘在
// 资源管理器里显示的是同一份，运维不会两头对不上。
func diskSpace(path string) (total, free int64) {
	volume := filepath.VolumeName(filepath.Clean(path))
	target := path
	if volume != "" {
		target = strings.TrimSuffix(volume, `\`) + `\`
	}
	p, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return -1, -1
	}
	var freeBytes, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeBytes, &totalBytes, &totalFree); err != nil {
		return -1, -1
	}
	return int64(totalBytes), int64(freeBytes)
}
