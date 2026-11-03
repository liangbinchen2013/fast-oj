package judge

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Case is one test case with its data already fetched. 判定与回报都按 Ordinal 走，
// 这里没有数据库 id：判机拿到那份 id 也没有能用的地方，它只会多一个枚举别题的把手。
type Case struct {
	Name     string
	Ordinal  int
	IsSample bool
	Score    int
	Input    []byte
	Expected []byte
}

// Task is the work order for one submission.
type Task struct {
	SubmissionID int64
	PID          string
	Language     string
	Source       string
	TimeLimitMS  int
	MemLimitKB   int
	// SPJ 是题目自带、已经编好的判题器命令行；空表示按标准输出与答案宽松比。
	// 命令行而不是路径：解释型判题器要 "<解释器> <源码>" 两段。
	SPJ []string
	// Interactor 是题目自带、已经编好的交互器命令行。交互题由它判定：这时候既不比字节，
	// 也不再跑判题器（两个同时配上会被判机拒绝，见 judgeagent）。
	Interactor []string
	Cases      []Case
}

type RunRow struct {
	Ordinal  int     `json:"ordinal"`
	CaseName string  `json:"case_name"`
	Verdict  Verdict `json:"verdict"`
	TimeMS   int     `json:"time_ms"`
	MemKB    int     `json:"mem_kb"`
	IsSample bool    `json:"is_sample"`
	// TimeMin and MemMin mark numbers that are lower bounds: the run was killed
	// before it finished, so the views print them with a leading ">".
	TimeMin bool `json:"time_min,omitempty"`
	MemMin  bool `json:"mem_min,omitempty"`
}

type Report struct {
	Verdict    Verdict
	TimeMS     int
	MemKB      int
	TimeMin    bool
	MemMin     bool
	FailedCase string
	Detail     string
	Runs       []RunRow
	Score      int
	TotalScore int
	// Toolchain 是这台判机自报"这次实际用的口径"（"C++14 · O2 关"、"python 3.14"）。
	// 由判机回报而不是服务器按提交时的选择写：选了 c++14 却被一台旧判机按 c++17 编了，
	// 差别只有动手编的那台机器知道。
	Toolchain string
}

// CompileLimits are deliberately loose: a slow link step is not the submitter's
// fault. The compiler is not the untrusted input here, but it still runs under a
// scrubbed environment and a wide process cap rather than unsandboxed, since it is
// the same execution path. TimeMS is a wall budget (limit + 50ms slack), so it
// carries the multi-process compiler's startup overhead rather than one CPU bound.
var CompileLimits = Limits{TimeMS: 120_000, MemKB: 2 << 20, MaxProcs: 16}

// maxDetail keeps judge-side text out of the database from growing unbounded.
const maxDetail = 4 << 10

// OutputsMatch compares program output the way an OJ is expected to: trailing
// whitespace on each line and trailing blank lines do not matter.
func OutputsMatch(got, want []byte) bool {
	return normalizeOutput(got) == normalizeOutput(want)
}

