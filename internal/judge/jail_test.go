//go:build linux

package judge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/jail"
)

// TestMain 让测试二进制也能当 jailer：沙盒是"判机 exec 自己"实现的，这里被 exec 的
// "自己"就是跑起来的这个测试二进制。
func TestMain(m *testing.M) {
	jail.Main()
	os.Exit(m.Run())
}

// jailOn 打开沙盒并当场自测一次。内核不给命名空间（或非 root 又禁了 unprivileged
// userns）时整组跳过——那台机器不行，不等于这段代码坏了。
func jailOn(t *testing.T) {
	t.Helper()
	requireToolchain(t)
	enableJailForTests(t)
}

// build 在牢房里用 g++ 编一个程序：编译这一步也走同一条路，所以它同时验证了
// "工具链在只读 bind 里还能用"。
func build(t *testing.T, dir, source, out string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, source), []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := Run([]string{"g++", "-O2", "-std=c++17", "-o", out, source}, dir, nil, CompileLimits)
	if ex.Exit != 0 {
		t.Fatalf("牢房里编译失败（%v）：%s", ex.Verdict, ex.Stderr)
	}
}

const testSource = `#include <cstdio>
int main(){ printf("hi\n"); return 0; }`

// 牢房不能把判题本身弄坏：正常程序仍然要跑出正确输出与判定。
func TestJailStillJudgesNormally(t *testing.T) {
	jailOn(t)
	dir := t.TempDir()
	build(t, dir, "main.cpp", "main")
	ex := Run([]string{filepath.Join(dir, "main")}, dir, nil, Limits{TimeMS: 2000, MemKB: 65536})
	if ex.Verdict != AC || strings.TrimSpace(string(ex.Stdout)) != "hi" {
		t.Fatalf("牢房里的正常程序 = %v exit=%d out=%q note=%s", ex.Verdict, ex.Exit, ex.Stdout, ex.Note)
	}
}

// 关掉沙盒时结果必须一模一样，否则"开不开沙盒"会改变判定口径。
//
// root 判机上跳过：那条形态现在按设计不存在——root + 没沙盒 + 没 -run-as 是**拒绝跑提交**
// （见 plainGuard.Configure），拿不到"关掉沙盒的那一份结果"来比。非 root 的判机（容器里的
// 专用账号、开发机）两边都照跑，而那正是生产中真的会关掉沙盒的形态。
func TestJailOffKeepsTheSameVerdict(t *testing.T) {
	requireToolchain(t)
	if os.Getuid() == 0 {
		t.Skip("root 判机关掉沙盒时按设计不跑提交，这里比的两种形态在 root 上只剩一种")
	}
	dir := t.TempDir()
	build(t, dir, "main.cpp", "main")
	ex := Run([]string{filepath.Join(dir, "main")}, dir, nil, Limits{TimeMS: 2000, MemKB: 65536})
	if ex.Verdict != AC {
		t.Fatalf("不开沙盒时 = %v（%s）", ex.Verdict, ex.Note)
	}
}

func runProbe(t *testing.T, dir, source string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "p.cpp"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if ex := Run([]string{"g++", "-O2", "-std=c++17", "-o", "p", "p.cpp"}, dir, nil, CompileLimits); ex.Exit != 0 {
		t.Fatalf("编译探针失败：%s", ex.Stderr)
	}
	ex := Run([]string{filepath.Join(dir, "p")}, dir, nil, Limits{TimeMS: 3000, MemKB: 65536})
	return strings.TrimSpace(string(ex.Stdout))
}

// 牢房里既看不到 /etc/passwd，也写不进宿主任何路径：这就是"目录读写"那一半。
func TestJailHidesAndFreezesTheHost(t *testing.T) {
	jailOn(t)
	dir := t.TempDir()
	out := runProbe(t, dir, `#include <cstdio>
#include <unistd.h>
int main(){
  printf("etc=%d proc1=%d var=%d",
    access("/etc/passwd", F_OK)==0,
    access("/proc/1/cmdline", F_OK)==0,
    access("/var", F_OK)==0);
  FILE *f = fopen("/etc/oj-escape", "w");
  printf(" wroteto_etc=%d", f != NULL);
  if (f) { fclose(f); }
  return 0;
}`)
	if strings.Contains(out, "etc=1") || strings.Contains(out, "var=1") || strings.Contains(out, "wroteto_etc=1") {
		t.Fatalf("牢房还能看见或写到宿主：%s", out)
	}
	if _, err := os.Stat("/etc/oj-escape"); err == nil {
		t.Fatal("宿主 /etc 下真的多了个文件")
	}
	// /proc/1 在牢房里应该是它自己（新 pid 命名空间），而不是宿主 systemd。
	t.Logf("探针输出：%s", out)
}

