package judge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 判题器约定：argv 依次是 输入文件、选手输出文件、答案文件，退出码 0=对、1=错、其他=判机故障。
// 这些测试用真的 g++ 现编一个判题器跑，因为"退出码换判定"这件事最容易在假对象上蒙混过关。
const checkerFmt = `#include <cstdio>
static void slurp(const char *p, char *buf){
  buf[0]=0; if(!p) return;
  FILE *f = fopen(p, "rb"); if(!f) return;
  fread(buf, 1, 255, f); fclose(f);
}
int main(int argc, char **argv){
  char inf[256], ouf[256], ans[256];
  slurp(argc>1?argv[1]:0, inf); slurp(argc>2?argv[2]:0, ouf); slurp(argc>3?argv[3]:0, ans);
  printf("inf=[%s] ouf=[%s] ans=[%s]", inf, ouf, ans);
  if (EXITCODE == 99) { *(volatile int*)0 = 1; }
  return EXITCODE;
}`

func checker(code string) string {
	return strings.ReplaceAll(checkerFmt, "EXITCODE", code)
}

// buildChecker 把判题器编进 root。**必须编在本次提交的工作目录里**：编到外面时开了沙盒
// 之后它在牢房里看不见自己的可执行文件，判机故障会看起来像沙盒坏了。
func buildChecker(t *testing.T, root, body string) []string {
	t.Helper()
	argv, err := BuildAux(Available(DefaultTools()), AuxProgram{Language: "cpp", Source: body},
		root, AuxChecker, nil)
	if err != nil {
		t.Fatalf("编判题器失败：%v", err)
	}
	return argv
}

// judgeWithSPJ 按生产的样子跑一次判题：工作目录先建好，判题器编在它里面。
func judgeWithSPJ(t *testing.T, tool Tool, checkerSource, source string, cases []Case) Report {
	t.Helper()
	requireRunnable(t)
	dir := filepath.Join(t.TempDir(), "work")
	return Judge(Task{
		SubmissionID: 1, PID: "P1", Language: "cpp", Source: source,
		TimeLimitMS: 2000, MemLimitKB: 262144,
		SPJ: buildChecker(t, dir, checkerSource), Cases: cases,
	}, tool, dir, nil, nil)
}

const messySum = `#include <iostream>
int main(){int a,b;std::cin>>a>>b;std::cout<<a+b<<" extra junk\n";}`

var oneSumCase = []Case{
	{Name: "1", Ordinal: 1, Score: 100, Input: []byte("1 2\n"), Expected: []byte("3\n")},
}

// SPJ 存在的意义就是"输出与答案文件不一样也可以是正确的"。判题器说对，就得是对。
func TestSPJReplacesTheByteCompare(t *testing.T) {
	tool := requireToolchain(t)
	if rep := judgeSource(t, tool, messySum, oneSumCase, 2000, 262144); rep.Verdict != WA {
		t.Fatalf("不带判题器时多打了字应当 WA，实际 %s：%s", rep.Verdict, rep.Detail)
	}
	rep := judgeWithSPJ(t, tool, checker("0"), messySum, oneSumCase)
	if rep.Verdict != AC || rep.Score != 100 {
		t.Errorf("判题器说对之后 = %s %d/%d（%s），要 AC 100/100",
			rep.Verdict, rep.Score, rep.TotalScore, rep.Detail)
	}
}

// 退出码 1 是 wrong answer，而判题器打印的那句话要成为详情里的评语：学生看得出差在哪，
// 才不会提一张"我为什么错了"的工单。
func TestSPJVerdictCodeAndComment(t *testing.T) {
	tool := requireToolchain(t)
	source := `#include <cstdio>
int main(){ printf("42\n"); }`
	cases := []Case{{Name: "1", Ordinal: 1, Score: 50, Input: []byte("x\n"), Expected: []byte("42\n")}}

	rep := judgeWithSPJ(t, tool, checker("1"), source, cases)
	if rep.Verdict != WA || rep.FailedCase != "1" {
		t.Fatalf("判题器退 1 判成 %s（%s），要 WA 且指名这条用例", rep.Verdict, rep.Detail)
	}
	if !strings.Contains(rep.Detail, "ouf=[42") {
		t.Errorf("判题器的评语没进详情：%q", rep.Detail)
	}
}

