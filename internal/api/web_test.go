package api

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// htmlGet fetches a page the way a browser would.
func (h *harness) htmlGet(path, token string) (int, string) {
	h.t.Helper()
	code, body, _ := h.html(http.MethodGet, path, token, nil, "")
	return code, body
}

// session is a logged-in browser: the cookie it holds plus the CSRF token every
// write form must carry. This is how the suite drives the app now that the
// programmatic surface is gone.
type session struct {
	handle       string
	cookie, csrf string
}

// signIn registers through the web form, so the account has a real password and
// a real session, then grants the listed capabilities through the console.
func (h *harness) signIn(handle string, perms ...string) session {
	h.t.Helper()
	cookie, csrf := h.login(handle, handle+"@example.com", testPassword)
	for _, p := range perms {
		h.grant(handle, p, true)
	}
	return session{handle: handle, cookie: cookie, csrf: csrf}
}

// clearOwner empties the super admin seat. The harness pre-seats the token account
// the way a real deployment does after its first run; a test that wants to watch a
// site being claimed has to take that back first.
func (h *harness) clearOwner() {
	h.t.Helper()
	if _, err := h.db.SQL.ExecContext(ctx(), `DELETE FROM site_owner`); err != nil {
		h.t.Fatal(err)
	}
}

// getAs fetches a page with this session's cookie.
func (h *harness) getAs(s session, path string) (int, string) {
	h.t.Helper()
	return h.htmlWithCookie(http.MethodGet, path, s.cookie)
}

// postAs submits a form as this browser: CSRF included, the 303 followed, so the
// page the person lands on is what the test asserts.
func (h *harness) postAs(s session, path string, values url.Values) (int, string) {
	h.t.Helper()
	return h.postForm2(path, values, s.cookie, s.csrf)
}

// problemValues is what the create form posts: a browser always sends every
// field, including the prefilled limits, so the tests must too.
func problemValues(pid, title string) url.Values {
	return url.Values{"pid": {pid}, "title": {title}, "time_ms": {"1000"}, "mem_mb": {"256"}, "visible": {"1"}}
}

// createProblem authors a problem through the web form as the operator token,
// which is the one credential that is not a browser session.
func (h *harness) createProblem(values url.Values) (int, string) {
	h.t.Helper()
	if values.Get("time_ms") == "" {
		values.Set("time_ms", "1000")
	}
	if values.Get("mem_mb") == "" {
		values.Set("mem_mb", "256")
	}
	if values.Get("visible") == "" {
		values.Set("visible", "1")
	}
	return h.postBearer("/problems", values, h.admin)
}

// submitCode posts the code form on the problem page, the only way to submit now.
func (h *harness) submitCode(s session, pid, lang, source string) (int, string) {
	h.t.Helper()
	return h.postAs(s, "/problems/"+url.PathEscape(pid)+"/submit", url.Values{
		"language": {lang}, "source": {source},
	})
}

// uploadZipAs posts a raw zip body as a signed-in browser. The CSRF token rides
// in its header because the body is the archive, not a form.
func (h *harness) uploadZipAs(s session, path string, archive []byte) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, bytes.NewReader(archive))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", s.cookie)
	req.Header.Set(csrfHeader, s.csrf)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(h.t, resp)
}

// uploadMultipart 按浏览器那个表单的样子发：multipart 请求体 + 隐藏 csrf 字段，
// 不带 X-CSRF-Token 头。Go 1.25 起 ParseForm 不再解析 multipart 请求体，这两条路
// 的解析方式不同，只测 raw zip 那条就会让网页上传悄悄坏掉。
func (h *harness) uploadMultipart(s session, path, csrf string, archive []byte) (int, string) {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("csrf", csrf); err != nil {
		h.t.Fatal(err)
	}
	file, err := mw.CreateFormFile("cases", "cases.zip")
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := file.Write(archive); err != nil {
		h.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, &buf)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", s.cookie)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(h.t, resp)
}

func (h *harness) html(method, path, token string, body []byte, contentType string) (int, string, http.Header) {
	h.t.Helper()
	var raw []byte
	if body != nil {
		raw = body
	}
	var reader *strings.Reader
	if raw != nil {
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := readAll(h.t, resp)
	return resp.StatusCode, out, resp.Header
}

// htmlForm posts an application/x-www-form-urlencoded body, like a browser form.
func (h *harness) htmlForm(path string, values url.Values, cookie string) (int, string, http.Header) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(values.Encode()))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(h.t, resp), resp.Header
}

