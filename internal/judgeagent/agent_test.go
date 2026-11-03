package judgeagent

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/api"
	"github.com/liangbinchen2013/fast-oj/internal/casefs"
	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/judge"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

const (
	sumSource    = "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b<<std::endl;return 0;}"
	offByOne     = "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b+1<<std::endl;return 0;}"
	brokenSource = "int main(){ this is not c++ }"
)

type env struct {
	srv   *httptest.Server
	db    *store.DB
	cfg   config.Config
	agent *Agent
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("g++"); err != nil {
		t.Skip("g++ not on PATH")
	}
	dir := t.TempDir()
	cfg := config.Config{
		Addr: "127.0.0.1:0", DataDir: dir, DBPath: filepath.Join(dir, "oj.db"),
		AdminToken: "admin-secret", JudgeToken: "judge-secret",
		MaxSource: 1 << 20, MaxUpload: 1 << 20, MaxCase: 1 << 20,
	}
	db, err := store.Open(context.Background(), cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	handler := api.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), db).Handler()
	srv := httptest.NewServer(handler)
	// 判机是 root 时按生产的形态把提交降权：root + 没沙盒 + 没给 -run-as 现在是**拒绝跑**
	// （不敢把 root 交给学生代码），而这些用例要测的是整条判题链路。降权之后进程还得走得
	// 进这次的工作目录，所以工作根目录放在 /tmp 下并开成 1777，而不是 t.TempDir() 那棵
	// 0700 的私有树——容器判机那份 compose 也是这么给的（tmpfs mode=1777）。
	agentDir := filepath.Join(t.TempDir(), "work")
	var runAs *judge.DropTo
	if os.Getuid() == 0 {
		root, err := os.MkdirTemp("", "fast-oj-agent-work")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
		if err := os.Chmod(root, 0o777); err != nil {
			t.Fatal(err)
		}
		agentDir = filepath.Join(root, "work")
		runAs = &judge.DropTo{UID: 65534, GID: 65534}
	}
	agent, err := New(Config{
		Server: srv.URL, Token: cfg.JudgeToken, Name: "e2e-box",
		Dir: agentDir, Logf: t.Logf, RunAs: runAs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})
	return &env{srv: srv, db: db, cfg: cfg, agent: agent}
}

