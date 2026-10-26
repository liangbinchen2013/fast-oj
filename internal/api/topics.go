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

// Topics are a web surface only, like the rest of the human-facing app: a person
// writes a post in a browser and reads replies in a browser.

// Posting limits: enough to ask a question and follow it up, not enough to paper
// over the home page from a script.
const (
	topicWindow     = time.Hour
	maxTopicsHour   = 10
	maxCommentsHour = 60
	// homeTopics is how many posts the front page shows, two per row.
	homeTopics = 10
)

// topicRow is one line in a list of posts.
type topicRow struct {
	ID          int64
	Kind        string
	KindLabel   string
	Title       string
	Author      string
	DisplayName string
	// Color 是这个名字的活跃度档位类名（见 feed.go 里同名的理由）。
	Color     string
	When      string
	Comments  int
	HasProblem  bool
	ProblemPID  string
	ProblemName string
	// CanDelete is per row, not per page: the author may delete their own post and
	// a moderator may delete anyone's, and the list is where a moderator actually
	// browses to clean up.
	CanDelete bool
}

func topicRows(items []store.Topic, a actor) []topicRow {
	rows := make([]topicRow, 0, len(items))
	for _, t := range items {
		name := t.Author
		if t.DisplayName != "" {
			name = t.DisplayName
		}
		rows = append(rows, topicRow{
			ID: t.ID, Kind: t.Kind, KindLabel: store.TopicKindLabel(t.Kind),
			Title: t.Title, Author: t.Author, DisplayName: name, Color: t.Color,
			When: agoMS(t.CreatedAt), Comments: t.Comments,
			HasProblem: t.ProblemID != 0, ProblemPID: t.ProblemPID, ProblemName: t.ProblemTitle,
			CanDelete: a.User.ID != 0 && (a.User.ID == t.AuthorID || a.can(permTopicModerate)),
		})
	}
	return rows
}

// topicListData is the payload of both list pages: /topics for 社区贴 and
// /problems/{key}/topics for one problem's 求助贴.
type topicListData struct {
	Rows        []topicRow
	Title       string
	SignedIn    bool
	Next        int64
	Limit       int
	Query       string
	Problem     string
	ProblemName string
	NewTopic    string
	EmptyText   string
}

