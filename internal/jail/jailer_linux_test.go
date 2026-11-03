//go:build linux

package jail

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// 测试二进制也要能当 jailer：牢房是"exec 自己"实现的，这里那个"自己"就是跑起来的测试进程。
func TestMain(m *testing.M) {
	Main()
	os.Exit(m.Run())
}

// 真 jailer 在真失败时必须把话说在信道上。这里挑的失败点在任何 mount 之前（工作目录不存在），
// 所以非 root 也测得成，不会因为这台机器开不了命名空间就悄悄跳过最该验的那一步。
func TestJailerReportsARealFailure(t *testing.T) {
	st, err := NewStatus()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Remove()

	spec := Spec{
		Dir:    filepath.Join(t.TempDir(), "并不存在"),
		Argv:   []string{"/bin/true"},
		Status: st.Path(),
		Nonce:  st.Nonce(),
	}
	blob, err := spec.encode()
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, blob)
	// 参数位在 os.Args[1] 上是 jailer 的约定，这里照抄判机那侧的包法。
	cmd.Args = []string{self, blob}
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != FailExit {
		t.Fatalf("jailer 没按 FailExit=%d 退出：%v %q", FailExit, err, out)
	}
	if _, broken := st.Failure(); !broken {
		t.Fatalf("信道上没有失败声明，stderr 里却是：%q", out)
	}
	if st.Execed() {
		t.Error("连工作目录都没有，却说自己 exec 过了")
	}
	reason, _ := st.Failure()
	if !strings.Contains(reason, "工作目录") {
		t.Errorf("失败声明没说出原因：%q", reason)
	}
}

// 同一把运行里，成功交出去时留下的必须是 exec 记号而不是失败声明。这里用一条真的牢房
// （能开就开，开不了就跳过）跑 /bin/true：判机据此区分"程序自己 exit 70"与"jailer 没跑成"。
func TestJailerMarksASuccessfulHandoff(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	st, err := NewStatus()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Remove()

	spec := Spec{
		Dir: dir, Argv: []string{"/bin/true"}, Env: []string{"PATH=/usr/bin:/bin"},
		TimeMS: 5000, MemKB: 64 << 10, Procs: 64,
		UserNS: os.Getuid() != 0, UID: 65534, GID: 65534,
		Status: st.Path(), Nonce: st.Nonce(),
	}
	blob, err := spec.encode()
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Args = []string{self, blob}
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: NamespaceFlags(spec.UserNS)}
	if spec.UserNS {
		own := os.Getuid()
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: own, HostID: own, Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: own, HostID: own, Size: 1}}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("这台机器开不了牢房：%v %q", err, out)
	}
	if _, broken := st.Failure(); broken {
		t.Errorf("跑成了却报了牢房失败：%q", out)
	}
	if !st.Execed() {
		t.Errorf("交出去的那一步没留记号，判机会把这次当成 jailer 死了：%q", out)
	}
}