// seedSubmission 写一个样例加两条计分用例，并留下一条排好队的提交，版本选择由
// options 带进去。
func (e *env) seedSubmission(t *testing.T, source, language, options string) store.Submission {
	t.Helper()
	ctx := context.Background()
	user, err := e.db.EnsureUser(ctx, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	prob, err := e.db.CreateProblem(ctx, &store.Problem{
		PID: "P1", Title: "sum", Statement: "read a b, print a+b",
		TimeLimitMS: 2000, MemLimitKB: 262144, Languages: []string{"cpp"}, Visible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := casefs.New(e.cfg.DataDir)
	specs := []struct {
		name    string
		in, out string
		sample  bool
		score   int
	}{
		{"sample1", "1 2\n", "3\n", true, 0},
		{"1", "3 4\n", "7\n", false, 40},
		{"2", "100 200\n", "300\n", false, 60},
	}
	for i, s := range specs {
		relIn, relOut, err := cases.Put(prob.ID, i+1, []byte(s.in), []byte(s.out))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.AddTestcase(ctx, &store.Testcase{
			ProblemID: prob.ID, Ordinal: i + 1, Name: s.name, IsSample: s.sample,
			Score: s.score, InputRel: relIn, OutputRel: relOut,
		}); err != nil {
			t.Fatal(err)
		}
	}
	total, err := e.db.ProblemScoreTotal(ctx, prob.ID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := e.db.CreateSubmission(ctx, &store.Submission{
		UserID: user.ID, ProblemID: prob.ID, Language: language, Options: options,
		Source: source, TotalScore: total,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// seedProblem 是不选版本的那条路：老数据与不选的人都走判机的默认口径。
func (e *env) seedProblem(t *testing.T, source, language string) store.Submission {
	t.Helper()
	return e.seedSubmission(t, source, language, "")
}

func (e *env) process(t *testing.T) (bool, error) {
	t.Helper()
	return e.agent.ProcessOnce(context.Background())
}

func TestEndToEndAccepted(t *testing.T) {
	e := newEnv(t)
	sub := e.seedProblem(t, sumSource, "cpp")

	worked, err := e.process(t)
	if err != nil || !worked {
		t.Fatalf("process = %v %v", worked, err)
	}

	row, err := e.db.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "done" || row.Verdict != "AC" {
		t.Fatalf("row = %+v", row)
	}
	if row.TotalScore != 100 || row.Score != 100 {
		t.Fatalf("score = %d/%d, want 100/100", row.Score, row.TotalScore)
	}
	if row.TimeMS < 0 {
		t.Fatalf("cpu time was not measured: %+v", row)
	}
	if row.MemKB <= 0 {
		t.Fatalf("memory was not measured: %d KB", row.MemKB)
	}
	if row.StartedAt == 0 || row.JudgedAt == 0 || row.JudgeID == 0 {
		t.Fatalf("timestamps or owner missing: %+v", row)
	}
	// 判机自报"这次实际用的口径"要落到库里：页面上那句版本说明必须是动手编这份代码的
	// 那台机器说的，而不是服务器照提交表单猜的。
	if row.Toolchain != "C++17 · O2 开" {
		t.Fatalf("自报的工具链 = %q，要 %q", row.Toolchain, "C++17 · O2 开")
	}
	runs, err := e.db.ListRuns(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %+v", runs)
	}
	for _, r := range runs {
		if r.Verdict != "AC" {
			t.Fatalf("run %s = %s", r.CaseName, r.Verdict)
		}
	}
	if !runs[0].IsSample || runs[1].IsSample {
		t.Fatalf("sample flags lost over the wire: %+v", runs)
	}
	// The judge box registered itself through the claim heartbeat.
	j, err := e.db.GetJudge(context.Background(), "e2e-box")
	if err != nil || j.LastSeenAt == 0 {
		t.Fatalf("box not registered: %+v %v", j, err)
	}
	// Work directories are per submission and removed afterwards.
	if entries, err := filepath.Glob(filepath.Join(e.agent.cfg.Dir, "*")); err != nil || len(entries) != 0 {
		t.Fatalf("work dir left behind: %v %v", entries, err)
	}
}

func TestEndToEndWrongAnswer(t *testing.T) {
	e := newEnv(t)
	sub := e.seedProblem(t, offByOne, "cpp")
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := e.db.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The server names the first failing judged case; a sample mismatch alone
	// does not become the reported case.
	if row.Verdict != "WA" || row.Score != 0 || row.FailedCase != "1" {
		t.Fatalf("row = %+v", row)
	}
	// The failing case is named and the first difference is quoted back.
	if _, detail, err := e.db.SubmissionPayload(context.Background(), sub.ID); err != nil || detail == "" {
		t.Fatalf("WA carried no diff detail: %q %v", detail, err)
	}
}

func TestEndToEndCompileError(t *testing.T) {
	e := newEnv(t)
	sub := e.seedProblem(t, brokenSource, "cpp")
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := e.db.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Verdict != "CE" || row.Score != 0 || row.FailedCase != "compile" {
		t.Fatalf("row = %+v", row)
	}
	runs, err := e.db.ListRuns(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("a build failure recorded case runs: %+v", runs)
	}
	if _, detail, err := e.db.SubmissionPayload(context.Background(), sub.ID); err != nil || detail == "" {
		t.Fatalf("compiler message was dropped: %q %v", detail, err)
	}
}

func TestEndToEndUnknownLanguageIsInfrastructureError(t *testing.T) {
	e := newEnv(t)
	sub := e.seedProblem(t, sumSource, "pascal")
	worked, err := e.process(t)
	if err != nil || !worked {
		t.Fatalf("process = %v %v", worked, err)
	}
	row, err := e.db.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The box cannot run it, so it must say so instead of leaving it claimed.
	if row.Status != "done" || row.Verdict != "IE" {
		t.Fatalf("row = %+v", row)
	}
	_, detail, err := e.db.SubmissionPayload(context.Background(), sub.ID)
	if err != nil || detail == "" || row.Score != 0 {
		t.Fatalf("IE detail missing: %q %v row=%+v", detail, err, row)
	}
}

func TestEmptyQueueIsNotAnError(t *testing.T) {
	e := newEnv(t)
	worked, err := e.process(t)
	if worked || err != nil {
		t.Fatalf("empty queue = %v %v, want false nil", worked, err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	base := Config{Server: "http://x", Token: "t", Name: "n"}
	if _, err := New(Config{Token: "t", Name: "n"}); err == nil {
		t.Error("missing server accepted")
	}
	if _, err := New(Config{Server: "http://x", Name: "n"}); err == nil {
		t.Error("missing token accepted")
	}
	if _, err := New(Config{Server: "http://x", Token: "t"}); err == nil {
		t.Error("missing name accepted")
	}
	// A box that advertises nothing it can run is refused up front.
	if _, err := New(Config{Server: base.Server, Token: base.Token, Name: base.Name,
		Tools: map[string]judge.Tool{}}); err == nil {
		t.Error("a box with no usable toolchain was accepted")
	}
}

// 版本选择必须一路走到命令行：这一环在半路被丢掉，页面上写的"C++14"就成了谎话，
// 而出争议时唯一能证明当时用了什么的证据就是判机实际跑的命令行。
// 用泛型 lambda 做探针——它是 C++14 才有的东西，c++11 编不过，c++14 能编过但对不上
// 用例，于是 CE 与 WA 这两个判定正好把"版本真的换了"说清楚。
const (
	// 算对了但多打了一行废话：逐字节比是 WA，判题器只认第一个整数的话就是 AC。
	junkSource = "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b<<\" 顺手多打的\\n\";}"
	// 只比第一个整数的判题器，并留下一句评语。
	tolerantChecker = "#include <cstdio>\n" +
		"#include <cstdlib>\n" +
		"static long first(const char *p){ FILE *f=fopen(p,\"rb\"); if(!f) return -424242;" +
		" char b[512]={0}; fread(b,1,511,f); fclose(f); return strtol(b,0,10); }\n" +
		"int main(int argc, char **argv){\n" +
		"  if (argc < 4) return 3;\n" +
		"  long got = first(argv[2]), want = first(argv[3]);\n" +
		"  printf(\"判题器：选手第一个整数 %ld，答案 %ld\", got, want);\n" +
		"  return got == want ? 0 : 1;\n" +
		"}\n"
)

// 判题器必须一路从题目走到判机的命令行里。中间任何一环丢掉，用例页就写着"本题用 SPJ
// 判定"，而判机实际按逐字节比——一份按题意正确的代码会被判 WA，还看不出来为什么。
func TestEndToEndSpecialJudge(t *testing.T) {
	ctx := context.Background()

	// 不带判题器：同一份代码应当是 WA，这是后面那条 AC 的对照。
	plain := newEnv(t)
	sub := plain.seedSubmission(t, junkSource, "cpp", "")
	if _, err := plain.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := plain.db.GetSubmission(ctx, sub.ID)
	if err != nil || row.Verdict != "WA" {
		t.Fatalf("不带判题器时 = %+v, %v，要 WA", row, err)
	}

	// 挂上只认第一个整数的判题器：同一份代码就该是 AC，而判题器那句评语进详情。
	e := newEnv(t)
	sub = e.seedSubmission(t, junkSource, "cpp", "")
	if err := e.db.PutJudgeProgram(ctx, store.JudgeProgram{
		ProblemID: sub.ProblemID, Kind: store.ProgramChecker, Language: "cpp", Source: tolerantChecker,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err = e.db.GetSubmission(ctx, sub.ID)
	if err != nil || row.Verdict != "AC" {
		t.Fatalf("带上判题器后 = %+v, %v，要 AC", row, err)
	}
	if row.Score != row.TotalScore || row.TotalScore == 0 {
		t.Errorf("判题器说对却没给满 %d/%d", row.Score, row.TotalScore)
	}
	if _, detail, err := e.db.SubmissionPayload(ctx, sub.ID); err != nil || detail != "" {
		t.Errorf("全过的提交不该留失败详情：%q %v", detail, err)
	}

	// 答案本身不对时，判题器的话要成为详情里的评语：学生看得出差在哪，才不会提工单问
	// "我为什么是 WA"。
	e = newEnv(t)
	sub = e.seedSubmission(t, offByOne, "cpp", "")
	if err := e.db.PutJudgeProgram(ctx, store.JudgeProgram{
		ProblemID: sub.ProblemID, Kind: store.ProgramChecker, Language: "cpp", Source: tolerantChecker,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err = e.db.GetSubmission(ctx, sub.ID)
	if err != nil || row.Verdict != "WA" {
		t.Fatalf("算错的提交带判题器 = %+v, %v，要 WA", row, err)
	}
	if _, detail, err := e.db.SubmissionPayload(ctx, sub.ID); err != nil ||
		!strings.Contains(detail, "判题器：选手第一个整数") {
		t.Fatalf("判题器的评语没进详情：%q %v", detail, err)
	}

	// 判题器自己写坏了（这里让它缺文件就读、退 3）：必须是判机故障，并且把原因带回来。
	// 把作者的 bug 判成学生的 CE/WA，是最难查的那种冤枉。
	e = newEnv(t)
	sub = e.seedSubmission(t, sumSource, "cpp", "")
	if err := e.db.PutJudgeProgram(ctx, store.JudgeProgram{
		ProblemID: sub.ProblemID, Kind: store.ProgramChecker, Language: "cpp",
		Source: "int main(){ return 3; }",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err = e.db.GetSubmission(ctx, sub.ID)
	if err != nil || row.Verdict != "IE" {
		t.Fatalf("判题器退 3 = %+v, %v，要 IE", row, err)
	}
	if _, detail, err := e.db.SubmissionPayload(ctx, sub.ID); err != nil ||
		!strings.Contains(detail, "判题器退出码 3") {
		t.Fatalf("判机故障没说出是谁的问题：%q %v", detail, err)
	}
}

// 判题器编不过时，交出去的必须是判机故障而不是 CE：学生的代码一行都没错。
func TestEndToEndUncompilableCheckerIsJudgeError(t *testing.T) {
	e := newEnv(t)
	sub := e.seedSubmission(t, sumSource, "cpp", "")
	if err := e.db.PutJudgeProgram(context.Background(), store.JudgeProgram{
		ProblemID: sub.ProblemID, Kind: store.ProgramChecker, Language: "cpp",
		Source: "int main(){ this is not c++ }",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := e.db.GetSubmission(context.Background(), sub.ID)
	if err != nil || row.Verdict != "IE" {
		t.Fatalf("判题器编不过 = %+v, %v，要 IE 而不是 CE", row, err)
	}
	if _, detail, err := e.db.SubmissionPayload(context.Background(), sub.ID); err != nil ||
		!strings.Contains(detail, "判题器编不过") {
		t.Fatalf("详情没点名是判题器的问题：%q %v", detail, err)
	}
}

func TestEndToEndVersionOptionsReachTheCompiler(t *testing.T) {
	const probe = "#include <iostream>\n" +
		"int main(){ auto f=[](auto x){return x*2;}; std::cout<<f(21)<<std::endl; }"
	cases := []struct {
		options string
		want    string
	}{
		{"std=c++11", "CE"},
		{"std=c++14", "WA"},
		{"", "WA"}, // 默认 c++17，也编得过
	}
	for _, c := range cases {
		e := newEnv(t)
		sub := e.seedSubmission(t, probe, "cpp", c.options)
		if _, err := e.process(t); err != nil {
			t.Fatalf("%q: %v", c.options, err)
		}
		row, err := e.db.GetSubmission(context.Background(), sub.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Verdict != c.want {
			t.Errorf("选 %q 判成 %s（%s），要 %s", c.options, row.Verdict, row.Detail, c.want)
		}
	}
}

// 判机不认识的值不能"降级成默认口径"继续编：那等于用另一条命令行判了这份代码，
// 却把提交时选的版本留在页面上。报 IE 并把拒绝的话交回去才是能查的那种失败。
func TestEndToEndRefusesAnOptionTheBoxDoesNotKnow(t *testing.T) {
	e := newEnv(t)
	sub := e.seedSubmission(t, sumSource, "cpp", "std=c++98")
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := e.db.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Verdict != "IE" {
		t.Fatalf("row = %+v，要 IE", row)
	}
	if _, detail, err := e.db.SubmissionPayload(context.Background(), sub.ID); err != nil ||
		!strings.Contains(detail, "不支持") {
		t.Fatalf("拒绝的话没交回去：%q %v", detail, err)
	}
}

// 判题器与交互器同时配上时，判机必须当面拒绝，不能"自己挑一个用"：那等于悄悄换了判定
// 口径，而出题人以为两层都在生效。
func TestEndToEndRejectsBothPrograms(t *testing.T) {
	e := newEnv(t)
	sub := e.seedSubmission(t, sumSource, "cpp", "")
	ctx := context.Background()
	for _, kind := range []string{store.ProgramChecker, store.ProgramInteractor} {
		if err := e.db.PutJudgeProgram(ctx, store.JudgeProgram{
			ProblemID: sub.ProblemID, Kind: kind, Language: "cpp", Source: tolerantChecker,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := e.db.GetSubmission(ctx, sub.ID)
	if err != nil || row.Verdict != "IE" {
		t.Fatalf("两个都配了 = %+v, %v，要 IE", row, err)
	}
	if _, detail, err := e.db.SubmissionPayload(ctx, sub.ID); err != nil ||
		!strings.Contains(detail, "只能留一个") {
		t.Fatalf("拒绝的话没说清只能留一个：%q %v", detail, err)
	}
}

// 没有沙盒的判机不收 Python。护栏画的那条线要靠 chroot 才成立：在那种机器上跑解释器，
// 就是让一份 Python 与判机同一个身份、同一个文件系统。"跑了但没隔离"是最不能悄悄发生的
// 一种失败，所以这里交的是判机故障加一句原因。
func TestEndToEndRefusesPythonWithoutSandbox(t *testing.T) {
	e := newEnv(t)
	sub := e.seedSubmission(t, "print(3)", "python", "")
	ctx := context.Background()
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := e.db.GetSubmission(ctx, sub.ID)
	if err != nil || row.Verdict != "IE" {
		t.Fatalf("没沙盒却收了 Python = %+v %v，要 IE", row, err)
	}
	if _, detail, err := e.db.SubmissionPayload(ctx, sub.ID); err != nil ||
		!strings.Contains(detail, "不收 Python") {
		t.Fatalf("拒绝的话没说是因为没沙盒：%q %v", detail, err)
	}
}

// 判题器是 Python 写的也算"要跑一份 Python"：闸门问的是这台机器会不会把 Python 关在
// 牢房外执行，学生那份、判题器那份、交互器那份是同一个答案。
func TestEndToEndRefusesPythonCheckerWithoutSandbox(t *testing.T) {
	e := newEnv(t)
	sub := e.seedSubmission(t, sumSource, "cpp", "")
	ctx := context.Background()
	if err := e.db.PutJudgeProgram(ctx, store.JudgeProgram{
		ProblemID: sub.ProblemID, Kind: store.ProgramChecker, Language: "python",
		Source: "import sys\nsys.exit(0)",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.process(t); err != nil {
		t.Fatal(err)
	}
	row, err := e.db.GetSubmission(ctx, sub.ID)
	if err != nil || row.Verdict != "IE" {
		t.Fatalf("Python 判题器在没有沙盒时被跑了 = %+v %v", row, err)
	}
}

// 启动自测没过就把这门语言从名单上摘掉，并把原因挂在 /judges 那句备注上：站点从此
// 不会摆出这个选项，操作者也看得见为什么少了一门。
func TestDisableLanguageDropsItAndSaysWhy(t *testing.T) {
	a, err := New(Config{Server: "http://127.0.0.1:1", Token: "t", Name: "box-x"})
	if err != nil {
		t.Fatal(err)
	}
	a.SetNote("沙盒未启用")
	a.DisableLanguage("python", "这台机器上没有 python3")
	for _, l := range a.Languages() {
		if l == "python" {
			t.Fatalf("名单里还留着 python：%v", a.Languages())
		}
	}
	if _, ok := a.cfg.Tools["python"]; ok {
		t.Error("工具表里还留着 python，下一次领单还是会去跑它")
	}
	if g := a.greeting(); !strings.Contains(g.Note, "python 不可用：这台机器上没有 python3") ||
		!strings.Contains(g.Note, "沙盒未启用") {
		t.Errorf("备注没把原因挂上去：%q", g.Note)
	}
}
