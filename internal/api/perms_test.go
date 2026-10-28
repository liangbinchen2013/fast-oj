package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

func (h *harness) user(handle string) store.User {
	h.t.Helper()
	u, err := h.db.GetUserByHandle(ctx(), handle)
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) grant(handle, key string, allowed bool) {
	h.t.Helper()
	u := h.user(handle)
	if err := h.db.SetUserPermission(ctx(), u.ID, key, allowed); err != nil {
		h.t.Fatal(err)
	}
}

// author registers a user with the given capabilities and returns a bearer token.
// author signs a person in through the web form and grants them the listed
// capabilities, which is what an operator does in the permission console.
func (h *harness) author(handle string, perms ...string) session {
	h.t.Helper()
	return h.signIn(handle, perms...)
}

// hiddenValues is what an unchecked 公开 box posts: the field arrives as "0",
// not missing, exactly like a browser form.
func hiddenValues(pid, title string) url.Values {
	values := problemValues(pid, title)
	values.Set("visible", "0")
	return values
}

func TestAuthoringNeedsTheCreateRight(t *testing.T) {
	h := newHarness(t)
	plain := h.author("nobody-yet")

	code, body := h.postAs(plain, "/problems", problemValues("P6000", "想出题"))
	if code != http.StatusForbidden {
		t.Fatalf("unprivileged create = %d %q", code, body)
	}
	if !strings.Contains(body, permProblemCreate) {
		t.Errorf("403 should name the missing capability: %q", body)
	}

	writer := h.author("writer", permProblemCreate)
	if code, body := h.postAs(writer, "/problems", problemValues("P6000", "我的题")); code != http.StatusOK {
		t.Fatalf("granted create = %d %q", code, body)
	}
	p, err := h.db.FindProblem(ctx(), "P6000")
	if err != nil {
		t.Fatal(err)
	}
	if p.OwnerID != h.user("writer").ID {
		t.Fatalf("owner not recorded: %+v", p)
	}
}

func TestOwnershipScopesEditing(t *testing.T) {
	h := newHarness(t)
	alice := h.author("alice2", permProblemCreate)
	bob := h.author("bob2", permProblemCreate)
	if code, _ := h.postAs(alice, "/problems", problemValues("P6001", "爱丽丝的题")); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	form := func(title string) url.Values {
		return url.Values{"pid": {"P6001"}, "title": {title}, "time_ms": {"1000"}, "mem_mb": {"256"}, "visible": {"1"}}
	}

	// Same right, different owner: still refused, and the message names the way out.
	if code, body := h.postAs(bob, "/problems/P6001", form("抢过来")); code != http.StatusForbidden {
		t.Fatalf("non-owner update = %d %q", code, body)
	} else if !strings.Contains(body, permProblemEditAny) {
		t.Errorf("message should name the way out: %q", body)
	}
	if code, _ := h.postAs(bob, "/problems/P6001/delete", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("non-owner delete = %d", code)
	}
	if code, body := h.postAs(alice, "/problems/P6001", form("改自己的")); code != http.StatusOK || !strings.Contains(body, "改自己的") {
		t.Fatalf("owner update = %d %q", code, body)
	}

	// The global right is exactly what unlocks someone else's problem.
	h.grant("bob2", permProblemEditAny, true)
	if code, body := h.postAs(bob, "/problems/P6001", form("管理员代改")); code != http.StatusOK {
		t.Fatalf("edit.any update = %d %q", code, body)
	}
}

func TestPublishRightIsSeparateFromAuthoring(t *testing.T) {
	h := newHarness(t)
	writer := h.author("writer3", permProblemCreate)
	if code, _ := h.postAs(writer, "/problems", problemValues("P6002", "待隐藏")); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	form := func(title, visible string) url.Values {
		return url.Values{"pid": {"P6002"}, "title": {title}, "time_ms": {"1000"}, "mem_mb": {"256"}, "visible": {visible}}
	}

	// Owning a problem does not let you hide it from the public.
	if code, body := h.postAs(writer, "/problems/P6002", form("待隐藏", "0")); code != http.StatusForbidden {
		t.Fatalf("owner hid a problem without the right = %d %q", code, body)
	} else if !strings.Contains(body, permProblemPublish) {
		t.Errorf("message should name the missing capability: %q", body)
	}
	h.grant("writer3", permProblemPublish, true)
	if code, body := h.postAs(writer, "/problems/P6002", form("待隐藏", "0")); code != http.StatusOK {
		t.Fatalf("publish right update = %d %q", code, body)
	}
	if _, page := h.htmlGet("/problems/P6002", h.adminToken()); !strings.Contains(page, "未公开") {
		t.Fatalf("problem should be hidden now:\n%s", page)
	}
	// An edit that leaves visibility alone needs no publishing right at all.
	if code, body := h.postAs(writer, "/problems/P6002", form("改名", "0")); code != http.StatusOK {
		t.Fatalf("same visibility should not need the right: %d %q", code, body)
	}
}

