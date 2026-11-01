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
	if _, jailed := jailFor(g.lim); !jailed && g.lim.RunAs != nil {
		// Dropping the uid also means the program cannot read anything the judge
		// owns, and RLIMIT-style per-user budgets stop competing with the judge.
		// Inside a jail the identity is dropped by the jailer instead, after the
		// mounts and chroot are done.
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: g.lim.RunAs.UID, Gid: g.lim.RunAs.GID, NoSetGroups: true,
		}
	}
	return applyJail(cmd, g.lim)
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
