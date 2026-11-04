package judge

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/jail"
)

const (
	// MaxOutput is the per-stream cap. Past it whatever the program printed
	// already cannot match the expected answer, so it is scored as WA.
	MaxOutput = 1 << 20
	MaxStderr = 1 << 16
	// timeSlackMS is how much longer than the problem's limit a run is allowed to
	// keep going before the wall clock cuts it off. Without the slack a program at
	// 1001ms of a 1000ms limit gets killed at the boundary and reports 1000ms; with
	// it we get to measure and report the real number.
	timeSlackMS = 50
	// minWallClock covers process creation on a cold judge: a problem with a 10ms
	// limit cannot be judged by a 10ms wall clock.
	minWallClock = 250 * time.Millisecond
	// memTripKB is how far past a problem's memory cap a run may grow before the
	// judge stops it: close enough that a runaway allocation does not reach the
	// container's ceiling, loose enough that a legitimate peak is still measured.
	memTripKB = 16 * 1024
	// memPollInterval is how often the guard is asked for the current peak.
	memPollInterval = 20 * time.Millisecond
	// reapGrace is how long Run waits for a killed tree to actually go away before
	// it reports the verdict anyway. A run that cannot be reaped (a judge without
	// CAP_KILL signalling a dropped-uid child, or a grandchild holding the output
	// pipe) must not be able to occupy the queue.
	reapGrace = 5 * time.Second
)

// DropTo is the uid/gid a judged process should run as. Windows ignores it.
// A containerised Linux judge uses it so file permissions and RLIMIT_NPROC are
// scoped to a user that owns nothing on the box, instead of to the judge itself.
type DropTo struct{ UID, GID uint32 }

type Limits struct {
	TimeMS int
	MemKB  int
	// MaxProcs bounds the process tree. Zero means the judge's default for a
	// submission; compiling needs more headroom because the driver runs its back
	// ends (cc1plus, as, ld) as children of the same job. The Windows job object
	// enforces it; on Linux the container's --pids-limit is what actually does.
	MaxProcs int
	// RunAs drops the judged process to an unprivileged user when set.
	RunAs *DropTo
	// jailStatus 是这一把运行的状态信道（见 internal/jail 的 Status）：牢房有没有建成
	// 只从它上面读。由 Run 建、由 applyJail 填进 Spec、由 Run 在收尾时读，不进任何
	// 用户可见的结构，也不是调用方能填的东西。
	jailStatus *jail.Status
}

// Guard is the platform-specific resource enforcement around one process tree.
type Guard interface {
	// Configure prepares the command before it is spawned. rlimits and a private
	// process group can only be applied there, which is why this is separate from
	// Attach; Windows has nothing to do before the process exists.
	Configure(cmd *exec.Cmd) error
	// Attach binds a running process to the limits.
	Attach(p *os.Process) error
	// Kill stops the whole tree and reports why it could not, which is a real
	// possibility: a judge that dropped a submission to another uid cannot signal
	// it without CAP_KILL.
	Kill() error
	// Measure reports accumulated CPU time in ms and peak memory in KB, or -1
	// for either when the platform cannot tell.
	Measure() (timeMS int, memKB int)
	Close() error
}

type Execution struct {
	Verdict Verdict
	TimeMS  int
	MemKB   int
	Stdout  []byte
	Stderr  []byte
	Exit    int
	Note    string
	// TimeMin and MemMin say the number is a lower bound: the run was killed
	// before it finished, so it cost at least that much and we cannot say how much
	// more. The views print those with a leading ">"; a number that ran to
	// completion stays exact.
	TimeMin bool
	MemMin  bool
}

type limitedBuffer struct {
	buf  bytes.Buffer
	cap  int
	flew bool
}

