package judge

// 交互题：选手写一份程序，作者写一份交互器，两个进程用两条管道对接——选手的标准输出接到
// 交互器的标准输入，交互器的标准输出接回选手的标准输入。判定由交互器的退出码给出。
//
// 资源口径（站主定的）：
//   - 时限按**两侧 CPU 之和**与题面时限比。交互器每答一句都要替选手算一次，分开算等于给
//     交互题白送一份时限。
//   - 内存只算**选手那一份**：交互器占多少不由学生负责。交互器仍有自己的硬上限，越线按
//     判机故障收，不能让它把机器顶穿。
//
// 交互器拿到的命令行与判题器同一套约定：argv 依次是 输入文件、选手输出转写、答案文件。
// 让作者只学一条规则，比让他再查一遍"交互器参数是不是另一套"好。

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/jail"
)

// Duo 是一次交互判题的两个进程。
type Duo struct {
	User, Interactor Execution
	// TimeMS 是两侧 CPU 之和，判定与报告都用它。
	TimeMS int
}

type duoSide int8

const (
	duoUser duoSide = iota
	duoInteractor
)

type duoTrip struct {
	side  duoSide
	memKB int
}

// duoWait 是两个进程各自的收尾；nil 表示这一侧没能收到尾，进程还挂着。
type duoWait struct{ user, itx *waitResult }

