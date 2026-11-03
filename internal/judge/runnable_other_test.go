//go:build !linux

package judge

import "testing"

// requireRunnable 在非 Linux 上没有可开的东西：那里没有沙盒，判机也不会以 root 跑提交，
// 那条拒绝（root + 没沙盒 + 没 -run-as）根本触发不了。
func requireRunnable(t *testing.T) {
	t.Helper()
}
