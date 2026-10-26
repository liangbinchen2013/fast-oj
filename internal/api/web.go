package api

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/judge"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// defaultLanguages is what the submit form offers when a problem does not
// restrict languages; it mirrors judge.DefaultTools().
var defaultLanguages = []string{"c", "cpp", judge.PythonLanguage}

// pythonMinMemKB 是收 Python 的题至少要给的内存。实测 python3.14 空跑常驻 ~9MB、VmPeak
// ~18.5MB，`ulimit -v 32M` 下解释器照样起得来，所以 32MB 是"判的还是学生的程序"那条线；
// 再往下（比如 8MB 的题）判定量的就是解释器，不是他的代码。
const pythonMinMemKB = 32 << 10

func statusLine(status int) string {
	return strconv.Itoa(status) + " " + http.StatusText(status)
}

func statusClass(status int) string {
	if status >= 400 {
		return "bad"
	}
	return ""
}

// --- formatting shared by pages ---

// outcomeOf names what a reader should see as the result of a submission.
func outcomeOf(status, verdict string) (string, string) {
	if judge.Status(status).InFlight() {
		return status, statusLabel(status)
	}
	if verdict != "" {
		return verdict, verdict
	}
	return status, statusLabel(status)
}

func statusLabel(status string) string {
	switch status {
	case "queued":
		return "排队中"
	case "compiling":
		return "编译中"
	case "running":
		return "运行中"
	case "done":
		return "已完成"
	case "abandoned":
		return "判机超时"
	}
	return status
}

func agoMS(ms int64) string {
	if ms == 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(ms))
	if d < 0 {
		return "刚刚"
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + " 秒前"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " 分钟前"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " 小时前"
	default:
		return time.UnixMilli(ms).Local().Format("2006-01-02")
	}
}

// subRow is the shared shape for any submission table.
type subRow struct {
	ID     int64
	Handle string
	// Name is who the row prints as. For a cancelled account that is not Handle:
	// the login name has been freed and may already belong to the next registrant.
	Name string
	// NameColor 是这个名字的活跃度档位类名（blue/green/orange/red/purple/brown 之一，
	// store.colorSQL 已经把空值收成 blue，所以这里可以直接拼进 class）。
	NameColor string
	PID       string
	Title     string
	Language  string
	Class     string
	Outcome   string
	Time      string
	Mem       string
	Score     string
	Ago       string
}

func rowOf(d store.SubmissionDetail) subRow {
	class, outcome := outcomeOf(d.Status, d.Verdict)
	score := "-"
	if d.TotalScore > 0 {
		score = fmt.Sprintf("%d/%d", d.Score, d.TotalScore)
	}
	return subRow{
		ID: d.ID, Handle: d.Handle, Name: personName(d.Name, d.Handle),
		NameColor: d.Color, PID: d.PID, Title: d.Title,
		Language: langCell(d.Language, d.Options, d.Toolchain),
		Class:    class, Outcome: outcome,
		Time: formatMS(d.TimeMS, d.TimeMin), Mem: formatKB(d.MemKB, d.MemMin), Score: score,
		Ago: agoMS(d.CreatedAt),
	}
}

// langCell 是列表里那一格语言。判机自报过口径就显示自报的那一份；没自报时只在选得与判机
// 默认不同时补一句，并标出「未回报」——列表也是拿来核对的，不能把提交时的选择写成判机
// 做过的事。全称在提交详情页上。
func langCell(lang, options, reported string) string {
	if reported != "" {
		return lang + " · " + reported
	}
	if note := versionNote(lang, options); note != "" {
		return lang + " · " + note + "（判机未回报）"
	}
	return lang
}

// personName keeps a label printable when a row has no display name. The store's
// SQL already resolves the cancelled-account case, so this only guards against an
// empty nickname falling through as an invisible link.
func personName(name, handle string) string {
	if strings.TrimSpace(name) == "" {
		return handle
	}
	return name
}

// --- problem list ---

type problemEntry struct {
	PID, Title, Time, Mem  string
	Cases, Solved, Submits int
	Hidden                 bool
}

type problemsData struct {
	Items []problemEntry
	Total int
	Limit int
	Next  int64
	Query string
}

