//go:build linux

package jail

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// 牢房里要按原路径只读出现的系统目录：编译器要工具链，跑起来要动态链接器和 libc。
// 少了哪个就跳过哪个，不同发行版的路径本来就不一样（merged-usr 把 /bin 做成 /usr/bin
// 的符号链接）。/etc 故意不 bind：glibc 找不到 ld.so.cache 会退回默认搜索路径，
// 而少一个目录就少一分"把宿主的账号列表端给提交程序"的可能。
var defaultBinds = []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"}

// 提交程序要用的设备节点。新 mount 命名空间里看不到宿主 /dev 以外的东西，但 /dev
// 本身是继承来的，所以逐个 bind 成只读，而不是挂一个空目录进去。
var defaultDev = []string{"null", "zero", "random", "urandom", "full"}

// NamespaceFlags 是牢房要开的命名空间。判机不是 root 时得多开一个 user namespace，
// 否则 mount/chroot/新网络命名空间这些都会被内核拒掉。
//
// 不含 CLONE_NEWPID：Go 是在 fork 之后用 unshare() 开命名空间的，而
// unshare(CLONE_NEWPID) 只把**调用者的子进程**放进新 pid 命名空间，被 exec 的程序
// 自己还在原来那个——留着它非但拿不到"init 一死全组清零"的好处，还会让被 exec 起来的
// Go 程序（判机自己）在起线程时 EINVAL。要真吃到 pid 命名空间，得让 jailer 再 fork 一层。
func NamespaceFlags(userNS bool) uintptr {
	flags := uintptr(syscall.CLONE_NEWNS | syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS)
	if userNS {
		flags |= syscall.CLONE_NEWUSER
	}
	return flags
}

// Binds 与 Dev 是牢房要放进来的东西，导出来给调用方填进 Spec。
func Binds() []string { return append([]string(nil), defaultBinds...) }
func Dev() []string   { return append([]string(nil), defaultDev...) }

// Enter 是 jailer 的正文：此刻进程已经在新的命名空间里，剩下的是把牢房钉死，
// 然后 exec 真正的程序。顺序是有讲究的——挂载要在 chroot 之前，chroot 要在降权之前
// （降了权就挂不动了），rlimit 要在 exec 之前（它跟着进程走）。
//
// st 是这一把的状态信道：成功走到 exec 前一刻就在那里留一个"exec 起来了"的记号，
// 任何一步失败由调用方（Main）写上原因。判机拿这两个记号区分"牢房没建起来"与
// "程序自己 exit 70"——它不再需要从不可信的 stderr 里猜。
func Enter(spec Spec, st *Status) error {
	root, err := filepath.Abs(spec.Dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("工作目录不可用: %v", err)
	}
	// 私有挂载：不这么来一下，牢房里的 mount/umount 会传播回宿主，那是最糟的一种泄漏。
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("mount private: %w", err)
	}
	if err := checkNoNetwork(); err != nil {
		return err
	}
	// 降权之后还要能在这目录里写，所以先把权限打开；它判完就删。
	if err := os.Chmod(root, 0o777); err != nil {
		return fmt.Errorf("chmod workdir: %w", err)
	}
	if err := fillJail(root, spec); err != nil {
		return err
	}
	if err := syscall.Chroot(root); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	// 先降权，再上限额。RLIMIT_NPROC 是按 **real uid 现有的任务数**算的：在还是 root 的
	// 时候设成 64，判的是"root 在这台机器上已经有多少进程"，于是宿主上 root 一多，牢房
	// 就会间歇建不起来（unshare/clone 报 EAGAIN，判机只能给一条判机故障）。挪到降权之后，
	// 64 这个数字才真是它想要的意思：这个学生的程序能开多少进程。
	if err := drop(spec); err != nil {
		return err
	}
	if err := applyLimits(spec); err != nil {
		return err
	}
	bin, err := exec.LookPath(inJail(spec.Argv[0], root))
	if err != nil {
		return fmt.Errorf("牢房里找不到 %s: %w（它所在的目录不在 bind 进来的那几个里：/usr /lib /lib64 /bin /sbin）",
			spec.Argv[0], err)
	}
	argv := make([]string, 0, len(spec.Argv))
	for _, a := range spec.Argv {
		argv = append(argv, inJail(a, root))
	}
	// seccomp 必须压在 exec 前面最后一件事：牢房自己还要用 unshare/mount/chroot 这些
	// 被挡的调用，早装一步就死在建牢房半路上。
	if err := applySeccomp(); err != nil {
		return fmt.Errorf("系统调用过滤装不上: %w", err)
	}
	// exec 之前的最后一笔：留下"提交程序真的被牢房交出去了"的记号。写在 seccomp 之后
	// 是有意的——过滤器已经装上，这一笔 write 必须在白名单里（write 本来就在）。
	st.markExeced()
	return syscall.Exec(bin, argv, jailEnv(spec.Env, root))
}

