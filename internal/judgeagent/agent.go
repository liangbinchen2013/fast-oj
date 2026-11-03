// Package judgeagent is the other half of the wire protocol: it pulls work from
// the server, runs it locally and reports back. Keeping it importable means the
// whole loop can be tested without spawning a second process.
package judgeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/judge"
	"github.com/liangbinchen2013/fast-oj/internal/protocol"
)

type Config struct {
	Server string
	Token  string
	Name   string
	Dir    string
	// Poll is how long to wait before asking again when the queue is empty.
	Poll time.Duration
	// Backoff caps the retry wait after a failure.
	Backoff time.Duration
	Tools   map[string]judge.Tool
	// RunAs drops each submission to this uid/gid (Linux and containers only).
	// The compile step keeps the judge's own identity so it can write the binary.
	RunAs *judge.DropTo
	// Languages to advertise; defaults to the keys of Tools.
	Languages []string
	// Note is what /judges shows about this box: whether judged runs are actually
	// jailed, which is the operator's first question about a judge.
	Note string
	Logf func(format string, args ...any)
}

type Agent struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) (*Agent, error) {
	switch {
	case cfg.Server == "":
		return nil, errors.New("server URL is required")
	case cfg.Token == "":
		return nil, errors.New("judge token is required")
	case cfg.Name == "":
		return nil, errors.New("judge name is required")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 500 * time.Millisecond
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 3 * time.Second
	}
	if cfg.Tools == nil {
		cfg.Tools = judge.Available(judge.DefaultTools())
	}
	if len(cfg.Languages) == 0 {
		for name := range cfg.Tools {
			cfg.Languages = append(cfg.Languages, name)
		}
		sort.Strings(cfg.Languages)
	}
	if len(cfg.Tools) == 0 {
		return nil, errors.New("no usable toolchain found (need g++ on PATH)")
	}
	if cfg.Dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		cfg.Dir = filepath.Join(base, "fast-oj-judge")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	// No client-wide timeout: the deadline belongs to each request's context, so
	// a long compile is not cut off by a global default.
	return &Agent{cfg: cfg, http: &http.Client{}}, nil
}

func (a *Agent) Languages() []string { return a.cfg.Languages }

// Dir 是每次运行的工作目录所在，启动时的沙盒自测要用它。
func (a *Agent) Dir() string { return a.cfg.Dir }

// SetNote 在自测之后补一句给 /judges 看的话：沙盒到底开没开，只有真建过一次才知道。
func (a *Agent) SetNote(note string) { a.cfg.Note = note }

// DisableLanguage 把一门语言从这台判机的名单上摘掉，并把原因挂在 /judges 那句备注里。
// 启动自测没过就用它：站点宁可不摆这个选项，也不会收到一条"能交但判不了"的判机故障，
// 更不会收到一条"能交、也跑了，只是跑得没隔离"。
func (a *Agent) DisableLanguage(name, why string) {
	delete(a.cfg.Tools, name)
	kept := a.cfg.Languages[:0]
	for _, l := range a.cfg.Languages {
		if l != name {
			kept = append(kept, l)
		}
	}
	a.cfg.Languages = kept
	if a.cfg.Note == "" {
		a.cfg.Note = name + " 不可用：" + why
		return
	}
	a.cfg.Note += "；" + name + " 不可用：" + why
}

func (a *Agent) greeting() protocol.Greeting {
	// 隔离状态是"此刻"而不是"这一版会"：沙盒开不起来时这个值会翻过去，操作员在 /judges
	// 上当场看得到，而不是要去翻启动日志的那一行。
	jailed := judge.Jailed()
	return protocol.Greeting{
		Name: a.cfg.Name, Languages: a.cfg.Languages,
		// 自报的是这一版代码真会做的事：需要新能力的工单只会派给报了它的机器。
		Capabilities: protocol.Capabilities,
		Jailed:       &jailed,
		Note:         a.cfg.Note,
	}
}

// request sends an authenticated call and returns the status plus raw body.
func (a *Agent) request(ctx context.Context, method, path string, in any, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.Server+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %w", method, path, err)
		}
	} else if out != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: empty body", method, path)
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s", method, path, firstLine(raw))
	}
	return resp.StatusCode, nil
}

