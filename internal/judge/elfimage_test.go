package judge

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ELF 里的段标志位（p_flags），手写而不引 debug/elf：测试的构造不该依赖被测实现。
const (
	pfR = 4
	pfW = 2
	pfX = 1
)

// writeELF 造一个只有程序头的 ELF64：段的内容无所谓，被判读的是 p_memsz。
func writeELF(t *testing.T, sizes []struct {
	mem   uint64
	flags uint32
}) string {
	t.Helper()
	const phentsize = 56

	buf := make([]byte, 64)
	copy(buf, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1}) // EI_MAG + 64 位 + 小端 + 版本
	binary.LittleEndian.PutUint16(buf[16:], 2)      // e_type = ET_EXEC
	binary.LittleEndian.PutUint16(buf[18:], 0x3e)   // e_machine = EM_X86_64
	binary.LittleEndian.PutUint32(buf[20:], 1)      // e_version
	binary.LittleEndian.PutUint64(buf[32:], 64)     // e_phoff
	binary.LittleEndian.PutUint16(buf[52:], 64)     // e_ehsize
	binary.LittleEndian.PutUint16(buf[54:], phentsize)
	binary.LittleEndian.PutUint16(buf[56:], uint16(len(sizes)))

	heads := make([]byte, len(sizes)*phentsize)
	for i, s := range sizes {
		ph := heads[i*phentsize:][:phentsize]
		binary.LittleEndian.PutUint32(ph[0:], 1) // p_type = PT_LOAD
		binary.LittleEndian.PutUint32(ph[4:], s.flags)
		binary.LittleEndian.PutUint64(ph[8:], 64) // p_offset，落在文件里就行
		binary.LittleEndian.PutUint64(ph[40:], s.mem)
	}

	path := filepath.Join(t.TempDir(), "a.out")
	if err := os.WriteFile(path, append(buf, heads...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const mb = 1024 // imageMemKB 的单位是 KB

func TestImageMemKBAddsUpTheLoadedSegments(t *testing.T) {
	// 32MB 的代码与只读数据 + 736MB 的静态数组：学生常说的"数组开大了"就是这个形状。
	path := writeELF(t, []struct {
		mem   uint64
		flags uint32
	}{
		{32 * mb * mb, pfR | pfX},
		{736 * mb * mb, pfR | pfW},
	})
	if got := imageMemKB(path); got != 768*mb {
		t.Errorf("镜像需求 = %dKB，想要 768MB（%dKB）", got, 768*mb)
	}
}

// 读不出来就返回 0：这条只能"把崩溃解释对"，绝不能变成判定的必要条件。
func TestImageMemKBGivesUpQuietly(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk")
	if err := os.WriteFile(junk, []byte("MZ\x90\x00not an elf at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := imageMemKB(junk); got != 0 {
		t.Errorf("非 ELF = %d，想要 0", got)
	}
	if got := imageMemKB(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("文件不存在 = %d，想要 0", got)
	}
	// 坏到头本身的文件不该让判题 panic。
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("\x7fELF\x02\x01\x01"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := imageMemKB(short); got != 0 {
		t.Errorf("截断的 ELF 头 = %d，想要 0", got)
	}
	// 1TiB 以上的段是坏头或壳：当成读不出来，别拿它去改判。p_memsz 的单位是字节。
	big := writeELF(t, []struct {
		mem   uint64
		flags uint32
	}{
		{1 << 41, pfR | pfW}, // 2TiB
	})
	if got := imageMemKB(big); got != 0 {
		t.Errorf("坏头吹出的 2TB = %d，想要 0", got)
	}
}

// 拿真实产物自检：能读到就一定是个合理的数（这个包本身编译出来是几十 MB 级别，不含大数组）。
func TestImageMemKBOnARealBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("拿不到自己的路径：", err)
	}
	got := imageMemKB(exe)
	if got == 0 {
		t.Skip("这个平台的可执行文件不是 ELF（Windows 的 PE 走作业对象那条路）")
	}
	if got < 1*mb || got > 4*mb*mb {
		t.Errorf("读出来的镜像需求 = %dKB，不像一个测试二进制的量级", got)
	}
}

func TestMarkImageOOMRelabelsOnlyStartupCrashes(t *testing.T) {
	lim := Limits{TimeMS: 1000, MemKB: 256 * mb}
	// 提交 #43 那份代码的真实镜像：3e7 个 vector 头（720MB）+ dis/vis 两组 3e7 个 long long
	// （480MB）+ 四组 5e5 的数组（16MB）。判机那台阿里云 ECS 上按这个形状编出来实测
	// p_memsz 之和 = 1187504KB（1.13GiB），这里取整成 1200MB —— 上限 256MB 连判机那 512MB
	// 余量都盖不住。
	const image = 1200 * mb

	// 提交 #43 那一幕：SIGSEGV 打死，驻留只有 17.5MB。Go 把信号死记成退出码 -1，
	//"被信号打死"这件事只有 ProcessState.Signalled() 知道，所以要由调用点告诉它。
	crash := Execution{Verdict: RE, Note: "signal: segmentation fault", Exit: -1, MemKB: 17944, TimeMS: 7}
	markImageOOM(&crash, lim, image, true)
	if crash.Verdict != MLE {
		t.Fatalf("RE + 镜像超上限 + 被信号打死 = %s，想要 MLE", crash.Verdict)
	}
	for _, want := range []string{"1200MB", "256MB", "512MB", "segmentation fault"} {
		if !strings.Contains(crash.Note, want) {
			t.Errorf("判机信息缺了 %q：%q", want, crash.Note)
		}
	}
	if crash.MemKB != 17944 {
		t.Errorf("内存那一格该保留实测值（%d），需求写在说明里就够", crash.MemKB)
	}

	// 自己 exit 非零：程序确实跑起来了，那是它自己的选择，不是地址空间不够。
	selfFail := Execution{Verdict: RE, Exit: 1, MemKB: 948}
	markImageOOM(&selfFail, lim, image, false)
	if selfFail.Verdict != RE {
		t.Errorf("自己退出非零被改判成 %s", selfFail.Verdict)
	}
	// 镜像比上限大、但还在判机余量以内：它跑得起来，真是崩溃就是它自己的 bug。
	insideSlack := Execution{Verdict: RE, Exit: -1, MemKB: 948}
	markImageOOM(&insideSlack, lim, 600*mb, true)
	if insideSlack.Verdict != RE || insideSlack.Note != "" {
		t.Errorf("600MB 的镜像在 256MB+512MB 之内，不该动它：%+v", insideSlack)
	}
	// 题目没设内存上限，或这个平台的镜像读不出来（0）：都不该动。
	noLimit := Execution{Verdict: RE, Exit: -1}
	markImageOOM(&noLimit, Limits{TimeMS: 1000}, image, true)
	if noLimit.Verdict != RE {
		t.Errorf("没有内存上限时也被改了：%s", noLimit.Verdict)
	}
	unknown := Execution{Verdict: RE, Exit: -1}
	markImageOOM(&unknown, lim, 0, true)
	if unknown.Verdict != RE {
		t.Errorf("镜像读不出来时被改了：%s", unknown.Verdict)
	}
	// 已经判成 MLE/TLE/AC 的更不该动。
	already := Execution{Verdict: MLE, Exit: 0, MemKB: 300 * mb}
	markImageOOM(&already, lim, image, true)
	if already.Verdict != MLE || already.Note != "" {
		t.Errorf("重复处理改变了它：%+v", already)
	}
}

func TestMebiFormatsWholeAndFractionalMB(t *testing.T) {
	for _, c := range []struct {
		kb   int
		want string
	}{{1228800, "1200MB"}, {262144, "256MB"}, {17944, "17.52MB"}} {
		if got := mebi(c.kb); got != c.want {
			t.Errorf("mebi(%dKB) = %q，想要 %q", c.kb, got, c.want)
		}
	}
}