func (s *Server) problemsPage(w http.ResponseWriter, r *http.Request, me *viewer, page store.ProblemPage, limit int, search string) {
	ids := make([]int64, len(page.Items))
	for i, p := range page.Items {
		ids[i] = p.ID
	}
	cases, err := s.db.CaseCounts(r.Context(), ids)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	counts, err := s.db.ProblemCounts(r.Context(), ids)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	rows := make([]problemEntry, 0, len(page.Items))
	for _, p := range page.Items {
		c := counts[p.ID]
		rows = append(rows, problemEntry{
			PID: p.PID, Title: p.Title,
			Time: formatMS(p.TimeLimitMS, false), Mem: formatKB(p.MemLimitKB, false),
			Cases: cases[p.ID], Solved: c.Solved, Submits: c.Submits,
			Hidden: !p.Visible,
		})
	}
	s.page(w, r, http.StatusOK, "题库", "problems", me, msgOf(r), problemsData{
		Items: rows, Total: page.Total, Limit: limit, Next: page.NextAfter, Query: search,
	})
}

func msgOf(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("msg"))
}

// --- problem detail ---

type samplePair struct{ Name, In, Out string }

// problemContest is the contest context a problem page may be shown inside. It is
// built from the ?contest= link and re-checked, never trusted. A zero value means no
// contest, which keeps the templates free of nil dereferences.
type problemContest struct {
	ID         int64
	CID        string
	Title      string
	Label      string
	Phase      string
	PhaseClass string
	Countdown  string
	Back       string
	CanSubmit  bool
	WhyNot     string
}

type problemData struct {
	PID, Title string
	Time, Mem  string
	LangText   string
	Langs      []string
	// Versions 是提交页上的版本下拉：每根轴一份候选，值与判机的候选表同源。
	Versions []versionAxis
	// Mode 是题面页上那句"这题怎么判"（SPJ / 交互题）。普通题是空串，不打扰学生。
	Mode        string
	JudgedCases int
	TotalScore  int
	Solved      int
	Submits     int
	Hidden      bool
	// CanEdit and CanCases are what the page shows the authoring links for: owning
	// a problem is enough for both, testcase.manage only buys the case page.
	CanEdit  bool
	CanCases bool
	// TopicsLink is where the 求助贴 for this problem live.
	TopicsLink string
	Contest    problemContest
	// VerifyNotice is why this reader's 邮箱 still does not let them submit, or "" when
	// nothing is missing. The form is replaced by this sentence rather than left there to
	// fail, which is how 被禁止发动态 that box already behaves.
	VerifyNotice string
	Statement    template.HTML
	Samples      []samplePair
	Recent       []subRow
}

// contestOfProblem turns the ?contest= of a contest problem link into a checked
// context. Anything that does not hold — an unknown id, a problem that is not mounted
// there, a reader who is not inside the contest — answers "no context", which is the
// ordinary problem page. A contest is not announced to someone outside it.
func (s *Server) contestOfProblem(r *http.Request, a actor, p store.Problem) problemContest {
	raw := strings.TrimSpace(r.URL.Query().Get("contest"))
	if raw == "" {
		return problemContest{}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return problemContest{}
	}
	c, err := s.db.GetContestByID(r.Context(), id)
	if err != nil {
		return problemContest{}
	}
	cp, err := s.db.GetContestProblem(r.Context(), c.ID, p.ID)
	if err != nil {
		return problemContest{}
	}
	now := time.Now().UnixMilli()
	access := s.contestAccessFor(r.Context(), c, a, now)
	if !access.ShowProblems {
		return problemContest{}
	}
	phase := c.Phase(now)
	pc := problemContest{
		ID: c.ID, CID: c.CID, Title: c.Title,
		Label: store.ContestLabel(cp.Label, cp.Ordinal),
		Phase: phase.Label(), PhaseClass: phase.Badge(),
		Back: "/contests/" + url.PathEscape(c.CID),
	}
	switch phase {
	case store.PhaseUpcoming:
		pc.Countdown = "距开始还有 " + formatSpan(c.StartAt-now)
	case store.PhaseRunning:
		pc.Countdown = "距结束还有 " + formatSpan(c.EndAt-now)
	case store.PhaseOver:
		pc.Countdown = "已结束"
	}
	pc.CanSubmit = phase == store.PhaseRunning || access.CanManage
	switch {
	case pc.CanSubmit:
	case phase == store.PhaseUpcoming:
		pc.WhyNot = "这场比赛还没开始，开始后这里才会收代码。"
	default:
		pc.WhyNot = "这场比赛已经结束，不再收提交。"
	}
	return pc
}