// postNoRedirect submits a form without following the redirect, so the 303
// target and Set-Cookie headers stay observable.
func (h *harness) postNoRedirect(path string, values url.Values, cookie string) (int, string, http.Header) {
	return h.postForm(path, values, cookie, "")
}

func (h *harness) postForm(path string, values url.Values, cookie, csrf string) (int, string, http.Header) {
	h.t.Helper()
	if csrf != "" {
		values.Set("csrf", csrf)
	}
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(values.Encode()))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(h.t, resp), resp.Header
}

// postBearer submits a form with an Authorization header instead of a cookie,
// which is how an operator drives the console from a script.
func (h *harness) postBearer(path string, values url.Values, token string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(values.Encode()))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(h.t, resp)
}

// postForm2 submits a form the way a browser does: it follows the 303 and hands
// back the page the user lands on, so flash messages stay assertable.
func (h *harness) postForm2(path string, values url.Values, cookie, csrf string) (int, string) {
	h.t.Helper()
	code, body, hdr := h.postForm(path, values, cookie, csrf)
	if code == http.StatusSeeOther {
		if loc := hdr.Get("Location"); loc != "" {
			return h.htmlWithCookie(http.MethodGet, loc, cookie)
		}
	}
	return code, body
}

// cookieValue pulls one cookie out of a Set-Cookie list.
func cookieValue(headers []string, name string) string {
	for _, line := range headers {
		if strings.HasPrefix(line, name+"=") {
			pair := strings.SplitN(line, ";", 2)[0]
			return strings.TrimPrefix(pair, name+"=")
		}
	}
	return ""
}

// extractCSRF reads the token the templates hide inside every form.
func extractCSRF(page string) string { return hiddenValue(page, "csrf") }

// login registers through the web form and returns the session cookie plus the
// CSRF token from a signed-in page. Passwords must clear the policy minimum.
func (h *harness) login(handle, email, password string) (cookie, csrf string) {
	h.t.Helper()
	code, body, hdr := h.postNoRedirect("/register", url.Values{
		"handle": {handle}, "email": {email}, "display_name": {handle + " 同学"}, "password": {password},
	}, "")
	if code != http.StatusSeeOther {
		h.t.Fatalf("register = %d %q", code, body)
	}
	set := hdr.Values("Set-Cookie")
	id := cookieValue(set, "fastoj_session")
	if id == "" {
		h.t.Fatalf("no session cookie in %q", set)
	}
	for _, line := range set {
		if strings.HasPrefix(line, "fastoj_session=") {
			if !strings.Contains(line, "HttpOnly") || !strings.Contains(line, "SameSite=Lax") {
				h.t.Fatalf("session cookie is not HttpOnly+Lax: %q", line)
			}
		}
	}
	cookie = "fastoj_session=" + id
	_, page := h.htmlWithCookie(http.MethodGet, "/settings", cookie)
	if csrf = extractCSRF(page); csrf == "" {
		h.t.Fatal("settings page carried no CSRF token")
	}
	return cookie, csrf
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

const testPassword = "correct-horse-battery"

func TestHomeProblemSearch(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("PUI001")
	code, body := h.htmlGet("/", "")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}
	for _, want := range []string{
		`<form class="home-search" method="get" action="/problems">`,
		`<label for="home-query">查找题目</label>`,
		`id="home-query" type="search" name="q"`,
		`href="/problems/PUI001"`,
		`暂无等待中的提交`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %q", want)
		}
	}
}