func TestHiddenProblemsRespectRights(t *testing.T) {
	h := newHarness(t)
	owner := h.author("owner4", permProblemCreate, permProblemPublish)
	if code, _ := h.postAs(owner, "/problems", hiddenValues("P6003", "内部题")); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	outsider := h.author("outsider4")

	if code, _ := h.getAs(outsider, "/problems/P6003"); code != http.StatusNotFound {
		t.Fatalf("hidden detail for outsider = %d", code)
	}
	if _, list := h.getAs(outsider, "/problems"); strings.Contains(list, "P6003") {
		t.Error("hidden problem leaked into the outsider list")
	}
	// The author can still see and work on it.
	if code, body := h.getAs(owner, "/problems/P6003"); code != http.StatusOK || !strings.Contains(body, "内部题") {
		t.Fatalf("author view = %d %q", code, body)
	}
	// Submitting to a hidden problem is allowed for the author, refused for others.
	h.uploadCasesAs(owner, "P6003")
	if code, body := h.submitCode(owner, "P6003", "cpp", "int main(){}"); code != http.StatusOK {
		t.Fatalf("author submit to own hidden problem = %d %q", code, body)
	}
	if code, _ := h.submitCode(outsider, "P6003", "cpp", "int main(){}"); code != http.StatusNotFound {
		t.Fatalf("outsider submit to hidden problem = %d", code)
	}
}

// uploadCasesAs posts a case zip as a signed-in browser.
func (h *harness) uploadCasesAs(s session, pid string) {
	h.t.Helper()
	archive := zipOf(map[string]string{
		"sample1.in": "1 2\n", "sample1.out": "3\n",
		"1.in": "1 2\n", "1.out": "3\n",
	})
	if code, body := h.uploadZipAs(s, "/problems/"+url.PathEscape(pid)+"/cases/upload", archive); code != http.StatusOK {
		h.t.Fatalf("upload as %s = %d %q", pid, code, body)
	}
}

func TestCaseRightsAreIndependentOfStatementEditing(t *testing.T) {
	h := newHarness(t)
	author := h.author("author5", permProblemCreate)
	if code, _ := h.postAs(author, "/problems", problemValues("P6004", "缺数据")); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}

	// The author may upload for their own problem.
	h.uploadCasesAs(author, "P6004")

	// A data maintainer with no create right can top up someone else's data...
	maintainer := h.author("maintainer5", permTestcaseManage)
	if code, body := h.uploadZipAs(maintainer, "/problems/P6004/cases/upload",
		zipOf(map[string]string{"2.in": "4 5\n", "2.out": "9\n"})); code != http.StatusOK {
		t.Fatalf("data maintainer upload = %d %q", code, body)
	}
	// ...but cannot rewrite the statement.
	if code, _ := h.postAs(maintainer, "/problems/P6004", url.Values{
		"pid": {"P6004"}, "title": {"越界改名"}, "time_ms": {"1000"}, "mem_mb": {"256"}, "visible": {"1"},
	}); code != http.StatusForbidden {
		t.Fatalf("maintainer edited a statement = %d", code)
	}
	// Nobody else may even list the case set.
	stranger := h.author("stranger5")
	if code, _ := h.getAs(stranger, "/problems/P6004/cases"); code != http.StatusForbidden {
		t.Fatalf("stranger listed cases = %d", code)
	}
	// Judged case bodies stay shut; samples stay open.
	if code, _ := h.get("/problems/P6004/cases/2/in", ""); code != http.StatusForbidden {
		t.Fatalf("anonymous judged case fetch = %d", code)
	}
	if code, _ := h.get("/problems/P6004/cases/sample1/in", ""); code != http.StatusOK {
		t.Fatalf("anonymous sample fetch = %d", code)
	}
}

