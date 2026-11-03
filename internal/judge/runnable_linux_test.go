//go:build linux

package judge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/jail"
)

// requireRunnable 让"真的跑一个程序"的用例在 root 判机上处于可跑的状态。
//
// 判机以 root 跑、没沙盒、又没给 -run-as 时，现在按设计**拒绝跑提交**（不敢把 root 交给
// 学生代码，见 plainGuard.Configure）。这一批用例测的是判题链路——编译、逐用例、SPJ、
// 交互——不是身份策略本身（那条有自己的用例）。所以 root 机器上先把牢房打开，跟生产的
// 形态一致；开不了牢房的机器如实跳过，不会假绿。
//
// 非 root（开发机、容器里的专用账号、Windows）不触发那条拒绝，什么都不做。
func requireRunnable(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		return
	}
	enableJailForTests(t)
}

// enableJailForTests 是 jailOn 里没有工具链检查的那半截：requireToolchain 也会叫
// requireRunnable，两条互相绕一下就递归了。
func enableJailForTests(t *testing.T) {
	t.Helper()
	uid, gid := uint32(65534), uint32(65534)
	if os.Getuid() == int(uid) { // 判机本身就是 nobody：换一个没映射的号
		uid, gid = 65533, 65533
	}
	SetSandbox("/proc/self/exe", jail.On, os.Getuid() != 0, uid, gid)
	t.Cleanup(func() { SetSandbox("", jail.Off, false, 0, 0) })
	note, err := ProbeSandbox(filepath.Join(t.TempDir(), "probe"))
	if err != nil {
		t.Skipf("这台机器开不了沙盒：%v", err)
	}
	t.Logf("沙盒：%s", note)
}