func TestSharedNavigationAndResponsiveTables(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("PUI001")
	for _, tc := range []struct{ path, section, page string }{
		{"/", "", "home"},
		{"/problems", "problems", "problems"},
		{"/problems/PUI001", "problems", "problem"},
		{"/contests", "contests", "contests"},
		{"/topics", "topics", "topics"},
		{"/feed", "feed", "feed"},
		{"/rank", "rank", "rank"},
		{"/submissions", "submissions", "submissions"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			code, body := h.htmlGet(tc.path, "")
			if code != http.StatusOK {
				t.Fatalf("GET %s = %d", tc.path, code)
			}
			for _, want := range []string{
				`data-page="` + tc.page + `"`,
				`href="#main-content"`,
				`id="main-content" tabindex="-1"`,
				`href="/` + tc.section + `" aria-current="page"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q", want)
				}
			}
			if strings.Count(body, `aria-current="page"`) != 1 {
				t.Error("navigation must have exactly one active destination")
			}
			if strings.Count(body, "<table") != strings.Count(body, `class="table-scroll"`) {
				t.Error("every data table must have a scroll region")
			}
			if strings.Contains(body, `href="/admin/perms"`) || strings.Contains(body, `href="/settings"`) {
				t.Error("anonymous navigation leaked account controls")
			}
		})
	}
}

func TestProblemListViewForBrowsers(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P4000")
	h.createProblem(hiddenValues("P4001", "隐藏题"))

	code, body := h.htmlGet("/", "")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}
	for _, want := range []string{
		"<!doctype html>", "<th>编号</th>", `/problems/P4000`, "case ready", "题库",
		`href="/static/style.css?v=`, `/static/math.js?v=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list page missing %q", want)
		}
	}
	// KaTeX is ~300 KB and this page has no formula on it, so the page must not
	// reference the CDN at all: math.js pulls it only when a math node exists.
	if strings.Contains(body, "katex") {
		t.Error("a formula-free page still loaded the KaTeX CDN")
	}
	if code, math := h.htmlGet("/static/math.js", ""); code != http.StatusOK || !strings.Contains(math, "katex@0.16.11") {
		t.Errorf("math.js lost the on-demand loader (%d)", code)
	}
	// A hidden problem is invisible to an anonymous browser, visible to admin.
	if strings.Contains(body, "P4001") {
		t.Error("hidden problem leaked into the anonymous list")
	}
	if _, mine := h.htmlGet("/", h.admin); !strings.Contains(mine, "P4001") {
		t.Error("admin browser list lost the hidden problem")
	}
}

func TestProblemDetailViewRendersMarkdownAndMath(t *testing.T) {
	h := newHarness(t)
	fence := "```"
	statement := `# 描述

输入 $a, b$，输出 $a+b$。平均值写成块级公式：

$$ \frac{a+b}{2} $$

` + fence + "cpp\nint main(){}\n" + fence + `

<script>alert(1)</script>

[坑](javascript:alert(2))
`
	if code, body := h.createProblem(url.Values{"pid": {"P4002"}, "title": {"公式题"}, "statement": {statement}}); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, body)
	}
	h.uploadCases("P4002")

	code, body := h.htmlGet("/problems/P4002", "")
	if code != http.StatusOK {
		t.Fatalf("detail = %d", code)
	}
	for _, want := range []string{
		"<h1>描述</h1>",
		`<span class="math inline">\(a, b\)</span>`,
		`<span class="math display">\[ \frac{a+b}{2} \]</span>`,
		`<pre><code class="language-cpp">`,
		"样例", "1 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q\n%s", want, body)
		}
	}
	// Scope the injection check to the statement block: the page chrome itself
	// legitimately carries <script> tags for KaTeX.
	start, end := strings.Index(body, "<article>"), strings.Index(body, "</article>")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("no statement block found in:\n%s", body)
	}
	article := body[start:end]
	for _, banned := range []string{"<script", "</script>", "javascript:alert", "onerror", "<img"} {
		if strings.Contains(article, banned) {
			t.Errorf("statement leaked %q:\n%s", banned, article)
		}
	}
	// uploadCases puts "4 5" in judged case 2; it must not reach the page.
	if strings.Contains(body, "4 5") {
		t.Errorf("judged case leaked: %s", body)
	}
	// Without a bound identity the form is replaced by a pointer to /account.
	if strings.Contains(body, `<form method="post" action="/problems/P4002/submit"`) {
		t.Error("submit form shown to an anonymous visitor")
	}
	bound, _ := h.login("webuser", "webuser@example.com", testPassword)
	code, page := h.htmlWithCookie(http.MethodGet, "/problems/P4002", bound)
	if code != http.StatusOK || !strings.Contains(page, `<textarea name="source"`) {
		t.Fatalf("bound browser should see the editor: %d", code)
	}
	if !strings.Contains(page, "webuser") {
		t.Errorf("nav lost the bound handle: %s", page)
	}
	// The page it lands on is the same one a browser gets, form and all.
	if _, page := h.htmlGet("/problems/P4002", h.admin); !strings.Contains(page, "<textarea") {
		t.Error("the problem page lost its editor for someone who may edit it")
	}
}

