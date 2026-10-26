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

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 工单：普通用户提交，持 ticket.handle 的人处理。
//
// 与帖子的根本区别是私密。/topics 谁都能读，/tickets 只有作者和处理人读得到——
// 申诉里会写"我认为标准答案错了"，举报里会写别人的用户名，这些都不该出现在公开页上。
// 所以陌生路径一律 404 而不是 403：存在本身就是信息。

const (
	ticketWindow     = time.Hour
	maxTicketsHour   = 5
	maxTicketReplyHR = 30
)

// ticketRow is one line in a list. The list is where a handler triages, so the row
// carries everything that decision needs: kind, status, who it is waiting on, and
// how much thread is behind it.
type ticketRow struct {
	ID          int64
	Kind        string
	KindLabel   string
	Title       string
	Author      string
	DisplayName string
	// Color 是这个名字的活跃度档位类名（见 feed.go 里同名的理由）。
	Color       string
	When        string
	Updated     string
	Status      string
	StatusLabel string
	StatusClass string
	Handler     string
	Replies     int
	HasProblem  bool
	ProblemPID  string
	HasSubmit   bool
	Submission  int64
}

func ticketRows(items []store.Ticket) []ticketRow {
	rows := make([]ticketRow, 0, len(items))
	for _, t := range items {
		rows = append(rows, ticketRow{
			ID: t.ID, Kind: t.Kind, KindLabel: store.TicketKindLabel(t.Kind),
			Title: t.Title, Author: t.Author, DisplayName: ticketName(t.DisplayName, t.Author),
			Color: t.Color, When: agoMS(t.CreatedAt), Updated: agoMS(t.UpdatedAt),
			Status: t.Status, StatusLabel: store.TicketStatusLabel(t.Status),
			StatusClass: store.TicketStatusClass(t.Status),
			Handler:     t.Handler, Replies: t.Replies,
			HasProblem: t.ProblemID != 0, ProblemPID: t.ProblemPID,
			HasSubmit: t.SubmissionID != 0, Submission: t.SubmissionID,
		})
	}
	return rows
}

// ticketScope is whose list the reader asked for. Anything a normal user cannot
// ask for is folded back to their own: the SQL filter is chosen here, once.
// nextTicketCursor only points at an older page when this one came back full.
func nextTicketCursor(items []store.Ticket, limit int) int64 {
	if len(items) < limit || len(items) == 0 {
		return 0
	}
	return items[len(items)-1].ID
}

func ticketScope(raw string, handler bool) string {
	switch raw {
	case "queue":
		if handler {
			return "queue"
		}
	case "all":
		if handler {
			return "all"
		}
	}
	return "mine"
}

type ticketListData struct {
	Rows      []ticketRow
	Title     string
	Scope     string
	Status    string
	Query     string
	Next      int64
	Limit     int
	Handler   bool
	OpenCount int
	EmptyText string
}

func (s *Server) handleTicketList(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next=/tickets", http.StatusSeeOther)
		return
	}
	handler := a.can(permTicketHandle)
	q := r.URL.Query()
	scope := ticketScope(q.Get("scope"), handler)
	status := strings.TrimSpace(q.Get("status"))
	if status != "" && !store.ValidTicketStatus(status) {
		status = ""
	}
	limit := topicPageLimit(q.Get("limit")) // 一页多少条与帖子同一套上下限
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	f := store.TicketFilter{
		Status: status, Search: strings.TrimSpace(q.Get("q")), After: after, Limit: limit,
	}
	switch scope {
	case "queue":
		f.OpenOnly = true
	case "all":
	default:
		f.AuthorID = a.User.ID
	}
	items, err := s.db.ListTickets(r.Context(), f)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	titles := map[string]string{"mine": "我的工单", "queue": "待处理工单", "all": "全部工单"}
	empties := map[string]string{
		"mine": "还没有工单。判题觉得判错了、站点有毛病、要举报谁，都可以提一张。",
	}
	empty := empties["mine"]
	if scope != "mine" {
		empty = "这一类下没有工单。"
	}
	open, err := s.db.CountOpenTickets(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, titles[scope], "tickets", a.viewer(), msgOf(r), ticketListData{
		Rows: ticketRows(items), Title: titles[scope], Scope: scope, Status: status,
		Query: q.Get("q"), Next: nextTicketCursor(items, limit), Limit: limit,
		Handler: handler, OpenCount: open, EmptyText: empty,
	})
}

