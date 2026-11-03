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

	// 下一条仍然在牢房里、仍然是 nobody。
	uid := judgeSource(t, tool, getuidSource, threeCase(), 2000, 262144)
	if uid.Verdict != AC {
		t.Fatalf("假输出之后的那条判成 %s（%s）：牢房或身份变了", uid.Verdict, uid.Detail)
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
	hostOnly := filepath.Join(t.TempDir(), "bin", "prog")
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

// 没有牢房时，身份这一层必须由判机自己补上：把 root 交给学生的代码永远不是可接受的默认值。
func TestUnjailedRunNeverKeepsRoot(t *testing.T) {
	if _, err := exec.LookPath("g++"); err != nil {
		t.Skip("没有 g++")
	}
	restoreSandboxFor(t)
	SetSandbox("", jail.Off, false, 0, 0) // 故意不带牢房：这一条测的正是少了它还剩什么

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil { // 降权之后还要能在里面编译
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.cpp"), []byte(getuidSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := Run([]string{"g++", "-O2", "-std=c++17", "-o", "main", "main.cpp"}, dir, nil, CompileLimits); out.Exit != 0 {
		t.Fatalf("编译失败：%q", out.Stderr)
	}
	ex := Run([]string{filepath.Join(dir, "main")}, dir, nil, Limits{TimeMS: 2000, MemKB: 262144})
	uid := strings.TrimSpace(string(ex.Stdout))
	if uid == "" {
		t.Fatalf("程序没跑出身份：%+v", ex)
	}
	if os.Getuid() == 0 {
		if uid != strconv.Itoa(nobodyUID) {
			t.Errorf("判机是 root、没牢房也没给 -run-as：提交以 uid %s 跑了，想要 %d", uid, nobodyUID)
		}
		return
	}
	// 判机本身不是 root（容器或专用账号）时照旧以自己的身份跑——那是调用方选的形态，
	// 这里只要求它别把人家的 root 端出来。
	if uid != strconv.Itoa(os.Getuid()) {
		t.Errorf("非 root 判机改变了提交的身份：uid %s，判机自己 %d", uid, os.Getuid())
	}
}

// restoreSandboxFor 把进程内的沙盒状态还原回去：这一组会故意撞上"牢房坏了"，
// 留着那份结论，同一次 go test 里后面的用例就都跑在没牢房的状态上了。
func restoreSandboxFor(t *testing.T) {
	t.Helper()
	saved := sandbox
	t.Cleanup(func() { sandbox = saved })
}
