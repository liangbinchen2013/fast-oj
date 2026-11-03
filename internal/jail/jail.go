// Package jail 把一次判题运行关进 Linux 命名空间：看不到宿主文件系统、没有网络、
// 没有可写的地方（除了这次运行的工作目录），并由内核而不是判机来兜住 CPU、内存、
// 进程数与文件大小。
//
// 为什么要"exec 自己"：Go 不能在 fork 与 exec 之间插东西，所以 setrlimit、mount、
// chroot 这些必须在子进程里、exec 之前完成（internal/judge/runner_other.go 里那句
// "Go cannot hand a Linux child its rlimits at spawn" 就是这个坑）。于是判机把真正的
// 命令行交给自己的另一个入口，由那个子进程先把牢房钉好，再 exec 提交程序。
//
// 建不起来的牢房必须明说，不能假装存在：任何一步失败都以 FailExit 退出，判机把它报成
// IE 而不是 RE——学生不该为内核不给权限去改自己的代码。
//
// **但这句话不能靠 stderr 传**：那份 stderr 同时装编译器与选手程序的输出，选手只要打印
// 一行 `OJ-JAIL: ...`（一条 `#line` 的编译错误就够，连执行都不用），判机就会以为"这台
// 机器开不了牢房"并从此全员裸跑——2026-11-01 在 ECS 上实测成立。失败的事实现在走 Status
// 那条独立信道：判机 spawn 前建一个只有自己能读写的文件并生成一次性暗号，jailer 在进
// 牢房之前握住它的 fd，失败时写上"暗号+原因"。选手程序既拿不到那个 fd（exec 之后它不在
// 命令行也不在环境里），也猜不到路径与暗号，更碰不到那个路径（牢房的根是这次工作的目录）。
package jail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// jailFlag 是隐藏入口：值里带着整份 Spec，不该出现在 -h，也不该有人手工敲。
	jailFlag = "--oj-jail"
	// FailExit 区分"沙盒没建起来"与"程序自己挂了"，方便人从退出码一眼看出是哪一种。
	// 它**单独不足以作判据**（任何程序都能 exit 70），所以真正的事实走 Status 信道。
	FailExit = 70
	// ErrPrefix 是 jailer 失败时顺手打在 stderr 上的前缀，只给人看：判机上没有一处拿它
	// 当判据。留着它是因为手工跑 jailer（没有状态信道）时，那是唯一的输出。
	ErrPrefix = "OJ-JAIL: "
	// statusSep 分开状态信道每一行的暗号与它说的是什么。
	statusSep = "\x1f"
	// 信道里的两种事实：牢房没能 exec 提交程序（F），以及真的把它 exec 起来了（E）。
	statusFail   = "F"
	statusExeced = "E"
)

// Status 是那条独立信道。判机用 NewStatus 建，jailer 写，判机读。
//
// 每一把运行一个：路径里有随机后缀，文件 0600 属于判机，用完就删。
type Status struct {
	path  string
	nonce string
	// f 是 jailer 侧握住的那个写端：必须在进牢房之前打开，chroot 之后那个路径就不存在了，
	// 而失败恰恰可能落在 chroot 前后的任意一步。
	f *os.File
}

// NewStatus 由判机在 spawn 之前调用：建好空的信道文件并生成这一把的暗号。
//
// 判机自己**不写**它，所以创建用的那个 fd 当场关掉——每次判题要跑几十个用例，留着就是
// 每用例漏一个 fd，而判机的 fd 上限本来就不该被这种事吃掉。
func NewStatus() (*Status, error) {
	f, err := os.CreateTemp("", "oj-jail-status-")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		os.Remove(path)
		return nil, err
	}
	return &Status{path: path, nonce: hex.EncodeToString(nonce)}, nil
}

// Path 与 Nonce 是要交给 jailer 的那两样（走 Spec，exec 之后就看不到了）。
func (s *Status) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Status) Nonce() string {
	if s == nil {
		return ""
	}
	return s.nonce
}