// ticketFormData is the payload of the submit form. actorID and ProblemID are
// decided here, not in the template: what the reader typed is not evidence that the
// problem or the submission is theirs.
type ticketFormData struct {
	Kind, Title, Body, Problem string
	Submission                 int64
	ProblemID                  int64
	Problems                   []bankEntry
	Errors                     []string
	Preview                    template.HTML
}

func (s *Server) handleTicketNew(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next=/tickets/new", http.StatusSeeOther)
		return
	}
	d, err := s.ticketFormFor(r, ticketFormData{Kind: store.TicketAppeal})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "提交工单", "ticket_edit", a.viewer(), msgOf(r), d)
}

// ticketFormFor fills the problem picker. A form that comes back after a rejected
// post keeps what was typed; only the empty choices get defaults.
func (s *Server) ticketFormFor(r *http.Request, d ticketFormData) (ticketFormData, error) {
	bank, err := s.db.ListProblems(r.Context(), 0, 500, "", !s.canSeeHiddenProblems(r))
	if err != nil {
		return d, err
	}
	for _, item := range bank.Items {
		d.Problems = append(d.Problems, bankEntry{Key: item.PID, Label: item.PID + "　" + item.Title})
	}
	return d, nil
}

// parseTicketForm reads the form and reports every problem in it, so a rejected
// ticket can be redrawn with what was typed instead of losing it.
func (s *Server) parseTicketForm(r *http.Request, actorID int64) (ticketFormData, []string) {
	var errs []string
	d := ticketFormData{
		Kind:    r.FormValue("kind"),
		Title:   strings.TrimSpace(r.FormValue("title")),
		Body:    r.FormValue("body"),
		Problem: strings.TrimSpace(r.FormValue("problem")),
	}
	if !store.ValidTicketKind(d.Kind) {
		errs = append(errs, "工单类型不认识")
	}
	if d.Title == "" {
		errs = append(errs, "标题不能是空的")
	} else if tooLong(d.Title, store.MaxTicketTitle) {
		errs = append(errs, fmt.Sprintf("标题最长 %d 个字符", store.MaxTicketTitle))
	}
	if tooLong(d.Body, store.MaxTicketBody) {
		errs = append(errs, fmt.Sprintf("正文最长 %d 个字符", store.MaxTicketBody))
	}
	if raw := strings.TrimSpace(r.FormValue("submission")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			errs = append(errs, "提交编号要是数字")
		} else if sub, err := s.db.GetSubmission(r.Context(), id); err != nil || sub.UserID != actorID {
			// 只能挂自己的提交：别人的提交编号本身就是不该被读到的东西，
			// 报"找不到"也比报"那是别人的"诚实。
			errs = append(errs, "找不到提交 "+raw+"，或者那不是你的提交")
		} else {
			d.Submission = id
			if d.Problem == "" {
				d.ProblemID = sub.ProblemID
			}
		}
	}
	if d.Problem != "" {
		p, err := s.db.FindProblem(r.Context(), d.Problem)
		if err != nil {
			errs = append(errs, "找不到题目 "+d.Problem)
		} else {
			d.ProblemID = p.ID
		}
	}
	return d, errs
}

