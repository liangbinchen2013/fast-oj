//go:build linux

package judge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/jail"
)

// 这一组测的是"牢房坏了没有"这件事**由谁说了算**。
//
// 2026-11-01 之前它由子进程的 stderr 行首说了算，而那份输出同时是编译器与选手程序的：
// 一条提交打印一行 `OJ-JAIL: ...`（甚至只用一条 `#line` 的编译错误）就能让判机把整台
// 机器的牢房永久关掉，后续所有人的提交在宿主上裸跑。ECS 上实测过：关掉之后下一条提交
// 打印 getuid() 得到 0。所以现在判据只有一条——jailer 在这一把的一次一用信道上写的话。

const fakeJailLineSource = `#include <cstdio>
int main(){ std::fprintf(stderr, "OJ-JAIL: 这是学生自己打印的一行字\n"); std::puts("3"); return 0; }`

// #line 会把编译器自己的诊断顶到第一行、换上指定的"文件名"：判机过去就是按行首认的，
// 于是一条编译错误的输出顺序就够冒充一句牢房失败声明。
const fakeCompileErrorSource = "#line 1 \"OJ-JAIL: x\"\n#error x\n"

const getuidSource = `#include <cstdio>
#include <unistd.h>
int main(){ std::printf("%d\n", (int)getuid()); return 0; }`

// threeCase 是"程序打印 3"就能过的那一道用例：这一组只关心身份与状态，不关心题目。
func threeCase() []Case {
	return []Case{{Name: "1", Ordinal: 1, Score: 100, Input: []byte("1 2\n"), Expected: []byte("3\n")}}
}

// 不可信输出里的那句假话，既不能改判定，也不能动这台机器的隔离状态。
func TestForgedJailStderrChangesNothing(t *testing.T) {
	jailOn(t)
	tool := requireToolchain(t)
	restoreSandboxFor(t)

	if !Jailed() {
		t.Fatal("前置：牢房该开着")
	}
	rep := judgeSource(t, tool, fakeJailLineSource, threeCase(), 2000, 262144)
	if rep.Verdict != AC {
		t.Errorf("打印了假失败声明的那条判成 %s（%s），想要 AC——判机不该再把它当牢房坏了",
			rep.Verdict, rep.Detail)
	}
	if !Jailed() || SandboxBroken() {
		t.Fatalf("一行假输出关掉了牢房：Jailed=%v 说明=%q", Jailed(), SandboxNote())
	}

	// 下一条仍然在牢房里、仍然是那个没被映射的号。身份要当场上验，不能只靠"状态没变"：
	// 期望输出就是牢房里的那个 uid，判成 AC 才说明提交既在牢房里也没被换成 root。
	uidCase := []Case{{Name: "1", Ordinal: 1, Score: 100, Input: []byte("1 2\n"),
		Expected: []byte(strconv.Itoa(int(sandbox.uid)) + "\n")}}
	uid := judgeSource(t, tool, getuidSource, uidCase, 2000, 262144)
	if uid.Verdict != AC {
		t.Fatalf("假输出之后的那条判成 %s（%s），想要 AC：那一格打印的 uid 才是这次要的读数",
			uid.Verdict, uid.Detail)
	}
}

// 编译那一路同样不能当判据：g++ 会把它自己的第一行诊断顶成 `OJ-JAIL: ...`，而那条提交
// 只是 CE，连程序都没跑起来。
func TestCompileErrorCannotFakeAJailFailure(t *testing.T) {
	jailOn(t)
	tool := requireToolchain(t)
	restoreSandboxFor(t)

	rep := judgeSource(t, tool, fakeCompileErrorSource, threeCase(), 2000, 262144)
	if rep.Verdict != CE {
		t.Errorf("那条假编译错误判成 %s，想要 CE（%s）", rep.Verdict, rep.Detail)
	}
	if !Jailed() || SandboxBroken() {
		t.Fatalf("一条编译错误关掉了牢房：Jailed=%v 说明=%q", Jailed(), SandboxNote())
	}
}

