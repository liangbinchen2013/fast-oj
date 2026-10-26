package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// admin.go is the permission console: who may do what, down to single
// capabilities, without handing out an admin account.

type adminUserRow struct {
	ID     int64
	Handle string
	Email  string
	Role   string
	Status string
	// Owner marks the super admin: the one account the console cannot touch.
	Owner bool
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	// 这一页对两种管理员都有意义：permission.manage 来看和改能力，user.manage 来改角色、
	// 状态、标签和注销。所以门槛是"任意一把"，具体哪几个按钮出现由下面的能力位决定。
	a, ok := s.requireAnyPerm(w, r, permPermissionManage, permUserManage)
	if !ok {
		return
	}
	users, err := s.db.ListUsers(r.Context(), 1000)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	ownerID, err := s.db.OwnerID(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	rows := make([]adminUserRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, adminUserRow{
			ID: u.ID, Handle: u.Handle, Email: u.Email, Role: u.Role, Status: u.Status,
			Owner: u.ID == ownerID,
		})
	}
	s.page(w, r, http.StatusOK, "权限管理", "admin_users", a.viewer(), msgOf(r), map[string]any{
		"Users": rows, "Self": a.User.ID,
	})
}

type grantRow struct {
	Key       string
	Label     string
	Staff     bool
	ByRole    bool
	Override  string // "", "granted", "removed"
	Effective bool
}

func (s *Server) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requireAnyPerm(w, r, permPermissionManage, permUserManage)
	if !ok {
		return
	}
	u, grants, rows, err := s.loadGrantView(r)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	owner, err := s.db.IsOwner(r.Context(), u.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "权限 · "+u.Handle, "admin_user", a.viewer(), msgOf(r), map[string]any{
		"Target":   u,
		"Grants":   rows,
		"AllPerms": grants,
		"Self":     a.User.ID == u.ID,
		// Owner is the second reason this page has no buttons: the console can
		// show the super admin what he holds, but not change it.
		"Owner": owner,
		// Cancelled hides the whole dangerous zone: a tombstone cannot be
		// re-edited, and its freed name may already belong to someone else.
		"Cancelled": u.IsCancelled(),
		"Anonym":    store.AnonymousName,
		"CanUser":   a.can(permUserManage),
		// CanPerm 决定那排"授予/收回/恢复默认"的按钮出不出现：能改角色与标签的人不必
		// 也能改能力，两件事分开授权。
		"CanPerm": a.can(permPermissionManage),
		// 标签给的是原文（逗号分隔），管理员看到的输入框就是他自己上次写的那样。
		"Tags":       u.Tags,
		"MaxTags":    store.MaxTags,
		"MaxTagLen":  store.MaxTagLen,
		"MaxTagText": store.MaxTagsText,
		// NeedStepUp 决定这一页摆的是密码框还是那排改权限的按钮：判断与 requireStepUp
		// 用的是同一个函数，所以页面和闸门不会各说一套。
		"NeedStepUp": !stepUpOK(a),
	})
}

// loadGrantView resolves the target user and merges the three sources of truth
// (tier default, individual override, effective result) into table rows.
func (s *Server) loadGrantView(r *http.Request) (store.User, []store.UserGrant, []grantRow, error) {
	u, err := s.db.GetUserByHandle(r.Context(), r.PathValue("handle"))
	if err != nil {
		return store.User{}, nil, nil, err
	}
	grants, err := s.db.UserGrants(r.Context(), u.ID, u.Role)
	if err != nil {
		return u, nil, nil, err
	}
	rows := make([]grantRow, 0, len(grants))
	for _, g := range grants {
		override := ""
		if g.Override != nil {
			if *g.Override {
				override = "granted"
			} else {
				override = "removed"
			}
		}
		rows = append(rows, grantRow{
			Key: g.Key, Label: g.Label, Staff: g.Staff, ByRole: g.ByRole,
			Override: override, Effective: g.Effective,
		})
	}
	return u, grants, rows, nil
}