func (s *Server) handleTicketCreate(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next=/tickets/new", http.StatusSeeOther)
		return
	}
	// The actor goes in as an argument, not as a form field: "is this submission
	// mine" is a question only the session can answer.
	d, errs := s.parseTicketForm(r, a.User.ID)
	if r.FormValue("preview") == "1" {
		d.Preview = renderMarkdownHTML(d.Body)
		d, _ = s.ticketFormFor(r, d)
		s.page(w, r, http.StatusOK, "提交工单", "ticket_edit", a.viewer(), "", d)
		return
	}
	if len(errs) > 0 {
		d.Errors = errs
		d, _ = s.ticketFormFor(r, d)
		s.page(w, r, http.StatusBadRequest, "提交工单", "ticket_edit", a.viewer(), "", d)
		return
	}
	key := fmt.Sprintf("ticket:%d", a.User.ID)
	allowed, err := s.db.LoginAllowed(r.Context(), key, ticketWindow, maxTicketsHour)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !allowed {
		d.Errors = []string{fmt.Sprintf("提得太快了，一小时最多 %d 张工单", maxTicketsHour)}
		d, _ = s.ticketFormFor(r, d)
		s.page(w, r, http.StatusTooManyRequests, "提交工单", "ticket_edit", a.viewer(), "", d)
		return
	}
	tk, err := s.db.CreateTicket(r.Context(), &store.Ticket{
		Kind: d.Kind, Title: d.Title, Body: d.Body, AuthorID: a.User.ID,
		ProblemID: d.ProblemID, SubmissionID: d.Submission,
	})
	if err != nil {
		d.Errors = []string{err.Error()}
		d, _ = s.ticketFormFor(r, d)
		s.page(w, r, http.StatusBadRequest, "提交工单", "ticket_edit", a.viewer(), "", d)
		return
	}
	if _, err := s.db.RegisterLoginFailure(r.Context(), key, ticketWindow); err != nil {
		s.log.Error("ticket counter", "err", err)
	}
	s.log.Info("ticket filed", "id", tk.ID, "kind", tk.Kind, "by", a.User.Handle)
	http.Redirect(w, r, "/tickets/"+strconv.FormatInt(tk.ID, 10)+"?msg="+url.QueryEscape("已提交，等处理"), http.StatusSeeOther)
}

type ticketReplyRow struct {
	ID          int64
	Author      string
	DisplayName string
	Color       string
	When        string
	Body        template.HTML
	// Event 非空时这一行是系统流水，排版上不该长得像谁说的话。
	Event   bool
	IsYou   bool
	IsStaff bool
}

type ticketData struct {
	ID          int64
	Kind        string
	KindLabel   string
	Title       string
	Author      string
	DisplayName string
	Color       string
	When        string
	Updated     string
	Body        template.HTML
	Status      string
	StatusLabel string
	StatusClass string
	Handler     string
	HasProblem  bool
	ProblemPID  string
	ProblemName string
	HasSubmit   bool
	Submission  int64
	Replies     []ticketReplyRow
	// 谁能做什么，一次算清楚交给模板：作者能关自己的单、能追问；处理人能改状态、能删单。
	IsAuthor   bool
	IsHandler  bool
	CanClose   bool
	CanDelete  bool
	StatusOpts []detailOption
	SignedIn   bool
	Back       string
}

func (s *Server) ticketByID(w http.ResponseWriter, r *http.Request) (actor, store.Ticket, bool) {
	a, err := s.authenticate(r)
	if err != nil {
		s.writeAuthError(w, r, err)
		return a, store.Ticket{}, false
	}
	if a.User.ID == 0 {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
		return a, store.Ticket{}, false
	}
	id, ok := pathID(r)
	if !ok {
		s.respondError(w, r, http.StatusBadRequest, "工单编号得是数字")
		return a, store.Ticket{}, false
	}
	tk, err := s.db.FindTicket(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		// 陌生人不给 403：工单的存在本身就是不该泄漏的东西。
		s.respondError(w, r, http.StatusNotFound, "没有这张工单")
		return a, store.Ticket{}, false
	}
	if err != nil {
		s.writeStoreError(w, r, err)
		return a, store.Ticket{}, false
	}
	if tk.AuthorID != a.User.ID && !a.can(permTicketHandle) {
		s.respondError(w, r, http.StatusNotFound, "没有这张工单")
		return a, store.Ticket{}, false
	}
	return a, tk, true
}

