package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

func TestSiteSettingsConsoleNeedsTheRight(t *testing.T) {
	h := newHarness(t)
	cookie, _ := h.login("viewer", "viewer@example.com", testPassword)

	if code, _ := h.htmlWithCookie(http.MethodGet, "/admin/site", cookie); code != http.StatusForbidden {
		t.Fatalf("plain user reached site settings: %d", code)
	}
	if code, _ := h.htmlGet("/admin/site", h.adminToken()); code != http.StatusOK {
		t.Fatalf("admin could not reach site settings: %d", code)
	}

	// The console follows the capability, not the role label: someone granted only
	// site.settings gets both the page and the nav link.
	delegate, _ := h.login("siteself", "siteself@example.com", testPassword)
	h.grant("siteself", permSiteSettings, true)
	if code, page := h.htmlWithCookie(http.MethodGet, "/admin/site", delegate); code != http.StatusOK ||
		!strings.Contains(page, "站点设置") {
		t.Fatalf("delegate console = %d", code)
	}
	if _, page := h.htmlWithCookie(http.MethodGet, "/", delegate); !strings.Contains(page, `/admin/site">站点`) {
		t.Error("delegate did not get the nav link")
	}
}

func TestSiteSettingsRoundTripAndCache(t *testing.T) {
	h := newHarness(t)

	if code, body := h.htmlGet("/", ""); code != http.StatusOK || !strings.Contains(body, "fast oj") {
		t.Fatalf("default home = %d", code)
	}

	code, out := h.postBearer("/admin/site", url.Values{
		"title": {"我的 OJ"}, "tagline": {"# 欢迎\n\n公式 $E=mc^2$ 也支持。"},
		"links":  {"洛谷 | https://luogu.com.cn\nCSDN | https://blog.csdn.net"},
		"footer": {"出题请走管理台"}, "allow_register": {"1"},
	}, h.adminToken())
	if code != http.StatusOK || !strings.Contains(out, "已保存") {
		t.Fatalf("save = %d %s", code, out)
	}

	_, home := h.htmlGet("/", "")
	for _, want := range []string{
		"我的 OJ",
		"<h1>欢迎</h1>",
		`<span class="math inline">\(E=mc^2\)</span>`,
		`href="https://luogu.com.cn"`, "洛谷", "友情链接", "出题请走管理台",
	} {
		if !strings.Contains(home, want) {
			t.Errorf("home missing %q:\n%s", want, home)
		}
	}
	if strings.Contains(home, ">fast-oj<") {
		t.Errorf("stale title still rendered:\n%s", home)
	}

	site, err := h.db.Site(ctx())
	if err != nil || site.Title != "我的 OJ" || len(site.Links) != 2 || !site.AllowRegister {
		t.Fatalf("stored config = %+v %v", site, err)
	}
}

func TestSiteAPIHintIsEditable(t *testing.T) {
	h := newHarness(t)

	// 默认不再宣传纯文本 API 与 ?format=json：那两个入口早就撤掉了，留着这句就是让每个
	// 新站点在页脚里教人去 curl 一个不存在的东西。空值意味着这一整行不出现。
	if _, home := h.htmlGet("/", ""); strings.Contains(home, "format=json") || strings.Contains(home, "curl -s") {
		t.Errorf("页脚还在宣传已经不存在的 API：\n%s", short(home))
	}

	code, out := h.postBearer("/admin/site", url.Values{
		"title": {"我的 OJ"}, "allow_register": {"1"},
		"api_hint": {"健康检查：`curl -s localhost:18080/healthz`"},
	}, h.adminToken())
	if code != http.StatusOK || !strings.Contains(out, "已保存") {
		t.Fatalf("save = %d %s", code, out)
	}
	if _, home := h.htmlGet("/", ""); !strings.Contains(home, "<code>curl -s localhost:18080/healthz</code>") {
		t.Errorf("edited hint not rendered:\n%s", home)
	}

	// Clearing it drops the whole line, not just its text.
	if _, out := h.postBearer("/admin/site", url.Values{"title": {"我的 OJ"}, "allow_register": {"1"}}, h.adminToken()); !strings.Contains(out, "已保存") {
		t.Fatal("second save failed")
	}
	if _, home := h.htmlGet("/", ""); strings.Contains(home, "健康检查") {
		t.Errorf("cleared hint still rendered:\n%s", home)
	}
}