// Remove 用完销毁：信道里带着这一把的暗号，留着不如删掉。jailer 那一侧的写端不用关，
// 它要么 exec 走了（fd 带 O_CLOEXEC，选手进程拿不到），要么直接退出。
func (s *Status) Remove() {
	if s == nil {
		return
	}
	os.Remove(s.path)
	s.path = ""
}

// take 是 jailer 侧的入口：按 spec 里的路径把信道握过来。没有信道（手工跑 jailer，或者
// 判机建文件失败）就返回错误，让调用方退回 stderr 那条老路。
func take(spec Spec) (*Status, error) {
	if spec.Status == "" || spec.Nonce == "" {
		return nil, errors.New("这一把没有状态信道")
	}
	f, err := os.OpenFile(spec.Status, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Status{path: spec.Status, nonce: spec.Nonce, f: f}, nil
}

// write 尽力而为：写不上信道绝不能反过来挡住牢房本身。os.File 没有缓冲，一次 Fprintf
// 就是一次 write，父进程等它退出之后必然读得到。
//
// 每一行是 `<暗号>\x1f<这件事>\x1f<正文>`：读的那一边按同一份格式匹配前缀，所以这里少一
// 个分隔符就会让真话也被当成没说过——第一版就是这么写错的，是本包的单测抓到的。
func (s *Status) write(kind, text string) {
	if s == nil || s.f == nil {
		return
	}
	fmt.Fprintf(s.f, "%s%s%s%s%s\n", s.nonce, statusSep, kind, statusSep,
		strings.ReplaceAll(strings.TrimSpace(text), "\n", " "))
}

func (s *Status) markFail(reason error) { s.write(statusFail, reason.Error()) }
func (s *Status) markExeced()           { s.write(statusExeced, "") }

// lineWith 取出以"暗号 + 这件事"开头的那一行的正文。别的行一律不算：暗号是这一把一次性
// 随机的，写得出那一行却写不出暗号，正是这条信道要挡的东西。
func (s *Status) lineWith(kind string) (string, bool) {
	if s == nil {
		return "", false
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return "", false
	}
	head := s.nonce + statusSep + kind + statusSep
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte(head)); ok {
			return string(bytes.TrimSpace(rest)), true
		}
	}
	return "", false
}

// Failure 报告"这一把牢房是不是没建成"，以及 jailer 给的原因。只有写着本次暗号的那一行
// 才算数。
func (s *Status) Failure() (string, bool) { return s.lineWith(statusFail) }

// Execed 报告牢房是不是已经把提交程序 exec 起来了。判机用它把"jailer 没跑成"与"程序
// 自己 exit 70"分开口。
func (s *Status) Execed() bool { _, ok := s.lineWith(statusExeced); return ok }

// peek 是信道上的原始字节，只给测试与诊断看。
func (s *Status) peek() ([]byte, error) {
	if s == nil {
		return nil, errors.New("没有状态信道")
	}
	return os.ReadFile(s.path)
}

// Mode 是操作者选的沙盒强度。Auto 是默认：能开就开，开不起来就在启动日志与 /judges 上
// 写明"未隔离"，此后不再尝试牢房，判题照常。On 是硬要求：启动时真建一次，建不起来就
// 拒绝启动——宁可这台判机不干活，也不悄悄少一层防护。
//
// 两者共同的一条底线：**跑到一半**牢房在可信信道上报告建不起来，判机退出领单（见
// judge.SandboxBroken）。auto 的"判题照常"说的是这台机器一开始就开不了牢房，不是隔离
// 中途没了还继续。
type Mode int

const (
	Off Mode = iota
	Auto
	On
)

func (m Mode) String() string {
	switch m {
	case Off:
		return "off"
	case On:
		return "on"
	}
	return "auto"
}

// ParseMode 只认 off/auto/on：拼错的沙盒开关比没有开关更危险。
func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return Auto, nil
	case "off", "none", "0":
		return Off, nil
	case "on", "1":
		return On, nil
	}
	return Off, fmt.Errorf("-sandbox 只认 auto/on/off，给了 %q", raw)
}

