package jail

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 状态信道是"牢房有没有建成"唯一的判据，所以它必须先证明自己读得出真话、挡得住假话。

func TestStatusRoundTrip(t *testing.T) {
	st, err := NewStatus()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Remove()

	if st.Path() == "" || len(st.Nonce()) < 16 {
		t.Fatalf("信道没建好：path=%q nonce 长度=%d", st.Path(), len(st.Nonce()))
	}
	info, err := os.Stat(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	// 只有判机自己能读写：这个文件里写着这一把的暗号，别的账号读到就等于能伪造
	// "牢房坏了"。Windows 上 Go 报不出 POSIX 权限位（也没有牢房可言），所以这一步只在
	// 真正会用这条信道的平台上断言。
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("信道权限 = %v，想要 0600", info.Mode().Perm())
	}
	if _, ok := st.Failure(); ok {
		t.Error("还没写就该没有失败声明")
	}
	if st.Execed() {
		t.Error("还没写就该没有 exec 记号")
	}

	// jailer 那一侧：按 spec 里的路径把写端握过来。
	w, err := take(Spec{Status: st.Path(), Nonce: st.Nonce()})
	if err != nil {
		t.Fatal(err)
	}
	w.markExeced()
	if !st.Execed() {
		t.Error("exec 记号没读到")
	}
	w.markFail(errFakeReason)
	reason, ok := st.Failure()
	if !ok || reason != "牢房少了一步 mount" {
		t.Errorf("失败声明读成 %q / %v", reason, ok)
	}
}

var errFakeReason = &jailError{"牢房少了一步 mount"}

type jailError struct{ s string }

func (e *jailError) Error() string { return e.s }

// 信道上的每一行都必须带着这一把的暗号。别人写得出"看起来像失败"的那一行，写不出暗号——
// 这正是这条信道取代 stderr 的全部理由。
func TestStatusIgnoresAnythingWithoutThisRunsNonce(t *testing.T) {
	st, err := NewStatus()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Remove()

	// 伪造者：知道格式，不知道这一把的暗号，也没读过那个 0600 的文件。
	raw, err := os.OpenFile(st.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, line := range []string{
		ErrPrefix + "假的", // 旧判据的那句话，现在不该有任何分量
		"deadbeef" + statusSep + statusFail + statusSep + "别人家的暗号",
		st.Nonce()[:8] + statusSep + statusFail + statusSep + "只猜对半截暗号",
		st.Nonce() + statusSep + "X" + statusSep + "暗号对但说的是别的事",
		st.Nonce() + statusSep + statusFail, // 少了第三个分隔符
	} {
		if _, err := raw.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if reason, ok := st.Failure(); ok {
		t.Errorf("伪造的行被当成真的失败声明了：%q", reason)
	}
	if st.Execed() {
		t.Error("伪造的行被当成 exec 记号了")
	}
	// 前面这些假话不能把信道"污染"掉：真 jailer 写的那一行仍然读得出来，
	// 否则一次伪造就能让真的牢房失败永远说不清。
	w, err := take(Spec{Status: st.Path(), Nonce: st.Nonce()})
	if err != nil {
		t.Fatal(err)
	}
	w.markFail(errFakeReason)
	if reason, ok := st.Failure(); !ok || reason != "牢房少了一步 mount" {
		t.Errorf("真话被假话挤掉了：%q / %v", reason, ok)
	}
}

// 没有信道的 jailer（手工跑、或判机建文件失败）不能因此崩掉：它退回 stderr，而判机那边
// 读不到任何东西——宁可比真话晚一点，也不把别人的话当真话。
func TestStatusWithoutChannelIsSilentNotFatal(t *testing.T) {
	var none *Status
	if _, err := take(Spec{}); err == nil {
		t.Error("没有信道的 spec 本该报错")
	}
	none.markFail(errFakeReason) // 不许 panic
	none.markExeced()
	if reason, ok := none.Failure(); ok {
		t.Errorf("空信道读出了东西：%q", reason)
	}
	if none.Execed() {
		t.Error("空信道读出了 exec 记号")
	}
	none.Remove()
	if _, err := take(Spec{Status: filepath.Join(t.TempDir(), "不存在"), Nonce: "x"}); err == nil {
		t.Error("打不开的信道本该报错")
	}
}

// 用完就删：信道里写着这一把的暗号，留着比删掉多一分被读走的风险。
func TestStatusRemove(t *testing.T) {
	st, err := NewStatus()
	if err != nil {
		t.Fatal(err)
	}
	path := st.Path()
	st.Remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("删完之后还在：%v", err)
	}
	// 删掉之后连读都读不到——留着路径也拿不出第二份暗号。
	if _, err := st.peek(); err == nil {
		t.Error("删完之后信道还读得到")
	}
}
