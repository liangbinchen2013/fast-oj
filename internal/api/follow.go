package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// nameOf is how a person is addressed: the nickname when they set one, the handle
// otherwise. 管理员贴的标签接在后面——这些列表拿到的是整行 User，走不了 SQL 那一路的
// personSQL，所以两边各自接一次，格式由 User.TagSuffix 统一。
func nameOf(u store.User) string {
	base := u.Handle
	if strings.TrimSpace(u.DisplayName) != "" {
		base = u.DisplayName
	}
	return base + u.TagSuffix()
}

// followHandler binds one of the two verbs to a route, so the form and a scripted
// client drive the same code path with no field to spoof.
func (s *Server) followHandler(follow bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleFollow(w, r, follow) }
}

// handleFollow answers with a redirect back to the profile, like every other form
// action here: the browser lands where it started with the result in the flash.
func (s *Server) handleFollow(w http.ResponseWriter, r *http.Request, follow bool) {
	target := r.PathValue("handle")
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		// Back to the profile they were looking at, not to the settings page.
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/u/"+target), http.StatusSeeOther)
		return
	}
	u, err := s.db.GetUserByHandle(r.Context(), target)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	back := "/u/" + url.PathEscape(u.Handle)

	note := "已关注 " + nameOf(u)
	if follow {
		// The store refuses self-follows and treats a repeat as success, so neither
		// needs its own branch here.
		if err := s.db.Follow(r.Context(), a.User.ID, u.ID); err != nil {
			if errors.Is(err, store.ErrSelfFollow) {
				// A profile page is the friendliest place to learn this, and the
				// store is the one that knows whether the target is self.
				http.Redirect(w, r, back+"?msg="+url.QueryEscape("不能关注自己"), http.StatusSeeOther)
				return
			}
			s.writeStoreError(w, r, err)
			return
		}
	} else {
		removed, err := s.db.Unfollow(r.Context(), a.User.ID, u.ID)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		if !removed {
			note = "你本来就没有关注 " + nameOf(u)
		} else {
			note = "已取消关注 " + nameOf(u)
		}
	}
	s.log.Info("follow change", "by", a.User.Handle, "target", u.Handle, "follow", follow)
	http.Redirect(w, r, back+"?msg="+url.QueryEscape(note), http.StatusSeeOther)
}

// followListHandler binds a direction to its route the same way the toggle does.
func (s *Server) followListHandler(direction string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleFollowList(w, r, direction) }
}

func (s *Server) handleFollowList(w http.ResponseWriter, r *http.Request, direction string) {
	ctx := r.Context()
	u, err := s.db.GetUserByHandle(ctx, r.PathValue("handle"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	people, err := s.followPeople(ctx, u.ID, direction)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	title := "关注"
	other := "followers"
	if direction == "followers" {
		title = "粉丝"
		other = "following"
	}

	a, _ := s.authenticate(r)
	s.page(w, r, http.StatusOK, u.Handle+" · "+title, "user_list", a.viewer(), msgOf(r), map[string]any{
		"Title":     title,
		"Handle":    u.Handle,
		"Name":      nameOf(u),
		"People":    people,
		"Direction": direction,
		"Other":     other,
		"OtherTo":   "/u/" + url.PathEscape(u.Handle) + "/" + other,
		"Back":      "/u/" + url.PathEscape(u.Handle),
	})
}

// followPeople picks the direction; the store clamps the page size to its ceiling.
func (s *Server) followPeople(ctx context.Context, id int64, direction string) ([]store.User, error) {
	if direction == "followers" {
		return s.db.ListFollowers(ctx, id, followPageLimit)
	}
	return s.db.ListFollowing(ctx, id, followPageLimit)
}

// followPageLimit bounds a list that has no cursor yet.
const followPageLimit = 200