func firstLine(raw []byte) string {
	s := string(raw)
	if i := indexOf(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Claim takes the next submission, or nil when the queue is empty. The same call
// doubles as the heartbeat that keeps this box listed on the server.
func (a *Agent) Claim(ctx context.Context) (*protocol.Task, error) {
	var task protocol.Task
	code, err := a.request(ctx, http.MethodPost, "/judge/claim", a.greeting(), &task)
	if code == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if task.SubmissionID == 0 {
		return nil, errors.New("claim returned a task without an id")
	}
	return &task, nil
}

// caseFile 取这份工单里第 ordinal 条用例的正文。地址是「这条提交的第 N 条」而不是
// 「用例 id 为 X」：服务器会核对这条提交确实派给了本机，判机因此读不到别的题目的数据。
func (a *Agent) caseFile(ctx context.Context, submissionID int64, ordinal int, which string) ([]byte, error) {
	path := "/judge/submissions/" + strconv.FormatInt(submissionID, 10) +
		"/cases/" + strconv.Itoa(ordinal) + "/" + which +
		"?judge=" + url.QueryEscape(a.cfg.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Case bodies are capped server side, and this is a second belt.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sid %d case #%d %s: %s", submissionID, ordinal, which, firstLine(body))
	}
	return body, nil
}

func (a *Agent) PostStatus(ctx context.Context, submissionID int64, state, detail string) error {
	_, err := a.request(ctx, http.MethodPost,
		"/judge/submissions/"+strconv.FormatInt(submissionID, 10)+"/status",
		protocol.Status{Name: a.cfg.Name, State: state, Detail: detail}, nil)
	return err
}

func (a *Agent) PostResult(ctx context.Context, submissionID int64, rep judge.Report) error {
	result := protocol.Result{
		Name:       a.cfg.Name,
		Verdict:    string(rep.Verdict),
		TimeMS:     rep.TimeMS,
		MemKB:      rep.MemKB,
		TimeMin:    rep.TimeMin,
		MemMin:     rep.MemMin,
		FailedCase: rep.FailedCase,
		Detail:     rep.Detail,
		Toolchain:  rep.Toolchain,
	}
	for _, row := range rep.Runs {
		result.Runs = append(result.Runs, protocol.RunRow{
			Ordinal: row.Ordinal, CaseName: row.CaseName, Verdict: string(row.Verdict),
			TimeMS: row.TimeMS, MemKB: row.MemKB, IsSample: row.IsSample,
			TimeMin: row.TimeMin, MemMin: row.MemMin,
		})
	}
	_, err := a.request(ctx, http.MethodPost,
		"/judge/submissions/"+strconv.FormatInt(submissionID, 10)+"/result", result, nil)
	return err
}

// PostRun reports one finished case while the rest of the submission is still being
// judged, which is what lets the detail page fill in as it goes. A failed progress
// report is not worth stopping for: the final result carries every row anyway.
func (a *Agent) PostRun(ctx context.Context, submissionID int64, detail string, row judge.RunRow) error {
	_, err := a.request(ctx, http.MethodPost,
		"/judge/submissions/"+strconv.FormatInt(submissionID, 10)+"/status",
		protocol.Status{
			Name: a.cfg.Name, State: string(judge.Running), Detail: detail,
			Run: &protocol.RunRow{
				Ordinal: row.Ordinal, CaseName: row.CaseName, Verdict: string(row.Verdict),
				TimeMS: row.TimeMS, MemKB: row.MemKB, IsSample: row.IsSample,
				TimeMin: row.TimeMin, MemMin: row.MemMin,
			},
		}, nil)
	return err
}

// ProcessOnce handles a single claimed submission. It reports true when work was
// found. Results are always reported, including "we could not do this at all",
// so a submission never sits in compiling without an answer.
func (a *Agent) ProcessOnce(ctx context.Context) (bool, error) {
	task, err := a.Claim(ctx)
	if err != nil || task == nil {
		return false, err
	}
	a.cfg.Logf("claimed sid=%d problem=%s lang=%s options=%q cases=%d",
		task.SubmissionID, task.PID, task.Language, task.Options, len(task.Cases))

	tool, ok := a.cfg.Tools[task.Language]
	if !ok {
		rep := judge.Report{Verdict: judge.IE, TimeMS: -1, MemKB: -1,
			Detail: "this judge box has no toolchain for " + task.Language}
		return true, a.PostResult(ctx, task.SubmissionID, rep)
	}
	// 闸门（启动时已经用自测把 Python 从名单上摘过一次，这里是第二道）：没牢房就不跑
	// Python。护栏拦不住的那一类操作，恰好是"这台机器本来就是判机的"这一件事。
	if !judge.Jailed() && wantsPython(task) {
		return true, a.postJudgeError(ctx, task.SubmissionID,
			fmt.Errorf("这台判机没有可用的沙盒（%s），不收 Python：%s 的护栏要靠 chroot 才能画出边界，"+
				"没有牢房时它拦不住想出去的东西。要么把 -sandbox 开起来，要么这道题别用 Python（含判题器与交互器）",
				judge.SandboxNote(), judge.PythonLanguage))
	}
	// 版本选项只有命中这台判机的候选表才会被拼进命令行。命中不了就报 IE 并把拒绝的话
	// 交回去：把工单按另一个版本偷偷编出来，比判不了更不能接受。
	picked, label, err := tool.WithOptions(task.Options)
	if err != nil {
		rep := judge.Report{Verdict: judge.IE, TimeMS: -1, MemKB: -1, Detail: err.Error()}
		return true, a.PostResult(ctx, task.SubmissionID, rep)
	}
	tool = picked
	if label != "" {
		a.cfg.Logf("sid=%d 版本：%s", task.SubmissionID, label)
	}

	dir := filepath.Join(a.cfg.Dir, strconv.FormatInt(task.SubmissionID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		rep := judge.Report{Verdict: judge.IE, TimeMS: -1, MemKB: -1, Detail: "workdir: " + err.Error()}
		return true, a.PostResult(ctx, task.SubmissionID, rep)
	}
	// Each submission gets a private, freshly created directory, and it is gone
	// when the run ends.
	defer os.RemoveAll(dir)

	// 题目自带的判题器与交互器先在这儿编好。编不过时交出去的必须是判机故障：一份本该
	// 正确的代码因为作者写坏了判题器而被判 WA，是本功能最不能出现的那种错。
	var spj, itx []string
	if task.Checker != nil {
		built, err := judge.BuildAux(a.cfg.Tools, judge.AuxProgram{
			Language: task.Checker.Language, Source: task.Checker.Source,
		}, dir, judge.AuxChecker)
		if err != nil {
			return true, a.postJudgeError(ctx, task.SubmissionID, err)
		}
		spj = built
	}
	if task.Interactor != nil {
		built, err := judge.BuildAux(a.cfg.Tools, judge.AuxProgram{
			Language: task.Interactor.Language, Source: task.Interactor.Source,
		}, dir, judge.AuxInteractor)
		if err != nil {
			return true, a.postJudgeError(ctx, task.SubmissionID, err)
		}
		itx = built
	}
	// 两个同时配上必须当面拒绝，不能"交互器说了算，判题器静默不跑"：出题人以为自己配了
	// 两层判定，实际有一层从来没生效过。
	if len(spj) > 0 && len(itx) > 0 {
		return true, a.postJudgeError(ctx, task.SubmissionID,
			errBothPrograms(task.PID))
	}

	cases := make([]judge.Case, 0, len(task.Cases))
	for _, c := range task.Cases {
		input, err := a.caseFile(ctx, task.SubmissionID, c.Ordinal, "in")
		if err != nil {
			return true, a.reportInfra(ctx, task.SubmissionID, "fetch input: "+err.Error())
		}
		expected, err := a.caseFile(ctx, task.SubmissionID, c.Ordinal, "out")
		if err != nil {
			return true, a.reportInfra(ctx, task.SubmissionID, "fetch answer: "+err.Error())
		}
		cases = append(cases, judge.Case{
			Name: c.Name, Ordinal: c.Ordinal, IsSample: c.IsSample,
			Score: c.Score, Input: input, Expected: expected,
		})
	}

	if err := a.PostStatus(ctx, task.SubmissionID, string(judge.Running),
		fmt.Sprintf("%d cases", len(cases))); err != nil {
		a.cfg.Logf("status report failed (continuing): %v", err)
	}

	total := len(cases)
	progress := func(row judge.RunRow) {
		a.reportProgress(ctx, task.SubmissionID, row, total)
	}

	started := time.Now()
	rep := judge.Judge(judge.Task{
		SubmissionID: task.SubmissionID, PID: task.PID, Language: task.Language,
		Source: task.Source, TimeLimitMS: task.TimeLimitMS, MemLimitKB: task.MemLimitKB,
		SPJ: spj, Interactor: itx, Cases: cases,
	}, tool, dir, a.cfg.RunAs, judge.Progress(progress))
	a.cfg.Logf("sid=%d verdict=%s score=%d/%d time=%dms mem=%dKB wall=%s%s",
		task.SubmissionID, rep.Verdict, rep.Score, rep.TotalScore, rep.TimeMS, rep.MemKB,
		time.Since(started).Truncate(time.Millisecond), detailSuffix(rep.Detail))

	return true, a.PostResult(ctx, task.SubmissionID, rep)
}

// detailSuffix keeps the judge-side reason in the log line: an IE verdict on its
// own tells the operator nothing.
func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " detail=" + strings.ReplaceAll(detail, "\n", " ")
}

// reportProgress publishes one case row with a "3/10 · case AC" note, so the
// submission page and the queue both show how far the run got.
func (a *Agent) reportProgress(ctx context.Context, submissionID int64, row judge.RunRow, total int) {
	detail := fmt.Sprintf("%d/%d · %s %s", row.Ordinal, total, row.CaseName, row.Verdict)
	if err := a.PostRun(ctx, submissionID, detail, row); err != nil {
		a.cfg.Logf("progress report failed (continuing): %v", err)
	}
}

// wantsPython 报告这份工单里有没有要靠 Python 解释器跑的东西：学生的提交、判题器、
// 交互器都算。闸门问的是"这台机器会不会把一段 Python 关在牢房外执行"，三份里任何一份
// 都是同一个答案。
func wantsPython(task *protocol.Task) bool {
	if task.Language == judge.PythonLanguage {
		return true
	}
	if task.Checker != nil && task.Checker.Language == judge.PythonLanguage {
		return true
	}
	return task.Interactor != nil && task.Interactor.Language == judge.PythonLanguage
}

// postJudgeError 把"判机侧自带的程序出了问题"交回服务器：判定一律是判机故障。
// 学生的代码不为一个编不过的判题器背 CE，也不为一个跑飞的交互器背 WA——那两类错误会让人
// 去改一份也许本来就正确的代码。
func (a *Agent) postJudgeError(ctx context.Context, submissionID int64, err error) error {
	return a.PostResult(ctx, submissionID, judge.Report{
		Verdict: judge.IE, TimeMS: -1, MemKB: -1,
		Detail: err.Error(), FailedCase: "judge program",
	})
}

// errBothPrograms 是"同一题同时配了判题器和交互器"。不能默默让其中一个不跑：
// 出题人以为自己有两层判定，实际那一层从来没生效过。
func errBothPrograms(pid string) error {
	return fmt.Errorf("%s 同时配了判题器与交互器：交互题由交互器判定，判题器不会被跑到。两者只能留一个", pid)
}

func (a *Agent) reportInfra(ctx context.Context, submissionID int64, detail string) error {
	return a.PostResult(ctx, submissionID, judge.Report{
		Verdict: judge.IE, TimeMS: -1, MemKB: -1, Detail: detail})
}

// Run polls until the context ends. A failed round waits Backoff rather than
// spinning, so an unreachable server does not turn into a hot loop.
func (a *Agent) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		worked, err := a.ProcessOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.cfg.Logf("round failed: %v", err)
			if !sleep(ctx, a.cfg.Backoff) {
				return ctx.Err()
			}
			continue
		}
		if !worked && !sleep(ctx, a.cfg.Poll) {
			return ctx.Err()
		}
		// 牢房在中途建不起来了——这件事来自 jailer 在那条一次一用信道上的亲口报告，不是
		// 来自任何输出文本。判机此时已经不能兑现它对外承诺的那层隔离，继续领单就是把下
		// 一条提交放在裸机上跑，所以退出进程：systemd 会重启它，重启时的启动自测重新回答
		// "这台机器到底开不开得了牢房"，并按 -sandbox 的语义要么拒绝启动、要么把"未隔离"
		// 写在日志与 /judges 上。宁可停摆，不悄悄降级。
		if judge.SandboxBroken() {
			return fmt.Errorf("牢房在中途建不起来了（%s）：停止领单", judge.SandboxNote())
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
