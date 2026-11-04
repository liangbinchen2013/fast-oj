package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/judge"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

func languageAllowed(list []string, lang string) bool {
	if len(list) == 0 {
		return true
	}
	for _, l := range list {
		if strings.EqualFold(l, lang) {
			return true
		}
	}
	return false
}

// detailView is how much of the judging a particular reader gets to see on the
// submission page. The problem's own 详情级别 sets the ceiling for everyone who is
// neither the author nor the operator, so a contest can hide the failing case
// without hiding the score.
type detailView struct {
	Source bool // the submitted code itself
	Runs   bool // the per-case table
	Perf   bool // per-case time and memory
	Diff   bool // what the judge compared, and which case failed
	Judge  bool // which box judged it
}

var seeEverything = detailView{Source: true, Runs: true, Perf: true, Diff: true, Judge: true}

// canReadSubmission 决定一条提交现在能不能给这个读者看。它和 store 里的公开列表过滤
// 保持同一条规则：隐藏题只给作者/题目主人/提交者自己；进行中的 OI 与封榜比赛里的别人
// 结果一律不可见，除非是主办方或有全见权限。
func canReadSubmission(a actor, d store.SubmissionDetail) bool {
	if a.can(permSubmissionViewAny) || a.can(permProblemEditAny) {
		return true
	}
	if d.ProblemOwner != 0 && a.User.ID == d.ProblemOwner {
		return true
	}
	if a.User.ID != 0 && a.User.ID == d.UserID {
		return true
	}
	if !d.ProblemVisible {
		return false
	}
	if d.ContestID == 0 || d.ContestEndAt <= time.Now().UnixMilli() {
		return true
	}
	if d.ContestOwnerID != 0 && a.User.ID == d.ContestOwnerID {
		return true
	}
	return d.ContestStanding && d.ContestMode != store.ContestOI
}

func submissionView(a actor, d store.SubmissionDetail) detailView {
	own := a.User.ID != 0 && a.User.ID == d.UserID
	if a.can(permSubmissionViewAny) || a.can(permProblemEditAny) ||
		(d.ProblemOwner != 0 && a.User.ID == d.ProblemOwner) {
		// Whoever owns the problem, or holds the audit right, needs the whole
		// picture to act on it.
		return seeEverything
	}
	// Somebody else's submission stays a summary: the per-case detail is the
	// author's and the operator's business, never the public's.
	if !own {
		return detailView{}
	}
	// Your own code and score are yours whatever the level says; what the level
	// grades is what the judging revealed about it.
	view := gradeDetail(d.DetailLevel)
	view.Source = true
	return view
}

func gradeDetail(level string) detailView {
	view := detailView{}
	switch level {
	case store.DetailCase:
		view.Runs = true
	case store.DetailDiff, store.DetailFull:
		view.Runs, view.Perf, view.Diff = true, true, true
		if level == store.DetailFull {
			view.Judge = true
		}
	case store.DetailNone:
	default:
		// An unknown value falls back to the middle of the ladder rather than to
		// showing everything.
		view.Runs, view.Perf, view.Diff = true, true, true
	}
	return view
}

func (s *Server) handleSubmitGet(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.respondError(w, r, http.StatusBadRequest, "submission id must be a positive number")
		return
	}
	d, err := s.db.SubmissionDetail(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !canReadSubmission(a, d) {
		// 不存在、隐藏题的别人提交、以及进行中 OI 的别人成绩，对外是同一个答案：
		// 别用存在性给攻击者当探针。
		s.respondError(w, r, http.StatusNotFound, "没有这条提交")
		return
	}

	view := submissionView(a, d)
	var runs []store.Run
	var source, detail string
	if view.Runs || view.Source {
		// Source and judge detail stay out of the joined list view; they are
		// loaded only for the one row someone is allowed to read.
		source, detail, err = s.db.SubmissionPayload(r.Context(), d.ID)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		if !view.Source {
			source = ""
		}
		if !view.Diff {
			detail = ""
		}
		d.Source, d.Detail = source, detail
	}
	if view.Runs {
		runs, err = s.db.ListRuns(r.Context(), d.ID)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
	}
	s.submissionPage(w, r, a, d, view, source, detail, runs)
}