func TestViewingOthersSourceNeedsARight(t *testing.T) {
	h := newHarness(t)
	writer := h.author("writer6", permProblemCreate)
	h.readyProblemAs(writer, "P6005")
	owner := h.author("owner6")
	intruder := h.author("intruder6")

	if code, body := h.submitCode(owner, "P6005", "cpp", "int main(){return 0;}"); code != http.StatusOK {
		t.Fatalf("submit = %d %.200q", code, body)
	}
	// The source is on the page for the person who wrote it, and for nobody else.
	if _, mine := h.getAs(owner, "/submissions/1"); !strings.Contains(mine, "int main()") {
		t.Fatal("author of the submission could not see their own source")
	}
	if _, other := h.getAs(intruder, "/submissions/1"); strings.Contains(other, "int main()") {
		t.Fatal("another user read someone else's source")
	}
	h.grant("intruder6", permSubmissionViewAny, true)
	if _, audited := h.getAs(intruder, "/submissions/1"); !strings.Contains(audited, "int main()") {
		t.Fatal("submission.view.any had no effect")
	}
}

func (h *harness) readyProblemAs(s session, pid string) {
	h.t.Helper()
	if code, _ := h.postAs(s, "/problems", url.Values{
		"pid": {pid}, "title": {"别人的题"}, "time_ms": {"1000"}, "mem_mb": {"256"},
		"languages": {"c", "cpp"}, "visible": {"1"},
	}); code != http.StatusOK {
		h.t.Fatalf("create %s = %d", pid, code)
	}
	h.uploadCasesAs(s, pid)
}