// adminTarget loads the user and refuses two things: editing yourself, and editing
// the super admin. Locking yourself out of the permission console is a far more
// likely accident than an attack, so the console simply refuses to change the
// account you are signed in with. The site owner is immune to every other admin,
// which is the one rule that has to hold even when someone else holds
// permission.manage.
func (s *Server) adminTarget(w http.ResponseWriter, r *http.Request, a actor) (store.User, bool) {
	u, err := s.db.GetUserByHandle(r.Context(), r.PathValue("handle"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return store.User{}, false
	}
	if u.ID == a.User.ID {
		s.respondError(w, r, http.StatusBadRequest, "不能修改自己的权限，请用另一个管理员账号")
		return store.User{}, false
	}
	owner, err := s.db.IsOwner(r.Context(), u.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return store.User{}, false
	}
	if owner {
		s.respondError(w, r, http.StatusForbidden, "超级管理员的权限、角色与状态只有他自己能动")
		return store.User{}, false
	}
	return u, true
}

// handleAdminCancel is 注销账号: the identity is erased and the login name freed for
// the next registrant, while every record the account produced stays and is shown as
// coming from 封禁用户.
//
// 这一步不可逆，所以页面上要求填一遍用户名确认，跟删除站点数据那种"我知道我在干什么"
// 的表单一个形状。
func (s *Server) handleAdminCancel(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permUserManage)
	if !ok {
		return
	}
	target, ok := s.adminTarget(w, r, a)
	if !ok {
		return
	}
	back := "/admin/perms/u/" + url.PathEscape(target.Handle)
	if target.IsCancelled() {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("这个账号已经注销过了"), http.StatusSeeOther)
		return
	}
	// 打字确认：注销之后用户名就自由了，下一个注册的人跟这些记录再无关系，
	// 管理员必须知道自己放弃的是什么。
	if strings.TrimSpace(r.FormValue("confirm")) != target.Handle {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("要注销请把用户名原样填进确认框"), http.StatusSeeOther)
		return
	}
	// 打字确认管的是"我知道我在干什么"，这道确认管的是"是我本人在干"。两个都要，顺序上
	// 先把填错的那种最常见的手滑挡掉。
	if !s.requireStepUp(w, r, a, back, "注销") {
		return
	}
	// 名下还有题或比赛时先拦住：注销会让这些题没人能改，比赛没人能办。
	problems, contests, err := s.db.OwnedCounts(r.Context(), target.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if problems > 0 || contests > 0 {
		s.respondError(w, r, http.StatusBadRequest, fmt.Sprintf(
			"这个账号名下还有 %d 道题、%d 场比赛。注销之后它们就没有作者了：先把题转走或删掉、比赛删掉，再来注销。",
			problems, contests))
		return
	}
	handle, err := s.db.CancelAccount(r.Context(), target.ID)
	if err != nil {
		if err == store.ErrSiteOwner || err == store.ErrAccountCancelled {
			s.respondError(w, r, http.StatusForbidden, err.Error())
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("account cancelled", "by", a.User.Handle, "target", target.Handle, "now", handle)
	s.audit(r, a, "account.cancel", target.Handle, "身份已抹掉，用户名释放；留下的记录署名成"+handle)
	http.Redirect(w, r, "/admin/perms?msg="+url.QueryEscape(
		fmt.Sprintf("%s 已注销：用户名、邮箱、密码全部抹掉，%q 可以重新注册；他留下的记录还在，显示为%s。",
			target.Handle, target.Handle, store.AnonymousName)), http.StatusSeeOther)
}

func (s *Server) handleAdminPermChange(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permPermissionManage)
	if !ok {
		return
	}
	target, ok := s.adminTarget(w, r, a)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	action := strings.TrimSpace(r.FormValue("action"))
	back := "/admin/perms/u/" + url.PathEscape(target.Handle)
	// 逐项权限是这套站点最值钱的改动：光有 permission.manage 不够，这一次还得证明
	// 拿着这个会话的人本人还在。
	if !s.requireStepUp(w, r, a, back, "权限") {
		return
	}

	switch action {
	case "grant", "revoke":
		if err := s.db.SetUserPermission(r.Context(), target.ID, key, action == "grant"); err != nil {
			if err == store.ErrNotFound {
				s.respondError(w, r, http.StatusBadRequest, "没有这个权限项："+key)
				return
			}
			s.writeStoreError(w, r, err)
			return
		}
	case "clear":
		if err := s.db.ClearUserPermission(r.Context(), target.ID, key); err != nil {
			s.writeStoreError(w, r, err)
			return
		}
	default:
		s.respondError(w, r, http.StatusBadRequest, "action 只能是 grant / revoke / clear")
		return
	}
	s.log.Info("permission changed", "by", a.User.Handle, "target", target.Handle, "key", key, "action", action)
	// action 只能是 grant / revoke / clear（上面那个 switch 已经把住了），所以拼出来的
	// 动作键是这三条之一，不是表单里随便填的文本。
	s.audit(r, a, "perm."+action, target.Handle, key)
	http.Redirect(w, r, back+"?msg="+url.QueryEscape(fmt.Sprintf("%s → %s", target.Handle, key)), http.StatusSeeOther)
}

func (s *Server) handleAdminRoleChange(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permUserManage)
	if !ok {
		return
	}
	target, ok := s.adminTarget(w, r, a)
	if !ok {
		return
	}
	role := strings.TrimSpace(r.FormValue("role"))
	if !s.requireStepUp(w, r, a, "/admin/perms/u/"+url.PathEscape(target.Handle), "角色") {
		return
	}
	if err := s.db.SetUserRole(r.Context(), target.ID, role); err != nil {
		s.respondError(w, r, http.StatusBadRequest, "角色设置失败: "+err.Error())
		return
	}
	s.log.Info("role changed", "by", a.User.Handle, "target", target.Handle, "role", role)
	s.audit(r, a, "role.change", target.Handle, "角色改为 "+role+"（原 "+target.Role+"）")
	http.Redirect(w, r, "/admin/perms/u/"+url.PathEscape(target.Handle)+"?msg="+url.QueryEscape("角色已设为 "+role), http.StatusSeeOther)
}