func (s *Server) handleTicketGet(w http.ResponseWriter, r *http.Request) {
	a, tk, ok := s.ticketByID(w, r)
	if !ok {
		return
	}
	handler := a.can(permTicketHandle)
	replies, err := s.db.ListTicketReplies(r.Context(), tk.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	d := ticketData{
		ID: tk.ID, Kind: tk.Kind, KindLabel: store.TicketKindLabel(tk.Kind),
		Title: tk.Title, Author: tk.Author, DisplayName: ticketName(tk.DisplayName, tk.Author),
		Color: tk.Color, When: agoMS(tk.CreatedAt), Updated: agoMS(tk.UpdatedAt),
		Body:   renderMarkdownHTML(tk.Body),
		Status: tk.Status, StatusLabel: store.TicketStatusLabel(tk.Status),
		StatusClass: store.TicketStatusClass(tk.Status), Handler: tk.Handler,
		HasProblem: tk.ProblemID != 0, ProblemPID: tk.ProblemPID, ProblemName: tk.ProblemTitle,
		HasSubmit: tk.SubmissionID != 0, Submission: tk.SubmissionID,
		IsAuthor: tk.AuthorID == a.User.ID, IsHandler: handler,
		CanDelete: handler,
		SignedIn:  true, Back: "/tickets",
	}
	// 作者能关自己的单（提错了、自己解决了），也能追问； reopen 留给处理人。
	if d.IsAuthor && tk.Status != store.TicketClosed && tk.Status != store.TicketDone {
		d.CanClose = true
	}
	if handler {
		for _, st := range []string{store.TicketOpen, store.TicketDoing, store.TicketDone, store.TicketClosed} {
			d.StatusOpts = append(d.StatusOpts, detailOption{Value: st, Label: store.TicketStatusLabel(st)})
		}
	} else if d.CanClose {
		d.StatusOpts = append(d.StatusOpts, detailOption{Value: store.TicketClosed, Label: "关闭这张工单"})
	}
	for _, rep := range replies {
		d.Replies = append(d.Replies, ticketReplyRow{
			ID: rep.ID, Author: rep.Author, DisplayName: ticketName(rep.DisplayName, rep.Author),
			Color: rep.Color, When: agoMS(rep.CreatedAt), Body: renderMarkdownHTML(rep.Body),
			Event: rep.Event != "",
			IsYou: rep.AuthorID == a.User.ID,
			// 「处理人」只给真正接手这单的人：有权限不等于在办。
			IsStaff: rep.AuthorID == tk.HandlerID,
		})
	}
	s.page(w, r, http.StatusOK, "工单 "+strconv.FormatInt(tk.ID, 10), "ticket", a.viewer(), msgOf(r), d)
}

func (s *Server) handleTicketReply(w http.ResponseWriter, r *http.Request) {
	a, tk, ok := s.ticketByID(w, r)
	if !ok {
		return
	}
	back := "/tickets/" + strconv.FormatInt(tk.ID, 10)
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		s.backToTicket(w, r, back, "回复不能是空的")
		return
	}
	if tooLong(body, store.MaxTicketReply) {
		s.backToTicket(w, r, back, fmt.Sprintf("回复最长 %d 个字符", store.MaxTicketReply))
		return
	}
	key := fmt.Sprintf("ticketreply:%d", a.User.ID)
	allowed, err := s.db.LoginAllowed(r.Context(), key, ticketWindow, maxTicketReplyHR)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !allowed {
		s.backToTicket(w, r, back, fmt.Sprintf("回得太快了，一小时最多 %d 条", maxTicketReplyHR))
		return
	}
	if _, err := s.db.AddTicketReply(r.Context(), &store.TicketReply{
		TicketID: tk.ID, AuthorID: a.User.ID, Body: body,
	}); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if _, err := s.db.RegisterLoginFailure(r.Context(), key, ticketWindow); err != nil {
		s.log.Error("ticket reply counter", "err", err)
	}
	s.log.Info("ticket reply", "ticket", tk.ID, "by", a.User.Handle)
	s.backToTicket(w, r, back, "已回复")
}

