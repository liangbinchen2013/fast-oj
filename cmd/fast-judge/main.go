package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/jail"
	"github.com/liangbinchen2013/fast-oj/internal/judge"
	"github.com/liangbinchen2013/fast-oj/internal/judgeagent"
)

func main() {
	// 判机被自己以 jailer 身份拉起时，这一行负责建牢房并 exec 提交程序，永不返回。
	jail.Main()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fast-judge:", err)
		os.Exit(1)
	}
}

// selfExe 是牢房要 exec 的那个"自己"。/proc/self/exe 在 chroot 之后仍然有效，
// 而且不受工作目录被删之类的破事影响，所以优先用它。
func selfExe() (string, error) {
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		return "/proc/self/exe", nil
	}
	return os.Executable()
}

// jailIdentity 决定牢房里的 uid。非 root 判机走 userns：要挑一个**没有映射**的号，
// 宿主上才会变成 nobody，判机的库与别人的源码才读不到。
func jailIdentity(drop *judge.DropTo, userNS bool) (uint32, uint32) {
	if !userNS {
		if drop != nil {
			return drop.UID, drop.GID
		}
		return nobody, nobody
	}
	uid, gid := uint32(nobody), uint32(nobody)
	if os.Getuid() == int(uid) { // 判机本身就是 nobody：换一个没映射的号
		uid, gid = nobody-1, nobody-1
	}
	return uid, gid
}

const nobody = 65534

// setupSandbox 定下沙盒配置，并当场真建一次牢房。-sandbox=on 而建不起来就直接不启动：
// 明确拒绝比悄悄少一层防护好。
//
// auto 的"开不了就继续"只适用于 Windows 这种根本没有对应内核机制的平台（run 里会先把
// mode 改成 off），以及 root 判机这种还能靠 -run-as 兜住身份的情况。Linux 非 root 判机
// 开不了 user namespace 牢房时，提交会和判机同 uid、同宿主文件系统；继续判就是把判机
// 令牌、数据库和别人的源码直接交给选手，所以这里必须拒绝启动。
func setupSandbox(dir string, mode jail.Mode, drop *judge.DropTo) (string, error) {
	userNS := os.Getuid() != 0
	uid, gid := jailIdentity(drop, userNS)
	self := ""
	if mode != jail.Off {
		path, err := selfExe()
		if err != nil {
			return "", fmt.Errorf("找不到判机自己的可执行文件，沙盒无从 exec：%w", err)
		}
		self = path
	}
	judge.SetSandbox(self, mode, userNS, uid, gid)
	note, err := judge.ProbeSandbox(dir)
	if err != nil {
		if mode == jail.On {
			return "", fmt.Errorf("-sandbox=on 但牢房建不起来：%w", err)
		}
		if refusal := unsandboxedStartupRefusal(mode, runtime.GOOS, os.Getuid(), err); refusal != nil {
			return "", refusal
		}
	}
	return note, nil
}

// unsandboxedStartupRefusal 决定 auto 自检失败时要不要拒绝启动。
//
// 只拦 Linux 非 root：这是唯一"牢房没了、提交却仍以判机身份跑"的组合。root 判机若也
// 开不了牢房，Runtime 的 Configure 会在没有 -run-as 时逐条拒绝（见 runner_other.go），
// 有 -run-as 时至少跨 uid；非 root 没有这层兜底，只能在启动时停住。
func unsandboxedStartupRefusal(mode jail.Mode, goos string, uid int, probeErr error) error {
	if probeErr == nil || mode != jail.Auto || goos != "linux" || uid == 0 {
		return nil
	}
	return fmt.Errorf("非 root 判机的 user namespace 牢房建不起来：%w。"+
		"拒绝在没有隔离的状态下判题：提交会以判机自己的 uid 运行，能读判机令牌、数据库与别人的源码。"+
		"请改用 root 或容器运行判机（root 判机建议同时配 -run-as uid:gid 作为第二身份）；"+
		"如果确实要在无隔离状态下判题，必须显式加 -sandbox=off 并自行承担后果", probeErr)
}

func defaultName() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "judge-1"
}