// 反过来：真的建不起牢房时，jailer 在信道上留的话必须被读到，而且判机从此不再裸跑。
// 这里的"真失败"是把一个牢房里找不到的程序交给它——失败发生在 jailer 自己身上。
func TestRealJailFailureIsReportedOnTheChannel(t *testing.T) {
	jailOn(t)
	requireToolchain(t)
	restoreSandboxFor(t)

	dir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	// 宿主上存在、牢房里不存在：jailer 会在"牢房里找不到它"那一步失败。
	hostBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(hostBin, 0o755); err != nil {
		t.Fatal(err)
	}
	hostOnly := filepath.Join(hostBin, "prog")
	if err := os.WriteFile(hostOnly, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := Run([]string{hostOnly}, dir, nil, Limits{TimeMS: 2000, MemKB: 262144})
	if ex.Verdict != IE {
		t.Fatalf("真建不起牢房时判成 %s（%q），想要判机故障", ex.Verdict, ex.Note)
	}
	if !strings.Contains(ex.Note, "沙盒未能建立") || !strings.Contains(ex.Note, "找不到") {
		t.Errorf("判机信息没把原因从信道上带回来：%q", ex.Note)
	}
	if !SandboxBroken() {
		t.Error("可信信道报了失败，判机却没把自己标记成坏了")
	}
	if Jailed() {
		t.Error("坏了之后还宣称有牢房")
	}
	// 停下是靠主循环问 SandboxBroken 实现的，这里核对它确实可读得到原因。
	if !strings.Contains(SandboxNote(), "找不到") {
		t.Errorf("说明里没留下线索：%q", SandboxNote())
	}
}

// 程序自己 exit(70) 不算牢房坏了：jailer 把它交出去的那一刻已经在信道上留了记号。
// 这一条钉住"退出码 70 但信道上什么都没有"那半个判据不会被选手踩到。
func TestProgramExiting70IsNotAJailFailure(t *testing.T) {
	jailOn(t)
	tool := requireToolchain(t)
	restoreSandboxFor(t)

	rep := judgeSource(t, tool, "int main(){ return 70; }", threeCase(), 2000, 262144)
	if rep.Verdict != RE {
		t.Errorf("exit(70) 判成 %s（%s），想要 RE", rep.Verdict, rep.Detail)
	}
	if SandboxBroken() || !Jailed() {
		t.Fatalf("选手的退出码关掉了牢房：Jailed=%v 说明=%q", Jailed(), SandboxNote())
	}
}

// root 判机 + 没沙盒 + 没给 -run-as 时，答案是**不跑**：把 root 交给提交的代码永远不是
// 可接受的默认值，而"兜底降到 nobody"在生产这台机上会撞在工作目录的祖先上（0750），
// 变成每条提交一句看不懂的 EACCES。非 root 判机（容器、run.sh 那个专用账号）不受影响。
func TestRootJudgeWithoutSandboxRefusesToRun(t *testing.T) {
	idPath, err := exec.LookPath("id")
	if err != nil {
		t.Skip("这台机器上没有 id 可用")
	}
	restoreSandboxFor(t)
	SetSandbox("", jail.Off, false, 0, 0) // 故意不带牢房：这一条测的正是少了它还剩什么

	// 降权之后还得能走进这个目录：用 /tmp 下面自己建的一层（/tmp 本身是 1777），
	// 而不是 t.TempDir()——它的祖先是 0700 的测试根目录，nobody 进不去，测的就不是这件事了。
	dir, err := os.MkdirTemp("", "oj-drop-check")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	ex := Run([]string{idPath, "-u"}, dir, nil, Limits{TimeMS: 2000, MemKB: 262144})
	if os.Getuid() != 0 {
		// 判机自己不是 root：照旧以它自己的身份跑，这条不许把普通形态也拦下来。
		if got := strings.TrimSpace(string(ex.Stdout)); got != strconv.Itoa(os.Getuid()) {
			t.Errorf("非 root 判机被改变了行为：判定 %s、输出 %q、说明 %q", ex.Verdict, got, ex.Note)
		}
		return
	}
	if ex.Verdict != IE {
		t.Fatalf("root + 没沙盒 判成 %s（%q），想要拒绝跑", ex.Verdict, ex.Note)
	}
	if len(ex.Stdout) != 0 {
		t.Errorf("拒绝之前已经把程序跑起来了，输出 %q", ex.Stdout)
	}
	if !strings.Contains(ex.Note, "不敢把 root 交给提交") {
		t.Errorf("拒绝得不够明白：%q", ex.Note)
	}
	// 给了 -run-as 就照办：这是容器与"root 起步、提交降到 nobody"那两种形态的写法。
	ex2 := Run([]string{idPath, "-u"}, dir, nil, Limits{TimeMS: 2000, MemKB: 262144, RunAs: &DropTo{UID: 65534, GID: 65534}})
	if got := strings.TrimSpace(string(ex2.Stdout)); got != "65534" {
		t.Errorf("显式 -run-as 的那一条以 uid %q 跑了（判定 %s，说明 %q），想要 65534", got, ex2.Verdict, ex2.Note)
	}
}

// restoreSandboxFor 把进程内的沙盒状态还原回去：这一组会故意撞上"牢房坏了"，
// 留着那份结论，同一次 go test 里后面的用例就都跑在没牢房的状态上了。
func restoreSandboxFor(t *testing.T) {
	t.Helper()
	saved := sandbox
	t.Cleanup(func() { sandbox = saved })
}