// handleTicketStatus moves a ticket. A handler may move it anywhere and is recorded
// as the one handling it; the author gets exactly one move — closing their own —
// because "我自己解决了" is a thing that happens.
func (s *Server) handleTicketStatus(w http.ResponseWriter, r *http.Request) {
	a, tk, ok := s.ticketByID(w, r)
	if !ok {
		return
	}
	back := "/tickets/" + strconv.FormatInt(tk.ID, 10)
	status := strings.TrimSpace(r.FormValue("status"))
	note := strings.TrimSpace(r.FormValue("note"))
	handler := a.can(permTicketHandle)
	switch {
	case handler:
	case status == store.TicketClosed && tk.AuthorID == a.User.ID:
	default:
		s.respondError(w, r, http.StatusForbidden, "改状态需要 "+permTicketHandle)
		return
	}
	if !store.ValidTicketStatus(status) {
		s.backToTicket(w, r, back, "不认识的状态")
		return
	}
	handlerID := tk.HandlerID
	if handler && status != store.TicketClosed {
		handlerID = a.User.ID
	}
	if handler && status == store.TicketOpen {
		handlerID = 0 // 退回待处理队列，就别还挂着"有人在办"
	}
	if tooLong(note, store.MaxTicketReply) {
		s.backToTicket(w, r, back, fmt.Sprintf("备注最长 %d 个字符", store.MaxTicketReply))
		return
	}
	updated, err := s.db.SetTicketStatus(r.Context(), tk.ID, status, handlerID, note)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("ticket moved", "ticket", tk.ID, "to", status, "by", a.User.Handle)
	// 工单线程里已经有一行 event（那次改动本身），审计给的是横切视图："这个处理人
	// 动过哪些单"。备注是自由文本，入库前按审计的字数上限截断。
	moved := "状态改为「" + store.TicketStatusLabel(updated.Status) + "」"
	if note != "" {
		moved += "；备注：" + note
	}
	s.audit(r, a, "ticket.status", strconv.FormatInt(tk.ID, 10), moved)
	s.backToTicket(w, r, back, "已改为「"+store.TicketStatusLabel(updated.Status)+"」")
}

func (s *Server) handleTicketDelete(w http.ResponseWriter, r *http.Request) {
	a, tk, ok := s.ticketByID(w, r)
	if !ok {
		return
	}
	if !a.can(permTicketHandle) {
		s.respondError(w, r, http.StatusForbidden, "删工单需要 "+permTicketHandle)
		return
	}
	if _, err := s.db.DeleteTicket(r.Context(), tk.ID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	// 删掉之后工单与它的线程都不在了，这一行是"这张单存在过、标题是什么、谁删的"唯一
	// 还剩下来的东西，所以标题要写进来。
	s.audit(r, a, "ticket.delete", strconv.FormatInt(tk.ID, 10), "已删除工单：「"+tk.Title+"」")
	s.log.Info("ticket deleted", "id", tk.ID, "by", a.User.Handle)
	http.Redirect(w, r, "/tickets?msg="+url.QueryEscape("已删除工单"), http.StatusSeeOther)
}

// backToTicket 回到工单页并带一句话。303 而不是 302：POST 之后要让浏览器换成 GET，
// 否则刷新会再提交一次回复。
// ticketName falls back to the handle when the user never set a nickname: an empty
// link is unreadable and unclickable-looking.
func ticketName(display, handle string) string {
	if strings.TrimSpace(display) != "" {
		return display
	}
	return handle
}

func (s *Server) backToTicket(w http.ResponseWriter, r *http.Request, path, msg string) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	http.Redirect(w, r, path+sep+"msg="+url.QueryEscape(msg), http.StatusSeeOther)
}
