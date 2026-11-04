//go:build linux

package jail

import (
	"fmt"
	"syscall"
	"unsafe"
)

// 这一层是 seccomp-bpf 的**黑名单**：把"从这次运行里出去"的那批系统调用直接变成 EPERM。
//
// 为什么不是白名单：白名单要 150 个以上才装得下 g++ 与 CPython（实测这两边各要几十个
// 互不相同的调用，换发行版就变），装漏一个就把"这台判机判不了题"变成常态；更糟的是要能
// 编译就得放行 execve/clone3/wait4，于是它连"不许起进程"这件本该由它解决的问题都没解决。
// 黑名单只挡真能出门的那些，代价是少了"未知调用也一并挡掉"那层保底。
//
// 为什么返回 EPERM 而不是 KILL：多线程程序里 KILL_THREAD 只杀一个线程，剩下的永远等
// （实测 Go 与 Python 都表现为挂住不返回，判机只能靠墙上时钟收，那条判定不可用）；
// KILL_PROCESS 又会把"学生调了一个被挡的调用"变成一条没有原因的信号死。EPERM 让调用
// 失败、让程序自己报错，学生看到的是一句能读懂的 PermissionError。
//
// 牢房本来就有 CLONE_NEWNET（命名空间里没有第二张网卡），所以断网不靠这一层；它挡的是
// 内核攻击面和 ptrace 这种"看着判机干活"的路子。

const (
	prSetNoNewPrivs   = 38
	prSetSeccomp      = 22
	seccompModeFilter = 2

	// Go 的 syscall 包没有 SYS_SECCOMP 与后面那几个号，这里按 x86_64 的 ABI 写死。
	// 整个过滤器只在 x86_64 上生效（见 program 里那条架构检查），所以号是对的。
	sysSeccomp           = 317
	sysProcessVMReadv    = 312
	sysProcessVMWritev   = 313
	sysSetns             = 308
	sysUserfaultfd       = 323
	sysBpf               = 321
	seccompSetModeFilter = 1
	seccompFlagTsync     = 1

	seccompRetAllow = 0x7fff0000
	seccompRetErrno = 0x00050000
	eperm           = 1
	auditArchX8664  = 0xc000003e

	// struct seccomp_data { __s32 nr; __u32 arch; __u64 ip; __u64 args[6]; }
	seccompDataNr   = 0
	seccompDataArch = 4
)

// denied 是这份黑名单：socket 全家、ptrace/读别人内存、改这台机器内核行为的东西、
// 以及在牢房里再动一次挂载表或换一个牢房。
var denied = []uint32{
	syscall.SYS_SOCKET, syscall.SYS_CONNECT, syscall.SYS_BIND, syscall.SYS_LISTEN,
	syscall.SYS_SENDTO, syscall.SYS_RECVFROM, syscall.SYS_SENDMSG, syscall.SYS_RECVMSG,
	syscall.SYS_SHUTDOWN, syscall.SYS_SOCKETPAIR, syscall.SYS_ACCEPT, syscall.SYS_ACCEPT4,

	syscall.SYS_PTRACE, sysProcessVMReadv, sysProcessVMWritev,

	syscall.SYS_REBOOT, syscall.SYS_KEXEC_LOAD, syscall.SYS_SWAPON, syscall.SYS_SWAPOFF,
	sysBpf, sysUserfaultfd,

	syscall.SYS_MOUNT, syscall.SYS_UMOUNT2, syscall.SYS_PIVOT_ROOT, syscall.SYS_CHROOT,
	sysSetns, syscall.SYS_UNSHARE,
	// 这两个不是"出门"，是"离开判机的进程组"：主程序退出后，换过会话/进程组的子进程
	// 会躲过判机的组回收，留在机器上吃 CPU、占满 uid 的 nproc 预算。判题程序不需要它们。
	syscall.SYS_SETSID, syscall.SYS_SETPGID,
}