func (h *harness) htmlWithCookie(method, path, cookie string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(""))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", cookie)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(h.t, resp)
}

func TestWebSubmitEndToEndThroughTheForm(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P4003")
	cookie, csrf := h.login("solver", "solver@example.com", testPassword)

	source := "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b;}\n"
	code, body, hdr := h.postForm("/problems/P4003/submit",
		url.Values{"language": {"cpp"}, "source": {source}}, cookie, csrf)
	if code != http.StatusSeeOther {
		t.Fatalf("submit = %d %q", code, body)
	}
	loc := hdr.Get("Location")
	if loc != "/submissions/1" {
		t.Fatalf("redirect target = %q", loc)
	}

	// The queued row is readable as a page.
	got, page := h.htmlWithCookie(http.MethodGet, loc, cookie)
	if got != http.StatusOK || !strings.Contains(page, "提交 #1") || !strings.Contains(page, "排队中") {
		t.Fatalf("submission page = %d %s", got, page)
	}
	// The source is HTML-escaped on the page, so match an inert fragment plus the
	// escaped form of the include line.
	if !strings.Contains(page, "int main()") || !strings.Contains(page, "&lt;iostream&gt;") {
		t.Errorf("owner cannot see their own source:\n%s", page)
	}

	// The judge box finishes it and the page shows the verdict.
	h.finishSubmission(1, store.Result{Verdict: "AC", TimeMS: 12, MemKB: 3072, Score: 2, TotalScore: 2},
		[]store.Run{{Ordinal: 2, CaseName: "1", Verdict: "AC", TimeMS: 12, MemKB: 3072}})
	_, judged := h.htmlWithCookie(http.MethodGet, loc, cookie)
	for _, want := range []string{"badge AC", "通过", "得分", "12ms", "逐用例"} {
		if !strings.Contains(judged, want) {
			t.Errorf("judged page missing %q\n%s", want, judged)
		}
	}

	// A stranger browsing the same page gets no source and no case table — but
	// the notice that explains why is allowed to mention them.
	_, anon := h.htmlGet(loc, "")
	if strings.Contains(anon, "std::cin") || strings.Contains(anon, "<th>类型</th>") {
		t.Errorf("anonymous visitor saw private detail:\n%s", anon)
	}
	if !strings.Contains(anon, "badge AC") {
		t.Error("verdict itself should stay public")
	}
	if !strings.Contains(anon, "只对本人、出题人和管理员可见") {
		t.Error("anonymous page should explain the restriction")
	}

	// Anonymous form posts bounce to the login page.
	code, _, hdr = h.postNoRedirect("/problems/P4003/submit",
		url.Values{"language": {"cpp"}, "source": {"x"}}, "")
	anonLoc := hdr.Get("Location")
	if code != http.StatusSeeOther || !strings.HasPrefix(anonLoc, "/login?") || !strings.Contains(anonLoc, "msg=") {
		t.Fatalf("anonymous form = %d %q", code, anonLoc)
	}
}

// finishSubmission plays the judge side: claim, then record a verdict.
func (h *harness) finishSubmission(id int64, res store.Result, runs []store.Run) {
	h.t.Helper()
	if _, err := h.db.UpsertJudge(ctx(), "box-web", []string{"cpp"}, nil, "", nil); err != nil {
		h.t.Fatal(err)
	}
	if ok, err := h.db.SetStatus(ctx(), id, "queued", "compiling", 1); err != nil || !ok {
		h.t.Fatalf("claim = %v %v", ok, err)
	}
	if err := h.db.RecordResult(ctx(), id, res, runs); err != nil {
		h.t.Fatal(err)
	}
}