func (s *Server) handleAdminStatusChange(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permUserManage)
	if !ok {
		return
	}
	target, ok := s.adminTarget(w, r, a)
	if !ok {
		return
	}
	status := strings.TrimSpace(r.FormValue("status"))
	if !s.requireStepUp(w, r, a, "/admin/perms/u/"+url.PathEscape(target.Handle), "状态") {
		return
	}
	if err := s.db.SetUserStatus(r.Context(), target.ID, status); err != nil {
		s.respondError(w, r, http.StatusBadRequest, "状态设置失败: "+err.Error())
		return
	}
	s.log.Info("status changed", "by", a.User.Handle, "target", target.Handle, "status", status)
	s.audit(r, a, "user.status", target.Handle,
		fmt.Sprintf("状态改为 %s（原 %s）；封禁会踢掉这个账号的所有登录会话", userStatusLabel(status), userStatusLabel(target.Status)))
	http.Redirect(w, r, "/admin/perms/u/"+url.PathEscape(target.Handle)+"?msg="+url.QueryEscape("状态已设为 "+status), http.StatusSeeOther)
}

// handleAdminTags 改的是"别人怎么称呼这个人"，不是"他能做什么"。
//
// 所以它要 user.manage，但**不进那道 2 分钟的身份确认**：那扇门是给权限、角色、状态、注销
// 准备的——那些一改就改变一个人能做的事。标签贴错随时能改回来，也不会让任何人多出一点权力；
// 把它一起关进门里，只会让管理员每贴一个标签都要重输一次密码。
func (s *Server) handleAdminTags(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permUserManage)
	if !ok {
		return
	}
	target, ok := s.adminTarget(w, r, a)
	if !ok {
		return
	}
	back := "/admin/perms/u/" + url.PathEscape(target.Handle)
	raw := r.FormValue("tags")
	if err := s.db.SetUserTags(r.Context(), target.ID, raw); err != nil {
		msg := "标签没能保存：" + err.Error()
		switch {
		case errors.Is(err, store.ErrTooManyTags):
			msg = fmt.Sprintf("标签最多 %d 个，用逗号分开就行。", store.MaxTags)
		case errors.Is(err, store.ErrBadTag):
			msg = fmt.Sprintf("标签里有不能用的字：逗号与方括号是分隔符，单个标签最长 %d 个字。", store.MaxTagLen)
		}
		http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	// 记的是这次真的写进去的那一串（SetUserTags 会 trim、会把中文逗号归一化），
	// 不是表单原文——审计要能回答"当时页面上显示的到底是什么"。
	now := store.TagsTextOf(raw)
	s.log.Info("tags changed", "by", a.User.Handle, "target", target.Handle, "from", target.Tags, "to", now)
	s.audit(r, a, "user.tags", target.Handle, fmt.Sprintf("由 %q 改成 %q", target.Tags, now))
	http.Redirect(w, r, back+"?msg="+url.QueryEscape("标签已保存，全站名字后面就会跟着显示。"), http.StatusSeeOther)
}
