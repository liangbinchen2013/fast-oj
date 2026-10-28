package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The bootstrap token is a curl credential, so before this page a brand-new
// database had no way to reach the console from a browser at all.
func TestAdminClaimTakesOverAFreshDatabase(t *testing.T) {
	h := newHarness(t)

	code, page := h.htmlGet("/admin/claim", "")
	if code != http.StatusOK || !strings.Contains(page, "接管后台") || !strings.Contains(page, `name="token"`) {
		t.Fatalf("claim page = %d:\n%s", code, page)
	}
	if strings.Contains(page, "已经有密码") {
		t.Error("a fresh account should not warn about overwriting a password")
	}
	// 令牌不该被渲染进页面，也不该在任何响应里出现一遍。
	token := h.adminToken()
	if strings.Contains(page, token) {
		t.Fatal("the form leaked the operator token")
	}

	if code, body := h.postForm2("/admin/claim", url.Values{"token": {"wrong"}, "password": {testPassword}}, "", ""); code != http.StatusForbidden {
		t.Fatalf("wrong token = %d:\n%s", code, body)
	}
	// 密码不合格先拒：这一步不该把唯一的那枚令牌烧掉，否则填错一次密码就得去日志里
	// 重新抄一串。
	if code, _ := h.postForm2("/admin/claim", url.Values{"token": {token}, "password": {"short1"}}, "", ""); code != http.StatusBadRequest {
		t.Fatalf("weak password = %d", code)
	}
	if u, err := h.db.GetUserByHandle(ctx(), "admin"); err == nil && u.HasPassword {
		t.Fatal("a refused claim still set a password")
	}

	code, _, hdr := h.postNoRedirect("/admin/claim", url.Values{"token": {token}, "password": {testPassword}}, "")
	if code != http.StatusSeeOther {
		t.Fatalf("claim = %d", code)
	}
	if cookieValue(hdr.Values("Set-Cookie"), "fastoj_session") == "" {
		t.Fatalf("no session cookie on a successful claim: %q", hdr.Values("Set-Cookie"))
	}
	// 用过的这一枚不再能动，哪怕密码换一个：这就是这次修的那个洞——以前同一个串可以
	// 反复接管，等于谁拿到它谁永久握着这个站。
	if code, _ := h.postForm2("/admin/claim", url.Values{"token": {token}, "password": {"another-pass-1"}}, "", ""); code != http.StatusForbidden {
		t.Fatalf("用过的令牌第二次还能动：%d", code)
	}
	// 被拒的那一次什么都没写：第二个密码没有生效（拿它登录还是会被拒）。
	if code, _, _ := h.postNoRedirect("/login", url.Values{"identifier": {"admin"}, "password": {"another-pass-1"}}, ""); code == http.StatusSeeOther {
		t.Fatalf("被拒的第二次接管竟然把密码改了（登录新密码 = %d）", code)
	}
	// The account the token owns is the account that can now log in.
	sess, _ := h.db.GetUserByHandle(ctx(), "admin")
	if sess.Role != "admin" || !sess.HasPassword {
		t.Fatalf("claimed account = %+v", sess)
	}
	_, perms := h.htmlGet("/admin/perms", h.adminToken())
	if !strings.Contains(perms, "admin") {
		t.Fatalf("the console lost the claimed account:\n%s", perms)
	}

	// And a real browser login works with the password just set.
	if code, _, lhdr := h.postNoRedirect("/login", url.Values{"identifier": {"admin"}, "password": {testPassword}}, ""); code != http.StatusSeeOther ||
		cookieValue(lhdr.Values("Set-Cookie"), "fastoj_session") == "" {
		t.Fatalf("login after claim = %d", code)
	}
}

func TestAdminClaimCannotBeGuessed(t *testing.T) {
	h := newHarness(t)

	// Each wrong token costs one of the small budget, then the page closes for the
	// window; guessing a 24-byte token from a browser is not a viable path.
	for i := 0; i < claimAttempts; i++ {
		if code, _ := h.postForm2("/admin/claim", url.Values{"token": {"guess"}, "password": {testPassword}}, "", ""); code != http.StatusForbidden {
			t.Fatalf("attempt %d = %d, want 403", i+1, code)
		}
	}
	code, body := h.postForm2("/admin/claim", url.Values{"token": {h.adminToken()}, "password": {testPassword}}, "", "")
	if code != http.StatusTooManyRequests || !strings.Contains(body, "15 分钟") {
		t.Fatalf("after the budget = %d:\n%s", code, body)
	}
	// Even the right token cannot get in during the lockout.
	if u, err := h.db.GetUserByHandle(ctx(), "admin"); err == nil && u.HasPassword {
		t.Fatal("the lockout was bypassed with the real token")
	}
}

func TestAdminClaimIsForAnonymousBrowsers(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("claimer", "claimer@example.com", testPassword)

	// A signed-in person is sent home rather than shown a token field they cannot
	// use, and their own write still needs the CSRF token.
	// The redirect is followed, so what matters is that the token field never
	// reaches a signed-in browser.
	if _, page := h.htmlWithCookie(http.MethodGet, "/admin/claim", cookie); strings.Contains(page, `name="token"`) {
		t.Error("a signed-in browser was shown the token field")
	}
	if code, _, _ := h.postNoRedirect("/admin/claim", url.Values{"token": {h.adminToken()}, "password": {testPassword}}, cookie); code != http.StatusForbidden {
		t.Fatalf("CSRF-less claim from a browser = %d, want 403", code)
	}
	// With the token it is a normal write... and it still hands the session to the
	// admin account, not to whoever was logged in.
	code, _, hdr := h.postForm("/admin/claim", url.Values{"token": {h.adminToken()}, "password": {testPassword}}, cookie, csrf)
	if code != http.StatusSeeOther {
		t.Fatalf("claim with csrf = %d", code)
	}
	if id := cookieValue(hdr.Values("Set-Cookie"), "fastoj_session"); id == "" {
		t.Fatal("no session issued")
	} else if u, _, err := h.db.SessionUser(ctx(), id); err != nil || u.Handle != claimHandle {
		t.Fatalf("session belongs to %+v %v", u, err)
	}
}