// inJail 把宿主绝对路径换成 chroot 之后的路径。/tmp/oj/12/main 变成 /main，
// 而 /usr/bin/g++ 本来就在牢房里按原路径 bind 进来了，保持不动。
func inJail(path, root string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return "/" + filepath.ToSlash(rel)
}

// jailEnv 把指向宿主工作目录的几个变量改成牢房内的写法：runEnv 给的是宿主路径，
// chroot 之后那些路径根本不存在。
func jailEnv(env []string, root string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && (k == "TEMP" || k == "TMP" || k == "TMPDIR" || k == "HOME" || k == "USERPROFILE") {
			v = "/"
		}
		out = append(out, k+"="+v)
		_ = ok
	}
	return out
}

// fillJail 在 chroot 之前把牢房里要用到的东西摆好：系统目录只读 bind、几个设备节点、
// 一个只属于这个牢房的 /proc，以及一个可写的 /tmp。
func fillJail(root string, spec Spec) error {
	binds := spec.Binds
	if len(binds) == 0 {
		binds = defaultBinds
	}
	for _, dir := range binds {
		info, err := os.Lstat(dir)
		if err != nil {
			continue // 这个发行版没有这个路径，不是错误
		}
		target := filepath.Join(root, dir)
		if info.Mode()&os.ModeSymlink != 0 {
			// merged-usr：宿主上 /bin -> usr/bin，牢房里照抄这个符号链接就行，
			// 因为 /usr 已经整个 bind 进来了。
			link, err := os.Readlink(dir)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(link, target); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := bindRO(dir, target); err != nil {
			return fmt.Errorf("bind %s: %w", dir, err)
		}
	}
	dev := spec.Dev
	if len(dev) == 0 {
		dev = defaultDev
	}
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		return err
	}
	for _, name := range dev {
		from := filepath.Join("/dev", name)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		to := filepath.Join(root, "dev", name)
		// bind 一个设备节点要求挂载点文件先存在——目录不够，得是个空文件；真正的
		// 设备随后被 bind 盖在上面。
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		if f, err := os.OpenFile(to, os.O_CREATE|os.O_RDONLY, 0o666); err != nil {
			return err
		} else {
			f.Close()
		}
		if err := bindRO(from, to); err != nil {
			return fmt.Errorf("bind %s: %w", from, err)
		}
	}
	// 继承来的 /proc 属于宿主那个 pid 命名空间：不换成新的，牢房里的程序就能数清
	// 判机上跑着谁的哪道题。
	procDir := filepath.Join(root, "proc")
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		return err
	}
	// hidepid=2：连自己的 pid 之外的一律看不见。光换 /proc 只挡住"数别人的题"，
	// 而 /proc/<别人的 pid>/cmdline、environ、stat 那些还摆在那里——判机上同时跑着
	// 别人的源码路径与判机自己的命令行，那些不该是一个提交能读的东西。
	if err := syscall.Mount("proc", procDir, "proc",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "hidepid=2,inode64"); err != nil {
		// inode64 在个别老内核上不被 proc 接受，而 hidepid 才是这次要的那条：
		// 退一步只报 hidepid=2，不要因为一个装饰性选项把整间牢房关掉。
		if err2 := syscall.Mount("proc", procDir, "proc",
			syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "hidepid=2"); err2 != nil {
			return fmt.Errorf("mount /proc: %w", err)
		}
	}
	tmpDir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o777); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", tmpDir, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "size=64m,mode=1777"); err != nil {
		return fmt.Errorf("mount /tmp: %w", err)
	}
	return nil
}