// 网络与"看着别人干活"这两条路都得断。
//
// CLONE_NEWNET 已经让牢房里没有第二张网卡（连不上才是原来的测法）；seccomp 那一层把
// socket() 本身变成 EPERM，并把 ptrace 挡在同一道门外——后者不是"能不能出门"的问题，
// 而是"能不能看见判机上正在跑谁的哪道题"的问题。
func TestJailHasNoNetwork(t *testing.T) {
	jailOn(t)
	dir := t.TempDir()
	out := runProbe(t, dir, `#include <cstdio>
#include <cerrno>
#include <cstring>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <sys/ptrace.h>
int main(){
  int s = socket(AF_INET, SOCK_STREAM, 0);
  printf("socket=%d errno=%d", s, s < 0 ? errno : 0);
  if (s >= 0) {
    sockaddr_in a{}; a.sin_family = AF_INET; a.sin_port = htons(53);
    inet_pton(AF_INET, "8.8.8.8", &a.sin_addr);
    int r = connect(s, (sockaddr*)&a, sizeof(a));
    printf(" connect=%d errno=%d", r, r ? errno : 0);
  }
  errno = 0;
  long p = ptrace(PTRACE_TRACEME, 0, nullptr, nullptr);
  printf(" ptrace=%ld errno=%d", p, p < 0 ? errno : 0);
  return 0;
}`)
	if strings.Contains(out, "connect=0") {
		t.Fatalf("牢房里还能连公网：%s", out)
	}
	if !strings.Contains(out, "socket=-1 errno=1") {
		t.Errorf("seccomp 没把 socket() 挡成 EPERM：%s", out)
	}
	if !strings.Contains(out, "ptrace=-1 errno=1") {
		t.Errorf("seccomp 没把 ptrace 挡成 EPERM：%s", out)
	}
	t.Logf("网络/ptrace 探针：%s", out)
}

// 超内存必须还是 MLE，不能被 RLIMIT_AS 抢先打成 RE：判题口径不能因为开沙盒而变。
func TestJailReportsMLENotRE(t *testing.T) {
	jailOn(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.cpp"), []byte(`#include <cstdlib>
#include <cstdio>
int main(){
  const size_t n = 300u<<20;
  char *raw = (char*)malloc(n);
  if (!raw) { printf("no-malloc\n"); return 0; }
  // volatile：不然 -O2 会把"写满 300MB 再读一个字节"整个折成一次赋值，
  // 内存根本没进来，测出来的数字是真的但测的是别的程序。
  volatile char *p = raw;
  for (size_t i = 0; i < n; i += 4096) { p[i] = (char)(i & 1); }
  long s = 0;
  for (size_t i = 0; i < n; i += 4096) { s += p[i]; }
  printf("sum=%ld\n", s);
  return 0;
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ex := Run([]string{"g++", "-O2", "-o", "m", "m.cpp"}, dir, nil, CompileLimits); ex.Exit != 0 {
		t.Fatalf("编译失败：%s", ex.Stderr)
	}
	ex := Run([]string{filepath.Join(dir, "m")}, dir, nil, Limits{TimeMS: 5000, MemKB: 64 << 10})
	if ex.Verdict != MLE {
		t.Fatalf("吃 300MB 而限 64MB = %v（note=%s out=%q mem=%d exit=%d），要 MLE",
			ex.Verdict, ex.Note, ex.Stdout, ex.MemKB, ex.Exit)
	}
}

// 死循环仍然是 TLE，并且杀完不留进程——pid 命名空间在这里是白捡的一层保险。
func TestJailReportsTLE(t *testing.T) {
	jailOn(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.cpp"), []byte("int main(){ for(;;); }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ex := Run([]string{"g++", "-O2", "-o", "s", "s.cpp"}, dir, nil, CompileLimits); ex.Exit != 0 {
		t.Fatalf("编译失败：%s", ex.Stderr)
	}
	ex := Run([]string{filepath.Join(dir, "s")}, dir, nil, Limits{TimeMS: 400, MemKB: 65536})
	if ex.Verdict != TLE {
		t.Fatalf("死循环 = %v，要 TLE", ex.Verdict)
	}
}

// fork 炸弹：RLIMIT_NPROC 在牢房里按未映射的那个 uid 计，炸不动判机。
func TestJailStopsForkBomb(t *testing.T) {
	jailOn(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.cpp"), []byte(`#include <unistd.h>
int main(){ for(int i=0;i<400;i++){ if (fork()==0) { for(;;); } } return 0; }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ex := Run([]string{"g++", "-O2", "-o", "f", "f.cpp"}, dir, nil, CompileLimits); ex.Exit != 0 {
		t.Fatalf("编译失败：%s", ex.Stderr)
	}
	ex := Run([]string{filepath.Join(dir, "f")}, dir, nil, Limits{TimeMS: 1000, MemKB: 65536, MaxProcs: 8})
	switch ex.Verdict {
	case AC, RE, TLE:
	default:
		t.Fatalf("fork 炸弹的判定 = %v（%s）", ex.Verdict, ex.Note)
	}
	t.Logf("fork 炸弹：%v exit=%d note=%s", ex.Verdict, ex.Exit, ex.Note)
}
