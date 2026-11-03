//go:build linux

package judge

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/liangbinchen2013/fast-oj/internal/jail"
)

// applyJail 把"跑这个程序"换成"跑判机的 jailer，由它建好牢房再 exec 这个程序"。
// 必须在 cmd.Start 之前调用：命名空间是 fork 那一刻开的，命令行是 exec 之前定的。
func applyJail(cmd *exec.Cmd, lim Limits) error {
	spec, ok := jailFor(lim)
	if !ok {
		return nil
	}
	spec.Dir = cmd.Dir
	spec.Argv = append([]string(nil), cmd.Args...)
	spec.Env = cmd.Env
	// 状态信道随 spec 一起交给 jailer：这两样东西只出现在 jailer 自己的命令行里，
	// 而它在 exec 时把命令行换成了提交程序的——选手进程看不到路径，也看不到暗号。
	spec.Status = lim.jailStatus.Path()
	spec.Nonce = lim.jailStatus.Nonce()
	wrapped, err := jail.Wrap(sandbox.self, spec)
	if err != nil {
		return err
	}
	cmd.Path = wrapped[0]
	cmd.Args = wrapped
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Unshareflags = jail.NamespaceFlags(spec.UserNS)
	// 身份交给 jailer 自己在牢房里降：判机不是 root 时，它得先是 ns 里的 root 才挂得动
	// mount 与 chroot。Credential 留着会让 Go 在 exec 前就 setuid，那一步之后牢房建不起来。
	cmd.SysProcAttr.Credential = nil
	if spec.UserNS {
		// 1:1 映射：牢房里的 root 是宿主上的判机用户，而提交程序随后降到一个未映射的号，
		// 宿主看到的是 nobody——判机的库、别人的源码、判机自己的环境就都读不到了。
		own := os.Getuid()
		grp := os.Getgid()
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: own, HostID: own, Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: grp, HostID: grp, Size: 1}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	return nil
}

// unshareRequested 报告这次运行是不是要自己开命名空间。Unshareflags 只有 Linux 有，
// 所以判据得留在这个文件里，通用路径只能问这个函数。
func unshareRequested(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.Unshareflags != 0
}
