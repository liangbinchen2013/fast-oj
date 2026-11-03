package judge

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 交互器：把题目输入交给选手，收一个回答，与答案文件比。退 0 算对、退 1 算错。
//
// 三个参数的顺序与判题器完全一样（输入、选手输出转写、答案），作者只需要学一条约定。
const itxCandidate = `#include <cstdio>
#include <cstdlib>
static long first(const char *p){ FILE *f = fopen(p,"rb"); if(!f) return -1;
  char b[256]={0}; fread(b,1,255,f); fclose(f); return strtol(b,0,10); }
int main(int argc, char **argv){
  long n = argc > 1 ? first(argv[1]) : 0;
  long want = argc > 3 ? first(argv[3]) : -1;
  printf("%ld\n", n); fflush(stdout);
  long got = -1;
  // 读不到回答就是选手的错，按 testlib 的规矩退 7（_pr），不能退成"判机自己坏了"。
  if (scanf("%ld", &got) != 1) { printf("交互器没读到选手的回答"); return 7; }
  printf("交互器收到 %ld，期望 %ld", got, want); fflush(stdout);
  return got == want ? 0 : 1;
}`

// 对话正确：读一句、算 n+1、答一句。
const itxGood = `#include <cstdio>
int main(){ long n; if (scanf("%ld",&n)!=1) return 3; printf("%ld\n", n+1); fflush(stdout); return 0; }`

// 答错一句：交互器会退 1。
const itxWrong = `#include <cstdio>
int main(){ long n; if (scanf("%ld",&n)!=1) return 3; printf("%ld\n", n+2); fflush(stdout); return 0; }`

// 不听也不退：一个只顾自己的程序。判机必须被墙上时钟收掉，而且两个进程都不能留活口。
const itxIgnore = `#include <cstdio>
#include <ctime>
int main(){ clock_t s = clock(); int k = 0;
  while (clock() - s < (CLOCKS_PER_SEC / 2)) { k++; }
  printf("%d\n", k); return 0; }`

// 对话中途崩掉：这必须报 RE，不能被交互器那句"我没读到回答"盖成判机故障。
const itxCrash = `#include <cstdio>
int main(){ long n; scanf("%ld",&n); *(volatile int*)0 = 1; return 0; }`

func itxCases() []Case {
	return []Case{{Name: "1", Ordinal: 1, Score: 100, Input: []byte("5\n"), Expected: []byte("6\n")}}
}

// judgeInteractive 按生产的样子跑一次交互判题：工作目录先建好，交互器编在**这个目录里面**。
//
// 后一句不是讲究而是必须：判题器与交互器落在本次提交的目录外，开了沙盒之后它在牢房里
// 看不见自己的可执行文件（牢房里第一条实测就是踩在这个上面）。测试要是把交互器编到别的
// 临时目录，就等于把这条生产约束跳过去了。
func judgeInteractive(t *testing.T, tool Tool, itxSource, source string, cases []Case, timeMS int) Report {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	return Judge(Task{
		SubmissionID: 1, PID: "P1", Language: "cpp", Source: source,
		TimeLimitMS: timeMS, MemLimitKB: 262144,
		Interactor: buildAuxIn(t, dir, AuxInteractor, itxSource), Cases: cases,
	}, tool, dir, nil, nil)
}

// buildAuxIn 在 root 里编一份判机自带的程序，返回它的命令行。
func buildAuxIn(t *testing.T, root, kind, body string) []string {
	t.Helper()
	requireRunnable(t)
	argv, err := BuildAux(Available(DefaultTools()), AuxProgram{Language: "cpp", Source: body}, root, kind, nil)
	if err != nil {
		t.Fatalf("编%s失败：%v", auxKindName(kind), err)
	}
	return argv
}

// 交互题的判定来自交互器：它说对就是 AC。这条同时证明了两件事——管道真的接通了
// （选手读到了交互器从输入文件转发的 5），以及退出码 0 被翻译成了 AC。
func TestInteractiveAcceptsAGoodDialogue(t *testing.T) {
	tool := requireToolchain(t)
	rep := judgeInteractive(t, tool, itxCandidate, itxGood, itxCases(), 5000)
	if rep.Verdict != AC || rep.Score != 100 {
		t.Fatalf("对话正确 = %s %d/%d（%s），要 AC 100/100", rep.Verdict, rep.Score, rep.TotalScore, rep.Detail)
	}
}

// 答错时交互器退 1，而它打印的那句话要成为评语："收到 7，期望 6"是学生唯一能看到的线索。
func TestInteractiveWrongAnswerCarriesTheInteractorMessage(t *testing.T) {
	tool := requireToolchain(t)
	rep := judgeInteractive(t, tool, itxCandidate, itxWrong, itxCases(), 5000)
	if rep.Verdict != WA {
		t.Fatalf("答错 = %s（%s），要 WA", rep.Verdict, rep.Detail)
	}
	if !strings.Contains(rep.Detail, "期望 6") {
		t.Errorf("交互器的话没进详情：%q", rep.Detail)
	}
}