// Spec 是父进程交给 jailer 的全部内容，JSON 之后走命令行参数。
type Spec struct {
	// Dir 是这次运行的工作目录，也是牢房里唯一可写的地方。
	Dir string `json:"dir"`
	// Argv 是真正要跑的程序。
	Argv []string `json:"argv"`
	Env  []string `json:"env"`

	TimeMS int `json:"time_ms"`
	MemKB  int `json:"mem_kb"`
	Procs  int `json:"procs"`

	// UserNS 为真表示这个牢房靠非特权 user namespace 撑起来（判机不是 root）。
	// 两种形态最后都把身份换成一个在宿主上什么都不是的 uid。
	UserNS bool `json:"user_ns"`
	// UID/GID 是牢房内的身份。UserNS 下它是个未映射的号，宿主看到的是 nobody。
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`

	// Binds 是牢房里要按原路径只读出现的系统目录：编译要工具链，跑要动态库。
	Binds []string `json:"binds"`
	// Dev 是牢房里要 bind 进去的设备文件（新 mount 命名空间里没有现成的 /dev）。
	Dev []string `json:"dev"`

	// Status 与 Nonce 是这条一次一用的状态信道：判机在 spawn 之前建好文件、生成暗号，
	// jailer 在进牢房之前握住 fd，只用它说两件事实——"没能 exec 起来（原因是…）"与
	// "exec 起来了"。判机只认写着**这一把暗号**的行。
	//
	// 为什么不能拿 stderr 当判据：那份输出同时是编译器与选手程序的，选手打印一行
	// `OJ-JAIL: ...` 就能让判机把这台机器的牢房永久关掉（本机实测过）。空表示这一把
	// 没有信道（手工跑 jailer，或判机建文件失败），此时 jailer 只能退回 stderr。
	Status string `json:"status,omitempty"`
	Nonce  string `json:"nonce,omitempty"`
}

// encode 把 Spec 变成 --oj-jail= 的参数值。
func (s Spec) encode() (string, error) {
	blob, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return jailFlag + "=" + string(blob), nil
}

// Wrap 把真正的命令行包成"跑这一个牢房"的命令行。self 是判机自己的可执行文件
// 路径（Linux 上用 /proc/self/exe，它不受后续 chroot 影响）。
func Wrap(self string, spec Spec) ([]string, error) {
	arg, err := spec.encode()
	if err != nil {
		return nil, err
	}
	return []string{self, arg}, nil
}

// IsJailer 报告这条进程命令行是不是 jailer 入口。
func IsJailer(args []string) bool {
	return len(args) > 1 && strings.HasPrefix(args[1], jailFlag+"=")
}

// Strip 把 jailer 参数从命令行里摘掉，留给 flag 解析的是人看的那几个。
func Strip(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if i == 1 && strings.HasPrefix(a, jailFlag+"=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// Main 是 cmd/fast-judge 在 main 最开头调的入口：如果自己是 jailer，就建牢房、
// exec 提交程序，永不返回；建不起来就在状态信道上写下原因再退出。
func Main() {
	if !IsJailer(os.Args) {
		return
	}
	spec, err := decodeSpec(os.Args[1])
	// 信道必须在进牢房之前握住：chroot 之后那个路径就不存在了，而失败可能落在 chroot
	// 前后的任意一步。fd 带 O_CLOEXEC（Go 打开文件一律带上），所以 exec 之后的选手程序
	// 连这个 fd 都没有——它写不出这条信道。
	var st *Status
	if err == nil {
		st, _ = take(spec)
		err = Enter(spec, st)
	}
	if err == nil {
		return
	}
	st.markFail(err)
	fmt.Fprintf(os.Stderr, "%s%v\n", ErrPrefix, err)
	os.Exit(FailExit)
}

func decodeSpec(arg string) (Spec, error) {
	var s Spec
	raw, ok := strings.CutPrefix(arg, jailFlag+"=")
	if !ok {
		return s, errors.New("not a jail spec")
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("spec: %w", err)
	}
	if s.Dir == "" || len(s.Argv) == 0 {
		return s, errors.New("spec needs dir and argv")
	}
	return s, nil
}
