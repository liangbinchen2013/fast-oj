package judge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireToolchain(t *testing.T) Tool {
	t.Helper()
	tools := Available(DefaultTools())
	tool, ok := tools["cpp"]
	if !ok {
		t.Skip("g++ not on PATH")
	}
	if _, err := exec.LookPath("g++"); err != nil {
		t.Skip("g++ not runnable")
	}
	return tool
}

func judgeSource(t *testing.T, tool Tool, source string, cases []Case, timeMS, memKB int) Report {
	t.Helper()
	return judgeSourceAs(t, tool, source, cases, timeMS, memKB, nil)
}

func judgeSourceAs(t *testing.T, tool Tool, source string, cases []Case, timeMS, memKB int, drop *DropTo) Report {
	t.Helper()
	requireRunnable(t)
	task := Task{
		SubmissionID: 1, PID: "P1", Language: "cpp", Source: source,
		TimeLimitMS: timeMS, MemLimitKB: memKB, Cases: cases,
	}
	return Judge(task, tool, filepath.Join(t.TempDir(), "work"), drop, nil)
}

func sumCases() []Case {
	return []Case{
		{Name: "sample1", Ordinal: 1, IsSample: true, Input: []byte("1 2\n"), Expected: []byte("3\n")},
		{Name: "1", Ordinal: 2, Score: 40, Input: []byte("3 4\n"), Expected: []byte("7\n")},
		{Name: "2", Ordinal: 3, Score: 60, Input: []byte("100 200\n"), Expected: []byte("300")},
	}
}

const sumSource = `#include <iostream>
int main(){int a,b;std::cin>>a>>b;std::cout<<a+b<<std::endl;return 0;}`

func TestJudgeAcceptsCorrectProgram(t *testing.T) {
	tool := requireToolchain(t)
	rep := judgeSource(t, tool, sumSource, sumCases(), 2000, 262144)

	if rep.Verdict != AC {
		t.Fatalf("verdict = %s (%s), want AC", rep.Verdict, rep.Detail)
	}
	if rep.Score != 100 || rep.TotalScore != 100 {
		t.Fatalf("score = %d/%d, want 100/100", rep.Score, rep.TotalScore)
	}
	if len(rep.Runs) != 3 {
		t.Fatalf("runs = %+v", rep.Runs)
	}
	if rep.FailedCase != "" || rep.Detail != "" {
		t.Fatalf("clean AC carried failure text: %q %q", rep.FailedCase, rep.Detail)
	}
	// The job object must report real usage, not placeholders.
	if rep.TimeMS < 0 {
		t.Fatalf("cpu time not measured: %+v", rep)
	}
	if rep.MemKB <= 0 {
		t.Fatalf("memory not measured: %d KB", rep.MemKB)
	}
	if !rep.Runs[0].IsSample || rep.Runs[1].IsSample {
		t.Fatalf("sample flags lost: %+v", rep.Runs)
	}
}

// The expected answer for case 2 has no trailing newline, so a normalising
// comparison is what keeps this AC.
func TestOutputComparisonIgnoresTrailingNoise(t *testing.T) {
	if !OutputsMatch([]byte("300\r\n\n\n"), []byte("300")) {
		t.Fatal("trailing blank lines should not matter")
	}
	if !OutputsMatch([]byte("7  \n"), []byte("7")) {
		t.Fatal("trailing spaces should not matter")
	}
	if OutputsMatch([]byte("7\n8\n"), []byte("7\n9\n")) {
		t.Fatal("different content matched")
	}
	if OutputsMatch([]byte(""), []byte("3")) {
		t.Fatal("empty output matched expected text")
	}
	if !OutputsMatch([]byte("\n\n"), []byte("")) {
		t.Fatal("whitespace-only output should match an empty answer")
	}
}

func TestJudgeDetectsWrongAnswer(t *testing.T) {
	tool := requireToolchain(t)
	cases := sumCases()
	cases[1].Expected = []byte("8\n")
	rep := judgeSource(t, tool, sumSource, cases, 2000, 262144)

	if rep.Verdict != WA {
		t.Fatalf("verdict = %s, want WA", rep.Verdict)
	}
	if rep.FailedCase != "1" {
		t.Fatalf("failed case = %q, want the first judged failure", rep.FailedCase)
	}
	if rep.Score != 60 {
		t.Fatalf("score = %d, want the 60 points still earned", rep.Score)
	}
	if !strings.Contains(rep.Detail, "line 1") || !strings.Contains(rep.Detail, "7") || !strings.Contains(rep.Detail, "8") {
		t.Fatalf("diff excerpt = %q", rep.Detail)
	}
	if rep.Runs[1].Verdict != WA || rep.Runs[2].Verdict != AC {
		t.Fatalf("per-case rows = %+v", rep.Runs)
	}
}