func closePipes(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// closeQuiet 关掉判机手里那两个写端：对面的进程据此读到 EOF。判机在任何一条退出路径上都
// 要关掉它们，否则一侧死了另一侧会永远等在管道上，把整条判机队列一起拖住。
func closeQuiet(cs ...io.Closer) {
	for _, c := range cs {
		if c != nil {
			_ = c.Close()
		}
	}
}

// startNote 把启动失败翻译成人能查的一句话。Go 把子进程整条 fork→unshare→exec 的失败
// 都报成 "fork/exec <路径>: <errno>"，所以 "permission denied" 很可能是 unshare 被拒，
// 而不是文件权限问题。
func startNote(err error, cmd *exec.Cmd) string {
	note := "start failed: " + err.Error()
	if unshareRequested(cmd) {
		note = "沙盒未能启动（unshare 命名空间或 exec 被拒）: " + err.Error()
	}
	return note
}

// fill 把一次 wait 的结果写进 Execution。verdictFor 拿到的是零值 Limits：时限与内存由
// RunDuo 按"两侧之和"与"只算选手"这两条口径统一判，这一步只认退出码与实测值。
//
// st 是这一侧那一把的状态信道：牢房建成没有只从它上面读，不读 stderr——那一份是选手与
// 交互器各自打印的，谁都能写出一句"OJ-JAIL:"（见 internal/jail 的 Status）。
func (e *Execution) fill(w waitResult, out, errBuf *limitedBuffer, g Guard, st *jail.Status) {
	_, memKB := g.Measure()
	timeMS := usageMS(w.state)
	if timeMS < 0 {
		timeMS, _ = g.Measure()
	}
	if memKB < 0 {
		memKB = usageMemKB(w.state)
	}
	e.TimeMS, e.MemKB, e.Exit = timeMS, memKB, exitCode(w.err)
	e.Stdout, e.Stderr = out.Bytes(), errBuf.Bytes()
	if reason, broken := jailBrokenFrom(st, e.Exit); broken {
		// 牢房没建起来不是学生的错，也不该由交互器背：报判机故障。
		note := jailBrokenNote(reason)
		reportJailBroken(note)
		e.Verdict, e.Note = IE, note
		return
	}
	e.Verdict, e.Note = verdictFor(Limits{}, timeMS, memKB, e.Exit, out.Overflowed())
	if e.Verdict == RE && w.err != nil {
		e.Note = w.err.Error()
	}
}

// waitCmd 收一个进程的尾。
func waitCmd(cmd *exec.Cmd) *waitResult {
	err := cmd.Wait()
	return &waitResult{err: err, state: cmd.ProcessState}
}

// grabAll 收两边的尾，最多等 max。判机不能为一个杀不掉的进程把整条队列占住，所以到点就
// 返回，没收到的那一侧留成"没能收尾"。
func grabAll(userCmd, itxCmd *exec.Cmd, max time.Duration) duoWait {
	ch := make(chan duoWait, 1)
	go func() { ch <- duoWait{user: waitCmd(userCmd), itx: waitCmd(itxCmd)} }()
	select {
	case w := <-ch:
		return w
	case <-time.After(max):
		return duoWait{}
	}
}

// stopBoth 停掉两个进程树。先交互器再选手：交互器一死，选手那一侧会读到 EOF，一个还算
// 规矩的程序就能自己结束，少一次硬杀。
func stopBoth(userGuard, itxGuard Guard) {
	_ = itxGuard.Kill()
	_ = userGuard.Kill()
}

// finish 把两边的收尾写进 Duo。没收到的那一侧留成判机故障：判机不知道它去了哪里。
func (d *Duo) finish(w duoWait, userOut, userErr, itxOut, itxErr *limitedBuffer,
	userGuard, itxGuard Guard, userSt, itxSt *jail.Status) {
	if w.user != nil {
		d.User.fill(*w.user, userOut, userErr, userGuard, userSt)
	} else {
		d.User.Verdict, d.User.Note = IE, orText(d.User.Note, "选手程序没能收尾")
	}
	if w.itx != nil {
		d.Interactor.fill(*w.itx, itxOut, itxErr, itxGuard, itxSt)
	} else {
		d.Interactor.Verdict, d.Interactor.Note = IE, orText(d.Interactor.Note, "交互器没能收尾")
	}
}

// RunDuo 同时起跑选手程序与交互器，跑到两边都结束、撞上墙上时钟或某一侧越过硬上限为止。
//
// 它不返回 error：一边崩了、被信号打死、卡住不返回，都是**判定**而不是判机出错，必须变成
// 一条能显示出来的结果，而不是一句 Go 错误把这条工单吞掉。
func RunDuo(userArgv, interactorArgv []string, dir string, userLim, interactorLim Limits) Duo {
	d := Duo{
		User:       Execution{Verdict: IE, TimeMS: -1, MemKB: -1},
		Interactor: Execution{Verdict: IE, TimeMS: -1, MemKB: -1},
	}
	giveUp := func(side duoSide, note string) Duo {
		if side == duoUser {
			d.User.Note = note
		} else {
			d.Interactor.Note = note
		}
		return d
	}
	if len(userArgv) == 0 || len(interactorArgv) == 0 {
		return giveUp(duoUser, "交互判题需要两个命令行，这里少了一个")
	}
	userBin, err := exec.LookPath(userArgv[0])
	if err != nil {
		return giveUp(duoUser, "missing program: "+userArgv[0])
	}
	itxBin, err := exec.LookPath(interactorArgv[0])
	if err != nil {
		return giveUp(duoInteractor, "交互器跑不起来："+interactorArgv[0])
	}
	if userLim.TimeMS <= 0 {
		userLim.TimeMS = 1000
	}

	// 对话的拓扑：每一侧的标准输出都先流进判机，判机一边把它转给对面，一边自己留一份转写。
	//
	//	选手 stdout ─判机搬运─┬→ 交互器 stdin
	//	                     └→ 选手转写（交互器 argv 的第二个参数，也是"学生看到了什么、
	//	                        答了什么"的唯一证据）
	//	交互器 stdout ─判机搬运─┬→ 选手 stdin
	//	                       └→ 交互器输出（评语的来源）
	//
	// 管道用 exec 自己的 StdinPipe/StdoutPipe，不用判机 os.Pipe 出来的手柄：实测在 Windows
	// 上把自造的管道手柄交给**两个**子进程时，判机这一侧读会拿到 "read |1: Access is
	// denied"，两边一个字都过不去（只交一个子进程时却是好的）。exec 造的管道是它保证能交给
	// 子进程的那一种，两条平台都走得通。
	//
	// 搬运读到 EOF 就等于那一侧关掉了自己的标准输出，也就是那个进程已经收工。所以顺序是固定
	// 的：先让两个搬运结束，再 Wait。反过来 Wait 会把管道关掉，尾巴上那几句对话就永久丢了，
	// 而那份转写正是出争议时唯一的凭据。
	userOut, userErr := newCappedBuffer(MaxOutput), newCappedBuffer(MaxStderr)
	itxOut, itxErr := newCappedBuffer(MaxOutput), newCappedBuffer(MaxStderr)

	userCmd := exec.Command(userBin, userArgv[1:]...)
	userCmd.Dir, userCmd.Env, userCmd.Stderr = dir, runEnv(dir), userErr
	userCmd.WaitDelay = reapGrace
	itxCmd := exec.Command(itxBin, interactorArgv[1:]...)
	itxCmd.Dir, itxCmd.Env, itxCmd.Stderr = dir, runEnv(dir), itxErr
	itxCmd.WaitDelay = reapGrace

	userIn, err := userCmd.StdinPipe()
	if err != nil {
		return giveUp(duoUser, "pipe: "+err.Error())
	}
	userPipe, err := userCmd.StdoutPipe()
	if err != nil {
		return giveUp(duoUser, "pipe: "+err.Error())
	}
	itxIn, err := itxCmd.StdinPipe()
	if err != nil {
		return giveUp(duoInteractor, "pipe: "+err.Error())
	}
	itxPipe, err := itxCmd.StdoutPipe()
	if err != nil {
		return giveUp(duoInteractor, "pipe: "+err.Error())
	}

	// 两边各自一条状态信道（为什么不能用 stderr 见 internal/jail 的 Status）：交互器也是
	// 别人写的代码，它那一侧的输出同样不可信。
	userSt, err := newJailStatus(userLim)
	if err != nil {
		return giveUp(duoUser, "牢房的状态信道建不起来，不敢裸跑: "+err.Error())
	}
	defer userSt.Remove()
	userLim.jailStatus = userSt
	itxSt, err := newJailStatus(interactorLim)
	if err != nil {
		return giveUp(duoInteractor, "牢房的状态信道建不起来，不敢裸跑: "+err.Error())
	}
	defer itxSt.Remove()
	interactorLim.jailStatus = itxSt

	userGuard, err := newGuard(userLim)
	if err != nil {
		return giveUp(duoUser, "sandbox init failed: "+err.Error())
	}
	defer userGuard.Close()
	itxGuard, err := newGuard(interactorLim)
	if err != nil {
		return giveUp(duoInteractor, "sandbox init failed: "+err.Error())
	}
	defer itxGuard.Close()

	// bail 用在"起不来"的那几条路上：已经起来的那一侧必须一起收掉。留一个没人管的进程在
	// 机器上跑，比一条失败的工单危险得多。
	bail := func(side duoSide, note string) Duo {
		stopBoth(userGuard, itxGuard)
		closeQuiet(userIn, itxIn)
		grabAll(userCmd, itxCmd, 2*reapGrace)
		return giveUp(side, note)
	}
	if err := userGuard.Configure(userCmd); err != nil {
		return bail(duoUser, "sandbox config failed: "+err.Error())
	}
	if err := itxGuard.Configure(itxCmd); err != nil {
		return bail(duoInteractor, "sandbox config failed: "+err.Error())
	}
	if err := userCmd.Start(); err != nil {
		return bail(duoUser, startNote(err, userCmd))
	}
	if err := userGuard.Attach(userCmd.Process); err != nil {
		return bail(duoUser, "attach failed: "+err.Error())
	}
	if err := itxCmd.Start(); err != nil {
		return bail(duoInteractor, startNote(err, itxCmd))
	}
	if err := itxGuard.Attach(itxCmd.Process); err != nil {
		return bail(duoInteractor, "attach failed: "+err.Error())
	}

	// 两个搬运。缓冲放在 MultiWriter 的第一个：对端已经退出时往它写会失败，排在后面那个
	// 错误不能让转写少收一段。
	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, _ = io.Copy(io.MultiWriter(userOut, itxIn), userPipe)
		_ = itxIn.Close()
	}()
	go func() {
		defer copies.Done()
		_, _ = io.Copy(io.MultiWriter(itxOut, userIn), itxPipe)
		_ = userIn.Close()
	}()
	copiesDone := make(chan struct{})
	go func() { copies.Wait(); close(copiesDone) }()

	// 墙上时钟按两侧时限之和给：两边都在等对方说话的死锁不会自己结束。
	deadline := wallClockFor(userLim.TimeMS + interactorLim.TimeMS)
	trip := make(chan duoTrip, 1)
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	defer func() { close(stopWatch); <-watchDone }()
	go func() {
		defer close(watchDone)
		tick := time.NewTicker(memPollInterval)
		defer tick.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-tick.C:
				if _, mem := userGuard.Measure(); userLim.MemKB > 0 && mem > userLim.MemKB+memTripKB {
					trip <- duoTrip{side: duoUser, memKB: mem}
					return
				}
				if _, mem := itxGuard.Measure(); interactorLim.MemKB > 0 && mem > interactorLim.MemKB+memTripKB {
					trip <- duoTrip{side: duoInteractor, memKB: mem}
					return
				}
			}
		}
	}()

	// settle 是强杀之后的收尾：kill 让两边关掉管道，搬运据此读到 EOF 或被写端报错叫醒，
	// 所以先等搬运结束，再 Wait 取退出码，两步各给 max。
	settle := func(max time.Duration) duoWait {
		select {
		case <-copiesDone:
		case <-time.After(max):
		}
		return grabAll(userCmd, itxCmd, max)
	}

	var w duoWait
	select {
	case <-copiesDone:
		w = grabAll(userCmd, itxCmd, 2*reapGrace)
		d.finish(w, userOut, userErr, itxOut, itxErr, userGuard, itxGuard, userSt, itxSt)

	case t := <-trip:
		over := "交互器的内存超过它自己的上限 " + strconv.Itoa(interactorLim.MemKB) + "KB 还不止 " +
			strconv.Itoa(memTripKB) + "KB"
		if t.side == duoUser {
			over = "选手程序的内存超过题面上限 " + strconv.Itoa(userLim.MemKB) + "KB 还不止 " +
				strconv.Itoa(memTripKB) + "KB"
		}
		stopBoth(userGuard, itxGuard)
		w = settle(2 * reapGrace)
		d.finish(w, userOut, userErr, itxOut, itxErr, userGuard, itxGuard, userSt, itxSt)
		if t.side == duoUser {
			d.User.Verdict, d.User.MemKB, d.User.MemMin, d.User.Note = MLE, t.memKB, true, over
		} else {
			// 交互器吃爆自己的预算是判机侧的事，不该由学生的内存判定买单。
			d.Interactor.Verdict, d.Interactor.MemKB, d.Interactor.MemMin, d.Interactor.Note = IE, t.memKB, true, over
		}
		d.TimeMS = d.User.TimeMS + d.Interactor.TimeMS
		return d

	case <-time.After(deadline):
		note := "墙上时钟 " + strconv.Itoa(int(deadline/time.Millisecond)) + "ms 到点"
		stopBoth(userGuard, itxGuard)
		// 写端也交掉：一个只顾自己算、从来不读输入的程序，只有看见 EOF 才可能结束。
		closeQuiet(userIn, itxIn)
		w = settle(2 * reapGrace)
		if w.user == nil || w.itx == nil {
			note += "，而且有进程杀不掉"
		}
		d.finish(w, userOut, userErr, itxOut, itxErr, userGuard, itxGuard, userSt, itxSt)
		d.User.Verdict, d.User.TimeMin, d.User.Note = TLE, true, note
		d.TimeMS = d.User.TimeMS + d.Interactor.TimeMS
		return d
	}

	d.TimeMS = d.User.TimeMS + d.Interactor.TimeMS
	// 时限口径是两侧之和。已经判成判机故障或内存超限时不再叠一个 TLE：那条更晚发生的坏
	// 消息会把真正的原因盖掉。
	if d.User.Verdict != IE && d.Interactor.Verdict != IE &&
		userLim.TimeMS > 0 && d.TimeMS > userLim.TimeMS {
		d.User.Verdict, d.User.TimeMin = TLE, true
		d.User.Note = "两侧用时之和 " + strconv.Itoa(d.TimeMS) + "ms 超过 " + strconv.Itoa(userLim.TimeMS) + "ms"
	}
	return d
}

