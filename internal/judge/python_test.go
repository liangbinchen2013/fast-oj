package judge

import (
	"path/filepath"
	"strings"
	"testing"
)

// requirePython 让这台机器没有解释器时跳过，而不是报一个跟判定无关的失败。
func requirePython(t *testing.T) Tool {
	t.Helper()
	if !pythonUsable() {
		t.Skipf("这台机器上没有 %s", pythonInterpreter())
	}
	return pythonTool
}

func judgePython(t *testing.T, source string, cases []Case) Report {
	t.Helper()
	tool := requirePython(t)
	requireRunnable(t)
	task := Task{
		SubmissionID: 1, PID: "P1", Language: PythonLanguage, Source: source,
		TimeLimitMS: 10_000, MemLimitKB: 256 << 10, Cases: cases,
	}
	return Judge(task, tool, filepath.Join(t.TempDir(), "work"), nil, nil)
}

const pySumSource = `import sys
a, b = sys.stdin.read().split()
print(int(a) + int(b))`

func TestPythonAcceptsCorrectProgram(t *testing.T) {
	rep := judgePython(t, pySumSource, sumCases())
	if rep.Verdict != AC {
		t.Fatalf("判定 = %s（%s），要 AC", rep.Verdict, rep.Detail)
	}
	if rep.Score != 100 {
		t.Fatalf("得分 = %d/%d，要 100", rep.Score, rep.TotalScore)
	}
}

func TestPythonWrongAnswerStillJudgedWA(t *testing.T) {
	rep := judgePython(t, `print(0)`, sumCases())
	if rep.Verdict != WA {
		t.Fatalf("判定 = %s，要 WA（%s）", rep.Verdict, rep.Detail)
	}
}

// 护栏要挡的是"离开这次运行"的那几扇门，而不是标准库本身。
//
// 这一条同时是当初那句警告的回归测试：一刀禁掉 os 会让 random / pathlib / tempfile
// 一起塌（random.py 自己就 import os）。所以断言是两半的——禁的必须真挡住，
// 而常用库必须还能用。
func TestPythonGuardBlocksEscapeButKeepsStdlib(t *testing.T) {
	cases := []Case{{Name: "1", Ordinal: 1, Score: 100, Input: []byte("1 2\n"), Expected: []byte("3\n")}}
	for _, c := range []struct {
		name   string
		source string
	}{
		{"subprocess", `import subprocess
print(subprocess.run(["echo","LEAKED"],capture_output=True).stdout.decode())`},
		{"os.system", `import os
os.system("echo LEAKED")
print("3")`},
		{"socket", `import socket
print(socket.socket().connect_ex(("127.0.0.1", 1)))`},
		{"ctypes", `import ctypes
print(ctypes.CDLL("libc.so.6"))`},
		{"pip", `import pip
print("3")`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := judgePython(t, c.source, cases)
			if rep.Verdict == AC {
				t.Fatalf("%s 没被挡下来（%s）", c.name, rep.Detail)
			}
			if rep.Verdict != RE {
				t.Fatalf("判定 = %s（%s），被护栏挡下来应当是学生运行时错误 RE", rep.Verdict, rep.Detail)
			}
			// 挡下来的证据是那句"判机不允许"：Traceback 里本来就带着学生写的那行
			// os.system("echo LEAKED")，拿输出里的字样去判断"有没有跑出去"会把源码
			// 本身当成泄漏。
			if !strings.Contains(rep.Detail, "判机不允许") {
				t.Fatalf("%s 的失败原因不是护栏：%s", c.name, rep.Detail)
			}
		})
	}

	// 反过来：这些是学生真会用的东西，禁了就是把 Python 变成另一门语言。
	// pathlib 在名单里不是凑数：python3.14 的 pathlib 会 import fcntl，早先那份名单
	// 禁了 fcntl，于是 `import pathlib` 直接炸——Linux 上实测到的，Windows 上没有 fcntl
	// 所以只有那边跑得出来。
	ok := `import math, random, pathlib, tempfile, json, sys, os, os.path
import decimal, fractions, statistics, heapq, functools, itertools, collections
import re, string, copy, array, bisect
random.seed(1)
d = tempfile.mkdtemp()
pathlib.Path(d).joinpath("x.txt").write_text(str(math.pi) + json.dumps({1: 2}))
print(len(os.listdir(d)), decimal.Decimal("1.5") + decimal.Decimal("0.5"),
	heapq.nsmallest(1, [3, 1, 2]), len(re.findall(r"\d", "a1b2")), len(string.ascii_lowercase))`
	rep := judgePython(t, ok, []Case{{Name: "1", Ordinal: 1, Score: 100,
		Input: []byte("1 2\n"), Expected: []byte("1 2.0 [1] 2 26\n")}})
	if rep.Verdict != AC {
		t.Fatalf("常用标准库被挡了：判定 %s，%s", rep.Verdict, rep.Detail)
	}
}

// 判题器也可以是 Python 写的：BuildAux 对不编译的工具要把护栏一起落进目录，
// 并交回"解释器 + 护栏 + 源码"这条命令行——少了护栏就是让作者那份代码在没有钩子的
// 解释器里跑，多了源码就变成把 main.py 交给解释器两遍。
func TestPythonCheckerBuildsAndJudges(t *testing.T) {
	requirePython(t)
	requireRunnable(t)
	dir := t.TempDir()
	src := `import sys
data = [line.strip() for line in open(sys.argv[2], encoding="utf-8")]
print("宽松判法：只要不是空")
sys.exit(0 if data and data[0] else 1)`
	argv, err := BuildAux(map[string]Tool{PythonLanguage: pythonTool},
		AuxProgram{Language: PythonLanguage, Source: src}, dir, AuxChecker, nil)
	if err != nil {
		t.Fatalf("Python 判题器建不起来：%v", err)
	}
	if len(argv) < 2 || !strings.Contains(strings.Join(argv, " "), "pyguard.py") {
		t.Fatalf("命令行里没有护栏：%v", argv)
	}
	tc := Case{Input: []byte("1\n"), Expected: []byte("ignored\n")}
	if v, note := checkWithSPJ(argv, dir, tc, []byte("7\n"), nil); v != AC {
		t.Errorf("非空输出应当 AC：%s %s", v, note)
	}
	if v, _ := checkWithSPJ(argv, dir, tc, []byte("\n"), nil); v != WA {
		t.Errorf("空输出应当 WA，得到 %s", v)
	}
}
