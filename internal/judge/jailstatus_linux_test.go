//go:build linux

package judge

import (
	"os"
	"os/exec"
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

// 真的建不起牢房时，判机必须把那句话读回来并停下领单。这里钉的是两处接线，都不依赖
// 目录布局（早先一版靠"把一个牢房里没有的程序交给 jailer"触发失败，而 jailer 把宿主路径
// 换成牢房路径时用了 filepath.Rel，临时目录的编号一变，那条路径可能反而解析得到——同一份
// 代码单独跑绿、全量跑红，那种测试不配当回归闸）：
//
//   - applyJail 有没有把这一把的路径与暗号交给 jailer（没有就没有任何信道）；
//   - jailBrokenFrom 三种读数：写着暗号的失败声明、只有 exec 记号 + 退出码 70、什么都没有。
//
// 真 jailer 真失败时确实会写下那行，由 internal/jail 的 TestJailerReportsARealFailure 覆盖；
// 两端合起来就是完整链路。
func TestJailStatusIsWiredIntoTheJailer(t *testing.T) {
	jailOn(t)
	restoreSandboxFor(t)

	st, err := newJailStatus(Limits{TimeMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Remove()
	lim := Limits{TimeMS: 1000, MemKB: 65536, jailStatus: st}

	cmd := exec.Command("/bin/true")
	cmd.Dir = t.TempDir()
	if err := applyJail(cmd, lim); err != nil {
		t.Fatal(err)
	}
	wrapped := cmd.Args[1]
	if !strings.Contains(wrapped, `"status":"`+st.Path()+`"`) ||
		!strings.Contains(wrapped, `"nonce":"`+st.Nonce()+`"`) {
		t.Fatalf("jailer 没拿到这一把的信道：%s", wrapped)
	}

	// 三种读数。第三种（什么都没写）配合退出码 70 也算失败，那是"jailer 死了却没写成"，
	// 宁可比真话晚一步，也不把没牢房的运行当成正常判完。
	written, err := jail.NewStatus()
	if err != nil {
		t.Fatal(err)
	}
	defer written.Remove()
	f, err := os.OpenFile(written.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// 暗号是从 spec 里拿到的那一份：这里就是这一把自己的暗号，等价于 jailer 写的话。
	// 格式与 jail 包里那一份一致：`暗号\x1fF\x1f原因`。
	if _, err := f.WriteString(written.Nonce() + "\x1fF\x1fmount bind 失败\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if reason, broken := jailBrokenFrom(written, 70); !broken || !strings.Contains(reason, "mount bind") {
		t.Errorf("jailer 写的失败声明没读到：%q / %v", reason, broken)
	}
	if reason, broken := jailBrokenFrom(nil, 70); broken {
		t.Errorf("这一把没有信道却判成牢房坏了：%q", reason)
	}
	if _, broken := jailBrokenFrom(st, 0); broken {
		t.Error("牢房建得好好的（退出码 0）却报坏了")
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
