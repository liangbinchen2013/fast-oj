package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Addr       string
	DataDir    string
	DBPath     string
	AdminToken string
	JudgeToken string
	MaxSource  int64
	// MaxUpload 与 MaxCase 都是**部署方选的闸门**，代码里不设默认天花板：0（默认）就是
	// 不限量，出题人要存多大的测试点是他的事，站点不替他决定。填正数才设闸。
	// 代价写清楚：整包 zip 与包内每一条正文现在都是一次性读进内存的，所以不限量意味着
	// 一次导入按包的大小占内存，判机判这条时也按正文大小占内存（见 judge 里 tc.Input）。
	// 谁要把这道闸门压回去，就填一个正数——它挡的从来不是"合法的大用例"，而是无界的上传。
	MaxUpload int64
	MaxCase   int64
	BCryptCost int
	Secure     bool
	// AllowRegister gates self-service signup; M10 moves the switch into the
	// site settings the operator can edit in the browser.
	AllowRegister bool
	// RegisterPerMinute is how many signups one machine may submit per minute.
	// 默认 1 是防刷；开学那天全班排队注册，把它调到 6 就够一节课用了。0 或负数按 1 处理。
	RegisterPerMinute int
	// SubmitPerMinute 是一个人每分钟能提交几次。默认 10：正常做题一道题试三四次也就
	// 十几条，够；拿它刷队列（判机是共享资源，队列深度是所有人的等待时间）就不够了。
	// 0 或负数按"不限"处理——这一条与注册不同，注册默认必须限，提交不该把赶时间的人挡在门外。
	SubmitPerMinute int
	// ProblemPerMinute 是一个人每分钟能建几道题。建题会带出用例文件与目录，是比提交更贵
	// 的动作；批量导入题库的人靠的是 zip 与脚本，不是这个。
	ProblemPerMinute int
	// HumanVerifyMax 是「伪人机验证」那次等待的上限（秒）。同一个出口 IP 注册得越多、
	// 登录失败得越多，要等的秒数就越往上加，这里给它的头顶封顶；负数与 0 都是关掉整套
	// ——全班共用一个 NAT 出口的那天，这是最快的降压阀，关掉之后剩下的是原来那两条限流。
	HumanVerifyMax int
	Verbose        bool
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt 给一个整数型环境变量的默认值：非数字或空都按默认，不报错——部署脚本里
// 写错一个数不该让服务起不来，而那条默认值本身就是安全的那一侧。
func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// envInt64 是 envInt 那一条给字节数用的版本：闸门是 64 位的，把 5 GB 写进环境变量
// 再被 32 位截成一个奇怪的数，比读不到还难查。
func envInt64(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func defaultDataDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "data"
	}
	return filepath.Join(wd, "data")
}

func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr must not be empty")
	}
	// AdminToken 允许留空，而且留空才是更安全的默认：服务端会自己生成一枚一次性令牌，
	// 只在日志里打印一次。写死在 env 里的那一串永久有效，而 env 文件、数据库备份、
	// /proc 都是它的暴露面——见 internal/store/admintoken.go。
	if c.JudgeToken == "" {
		return fmt.Errorf("judge token must not be empty (set -judge-token or FASTOJ_JUDGE_TOKEN)")
	}
	if c.MaxSource <= 0 {
		return fmt.Errorf("max source size must be positive")
	}
	if c.MaxUpload < 0 {
		c.MaxUpload = 0
	}
	if c.MaxCase < 0 {
		c.MaxCase = 0
	}
	// 0 = 不限量（见结构体那段），所以这里不再有"必须为正"这一条；也不再有
	// max-case ≤ max-upload：单条本来就不该比整包大，那个不等式只在两个都设了数字时
	// 才是人能理解的话，而它挡住过"只想放宽单条"的部署方。
	if c.BCryptCost < 10 || c.BCryptCost > 14 {
		return fmt.Errorf("bcrypt cost must be 10..14")
	}
	if c.RegisterPerMinute <= 0 {
		// 0 does not mean "unlimited", it means unset: a site nobody can register for
		// would look like a bug, and one everybody can flood looks like one too.
		c.RegisterPerMinute = 1
	}
	if c.HumanVerifyMax < 0 {
		// 关掉的写法只留一种：0。负数是笔误，按关掉的语义处理而不是让它起不来——
		// 部署脚本里多打一个减号不该让站点发不动。
		c.HumanVerifyMax = 0
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}
	c.DataDir = abs
	if c.DBPath == "" {
		c.DBPath = filepath.Join(c.DataDir, "fast-oj.db")
	}
	return os.MkdirAll(filepath.Dir(c.DBPath), 0o750)
}

func Load(args []string) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("fast-oj", flag.ContinueOnError)
	fs.StringVar(&c.Addr, "addr", envOr("FASTOJ_ADDR", "127.0.0.1:8080"), "listen address")
	fs.StringVar(&c.DataDir, "data", envOr("FASTOJ_DATA", defaultDataDir()), "data directory")
	fs.StringVar(&c.DBPath, "db", os.Getenv("FASTOJ_DB"), "sqlite file (default <data>/fast-oj.db)")
	fs.StringVar(&c.AdminToken, "admin-token", os.Getenv("FASTOJ_ADMIN_TOKEN"),
		"一次性运维令牌（留空则由服务端生成并打印一次；用一次即作废）")
	fs.StringVar(&c.JudgeToken, "judge-token", os.Getenv("FASTOJ_JUDGE_TOKEN"), "token granting judge access")
	fs.Int64Var(&c.MaxSource, "max-source", 256<<10, "maximum accepted source/json body bytes")
	fs.Int64Var(&c.MaxUpload, "max-upload", envInt64("FASTOJ_MAX_UPLOAD", 0),
		"用例包上传的字节上限（0=不限，默认不限；填正数才设闸）")
	fs.Int64Var(&c.MaxCase, "max-case", envInt64("FASTOJ_MAX_CASE", 0),
		"单个测试点正文的字节上限（0=不限，默认不限：存多大是出题人的事）")
	fs.IntVar(&c.BCryptCost, "bcrypt-cost", 11, "password hash cost (10 is weak, 14 is slow on login)")
	fs.BoolVar(&c.Secure, "secure-cookies", false, "mark session cookies Secure (enable when serving over TLS)")
	noRegister := fs.Bool("no-register", false, "关闭自助注册")
	fs.IntVar(&c.RegisterPerMinute, "register-per-minute", 1, "每台机器每分钟可注册的账号数（开学集体注册时可以调大）")
	submitPerMinute := fs.Int("submit-per-minute", envInt("FASTOJ_SUBMIT_PER_MINUTE", 10),
		"一个人每分钟可提交几次（0=不限；判机是共享资源，队列深度是所有人的等待时间）")
	problemPerMinute := fs.Int("problem-per-minute", 20,
		"一个人每分钟可建几道题（0=不限）")
	fs.IntVar(&c.HumanVerifyMax, "human-verify-max-seconds", envInt("FASTOJ_HUMAN_VERIFY_MAX", 20),
		"人机验证那次等待的秒数上限（越注册/越失败等得越久，到这条就封顶；0=关掉整套验证窗）")
	fs.BoolVar(&c.Verbose, "v", false, "verbose request logging")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	c.AllowRegister = !*noRegister
	c.SubmitPerMinute = *submitPerMinute
	c.ProblemPerMinute = *problemPerMinute
	c.DataDir, _ = filepath.Abs(c.DataDir)
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}
