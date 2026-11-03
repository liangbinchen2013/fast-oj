//go:build !windows

package judge

import (
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
		// 没有牢房，身份这一层就得由判机自己补。以前只有"调用方给了 -run-as"才降，于是
		// 判机以 root 跑又没写 -run-as 的那台机器上，牢房一旦关掉（无论是内核开不了，还是
		// 被人用一行假输出关掉），学生的代码就直接在宿主上以 root 跑。
		if drop := g.unjailedIdentity(); drop != nil {
			cmd.SysProcAttr.Credential = &syscall.Credential{
				Uid: drop.UID, Gid: drop.GID, NoSetGroups: true,
			}
		}
	}
	return applyJail(cmd, g.lim)
}

// nobodyUID 是"在宿主上什么都不是"的那个号：内核兜底与容器都按它算。
const nobodyUID = 65534

// unjailedIdentity 回答"这一把没有牢房，那它以谁的身份跑"。
//
// 调用方指定了身份就用它；没指定时，判机不是 root 就照旧以自己的身份跑（容器里、或专用
// 账号下跑的判机本来就没多少可降的），判机**是** root 则兜底压到 nobody——把 root 交给
// 学生代码永远不是可接受的默认值，哪怕"牢房应该是开着的"。
func (g *plainGuard) unjailedIdentity() *DropTo {
	if g.lim.RunAs != nil {
		return g.lim.RunAs
	}
	if os.Getuid() != 0 {
		// 身份已经由启动判机的那个人定好了；判机也降不动别人的账号。
		return nil
	}
	return &DropTo{UID: nobodyUID, GID: nobodyUID}
}

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

// Measure reports the leader's current resident set from /proc, which is what the
// memory trip-wire polls while the run is alive. Time still comes from the wait
// status. It gives up with -1 where /proc does not exist (macOS) or the process is
// already gone, so the trip-wire simply never fires and the container's cgroup
// stays the only hard cap. Child processes are not summed: a submission that forks
// is bounded by --pids-limit anyway.
func (g *plainGuard) Measure() (int, int) {
	if g.proc == nil {
		return -1, -1
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