func TestSiteSettingsRejectsBadInput(t *testing.T) {
	h := newHarness(t)

	cases := map[string]url.Values{
		"empty title":  {"title": {""}, "links": {""}},
		"no separator": {"title": {"ok"}, "links": {"没有竖线"}},
		"js link":      {"title": {"ok"}, "links": {"点我 | javascript:alert(1)"}},
		"data link":    {"title": {"ok"}, "links": {"x | data:text/html,hi"}},
		"too many":     {"title": {"ok"}, "links": {strings.Repeat("a | https://a.b\n", 21)}},
	}
	for name, values := range cases {
		code, body := h.postBearer("/admin/site", values, h.adminToken())
		if code != http.StatusBadRequest {
			t.Errorf("%s: accepted with %d", name, code)
		}
		if title := values.Get("title"); title != "" && !strings.Contains(body, title) {
			t.Errorf("%s: the form lost what was typed:\n%s", name, body)
		}
	}
	// Defaults survived the rejected saves.
	site, err := h.db.Site(ctx())
	if err != nil || site.Title != "fast oj" {
		t.Fatalf("site config after rejects = %+v %v", site, err)
	}
}

func TestSiteSettingsCountsLimitsInCharacters(t *testing.T) {
	h := newHarness(t)

	long := strings.Repeat("极速判题题库", 21)
	if code, body := h.postBearer("/admin/site", url.Values{"title": {long}, "links": {""}}, h.adminToken()); code != http.StatusBadRequest {
		t.Fatalf("oversized title = %d %s", code, body)
	}

	// 60 Chinese characters are 180 bytes; the form's maxlength counts characters,
	// so a byte limit would refuse input the browser allowed.
	ok := strings.Repeat("题", 60)
	if code, body := h.postBearer("/admin/site", url.Values{"title": {ok}, "links": {""}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("60-character title = %d %s", code, body)
	}
	site, err := h.db.Site(ctx())
	if err != nil || site.Title != ok {
		t.Fatalf("stored title = %q %v", site.Title, err)
	}
	if _, page := h.htmlGet("/", ""); !strings.Contains(page, ok) {
		t.Error("home did not render the Chinese title")
	}
}

func TestRegistrationSwitchInSettings(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.htmlGet("/register", ""); code != http.StatusOK {
		t.Fatalf("register page = %d", code)
	}

	// The console path is what invalidates the cache, so use it like an operator.
	if code, _ := h.postBearer("/admin/site", url.Values{
		"title": {"fast oj"}, "links": {""}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("save without the checkbox = %d", code)
	}
	site, err := h.db.Site(ctx())
	if err != nil || site.AllowRegister {
		t.Fatalf("allow_register = %v %v, want false", site.AllowRegister, err)
	}

	if code, _ := h.htmlGet("/register", ""); code != http.StatusForbidden {
		t.Fatalf("register page after closing = %d", code)
	}
	if code, _ := h.formPage("/register", url.Values{
		"handle": {"late"}, "email": {"late@example.com"}, "password": {testPassword}}); code != http.StatusForbidden {
		t.Fatalf("register post after closing = %d", code)
	}
	if _, home := h.htmlGet("/", ""); !strings.Contains(home, "已关闭注册") {
		t.Error("home did not mention the closed registration")
	}
}

func TestFeedPosting(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("poster", "poster@example.com", testPassword)

	_, anon := h.htmlGet("/feed", "")
	if strings.Contains(anon, `name="body"`) {
		t.Error("anonymous visitor saw the composer")
	}
	if !strings.Contains(anon, "就能发动态") {
		t.Errorf("missing login hint:\n%s", anon)
	}
	if code, _, hdr := h.postNoRedirect("/feed", url.Values{"body": {"x"}}, ""); code != http.StatusSeeOther ||
		!strings.Contains(hdr.Get("Location"), "/login") {
		t.Fatalf("anonymous post = %d %q", code, hdr.Get("Location"))
	}

	// A cookie-authenticated post still needs its CSRF token.
	if code, _, _ := h.postForm("/feed", url.Values{"body": {"占位"}}, cookie, ""); code != http.StatusForbidden {
		t.Fatalf("CSRF-less post = %d, want 403", code)
	}

	code, body := h.postForm2("/feed", url.Values{
		"body": {"今天调了一道 $a+b$ 的题\n\n<script>alert(1)</script>\n\n[b](javascript:alert(2))"},
	}, cookie, csrf)
	if code != http.StatusOK {
		t.Fatalf("post = %d %s", code, body)
	}

	_, feed := h.htmlGet("/feed", "")
	for _, want := range []string{"今天调了一道", `<span class="math inline">\(a+b\)</span>`, "poster", "<p>"} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed missing %q:\n%s", want, feed)
		}
	}
	if strings.Contains(feed, "<script>alert") || strings.Contains(feed, "javascript:alert") {
		t.Errorf("feed leaked live markup:\n%s", feed)
	}
}

