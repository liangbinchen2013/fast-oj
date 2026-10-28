package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

func TestRegisterRejectsBadInput(t *testing.T) {
	h := newHarness(t)

	bad := []struct {
		values url.Values
		why    string
	}{
		{url.Values{"handle": {"ok"}, "email": {"not-an-email"}, "password": {testPassword}}, "邮箱"},
		{url.Values{"handle": {"ok"}, "email": {"a@b.co"}, "password": {"short1"}}, "至少"},
		{url.Values{"handle": {"ok"}, "email": {"a@b.co"}, "password": {"1234567890"}}, "全是数字"},
		{url.Values{"handle": {"has space"}, "email": {"a@b.co"}, "password": {testPassword}}, "用户名"},
		{url.Values{"handle": {"admin"}, "email": {"a@b.co"}, "password": {testPassword}}, "保留"},
	}
	for _, c := range bad {
		code, body := h.formPage("/register", c.values)
		if code == http.StatusSeeOther {
			t.Errorf("%v accepted (%s)", c.values, c.why)
		}
		if c.why != "" && !strings.Contains(body, c.why) {
			t.Errorf("%v: message lost %q:\n%s", c.values, c.why, body)
		}
	}

	// Duplicates in either field collide instead of creating a second account.
	if code, _, _ := h.postNoRedirect("/register",
		url.Values{"handle": {"dup"}, "email": {"dup@example.com"}, "password": {testPassword}}, ""); code != http.StatusSeeOther {
		t.Fatalf("first register = %d", code)
	}
	if _, body := h.formPage("/register", url.Values{"handle": {"dup"}, "email": {"other@example.com"}, "password": {testPassword}}); !strings.Contains(body, "已经注册过") {
		t.Errorf("duplicate handle: %s", body)
	}
	if _, body := h.formPage("/register", url.Values{"handle": {"other"}, "email": {"dup@example.com"}, "password": {testPassword}}); !strings.Contains(body, "已经注册过") {
		t.Errorf("duplicate email: %s", body)
	}
}

// formPage posts a form and follows the redirect, returning the rendered page.
func (h *harness) formPage(path string, values url.Values) (int, string) {
	h.t.Helper()
	code, body, _ := h.postForm(path, values, "", "")
	return code, body
}

// 已登录的浏览器不该看到注册表单：那表单没有 CSRF 字段（匿名不需要），带着 cookie
// 一提交就会被闸门挡成一句看不懂的「表单已过期」。
func TestRegisterPageRedirectsSignedIn(t *testing.T) {
	h := newHarness(t)
	user := h.signIn("already_in")
	// 客户端会跟着 303 走到首页，所以断言"落在首页、看不到注册表单"。
	for _, path := range []string{"/register", "/login"} {
		code, body := h.getAs(user, path)
		if code != http.StatusOK || strings.Contains(body, "密码（至少 10 位）") {
			t.Errorf("signed-in GET %s = %d and the register form is still on screen:\n%s", path, code, short(body))
		}
	}
	// 匿名仍然要能看到并填这张表单。
	if code, body := h.htmlGet("/register", ""); code != http.StatusOK || !strings.Contains(body, "密码") {
		t.Errorf("anonymous register page = %d", code)
	}
}

func TestLoginFailureIsIndistinguishable(t *testing.T) {
	h := newHarness(t)
	if _, err := h.db.CreateAccount(ctx(), store.AccountInput{
		Handle: "target", Email: "target@example.com", Password: testPassword,
	}); err != nil {
		t.Fatal(err)
	}

	_, wrongPassword := h.formPage("/login", url.Values{"identifier": {"target"}, "password": {"wrong-password-here"}})
	_, unknownUser := h.formPage("/login", url.Values{"identifier": {"ghost"}, "password": {"wrong-password-here"}})
	if !strings.Contains(wrongPassword, "账号或密码不正确") || !strings.Contains(unknownUser, "账号或密码不正确") {
		t.Fatal("login errors must not differ")
	}
	if strings.Contains(unknownUser, "不存在") || strings.Contains(wrongPassword, "密码错误") {
		t.Fatal("login error leaks which half was wrong")
	}

	// Email or handle both work, and the session cookie follows.
	code, _, hdr := h.postNoRedirect("/login", url.Values{"identifier": {"target@example.com"}, "password": {testPassword}}, "")
	if code != http.StatusSeeOther || cookieValue(hdr.Values("Set-Cookie"), "fastoj_session") == "" {
		t.Fatalf("login by email = %d %v", code, hdr.Values("Set-Cookie"))
	}
}