// program 编出那条 cBPF 指令流：先认架构（不是 x86_64 就整个放行——这里的系统调用号
// 是按 x86_64 写的，拿错号去挡等于随机挡），再逐个比对号，命中返回 EPERM，其余放行。
func program() []syscall.SockFilter {
	next := func(code uint16, jt, jf uint8, k uint32) syscall.SockFilter {
		return syscall.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
	}
	out := []syscall.SockFilter{
		next(syscall.BPF_LD|syscall.BPF_W|syscall.BPF_ABS, 0, 0, seccompDataArch),
		// 架构不对 → 跳开整条 DENY 链直接 ALLOW；对了就顺着往下走。
		// 这里的 jt/jf 是"从下一条指令算起的跳过条数"：链长 = 1 条载入号 + 每个号 2 条，
		// 反过来写（把放行当成命中）不会报错，只会静默地什么都不挡——实测就是这样让
		// socket() 与 ptrace() 在"装了过滤器"的牢房里照样成功。
		next(syscall.BPF_JMP|syscall.BPF_JEQ|syscall.BPF_K, 0,
			uint8(len(denied)*2+1), auditArchX8664),
		next(syscall.BPF_LD|syscall.BPF_W|syscall.BPF_ABS, 0, 0, seccompDataNr),
	}
	for _, nr := range denied {
		// 命中 → 下一条就是 RET ERRNO；不命中 → 跳 1 条继续比下一个号。
		out = append(out, next(syscall.BPF_JMP|syscall.BPF_JEQ|syscall.BPF_K, 0, 1, nr))
		out = append(out, next(syscall.BPF_RET|syscall.BPF_K, 0, 0, seccompRetErrno|eperm))
	}
	out = append(out, next(syscall.BPF_RET|syscall.BPF_K, 0, 0, seccompRetAllow))
	return out
}

// applySeccomp 在 exec 之前把过滤器装上。它必须是**最后一件事**：降权、上限额、
// LookPath 都要用被挡的那批调用（牢房自己就靠 unshare/mount/chroot），早装一步牢房
// 还没建好就先死了。
//
// 装不上就按"牢房没建起来"出去（ErrPrefix + FailExit）：判机报判机故障而不是让学生
// 去改一份没做错的文件——这一层失败是这台机器的配置问题，不是那道题的问题。
func applySeccomp() error {
	// 没特权时装不上过滤器，除非先把"以后再也拿不到特权"说死：内核要求这两件事一起来。
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", errno)
	}
	instr := program()
	// Go 的 SockFprog.Len 是 uint16：这份名单远小于 65535 条，但加进黑名单的人不该靠
	// 运气发现溢出，所以这里当场算一遍并要求它待在 uint16 里。
	if len(instr) > 0xffff {
		return fmt.Errorf("过滤器 %d 条指令，超过内核单条过滤器的上限", len(instr))
	}
	prog := syscall.SockFprog{Len: uint16(len(instr)), Filter: &instr[0]}
	// TSYNC：一次装到**所有线程**。Go 与 Python 都是多线程的，而 prctl 那条路只管当前线程
	// ——剩下那些线程等于没被管过，"看着装了其实没装"是最坏的一种失败。
	if _, _, errno := syscall.RawSyscall(sysSeccomp, seccompSetModeFilter, seccompFlagTsync,
		uintptr(unsafe.Pointer(&prog))); errno == 0 {
		return nil
	} else if errno != syscall.ENOSYS && errno != syscall.EINVAL {
		return fmt.Errorf("seccomp(TSYNC): %w", errno)
	}
	// 老内核没有 seccomp(2)：退回 prctl，覆盖面小到当前线程。判机要能在日志里看见这件事，
	// 所以调用方拿到的是 ok 但操作员看到的备注里没有它——那条路留给 -sandbox=on 的检查。
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter,
		uintptr(unsafe.Pointer(&prog)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(SET_SECCOMP): %w", errno)
	}
	return nil
}
