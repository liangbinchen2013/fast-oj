package judge

// SPJ 判题器：作者写的一份程序说了算，而不是拿标准输出逐字节比。
//
// 约定跟 Codeforces 与洛谷一致，因为现成的判题器基本都按这一套写：
//
//	argv = <判题器> <输入文件> <选手输出文件> <答案文件>
//	退出码 0 = 正确，1 = wrong answer，其他 = 判题器自己出了问题
//
// 判题器的 stdout 当作评语带回去，显示在判题详情里（受题目详情级别约束）。出题人常靠
// 这一句告诉学生"你的输出不合法"或"精度不够"，丢掉它就只剩一个干巴巴的 WA。
//
// 判题器自己的用时与内存**不算进学生的口径**：那是判机在干活，学生控制不了它，让它为
// 判题器的速度买单是冤枉的。但判题器仍然要在同一套牢房与限额里跑——它是作者写的代码，
// 权限上不比一份学生提交更高。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// auxLimits 是给判题器与交互器的预算。它与学生题目的时限无关：太紧会让一个写得慢的
// 判题器把正确答案判成判机故障，太松则没人管着作者那段在判机上跑的代码。
// 十秒与 512MB 取的是"判一份输出用得着这么多吗"和"别把机器拖死"之间的中。
//
// 这两份程序也是**别人写的代码**，所以它们跟学生提交同一个身份跑（RunAs 由调用方补上，
// 见 auxLimitsFor）：开了沙盒时牢房本来就降权，没沙盒时不补这一刀就是"作者以 root 跑"。
var auxLimits = Limits{TimeMS: 10_000, MemKB: 512 << 10, MaxProcs: 8}

// auxLimitsFor 是那两份辅助程序的预算加上"跟提交一起降权"的那份身份。
func auxLimitsFor(drop *DropTo) Limits {
	lim := auxLimits
	lim.RunAs = drop
	return lim
}

// AuxProgram 是题目自带的一份判机程序（判题器或交互器）。
type AuxProgram struct {
	Language string
	Source   string
}

// 判机自带程序的两类身份，也是它们在工作目录里的落点。
const (
	AuxChecker    = "checker"
	AuxInteractor = "interactor"
)

func auxKindName(name string) string {
	if name == AuxChecker {
		return "判题器"
	}
	return "交互器"
}

// BuildAux 在工作目录里编好一份判机自带的程序，返回跑它的命令行。
//
// 必须现编：判题器编在别处会带来另一套 libc，而它判的正是这台机器上编出来的学生程序，
// 两边连 ABI 都不是同一份时，"判题器崩了"会变成学生的锅。
//
// 返回的是 argv 而不是一个路径，因为解释型判题器的命令行是 <解释器> <源码>：拼成字符串
// 再拆回去，路径里一个空格就把命令行切坏了。
func BuildAux(tools map[string]Tool, prog AuxProgram, root, name string, drop *DropTo) ([]string, error) {
	tool, ok := tools[strings.ToLower(strings.TrimSpace(prog.Language))]
	if !ok {
		return nil, fmt.Errorf("这台判机没有 %s 的工具链，编不了这道题的%s", prog.Language, auxKindName(name))
	}
	dir := filepath.Join(root, name)
	if err := PrepareDir(dir, drop); err != nil {
		return nil, err
	}
	source := filepath.Join(dir, tool.SourceFile)
	if err := writeRunFile(source, []byte(prog.Source)); err != nil {
		return nil, err
	}
	if !tool.Compiles() {
		// 解释型的判题器/交互器也要那份护栏：它和学生的 Python 走的是同一道闸门，
		// 少写一份就是让作者的那份代码在没有钩子的解释器里跑。
		if err := writeSupport(tool, dir); err != nil {
			return nil, err
		}
		return tool.RunArgs(dir), nil
	}
	build := Run(tool.CompileArgs(dir), dir, nil, compileLimitsFor(drop))
	if build.Verdict == IE {
		// 编译器不在、沙盒没建起来：说清楚是判机的问题，别把作者的 bug 算成学生的。
		return nil, fmt.Errorf("%s编译时判机出错了：%s", auxKindName(name),
			orText(build.Note, orText(string(build.Stderr), "没有原因")))
	}
	if build.Exit != 0 {
		return nil, fmt.Errorf("%s编不过：%s", auxKindName(name),
			clipText(strings.TrimSpace(string(build.Stderr)), maxDetail))
	}
	exe := filepath.Join(dir, "program")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if _, err := os.Stat(exe); err != nil {
		// 编译器退 0 却没有产物（工具链被截断、-static 缺库时见过）：这仍然是判机故障。
		return nil, fmt.Errorf("%s说编好了，可执行文件却不在：%v", auxKindName(name), err)
	}
	return []string{exe}, nil
}

// checkWithSPJ 让判题器对一次运行下判定，返回判定与给人看的那句话。
//
// drop 是这次提交降到的那个身份：判题器也是别人写的代码，没理由以判机自己的（可能就是
// root）身份跑。
func checkWithSPJ(argv []string, dir string, tc Case, got []byte, drop *DropTo) (Verdict, string) {
	// 三个文件写在工作目录里：学生程序此时已经跑完，所以它读不到这些——反过来（判题前
	// 就把答案放进去）才是漏答案。
	paths := make([]string, 0, 3)
	for i, body := range [][]byte{tc.Input, got, tc.Expected} {
		p := filepath.Join(dir, "spj"+strconv.Itoa(i)+".dat")
		if err := writeRunFile(p, body); err != nil {
			return IE, "判题器要的文件写不出来: " + err.Error()
		}
		paths = append(paths, p)
	}
	ex := Run(append(append([]string{}, argv...), paths...), dir, nil, auxLimitsFor(drop))
	comment := clipText(strings.TrimSpace(string(ex.Stdout)), maxDetail)
	switch {
	case ex.Verdict == IE:
		return IE, orText(comment, orText(ex.Note, "判题器没能跑起来，也没有给出原因"))
	case ex.Verdict == TLE || ex.Verdict == MLE:
		// 判题器自己超时或超内存：这是判机的事，不该让学生去改一份也许正确的代码。
		return IE, fmt.Sprintf("判题器跑到上限还没完（用时 %dms，内存 %dKB；上限 %dms / %dKB）",
			ex.TimeMS, ex.MemKB, auxLimits.TimeMS, auxLimits.MemKB)
	case ex.Exit == 0:
		return AC, comment
	case ex.Exit == 1:
		return WA, comment
	default:
		// 判题器崩了或被信号打死：回一句它自己的 stderr，否则这又是一条 detail 全空的 IE。
		note := "判题器退出码 " + strconv.Itoa(ex.Exit)
		if errText := strings.TrimSpace(string(ex.Stderr)); errText != "" {
			note += "：" + clipText(errText, 512)
		} else if comment != "" {
			note += "：" + comment
		}
		return IE, note
	}
}