func TestLoginThrottlesRepeatedFailures(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxLoginFailures; i++ {
		code, _ := h.formPage("/login", url.Values{"identifier": {"ghost"}, "password": {"wrong-password-here"}})
		if code == http.StatusTooManyRequests && i < maxLoginFailures-1 {
			t.Fatalf("locked out early at attempt %d", i+1)
		}
	}
	locked, body := h.formPage("/login", url.Values{"identifier": {"ghost"}, "password": {"wrong-password-here"}})
	if locked != http.StatusTooManyRequests || !strings.Contains(body, "15 分钟") {
		t.Fatalf("after %d failures: %d %q", maxLoginFailures, locked, body)
	}
	// A successful login clears the counter for that client.
	if err := h.db.ClearLoginFailures(ctx(), "login:127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.formPage("/login", url.Values{"identifier": {"ghost"}, "password": {"wrong"}}); code == http.StatusTooManyRequests {
		t.Fatal("counter not cleared")
	}
}

func TestNextParameterCannotEscapeTheSite(t *testing.T) {
	h := newHarness(t)
	if _, err := h.db.CreateAccount(ctx(), store.AccountInput{
		Handle: "redirectee", Email: "redirectee@example.com", Password: testPassword,
	}); err != nil {
		t.Fatal(err)
	}
	for _, evil := range []string{
		"https://evil.example/phish", "//evil.example", "/\\evil.example", "http://evil.example/x",
	} {
		code, _, hdr := h.postNoRedirect("/login",
			url.Values{"identifier": {"redirectee"}, "password": {testPassword}, "next": {evil}}, "")
		if code != http.StatusSeeOther {
			t.Fatalf("%s login = %d", evil, code)
		}
		if got := hdr.Get("Location"); got != "/" {
			t.Errorf("next=%s bounced to %q, want /", evil, got)
		}
	}
	// A legitimate in-site target is honoured.
	if _, _, hdr := h.postNoRedirect("/login",
		url.Values{"identifier": {"redirectee"}, "password": {testPassword}, "next": {"/submissions?user=me"}}, ""); hdr.Get("Location") != "/submissions?user=me" {
		t.Errorf("in-site next lost: %q", hdr.Get("Location"))
	}
}

func TestCSRFGatesCookieWritesOnly(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P5000")
	cookie, csrf := h.login("csrfuser", "csrf@example.com", testPassword)

	// A cookie-authenticated POST with no token is refused...
	if code, _, _ := h.postNoRedirect("/problems/P5000/submit",
		url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, cookie); code != http.StatusForbidden {
		t.Fatalf("missing csrf = %d, want 403", code)
	}
	// ...with the wrong token...
	if code, _, _ := h.postForm("/problems/P5000/submit",
		url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, cookie, "guessed"); code != http.StatusForbidden {
		t.Fatalf("wrong csrf = %d, want 403", code)
	}
	// ...and with a token from someone else's session.
	otherCookie, otherCSRF := h.login("other", "other@example.com", testPassword)
	if code, _, _ := h.postForm("/problems/P5000/submit",
		url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, otherCookie, csrf); code != http.StatusForbidden {
		t.Fatalf("cross-session csrf = %d, want 403", code)
	}
	// The right pair works, and the header form is accepted too.
	if code, _, hdr := h.postForm("/problems/P5000/submit",
		url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, otherCookie, otherCSRF); code != http.StatusSeeOther {
		t.Fatalf("valid csrf = %d", code)
	} else if !strings.HasPrefix(hdr.Get("Location"), "/submissions/") {
		t.Fatalf("no submission redirect: %q", hdr.Get("Location"))
	}

	// The operator token authenticates without a cookie, so it needs no CSRF token
	// either: there is no browser session for a cross-site request to ride.
	if code, body := h.postBearer("/problems", problemValues("P9100", "运维建的题"), h.adminToken()); code != http.StatusOK {
		t.Fatalf("operator post without csrf = %d %s", code, body)
	}
}