// 犇犇的隐藏：一条动态默认只占一行——谁、什么时候发的、一个"展开"。正文要点开才出现，
// 出现之后也是在一个最多 500px 的盒子里滚动：一张长图不该把整页顶走。
func TestFeedCollapsesToOneLine(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("folder", "folder@example.com", testPassword)
	if code, _ := h.postForm2("/feed", url.Values{"body": {"很长的一段正文"}}, cookie, csrf); code != http.StatusOK {
		t.Fatalf("发动态 = %d", code)
	}

	_, page := h.htmlGet("/feed", "")
	if strings.Contains(page, `<details class="fold" open`) {
		t.Error("动态默认就该是折着的")
	}
	for _, want := range []string{`<details class="fold">`, "<summary>", "展开", "很长的一段正文"} {
		if !strings.Contains(page, want) {
			t.Errorf("动态页缺了 %q：\n%s", want, short(page))
		}
	}
	// 正文必须在折叠块里面，否则浏览器折叠状态也照样把它画出来。
	fold := page[strings.Index(page, `<details class="fold">`):]
	if end := strings.Index(fold, "</details>"); end < 0 || !strings.Contains(fold[:end], "很长的一段正文") {
		t.Errorf("正文不在折叠块里：\n%s", short(fold))
	}

	css := h.stylesheet(page)
	for _, want := range []string{"max-height: 500px", "overflow-y: auto", "display: contents"} {
		if !strings.Contains(css, want) {
			t.Errorf("样式表缺了 %q，长图就拦不住", want)
		}
	}
}

// stylesheet 取出页面正在用的那份样式表：静态文件带着内容哈希，断言它而不是断言仓库里的
// 文件，才说明页面真的加载了这一条规则。
func (h *harness) stylesheet(page string) string {
	h.t.Helper()
	i := strings.Index(page, "/static/style.css?v=")
	if i < 0 {
		h.t.Fatal("页面没有引用样式表")
	}
	rest := page[i:]
	if end := strings.IndexByte(rest, '"'); end > 0 {
		rest = rest[:end]
	}
	code, css := h.htmlGet(rest, "")
	if code != http.StatusOK {
		h.t.Fatalf("样式表 %s = %d", rest, code)
	}
	return css
}

func TestFeedCursorOnlyOffersMoreWhenFull(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("cursor9", "cursor9@example.com", testPassword)
	for i := 0; i < 3; i++ {
		h.postForm2("/feed", url.Values{"body": {"第 " + strconv.Itoa(i) + " 条"}}, cookie, csrf)
	}
	if _, page := h.htmlGet("/feed", ""); strings.Contains(page, "更早的动态") {
		t.Error("a short page still offered the next cursor")
	}
	code, page := h.htmlGet("/feed?limit=2", "")
	if code != http.StatusOK || !strings.Contains(page, "更早的动态 →") {
		t.Errorf("a full page did not offer a cursor: %d", code)
	}
}

func TestFeedValidation(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("editor", "editor@example.com", testPassword)

	if _, body := h.postForm2("/feed", url.Values{"body": {"   "}}, cookie, csrf); !strings.Contains(body, "说点什么") {
		t.Errorf("empty post message lost:\n%s", body)
	}
	if _, body := h.postForm2("/feed",
		url.Values{"body": {strings.Repeat("长", store.MaxPostBody+10)}}, cookie, csrf); !strings.Contains(body, "太长") {
		t.Errorf("oversized post message lost")
	}
}

func TestFeedRateLimit(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("ratey", "ratey@example.com", testPassword)

	for i := 0; i < maxPostsHour; i++ {
		if code, body := h.postForm2("/feed", url.Values{"body": {"灌水 " + strconv.Itoa(i)}}, cookie, csrf); code != http.StatusOK {
			t.Fatalf("post %d = %d %s", i+1, code, body)
		}
	}
	if _, blocked := h.postForm2("/feed", url.Values{"body": {"再灌一条"}}, cookie, csrf); !strings.Contains(blocked, "发得太快") {
		t.Errorf("rate limit not applied:\n%s", blocked)
	}
	// A different account is unaffected.
	otherCookie, otherCSRF := h.login("fresh", "fresh@example.com", testPassword)
	if code, _ := h.postForm2("/feed", url.Values{"body": {"我的第一条"}}, otherCookie, otherCSRF); code != http.StatusOK {
		t.Fatalf("other account blocked: %d", code)
	}
}