// 判题器自己出问题——退出码既不是 0 也不是 1，或者干脆被信号打死——必须是判机故障。
// 算成 WA 会让学生去改一份也许正确的代码，算成 AC 更是放过所有错误答案。
func TestSPJBrokenCheckerIsJudgeError(t *testing.T) {
	tool := requireToolchain(t)
	source := `#include <cstdio>
int main(){ printf("1\n"); }`
	cases := []Case{{Name: "1", Ordinal: 1, Score: 10, Input: []byte("i\n"), Expected: []byte("1\n")}}

	for _, code := range []string{"2", "70", "99"} {
		rep := judgeWithSPJ(t, tool, checker(code), source, cases)
		if rep.Verdict != IE {
			t.Errorf("判题器退出码 %s 判成 %s（%s），要 IE", code, rep.Verdict, rep.Detail)
			continue
		}
		if rep.Detail == "" {
			t.Errorf("退出码 %s 的 IE 没留下任何原因，这种工单查不下去", code)
		}
		if rep.Score != 0 {
			t.Errorf("判机故障还给了 %d 分", rep.Score)
		}
	}
}

// argv 的顺序是判题器与判机之间唯一的约定，弄反了会把答案当输入用：每一道题都判错，
// 而且错得很安静。
func TestSPJPassesInputOutputAnswerInThatOrder(t *testing.T) {
	requireToolchain(t)
	requireRunnable(t)
	dir := t.TempDir()
	argv := buildChecker(t, dir, checker("0"))
	for name, body := range map[string]string{
		"a.in": "IN\n", "b.out": "OUT\n", "c.ans": "ANS\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex := Run(append(append([]string{}, argv...),
		filepath.Join(dir, "a.in"), filepath.Join(dir, "b.out"), filepath.Join(dir, "c.ans")),
		dir, nil, Limits{TimeMS: 5000, MemKB: 65536})
	out := string(ex.Stdout)
	if !strings.Contains(out, "inf=[IN") || !strings.Contains(out, "ouf=[OUT") || !strings.Contains(out, "ans=[ANS") {
		t.Errorf("判题器读到的三个文件不对：%q", out)
	}
}

// 判题器编不过时 BuildAux 要把编译输出带回来，工单交出去的是判机故障而不是 CE：学生
// 一行代码都没错，让他吃一个 Compile Error 是最难查的冤枉。
func TestBuildAuxReportsTheAuthorsBug(t *testing.T) {
	requireToolchain(t)
	tools := Available(DefaultTools())
	_, err := BuildAux(tools, AuxProgram{Language: "cpp", Source: "int main(){ this is not c++ }"},
		filepath.Join(t.TempDir(), "aux3"), AuxChecker, nil)
	if err == nil {
		t.Fatal("编不过的判题器竟然返回了成功")
	}
	if !strings.Contains(err.Error(), "判题器编不过") {
		t.Errorf("错误的话没点名是判题器：%q", err.Error())
	}
	if _, err := BuildAux(tools, AuxProgram{Language: "pas", Source: "x"},
		filepath.Join(t.TempDir(), "aux4"), AuxChecker, nil); err == nil ||
		!strings.Contains(err.Error(), "工具链") {
		t.Errorf("没有的工具链应当直说：%v", err)
	}
}

// 判题器慢，学生控制不了它，所以判题器的 CPU 时间不能算进学生头上：算进去的话，一个
// 一百二十行的判题器能把整道题的时限吃光。
func TestSPJTimeIsNotChargedToTheStudent(t *testing.T) {
	tool := requireToolchain(t)
	source := `#include <cstdio>
int main(){ printf("1\n"); }`
	cases := []Case{{Name: "1", Ordinal: 1, Score: 10, Input: []byte("i\n"), Expected: []byte("1\n")}}
	burn := `#include <cstdio>
#include <ctime>
int main(){
  clock_t start = clock();
  while (clock() - start < CLOCKS_PER_SEC / 2) {}
  return 0;
}`
	rep := judgeWithSPJ(t, tool, burn, source, cases)
	if rep.Verdict != AC {
		t.Fatalf("慢判题器把判定改成了 %s：%s", rep.Verdict, rep.Detail)
	}
	if rep.TimeMS > 1000 {
		t.Errorf("报告用时 %dms，判题器烧掉的半秒被算进学生头上了", rep.TimeMS)
	}
}

// 判题器不能把别的题目的文件读进来当答案：三个文件写在**这次提交的工作目录**里，
// 换一道题就是另一个目录。
func TestSPJFilesStayInTheSubmissionWorkdir(t *testing.T) {
	tool := requireToolchain(t)
	source := `#include <cstdio>
int main(){ printf("1\n"); }`
	cases := []Case{{Name: "1", Ordinal: 1, IsSample: true, Input: []byte("i\n"), Expected: []byte("1\n")}}
	rep := judgeWithSPJ(t, tool, checker("0"), source, cases)
	if rep.Verdict != AC {
		t.Fatalf("样例也要走判题器：%s %s", rep.Verdict, rep.Detail)
	}
}