// 选手不理交互器、自己空转：必须被墙上时钟收掉，并且判成 TLE 而不是"永远等在管道上"。
// 这条测试真正的价值是它会**结束**：任何一侧没被干净收掉，判机就会把整条队列拖死。
func TestInteractiveDeadlockIsCutOff(t *testing.T) {
	tool := requireToolchain(t)
	started := time.Now()
	rep := judgeInteractive(t, tool, itxCandidate, itxIgnore, itxCases(), 300)
	if elapsed := time.Since(started); elapsed > 40*time.Second {
		t.Fatalf("判机等了 %s 才收场，两侧进程没被干净杀掉", elapsed)
	}
	if rep.Verdict != TLE {
		t.Fatalf("选手不理交互器 = %s（%s），要 TLE", rep.Verdict, rep.Detail)
	}
	if rep.TimeMS < 300 {
		t.Errorf("报的用时 %dms 比时限还小，而这一条是被超时收的", rep.TimeMS)
	}
}

// 选手在对话中途崩掉 → RE。交互器那边只会说"没读到回答"（退出码 2），那是崩掉的后果，
// 不是原因；把学生自己的崩溃盖成判机故障，他就永远不知道自己程序崩了。
func TestInteractiveCrashIsReportedAsRE(t *testing.T) {
	tool := requireToolchain(t)
	rep := judgeInteractive(t, tool, itxCandidate, itxCrash, itxCases(), 5000)
	if rep.Verdict != RE {
		t.Fatalf("选手中途崩 = %s（%s），要 RE", rep.Verdict, rep.Detail)
	}
}

// 交互器退出码既不是 0 也不是 1（这里让它读不到回答就退 2），而选手没崩：那是作者的程序
// 出了问题，判机故障，不是学生的 WA。
func TestInteractiveBrokenInteractorIsJudgeError(t *testing.T) {
	tool := requireToolchain(t)
	// 一个上来就退 2、什么都不说的交互器。
	broken := `#include <cstdio>
int main(){ printf("交互器自己坏了"); return 2; }`
	rep := judgeInteractive(t, tool, broken, itxGood, itxCases(), 5000)
	if rep.Verdict != IE {
		t.Fatalf("交互器退 2 = %s（%s），要 IE", rep.Verdict, rep.Detail)
	}
	if !strings.Contains(rep.Detail, "没给出判定") {
		t.Errorf("判机故障没说清是交互器没给出判定：%q", rep.Detail)
	}
}

// 时限口径是两侧之和：选手几乎不花时间也没用，交互器把预算吃光就是 TLE；而同一份对话给
// 它留出余量就该是 AC。两条一起才说清"之和"这件事，分开算等于给交互题白送一份时限。
func TestInteractiveTimeIsTheSumOfBothSides(t *testing.T) {
	tool := requireToolchain(t)
	burn := `#include <cstdio>
#include <ctime>
int main(){ clock_t s = clock(); while (clock() - s < (CLOCKS_PER_SEC / 2)) {}
  printf("5\n"); fflush(stdout); long g; if (scanf("%ld",&g)!=1) return 7;
  printf("交互器收到 %ld\n", g); fflush(stdout); return g==6?0:1; }`
	itx := burn
	// 选手本身几乎不花时间（读一个数、答一个数），慢的是交互器。
	rep := judgeInteractive(t, tool, itx, itxGood, itxCases(), 300)
	if rep.Verdict != TLE {
		t.Fatalf("交互器烧掉半秒而时限 300ms = %s（%s），要 TLE", rep.Verdict, rep.Detail)
	}
	if !strings.Contains(rep.Detail, "两侧用时之和") && !strings.Contains(rep.Detail, "墙上时钟") {
		t.Errorf("TLE 的话没说清这一条是按两侧的总用时收的：%q", rep.Detail)
	}
	if roomy := judgeInteractive(t, tool, itx, itxGood, itxCases(), 5000); roomy.Verdict != AC {
		t.Errorf("同样这份对话，时限放到 5000ms = %s（%s），要 AC", roomy.Verdict, roomy.Detail)
	}
}

// 交互题不再逐字节比输出：选手的 stdout 是对话内容，本来就不等于答案文件。
// 这条挡住一个很容易犯的错——忘了切换判定，于是把所有对话都判成 WA。
func TestInteractiveIgnoresTheByteCompare(t *testing.T) {
	tool := requireToolchain(t)
	// 对话内容（"6\n"）与答案文件（"6\n"）这里恰好相同，所以换一个明确不同的：让选手
	// 多打一句感谢的话，逐字节比必 WA，而交互器只认那个整数。
	source := `#include <cstdio>
int main(){ long n; scanf("%ld",&n); printf("%ld\n", n+1); printf("thanks\n"); fflush(stdout); return 0; }`
	rep := judgeInteractive(t, tool, itxCandidate, source, itxCases(), 5000)
	if rep.Verdict != AC {
		t.Fatalf("多打一句的对话 = %s（%s），要 AC：交互题不该按字节比", rep.Verdict, rep.Detail)
	}
}

// 走一遍 work.go 的分支表：交互器编不过时必须是判机故障，而不是把学生那份正确的代码
// 退回逐字节比较（那才是真的悄悄改了判定口径）。
func TestInteractiveUncompilableIsJudgeError(t *testing.T) {
	requireToolchain(t)
	requireRunnable(t) // 同 TestBuildAuxReportsTheAuthorsBug：要的是编译器真的开口说话
	_, err := BuildAux(Available(DefaultTools()),
		AuxProgram{Language: "cpp", Source: "int main(){ not c++ }"},
		filepath.Join(t.TempDir(), "aux"), AuxInteractor, nil)
	if err == nil || !strings.Contains(err.Error(), "交互器编不过") {
		t.Fatalf("编不过的交互器 = %v，要一句点名交互器的错误", err)
	}
}
