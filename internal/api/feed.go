package api

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// Feed posting limits: enough for a conversation, not enough to flood the home
// page from a script.
const (
	postWindow   = time.Hour
	maxPostsHour = 20
)

type postRow struct {
	ID          int64
	Author      string
	DisplayName string
	// Color 是这个名字的活跃度档位类名，直接拼进 class（store.colorSQL 已经把它收进那六个词）。
	Color       string
	When        string
	Body        template.HTML
	CanDelete   bool
	CanModerate bool
	Hidden      bool
}

// postRows renders posts and marks what the current viewer may do with each.
func postRows(posts []store.Post, a actor) []postRow {
	rows := make([]postRow, 0, len(posts))
	for _, p := range posts {
		name := p.Author
		if p.DisplayName != "" {
			name = p.DisplayName
		}
		rows = append(rows, postRow{
			ID: p.ID, Author: p.Author, DisplayName: name, Color: p.Color, When: agoMS(p.CreatedAt),
			Body:        renderMarkdownHTML(p.Body),
			CanDelete:   a.User.ID == p.AuthorID,
			CanModerate: a.can(permFeedModerate),
			Hidden:      !p.Visible,
		})
	}
	return rows
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	author := r.URL.Query().Get("user")
	limit := pageLimit(r.URL.Query().Get("limit"))
	// Clamp identically to ListPosts, otherwise "this page was full" would be
	// judged against a number the query never returned rows for.
	if limit > store.MaxPostPage {
		limit = store.MaxPostPage
	}

	var authorID int64
	if author != "" {
		u, err := s.db.GetUserByHandle(r.Context(), author)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		authorID = u.ID
	}
	// A moderator's timeline includes hidden entries, otherwise a hide could
	// never be undone from the UI.
	posts, err := s.db.ListPosts(r.Context(), limit, after, authorID, a.can(permFeedModerate))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "动态", "feed", a.viewer(), msgOf(r), map[string]any{
		"Posts":    postRows(posts, a),
		"SignedIn": a.User.ID != 0,
		"Banned":   s.feedNotice(r, a),
		"Next":     nextCursor(posts, limit),
	})
}

// nextCursor only points at an older page when this one came back full, so the
// timeline does not invite a click that lands on nothing.
func nextCursor(posts []store.Post, limit int) int64 {
	if len(posts) < limit {
		return 0
	}
	return posts[len(posts)-1].ID
}

func (s *Server) handlePostCreate(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil {
		http.Redirect(w, r, "/login?next=/feed", http.StatusSeeOther)
		return
	}
	if !s.site(r.Context()).AllowFeed {
		s.featureClosed(w, r, "动态发布")
		return
	}
	if msg, banned := a.bannedFrom(permFeedPost, "发动态"); banned {
		s.respondError(w, r, http.StatusForbidden, msg)
		return
	}
	key := fmt.Sprintf("post:%d", a.User.ID)
	allowed, err := s.db.LoginAllowed(r.Context(), key, postWindow, maxPostsHour)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !allowed {
		http.Redirect(w, r, "/feed?msg="+url.QueryEscape("发得太快了，一小时最多 "+strconv.Itoa(maxPostsHour)+" 条"), http.StatusSeeOther)
		return
	}

	post, err := s.db.CreatePost(r.Context(), a.User.ID, r.FormValue("body"))
	if err != nil {
		http.Redirect(w, r, "/feed?msg="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if _, err := s.db.RegisterLoginFailure(r.Context(), key, postWindow); err != nil {
		s.log.Error("post counter", "err", err)
	}
	s.log.Info("post created", "id", post.ID, "by", a.User.Handle)
	http.Redirect(w, r, "/feed?msg="+url.QueryEscape("已发布"), http.StatusSeeOther)
}

func (s *Server) handlePostDelete(w http.ResponseWriter, r *http.Request) {
	a, err := s.authenticate(r)
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	id, ok := pathID(r)
	if !ok {
		return
	}
	// A moderator may remove anyone's entry; everyone else only their own.
	owner := true
	if a.can(permFeedModerate) {
		owner = false
	}
	var removed bool
	if owner {
		removed, err = s.db.DeletePost(r.Context(), id, a.User.ID)
	} else {
		removed, err = s.db.DeletePostAny(r.Context(), id)
	}
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !removed {
		s.respondError(w, r, http.StatusNotFound, "没有这条动态，或者它不是你的")
		return
	}
	s.log.Info("post deleted", "id", id, "by", a.User.Handle)
	http.Redirect(w, r, "/feed?msg="+url.QueryEscape("已删除"), http.StatusSeeOther)
}

func (s *Server) handlePostHide(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permFeedModerate)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		return
	}
	hide := r.FormValue("hidden") != "0"
	changed, err := s.db.SetPostHidden(r.Context(), id, a.User.ID, hide)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !changed {
		s.respondError(w, r, http.StatusConflict, "这条动态已经是那个状态了")
		return
	}
	s.log.Info("post moderated", "id", id, "hidden", hide, "by", a.User.Handle)
	http.Redirect(w, r, "/feed?msg="+url.QueryEscape("已处理"), http.StatusSeeOther)
}

// pathID reads the {id} of the route, without writing anything: the caller decides
// what a nonsense id means for its own page.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