func ago(ms int64) string {
	if ms == 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(ms))
	if d < 0 {
		return "0s"
	}
	return d.Truncate(time.Second).String()
}

// pageLimit mirrors the store-side clamp so the next_after footer stays exact.
func pageLimit(raw string) int {
	n, _ := strconv.Atoi(raw)
	switch {
	case n <= 0:
		return 50
	case n > 500:
		return 500
	}
	return n
}

func (s *Server) handleSubmitList(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	q := r.URL.Query()
	var f store.DetailFilter
	f.Limit = pageLimit(q.Get("limit"))
	f.AfterID, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	f.PublicView = true
	f.ViewerID = a.User.ID
	f.SeeAll = a.can(permSubmissionViewAny) || a.can(permProblemEditAny)

	switch who := q.Get("user"); who {
	case "":
	case "me":
		if a.User.ID == 0 {
			s.writeAuthError(w, r, errNoCredentials)
			return
		}
		f.UserID = a.User.ID
	default:
		u, err := s.db.GetUserByHandle(r.Context(), who)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		f.UserID = u.ID
	}
	if key := q.Get("problem"); key != "" {
		p, err := s.db.FindProblem(r.Context(), key)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		f.ProblemID = p.ID
	}
	// 状态筛选是给管理员找人的：停在队列里的、判机超时的，混在一堆已完成的记录里翻不出来。
	status := q.Get("status")
	if status != "" && !knownStatus(status) {
		s.respondError(w, r, http.StatusBadRequest, "status must be one of queued, compiling, running, done, abandoned")
		return
	}
	f.Status = status

	items, err := s.db.ListSubmissionDetails(r.Context(), f)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.submissionsPage(w, r, a.viewer(), items, f.Limit, q.Get("user"), q.Get("problem"), status, a.can(permRejudge))
}

// knownStatus 认 schema 的 CHECK 允许的那五个值。填错了要给一句话，而不是让人对着一张
// 空表猜自己筛错了什么——这条筛选项就是拿来找"卡住的提交"的，参数静默失效最误事。
func knownStatus(raw string) bool {
	switch judge.Status(raw) {
	case judge.Queued, judge.Compiling, judge.Running, judge.Done, judge.Abandoned:
		return true
	}
	return false
}

// handleRejudge 把一条提交退回队列重判。
//
// 判题口径是会变的（这一条功能的起因就是"数组开太大本来该判 MLE 而不是 RE"），而库里的
// 判定是**当时那一次运行**的结果，不会因为判机升级就自己改判。重判就是管理员把那条旧判定
// 纠正回来的唯一动作，所以它改的是"结果是什么"，单独一把 submission.rejudge 钥匙，不跟着
// submission.view.any 走。
//
// 已经在飞的那一次盖不掉新结果：Rejudge 把 judge_id 清成 NULL，旧判机后来回报时先在归属
// 检查上吃 403，再到状态迁移上被拒（TestRejudgedSubmissionIgnoresTheOldJudge）。
// 退回的这条按 id 排在队列最前——管理员按下按钮是想要一个答案，不是想要一次排队。
func (s *Server) handleRejudge(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permRejudge)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.respondError(w, r, http.StatusBadRequest, "submission id must be a positive number")
		return
	}
	// 先把这一行读出来：不存在要如实 404，而且审计里要留下"改掉的是哪一个状态、哪一个判定"，
	// 事后才答得出这条成绩被重判过几次、每次改前是什么。
	d, err := s.db.SubmissionDetail(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	back := "/submissions/" + strconv.FormatInt(id, 10)
	requeued, err := s.db.Rejudge(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !requeued {
		// 排队中的那条没有上一次可清：再说一遍"已退回队列"就是谎，所以如实讲它本来就在等。
		s.redirectMsg(w, r, back, "这条已经在队列里等判了，不需要重判。")
		return
	}
	s.log.Info("submission requeued", "id", id, "was", d.Status+"/"+d.Verdict, "by", a.User.Handle)
	s.audit(r, a, "submission.rejudge", "submission:"+strconv.FormatInt(id, 10),
		d.PID+" "+d.Status+"/"+orDash(d.Verdict)+" → 退回队列")
	s.redirectMsg(w, r, back, "已退回队列，判机重跑之后这一页会跟着更新。")
}
