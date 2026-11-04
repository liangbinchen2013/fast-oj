package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// Contests are a web surface only: a person creates one in a browser, and the
// scoreboard is a page rather than an endpoint.

const (
	maxContestTitleRunes = 200
	maxPenaltyMS         = 3_600_000
	// contestLayout is what a browser's datetime-local sends; the seconds are
	// optional because not every browser includes them.
	contestLayoutFull = "2006-01-02T15:04:05"
	contestLayoutMini = "2006-01-02T15:04"
	// clockLayout is how a contest window is printed back to a person.
	clockLayout = "2006-01-02 15:04"
)

// The contest clock is read and written in the server's own zone, and every view
// prints it the same way. A site where half the times are UTC and half are local is
// worse than a site with one documented zone.
func parseContestTime(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{contestLayoutFull, contestLayoutMini} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func formatContestClock(ms int64) string {
	return time.UnixMilli(ms).In(time.Local).Format(clockLayout)
}

// formatContestInput prints the stored moment back in the shape the form field
// needs, so a rejected save does not silently drop the time that was typed.
func formatContestInput(ms int64) string {
	return time.UnixMilli(ms).In(time.Local).Format(contestLayoutMini)
}

// formatSpan writes a duration the way a Chinese reader counts it: 天/小时/分, no
// leading zero units, and never more than two of them.
func formatSpan(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	m := ms / 60000
	const day = 24 * 60
	switch {
	case m >= day:
		return fmt.Sprintf("%d 天 %d 小时", m/day, m%day/60)
	case m >= 60:
		return fmt.Sprintf("%d 小时 %d 分", m/60, m%60)
	case m >= 1:
		return fmt.Sprintf("%d 分", m)
	}
	return fmt.Sprintf("%d 秒", ms/1000)
}

// formatClock is elapsed-contest time for a cell: minutes can run past an hour, so
// mm:ss reads better than a clock face.
func formatClock(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%d:%02d", ms/60000, ms%60000/1000)
}

// validJoinCode keeps the code inside the alphabet a person can read off a screen
// and type without a keyboard layout getting in the way.
func validJoinCode(code string) bool {
	if len(code) > store.MaxJoinCodeLen {
		return false
	}
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// newJoinCode draws a code from the system randomness source. The alphabet drops the
// characters that get misread in a chat window: 0/O, 1/l/I. A failure is reported
// rather than answered with an empty code, because an empty code means 公开赛 and
// would quietly throw the door open.
func newJoinCode() (string, error) {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	out := make([]byte, 8)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// contestForm is one contest as the browser posts it.
type contestForm struct {
	CID, Title, Statement, Mode string
	JoinCode                    string
	StartAt, EndAt              int64
	PenaltyMS                   int64
	TeamSize                    int
	StandingOpen, Visible       bool
	// The raw text of every numeric and time field, so a rejected save can put back
	// exactly what was typed instead of a plausible-looking zero.
	StartText, EndText        string
	TeamSizeText, PenaltyText string
}

func (f contestForm) apply(c *store.Contest) {
	c.CID, c.Title, c.Statement, c.Mode = f.CID, f.Title, f.Statement, f.Mode
	c.JoinCode, c.StartAt, c.EndAt = f.JoinCode, f.StartAt, f.EndAt
	c.PenaltyMS, c.TeamSize = f.PenaltyMS, f.TeamSize
	c.StandingOpen, c.Visible = f.StandingOpen, f.Visible
}

// parseContestForm reads the form and reports every problem in it, so a rejected
// save can redraw the page with everything already typed.
func parseContestForm(r *http.Request) (contestForm, []string) {
	f := contestForm{
		CID:          strings.TrimSpace(r.FormValue("cid")),
		Title:        strings.TrimSpace(r.FormValue("title")),
		Statement:    r.FormValue("statement"),
		Mode:         r.FormValue("mode"),
		JoinCode:     strings.TrimSpace(r.FormValue("join_code")),
		StartText:    strings.TrimSpace(r.FormValue("start_at")),
		EndText:      strings.TrimSpace(r.FormValue("end_at")),
		TeamSizeText: strings.TrimSpace(r.FormValue("team_size")),
		PenaltyText:  strings.TrimSpace(r.FormValue("penalty_s")),
		StandingOpen: r.FormValue("standing_open") == "1",
		Visible:      r.FormValue("visible") == "1",
	}
	var errs []string
	if ms, ok := parseContestTime(f.StartText); ok {
		f.StartAt = ms
	} else {
		errs = append(errs, "开始时间没看懂，形如 2026-01-02T15:04")
	}
	if ms, ok := parseContestTime(f.EndText); ok {
		f.EndAt = ms
	} else {
		errs = append(errs, "结束时间没看懂，形如 2026-01-02T15:04")
	}
	if f.StartAt > 0 && f.EndAt > 0 && f.EndAt <= f.StartAt {
		errs = append(errs, "结束时间必须晚于开始时间")
	}
	if f.CID == "" {
		// Blank is a request to generate, not a missing field.
	} else if len(f.CID) > store.MaxCIDLen || strings.ContainsAny(f.CID, "/\\ \t") {
		errs = append(errs, fmt.Sprintf("比赛编号要 1..%d 个字符，且不含斜杠或空格", store.MaxCIDLen))
	}
	if f.Title == "" || tooLong(f.Title, maxContestTitleRunes) {
		errs = append(errs, fmt.Sprintf("标题要 1..%d 个字符", maxContestTitleRunes))
	}
	if f.Mode != "" && !store.ValidContestMode(f.Mode) {
		errs = append(errs, "计分模式只认 oi、ioi 或 acm")
	}
	if f.JoinCode != "" && !validJoinCode(f.JoinCode) {
		errs = append(errs, "加入码只能是字母、数字和连字符，最长 32 位")
	}
	size, err := strconv.Atoi(f.TeamSizeText)
	if err != nil || size < store.MinTeamSize || size > store.MaxTeamSize {
		errs = append(errs, fmt.Sprintf("队伍人数要 %d..%d", store.MinTeamSize, store.MaxTeamSize))
	} else {
		f.TeamSize = size
	}
	penaltyS, err := strconv.Atoi(f.PenaltyText)
	if err != nil || penaltyS < 0 || penaltyS*1000 > maxPenaltyMS {
		errs = append(errs, "罚时要 0..3600 秒")
	} else {
		f.PenaltyMS = int64(penaltyS) * 1000
	}
	return f, errs
}

// contestData is the payload for the create and edit pages.
type contestData struct {
	CID, Title, Statement, Mode string
	JoinCode                    string
	StartText, EndText          string
	TeamSize                    int
	TeamSizeText, PenaltyText   string
	StandingOpen, Visible       bool
	IsNew                       bool
	Errors                      []string
	Preview                     template.HTML
	Action, Key                 string
	ModeOptions                 []detailOption
	ProblemCount, MemberCount   int
	Problems                    []contestProblemRow
	Bank                        []bankEntry
	PhaseLabel, PhaseClass      string
	Window                      string
}

// newContestData builds the form payload from a stored row, or from the defaults a
// first-time host would want.
func newContestData(c store.Contest, isNew bool) contestData {
	d := contestData{
		IsNew: isNew, ModeOptions: contestModeOptions,
		Key: c.CID, Action: "/contests/" + url.PathEscape(c.CID),
	}
	if isNew {
		now := time.Now()
		// A contest created "now" starts in five minutes and runs two hours, which is
		// what a person means when they only fill in the title.
		start := now.Add(5 * time.Minute)
		d.Action = "/contests"
		d.Mode, d.TeamSize = store.ContestOI, 1
		d.StartText = start.Format(contestLayoutMini)
		d.EndText = start.Add(2 * time.Hour).Format(contestLayoutMini)
		d.TeamSizeText = "1"
		d.PenaltyText = strconv.Itoa(store.DefaultPenaltyS)
		d.Visible = true
		return d
	}
	d.CID, d.Title, d.Statement, d.Mode = c.CID, c.Title, c.Statement, c.Mode
	d.JoinCode = c.JoinCode
	d.StartText, d.EndText = formatContestInput(c.StartAt), formatContestInput(c.EndAt)
	d.TeamSize, d.TeamSizeText = c.TeamSize, strconv.Itoa(c.TeamSize)
	d.PenaltyText = strconv.FormatInt(c.PenaltyMS/1000, 10)
	d.StandingOpen, d.Visible = c.StandingOpen, c.Visible
	d.PhaseLabel, d.PhaseClass = phaseView(c, time.Now().UnixMilli())
	d.Window = formatContestClock(c.StartAt) + " —— " + formatContestClock(c.EndAt)
	return d
}

// fillFromForm keeps what was typed when the save was rejected.
func (d *contestData) fillFromForm(f contestForm, errs []string) {
	d.CID, d.Title, d.Statement, d.Mode = f.CID, f.Title, f.Statement, f.Mode
	d.JoinCode = f.JoinCode
	d.StartText, d.EndText = f.StartText, f.EndText
	d.TeamSize, d.Visible, d.StandingOpen = f.TeamSize, f.Visible, f.StandingOpen
	d.TeamSizeText, d.PenaltyText = f.TeamSizeText, f.PenaltyText
	d.Errors = errs
}

var contestModeOptions = []detailOption{
	{Value: store.ContestOI, Label: "OI 赛制（CSP/NOIP/NOI）：取最后一次提交，同分同名次，比赛中不出榜"},
	{Value: store.ContestIOI, Label: "IOI 赛制：每题取最高分，同分比总用时"},
	{Value: store.ContestACM, Label: "ACM 赛制：比解题数，再比首过时间加罚时"},
}

// bankEntry is one problem offered by the mount field.
type bankEntry struct {
	Key, Label string
}

// contestProblemRow is one mounted problem as the management list shows it.
type contestProblemRow struct {
	ProblemID  int64
	Label, PID string
	Title      string
	Ordinal    int
	PointsText string
	NoCases    bool
	Hidden     bool
	// Entry is the contest's own way into the problem, so the submission form knows
	// which contest it belongs to. Empty on the management page, where the plain
	// problem link is what the host wants.
	Entry string
}

func contestProblemOf(cp store.ContestProblem) contestProblemRow {
	points := "题目满分 " + strconv.Itoa(cp.TotalScore)
	if cp.Points > 0 {
		points = strconv.Itoa(cp.Points) + "（题目满分 " + strconv.Itoa(cp.TotalScore) + "）"
	}
	return contestProblemRow{
		ProblemID: cp.ProblemID, Label: store.ContestLabel(cp.Label, cp.Ordinal),
		PID: cp.PID, Title: cp.Title, Ordinal: cp.Ordinal,
		PointsText: points, NoCases: cp.TotalScore == 0, Hidden: !cp.ProblemVisible,
	}
}

// phaseView is the label plus its CSS class, so the pages and the board agree.
func phaseView(c store.Contest, now int64) (string, string) {
	phase := c.Phase(now)
	return phase.Label(), phase.Badge()
}

// contestAccess is the single answer to "what may this reader see here", shared by
// every contest page so the templates hold no permission arithmetic.
type contestAccess struct {
	Joined       bool
	CanManage    bool
	ShowProblems bool
	ShowMembers  bool
	ShowBoard    bool
	// Pending is the message a reader who cannot see the problems should get.
	WhyNot string
}

func (s *Server) contestAccessFor(ctx context.Context, c store.Contest, a actor, now int64) contestAccess {
	v := contestAccess{CanManage: a.canManageContest(c)}
	if a.User.ID != 0 {
		_, v.Joined = s.db.ContestMemberOf(ctx, c.ID, a.User.ID)
	}
	// A mount list is the contest's own material: the host and the people inside the
	// contest see it, a passing reader does not.
	v.ShowProblems = v.CanManage || v.Joined
	v.ShowMembers = v.CanManage || v.Joined
	// Anyone inside the contest sees a board: the whole field when it is open, their
	// own row when it is sealed. Only an outsider is turned away.
	v.ShowBoard = c.StandingOpen || v.CanManage || v.Joined
	switch {
	case v.ShowProblems:
	case c.JoinCode != "" && c.Phase(now) != store.PhaseOver:
		v.WhyNot = "这是一场私有比赛，凭加入码进来之后才能看题目。"
	case c.Phase(now) == store.PhaseOver:
		v.WhyNot = "这场比赛已经结束，题目只对当时的成员开放。"
	default:
		v.WhyNot = "加入这场比赛之后就能看题目。"
	}
	return v
}

// handleContestList is the public board of contests. Staff also see the hidden
// ones, which is how a host finds their own draft again.
func (s *Server) handleContestList(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := pageLimit(r.URL.Query().Get("limit"))
	search := r.URL.Query().Get("q")
	page, err := s.db.ListContests(r.Context(), after, limit, search, a.canHostContests())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	now := time.Now().UnixMilli()
	rows := make([]contestListRow, 0, len(page.Items))
	for _, item := range page.Items {
		label, class := phaseView(item.Contest, now)
		rows = append(rows, contestListRow{
			CID: item.CID, Title: item.Title, Phase: label, PhaseClass: class,
			Mode: contestModeLabel(item.Mode), Start: formatContestClock(item.StartAt),
			End:      formatContestClock(item.EndAt),
			Problems: item.ProblemCount, Members: item.MemberCount,
			Private:   item.JoinCode != "",
			Hidden:    !item.Visible,
			BoardOpen: item.StandingOpen,
		})
	}
	s.page(w, r, http.StatusOK, "比赛", "contests", a.viewer(), msgOf(r), map[string]any{
		"Rows": rows, "Total": page.Total, "Limit": limit, "Next": page.NextAfter, "Query": search,
		"CanHost": a.canHostContests(),
	})
}

type contestListRow struct {
	CID, Title string
	Phase      string
	PhaseClass string
	Mode       string
	Start, End string
	Problems   int
	Members    int
	Private    bool
	Hidden     bool
	BoardOpen  bool
}

func contestModeLabel(mode string) string {
	switch mode {
	case store.ContestIOI:
		return "IOI"
	case store.ContestACM:
		return "ACM"
	}
	return "OI"
}

// penaltyNote is honest about the one mode that actually uses the penalty setting:
// an OI or IOI board has no penalty to speak of, so printing a number there would
// describe a rule the scoreboard ignores.
func penaltyNote(c store.Contest) string {
	if c.Mode != store.ContestACM {
		return "本赛制不比罚时"
	}
	if c.PenaltyMS == 0 {
		return "不设罚时"
	}
	return "每次未过尝试罚 " + strconv.FormatInt(c.PenaltyMS/60000, 10) + " 分钟"
}

func (s *Server) handleContestNew(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next=/contests/new", http.StatusSeeOther)
		return
	}
	if !a.can(permContestCreate) {
		s.respondError(w, r, http.StatusForbidden, "缺少权限："+permContestCreate)
		return
	}
	d := newContestData(store.Contest{}, true)
	s.page(w, r, http.StatusOK, "新建比赛", "contest_edit", a.viewer(), msgOf(r), d)
}

func (s *Server) handleContestCreate(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next=/contests/new", http.StatusSeeOther)
		return
	}
	if !s.site(r.Context()).AllowContests {
		s.featureClosed(w, r, "建比赛")
		return
	}
	if !a.can(permContestCreate) {
		s.respondError(w, r, http.StatusForbidden, "缺少权限："+permContestCreate)
		return
	}
	f, errs := parseContestForm(r)
	if r.FormValue("newcode") == "1" {
		code, err := newJoinCode()
		if err != nil {
			errs = append(errs, "生成加入码失败："+err.Error())
		} else {
			f.JoinCode = code
		}
	}
	d := newContestData(store.Contest{}, true)
	d.fillFromForm(f, errs)
	if r.FormValue("preview") == "1" {
		d.Preview = renderMarkdownHTML(f.Statement)
		s.page(w, r, http.StatusOK, "新建比赛", "contest_edit", a.viewer(), "", d)
		return
	}
	if len(errs) > 0 {
		s.page(w, r, http.StatusBadRequest, "新建比赛", "contest_edit", a.viewer(), "", d)
		return
	}
	var c store.Contest
	c.OwnerID = a.User.ID
	f.apply(&c)
	created, err := s.db.CreateContest(r.Context(), &c)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			d.fillFromForm(f, []string{"比赛编号 " + f.CID + " 已经存在"})
			s.page(w, r, http.StatusConflict, "新建比赛", "contest_edit", a.viewer(), "", d)
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("contest created", "cid", created.CID, "id", created.ID, "by", a.User.Handle)
	http.Redirect(w, r, "/contests/"+url.PathEscape(created.CID)+"/edit?msg="+
		url.QueryEscape("比赛已建好，现在挂题"), http.StatusSeeOther)
}

// contestForEdit resolves {key} and applies the management right.
func (s *Server) contestForEdit(w http.ResponseWriter, r *http.Request) (actor, store.Contest, bool) {
	c, err := s.db.FindContest(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return actor{}, c, false
	}
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
		return actor{}, c, false
	}
	if !a.canManageContest(c) {
		s.respondError(w, r, http.StatusForbidden,
			"这场比赛由他人创建，需要 "+permContestManageAny+" 才能修改")
		return a, c, false
	}
	return a, c, true
}