// bindRO 把一个路径挂到牢房里，并且立刻 remount 成只读。bind 与 remount 是两步，
// 少第二步的话"只读"只是注释里写着好看。
func bindRO(from, to string) error {
	if err := syscall.Mount(from, to, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return err
	}
	return syscall.Mount(from, to, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_REC, "")
}

// checkNoNetwork 确认这个命名空间里真的没有网卡。新网络命名空间只有一条 DOWN 状态
// 的 lo；看到别的东西说明 CLONE_NEWNET 没生效，那就别骗自己了。
func checkNoNetwork() error {
	ifs, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("读网卡列表: %w", err)
	}
	for _, ifc := range ifs {
		if ifc.Name != "lo" {
			return fmt.Errorf("牢房里还能看到网卡 %s，CLONE_NEWNET 没生效", ifc.Name)
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil // 地址列表读不到不影响"没有网卡"这个结论
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			return fmt.Errorf("牢房里还有非回环地址 %s", ipnet.IP)
		}
	}
	return nil
}

// Linux 的 syscall 包不带 RLIMIT_* 常量（那是 BSD 那边有的），编号是 asm-generic 的。
const (
	rlimitCPU    = 0
	rlimitFSIZE  = 1
	rlimitCORE   = 4
	rlimitNPROC  = 6
	rlimitNOFILE = 7
	rlimitAS     = 9
)

// applyLimits 交给内核去兜：CPU 秒、地址空间、进程数、单文件大小、fd 数、core dump。
// 判定用的仍是判机实测的 rusage 与 RSS 熔断，这里只是"判机万一没杀掉"时的第二道闸。
//
// RLIMIT_AS 故意留了 512 MB 余量：卡得太紧会让超内存的申请直接 ENOMEM 崩掉，
// 报成 RE 而不是 MLE，那是把判题口径搞坏。
func applyLimits(spec Spec) error {
	const (
		fsizeBytes = 64 << 20
		asSlackKB  = 512 << 10
		nofile     = 512
	)
	type rlim struct {
		res  int
		cur  uint64
		hard uint64
		what string
	}
	limits := []rlim{
		{rlimitCORE, 0, 0, "core dump"},
		{rlimitFSIZE, fsizeBytes, fsizeBytes, "单文件 64MB"},
		{rlimitNOFILE, nofile, nofile, "fd 512"},
	}
	if spec.Procs > 0 {
		limits = append(limits, rlim{rlimitNPROC, uint64(spec.Procs), uint64(spec.Procs), "进程数"})
	}
	if spec.TimeMS > 0 {
		// 比判机的墙上时钟宽两秒：正常超时由判机先杀并给出实测值，这里只兜"判机
		// 没杀掉"的情况。软限先发 SIGXCPU，硬限再兜一层。
		secs := uint64(spec.TimeMS/1000 + 2)
		limits = append(limits, rlim{rlimitCPU, secs, secs + 2, "CPU 秒"})
	}
	if spec.MemKB > 0 {
		as := uint64(spec.MemKB+asSlackKB) * 1024
		limits = append(limits, rlim{rlimitAS, as, as, "地址空间"})
	}
	for _, l := range limits {
		rl := syscall.Rlimit{Cur: l.cur, Max: l.hard}
		if err := syscall.Setrlimit(l.res, &rl); err != nil {
			return fmt.Errorf("setrlimit %s: %w", l.what, err)
		}
	}
	return nil
}

// drop 换成一个在宿主上什么都不是的身份。userns 形态下这是个未映射的号：宿主看到
// 的是 nobody，于是判机的库、别人的源码、判机自己的环境全都读不到。
func drop(spec Spec) error {
	uid, gid := int(spec.UID), int(spec.GID)
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	if err := syscall.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	if os.Getuid() != uid {
		return fmt.Errorf("降权没生效：还是 uid %d", os.Getuid())
	}
	return nil
}