// verdictForDuo 把两个进程的结果合成这一条用例的判定与评语。
//
// 交互器的退出码按 testlib 那一套认：**0 = 对，1 = 错，7 = 选手程序出错（_pr）**，其他都
// 算"交互器没给出判定"。为什么先问交互器有没有给判定，再问选手怎么了：交互器消失或崩掉
// 之后，选手那一侧几乎必然跟着非零退出（读到 EOF、管道被关），把那种退出判成 RE 或 WA
// 就是让作者程序的坏结果算到学生头上。所以：
//
//  1. 任一侧的牢房没建起来或进程起不来 → 判机故障。
//  2. 选手这边超时或超内存 → 如实判：这是判机量到的物理事实。墙上时钟收场时两边都是被
//     信号杀掉的（交互器退出码 -1），此时"交互器没给出判定"只是被杀的后果，把它摆在前面
//     就会把一条 TLE 报成判机故障——Linux 上实测踩过。
//  3. 交互器没给出判定（退出码不在 0/1/7，或它自己崩了/跑不完）→ 判机故障。
//  4. 选手自己崩了 → RE。
//  5. 剩下的按交互器说的算：0 对、1 错、7 选手程序出错；它打印的内容成为评语。
func verdictForDuo(d Duo) (Verdict, string) {
	itxVerdict := d.Interactor.Exit
	itxSaid := itxVerdict == 0 || itxVerdict == 1 || itxVerdict == 7
	comment := clipText(commentOf(d.Interactor), maxDetail)
	switch {
	case d.User.Verdict == IE:
		return IE, orText(d.User.Note, "选手程序没能跑起来，也没有给出原因")
	case d.Interactor.Verdict == IE:
		return IE, orText(d.Interactor.Note, "交互器没能跑起来，也没有给出原因")
	case d.User.Verdict == MLE:
		return MLE, d.User.Note
	case d.User.Verdict == TLE:
		return TLE, orText(d.User.Note, "两侧用时之和超过了时限")
	case !itxSaid:
		// 交互器自己崩了或没跑完也落在这一条里：它没资格替这轮对话下判定。
		return IE, "交互器没给出判定（退出码 " + strconv.Itoa(d.Interactor.Exit) +
			"；只有 0 算对、1 算错、7 算选手程序出错）；它那边的话：" +
			orText(comment, "没有输出")
	case d.User.Verdict == RE:
		return RE, orText(d.User.Note, "选手程序异常退出（退出码 "+strconv.Itoa(d.User.Exit)+"）")
	case itxVerdict == 0:
		return AC, comment
	case itxVerdict == 1:
		return WA, orText(comment, "交互器判定为错，但没有说明原因")
	default: // 7：交互器说选手程序出了错，通常是对话没按题目规矩走完
		return RE, orText(comment, "交互器报告选手程序出错")
	}
}