func TestAdminConsolePermissions(t *testing.T) {
	h := newHarness(t)
	plainCookie, plainCSRF := h.login("plain", "plain@example.com", testPassword)

	// The console is closed without permission.manage.
	if code, _ := h.htmlWithCookie(http.MethodGet, "/admin/perms", plainCookie); code != http.StatusForbidden {
		t.Fatalf("plain user reached the console: %d", code)
	}

	// The bootstrap admin token has it, and can hand it out.
	if code, page := h.get("/admin/perms", h.adminToken()); code != http.StatusOK || !strings.Contains(page, "plain") {
		t.Fatalf("admin console = %d", code)
	}
	if code, body := h.postBearer("/admin/perms/u/plain",
		url.Values{"key": {permProblemCreate}, "action": {"grant"}}, h.adminToken()); code != http.StatusOK {
		// A bearer-authenticated POST needs no CSRF, and the console answers with
		// a redirect that the client follows back to the page.
		t.Fatalf("grant via console = %d %s", code, body)
	}
	u := h.user("plain")
	perms, err := h.db.EffectivePermissions(ctx(), u.ID, u.Role)
	if err != nil || !perms[permProblemCreate] {
		t.Fatalf("grant did not take effect: %+v %v", perms, err)
	}

	// Clearing restores the tier default, which for a plain user is nothing.
	if code, body := h.postBearer("/admin/perms/u/plain",
		url.Values{"key": {permProblemCreate}, "action": {"clear"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("clear = %d %s", code, body)
	}
	if perms, _ := h.db.EffectivePermissions(ctx(), u.ID, u.Role); perms[permProblemCreate] {
		t.Fatal("clear left the grant in place")
	}

	// A cookie session in the console still needs its CSRF token.
	if code, _, _ := h.postNoRedirect("/admin/perms/u/plain",
		url.Values{"key": {permProblemCreate}, "action": {"grant"}}, plainCookie); code != http.StatusForbidden {
		t.Fatalf("CSRF-less console POST = %d", code)
	}
	_ = plainCSRF
}

func TestAdminConsoleRefusesSelfEditAndBlocksLockout(t *testing.T) {
	h := newHarness(t)
	// The bootstrap admin token maps to the "admin" account, so editing that
	// account through the console is the self-edit case.
	code, body := h.json(http.MethodPost, "/admin/perms/u/admin", h.adminToken(),
		[]byte(`{"key":"problem.create","action":"revoke"}`))
	if code != http.StatusBadRequest || !strings.Contains(body, "自己") {
		t.Fatalf("self-edit = %d %q", code, body)
	}
}

func TestRoleAndStatusChanges(t *testing.T) {
	h := newHarness(t)
	cookie, _ := h.login("victim", "victim@example.com", testPassword)

	// A promoted user gains the whole staff set through the tier.
	if code, body := h.postBearer("/admin/users/victim/role", url.Values{"role": {"admin"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("role change = %d %s", code, body)
	}
	if u := h.user("victim"); u.Role != "admin" {
		t.Fatalf("role = %s", u.Role)
	}
	if _, page := h.htmlWithCookie(http.MethodGet, "/admin/perms", cookie); !strings.Contains(page, "victim") {
		t.Error("promoted user still cannot reach the console")
	}

	// Banning kills live sessions immediately.
	if code, body := h.postBearer("/admin/users/victim/status", url.Values{"status": {"banned"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("ban = %d %s", code, body)
	}
	if _, page := h.htmlWithCookie(http.MethodGet, "/admin/perms", cookie); strings.Contains(page, "victim") {
		t.Error("banned session still authenticated")
	}
	// A banned account cannot log back in, and the reason is not spelled out.
	banned, bannedPage := h.formPage("/login", url.Values{"identifier": {"victim"}, "password": {testPassword}})
	if banned == http.StatusSeeOther {
		t.Fatal("banned account logged in")
	}
	if strings.Contains(bannedPage, "封禁") || strings.Contains(bannedPage, "banned") {
		t.Errorf("login should not reveal the ban reason:\n%s", bannedPage)
	}

	// Unbanning restores access without a password reset.
	if code, body := h.postBearer("/admin/users/victim/status", url.Values{"status": {"active"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("unban = %d %s", code, body)
	}
	code, _, hdr := h.postNoRedirect("/login", url.Values{"identifier": {"victim"}, "password": {testPassword}}, "")
	if code != http.StatusSeeOther || hdr.Get("Set-Cookie") == "" {
		t.Fatalf("reactivated login = %d %v", code, hdr.Values("Set-Cookie"))
	}
}

func TestJudgeViewRight(t *testing.T) {
	h := newHarness(t)
	watcher := h.author("watcher7")
	if code, _ := h.getAs(watcher, "/judges"); code != http.StatusForbidden {
		t.Fatalf("plain user listed judges = %d", code)
	}
	auditor := h.author("auditor7", permJudgeView)
	if code, _ := h.getAs(auditor, "/judges"); code != http.StatusOK {
		t.Fatalf("judge.view holder = %d", code)
	}
	// The judge token itself is not a licence to browse the registry.
	if code, _ := h.get("/judges", h.judge); code != http.StatusForbidden {
		t.Fatalf("judge token listed judges = %d", code)
	}
}

func TestPermissionStoreRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := ctx()
	// Both accounts have to exist first: the admin row is created lazily by the
	// first request that authenticates with the bootstrap token.
	h.htmlGet("/problems", h.adminToken())
	h.login("plain", "plain@example.com", testPassword)

	perms, err := h.db.ListPermissions(ctx)
	if err != nil || len(perms) < 8 {
		t.Fatalf("permissions = %d %v", len(perms), err)
	}
	u := h.user("admin")
	if grants, err := h.db.UserGrants(ctx, u.ID, u.Role); err != nil || len(grants) != len(perms) {
		t.Fatalf("grants = %d want %d (%v)", len(grants), len(perms), err)
	} else {
		for _, g := range grants {
			if !g.Effective || !g.ByRole {
				t.Fatalf("admin tier should hold everything: %+v", g)
			}
		}
	}

	// An unknown key is refused rather than silently doing nothing. The target is a
	// normal account: the admin row is this site's super admin, and every write
	// against it is refused before the key is even looked at.
	if err := h.db.SetUserPermission(ctx, h.user("plain").ID, "not.a.permission", true); err != store.ErrNotFound {
		t.Fatalf("unknown key = %v", err)
	}

	// An explicit removal beats the tier default, and clearing restores it.
	plain := h.user("plain")
	if err := h.db.SetUserRole(ctx, plain.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetUserPermission(ctx, plain.ID, permProblemCreate, false); err != nil {
		t.Fatal(err)
	}
	eff, err := h.db.EffectivePermissions(ctx, plain.ID, "admin")
	if err != nil || eff[permProblemCreate] {
		t.Fatalf("explicit removal should beat the tier: %+v %v", eff[permProblemCreate], err)
	}
	// An explicit grant on a plain tier works the other way too.
	if err := h.db.SetUserPermission(ctx, plain.ID, permFeedModerate, true); err != nil {
		t.Fatal(err)
	}
	if eff, _ = h.db.EffectivePermissions(ctx, plain.ID, "user"); !eff[permFeedModerate] {
		t.Fatal("explicit grant on the plain tier had no effect")
	}
	if err := h.db.ClearUserPermission(ctx, plain.ID, permProblemCreate); err != nil {
		t.Fatal(err)
	}
	if eff, _ := h.db.EffectivePermissions(ctx, plain.ID, "admin"); !eff[permProblemCreate] {
		t.Fatal("clear did not restore the tier default")
	}
}

// 站点空着的时候，第一个注册的人就是超级管理员：不需要任何人授权，他什么都能干。
func TestFirstRegistrantSeatsTheSuperAdmin(t *testing.T) {
	h := newHarness(t)
	h.clearOwner()

	founder := h.signIn("founder")
	u := h.user("founder")
	if id, err := h.db.OwnerID(ctx()); err != nil || id != u.ID {
		t.Fatalf("第一个注册的人没坐上站主的位子：owner=%d err=%v", id, err)
	}
	// 坐这个位子的同时就是管理员，因为别的路径都读 users.role 判管理员。
	if u.Role != "admin" {
		t.Errorf("站主角色 = %s, want admin", u.Role)
	}
	if code, page := h.getAs(founder, "/admin/perms"); code != http.StatusOK || !strings.Contains(page, "founder") {
		t.Fatalf("站主进权限控制台 = %d", code)
	}

	// 第二个来注册的人什么也没多。
	second := h.signIn("second")
	if id, _ := h.db.OwnerID(ctx()); id != u.ID {
		t.Fatal("站主的位子被第二个注册的人抢走了")
	}
	if code, _ := h.getAs(second, "/admin/perms"); code != http.StatusForbidden {
		t.Errorf("第二个注册的人进控制台 = %d, want 403", code)
	}
}

// 已经有人在用的站点里，路人注册补不上这个位子；只有拿着管理令牌接管的那次能补。
// 这条是给"老师先用批量建号建了全班，自己还没注册"那种顺序兜底的。
func TestStrayRegistrationCannotSeatOwnerOnAPopulatedSite(t *testing.T) {
	h := newHarness(t)
	h.signIn("teacher")
	h.clearOwner()

	h.signIn("student")
	if id, _ := h.db.OwnerID(ctx()); id != 0 {
		t.Fatalf("库里已经有人了，注册还是补上了站主：%d", id)
	}
	if code, _, _ := h.postNoRedirect("/admin/claim",
		url.Values{"token": {h.adminToken()}, "password": {testPassword}}, ""); code != http.StatusSeeOther {
		t.Fatalf("接管 = %d", code)
	}
	if id, _ := h.db.OwnerID(ctx()); id != h.user(claimHandle).ID {
		t.Fatalf("接管之后站主 = %d, want %d", id, h.user(claimHandle).ID)
	}
}

// 另一个管理员，哪怕 permission.manage 和 user.manage 都在手上，也动不了站主。
func TestSuperAdminSurvivesEveryOtherAdmin(t *testing.T) {
	h := newHarness(t)
	h.clearOwner()
	h.signIn("founder")

	deputy := h.author("deputy", permPermissionManage, permUserManage)
	for _, try := range []struct {
		path   string
		values url.Values
		why    string
	}{
		{"/admin/users/founder/role", url.Values{"role": {"user"}}, "降成普通人"},
		{"/admin/users/founder/status", url.Values{"status": {"banned"}}, "封号"},
		{"/admin/perms/u/founder", url.Values{"key": {permProblemCreate}, "action": {"revoke"}}, "收回出题权"},
	} {
		code, body := h.postAs(deputy, try.path, try.values)
		if !strings.Contains(body, "超级管理员") {
			t.Errorf("%s（%s）没有挡住：= %d\n%s", try.path, try.why, code, short(body))
		}
	}

	u := h.user("founder")
	if u.Role != "admin" || u.Status != "active" {
		t.Fatalf("站主被改动了：%+v", u)
	}
	perms, err := h.db.EffectivePermissions(ctx(), u.ID, u.Role)
	if err != nil || !perms[permProblemCreate] {
		t.Fatalf("站主少了出题权：%+v %v", perms, err)
	}
	// 他的那一行在控制台里读得到，只是没有任何按钮。
	if _, page := h.getAs(deputy, "/admin/perms/u/founder"); !strings.Contains(page, "超级管理员") {
		t.Errorf("站主页面没说他动不了：\n%s", short(page))
	}
}