// The submission page fills in as cases finish, which only works if the judge
// hands each row over the moment it exists, in case order.
func TestProgressReportsEachCaseAsItFinishes(t *testing.T) {
	tool := requireToolchain(t)
	requireRunnable(t)
	var seen []RunRow
	rep := Judge(Task{SubmissionID: 1, PID: "P1", Language: "cpp", Source: sumSource,
		TimeLimitMS: 2000, MemLimitKB: 262144, Cases: sumCases()},
		tool, filepath.Join(t.TempDir(), "work"), nil, func(row RunRow) { seen = append(seen, row) })

	if len(seen) != len(rep.Runs) {
		t.Fatalf("progress fired %d times for %d cases", len(seen), len(rep.Runs))
	}
	for i := range seen {
		if seen[i].Ordinal != i+1 {
			t.Fatalf("row %d arrived with ordinal %d", i, seen[i].Ordinal)
		}
		if seen[i].CaseName != rep.Runs[i].CaseName || seen[i].Verdict != rep.Runs[i].Verdict {
			t.Errorf("row %d = %+v, want %+v", i, seen[i], rep.Runs[i])
		}
	}
}

func TestJudgeDetectsCompileError(t *testing.T) {
	tool := requireToolchain(t)
	rep := judgeSource(t, tool, "int main(){ this is not c++", sumCases(), 2000, 262144)
	if rep.Verdict != CE {
		t.Fatalf("verdict = %s, want CE", rep.Verdict)
	}
	if rep.FailedCase != "compile" {
		t.Fatalf("failed = %q", rep.FailedCase)
	}
	if !strings.Contains(rep.Detail, "error") {
		t.Fatalf("compiler message missing: %q", rep.Detail)
	}
	if len(rep.Runs) != 0 {
		t.Fatalf("a build failure must not run cases: %+v", rep.Runs)
	}
}

func TestJudgeDetectsRuntimeError(t *testing.T) {
	tool := requireToolchain(t)
	rep := judgeSource(t, tool, "int main(){return 3;}", sumCases(), 2000, 262144)
	if rep.Verdict != RE {
		t.Fatalf("verdict = %s, want RE", rep.Verdict)
	}
	if rep.Runs[0].Verdict != RE {
		t.Fatalf("rows = %+v", rep.Runs)
	}
}

func TestJudgeTimesOut(t *testing.T) {
	tool := requireToolchain(t)
	// volatile keeps -O2 from deleting the loop.
	rep := judgeSource(t, tool, "int main(){volatile int x=0;while(1){x++;}return 0;}",
		sumCases(), 300, 262144)
	if rep.Verdict != TLE {
		t.Fatalf("verdict = %s, want TLE", rep.Verdict)
	}
	if rep.FailedCase != "sample1" {
		t.Fatalf("failed case = %q", rep.FailedCase)
	}
	if rep.Score != 0 {
		t.Fatalf("a timeout scored %d points", rep.Score)
	}
	// The run was cut off mid-flight, so its time is a lower bound and has to be
	// labelled one: ">=300ms" is what actually happened.
	if !rep.TimeMin {
		t.Error("a killed run reported its time as an exact measurement")
	}
	if rep.TimeMS < 300 {
		t.Errorf("killed run reported %dms, below the 300ms limit it was cut at", rep.TimeMS)
	}
}

func TestJudgeRefusesRunawayMemory(t *testing.T) {
	tool := requireToolchain(t)
	// The printf keeps -O2 from eliding an unused allocation, so the program
	// really does ask for 512 MB against a 32 MB cap.
	src := `#include <cstring>
	#include <cstdio>
	#include <cstdlib>
	int main(){
		volatile char *p = (char*)malloc(512u*1024u*1024u);
		printf("%p\n", (void*)p);
		if(!p) return 7;
		std::memset((void*)p, 1, 512u*1024u*1024u);
		return 0;
	}`
	rep := judgeSource(t, tool, src, []Case{{Name: "1", Ordinal: 1, Score: 10,
		Input: []byte("\n"), Expected: []byte("0\n")}}, 5000, 32_768)

	// A Windows job refuses the allocation instead of killing the process, so
	// this arrives as RE; the Linux sandbox path reports MLE. What matters is
	// that the cap held, the box was not drained, and nothing scored.
	switch rep.Verdict {
	case MLE, RE:
	default:
		t.Fatalf("verdict = %s (%s), want MLE or RE", rep.Verdict, rep.Detail)
	}
	if rep.Score != 0 || rep.Runs[0].Verdict == AC {
		t.Fatalf("runaway memory scored: %+v", rep)
	}
	if rep.MemKB < 0 {
		t.Fatalf("job did not measure memory: %+v", rep)
	}
}

func TestJudgeWithoutCasesIsInfrastructureError(t *testing.T) {
	tool := requireToolchain(t)
	rep := judgeSource(t, tool, sumSource, nil, 1000, 65536)
	if rep.Verdict != IE || !strings.Contains(rep.Detail, "no test cases") {
		t.Fatalf("verdict = %s %q", rep.Verdict, rep.Detail)
	}
}

func TestRunReportsMissingBinaryAsIE(t *testing.T) {
	ex := Run([]string{"definitely-not-a-real-binary-xyz"}, t.TempDir(), nil, Limits{TimeMS: 100})
	if ex.Verdict != IE {
		t.Fatalf("verdict = %s, want IE", ex.Verdict)
	}
	ex = Run(nil, t.TempDir(), nil, Limits{TimeMS: 100})
	if ex.Verdict != IE {
		t.Fatalf("empty argv = %s", ex.Verdict)
	}
}

