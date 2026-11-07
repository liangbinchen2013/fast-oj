package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/liangbinchen2013/fast-oj/internal/judge"
	"github.com/liangbinchen2013/fast-oj/internal/protocol"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

const maxJudgeRuns = 4096

func validJudgeName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// registerJudge folds the heartbeat into every claim, so a box that only knows
// how to poll still shows up in the box list.
func (s *Server) registerJudge(r *http.Request, g protocol.Greeting) (store.Judge, error) {
	g.Name = strings.TrimSpace(g.Name)
	if !validJudgeName(g.Name) {
		return store.Judge{}, fmt.Errorf("judge name must be 1..64 of [A-Za-z0-9_.-]")
	}
	for _, l := range g.Languages {
		if !validLanguageTag(l) {
			return store.Judge{}, fmt.Errorf("bad language tag %q", l)
		}
	}
	for _, c := range g.Capabilities {
		if !protocol.KnownCapability(c) {
			return store.Judge{}, fmt.Errorf("bad capability tag %q", c)
		}
	}
	return s.db.UpsertJudge(r.Context(), g.Name, g.Languages, g.Capabilities, g.Note, g.Jailed)
}

func (s *Server) handleJudgeClaim(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireJudge(r); err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	var g protocol.Greeting
	if !s.decodeBody(w, r, &g) {
		return
	}
	j, err := s.registerJudge(r, g)
	if err != nil {
		s.respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	task, err := s.db.ClaimNext(r.Context(), j.ID, store.Caps{
		Options:    j.Has(protocol.CapOptions),
		Checker:    j.Has(protocol.CapChecker),
		Interactor: j.Has(protocol.CapInteractor),
	})
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"queue": "empty", "judge": j.Name})
		return
	}
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	cases, err := s.db.ListTestcases(r.Context(), task.ProblemID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	programs, err := s.db.JudgePrograms(r.Context(), task.ProblemID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	work := protocol.Task{
		SubmissionID: task.SubmissionID, PID: task.PID, Language: task.Language,
		Options: task.Options,
		Source:  task.Source, TimeLimitMS: task.TimeLimitMS, MemLimitKB: task.MemLimitKB,
	}
	// 判题器与交互器跟着工单走：判机必须用**自己那套工具链**现编它们，别处编好的二进制
	// 带着另一套 libc，判出来的东西和判这台机器上学生程序的不是同一件事。
	if p, ok := programs[store.ProgramChecker]; ok {
		work.Checker = &protocol.AuxProgram{Language: p.Language, Source: p.Source}
	}
	if p, ok := programs[store.ProgramInteractor]; ok {
		work.Interactor = &protocol.AuxProgram{Language: p.Language, Source: p.Source}
	}
	for _, tc := range cases {
		work.Cases = append(work.Cases, protocol.Case{
			Name: tc.Name, Ordinal: tc.Ordinal, IsSample: tc.IsSample, Score: tc.Score,
		})
	}

	s.log.Info("claimed", "sid", task.SubmissionID, "judge", j.Name, "pid", task.PID)
	// 领单就是"这台机器拿走了谁的源码和哪些隐藏数据"：令牌泄露后要回答的第一问就是它。
	// 判题器/交互器的源码也在这条工单里一起出去，所以把它写进 detail。
	taken := fmt.Sprintf("%s 第 %d 条（%s%s）", task.PID, task.SubmissionID, task.Language,
		programsCarried(work))
	s.auditJudge(j.Name, "judge.claim", strconv.Itoa(int(task.SubmissionID)), taken)
	writeJSON(w, http.StatusOK, work)
}

// programsCarried 说清这份工单除了源码还带走了什么题目侧的数据。
func programsCarried(t protocol.Task) string {
	extra := ""
	if t.Checker != nil {
		extra += "，含判题器"
	}
	if t.Interactor != nil {
		extra += "，含交互器"
	}
	if n := len(t.Cases); n > 0 {
		extra += fmt.Sprintf("，用例 %d 条", n)
	}
	return extra
}

// handleJudgeCaseFile streams case data to a judge box. Unlike the public case
// route this also serves judged (non-sample) cases.
//
// 地址是「某条工单的第 N 条用例」，不是「库里 id 为 X 的用例」：判机令牌一旦被人拿到
// （它泄在 ps 里过），按 id 取就意味着他能顺着 1、2、3……把**全部题目的隐藏输入与答案**
// 抄走，而答案泄露是没法回收的。按工单取之后他能读到的只有他被派到的那几份文件。
func (s *Server) handleJudgeCaseFile(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireJudge(r); err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	sid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || sid <= 0 {
		s.respondError(w, r, http.StatusBadRequest, "submission id must be a positive number")
		return
	}
	ordinal, err := strconv.Atoi(r.PathValue("ordinal"))
	if err != nil || ordinal <= 0 {
		s.respondError(w, r, http.StatusBadRequest, "case ordinal must be a positive number")
		return
	}
	which := r.PathValue("which")
	switch which {
	case "in", "out":
	default:
		s.respondError(w, r, http.StatusBadRequest, "which must be in or out")
		return
	}
	sub, err := s.db.GetSubmission(r.Context(), sid)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("judge"))
	if name == "" {
		s.respondError(w, r, http.StatusBadRequest, "judge box name is required (?judge=)")
		return
	}
	if _, ok := s.resolveJudgeOwner(w, r, name, sub); !ok {
		return
	}
	cases, err := s.db.ListTestcases(r.Context(), sub.ProblemID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	var tc store.Testcase
	found := false
	for _, c := range cases {
		if c.Ordinal == ordinal {
			tc, found = c, true
			break
		}
	}
	if !found {
		s.respondError(w, r, http.StatusNotFound, "this submission has no such case")
		return
	}
	rel := tc.OutputRel
	if which == "in" {
		rel = tc.InputRel
	}
	body, err := s.cases.Read(rel)
	if err != nil {
		s.respondError(w, r, http.StatusNotFound, "case file missing")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// resolveJudgeOwner looks up the box that must own the row before it may report.
func (s *Server) resolveJudgeOwner(w http.ResponseWriter, r *http.Request, name string, sub store.Submission) (store.Judge, bool) {
	j, err := s.db.GetJudge(r.Context(), strings.TrimSpace(name))
	if err != nil {
		s.respondError(w, r, http.StatusForbidden, "unknown judge box, claim the submission first")
		return store.Judge{}, false
	}
	if sub.JudgeID != j.ID {
		s.respondError(w, r, http.StatusForbidden,
			"judge "+j.Name+" does not own submission "+strconv.FormatInt(sub.ID, 10))
		return store.Judge{}, false
	}
	return j, true
}

func (s *Server) handleJudgeStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireJudge(r); err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	var req protocol.Status
	if !s.decodeBody(w, r, &req) {
		return
	}
	id, err := submissionID(r)
	if err != nil {
		s.respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := s.db.GetSubmission(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	j, ok := s.resolveJudgeOwner(w, r, req.Name, sub)
	if !ok {
		return
	}

	to := judge.Status(req.State)
	if to != judge.Compiling && to != judge.Running {
		s.respondError(w, r, http.StatusBadRequest, "state must be compiling or running")
		return
	}
	if !judge.Transition(judge.Status(sub.Status), to) {
		s.respondError(w, r, http.StatusConflict,
			fmt.Sprintf("cannot move submission %d from %s to %s", id, sub.Status, to))
		return
	}
	var progress *store.Run
	if req.Run != nil {
		v := judge.VerdictName(req.Run.Verdict)
		// VerdictName folds the unknown into IE, so compare against the raw string
		// to tell "the judge sent IE" apart from "the judge sent garbage".
		if string(v) != req.Run.Verdict || req.Run.Ordinal <= 0 {
			s.respondError(w, r, http.StatusBadRequest, "progress row needs a known verdict and a positive ordinal")
			return
		}
		progress = &store.Run{
			Ordinal: req.Run.Ordinal, CaseName: clip(req.Run.CaseName, 64), Verdict: string(v),
			TimeMS: req.Run.TimeMS, MemKB: req.Run.MemKB, IsSample: req.Run.IsSample,
			TimeMin: req.Run.TimeMin, MemMin: req.Run.MemMin,
		}
	}
	ok, err = s.db.ReportProgress(r.Context(), id, j.ID, sub.Status, string(to), req.Detail, progress)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !ok {
		s.respondError(w, r, http.StatusConflict, "submission state changed under the judge")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sid": id, "state": string(to)})
}

func (s *Server) handleJudgeResult(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireJudge(r); err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	var req protocol.Result
	// A report carries a row per case, so it gets a larger budget than a normal
	// JSON body.
	if !s.decodeBodyLimit(w, r, &req, s.cfg.MaxUpload) {
		return
	}
	if len(req.Runs) > maxJudgeRuns {
		s.respondError(w, r, http.StatusBadRequest, "too many case rows")
		return
	}
	id, err := submissionID(r)
	if err != nil {
		s.respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := s.db.GetSubmission(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	j, ok := s.resolveJudgeOwner(w, r, req.Name, sub)
	if !ok {
		return
	}
	if !judge.Transition(judge.Status(sub.Status), judge.Done) {
		s.respondError(w, r, http.StatusConflict,
			fmt.Sprintf("submission %d is already %s", id, sub.Status))
		return
	}

	cases, err := s.db.ListTestcases(r.Context(), sub.ProblemID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	verdict, runs, score, total, failed := tallyResult(judge.VerdictName(req.Verdict), req.Runs, cases)
	// CE and IE have no failing case to name, so keep whatever the box labelled.
	if failed == "" {
		failed = req.FailedCase
	}

	if err := s.db.RecordResult(r.Context(), id, store.Result{
		Verdict: string(verdict), TimeMS: req.TimeMS, MemKB: req.MemKB,
		TimeMin: req.TimeMin, MemMin: req.MemMin, Score: score,
		TotalScore: total, FailedCase: failed, Detail: req.Detail,
		Toolchain: strings.TrimSpace(req.Toolchain),
	}, runs); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("judged", "sid", id, "judge", j.Name, "verdict", string(verdict),
		"score", fmt.Sprintf("%d/%d", score, total), "time_ms", req.TimeMS)
	// 判定是谁写的这条也要留痕：分数与判定都是这台机器报来的，服务器只按逐用例行重算。
	s.auditJudge(j.Name, "judge.result", strconv.Itoa(int(id)), fmt.Sprintf(
		"%s %d/%d（判机自报 %s，用时 %dms，内存 %dKB）", verdict, score, total,
		orDash(req.Toolchain), req.TimeMS, req.MemKB))
	writeJSON(w, http.StatusOK, map[string]any{"sid": id, "verdict": string(verdict),
		"score": score, "total_score": total, "failed_case": failed, "cases": len(runs)})
}

// tallyResult treats the judge as untrusted arithmetic: the score is recomputed
// from the per-case rows against the stored case weights, and an AC claim whose
// rows contain a failure is downgraded to that failure.
//
// Bundling is applied here rather than in the judge: a group pays only when every
// case declared in it came back AC, so a box that skips the hard case of a group
// earns nothing from the easy ones. Each row also records what it actually earned,
// which is how the page can say "这条过了，但这组没给分".
func tallyResult(claimed judge.Verdict, reqRuns []protocol.RunRow, cases []store.Testcase) (judge.Verdict, []store.Run, int, int, string) {
	byName := make(map[string]store.Testcase, len(cases))
	// judgedByName holds only the scored cases, in the order they were declared.
	var judged []store.Testcase
	groupMembers := map[string][]string{}
	total := 0
	for _, tc := range cases {
		byName[tc.Name] = tc
		if tc.IsSample {
			continue
		}
		judged = append(judged, tc)
		total += tc.Score
		if tc.GroupName != "" {
			groupMembers[tc.GroupName] = append(groupMembers[tc.GroupName], tc.Name)
		}
	}

	ac := make(map[string]bool, len(reqRuns))
	clean := make(map[string]bool, len(reqRuns))
	for _, rr := range reqRuns {
		if _, known := byName[rr.CaseName]; !known {
			continue
		}
		clean[rr.CaseName] = true
		if judge.VerdictName(rr.Verdict) == judge.AC {
			ac[rr.CaseName] = true
		}
	}
	// A group pays only when all of its cases were reported *and* all of them are
	// AC; a missing row is a failure, not a pass.
	groupOK := make(map[string]bool, len(groupMembers))
	for name, members := range groupMembers {
		groupOK[name] = true
		for _, m := range members {
			if !ac[m] {
				groupOK[name] = false
				break
			}
		}
	}

	// earned is the points a case contributes given the bundle rules.
	earned := func(tc store.Testcase) int {
		if !ac[tc.Name] {
			return 0
		}
		if tc.GroupName != "" && !groupOK[tc.GroupName] {
			return 0
		}
		return tc.Score
	}

	verdict := claimed
	score := 0
	runs := make([]store.Run, 0, len(reqRuns))
	for i, rr := range reqRuns {
		v := judge.VerdictName(rr.Verdict)
		tc, known := byName[rr.CaseName]
		sample := rr.IsSample
		if known {
			sample = tc.IsSample
		}
		ordinal := rr.Ordinal
		if ordinal == 0 {
			ordinal = i + 1
		}
		points := 0
		if known && !sample {
			points = earned(tc)
			score += points
		}
		runs = append(runs, store.Run{
			Ordinal: ordinal, CaseName: rr.CaseName, Verdict: string(v), Score: points,
			TimeMS: rr.TimeMS, MemKB: rr.MemKB, IsSample: sample,
			TimeMin: rr.TimeMin, MemMin: rr.MemMin,
		})
	}

	// The first judged case that did not pass is the one worth naming, whatever the
	// box claimed: a sample mismatch alone does not become the reported case, and
	// the passing rows of a broken bundle are guilty of nothing but being in it.
	failed := ""
	for _, rr := range reqRuns {
		v := judge.VerdictName(rr.Verdict)
		if v == judge.AC {
			continue
		}
		if claimed == judge.AC && verdict == judge.AC {
			verdict = v
		}
		tc, known := byName[rr.CaseName]
		switch {
		case known && !tc.IsSample:
			failed = tc.Name
		case !known && failed == "" && claimed == judge.AC:
			// A row for a case that is gone cannot certify AC either, though a
			// judged row further down would be the better name.
			failed = rr.CaseName
		}
		if failed != "" {
			break
		}
	}
	if claimed == judge.AC && failed == "" && score != total {
		// Every reported row passed, so the gap is a skipped case or a bundle
		// broken by a case that was never reported.
		for _, tc := range judged {
			if !clean[tc.Name] {
				verdict, failed = judge.WA, tc.Name
				break
			}
		}
		if failed == "" {
			for _, tc := range judged {
				if earned(tc) != tc.Score {
					verdict, failed = judge.WA, tc.Name
					break
				}
			}
		}
	}
	// CE/IE fail the whole run without a judged case being implicated, so fall
	// back to the first row that did not pass.
	if failed == "" && verdict != judge.AC {
		for _, rr := range reqRuns {
			if judge.VerdictName(rr.Verdict) != judge.AC {
				failed = rr.CaseName
				break
			}
		}
	}
	return verdict, runs, score, total, failed
}

func submissionID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("submission id must be a positive number")
	}
	return id, nil
}

// judgeRow is one box in the registry the operator watches.
type judgeRow struct {
	ID        int64
	Name      string
	Languages string
	Jail      string
	Caps      string
	Seen      string
	Note      string
}

func (s *Server) handleJudgeList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requirePerm(w, r, permJudgeView); !ok {
		return
	}
	judges, err := s.db.ListJudges(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	rows := judgeRowsOf(judges)
	s.page(w, r, http.StatusOK, "判机", "judges", s.viewerOf(r), msgOf(r),
		map[string]any{"Rows": rows, "Warning": noJailWarning(judges)})
}

// judgeRowsOf 把注册表里的判机排成给人看的那几行。/judges 与站点状态页共用它，
// 因为两页该说同一句话——同一份事实写两处，迟早会有一处先撒谎。
func judgeRowsOf(judges []store.Judge) []judgeRow {
	rows := make([]judgeRow, 0, len(judges))
	for _, j := range judges {
		rows = append(rows, judgeRow{
			ID: j.ID, Name: j.Name, Languages: joinOr(j.Languages, "不限"),
			Jail: jailCell(j),
			// 自报能力为空 = 这台判机是没这功能时注册的：需要版本选项 / SPJ / 交互题的
			// 工单不会派给它，会排队等一台判得了的机器。操作员要能一眼看出是哪台掉队了。
			Caps: joinOr(j.Capabilities, "未自报（老判机：不派 SPJ/交互题/选过版本的工单）"),
			Seen: ago(j.LastSeenAt), Note: j.Note,
		})
	}
	return rows
}

// noJailWarning 是没有任何一台机器有牢房时当面那句警告：这时候提交是在判机的身份下、
// 在宿主文件系统上跑的，而这件事过去只写在启动日志的一行里。
func noJailWarning(judges []store.Judge) string {
	jailed := 0
	for _, j := range judges {
		if j.Jailed {
			jailed++
		}
	}
	if len(judges) == 0 || jailed > 0 {
		return ""
	}
	return "现在在线的判机没有一台会把提交关进牢房：学生的代码是在判机自己的身份下、" +
		"在宿主文件系统上跑的。要么把判机换成 root 起（牢房开得起，提交降到 nobody），" +
		"要么给判机那台机器配好非特权 user namespace（Ubuntu 上就是 " +
		"kernel.apparmor_restrict_unprivileged_userns 那一条），要么放进容器判。" +
		"这不是判题失败：判定照常出，只是没有隔离。"
}

// jailCell 是一行判机的隔离状态。三种说法必须分开：有牢房、报了开不起来、还没报过。
func jailCell(j store.Judge) string {
	switch {
	case j.Jailed:
		return "有牢房"
	case j.JailedKnown:
		return "没有（开不起来）"
	default:
		return "未报（老判机）"
	}
}