func TestWebFormValidationMirrorsTheAPI(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P4004")
	h.createProblem(url.Values{"pid": {"P4005"}, "title": {"no cases"}})
	cookie, csrf := h.login("checker", "checker@example.com", testPassword)

	cases := []struct{ pid, lang, source string }{
		{"P4004", "pascal", "int main(){}"}, // language not accepted
		{"P4005", "cpp", "int main(){}"},    // no judged cases yet
		{"P4004", "", "int main(){}"},       // missing language
		{"P4004", "cpp", "   "},             // empty source
		{"MISSING", "cpp", "int main(){}"},  // unknown problem
	}
	for _, c := range cases {
		code, _, hdr := h.postForm("/problems/"+c.pid+"/submit",
			url.Values{"language": {c.lang}, "source": {c.source}}, cookie, csrf)
		if code != http.StatusSeeOther {
			t.Fatalf("%+v = %d", c, code)
		}
		target, err := url.QueryUnescape(hdr.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(target, "/problems/"+c.pid+"?msg=") {
			t.Fatalf("%+v bounced to %q", c, target)
		}
		if strings.Contains(target, "还没有判题用例") == false && c.pid == "P4005" {
			t.Fatalf("empty problem message lost: %q", target)
		}
	}

	// Nothing was created.
	if _, list := h.htmlGet("/submissions", ""); !strings.Contains(list, "还没有提交") {
		t.Fatalf("rejected forms created rows:\n%s", list)
	}
}

func TestUnrestrictedProblemFormStillOffersLanguages(t *testing.T) {
	h := newHarness(t)
	h.createProblem(url.Values{"pid": {"P4010"}, "title": {"语言不限"}})
	h.uploadCases("P4010")
	cookie, csrf := h.login("opener", "opener@example.com", testPassword)

	_, page := h.htmlWithCookie(http.MethodGet, "/problems/P4010", cookie)
	for _, want := range []string{`<option value="c">`, `<option value="cpp">`, "语言 <b>不限</b>"} {
		if !strings.Contains(page, want) {
			t.Errorf("form missing %q:\n%s", want, page)
		}
	}
	// And the form actually works for an unrestricted problem.
	code, _, hdr := h.postForm("/problems/P4010/submit",
		url.Values{"language": {"Cpp"}, "source": {"int main(){}"}}, cookie, csrf)
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/submissions/") {
		t.Fatalf("submit to unrestricted problem = %d %q", code, hdr.Get("Location"))
	}
}

func TestRankAndUserPages(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P4006")
	champion := h.signIn("champion")

	h.submitCode(champion, "P4006", "cpp", "int main(){}")
	h.submitCode(champion, "P4006", "cpp", "int main(){return 1;}")
	h.finishSubmission(1, store.Result{Verdict: "AC", TimeMS: 9, MemKB: 2048, Score: 2, TotalScore: 2}, nil)
	h.finishSubmission(2, store.Result{Verdict: "RE", TimeMS: 5, MemKB: 2048, Score: 0, TotalScore: 2, FailedCase: "1"}, nil)

	code, body := h.htmlGet("/rank", "")
	if code != http.StatusOK || !strings.Contains(body, "排行榜") || !strings.Contains(body, "champion") {
		t.Fatalf("rank page = %d %s", code, body)
	}
	if !strings.Contains(body, "<td class=\"num\">1</td>") {
		t.Errorf("rank number missing:\n%s", body)
	}

	code, body = h.htmlGet("/u/champion", "")
	if code != http.StatusOK {
		t.Fatalf("user page = %d", code)
	}
	for _, want := range []string{"champion", "已解出题", "判定分布", "P4006", "AC", "RE"} {
		if !strings.Contains(body, want) {
			t.Errorf("user page missing %q", want)
		}
	}
	if code, _ := h.htmlGet("/u/nobody", ""); code != http.StatusNotFound {
		t.Fatalf("unknown user = %d", code)
	}
	// An anonymous profile page still works (no private data on it).
	if code, _ := h.htmlGet("/u/champion", ""); code != http.StatusOK {
		t.Fatalf("anon profile = %d", code)
	}
}

