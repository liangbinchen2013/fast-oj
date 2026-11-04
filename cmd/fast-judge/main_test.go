package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/jail"
)

// 非 root 的 Linux 判机一旦 userns 牢房建不起来，auto 不能继续裸跑：提交会以判机自己的
// uid 运行，能读令牌、数据库和别人的源码。启动时停住是唯一诚实的答案。
func TestNonRootLinuxSandboxFailureRefusesToStart(t *testing.T) {
	err := unsandboxedStartupRefusal(jail.Auto, "linux", 1000, errors.New("setgroups: operation not permitted"))
	if err == nil {
		t.Fatal("非 root Linux 的 auto 自检失败竟然被放行了")
	}
	if !strings.Contains(err.Error(), "拒绝在没有隔离的状态下判题") ||
		!strings.Contains(err.Error(), "-sandbox=off") {
		t.Fatalf("拒绝的话没有说清后果与出路：%q", err)
	}
}

// root 判机是另一条兜底路径：没 -run-as 时 Configure 会逐条拒绝，有 -run-as 时跨 uid；
// 启动阶段不能把 auto 的"开不了就继续"误伤成起不来。
func TestRootSandboxFailureKeepsAutoSemantics(t *testing.T) {
	if err := unsandboxedStartupRefusal(jail.Auto, "linux", 0, errors.New("no userns")); err != nil {
		t.Fatalf("root 判机的 auto 被误拦：%v", err)
	}
}

func TestUnsandboxedRefusalOnlyAppliesToTheDangerousCombination(t *testing.T) {
	probeErr := errors.New("no userns")
	cases := []struct {
		name string
		mode jail.Mode
		goos string
		uid  int
		err  error
		want bool
	}{
		{name: "non-root linux auto", mode: jail.Auto, goos: "linux", uid: 1000, err: probeErr, want: true},
		{name: "root linux auto", mode: jail.Auto, goos: "linux", uid: 0, err: probeErr},
		{name: "non-root windows auto", mode: jail.Auto, goos: "windows", uid: 1000, err: probeErr},
		{name: "non-root linux off", mode: jail.Off, goos: "linux", uid: 1000, err: probeErr},
		{name: "non-root linux on", mode: jail.On, goos: "linux", uid: 1000, err: probeErr},
		{name: "probe passed", mode: jail.Auto, goos: "linux", uid: 1000},
	}
	for _, tc := range cases {
		if got := unsandboxedStartupRefusal(tc.mode, tc.goos, tc.uid, tc.err) != nil; got != tc.want {
			t.Errorf("%s: refusal=%v, want %v", tc.name, got, tc.want)
		}
	}
}