func (s *Server) handleContestEdit(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestForEdit(w, r)
	if !ok {
		return
	}
	d, err := s.contestEditData(r, c)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "编辑 "+c.CID, "contest_edit", a.viewer(), msgOf(r), d)
}

// contestEditData adds what the host needs next to the form: the mount list and the
// problem bank the mount field offers.
func (s *Server) contestEditData(r *http.Request, c store.Contest) (contestData, error) {
	d := newContestData(c, false)
	problems, err := s.db.ListContestProblems(r.Context(), c.ID)
	if err != nil {
		return d, err
	}
	for _, cp := range problems {
		row := contestProblemOf(cp)
		d.Problems = append(d.Problems, row)
	}
	d.ProblemCount = len(problems)
	members, err := s.db.ListContestMembers(r.Context(), c.ID)
	if err != nil {
		return d, err
	}
	d.MemberCount = len(members)
	a, _ := s.authenticate(r)
	bank, err := s.db.ListProblems(r.Context(), 0, 500, "", !a.can(permProblemEditAny))
	if err != nil {
		return d, err
	}
	for _, p := range bank.Items {
		d.Bank = append(d.Bank, bankEntry{Key: p.PID, Label: p.PID + "　" + p.Title})
	}
	return d, nil
}

func (s *Server) handleContestUpdate(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestForEdit(w, r)
	if !ok {
		return
	}
	f, errs := parseContestForm(r)
	if r.FormValue("newcode") == "1" {
		code, err := newJoinCode()
		if err != nil {
			errs = append(errs, "生成加入码失败："+err.Error())
		} else {
			f.JoinCode = code
		}
	}
	d, err := s.contestEditData(r, c)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	d.fillFromForm(f, errs)
	if r.FormValue("preview") == "1" {
		d.Preview = renderMarkdownHTML(f.Statement)
		s.page(w, r, http.StatusOK, "编辑 "+c.CID, "contest_edit", a.viewer(), "", d)
		return
	}
	if len(errs) > 0 {
		s.page(w, r, http.StatusBadRequest, "编辑 "+c.CID, "contest_edit", a.viewer(), "", d)
		return
	}
	f.apply(&c)
	updated, err := s.db.UpdateContest(r.Context(), c)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			d.fillFromForm(f, []string{"比赛编号 " + f.CID + " 已经存在"})
			s.page(w, r, http.StatusConflict, "编辑 "+c.CID, "contest_edit", a.viewer(), "", d)
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("contest updated", "cid", updated.CID, "by", a.User.Handle)
	http.Redirect(w, r, "/contests/"+url.PathEscape(updated.CID)+"?msg="+url.QueryEscape("已保存"), http.StatusSeeOther)
}