func TestFeedModerationAndOwnership(t *testing.T) {
	h := newHarness(t)
	authorCookie, authorCSRF := h.login("author8", "author8@example.com", testPassword)
	if code, body := h.postForm2("/feed", url.Values{"body": {"要被删掉的内容"}}, authorCookie, authorCSRF); code != http.StatusOK {
		t.Fatalf("post = %d %s", code, body)
	}
	post, err := h.db.GetPost(ctx(), 1)
	if err != nil {
		t.Fatal(err)
	}
	path := "/feed/" + strconv.FormatInt(post.ID, 10)

	otherCookie, otherCSRF := h.login("other8", "other8@example.com", testPassword)
	if code, _ := h.postForm2(path+"/delete", url.Values{}, otherCookie, otherCSRF); code != http.StatusNotFound {
		t.Fatalf("non-author delete = %d", code)
	}
	if code, _ := h.postForm2(path+"/hide", url.Values{"hidden": {"1"}}, otherCookie, otherCSRF); code != http.StatusForbidden {
		t.Fatalf("non-moderator hide = %d", code)
	}

	h.grant("other8", permFeedModerate, true)
	if code, _ := h.postForm2(path+"/hide", url.Values{"hidden": {"1"}}, otherCookie, otherCSRF); code != http.StatusOK {
		t.Fatalf("moderator hide = %d", code)
	}

	// Hidden: gone from public views, still visible to the moderator (to undo)
	// and to the author.
	if _, feed := h.htmlGet("/feed", ""); strings.Contains(feed, "要被删掉的内容") {
		t.Error("hidden post still in the public timeline")
	}
	if _, home := h.htmlGet("/", ""); strings.Contains(home, "要被删掉的内容") {
		t.Error("hidden post still on the home page")
	}
	if _, modView := h.htmlWithCookie(http.MethodGet, "/feed", otherCookie); !strings.Contains(modView, "要被删掉的内容") {
		t.Error("moderator cannot see what they hid")
	}
	if _, own := h.htmlWithCookie(http.MethodGet, "/u/author8", authorCookie); !strings.Contains(own, "已隐藏") {
		t.Error("author cannot see that their post was hidden")
	}

	if code, _ := h.postForm2(path+"/hide", url.Values{"hidden": {"0"}}, otherCookie, otherCSRF); code != http.StatusOK {
		t.Fatalf("restore = %d", code)
	}
	if _, feed := h.htmlGet("/feed", ""); !strings.Contains(feed, "要被删掉的内容") {
		t.Error("restored post did not come back")
	}
	if code, _ := h.postForm2(path+"/delete", url.Values{}, authorCookie, authorCSRF); code != http.StatusOK {
		t.Fatalf("author delete = %d", code)
	}
	if _, err := h.db.GetPost(ctx(), post.ID); err == nil {
		t.Fatal("post still in the database")
	}
	// Hiding twice in a row is a no-op the handler reports as a conflict.
	if code, body := h.postForm2("/feed", url.Values{"body": {"第二条"}}, authorCookie, authorCSRF); code != http.StatusOK {
		t.Fatalf("second post = %d %s", code, body)
	}
	mine, err := h.db.ListPosts(ctx(), 1, 0, h.user("author8").ID, true)
	if err != nil || len(mine) != 1 {
		t.Fatalf("author timeline = %+v %v", mine, err)
	}
	p2 := "/feed/" + strconv.FormatInt(mine[0].ID, 10)
	if code, _ := h.postForm2(p2+"/hide", url.Values{"hidden": {"1"}}, otherCookie, otherCSRF); code != http.StatusOK {
		t.Fatalf("first hide = %d", code)
	}
	if code, _ := h.postForm2(p2+"/hide", url.Values{"hidden": {"1"}}, otherCookie, otherCSRF); code != http.StatusConflict {
		t.Fatalf("double hide = %d, want 409", code)
	}
}

func TestProfileShowsBioAndPosts(t *testing.T) {
	h := newHarness(t)
	cookie, csrf := h.login("bioed", "bioed@example.com", testPassword)

	if code, _, _ := h.postForm("/settings/profile",
		url.Values{"display_name": {"带带"}, "bio": {"# 关于我\n\n喜欢 $O(n\\log n)$ 的题。"}}, cookie, csrf); code != http.StatusSeeOther {
		t.Fatalf("profile save = %d", code)
	}
	h.postForm2("/feed", url.Values{"body": {"第一条动态"}}, cookie, csrf)

	_, page := h.htmlGet("/u/bioed", "")
	for _, want := range []string{"带带", "@bioed", "<h1>关于我</h1>", `<span class="math inline">\(O(n\log n)\)</span>`, "第一条动态", "TA 的动态"} {
		if !strings.Contains(page, want) {
			t.Errorf("profile missing %q:\n%s", want, page)
		}
	}
	if code, body := h.htmlGet("/u/bioed", ""); code != http.StatusOK || !strings.Contains(body, "bioed") {
		t.Fatalf("json profile = %d %s", code, body)
	}
}
