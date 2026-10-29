package api

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 配色是"每个人自己的样子"：跟着账号走、别人看不见、匿名访客拿到的是站点默认，
// 而且这一串文本要进 <html style>——所以这几条用例同时盯着独立性与"脏值进不来"。

var htmlOpenRe = regexp.MustCompile(`<html[^>]*>`)

func TestThemeDefaultsMatchStylesheet(t *testing.T) {
	css, err := assetFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pattern string
		index   int
	}{
		{`(?s):root \{(.*?)\}`, 0},
		{`(?s)\[data-theme=dark\] \{(.*?)\}`, 1},
		{`(?s):root:not\(\[data-theme=light\]\) \{(.*?)\}`, 1},
	} {
		block := regexp.MustCompile(tc.pattern).FindSubmatch(css)
		if len(block) != 2 {
			t.Fatalf("missing palette %s", tc.pattern)
		}
		for key, defaults := range themeDefaults {
			want := "--" + key + ": " + defaults[tc.index] + ";"
			if !strings.Contains(string(block[1]), want) {
				t.Errorf("palette %s does not match setting default %s", tc.pattern, want)
			}
		}
	}
}

// themeAccount 拿一个能登录的普通账号：夹具里的 signIn 就是"注册并登录"那一路，
// 配色用例要的是两个不同的人各自登录。
func (h *harness) themeAccount(handle string) session {
	h.t.Helper()
	return h.signIn(handle)
}

// htmlTag 取回来的是"这个人看到的 <html> 那一行"：配色就挂在这一个标签上。
func (h *harness) htmlTag(s session, path string) string {
	h.t.Helper()
	code, body := h.getAs(s, path)
	if code != http.StatusOK {
		h.t.Fatalf("GET %s = %d:\n%s", path, code, short(body))
	}
	return htmlOpenRe.FindString(body)
}

// 配色的代价在这两条微基准里量（internal/store/theme_test.go）：一次九键解析约 1.7 微秒，
// 拼串约 0.6 微秒。这里不再放渲染基准——harness 绑的是 *testing.B 接不了的 tb，而真正要证的
// 不是"快了多少纳秒"而是"没有多一次查询"，那件事由 userColumns 那一行保证：配色跟着
// authenticate 已经读出来的那一行走。

// themeForm 是浏览器那次提交：九个取色器全都在，一个都不留空。
func themeForm(mode string, colors map[string]string) url.Values {
	v := url.Values{"mode": {mode}}
	for _, k := range store.ThemeKeys {
		v.Set(k, colors[k])
	}
	return v
}

func TestThemeFollowsTheAccountNotTheBrowser(t *testing.T) {
	h := newHarness(t)
	alice := h.themeAccount("alice_theme")
	bob := h.themeAccount("bob_theme")

	if _, page := h.getAs(alice, "/settings"); !strings.Contains(page, "action=\"/settings/theme\"") {
		t.Fatalf("设置页没有那张配色卡：\n%s", short(page))
	}
	code, page := h.postAs(alice, "/settings/theme", themeForm("dark", map[string]string{
		"fg": "#ff0000", "bg": "#001122", "accent": "#00ff00",
	}))
	if code != http.StatusOK || !strings.Contains(page, "配色已保存") {
		t.Fatalf("保存配色 = %d:\n%s", code, short(page))
	}

	// 她的页面上带着她挑的颜色，明暗底色也跟着她选的走。
	open := h.htmlTag(alice, "/")
	if !strings.Contains(open, `data-theme="dark"`) {
		t.Errorf("她的页面没跟着她选的底色：<html %s", open)
	}
	if !strings.Contains(open, "--fg:#ff0000") || !strings.Contains(open, "--bg:#001122") {
		t.Errorf("她的页面没有她的颜色：<html %s", open)
	}
	// 按固定顺序出现，且没调过的那几格不该混进来。
	if strings.Contains(open, "--muted") || strings.Contains(open, "--warn") {
		t.Errorf("没调过的格子也被写进了 style：<html %s", open)
	}

	// 同一条链接、同一台服务器，别人的样子一点没被影响。
	other := h.htmlTag(bob, "/")
	if strings.Contains(other, "--fg") || strings.Contains(other, `data-theme="dark"`) {
		t.Errorf("她的配色漏到了别人页面上：<html %s", other)
	}
	// 匿名访客连 data-theme 都不该有：那是最便宜的那条渲染路径。
	_, anon := h.htmlGet("/", "")
	anonOpen := htmlOpenRe.FindString(anon)
	if strings.Contains(anonOpen, "data-theme") || strings.Contains(anonOpen, "style=") {
		t.Errorf("匿名页面带了配色属性：<html %s", anonOpen)
	}

	// 恢复默认：列清回空串，页面回到站点默认。
	if _, page := h.postAs(alice, "/settings/theme", url.Values{"clear": {"1"}}); !strings.Contains(page, "已恢复默认") {
		t.Fatalf("恢复默认没给出话术：\n%s", short(page))
	}
	u := h.user("alice_theme")
	if u.Theme != "" {
		t.Errorf("恢复默认之后库里还剩 %q", u.Theme)
	}
	open = h.htmlTag(alice, "/")
	if strings.Contains(open, "data-theme") || strings.Contains(open, "--fg") {
		t.Errorf("恢复默认之后页面还带着她调的颜色：<html %s", open)
	}
}

