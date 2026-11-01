package judge

import (
	"debug/elf"
	"fmt"
	"strconv"
)

// elfimage.go 回答一个问题：**这个可执行文件还没跑起来，就已经要占多少内存**。
//
// 为什么需要它：判题按驻留内存（RSS）计量，而一个全局大数组的程序（OI 里最常见的"数组
// 开大了"）根本走不到"驻留很多"那一步。牢房里 RLIMIT_AS = 上限 + 512 MB（见
// internal/jail/jail_linux.go 的 asSlackKB），镜像比这还大时内核扩 brk 覆盖 .bss 那一步是
// **静默失败**的：进程照旧起得来，碰到第一个没映射上的页才被打死。判机那台 ECS 上实测过
// p_memsz=915.6MiB 的二进制在 `ulimit -v 786432` 下退出码 139、驻留几 MB —— 于是它只驻留了
// 十几 MB 就死了，判定按"没超内存 + 非零退出"落成 RE。学生看到的是一行自相矛盾的话：
//
//	运行时错误 · 内存 17.52M        ← 真相反：他要的地址空间是本题上限的四倍多
//
// 镜像的 PT_LOAD 段之和（p_memsz，含 .bss）正是"注定要占的那块地址空间"，读它只要解析
// 文件头，不需要跑起来。认不出来（Windows 的 PE、被壳处理过的文件）就返回 0：这一条只
// 负责把一个已经发生的崩溃解释对，绝不能变成任何判定的必要条件。

// imageMemKB 返回可执行文件所有已加载段的内存需求（KB），0 表示"这台机器/这个格式我读不出来"。
func imageMemKB(path string) int {
	f, err := elf.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	var total uint64
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		total += p.Memsz
		if total > 1<<40 { // 超过 1TB 的一定是坏头或壳，不值得当真
			return 0
		}
	}
	return int(total / 1024)
}

// imageSlackKB 是牢房里 RLIMIT_AS 相对题目上限留的余量（见 internal/jail/jail_linux.go
// 的 asSlackKB，两边必须一致）。用它划界是为了只改判"连这点宽容都放不下"的那次崩溃：
// 镜像比上限大但还在余量内的程序**跑得起来**，它要是被信号打死了，那是它自己的 bug，
// 报 RE 才对。
const imageSlackKB = 512 << 10

// markImageOOM 把"镜像本身已经越过本题上限"解释出来的那一次崩溃改成 MLE。
//
// 只在三个条件同时成立时动手：判出来是 RE、进程是**被信号打死**（`diedBySignal` 由调用点从
// wait status 取——Go 把信号死记成退出码 -1，从 `ex.Exit` 反推不出来；程序自己 exit 非零说明
// 它真的跑起来了，只是选择了失败）、镜像需求超过上限连同那道余量。满足时这个数字不再是测量值
// 而是需求值，所以内存那一格保留实测的那份，把"要 1.2GB"这件事写进判机信息，
// 让老师一眼看得出孩子的问题是数组开大了。
func markImageOOM(ex *Execution, lim Limits, imageKB int, diedBySignal bool) {
	if ex.Verdict != RE || lim.MemKB <= 0 || imageKB <= 0 || !diedBySignal {
		return
	}
	if imageKB <= lim.MemKB+imageSlackKB {
		return // 连余量都压得住，说明它本来跑得起来：这次崩溃不关内存的事
	}
	reason := fmt.Sprintf("程序本身要 %s 内存（静态数组、代码与只读数据加起来），本题上限只有 %s，判机额外给的 %s 余量也放不下：还没开始跑，地址空间就已经不够，第一条指令附近就被内核打死",
		mebi(imageKB), mebi(lim.MemKB), mebi(imageSlackKB))
	if ex.Note != "" {
		reason += "；" + ex.Note
	}
	ex.Verdict = MLE
	ex.Note = reason
}

// mebi 把 KB 说成人口语的单位：整 MB 就不带小数，否则留两位。
func mebi(kb int) string {
	mb := float64(kb) / 1024
	if mb == float64(int64(mb)) {
		return strconv.FormatInt(int64(mb), 10) + "MB"
	}
	return strconv.FormatFloat(mb, 'f', 2, 64) + "MB"
}
