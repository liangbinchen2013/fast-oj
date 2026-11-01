//go:build windows

package judge

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobGuard enforces the memory cap with a Windows job object and kills the
// whole tree through it. CPU time is not capped by the job (a job time limit
// terminates the process with an opaque exit status, which would be impossible
// to tell apart from a crash), so Run's wall-clock deadline is the time guard.
type jobGuard struct {
	job  windows.Handle
	proc windows.Handle
	lim  Limits
}

// maxTreeProcesses caps how many processes a submission's job may hold at once.
// Without it a program that forks in a loop stays inside the per-process memory
// cap while eating the box's process table and CPU.
const maxTreeProcesses = 4

// jobMemFactor turns the per-case memory cap into a cap for the whole tree: the
// per-process limit alone is multiplied by however many children fit.
const jobMemFactor = 4

func newGuard(lim Limits) (Guard, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	g := &jobGuard{job: job, lim: lim}

	procs := lim.MaxProcs
	if procs <= 0 {
		procs = maxTreeProcesses
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	info.BasicLimitInformation.ActiveProcessLimit = uint32(procs)
	if lim.MemKB > 0 {
		info.ProcessMemoryLimit = uintptr(lim.MemKB) * 1024
		info.JobMemoryLimit = uintptr(lim.MemKB) * 1024 * jobMemFactor
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY |
			windows.JOB_OBJECT_LIMIT_JOB_MEMORY
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return g, nil
}

// Configure has nothing to do before the process exists: a job object binds a
// running process, so everything happens in Attach.
func (g *jobGuard) Configure(cmd *exec.Cmd) error { return nil }

// memKB is not read from the wait status on Windows: the job object already
// reports the tree's peak commit, which is what the limit is enforced against.
func memKB(ru *syscall.Rusage) int { return -1 }

func (g *jobGuard) Attach(p *os.Process) error {
	// Go owns the handle it made for the child, so reopen it by pid with only the
	// rights a job needs. A short unbound window sits between Start and here;
	// kill-on-close and the wall-clock deadline bound it.
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	g.proc = handle
	return windows.AssignProcessToJobObject(g.job, handle)
}

// cpuMS reads the USER/Kernel FILETIMEs that Go puts in the Windows wait
// status. They are 100 ns ticks.
func cpuMS(ru *syscall.Rusage) int {
	ticks := func(ft syscall.Filetime) int64 {
		return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	}
	return int((ticks(ru.UserTime) + ticks(ru.KernelTime)) / 10000)
}

func (g *jobGuard) Kill() error {
	if g.job == 0 {
		return errors.New("job handle already closed")
	}
	return windows.TerminateJobObject(g.job, 1)
}

// Measure reports peak committed memory for the process tree from the job. CPU
// time comes from the wait status instead, because the job's accounting block is
// not what Windows fills in for a single-child job here.
func (g *jobGuard) Measure() (timeMS int, memKB int) {
	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := queryJob(g.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext))); err != nil {
		return -1, -1
	}
	return -1, int(uint64(ext.PeakProcessMemoryUsed) / 1024)
}

func queryJob(job windows.Handle, class int32, info uintptr, size uint32) error {
	if job == 0 {
		return os.ErrClosed
	}
	var returned uint32
	return windows.QueryInformationJobObject(job, class, info, size, &returned)
}

func (g *jobGuard) Close() error {
	if g.proc != 0 {
		windows.CloseHandle(g.proc)
		g.proc = 0
	}
	if g.job != 0 {
		err := windows.CloseHandle(g.job)
		g.job = 0
		return err
	}
	return nil
}

// signalled 在 Windows 上永远是 false：那里没有"被信号打死"这回事，击杀就是退出码。
// 要靠它判 MLE 的那条修正本来也只在读得出 ELF 镜像的平台上生效（见 elfimage.go）。
func signalled(state *os.ProcessState) bool { return false }