func normalizeOutput(body []byte) string {
	body = bytes.TrimRight(body, " \t\r\n")
	if len(body) == 0 {
		return ""
	}
	lines := strings.Split(string(body), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.Join(lines, "\n")
}

// Progress receives each case row as soon as that case has been judged, so the
// caller can publish partial results while the rest is still running.
type Progress func(row RunRow)

// Judge builds and runs one submission over all of its cases. It never talks to
// the network: the caller has already fetched the case data. drop is the uid a
// submission is run as, when the judge has one to give it; onProgress may be nil.
func Judge(task Task, tool Tool, dir string, drop *DropTo, onProgress Progress) Report {
	rep := Report{Verdict: IE, TimeMS: -1, MemKB: -1, Toolchain: tool.ToolchainLabel()}
	if len(task.Cases) == 0 {
		rep.Detail = "problem " + task.PID + " has no test cases"
		return rep
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		rep.Detail = "workdir: " + err.Error()
		return rep
	}
	if !Jailed() && drop != nil {
		// 牢房在建的时候会把这个目录开成 0777（jail.Enter 里那一步），因为它随后要降权。
		// 没牢房却照样降权时同理：setuid 之后的进程连自己的目录都进不去，每条提交只会撞
		// 一句 `fork/exec ...: permission denied`。判完就删，这里放宽的是这一次提交自己住的那层。
		// （再往上的 `-dir` 仍归部署方负责：容器把它挂在 mode=1777 的 tmpfs 上。）
		if err := os.Chmod(dir, 0o777); err != nil {
			rep.Detail = "workdir mode: " + err.Error()
			return rep
		}
	}

	source := filepath.Join(dir, tool.SourceFile)
	if err := writeRunFile(source, []byte(task.Source)); err != nil {
		rep.Detail = "write source: " + err.Error()
		return rep
	}
	// 解释型语言自带的那份护栏要在学生代码之前落到同一个目录里：Run 里按 {dir} 引用它。
	if err := writeSupport(tool, dir); err != nil {
		rep.Verdict, rep.Detail = IE, err.Error()
		return rep
	}

	if tool.Compiles() {
		build := Run(tool.CompileArgs(dir), dir, nil, CompileLimits)
		if build.Exit != 0 {
			rep.Verdict = CE
			rep.Detail = clipText(string(build.Stderr), maxDetail)
			rep.FailedCase = "compile"
			return rep
		}
		if build.Verdict == TLE {
			rep.Verdict = IE
			rep.Detail = "compiler timed out"
			return rep
		}
		if build.Verdict == IE {
			// 编译器找不到时 Run 是提前返回的，Exit 仍然是 0：只盯 Exit 会把"这台判机
			// 没有 g++"当成"编译成功"，然后去跑一个根本没生成的二进制，最后交出一条
			// detail 全空的 IE，真正的原因死在这一步。判机故障必须自己说清自己。
			rep.Verdict = IE
			rep.Detail = clipText(orText(build.Note, "编译步骤失败了，但没有给出原因"), maxDetail)
			rep.FailedCase = "compile"
			return rep
		}
	}

	runLimits := Limits{TimeMS: task.TimeLimitMS, MemKB: task.MemLimitKB, RunAs: drop}
	argv := tool.RunArgs(dir)
	if !tool.Compiles() && !strings.Contains(strings.Join(tool.Run, " "), "{src}") {
		// 解释器把源码接在命令行末尾；Run 里已经写了 {src} 的（Python 要在源码之前
		// 先上护栏）就不再追加，否则同一份 main.py 会被交给解释器两遍。
		argv = append(argv, source)
	}

	worst := AC
	firstFailure := -1
	for _, tc := range task.Cases {
		var ex Execution
		v := AC
		// note 是"这次为什么算这个判定"的那句话：普通题是首个不同的那一行，SPJ 题是
		// 判题器自己打印的评语，交互题是交互器打印的评语。
		note := ""
		switch {
		case len(task.Interactor) > 0:
			// 交互题：选手的 stdin/stdout 接到交互器上，判定由交互器的退出码给出。
			duo := runInteractive(task.Interactor, argv, dir, tc, runLimits)
			ex = duo.User
			// 用时按两侧之和报（口径与判定一致），内存仍报选手那一份——交互器占多少不是
			// 学生的错。
			ex.TimeMS, ex.TimeMin = duo.TimeMS, duo.User.TimeMin || duo.Interactor.TimeMin
			v, note = verdictForDuo(duo)
		default:
			ex = Run(argv, dir, tc.Input, runLimits)
			v = ex.Verdict
			if v == AC {
				switch {
				case len(task.SPJ) > 0:
					v, note = checkWithSPJ(task.SPJ, dir, tc, ex.Stdout)
				case !OutputsMatch(ex.Stdout, tc.Expected):
					v, note = WA, diffExcerpt(ex.Stdout, tc.Expected)
				}
			}
		}
		row := RunRow{Ordinal: tc.Ordinal, CaseName: tc.Name, Verdict: v,
			TimeMS: ex.TimeMS, MemKB: ex.MemKB, IsSample: tc.IsSample,
			TimeMin: ex.TimeMin, MemMin: ex.MemMin}
		rep.Runs = append(rep.Runs, row)
		if onProgress != nil {
			onProgress(row)
		}

		// The headline numbers are the worst across cases, and they inherit that
		// case's lower-bound flag: a max taken from a killed run is itself a bound.
		if ex.TimeMS > rep.TimeMS {
			rep.TimeMS, rep.TimeMin = ex.TimeMS, ex.TimeMin
		}
		if ex.MemKB > rep.MemKB {
			rep.MemKB, rep.MemMin = ex.MemKB, ex.MemMin
		}
		if v == AC {
			if !tc.IsSample {
				rep.Score += tc.Score
			}
			continue
		}
		if firstFailure < 0 {
			firstFailure = len(rep.Runs) - 1
			rep.FailedCase = tc.Name
			detail := note
			if detail == "" {
				detail = strings.TrimSpace(string(ex.Stderr))
			}
			if detail == "" {
				// 一个跑都没跑起来的失败（可执行文件不在、沙盒没建起来、fork 被拒）不会
				// 留下任何 stderr，原因只在 Note 里。丢掉它就是又一条查不出来的 IE。
				detail = strings.TrimSpace(ex.Note)
			}
			rep.Detail = clipText(detail, maxDetail)
		}
		if severity(v) > severity(worst) {
			worst = v
		}
	}
	for _, tc := range task.Cases {
		if !tc.IsSample {
			rep.TotalScore += tc.Score
		}
	}

	switch {
	case firstFailure < 0:
		rep.Verdict = AC
	case worst == AC:
		rep.Verdict = WA
	default:
		rep.Verdict = worst
	}
	return rep
}

// severity orders verdicts so the most informative one wins when several cases
// fail for different reasons.
func severity(v Verdict) int {
	switch v {
	case AC:
		return 0
	case WA:
		return 1
	case RE:
		return 2
	case MLE:
		return 3
	case TLE:
		return 4
	case CE:
		return 5
	default:
		return 6
	}
}

func diffExcerpt(got, want []byte) string {
	g := normalizeOutput(got)
	w := normalizeOutput(want)
	gl, wl := strings.Split(g, "\n"), strings.Split(w, "\n")
	for i := 0; i < len(gl) && i < len(wl); i++ {
		if gl[i] != wl[i] {
			return "line " + strconv.Itoa(i+1) + ": got [" + clipText(gl[i], 200) + "] want [" + clipText(wl[i], 200) + "]"
		}
	}
	if len(gl) != len(wl) {
		return "line count: got " + strconv.Itoa(len(gl)) + " want " + strconv.Itoa(len(wl))
	}
	return "output differs"
}

// orText keeps a failure from reporting itself as nothing.
func orText(primary, fallback string) string {
	if strings.TrimSpace(primary) == "" {
		return fallback
	}
	return primary
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[... clipped]"
}