func TestRegistrationCanBeClosed(t *testing.T) {
	h := newHarnessCfg(t, func(cfg *config.Config) { cfg.AllowRegister = false })
	if code, _ := h.htmlGet("/register", ""); code != http.StatusForbidden {
		t.Fatalf("register page = %d, want 403", code)
	}
	if code, _ := h.formPage("/register", url.Values{
		"handle": {"late"}, "email": {"late@example.com"}, "password": {testPassword}}); code != http.StatusForbidden {
		t.Fatalf("register post = %d, want 403", code)
	}
}

func TestPasswordChangeDropsSessions(t *testing.T) {
	h := newHarness(t)
	cookie, _ := h.login("dual", "dual@example.com", testPassword)
	u, err := h.db.GetUserByHandle(ctx(), "dual")
	if err != nil {
		t.Fatal(err)
	}

	// A browser session is the only credential a person holds, so changing the
	// password has to end every one of them.
	if err := h.db.SetPassword(ctx(), u.ID, "another-good-password", 4); err != nil {
		t.Fatal(err)
	}
	if _, page := h.htmlWithCookie(http.MethodGet, "/settings", cookie); strings.Contains(page, "dual@example.com") {
		t.Error("session survived a password change")
	}
}

func TestPasswordHelpers(t *testing.T) {
	for _, good := range []string{"a@b.co", "user.name+tag@example.co.uk", "x_y@sub.domain.com"} {
		if !validEmail(good) {
			t.Errorf("%s should be valid", good)
		}
	}
	for _, bad := range []string{"", "a", "no-at-sign", "@b.co", "a@", "a@@b.co", "a b@c.co", "a@b", "a@.co", "a@co.", "中文@例.com", "a<b>@c.co"} {
		if validEmail(bad) {
			t.Errorf("%s should be invalid", bad)
		}
	}
	for pw, want := range map[string]string{
		"short1":                "至少",
		strings.Repeat("x", 80): "最长",
		"1234567890":            "数字",
		"correct-horse":         "",
	} {
		if got := passwordProblem(pw); (want == "") != (got == "") || (want != "" && !strings.Contains(got, want)) {
			t.Errorf("passwordProblem(%q) = %q, want %q", pw, got, want)
		}
	}
}

func TestSafeNextOnlyAcceptsSitePaths(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/problems/P1": "/problems/P1", "/": "/",
		"//evil.example": "/", "https://evil.example": "/", "/\\evil": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// 表单请求体要封顶。没有这一条，任何人（包括没登录的）都能拿一条 POST 换一次
// ParseForm 与一行原样进日志的超长字段——登录接口的 identifier 就是这么被写进日志的。
func TestFormBodyIsCapped(t *testing.T) {
	h := newHarness(t)
	// 上限是 源码上限 + formHeadroom（测试库里是 1MB + 1MB），所以要报得比它大才叫超限。
	huge := strings.Repeat("a", 3<<20)
	code, body := h.postForm2("/login", url.Values{"identifier": {"admin"}, "password": {huge}}, "", "")
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大表单 = %d，要 413：%s", code, short(body))
	}
	// 正常大小照旧走得通（这里要的是"拒得下一样东西"而不是"把门关上"）。
	if code, _ := h.postForm2("/login", url.Values{"identifier": {"admin"}, "password": {"wrong"}}, "", ""); code == http.StatusRequestEntityTooLarge {
		t.Fatal("正常登录被同一个上限挡住了")
	}
}

// 长登录名会被截断后写进日志与回填进页面：这里能断言的是后者——响应里不能出现
// 那条超长输入的原样。
func TestLoginDoesNotEchoHugeIdentifier(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("z", 5000)
	code, body := h.postForm2("/login", url.Values{"identifier": {long}, "password": {"x"}}, "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("错误口令 = %d，要 401", code)
	}
	if strings.Contains(body, strings.Repeat("z", 400)) {
		t.Error("登录页把超长的登录名原样印回了响应里")
	}
}
