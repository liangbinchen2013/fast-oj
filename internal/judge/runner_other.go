//go:build !windows

package judge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// plainGuard is the Unix side of enforcement. It deliberately does not pretend to
// be a sandbox: what it guarantees is that the whole process tree dies on timeout
// (Setpgid + group kill) and that time and peak memory are measured for real.
//
// Hard caps belong to whoever started the judge. Go cannot hand a Linux child its
// rlimits at spawn (SysProcAttr.Rlimits only exists on the BSDs), and trying to
// wrap every run in a shell's ulimit would make the verdict depend on which shell
// the image happens to ship. Inside the container this is what actually enforces
// the limits:
//
//	--memory / --cpus / --pids-limit / --ulimit core=0 / --read-only + tmpfs
//
// With those in place a submission that allocates past its problem limit is
// OOM-killed by the cgroup and we still see the peak in ru_maxrss, so the verdict
// is MLE rather than an unexplained crash.
type plainGuard struct {
	lim  Limits
	proc *os.Process
}

func newGuard(lim Limits) (Guard, error) { return &plainGuard{lim: lim}, nil }

func (g *plainGuard) Configure(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // so Kill takes the whole tree, not just the leader
	}
	if _, jailed := jailFor(g.lim); !jailed {
		// 没有牢房，身份这一层就得由判机自己补。判机是 root 而没人告诉它要降到谁时，
		// 答案是**拒绝跑**，不是"就按 root 跑吧"：2026-11-01 那条伪造一行 stderr 就能
		// 关掉牢房的漏洞，撞出来的正是这个组合（ECS 上实测：关掉之后下一条提交打印
		// getuid() 得到 0）。
		if g.lim.RunAs == nil && os.Getuid() == 0 {
			return errRootWithoutSandbox
		}
		if drop := g.lim.RunAs; drop != nil {
			// Dropping the uid also means the program cannot read anything the judge
			// owns, and RLIMIT-style per-user budgets stop competing with the judge.
			// Inside a jail the identity is dropped by the jailer instead, after the
			// mounts and chroot are done.
			cmd.SysProcAttr.Credential = &syscall.Credential{
				Uid: drop.UID, Gid: drop.GID, NoSetGroups: true,
			}
		}
	}
	return applyJail(cmd, g.lim)
}

// errRootWithoutSandbox 是"root 判机 + 没沙盒 + 没给 -run-as"这一组合的拒绝。
//
// 为什么不改成"兜底降到 nobody"：那需要提交能走进判机给它准备的工作目录，而生产上那个
// 目录的祖先 (/var/lib/fast-oj 是 0750) 是**故意**不给别人进的——实测 fork/exec 直接
// EACCES，每条提交都成判机故障，还说不清为什么。两种已知的部署形态本来都不该落到这里：
// 容器判机显式写着 -run-as 65534:65534 且 /work 是 1777 的 tmpfs，root 起步的裸机判机
// 开的是沙盒。真要不开沙盒，就用一个普通账号跑判机（run.sh 就是这么做的）。
var errRootWithoutSandbox = errors.New(
	"判机以 root 跑、没有沙盒、也没有 -run-as：不敢把 root 交给提交的代码。" +
		"要么开沙盒（-sandbox=on 或 auto），要么用 -run-as uid:gid 指定一个在宿主上什么都不是的号")

func (g *plainGuard) Attach(p *os.Process) error {
	g.proc = p
	return nil
}

// Kill stops the process group. A program that forked before hanging would
// otherwise orphan and keep burning the box while the judge moved on.
func (g *plainGuard) Kill() error {
	if g.proc == nil {
		return nil
	}
	err := syscall.Kill(-g.proc.Pid, syscall.SIGKILL)
	if err == nil {
		return nil
	}
	// Root without CAP_KILL cannot signal a child it dropped to another uid, so the
	// group kill is the one that fails first; fall back to the leader itself.
	if err2 := g.proc.Kill(); err2 != nil {
		return errors.Join(err, err2)
	}
	return nil
}

// Measure reports the resident set of the whole process group, which is what the
// memory trip-wire polls while the run is alive. Time still comes from the wait
// status. It gives up with -1 where /proc does not exist (macOS) or the process is
// already gone, so the trip-wire simply never fires and the container's cgroup
// stays the only hard cap.
//
// 子进程必须算进来：主程序可以 fork 一批孩子，每个孩子都只吃自己的 RLIMIT_AS，而只读
// leader 的 statm 时这些内存完全看不见。Setpgid 让它们都留在同一个组里，setsid/setpgid
// 又已经被 seccomp 挡住，所以按 pgrp 汇总就是这次运行的内存。
func (g *plainGuard) Measure() (int, int) {
	if g.proc == nil {
		return -1, -1
	}
	if runtime.GOOS == "linux" {
		if memKB, ok := processGroupRSSKB(g.proc.Pid); ok {
			return -1, memKB
		}
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", g.proc.Pid))
	if err != nil {
		return -1, -1
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return -1, -1
	}
	pages, err := strconv.Atoi(fields[1])
	if err != nil || pages <= 0 {
		return -1, -1
	}
	return -1, pages * os.Getpagesize() / 1024
}

// processGroupRSSKB sums statm resident pages for every process whose pgrp is
// pgid. It reads /proc directly and returns false on any platform/container where
// that is unavailable, so callers can fall back to the leader's reading.
func processGroupRSSKB(pgid int) (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	pageKB := os.Getpagesize() / 1024
	total, found := 0, false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// comm 可能带空格和右括号；pgrp 在最后一个 ')' 之后：state ppid pgrp ...
		closeParen := bytes.LastIndexByte(stat, ')')
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(stat[closeParen+1:]))
		if len(fields) < 3 {
			continue
		}
		pgrp, err := strconv.Atoi(fields[2])
		if err != nil || pgrp != pgid {
			continue
		}
		statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
		if err != nil {
			continue
		}
		statmFields := strings.Fields(string(statm))
		if len(statmFields) < 2 {
			continue
		}
		pages, err := strconv.Atoi(statmFields[1])
		if err != nil || pages <= 0 {
			continue
		}
		total += pages * pageKB
		found = true
	}
	return total, found
}

func (g *plainGuard) Close() error { return nil }

// signalled 说这次运行是**被信号打死**的，而不是自己 exit 非零。Go 的
// `ProcessState.ExitCode()` 把所有信号死都记成 -1，具体哪个信号只能从 wait status 里读，
// 而"跑都没跑起来"这件事判定上要区分出来（见 elfimage.go），所以直接问内核。
func signalled(state *os.ProcessState) bool {
	if state == nil {
		return false
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// cpuMS converts the child rusage from the wait status; timevals are seconds
// plus microseconds.
func cpuMS(ru *syscall.Rusage) int {
	user := ru.Utime.Sec*1000 + ru.Utime.Usec/1000
	sys := ru.Stime.Sec*1000 + ru.Stime.Usec/1000
	return int(user + sys)
}

// memKB converts ru_maxrss, which the kernel reports in kilobytes on Linux and in
// bytes on the BSDs.
func memKB(ru *syscall.Rusage) int {
	if runtime.GOOS == "linux" {
		return int(ru.Maxrss)
	}
	return int(ru.Maxrss / 1024)
}
