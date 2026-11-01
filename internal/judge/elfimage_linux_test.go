//go:build linux

package judge

import (
	"strings"
	"testing"
)

// 960MB 的全局数组：题目上限 256 MB，加上判机自己那 512 MB 余量也放不下。阿里云那台判机上
// 实测过同一个形状：`ulimit -v 786432` 之下 `readelf` 报出 p_memsz=0x39387298（915.6MiB）的二进制
// 退出码 139（SIGSEGV），驻留只有几 MB —— 牢房的 set_brk 扩不动，程序碰一下 .bss 就被打死。
// 生产提交 #43 就是这么炸的，而它交到学生眼前的是"运行时错误 · 内存 17.52M"，一句自相矛盾的话。
//
// 那个数组必须**被真的用到**（用输入当下标）：写成 `static` 又只打印 g[1]，-O2 会认出整块都是
// 0 直接把它优化没，镜像缩回几 KB，测的就不是这件事了。
const hugeStaticSource = `#include <cstdio>
long long g[120000000];
int main(){ int i=0; if (std::scanf("%d", &i)!=1) return 1; std::printf("%lld\n", g[i]); return 0; }`

func TestHugeStaticImageJudgesMLENotRE(t *testing.T) {
	jailOn(t) // 只有牢房会设 RLIMIT_AS，没开牢房这台机器根本拦不住它
	tool := requireToolchain(t)

	cases := []Case{{Name: "1", Ordinal: 1, Score: 100, Input: []byte("1 2\n"), Expected: []byte("3\n")}}
	rep := judgeSource(t, tool, hugeStaticSource, cases, 2000, 256*mb)
	t.Logf("判定 = %s，内存 = %dKB，detail = %q", rep.Verdict, rep.MemKB, rep.Detail)

	if rep.Verdict != MLE {
		t.Fatalf("960MB 静态数组在 256MB 题上判成 %s，想要 MLE（%s）", rep.Verdict, rep.Detail)
	}
	for _, want := range []string{"程序本身要", "上限只有"} {
		if !strings.Contains(rep.Detail, want) {
			t.Errorf("判机信息没把原因说清（缺 %q）：%q", want, rep.Detail)
		}
	}
	if rep.MemKB <= 0 || rep.MemKB > 64*mb {
		t.Errorf("内存那一格该是实测的驻留值，别把需求塞进去：%dKB", rep.MemKB)
	}
}