func newCappedBuffer(capacity int) *limitedBuffer {
	return &limitedBuffer{cap: capacity}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.cap - b.buf.Len(); room >= len(p) {
		b.buf.Write(p)
	} else {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.flew = true
	}
	// Always report the full count so os/exec keeps draining the pipe instead of
	// blocking a chatty program forever.
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

func (b *limitedBuffer) Overflowed() bool { return b.flew }

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// killText turns a nil error into the only other explanation worth printing.
func killText(err error) string {
	if err == nil {
		return "the signal was sent but the output never closed"
	}
	return err.Error()
}

// verdictFor decides the outcome of one finished run. The order is the meaning:
// output that blew past the cap can no longer match the answer, a blown memory cap
// explains a crash better than its exit code does, and a run that used more CPU
// than the problem allows is TLE even when it exited cleanly on its own.
func verdictFor(lim Limits, timeMS, memKB, exit int, overflowed bool) (Verdict, string) {
	switch {
	case overflowed:
		return WA, "output over " + strconv.Itoa(MaxOutput) + " bytes"
	case lim.MemKB > 0 && memKB > lim.MemKB:
		return MLE, ""
	case lim.TimeMS > 0 && timeMS > lim.TimeMS:
		return TLE, "cpu time over the " + strconv.Itoa(lim.TimeMS) + "ms limit"
	case exit != 0:
		return RE, ""
	}
	return AC, ""
}

// allowedEnvKeys is everything a judged program may inherit. SystemRoot/WINDIR
// are what the loader needs to find the system DLLs; PATH is what reaches the
// compiler. Nothing else survives, because none of it is the program's business.
var allowedEnvKeys = []string{"PATH", "SystemRoot", "WINDIR"}

// runEnv is the entire environment a judged program is allowed to see. Nothing
// is inherited on purpose: the judge process holds operator secrets (its token,
// the database path, whatever the service unit exports), and a submission that
// could call getenv would read them out of the very same process it was spawned
// from. Only what a native toolchain needs to start survives, and temp/home are
// aimed at the throwaway work directory.
func runEnv(dir string) []string {
	env := []string{
		"TEMP=" + dir, "TMP=" + dir, "TMPDIR=" + dir,
		"HOME=" + dir, "USERPROFILE=" + dir,
		"LANG=C", "LC_ALL=C",
	}
	for _, key := range allowedEnvKeys {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// Run executes argv in dir with stdin under lim. The verdict describes this one
// execution only; comparing output is the caller's job.
func Run(argv []string, dir string, stdin []byte, lim Limits) Execution {
	if len(argv) == 0 {
		return Execution{Verdict: IE, Note: "empty command line"}
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return Execution{Verdict: IE, Note: "missing program: " + argv[0]}
	}
	if lim.TimeMS <= 0 {
		lim.TimeMS = 1000
	}
	// 一次文件头解析：用来把"数组开太大、还没跑就崩"的那次崩溃解释对（见 elfimage.go）。
	image := imageMemKB(bin)
	// 牢房有没有建成，只从一条选手碰不到的信道读（见 internal/jail 的 Status 为什么存在）。
	// 必须在这里、newGuard 之前放好：guard 拿到的是一份 Limits 的拷贝。
	// 建不起这条信道就不跑——没有它，一次真的牢房失败会被当成程序自己的崩溃判掉。
	st, err := newJailStatus(lim)
	if err != nil {
		return Execution{Verdict: IE, Note: "牢房的状态信道建不起来，不敢裸跑: " + err.Error()}
	}
	lim.jailStatus = st
	defer st.Remove()

	cmd := exec.Command(bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = runEnv(dir)
	stdout, stderr := newCappedBuffer(MaxOutput), newCappedBuffer(MaxStderr)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = bytes.NewReader(stdin)
	// A grandchild that inherited the output pipe must not keep Wait busy after the
	// run itself is over.
	cmd.WaitDelay = reapGrace

	g, err := newGuard(lim)
	if err != nil {
		return Execution{Verdict: IE, Note: "sandbox init failed: " + err.Error()}
	}
	// 正常结束也要收整个进程组：主程序 exit 之后，fork 出来的子进程不会自己消失。
	// defer 的顺序是 LIFO：先注册 Close，再注册 Kill，收尾时先 Kill 再 Close。
	defer g.Close()
	defer func() { _ = g.Kill() }()

	if err := g.Configure(cmd); err != nil {
		return Execution{Verdict: IE, Note: "sandbox config failed: " + err.Error()}
	}
	if err := cmd.Start(); err != nil {
		// Go 把子进程整条 fork→unshare→exec 的失败都报成 "fork/exec <路径>: <errno>"，
		// 所以这条 "permission denied" 未必是执行被拒，更可能是 unshare 被拒（内核关了
		// 非特权命名空间、或 unit 的 SystemCallFilter 挡住了 unshare/mount）。不说清这一层，
		// 运维就会去查文件权限而查不到东西。
		note := "start failed: " + err.Error()
		if unshareRequested(cmd) {
			note = "沙盒未能启动（unshare 命名空间或 exec 被拒）: " + err.Error()
		}
		return Execution{Verdict: IE, Note: note}
	}
	if err := g.Attach(cmd.Process); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return Execution{Verdict: IE, Note: "attach failed: " + err.Error()}
	}

	done := make(chan waitResult, 1)
	go func() {
		err := cmd.Wait()
		done <- waitResult{err: err, state: cmd.ProcessState}
	}()

	// The memory trip-wire stops a runaway allocation near its cap instead of
	// letting it reach the container's ceiling and be OOM-killed on the way.
	trip := make(chan int, 1)
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	// The watcher reads the guard's own accounting, so it has to be gone before the
	// guard is closed: on Windows the job handle number gets reused by the next
	// case, and a watcher still reading it would kill an unrelated run.
	defer func() { close(stopWatch); <-watchDone }()
	if lim.MemKB > 0 {
		go func() {
			defer close(watchDone)
			ceiling := lim.MemKB + memTripKB
			tick := time.NewTicker(memPollInterval)
			defer tick.Stop()
			for {
				select {
				case <-stopWatch:
					return
				case <-tick.C:
					if _, mem := g.Measure(); mem > ceiling {
						_ = g.Kill()
						trip <- mem
						return
					}
				}
			}
		}()
	} else {
		close(watchDone)
	}

	select {
	case w := <-done:
		_, jobMemKB := g.Measure()
		timeMS := usageMS(w.state)
		if timeMS < 0 {
			timeMS, _ = g.Measure()
		}
		memKB := jobMemKB
		if memKB < 0 {
			// Platforms without a job object report peak RSS out of the wait
			// status instead.
			memKB = usageMemKB(w.state)
		}
		ex := Execution{
			TimeMS: timeMS, MemKB: memKB, Exit: exitCode(w.err),
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		}
		// 牢房没建成不是学生的错：判成 RE 会让他去查自己根本没跑起来的代码。判据只有一条
		// ——jailer 在这一把的状态信道上留下的那句话（暗号对得上才算，见 jailBrokenFrom）。
		if reason, broken := jailBrokenFrom(lim.jailStatus, ex.Exit); broken {
			note := jailBrokenNote(reason)
			reportJailBroken(note)
			ex.Verdict, ex.Note = IE, note
			return ex
		}
		v, note := verdictFor(lim, timeMS, memKB, ex.Exit, stdout.Overflowed())
		ex.Verdict, ex.Note = v, note
		if ex.Verdict == RE && w.err != nil {
			ex.Note = w.err.Error()
		}
		// 镜像本身就超上限的那种崩溃，报 RE + 一个很小的内存数是误导：改判 MLE 并说清原因。
		markImageOOM(&ex, lim, image, signalled(w.state))
		return ex

	case mem := <-trip:
		w, reaped := reap(done)
		_, peak := g.Measure()
		if peak < mem {
			peak = mem
		}
		ex := Execution{
			Verdict: MLE, MemKB: peak, MemMin: true, TimeMin: true,
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
			Note: "memory over " + strconv.Itoa(lim.MemKB) + "KB by more than " + strconv.Itoa(memTripKB) + "KB",
		}
		if reaped {
			ex.Exit = exitCode(w.err)
			if t := usageMS(w.state); t > 0 {
				ex.TimeMS = t
			}
		}
		return ex

	case <-time.After(wallClockFor(lim.TimeMS)):
		kerr := g.Kill()
		w, reaped := reap(done)
		if !reaped {
			// The tree is still alive, so the judge must not sit on it forever:
			// one unkillable run would take the queue with it. Report what the
			// verdict was and why the cleanup did not finish.
			_, memKB := g.Measure()
			return Execution{
				Verdict: TLE, TimeMS: lim.TimeMS, TimeMin: true, MemKB: memKB,
				Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
				Note: "wall clock limit exceeded and the run could not be reaped: " + killText(kerr),
			}
		}
		_, memKB := g.Measure()
		if memKB < 0 {
			memKB = usageMemKB(w.state)
		}
		// Show what the run actually cost, floored at the limit: a CPU-bound program
		// killed at 1050ms should not be reported as "1000ms", while a program that
		// slept instead of computing has no meaningful CPU number to show. Either
		// way the run was cut short, so the number is a lower bound.
		measured := usageMS(w.state)
		if measured < 0 {
			_, measured = g.Measure()
		}
		if measured < lim.TimeMS {
			measured = lim.TimeMS
		}
		return Execution{
			Verdict: TLE, TimeMS: measured, TimeMin: true, MemKB: memKB,
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
			Note: "wall clock limit " + strconv.Itoa(lim.TimeMS+timeSlackMS) + "ms exceeded",
		}
	}
}

// waitResult carries both halves of cmd.Wait: the error and the exit status the
// platform put in it.
type waitResult struct {
	err   error
	state *os.ProcessState
}

// usageMS reads per-process CPU time out of the wait status; -1 when the
// platform does not put it there.
func usageMS(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	ru, ok := state.SysUsage().(*syscall.Rusage)
	if !ok {
		return -1
	}
	return cpuMS(ru)
}

// usageMemKB is the platform's peak-memory reading from the same wait status.
func usageMemKB(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	ru, ok := state.SysUsage().(*syscall.Rusage)
	if !ok {
		return -1
	}
	return memKB(ru)
}

// reap waits for the killed run to be collected, giving up after reapGrace so an
// unkillable process cannot occupy the judge forever.
func reap(done chan waitResult) (waitResult, bool) {
	select {
	case w := <-done:
		return w, true
	case <-time.After(reapGrace):
		return waitResult{}, false
	}
}

// wallClockFor turns a CPU limit into a hard deadline. Without one, a program
// that sleeps instead of computing would pin a judge slot forever.
func wallClockFor(timeMS int) time.Duration {
	d := time.Duration(timeMS+timeSlackMS) * time.Millisecond
	if d < minWallClock {
		d = minWallClock
	}
	return d
}