// commentOf 取交互器想说的话：正常在 stdout，崩掉的那一类只在 stderr。
func commentOf(e Execution) string {
	if s := strings.TrimSpace(string(e.Stdout)); s != "" {
		return s
	}
	return strings.TrimSpace(string(e.Stderr))
}

// runInteractive 跑一条用例的交互对话：先把输入与答案写成文件交给交互器，两侧同时起跑，
// 结束后把选手的输出转写也落成文件（交互器 argv 的第二个参数）。
//
// 时限只有一份，就是题面那个：它按两侧 CPU 之和判定，所以交互器自己不设单独时限——各设
// 一份等于给同一道题两个不同的时钟，报了之和却按各自的限值杀进程，谁也对不上。
// 内存两边各有各的：题面那个管选手，auxLimits 那个管交互器。
func runInteractive(interactor, userArgv []string, dir string, tc Case, lim Limits) Duo {
	paths := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		var body []byte
		switch i {
		case 0:
			body = tc.Input
		case 2:
			body = tc.Expected
		}
		// 中间那份是对话结束后才写的转写，先把名字占好，交互器按同一套约定拿三个参数。
		p := filepath.Join(dir, "itx"+strconv.Itoa(i)+".dat")
		if err := writeRunFile(p, body); err != nil {
			return Duo{
				User:       Execution{Verdict: IE, TimeMS: -1, MemKB: -1, Note: "交互题的文件写不出来: " + err.Error()},
				Interactor: Execution{Verdict: IE, TimeMS: -1, MemKB: -1},
			}
		}
		paths = append(paths, p)
	}
	d := RunDuo(userArgv, append(append([]string{}, interactor...), paths...), dir,
		lim, Limits{MemKB: auxLimits.MemKB, MaxProcs: auxLimits.MaxProcs})
	// 转写落在交互器的第二个参数上：出争议时"学生到底答了什么"只有这一份证据。
	if err := writeRunFile(paths[1], d.User.Stdout); err != nil {
		d.User.Note = orText(d.User.Note, "对话记录写不进工作目录: "+err.Error())
	}
	return d
}