func TestCompileArgsPointIntoWorkDir(t *testing.T) {
	tool := DefaultTools()["cpp"]
	args := tool.CompileArgs(filepath.FromSlash("/tmp/w"))
	src := args[len(args)-1]
	if filepath.Base(src) != "main.cpp" {
		t.Fatalf("source arg = %s", src)
	}
	if !strings.Contains(strings.Join(args, " "), "-static") {
		t.Fatalf("static link flag missing: %v", args)
	}
	if !tool.Compiles() {
		t.Fatal("cpp must require a compile step")
	}
	runArgs := tool.RunArgs(filepath.FromSlash("/tmp/w"))
	if len(runArgs) != 1 || !strings.HasPrefix(runArgs[0], filepath.FromSlash("/tmp/w")) {
		t.Fatalf("run argv = %v", runArgs)
	}
	if Available(map[string]Tool{"nope": {Language: "nope", SourceFile: "x.nope",
		Compile: []string{"this-compiler-does-not-exist"}}})["nope"].Language != "" {
		t.Fatal("unavailable tool must be dropped")
	}
}

func TestSeverityPrefersTheLoudestFailure(t *testing.T) {
	if severity(AC) >= severity(WA) || severity(WA) >= severity(RE) ||
		severity(RE) >= severity(MLE) || severity(MLE) >= severity(TLE) {
		t.Fatal("verdict severity is out of order")
	}
	if severity(IE) <= severity(TLE) {
		t.Fatal("an infrastructure fault must outrank a timeout")
	}
}

func TestClipTextBoundsJudgeDetail(t *testing.T) {
	long := strings.Repeat("x", maxDetail+100)
	clipped := clipText(long, maxDetail)
	if len(clipped) > len(long) || !strings.HasSuffix(clipped, "clipped]") {
		t.Fatalf("clip = %d bytes %q", len(clipped), clipped[len(clipped)-20:])
	}
	if clipText("short", maxDetail) != "short" {
		t.Fatal("short text was altered")
	}
}

func TestWorkDirFailureIsInfrastructureError(t *testing.T) {
	tool := requireToolchain(t)
	// A regular file in the place of a parent directory cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := Judge(Task{PID: "P", Language: "cpp", Source: "int main(){}",
		Cases: []Case{{Name: "1", Input: []byte("a"), Expected: []byte("b")}}},
		tool, filepath.Join(blocker, "work"), nil, nil)
	if rep.Verdict != IE || !strings.Contains(rep.Detail, "workdir") {
		t.Fatalf("verdict = %s %q, want IE from the workdir failure", rep.Verdict, rep.Detail)
	}
}

// 判机上没有编译器（systemd 的 PATH 里没有 g++、容器里没装工具链）时，Run 是提前
// 返回的，Exit 仍然是 0。以前只盯 Exit 的话，这一步会被当成"编译成功"，然后去跑一个
// 根本没生成的二进制，最后交出一条 detail 全空的 IE——真正的原因谁也没看到。
func TestJudgeMissingCompilerReportsTheReason(t *testing.T) {
	missing := "definitely-not-a-real-compiler-xyz"
	tool := Tool{
		Language:   "cpp",
		SourceFile: "main.cpp",
		Compile:    []string{missing, "-O2", "-o", "{exe}", "{src}"},
		Run:        []string{"{exe}"},
	}
	rep := judgeSource(t, tool, sumSource, sumCases(), 2000, 262144)
	if rep.Verdict != IE {
		t.Fatalf("verdict = %s, want IE", rep.Verdict)
	}
	if !strings.Contains(rep.Detail, missing) {
		t.Errorf("IE 必须说清是编译步骤找不到东西，实际 detail = %q", rep.Detail)
	}
	if rep.FailedCase != "compile" {
		t.Errorf("failed = %q, want compile", rep.FailedCase)
	}
	if len(rep.Runs) != 0 {
		t.Errorf("编译都没起来就不该跑用例：%+v", rep.Runs)
	}
}

// 跑不起来的用例（可执行文件不在、沙盒没建起来）不留 stderr，原因只在 Note 里。
func TestJudgeRunFailureWithoutStderrKeepsTheNote(t *testing.T) {
	tool := Tool{
		Language:   "cpp",
		SourceFile: "main.cpp",
		Run:        []string{"./never-built-program"},
	}
	rep := judgeSource(t, tool, sumSource, sumCases(), 2000, 262144)
	if rep.Verdict != IE {
		t.Fatalf("verdict = %s, want IE", rep.Verdict)
	}
	if strings.TrimSpace(rep.Detail) == "" {
		t.Fatal("detail 又空了：判机故障必须自己说清自己")
	}
	if !strings.Contains(rep.Detail, "never-built-program") {
		t.Errorf("detail = %q，应当点出跑不起来的是什么", rep.Detail)
	}
}