func TestHTMLNotFoundAndStaticAssets(t *testing.T) {
	h := newHarness(t)

	code, body := h.htmlGet("/definitely/not/here", "")
	if code != http.StatusNotFound {
		t.Fatalf("404 = %d", code)
	}
	if !strings.Contains(body, "404 Not Found") || !strings.Contains(body, "/definitely/not/here") {
		t.Errorf("error page content: %s", body)
	}

	for path, want := range map[string]string{
		"/static/style.css": "body {",
		"/static/math.js":   "span.math",
	} {
		code, body := h.htmlGet(path, "")
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("%s = %d", path, code)
		}
	}
	if code, _ := h.htmlGet("/static/style.css.exe", ""); code != http.StatusNotFound {
		t.Fatalf("odd asset suffix = %d", code)
	}
	// Only the two known files are served; no directory listing or traversal.
	if code, _ := h.htmlGet("/static/", ""); code != http.StatusNotFound {
		t.Fatalf("static root = %d", code)
	}
}

func TestSettingsSavesAndLogout(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("wallet", "wallet@example.com", testPassword)

	// Anonymous visitors never see someone else's settings.
	if _, anonPage := h.htmlGet("/settings", ""); strings.Contains(anonPage, "wallet@example.com") {
		t.Fatal("anonymous visitor reached the settings page")
	}
	code, mine := h.htmlWithCookie(http.MethodGet, "/settings", cookie)
	if code != http.StatusOK || !strings.Contains(mine, "wallet@example.com") {
		t.Fatalf("settings = %d", code)
	}
	if !strings.Contains(mine, "wallet 同学") {
		t.Errorf("nickname lost from the page:\n%s", mine)
	}

	// A cookie-authenticated write without the CSRF token is refused.
	if code, _, _ := h.postNoRedirect("/settings/profile", url.Values{"display_name": {"没令牌"}}, cookie); code != http.StatusForbidden {
		t.Fatalf("CSRF-less profile save = %d, want 403", code)
	}

	// Profile edits stick.
	if code, _, _ := h.postForm("/settings/profile",
		url.Values{"display_name": {"钱包"}, "bio": {"刷题的人"}}, cookie, csrf); code != http.StatusSeeOther {
		t.Fatalf("profile save = %d", code)
	}
	_, after := h.htmlWithCookie(http.MethodGet, "/settings", cookie)
	if !strings.Contains(after, "钱包") || !strings.Contains(after, "刷题的人") {
		t.Errorf("profile not saved:\n%s", after)
	}
	// The nav bar shows the new nickname everywhere.
	if _, list := h.htmlWithCookie(http.MethodGet, "/problems", cookie); !strings.Contains(list, "钱包") {
		t.Error("nav still shows the old name")
	}

	// Logout drops the session server-side, not just the cookie.
	code, _, hdr := h.postForm("/logout", url.Values{}, cookie, csrf)
	if code != http.StatusSeeOther {
		t.Fatalf("logout = %d", code)
	}
	if cleared := hdr.Get("Set-Cookie"); !strings.Contains(cleared, "fastoj_session=;") {
		t.Fatalf("session cookie not cleared: %q", cleared)
	}
	if _, page := h.htmlWithCookie(http.MethodGet, "/settings", cookie); strings.Contains(page, "wallet@example.com") {
		t.Error("the old session still authenticates")
	}
}

