//go:build !linux

package jail

import "errors"

// 非 Linux 没有对应的内核机制：Windows 上判机能做的只有 Job Object 那几项
// （CPU、内存、进程数），文件系统与网络不设边界。所以沙盒在这里不是"降级"，
// 而是根本没有——-sandbox=on 会直接拒绝启动，而不是假装开了。

func NamespaceFlags(bool) uintptr { return 0 }

func Binds() []string { return nil }

func Dev() []string { return nil }

func Enter(_ Spec, _ *Status) error {
	return errors.New("沙盒需要 Linux 命名空间，这个平台没有")
}