// 这一条盯的是注入面：这一段文本落在 CSS 上下文里，一个右花括号就能改写出新规则。
func TestThemeRejectsAnythingThatIsNotAPaletteColor(t *testing.T) {
	h := newHarness(t)
	alice := h.themeAccount("carol_theme")

	_, page := h.postAs(alice, "/settings/theme", themeForm("neon", map[string]string{
		"fg":   "}body{background:url(https://evil.example/x)",
		"bg":   "red",
		"code": "#fff; --accent: url(evil)",
		"ok":   "#12g456",
		"warn": "#abcdef",
	}))
	if !strings.Contains(page, "配色已保存") {
		t.Fatalf("这一份表单没保存下来：\n%s", short(page))
	}
	open := h.htmlTag(alice, "/")
	for _, bad := range []string{"}", "{", "url(", "expression", "javascript", "evil", "red", "neon"} {
		if strings.Contains(open, bad) {
			t.Errorf("<html> 上出现了 %q：<html %s", bad, open)
		}
	}
	// 唯一活下来的是那个合法的 #abcdef（三位/大写/带脏字符的都被挡掉了）。
	if !strings.Contains(open, "--warn:#abcdef") {
		t.Errorf("合法的 warn 被连坐丢掉了：<html %s", open)
	}
	// mode 只认三种，"neon" 不该变成 data-theme="neon"。
	if strings.Contains(open, `data-theme="neon"`) {
		t.Errorf("mode 没被白名单挡住：<html %s", open)
	}

	// 库里被人手工改脏也渲染不出东西：读回时那一层白名单是第二道。
	if _, err := h.db.SQL.ExecContext(ctx(),
		`UPDATE users SET theme=? WHERE handle='carol_theme'`, `fg=;bg=}&{x:ss;y=#zzz`); err != nil {
		t.Fatal(err)
	}
	open = h.htmlTag(alice, "/")
	if strings.Contains(open, "style=\"--") || strings.Contains(open, "}") || strings.Contains(open, "{") {
		t.Errorf("脏值被拼进了 style：<html %s", open)
	}
}

func TestThemeIsReappliedOnEveryPage(t *testing.T) {
	h := newHarness(t)
	alice := h.themeAccount("dave_theme")
	if _, page := h.postAs(alice, "/settings/theme", themeForm("light", map[string]string{"bg": "#0a0b0c"})); page == "" {
		t.Fatal("保存配色没有返回页面")
	}
	// 换页不算，换标签页也不算：它跟着 viewer 出去，每个页面都带。
	for _, path := range []string{"/", "/problems", "/settings", "/rank"} {
		code, body := h.getAs(alice, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		if !strings.Contains(htmlOpenRe.FindString(body), "--bg:#0a0b0c") {
			t.Errorf("%s 的 <html> 没带着她调的颜色：%s", path, short(body))
		}
	}
}