// hiddenValue pulls the value of a hidden input by name.
func hiddenValue(page, name string) string {
	marker := `name="` + name + `" value="`
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}
	rest := page[i+len(marker):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// plaintextToken lifts the one-time token out of the settings banner.
func plaintextToken(page string) string {
	i := strings.Index(page, "<code>")
	if i < 0 {
		return ""
	}
	rest := page[i+len("<code>"):]
	j := strings.Index(rest, "</code>")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func TestSubmissionListViewForBrowsers(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P4007")
	watcher := h.signIn("watcher")
	for i := 0; i < 3; i++ {
		h.submitCode(watcher, "P4007", "cpp", fmt.Sprintf("int main(){return %d;}", i))
	}
	code, body := h.htmlGet("/submissions", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	for _, want := range []string{"提交记录", "P4007", "watcher", "/submissions/3", "显示 3 条"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	_, filtered := h.htmlGet("/submissions?user=watcher&problem=P4007", "")
	if !strings.Contains(filtered, "watcher") {
		t.Errorf("filter lost: %s", filtered)
	}
	_, unknown := h.htmlGet("/submissions?user=ghost", "")
	if !strings.Contains(unknown, "404") {
		t.Errorf("unknown user filter should 404:\n%s", unknown)
	}
}

// 个人主页那一行行提交要带得分：部分分的题只写"未通过"就看不出这题拿了 40 还是 0，
// 而这正是学生会拿去问人的那个数。列与 /submissions 上的一致。
func TestUserPageShowsSubmissionScore(t *testing.T) {
	h := newHarness(t)
	student := h.signIn("stu2110")
	h.readyProblem("P2110")
	if code, body := h.submitCode(student, "P2110", "cpp", "int main(){}"); code != http.StatusOK {
		t.Fatalf("提交 = %d：\n%s", code, short(body))
	}
	_, page := h.getAs(student, "/u/stu2110")
	if !strings.Contains(page, "得分") {
		t.Errorf("最近提交没有得分列：\n%s", short(page))
	}
	// 那一格填的是库里那条提交的 得分/总分——照库里读出来的数断言，不猜这道题几分。
	p, err := h.db.FindProblem(ctx(), "P2110")
	if err != nil {
		t.Fatal(err)
	}
	list, err := h.db.ListSubmissions(ctx(), store.SubmissionFilter{ProblemID: p.ID, Limit: 5})
	if err != nil || len(list) == 0 {
		t.Fatalf("提交没写进库：%d 条, %v", len(list), err)
	}
	want := fmt.Sprintf("%d/%d", list[0].Score, list[0].TotalScore)
	if !strings.Contains(page, want) {
		t.Errorf("最近提交那一格的得分应当写着 %s：\n%s", want, short(page))
	}
}

// 提交与建题都要限流：判机是共享资源，队列长度是所有人的等待时间。生产默认不是零，
// 但按人而不是按 IP——一个机房 NAT 出一个地址是常态。
func TestSubmitIsRateLimitedPerUser(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) { c.SubmitPerMinute = 3 })
	student := h.signIn("stu2120")
	h.readyProblem("P2120")
	for i := 0; i < 3; i++ {
		if code, body := h.submitCode(student, "P2120", "cpp", "int main(){}"); code != http.StatusOK {
			t.Fatalf("第 %d 次提交 = %d：\n%s", i+1, code, short(body))
		}
	}
	code, body := h.submitCode(student, "P2120", "cpp", "int main(){}")
	if code != http.StatusTooManyRequests {
		t.Fatalf("第 4 次提交 = %d，要 429：\n%s", code, short(body))
	}
	if !strings.Contains(body, "提交太快") {
		t.Errorf("被限住却没说清原因：\n%s", short(body))
	}
	// 另一个人不受影响：限的是这一个人，不是这道题、不是这台机器。
	other := h.signIn("stu2121")
	if code, body := h.submitCode(other, "P2120", "cpp", "int main(){}"); code != http.StatusOK {
		t.Fatalf("别人提交 = %d：\n%s", code, short(body))
	}
}

func TestProblemCreateIsRateLimitedPerUser(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) { c.ProblemPerMinute = 2 })
	for i := 0; i < 2; i++ {
		if code, body := h.createProblem(url.Values{
			"pid": {fmt.Sprintf("P212%d", i)}, "title": {"建题限流"}, "languages": {"cpp"},
		}); code != http.StatusOK {
			t.Fatalf("建第 %d 题 = %d：\n%s", i+1, code, short(body))
		}
	}
	code, body := h.createProblem(url.Values{"pid": {"P2129"}, "title": {"第三题"}, "languages": {"cpp"}})
	if code != http.StatusTooManyRequests {
		t.Fatalf("第三次建题 = %d，要 429：\n%s", code, short(body))
	}
	if !strings.Contains(body, "建题太快") {
		t.Errorf("被限住却没说清原因：\n%s", short(body))
	}
}

// 限流关掉时（0=不限）不能留任何痕迹：赶时间的人不该被一个他不知道存在的闸门挡住。
func TestRateLimitsCanBeTurnedOff(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) { c.SubmitPerMinute = 0 })
	student := h.signIn("stu2123")
	h.readyProblem("P2123")
	for i := 0; i < 12; i++ {
		if code, body := h.submitCode(student, "P2123", "cpp", "int main(){}"); code != http.StatusOK {
			t.Fatalf("关掉限流后第 %d 次提交 = %d：\n%s", i+1, code, short(body))
		}
	}
}