func run() error {
	var (
		server  = flag.String("server", envOr("FASTOJ_SERVER", "http://127.0.0.1:8080"), "fast-oj base URL")
		token   = flag.String("token", os.Getenv("FASTOJ_JUDGE_TOKEN"), "judge token (required)")
		name    = flag.String("name", envOr("FASTOJ_JUDGE_NAME", defaultName()), "judge box name")
		dir     = flag.String("dir", os.Getenv("FASTOJ_JUDGE_DIR"), "work directory, default under the user cache dir")
		poll    = flag.Duration("poll", 500*time.Millisecond, "wait between empty queue polls")
		backoff = flag.Duration("backoff", 3*time.Second, "wait after a failed round")
		once    = flag.Bool("once", false, "process at most one submission, then exit")
		tools   = flag.String("tools", "", "comma list of languages to advertise (default: everything usable)")
		runAs   = flag.String("run-as", envOr("FASTOJ_JUDGE_RUN_AS", ""), "run submissions as this uid:gid (Linux/containers; empty keeps the judge's own user)")
		sbox    = flag.String("sandbox", envOr("FASTOJ_JUDGE_SANDBOX", "auto"), "判题沙盒：auto=能开就开，on=开不起来就不判，off=不隔离")
		verbose = flag.Bool("v", false, "log each step")
	)
	flag.Parse()

	// 令牌写在命令行上就等于发给了整台机器：/proc/<pid>/cmdline 是所有本地账号可读的，
	// 而判机上跑的正是别人提交的代码。systemd 里写 `-token $FASTOJ_JUDGE_TOKEN` 会先展开
	// 再 exec，0600 的 EnvironmentFile 也因此白设——默认值本来就是那个环境变量，别再传一遍。
	if leaked := config.ArgvSecrets(os.Args[1:], map[string]string{"-token": *token}); len(leaked) > 0 {
		if runtime.GOOS == "linux" {
			return fmt.Errorf("判机令牌不能写在命令行上（%s）：改用环境变量 FASTOJ_JUDGE_TOKEN，"+
				"并把它从 ExecStart 里去掉（systemd 的 EnvironmentFile 已经提供了它）",
				strings.Join(leaked, "、"))
		}
		fmt.Fprintf(os.Stderr, "fast-judge: 警告：判机令牌出现在命令行上，本机上其他账号看得到；"+
			"正式部署请改用环境变量 FASTOJ_JUDGE_TOKEN\n")
	}

	drop, err := parseRunAs(*runAs)
	if err != nil {
		return err
	}
	mode, err := jail.ParseMode(*sbox)
	if err != nil {
		return err
	}
	if drop != nil && runtime.GOOS == "windows" {
		// Refusing beats a silently ignored flag: on Windows there is no way to
		// hand the judged process a different identity here.
		return fmt.Errorf("-run-as 在 Windows 上不生效，判机只能以自己的身份跑；要么去掉它，要么把判机放进容器")
	}
	if mode == jail.On && runtime.GOOS != "linux" {
		return fmt.Errorf("-sandbox=on 需要 Linux 命名空间，%s 上没有；要么用 -sandbox=off，要么换 Linux/容器", runtime.GOOS)
	}
	if runtime.GOOS != "linux" {
		// 非 Linux 没有"降级版"沙盒可言：直接按关闭处理，日志与 /judges 上都会写明。
		mode = jail.Off
	}

	logf := func(format string, args ...any) {
		if *verbose {
			log.Printf(format, args...)
		}
	}
	langs := splitCSV(*tools)

	agent, err := judgeagent.New(judgeagent.Config{
		Server: *server, Token: *token, Name: *name, Dir: *dir,
		Poll: *poll, Backoff: *backoff, Languages: langs, Logf: logf, RunAs: drop,
	})
	if err != nil {
		return err
	}
	// 沙盒配置要在任何一次判题之前定下来：agent.New 已经拿到了工作目录，自测就用它。
	note, sandboxErr := setupSandbox(agent.Dir(), mode, drop)
	if sandboxErr != nil {
		return sandboxErr
	}
	agent.SetNote(note)
	// Python 要两道都过才上名单：这台机器有解释器，且护栏在牢房里真挡得住一次子进程调用。
	// 缺任何一道就把这门语言摘掉并把原因写在 /judges 上——"页面上能选、判机却判不了"是
	// 最差的一种，"能判但没隔离"是更差的一种。
	if pyNote, err := judge.PythonProbe(filepath.Join(agent.Dir(), "pyprobe")); err != nil {
		agent.DisableLanguage(judge.PythonLanguage, err.Error())
		log.Printf("Python 不在这台判机的名单上：%v", err)
	} else {
		log.Printf("Python 自测：%s", pyNote)
	}
	log.Printf("judge %s polling %s for [%v]", *name, *server, agent.Languages())
	log.Printf("隔离边界：%s。环境变量只发最小集，内存/CPU/进程数与超时由判机实测加内核 rlimit 双重兜住。", note)
	if mode == jail.Off {
		log.Printf("警告：沙盒已关闭。提交的代码能读写这个账号能访问的一切并能出网——只在你信得过的机器上这么用。")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		worked, err := agent.ProcessOnce(ctx)
		if err != nil {
			return err
		}
		if !worked {
			log.Printf("queue empty")
		}
		return nil
	}

	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseRunAs turns the operator's "uid:gid" into a drop target. A malformed value
// fails at startup rather than on the first submission; empty means the judged
// program keeps the judge's own user, which is what running without a container
// looks like.
func parseRunAs(raw string) (*judge.DropTo, error) {
	if raw == "" {
		return nil, nil
	}
	uidPart, gidPart, found := strings.Cut(raw, ":")
	if !found {
		return nil, fmt.Errorf("-run-as wants uid:gid, got %q", raw)
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(uidPart), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("-run-as uid: %w", err)
	}
	gid, err := strconv.ParseUint(strings.TrimSpace(gidPart), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("-run-as gid: %w", err)
	}
	return &judge.DropTo{UID: uint32(uid), GID: uint32(gid)}, nil
}