func (s *Server) problemPage(w http.ResponseWriter, r *http.Request, a actor, p store.Problem, contest problemContest) {
	cases, err := s.db.ListTestcases(r.Context(), p.ID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	total, err := s.db.ProblemScoreTotal(r.Context(), p.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	counts, err := s.db.ProblemCounts(r.Context(), []int64{p.ID})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	// Sample bodies are read from disk; judged cases are named but never shown.
	var samples []samplePair
	judged := 0
	for _, tc := range cases {
		if tc.IsSample {
			in, errIn := s.cases.Read(tc.InputRel)
			out, errOut := s.cases.Read(tc.OutputRel)
			pair := samplePair{Name: tc.Name, In: "（用例文件缺失）"}
			if errIn == nil && errOut == nil {
				pair.In = string(in)
				pair.Out = string(out)
			}
			samples = append(samples, pair)
			continue
		}
		judged++
	}

	// The recent list is scoped below when the page is a contest page.
	filter := store.DetailFilter{ProblemID: p.ID, Limit: 10}
	if contest.ID != 0 {
		// Inside a contest the recent list is your own: other people's verdicts on a
		// problem that is still being played are the answer sheet.
		filter.UserID = a.User.ID
	}
	recent, err := s.db.ListSubmissionDetails(r.Context(), filter)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	rows := make([]subRow, 0, len(recent))
	for _, d := range recent {
		rows = append(rows, rowOf(d))
	}

	langs := p.Languages
	langText := joinOr(langs, "不限")
	// An unrestricted problem still needs concrete choices in the form, so offer
	// the languages the built-in judge can actually build.
	options := langs
	if len(options) == 0 {
		options = defaultLanguages
	}
	c := counts[p.ID]
	// 判定方式是学生打开题面第一眼就该看到的（交互题尤其），所以这一步按主键多读一张小表。
	programs, err := s.db.JudgePrograms(r.Context(), p.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, p.PID+" "+p.Title, "problem", a.viewer(), msgOf(r), problemData{
		PID: p.PID, Title: p.Title,
		Time: formatMS(p.TimeLimitMS, false), Mem: formatKB(p.MemLimitKB, false),
		LangText: langText, Langs: options, Versions: versionAxesFor(options),
		Mode:        modeBadge(programs),
		JudgedCases: judged, TotalScore: total,
		Solved: c.Solved, Submits: c.Submits,
		Hidden:     !p.Visible,
		CanEdit:    a.canEditProblem(p),
		CanCases:   a.canSeeTestcases(p),
		TopicsLink: "/problems/" + url.PathEscape(p.PID) + "/topics",
		Contest:    contest,
		// 输入框在没有资格的那一种情况下不出现，理由写在它该在的位置上。
		VerifyNotice: s.verifyNotice(r, a),
		Statement:    renderMarkdownHTML(p.Statement),
		Samples:      samples,
		Recent:       rows,
	})
}

// --- submission list ---

type submissionsData struct {
	Rows          []subRow
	Shown, Limit  int
	Next          int64
	UserFilter    string
	ProblemFilter string
}

func (s *Server) submissionsPage(w http.ResponseWriter, r *http.Request, me *viewer, items []store.SubmissionDetail, limit int, userFilter, problemFilter string) {
	rows := make([]subRow, 0, len(items))
	for _, d := range items {
		rows = append(rows, rowOf(d))
	}
	var next int64
	if len(items) == limit && len(items) > 0 {
		next = items[len(items)-1].ID
	}
	s.page(w, r, http.StatusOK, "提交记录", "submissions", me, msgOf(r), submissionsData{
		Rows: rows, Shown: len(rows), Limit: limit, Next: next,
		UserFilter: userFilter, ProblemFilter: problemFilter,
	})
}

// --- submission detail ---

type runRow struct {
	Ordinal  int
	CaseName string
	Verdict  string
	Time     string
	Mem      string
	IsSample bool
	// Points is what this row earned, and Unpaid marks the case that passed while
	// its bundle did not: those two need not be the same story.
	Points string
	Unpaid bool
}

type submissionData struct {
	ID     int64
	Handle string
	// Name is who the page credits with this run, which for a cancelled account is
	// not its handle: that login name may already belong to someone else.
	Name      string
	NameColor string
	PID       string
	Title     string
	Language  string
	// Version 是这次判完能引用的那句实话："C++17 · O2 开"。老数据也有，因为它的默认
	// 口径正是把版本拆成选项之前硬编码的那一条命令行。
	Version   string
	Class     string
	Outcome   string
	Headline  string
	Time      string
	Mem       string
	Score     string
	Failed    string
	JudgeName string
	Created   string
	Judged    string
	// Contest is the cid when the submission was made inside a contest; ContestGone
	// covers the case where the contest has since been deleted, so the page can say so
	// instead of linking at nothing.
	Contest     string
	ContestLink string
	ContestGone bool
	Runs        []runRow
	Detail      string
	Source      string
	View        detailView
	Partial     bool
	Waiting     bool
}

func (s *Server) submissionPage(w http.ResponseWriter, r *http.Request, a actor, d store.SubmissionDetail, view detailView, source, detail string, runs []store.Run) {
	class, outcome := outcomeOf(d.Status, d.Verdict)
	score := "-"
	if d.TotalScore > 0 {
		score = fmt.Sprintf("%d/%d", d.Score, d.TotalScore)
	}
	headline := d.Verdict
	if headline == "" {
		headline = outcome
	}
	if d.Verdict != "" {
		headline = fmt.Sprintf("%s · 得分 %s · 用时 %s · 内存 %s", verdictSentence(d.Verdict), score, formatMS(d.TimeMS, d.TimeMin), formatKB(d.MemKB, d.MemMin))
	}

	rows := make([]runRow, 0, len(runs))
	for _, run := range runs {
		unpaid := !run.IsSample && run.Verdict == "AC" && run.Score == 0
		points := "—"
		if !run.IsSample {
			points = strconv.Itoa(run.Score)
		}
		time, mem := formatMS(run.TimeMS, run.TimeMin), formatKB(run.MemKB, run.MemMin)
		if !view.Perf {
			time, mem = "", ""
		}
		rows = append(rows, runRow{
			Ordinal: run.Ordinal, CaseName: run.CaseName, Verdict: run.Verdict,
			Time: time, Mem: mem,
			IsSample: run.IsSample, Points: points, Unpaid: unpaid,
		})
	}
	judgeName := "-"
	if d.JudgeID != 0 {
		judgeName = strconv.FormatInt(d.JudgeID, 10)
	}
	contested := d.ContestID != 0
	var contestLink string
	if d.ContestCID != "" {
		contestLink = "/contests/" + url.PathEscape(d.ContestCID) + "/standings"
	}

	s.page(w, r, http.StatusOK, fmt.Sprintf("提交 #%d", d.ID), "submission", a.viewer(), "", submissionData{
		ID: d.ID, Handle: d.Handle, Name: personName(d.Name, d.Handle), NameColor: d.Color,
		PID: d.PID, Title: d.Title,
		Language: d.Language, Version: toolchainCell(d.Language, d.Toolchain, d.Options),
		Class: class, Outcome: outcome, Headline: headline,
		Time: formatMS(d.TimeMS, d.TimeMin), Mem: formatKB(d.MemKB, d.MemMin), Score: score,
		Failed: orDash(d.FailedCase), JudgeName: judgeName,
		Created: agoMS(d.CreatedAt), Judged: agoMS(d.JudgedAt),
		Contest:     d.ContestCID,
		ContestLink: contestLink,
		ContestGone: contested && d.ContestCID == "",
		Runs:        rows, Detail: detail, Source: source,
		View:    view,
		Partial: !(view.Runs && view.Perf && view.Diff && view.Judge && view.Source),
		Waiting: judge.Status(d.Status).InFlight(),
	})
}

func verdictSentence(v string) string {
	switch judge.Verdict(v) {
	case judge.AC:
		return "通过"
	case judge.WA:
		return "解答错误"
	case judge.TLE:
		return "用时超限"
	case judge.MLE:
		return "内存超限"
	case judge.RE:
		return "运行时错误"
	case judge.CE:
		return "编译错误"
	case judge.IE:
		return "判机故障"
	}
	return v
}

// --- leaderboard ---

type rankRowView struct {
	Rank      int
	Handle    string
	Name      string
	NameColor string
	Solved    int
	Submits   int
	// Points 是后台算好的活跃度：排行榜按做题数排，但那一列已经不再唯一说明"谁在这站
	// 上花的时间多"，所以两个数都给，各自说各自的事。
	Points int
	LastAC string
}

func (s *Server) handleRank(w http.ResponseWriter, r *http.Request) {
	rank, err := s.db.Leaderboard(r.Context(), 100)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	rows := make([]rankRowView, 0, len(rank))
	for i, row := range rank {
		last := "-"
		if row.LastAC != 0 {
			last = agoMS(row.LastAC)
		}
		rows = append(rows, rankRowView{Rank: i + 1, Handle: row.Handle, Name: personName(row.Name, row.Handle),
			NameColor: row.Color, Solved: row.Solved, Submits: row.Submits, Points: row.Points, LastAC: last})
	}
	s.page(w, r, http.StatusOK, "排行榜", "rank", s.viewerOf(r), "", map[string]any{"Rows": rows})
}

// --- user profile ---

type tallyView struct {
	Name, Class string
	Count       int
}
type solvedView struct{ PID, Title, Time, When string }

type userData struct {
	Handle      string
	DisplayName string
	Bio         template.HTML
	HasBio      bool
	Admin       bool
	// Owner is the super admin: the account that holds every capability and that
	// no other admin can change.
	Owner bool
	// Cancelled marks a wiped identity whose records outlive it. The profile keeps
	// the history and drops the social controls, because there is nobody behind them.
	Cancelled     bool
	Joined        string
	Solved        int
	TotalProblems int
	Submits       int
	// Points 与 Dims 是后台算好的活跃度与它的四维明细：个人页把"分是怎么来的"摊开，
	// 一个人不必猜自己为什么是绿的、别人为什么是红的。
	Points int
	Dims   []store.Dim
	// NameColor 是这个名字该渲染成哪一档。
	NameColor  string
	Tally      []tallyView
	SolvedList []solvedView
	Recent     []subRow
	Posts      []postRow
	IsSelf     bool
	// CheckInStreak and CheckInTotal are the daily punch-in record.
	CheckInStreak int
	CheckInTotal  int
	// Follow counts plus the viewer's own relation to this profile, which is what
	// decides between the 关注 and 取消关注 buttons.
	Following int
	Followers int
	IFollow   bool
	Mutual    bool
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	handle := r.PathValue("handle")
	u, err := s.db.GetUserByHandle(ctx, handle)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	solved, err := s.db.SolvedProblems(ctx, u.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	tally, err := s.db.VerdictTally(ctx, u.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	recent, err := s.db.ListSubmissionDetails(ctx, store.DetailFilter{UserID: u.ID, Limit: 20})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	submits := 0
	for _, n := range tally {
		submits += n
	}

	a, _ := s.authenticate(r)
	me := a.viewer()
	totalProblems, err := s.db.CountProblems(ctx, !a.can(permProblemEditAny))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	// Fixed order, so the two most common verdicts anchor the header.
	var tallyRows []tallyView
	seen := map[string]bool{}
	for _, name := range []string{"AC", "WA", "TLE", "MLE", "RE", "CE", "IE",
		"queued", "compiling", "running", "abandoned"} {
		if n := tally[name]; n > 0 {
			tallyRows = append(tallyRows, tallyView{Name: name, Class: name, Count: n})
			seen[name] = true
		}
	}
	for name, n := range tally {
		if !seen[name] && n > 0 {
			tallyRows = append(tallyRows, tallyView{Name: name, Class: name, Count: n})
		}
	}

	solvedList := make([]solvedView, 0, len(solved))
	for _, sp := range solved {
		solvedList = append(solvedList, solvedView{
			PID: sp.PID, Title: sp.Title, Time: formatMS(sp.BestTime, false), When: agoMS(sp.SolvedAt),
		})
	}
	rows := make([]subRow, 0, len(recent))
	for _, d := range recent {
		rows = append(rows, rowOf(d))
	}

	// Own profile shows hidden entries too, so the author can see what was taken
	// down; moderators see them everywhere.
	canSeeHidden := a.User.ID == u.ID || a.can(permFeedModerate)
	myPosts, err := s.db.ListPosts(ctx, 20, 0, u.ID, canSeeHidden)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	checkIn, err := s.db.CheckInState(ctx, u.ID, time.Now())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	following, followers, err := s.db.FollowCounts(ctx, u.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	iFollow, mutual := false, false
	if a.User.ID != 0 && a.User.ID != u.ID {
		if iFollow, err = s.db.IsFollowing(ctx, a.User.ID, u.ID); err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		// Mutual only matters when we already follow them, so the second pair of
		// lookups is skipped for the common case.
		if iFollow {
			if mutual, err = s.db.AreMutual(ctx, a.User.ID, u.ID); err != nil {
				s.writeStoreError(w, r, err)
				return
			}
		}
	}

	owner, err := s.db.IsOwner(ctx, u.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	s.page(w, r, http.StatusOK, u.Handle, "user", me, msgOf(r), userData{
		Handle: u.Handle, DisplayName: u.DisplayName,
		Bio: renderMarkdownHTML(u.Bio), HasBio: strings.TrimSpace(u.Bio) != "",
		Admin: u.Role == "admin", Owner: owner, Joined: agoMS(u.CreatedAt),
		// Cancelled is the tombstone marker: the profile still shows what this
		// user_id produced, but nobody can follow, message or log in as it.
		Cancelled: u.IsCancelled(),
		Solved:    len(solved), TotalProblems: totalProblems, Submits: submits,
		Points: u.Points, Dims: u.Dims(), NameColor: u.ColorClass(),
		Tally: tallyRows, SolvedList: solvedList, Recent: rows,
		Posts:     postRows(myPosts, a),
		IsSelf:    a.User.ID == u.ID,
		Following: following, Followers: followers,
		IFollow: iFollow, Mutual: mutual,
		CheckInStreak: checkIn.Streak, CheckInTotal: checkIn.Total,
	})
}

// createSubmission is the single validation path for every way a person can hand in
// code, so the checks cannot drift depending on where the form came from. contestID
// is the optional 归属 the contest pages attach; naming it in a form is a claim, not a
// fact, so the window, the roster and the mount table are all re-read here.
//
// It also answers where the browser should be sent back with the reason: a message
// written onto a page the reader cannot open is a message nobody reads.
func (s *Server) createSubmission(r *http.Request, a actor, problemKey, language, source string, contestID int64) (store.Submission, string, string, error) {
	key := strings.TrimSpace(problemKey)
	lang := strings.ToLower(strings.TrimSpace(language))
	if key == "" || lang == "" {
		return store.Submission{}, "题目与语言都要填", rejectPath(key, 0), errors.New("missing fields")
	}
	if strings.ContainsRune(source, 0) {
		return store.Submission{}, "源代码不能包含空字节", rejectPath(key, 0), errors.New("nul in source")
	}
	if len(strings.TrimSpace(source)) == 0 {
		return store.Submission{}, "源代码是空的", rejectPath(key, 0), errors.New("empty source")
	}

	p, err := s.db.FindProblem(r.Context(), key)
	if err != nil {
		return store.Submission{}, "找不到这道题", rejectPath(key, 0), err
	}
	inside, err := s.contestForSubmission(r, a, p, contestID)
	if err != nil {
		msg := err.Error()
		if inside.CID != "" {
			msg = "不能按「" + inside.CID + "」提交：" + msg
		}
		// The contest page is the one place that can explain a rejected membership, so
		// send the reader there instead of at a problem they may not open. Only a
		// contest that really exists can be that landing page.
		return store.Submission{}, msg, rejectPath(key, inside.ContestID), err
	}
	problemPath := "/problems/" + url.PathEscape(p.PID)
	if inside.ContestID != 0 {
		// Keep the contest context the reader was working in, so the reason lands
		// where they can act on it.
		problemPath += "?contest=" + strconv.FormatInt(inside.ContestID, 10)
	}
	// 邮箱没验证就不收代码（M25f）。这一条在这里，而不是在各个表单里，是因为所有交代码
	// 的路——题面页、比赛页、赛内页——都汇到这一个函数，写在页面上就能被 curl 绕过去。
	// 读的部分一点没动：题目、动态、帖子、比赛页面全都照看，被拦的只有这次写。
	if why := s.verifyNotice(r, a); why != "" {
		return store.Submission{}, why, problemPath, errors.New("email not verified")
	}
	if !p.Visible && !a.canEditProblem(p) && !a.can(permProblemPublish) && !inside.Member {
		return store.Submission{}, "找不到这道题", rejectPath(key, inside.ContestID), store.ErrNotFound
	}
	if !languageAllowed(p.Languages, lang) {
		return store.Submission{}, fmt.Sprintf("这道题只收 %s，不收 %s", strings.Join(p.Languages, "/"), lang),
			problemPath, errors.New("language")
	}
	// Python 的内存下限是解释器自己要的那一份：实测 python3.14 空跑一次就要 ~9MB 常驻，
	// 加上 import 一点标准库就更多。题面内存比这还小的时候，每一条 Python 提交都会
	// MLE——那不是判定，那是解释器在替学生报错。所以这里当面拒绝并说清原因，而不是
	// 让提交进去撞一条莫名其妙的内存超限。
	if lang == judge.PythonLanguage && p.MemLimitKB < pythonMinMemKB {
		return store.Submission{}, fmt.Sprintf(
				"这道题的内存上限是 %s，Python 解释器自己就要占约 10MB，交上来只会因为内存超限被拒。"+
					"要么把题面内存调到 %dMB 以上，要么这道题别收 Python",
				formatKB(p.MemLimitKB, false), pythonMinMemKB>>10),
			problemPath, errors.New("python memory")
	}
	// 版本选择在写库前就按判机的候选表规范化：这一串以后会被拼进命令行，含糊的、越界的
	// 都不该进去。被拒时说明能选哪些，而不是悄悄换成另一个版本编——那等于用一条谁都没
	// 选过的命令行判了这份代码。
	tool, _ := versionTool(lang)
	options, err := submitVersion(tool, r.Form)
	if err != nil {
		return store.Submission{}, "版本选择：" + err.Error(), problemPath, err
	}
	total, err := s.db.ProblemScoreTotal(r.Context(), p.ID)
	if err != nil {
		return store.Submission{}, "数据库错误", problemPath, err
	}
	if total == 0 {
		return store.Submission{}, "这道题还没有判题用例", problemPath, errors.New("no cases")
	}

	sub, err := s.db.CreateSubmission(r.Context(), &store.Submission{
		UserID: a.User.ID, ProblemID: p.ID, ContestID: inside.ContestID,
		Language: lang, Options: options, Source: source, TotalScore: total,
	})
	if err != nil {
		return store.Submission{}, "数据库错误", problemPath, err
	}
	s.log.Info("submitted", "sid", sub.ID, "user", a.User.Handle, "pid", p.PID, "lang", lang,
		"options", options, "contest", sub.ContestID)
	return sub, "", "", nil
}

// rejectPath is where a rejected submission lands with its reason. Inside a contest it
// is the contest page, which is the one place that can explain a join problem and is
// always readable; otherwise it stays the problem the form named. The numeric contest id
// works in that URL because a contest is found by id as well as by label.
func rejectPath(problemKey string, contestID int64) string {
	if contestID > 0 {
		return "/contests/" + strconv.FormatInt(contestID, 10)
	}
	return "/problems/" + url.PathEscape(strings.TrimSpace(problemKey))
}

// contestClaim is what the submission gate hands back: the contest to attribute to,
// and whether the submitter is inside it as a member.
type contestClaim struct {
	ContestID int64
	CID       string
	Member    bool
}

// contestForSubmission re-checks a contest the form claimed. Nothing is trusted from
// the field: the window, the roster and the mount table are all read again.
func (s *Server) contestForSubmission(r *http.Request, a actor, p store.Problem, contestID int64) (contestClaim, error) {
	if contestID == 0 {
		return contestClaim{}, nil
	}
	c, err := s.db.GetContestByID(r.Context(), contestID)
	if err != nil {
		return contestClaim{}, errors.New("没有这场比赛")
	}
	claim := contestClaim{ContestID: c.ID, CID: c.CID}
	mounted, err := s.db.ContestProblemMounted(r.Context(), c.ID, p.ID)
	if err != nil {
		return contestClaim{}, err
	}
	if !mounted {
		return claim, errors.New("这道题不在这场比赛里")
	}
	if a.User.ID == 0 {
		return claim, errors.New("提交前先登录并加入这场比赛")
	}
	// 站级开关关掉比赛时赛内提交一并停：只挡加入与建赛的话，名单里的人还能继续交。
	if !s.site(r.Context()).AllowContests {
		return claim, errors.New("本站已关闭比赛，不再收提交。")
	}
	// 被禁止参加比赛的人连赛内提交也不能做：只挡住"加入"的话，已经在名单里的人
	// 就还能继续交。这条放在管理员分支之前——被禁止的人不该靠自己是主办方绕过去。
	if msg, banned := a.bannedFrom(permContestJoin, "参加比赛"); banned {
		return claim, errors.New(msg)
	}
	// A manager may test their own contest outside the window; that is how a host
	// checks the data before the door opens. Everyone else is bound by it.
	manage := a.canManageContest(c)
	if !manage {
		if _, ok := s.db.ContestMemberOf(r.Context(), c.ID, a.User.ID); !ok {
			return claim, errors.New("你还不是这场比赛的成员")
		}
		switch c.Phase(time.Now().UnixMilli()) {
		case store.PhaseUpcoming:
			return claim, errors.New("这场比赛还没开始")
		case store.PhaseOver:
			return claim, errors.New("这场比赛已经结束")
		}
	}
	claim.Member = true
	return claim, nil
}

// handleSubmitForm is the browser side of submitting code: the same validation, then a
// redirect to the result page instead of a body.
func (s *Server) handleSubmitForm(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		back := "/problems/" + url.PathEscape(r.PathValue("key"))
		http.Redirect(w, r, "/login?next="+url.QueryEscape(back)+"&msg="+url.QueryEscape("提交前请先登录"), http.StatusSeeOther)
		return
	}
	if r.ContentLength > s.cfg.MaxSource {
		s.errorPage(w, r, http.StatusRequestEntityTooLarge, "代码太长")
		return
	}
	// 提交速率限制：判机是共享资源，队列深度就是所有人的等待时间。按人限而不是按 IP——
	// 一个机房 NAT 出一个 IP 是常态，按 IP 限会在开学第一节课把整班人挡在门外。
	if s.cfg.SubmitPerMinute > 0 {
		key := "submit:user:" + strconv.FormatInt(a.User.ID, 10)
		if allowed, err := s.db.RateAllowed(r.Context(), key, rateMinute, s.cfg.SubmitPerMinute); err != nil {
			s.writeStoreError(w, r, err)
			return
		} else if !allowed {
			s.errorPage(w, r, http.StatusTooManyRequests, fmt.Sprintf(
				"提交太快了：一人每分钟最多 %d 次，等一分钟再试。"+
					"判机是共享的，队列越长所有人的结果都越晚出来。", s.cfg.SubmitPerMinute))
			return
		}
		if _, err := s.db.RateHit(r.Context(), key, rateMinute); err != nil {
			s.writeStoreError(w, r, err)
			return
		}
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "表单解析失败: "+err.Error())
		return
	}
	// The contest travels in a hidden field, so it is a claim to verify, not a fact.
	contestID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("contest")), 10, 64)
	sub, msg, back, err := s.createSubmission(r, a, r.PathValue("key"), r.FormValue("language"), r.FormValue("source"), contestID)
	if err != nil {
		if msg == "" {
			msg = "提交失败"
		}
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		http.Redirect(w, r, back+sep+"msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/submissions/"+strconv.FormatInt(sub.ID, 10), http.StatusSeeOther)
}