func (s *Server) handleContestDelete(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestForEdit(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteContest(r.Context(), c.ID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.audit(r, a, "contest.delete", c.CID, "已删除比赛：「"+c.Title+"」（名单与挂题一起没，历史提交保留）")
	s.log.Info("contest deleted", "cid", c.CID, "id", c.ID, "by", a.User.Handle)
	http.Redirect(w, r, "/contests?msg="+url.QueryEscape("已删除 "+c.CID+"：名单与挂题记录一并清掉了，历史提交还在"), http.StatusSeeOther)
}

// handleContestProblemSave mounts a problem, or re-saves the settings of one that is
// already there. A problem that is not published may only be mounted by someone who
// may edit it, so hosting a contest cannot leak somebody else's hidden problem.
func (s *Server) handleContestProblemSave(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestForEdit(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.FormValue("problem"))
	if key == "" {
		s.backToContestEdit(w, r, c, "要填题目编号")
		return
	}
	p, err := s.db.FindProblem(r.Context(), key)
	if err != nil {
		s.backToContestEdit(w, r, c, "找不到题目 "+key)
		return
	}
	points, err := strconv.Atoi(strings.TrimSpace(defaultZero(r.FormValue("points"))))
	if err != nil || points < 0 {
		s.backToContestEdit(w, r, c, "比赛内分值要是不小于 0 的整数")
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if len(label) > 8 || strings.ContainsAny(label, "/\\ \t") {
		s.backToContestEdit(w, r, c, "题号标签最多 8 个字符，不能含斜杠或空格")
		return
	}
	if !p.Visible && !a.canEditProblem(p) {
		s.backToContestEdit(w, r, c, "题目 "+p.PID+" 没公开，只有它的作者或持 "+permProblemEditAny+" 的人能挂进比赛")
		return
	}
	cp := store.ContestProblem{ContestID: c.ID, ProblemID: p.ID, Points: points, Label: label}
	if mounted, _ := s.db.ContestProblemMounted(r.Context(), c.ID, p.ID); mounted {
		if _, err := s.db.UpdateContestProblem(r.Context(), cp); err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		s.log.Info("contest problem updated", "cid", c.CID, "pid", p.PID, "by", a.User.Handle)
		s.backToContestEdit(w, r, c, "已更新 "+p.PID+" 的比赛设置")
		return
	}
	created, err := s.db.AddContestProblem(r.Context(), &cp)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.backToContestEdit(w, r, c, "挂不上去："+err.Error())
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("contest problem mounted", "cid", c.CID, "pid", p.PID, "label",
		store.ContestLabel(created.Label, created.Ordinal), "by", a.User.Handle)
	s.backToContestEdit(w, r, c, "已挂上 "+store.ContestLabel(created.Label, created.Ordinal)+"："+p.PID)
}

func (s *Server) handleContestProblemRemove(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestForEdit(w, r)
	if !ok {
		return
	}
	p, err := s.db.FindProblem(r.Context(), r.PathValue("problemKey"))
	if err != nil {
		s.backToContestEdit(w, r, c, "找不到题目")
		return
	}
	if err := s.db.RemoveContestProblem(r.Context(), c.ID, p.ID); err != nil {
		s.backToContestEdit(w, r, c, "这道题没挂在这场比赛里")
		return
	}
	s.log.Info("contest problem removed", "cid", c.CID, "pid", p.PID, "by", a.User.Handle)
	s.audit(r, a, "contest.problem.remove", c.CID, "移下 "+p.PID+"（历史提交照算，榜上多一列「已移除」）")
	// History is untouched: submissions keep their contest_id and still pay, into a
	// column the board marks 已移除.
	s.backToContestEdit(w, r, c, "已移下 "+p.PID+"（历史提交照算，榜上会多一列「已移除」）")
}

func (s *Server) handleContestProblemMove(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestForEdit(w, r)
	if !ok {
		return
	}
	p, err := s.db.FindProblem(r.Context(), r.PathValue("problemKey"))
	if err != nil {
		s.backToContestEdit(w, r, c, "找不到题目")
		return
	}
	up := r.FormValue("dir") == "up"
	if err := s.db.MoveContestProblem(r.Context(), c.ID, p.ID, up); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("contest problem moved", "cid", c.CID, "pid", p.PID, "dir", r.FormValue("dir"), "by", a.User.Handle)
	s.backToContestEdit(w, r, c, "已调整 "+p.PID+" 的顺序")
}

// defaultZero treats a blank number field as the neutral value rather than a typo,
// because a browser posts every field of the form.
func defaultZero(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "0"
	}
	return raw
}

// backToContestEdit is the one redirect every contest write ends with, so the flash
// always lands on the page the host was working on.
func (s *Server) backToContestEdit(w http.ResponseWriter, r *http.Request, c store.Contest, msg string) {
	http.Redirect(w, r, "/contests/"+url.PathEscape(c.CID)+"/edit?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// contestHomeData is what the contest page shows a reader.
type contestHomeData struct {
	CID, Title   string
	Statement    template.HTML
	HasStatement bool
	Phase        string
	PhaseClass   string
	Mode         string
	Window       string
	Countdown    string
	TeamSize     int
	PenaltyText  string
	BoardOpen    bool
	// BoardPending is the OI case: the board exists but nobody but the host may see
	// it until the clock stops.
	BoardPending bool
	Private      bool
	Problems     []contestProblemRow
	Teams        []contestTeamRow
	Solo         []string
	Access       contestAccess
	Members      int
	CanJoin      bool
	CanManage    bool
	EditLink     string
	StandLink    string
}

// contestTeamRow is one team on the roster.
type contestTeamRow struct {
	ID      int64
	Name    string
	Members []string
	Full    bool
}

// handleContestHome is the contest front page: the window, the rules, the roster and
// the problem list as far as the reader is allowed to see it.
func (s *Server) handleContestHome(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.FindContest(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	a, _ := s.authenticate(r)
	if !c.Visible && !a.canManageContest(c) && !a.canHostContests() {
		s.respondError(w, r, http.StatusNotFound, "没有这场比赛："+c.CID)
		return
	}
	now := time.Now().UnixMilli()
	access := s.contestAccessFor(r.Context(), c, a, now)
	d := contestHomeData{
		CID: c.CID, Title: c.Title, Statement: renderMarkdownHTML(c.Statement),
		HasStatement: strings.TrimSpace(c.Statement) != "",
		Mode:         contestModeLabel(c.Mode),
		Window:       formatContestClock(c.StartAt) + " —— " + formatContestClock(c.EndAt),
		TeamSize:     c.TeamSize, Access: access, CanManage: access.CanManage,
		BoardOpen:    c.StandingOpen && c.BoardLive(now),
		BoardPending: !c.BoardLive(now),
		Private:      c.JoinCode != "",
		// 已结束不再收人：赛后补进榜会把榜单变成一份事后总结。
		CanJoin:   !access.Joined && c.Phase(now) != store.PhaseOver,
		EditLink:  "/contests/" + url.PathEscape(c.CID) + "/edit",
		StandLink: "/contests/" + url.PathEscape(c.CID) + "/standings",
	}
	switch c.Phase(now) {
	case store.PhaseUpcoming:
		d.Countdown = "距开始还有 " + formatSpan(c.StartAt-now)
	case store.PhaseRunning:
		d.Countdown = "距结束还有 " + formatSpan(c.EndAt-now)
	case store.PhaseOver:
		d.Countdown = "已结束 " + formatSpan(now-c.EndAt) + "，成绩不再变动"
	}
	d.Phase = c.Phase(now).Label()
	d.PhaseClass = c.Phase(now).Badge()
	d.PenaltyText = penaltyNote(c)
	if access.ShowProblems {
		problems, err := s.db.ListContestProblems(r.Context(), c.ID)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		for _, cp := range problems {
			row := contestProblemOf(cp)
			// Inside a contest the entry point carries the contest, so the problem
			// page knows which submission belongs where.
			row.Entry = "/contests/" + url.PathEscape(c.CID) + "/p/" + url.PathEscape(row.Label)
			d.Problems = append(d.Problems, row)
		}
	}
	if access.ShowMembers {
		members, err := s.db.ListContestMembers(r.Context(), c.ID)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		d.Members = len(members)
		for _, r := range store.TeamRoster(members) {
			if r.TeamID == 0 {
				d.Solo = append(d.Solo, r.Handles...)
				continue
			}
			d.Teams = append(d.Teams, contestTeamRow{
				ID: r.TeamID, Name: r.Name, Members: r.Handles, Full: len(r.Handles) >= c.TeamSize,
			})
		}
	}
	s.page(w, r, http.StatusOK, c.Title, "contest", a.viewer(), msgOf(r), d)
}

// contestByKey loads {key} for the pages that any reader may reach.
func (s *Server) contestByKey(w http.ResponseWriter, r *http.Request) (actor, store.Contest, bool) {
	c, err := s.db.FindContest(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return actor{}, c, false
	}
	a, _ := s.authenticate(r)
	if !c.Visible && !a.canManageContest(c) && !a.canHostContests() {
		s.respondError(w, r, http.StatusNotFound, "没有这场比赛："+c.CID)
		return a, c, false
	}
	return a, c, true
}

// backToContest is the one redirect every contest write ends with, so the flash lands
// on the page the reader was on.
func (s *Server) backToContest(w http.ResponseWriter, r *http.Request, c store.Contest, msg string) {
	http.Redirect(w, r, "/contests/"+url.PathEscape(c.CID)+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// signedInToContest sends an anonymous browser to the login page with the contest as
// the way back, because "请先登录" alone is not something a person can act on.
func (s *Server) signedInToContest(w http.ResponseWriter, r *http.Request, c store.Contest) (actor, bool) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/contests/"+c.CID), http.StatusSeeOther)
		return a, false
	}
	return a, true
}

func (s *Server) handleContestJoin(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.FindContest(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	// 未公开且没有加入码的比赛（草稿/内部赛）的 join 入口按不存在处理：知道/猜到 CID
	// 不能把草稿赛变成成员可见。隐藏但有码的私有赛仍走加入码；主办方本来就能管理整场。
	if !c.Visible && c.JoinCode == "" {
		s.respondError(w, r, http.StatusNotFound, "没有这场比赛："+c.CID)
		return
	}
	a, ok := s.signedInToContest(w, r, c)
	if !ok {
		return
	}
	if !s.site(r.Context()).AllowContests {
		s.featureClosed(w, r, "参加比赛")
		return
	}
	if msg, banned := a.bannedFrom(permContestJoin, "参加比赛"); banned {
		s.backToContest(w, r, c, msg)
		return
	}
	// 邮箱没验证的人也不能占一个参赛名额（M25f）。赛内提交另外在 createSubmission 里拦，
	// 所以这里挡住的是"进名单"这一步。
	if why := s.verifyNotice(r, a); why != "" {
		s.backToContest(w, r, c, why)
		return
	}
	if _, err := s.db.JoinContest(r.Context(), c.ID, a.User.ID, r.FormValue("code"), ""); err != nil {
		s.backToContest(w, r, c, err.Error())
		return
	}
	s.log.Info("contest joined", "cid", c.CID, "user", a.User.Handle)
	s.backToContest(w, r, c, "已加入「"+c.Title+"」，可以去做题了")
}

func (s *Server) handleContestLeave(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.FindContest(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	a, ok := s.signedInToContest(w, r, c)
	if !ok {
		return
	}
	// Existing submissions keep their contest_id: leaving does not rewrite history.
	if err := s.db.LeaveContest(r.Context(), c.ID, a.User.ID); err != nil {
		s.backToContest(w, r, c, err.Error())
		return
	}
	s.log.Info("contest left", "cid", c.CID, "user", a.User.Handle)
	s.backToContest(w, r, c, "已退出这场比赛，之前的提交记录还在")
}

// handleContestTeam is the one form for the three team moves: create a team and join
// it, join an existing one by name, or step out.
func (s *Server) handleContestTeam(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.FindContest(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !c.Visible && c.JoinCode == "" {
		// 组队表单也是写入口：未公开且无码的比赛不通过它确认存在，更不允许非成员建空队占名。
		s.respondError(w, r, http.StatusNotFound, "没有这场比赛："+c.CID)
		return
	}
	a, ok := s.signedInToContest(w, r, c)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	op := r.FormValue("op")
	if op != "leave" && !store.ValidTeamName(name) {
		s.backToContest(w, r, c, "队名要 1.."+strconv.Itoa(store.MaxTeamNameLen)+" 个字符，且不含斜杠")
		return
	}
	switch op {
	case "leave":
		if err := s.db.SetTeam(r.Context(), c.ID, a.User.ID, 0); err != nil {
			s.backToContest(w, r, c, err.Error())
			return
		}
		s.backToContest(w, r, c, "已退出队伍，现在按个人算名次")
		return
	case "create":
		team, err := s.db.CreateTeam(r.Context(), c.ID, name)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				s.backToContest(w, r, c, "已经有队伍叫 "+name+" 了，用「加入队伍」吧")
				return
			}
			s.writeStoreError(w, r, err)
			return
		}
		if err := s.db.SetTeam(r.Context(), c.ID, a.User.ID, team.ID); err != nil {
			s.backToContest(w, r, c, err.Error())
			return
		}
		s.log.Info("contest team created", "cid", c.CID, "team", name, "by", a.User.Handle)
		s.backToContest(w, r, c, "已创建队伍 "+name)
		return
	case "join":
		team, err := s.db.FindTeam(r.Context(), c.ID, name)
		if err != nil {
			s.backToContest(w, r, c, "没有队伍叫 "+name)
			return
		}
		if err := s.db.SetTeam(r.Context(), c.ID, a.User.ID, team.ID); err != nil {
			s.backToContest(w, r, c, err.Error())
			return
		}
		s.log.Info("contest team joined", "cid", c.CID, "team", name, "by", a.User.Handle)
		s.backToContest(w, r, c, "已加入队伍 "+name)
		return
	}
	s.backToContest(w, r, c, "不认识的操作："+op)
}

// handleContestProblemEntry is the contest's own link into a problem: it resolves the
// letter inside this contest and hands the browser over to the problem page with the
// contest attached. Only members and the host get in, so a shared link cannot be used
// to read a sealed contest's statements.
func (s *Server) handleContestProblemEntry(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestByKey(w, r)
	if !ok {
		return
	}
	access := s.contestAccessFor(r.Context(), c, a, time.Now().UnixMilli())
	if !access.ShowProblems {
		s.backToContest(w, r, c, access.WhyNot)
		return
	}
	label := strings.TrimSpace(r.PathValue("label"))
	problems, err := s.db.ListContestProblems(r.Context(), c.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	for _, cp := range problems {
		if strings.EqualFold(store.ContestLabel(cp.Label, cp.Ordinal), label) {
			http.Redirect(w, r, "/problems/"+url.PathEscape(cp.PID)+
				"?contest="+strconv.FormatInt(c.ID, 10), http.StatusSeeOther)
			return
		}
	}
	s.backToContest(w, r, c, "这场比赛没有第 "+label+" 题")
}

// --- M14d: the independent board ---

// standingCellView is one competitor/problem square. The number is the score (OI) or
// the attempt count (ACM) and carries the colour; the time always goes underneath in
// grey, because a coloured clock reads like a verdict.
type standingCellView struct {
	Label   string
	PID     string
	Title   string
	Text    string
	Class   string
	When    string
	Seen    bool
	Removed bool
}

// scoreClass is the red→green scale: the further a score is from the problem's own
// maximum, the redder the number. 0 分 = sc0（红），满分 = sc9（绿）。
func scoreClass(points, max int) string {
	if max <= 0 || points <= 0 {
		return "sc0"
	}
	if points >= max {
		return "sc9"
	}
	return "sc" + strconv.Itoa(points*9/max)
}

type standingRowView struct {
	Rank    int
	Name    string
	Members string
	Score   string
	// ScoreClass colours the total the same way the cells are coloured: OI 按得分
	// 百分比，ACM 按解题数占题数比例。
	ScoreClass string
	Time       string
	Cells      []standingCellView
	Me         bool
}

type standingsData struct {
	CID, Title  string
	Mode        string
	Phase       string
	PhaseClass  string
	Window      string
	Sealed      bool
	BoardOpen   bool
	CanManage   bool
	Rows        []standingRowView
	Columns     []string
	PenaltyText string
	// ShowsClock is false for OI: that board has no clock and no attempt counts.
	ShowsClock bool
	// Live is false while an OI contest is still running, which is a different
	// answer from 封榜: nobody but the host may see anything yet.
	Live     bool
	Members  int
	SelfRank string
	WhyNot   string
	Back     string
	TeamMode bool
}

// handleContestStandings is the contest's own board. The whole field is always
// computed, because a sealed board still owes the reader a true place; what changes is
// how many rows are printed.
func (s *Server) handleContestStandings(w http.ResponseWriter, r *http.Request) {
	a, c, ok := s.contestByKey(w, r)
	if !ok {
		return
	}
	now := time.Now().UnixMilli()
	access := s.contestAccessFor(r.Context(), c, a, now)
	d := standingsData{
		CID: c.CID, Title: c.Title, Mode: contestModeLabel(c.Mode),
		Phase: c.Phase(now).Label(), PhaseClass: c.Phase(now).Badge(),
		Window:    formatContestClock(c.StartAt) + " —— " + formatContestClock(c.EndAt),
		BoardOpen: c.StandingOpen, CanManage: access.CanManage,
		Sealed:     !c.StandingOpen,
		Back:       "/contests/" + url.PathEscape(c.CID),
		TeamMode:   c.TeamSize > store.MinTeamSize,
		ShowsClock: c.Mode != store.ContestOI,
		Live:       c.BoardLive(now),
	}
	if members, err := s.db.ListContestMembers(r.Context(), c.ID); err == nil {
		d.Members = len(members)
	}
	if !c.BoardLive(now) && !access.CanManage {
		// OI 是闭卷的：比赛结束之前谁都没有榜，包括自己。这跟封榜是两件事，
		// 封榜还能看见自己那一行，这里是什么都看不见。
		d.WhyNot = "OI 赛制比赛中不实时出榜：提交看不到结果，成绩要等比赛结束后由主办方放榜。"
		d.Rows = nil
		s.page(w, r, http.StatusOK, "成绩榜 "+c.CID, "contest_standings", a.viewer(), msgOf(r), d)
		return
	}
	if !access.ShowBoard {
		// 封榜 and not inside the contest: nothing to show, and the page says why
		// rather than printing an empty table that looks like a bug.
		d.WhyNot = "这场比赛正在封榜，别人看不到榜单；加入之后能看到你自己的名次。"
		if c.Phase(now) == store.PhaseOver {
			d.WhyNot = "这场比赛已经结束，主办方还没放榜。加入过的人仍能看到自己那一行。"
		}
		s.page(w, r, http.StatusOK, "成绩榜 "+c.CID, "contest_standings", a.viewer(), msgOf(r), d)
		return
	}

	rows, err := s.db.ContestScoreboard(r.Context(), c)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	// The columns are the same for every row, so they are read off the first one.
	if len(rows) > 0 {
		for _, cell := range rows[0].Cells {
			d.Columns = append(d.Columns, cell.Label)
		}
	}
	d.PenaltyText = penaltyNote(c)

	// A sealed board shows a member their own row only, so nobody can read the field
	// over someone's shoulder. The host sees everything, marked as sealed.
	onlySelf := d.Sealed && !access.CanManage
	for _, row := range rows {
		me := false
		for _, id := range row.UserIDs {
			if id == a.User.ID {
				me = true
			}
		}
		if onlySelf && !me {
			continue
		}
		view := standingRowView{
			Rank: row.Rank, Name: row.Name, Members: strings.Join(row.Handles, "、"),
			Me: me,
		}
		// 总分那一列也是分数，所以走同一把色阶：满分绿、零分红。
		total, full := row.Score, 0
		for _, cell := range row.Cells {
			full += cell.Max
		}
		if c.Mode == store.ContestACM {
			total, full = row.Solved, len(row.Cells)
			view.Score = strconv.Itoa(row.Solved)
		} else {
			view.Score = strconv.Itoa(row.Score)
		}
		view.ScoreClass = scoreClass(total, full)
		if d.ShowsClock {
			view.Time = formatClock(row.TimeMS)
		}
		for _, cell := range row.Cells {
			view.Cells = append(view.Cells, cellView(c.Mode, cell, d.ShowsClock))
		}
		d.Rows = append(d.Rows, view)
	}
	if onlySelf {
		// The rank still comes from the whole field, so a private board can tell the
		// truth about your place without printing anyone else.
		if own, found, err := s.db.ContestSelfStanding(r.Context(), c, a.User.ID); err == nil && found {
			d.SelfRank = "第 " + strconv.Itoa(own.Rank) + " 名"
		}
	}
	s.page(w, r, http.StatusOK, "成绩榜 "+c.CID, "contest_standings", a.viewer(), msgOf(r), d)
}

// cellView turns one stored square into what a reader should see for that mode.
func cellView(mode string, cell store.Cell, showsClock bool) standingCellView {
	v := standingCellView{
		Label: cell.Label, PID: cell.PID, Title: cell.Title,
		Seen: cell.Seen, Removed: cell.Removed,
	}
	if !v.Seen {
		v.Text = "—"
		return v
	}
	attempts := ""
	if cell.Attempts > 1 {
		attempts = strconv.Itoa(cell.Attempts) + " 次"
	}
	if !showsClock {
		// OI 的格子只有一个数字：不比用时，也不给尝试次数。
		v.Text = strconv.Itoa(cell.Points)
		v.Class = scoreClass(cell.Points, cell.Max)
		v.Title = cell.Title + " · 本题满分 " + strconv.Itoa(cell.Max)
		return v
	}
	if mode == store.ContestACM {
		// 一题只认首次 AC：数字是尝试次数，绿色即已解出，时间放下面。
		v.Text, v.Class = strconv.Itoa(cell.Attempts), "sc0"
		v.Title = cell.Title + " · 未解出"
		if cell.Solved {
			v.Class = "sc9"
			v.Title = cell.Title + " · 已解出"
			v.When = formatClock(cell.TimeMS)
		}
		return v
	}
	v.Text = strconv.Itoa(cell.Points)
	v.Class = scoreClass(cell.Points, cell.Max)
	v.Title = cell.Title + " · 本题满分 " + strconv.Itoa(cell.Max)
	v.When = formatClock(cell.TimeMS)
	if attempts != "" {
		v.When += " · " + attempts
	}
	return v
}