// handleTopicList is the page that manages 社区贴. 求助贴 are read from their
// problem, so this list shows the community kind and nothing else.
func (s *Server) handleTopicList(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit := topicPageLimit(q.Get("limit"))
	search := strings.TrimSpace(q.Get("q"))
	items, err := s.db.ListTopics(r.Context(), store.TopicFilter{
		Kind: store.TopicCommunity, Search: search, After: after, Limit: limit,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "社区贴", "topics", a.viewer(), msgOf(r), topicListData{
		Rows: topicRows(items, a), Title: "社区贴", SignedIn: a.User.ID != 0,
		Next: nextTopicCursor(items, limit), Limit: limit, Query: search,
		NewTopic:  "/topics/new",
		EmptyText: "还没有社区贴。",
	})
}

// handleProblemTopics lists the 求助贴 of one problem, which is how a question
// reaches the people who can answer it.
func (s *Server) handleProblemTopics(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	p, ok := s.problemByKey(w, r)
	if !ok {
		return
	}
	if !p.Visible && !a.canEditProblem(p) && !a.can(permProblemPublish) {
		s.respondError(w, r, http.StatusNotFound, "problem "+p.PID+" not found")
		return
	}
	limit := topicPageLimit(r.URL.Query().Get("limit"))
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	items, err := s.db.ListTopics(r.Context(), store.TopicFilter{
		ProblemID: p.ID, After: after, Limit: limit,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "题目帖子 "+p.PID, "topics", a.viewer(), msgOf(r), topicListData{
		Rows: topicRows(items, a), Title: "关于 " + p.PID + " 的求助", SignedIn: a.User.ID != 0,
		Next: nextTopicCursor(items, limit), Limit: limit,
		Problem: p.PID, ProblemName: p.Title,
		NewTopic:  "/topics/new?problem=" + url.QueryEscape(p.PID),
		EmptyText: "这道题还没有人提问。",
	})
}

func topicPageLimit(raw string) int {
	n, _ := strconv.Atoi(raw)
	switch {
	case n <= 0:
		return store.DefaultTopicPage
	case n > store.MaxTopicPage:
		return store.MaxTopicPage
	}
	return n
}

// nextTopicCursor only points at an older page when this one came back full, so the
// footer does not invite a click that lands on nothing.
func nextTopicCursor(items []store.Topic, limit int) int64 {
	if len(items) < limit || len(items) == 0 {
		return 0
	}
	return items[len(items)-1].ID
}

// topicForm is what the browser posts.
type topicForm struct {
	Kind, Title, Body string
	ProblemKey        string
	ProblemID         int64
	Errors            []string
}

// parseTopicForm reads the form and reports every problem in it, so a rejected post
// can be redrawn with what was typed.
func (s *Server) parseTopicForm(r *http.Request) topicForm {
	f := topicForm{
		Kind:       r.FormValue("kind"),
		Title:      strings.TrimSpace(r.FormValue("title")),
		Body:       r.FormValue("body"),
		ProblemKey: strings.TrimSpace(r.FormValue("problem")),
	}
	if !store.ValidTopicKind(f.Kind) {
		f.Errors = append(f.Errors, "帖子类型只认求助或社区")
	}
	if f.Title == "" {
		f.Errors = append(f.Errors, "标题不能是空的")
	} else if tooLong(f.Title, store.MaxTopicTitle) {
		f.Errors = append(f.Errors, fmt.Sprintf("标题最长 %d 个字符", store.MaxTopicTitle))
	}
	if tooLong(f.Body, store.MaxTopicBody) {
		f.Errors = append(f.Errors, fmt.Sprintf("正文最长 %d 个字符", store.MaxTopicBody))
	}
	if f.Kind == store.TopicHelp {
		if f.ProblemKey == "" {
			f.Errors = append(f.Errors, "求助贴要写清是哪道题")
		} else {
			p, err := s.db.FindProblem(r.Context(), f.ProblemKey)
			if err != nil {
				f.Errors = append(f.Errors, "找不到题目 "+f.ProblemKey)
			} else {
				f.ProblemID = p.ID
			}
		}
	}
	return f
}

// topicFormData is the payload of the post form.
type topicFormData struct {
	Kind, Title, Body, Problem string
	Problems                   []bankEntry
	Errors                     []string
	Preview                    template.HTML
	// LockedProblem is set when the form came from a problem page: the kind is then
	// fixed to 求助 and the reader does not get to un-pick the problem.
	LockedProblem bool
	Action        string
	// Banned is the reason this reader may not post at all; empty means the form
	// is usable. Like the feed composer, the page says so instead of letting the
	// submit button walk into a 403.
	Banned string
}

func newTopicFormData(p store.Problem, locked bool) topicFormData {
	d := topicFormData{Action: "/topics", Kind: store.TopicCommunity}
	if locked {
		d.Kind, d.Problem, d.LockedProblem = store.TopicHelp, p.PID, true
	}
	return d
}

func (d *topicFormData) fillFromForm(f topicForm) {
	d.Kind, d.Title, d.Body, d.Problem = f.Kind, f.Title, f.Body, f.ProblemKey
	d.Errors = append(d.Errors, f.Errors...)
}

func (s *Server) handleTopicNew(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	d, err := s.topicFormFor(r, r.URL.Query().Get("problem"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	d.Banned = s.topicNotice(r, a)
	s.page(w, r, http.StatusOK, "发帖", "topic_edit", a.viewer(), msgOf(r), d)
}

// topicFormFor builds an empty form, preset to a 求助贴 when the reader came from a
// problem page. A problem key that no longer resolves answers "no preset" rather than
// failing the page: the reader can still write a 社区贴.
func (s *Server) topicFormFor(r *http.Request, problemKey string) (topicFormData, error) {
	d := topicFormData{Action: "/topics", Kind: store.TopicCommunity}
	if problemKey != "" {
		if p, err := s.db.FindProblem(r.Context(), problemKey); err == nil {
			d.Kind, d.Problem, d.LockedProblem = store.TopicHelp, p.PID, true
		}
	}
	bank, err := s.db.ListProblems(r.Context(), 0, 500, "", !s.canSeeHiddenProblems(r))
	if err != nil {
		return d, err
	}
	for _, item := range bank.Items {
		d.Problems = append(d.Problems, bankEntry{Key: item.PID, Label: item.PID + "　" + item.Title})
	}
	return d, nil
}

// canSeeHiddenProblems is the same question the problem list asks: anyone who may
// edit anybody's problem also gets the unpublished ones offered in the picker.
func (s *Server) canSeeHiddenProblems(r *http.Request) bool {
	a, _ := s.authenticate(r)
	return a.can(permProblemEditAny)
}

func (s *Server) handleTopicCreate(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next=/topics/new", http.StatusSeeOther)
		return
	}
	if msg := s.topicNotice(r, a); msg != "" {
		s.respondError(w, r, http.StatusForbidden, msg)
		return
	}
	f := s.parseTopicForm(r)
	// The form is redrawn from what was posted, so an unresolvable problem key stays
	// on the page as the error parseTopicForm reported instead of failing here.
	d := topicFormData{Action: "/topics", Kind: f.Kind, LockedProblem: r.FormValue("locked") == "1"}
	bank, err := s.db.ListProblems(r.Context(), 0, 500, "", !a.can(permProblemEditAny))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	for _, item := range bank.Items {
		d.Problems = append(d.Problems, bankEntry{Key: item.PID, Label: item.PID + "　" + item.Title})
	}
	d.fillFromForm(f)
	if r.FormValue("preview") == "1" {
		d.Preview = renderMarkdownHTML(f.Body)
		s.page(w, r, http.StatusOK, "发帖", "topic_edit", a.viewer(), "", d)
		return
	}
	if len(d.Errors) > 0 {
		s.page(w, r, http.StatusBadRequest, "发帖", "topic_edit", a.viewer(), "", d)
		return
	}
	key := fmt.Sprintf("topic:%d", a.User.ID)
	allowed, err := s.db.LoginAllowed(r.Context(), key, topicWindow, maxTopicsHour)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !allowed {
		d.Errors = []string{fmt.Sprintf("发得太快了，一小时最多 %d 帖", maxTopicsHour)}
		s.page(w, r, http.StatusTooManyRequests, "发帖", "topic_edit", a.viewer(), "", d)
		return
	}
	topic, err := s.db.CreateTopic(r.Context(), f.Kind, f.Title, f.Body, a.User.ID, f.ProblemID)
	if err != nil {
		d.Errors = []string{err.Error()}
		s.page(w, r, http.StatusBadRequest, "发帖", "topic_edit", a.viewer(), "", d)
		return
	}
	if _, err := s.db.RegisterLoginFailure(r.Context(), key, topicWindow); err != nil {
		s.log.Error("topic counter", "err", err)
	}
	s.log.Info("topic created", "id", topic.ID, "kind", topic.Kind, "by", a.User.Handle)
	http.Redirect(w, r, "/topics/"+strconv.FormatInt(topic.ID, 10)+"?msg="+url.QueryEscape("已发布"), http.StatusSeeOther)
}

// topicData is one post with what a reader may do with it.
type topicData struct {
	ID          int64
	Kind        string
	KindLabel   string
	Title       string
	Author      string
	DisplayName string
	// Color 是名字的活跃度档位类名。
	Color       string
	When        string
	Body        template.HTML
	HasProblem  bool
	ProblemPID  string
	ProblemName string
	CanDelete   bool
	BackLink    string
	BackText    string
	// Comments are read oldest-first, and the count is what the list pages print.
	Comments   []commentRow
	CommentNum int
	SignedIn   bool
}

func (s *Server) handleTopicGet(w http.ResponseWriter, r *http.Request) {
	a, topic, ok := s.topicByID(w, r)
	if !ok {
		return
	}
	d := topicData{
		ID: topic.ID, Kind: topic.Kind, KindLabel: store.TopicKindLabel(topic.Kind),
		Title: topic.Title, Author: topic.Author, Color: topic.Color, When: agoMS(topic.CreatedAt),
		Body:       renderMarkdownHTML(topic.Body),
		HasProblem: topic.ProblemID != 0, ProblemPID: topic.ProblemPID, ProblemName: topic.ProblemTitle,
		CanDelete: a.User.ID != 0 && (a.User.ID == topic.AuthorID || a.can(permTopicModerate)),
	}
	if topic.DisplayName != "" {
		d.DisplayName = topic.DisplayName
	} else {
		d.DisplayName = topic.Author
	}
	if topic.ProblemID != 0 {
		d.BackLink = "/problems/" + url.PathEscape(topic.ProblemPID) + "/topics"
		d.BackText = "这道题的全部求助"
	} else {
		d.BackLink = "/topics"
		d.BackText = "全部社区贴"
	}
	comments, err := s.db.ListTopicComments(r.Context(), topic.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	d.Comments = commentRows(comments, a)
	d.CommentNum = len(comments)
	d.SignedIn = a.User.ID != 0
	s.page(w, r, http.StatusOK, topic.Title, "topic", a.viewer(), msgOf(r), d)
}

// topicByID resolves /topics/{id} for both the page and the comment form, so the two
// cannot disagree about what "the same post" means.
func (s *Server) topicByID(w http.ResponseWriter, r *http.Request) (actor, store.Topic, bool) {
	a, _ := s.authenticate(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.respondError(w, r, http.StatusBadRequest, "帖子编号要是正整数")
		return a, store.Topic{}, false
	}
	topic, err := s.db.GetTopic(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return a, topic, false
	}
	return a, topic, true
}

func (s *Server) handleTopicDelete(w http.ResponseWriter, r *http.Request) {
	a, topic, ok := s.topicByID(w, r)
	if !ok {
		return
	}
	// Everyone else needs the moderation right; the author only needs to be themself.
	moderating := a.User.ID != topic.AuthorID
	if moderating && !a.can(permTopicModerate) {
		s.respondError(w, r, http.StatusForbidden, "这个帖子由他人创建，需要 "+permTopicModerate+" 才能删除")
		return
	}
	var removed bool
	var err error
	if moderating {
		removed, err = s.db.DeleteTopicAny(r.Context(), topic.ID)
	} else {
		removed, err = s.db.DeleteTopic(r.Context(), topic.ID, a.User.ID)
	}
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !removed {
		s.respondError(w, r, http.StatusNotFound, "没有这个帖子，或者它已经不是你的了")
		return
	}
	s.log.Info("topic deleted", "id", topic.ID, "by", a.User.Handle, "moderated", moderating)
	back := "/topics"
	if topic.ProblemID != 0 {
		back = "/problems/" + url.PathEscape(topic.ProblemPID) + "/topics"
	}
	http.Redirect(w, r, back+"?msg="+url.QueryEscape("已删除帖子"), http.StatusSeeOther)
}

// --- comments ---

// commentRow is one reply as the page shows it.
type commentRow struct {
	ID          int64
	Author      string
	DisplayName string
	Color       string
	When        string
	Body        template.HTML
	CanDelete   bool
}

func commentRows(items []store.TopicComment, a actor) []commentRow {
	rows := make([]commentRow, 0, len(items))
	for _, c := range items {
		name := c.Author
		if c.DisplayName != "" {
			name = c.DisplayName
		}
		rows = append(rows, commentRow{
			ID: c.ID, Author: c.Author, DisplayName: name, Color: c.Color, When: agoMS(c.CreatedAt),
			Body: renderMarkdownHTML(c.Body),
			// The author may always take their own words back; anyone else needs the
			// moderation right.
			CanDelete: a.User.ID != 0 && (a.User.ID == c.AuthorID || a.can(permTopicModerate)),
		})
	}
	return rows
}

// handleTopicComment is the write half of one thread. The post itself is re-read
// rather than trusted from the form, so a comment cannot be attached to a topic that
// was deleted while the reader was typing.
func (s *Server) handleTopicComment(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/topics/"+r.PathValue("id")), http.StatusSeeOther)
		return
	}
	// 站级开关关掉帖子时，评论也一并停：评论是帖子的一部分，只关发帖的话，
	// 老帖子下面还能继续盖楼。
	if !s.site(r.Context()).AllowTopics {
		s.featureClosed(w, r, "评论")
		return
	}
	id, ok := pathID(r)
	if !ok {
		return
	}
	key := fmt.Sprintf("topiccomment:%d", a.User.ID)
	allowed, err := s.db.LoginAllowed(r.Context(), key, topicWindow, maxCommentsHour)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !allowed {
		http.Redirect(w, r, fmt.Sprintf("/topics/%d?msg=%s", id,
			url.QueryEscape("评论得太快了，一小时最多 "+strconv.Itoa(maxCommentsHour)+" 条")), http.StatusSeeOther)
		return
	}
	comment, err := s.db.CreateTopicComment(r.Context(), id, a.User.ID, r.FormValue("body"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, "/topics?msg="+url.QueryEscape("这个帖子已经不在了，评论没处可去"), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/topics/%d?msg=%s", id, url.QueryEscape(err.Error())), http.StatusSeeOther)
		return
	}
	if _, err := s.db.RegisterLoginFailure(r.Context(), key, topicWindow); err != nil {
		s.log.Error("comment counter", "err", err)
	}
	s.log.Info("topic comment", "id", comment.ID, "topic", comment.TopicID, "by", a.User.Handle)
	http.Redirect(w, r, fmt.Sprintf("/topics/%d?msg=%s", id, url.QueryEscape("已发布评论")), http.StatusSeeOther)
}

// handleTopicCommentDelete removes one reply and returns the reader to its thread.
func (s *Server) handleTopicCommentDelete(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		http.Redirect(w, r, "/login?next=/topics", http.StatusSeeOther)
		return
	}
	id, ok := pathID(r)
	if !ok {
		return
	}
	comment, err := s.db.GetTopicComment(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	moderating := a.User.ID != comment.AuthorID
	if moderating && !a.can(permTopicModerate) {
		s.respondError(w, r, http.StatusForbidden, "这条评论由他人写的，需要 "+permTopicModerate+" 才能删除")
		return
	}
	var removed bool
	if moderating {
		removed, err = s.db.DeleteTopicCommentAny(r.Context(), id)
	} else {
		removed, err = s.db.DeleteTopicComment(r.Context(), id, a.User.ID)
	}
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !removed {
		s.respondError(w, r, http.StatusNotFound, "没有这条评论，或者它已经不是你的了")
		return
	}
	// The comment's parent cannot have vanished: the foreign key would have taken the
	// comment with it.
	back := fmt.Sprintf("/topics/%d", comment.TopicID)
	s.log.Info("topic comment deleted", "id", id, "topic", comment.TopicID, "by", a.User.Handle)
	http.Redirect(w, r, back+"?msg="+url.QueryEscape("已删除评论"), http.StatusSeeOther)
}
