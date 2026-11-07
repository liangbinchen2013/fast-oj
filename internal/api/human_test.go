package api

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 伪人机验证：观感在页面上，强制的是服务端那段时间加上"那一下真的点到了"。
// 这几条用例盯的是后者——弹不弹按这个地址的频繁度；等没等够由 ready_at 说话；
// 而复选框必须变成一次服务端收得到的请求。

var verifyTokenRe = regexp.MustCompile(`name="verify" value="([^"]+)"`)
var verifySecondsRe = regexp.MustCompile(`data-seconds="(\d+)"`)

// humanToken 取出这一页发下来的令牌和它要求的秒数；页面没有那个窗时 ok 为 false。
func humanToken(body string) (string, int, bool) {
	m := verifyTokenRe.FindStringSubmatch(body)
	if m == nil {
		return "", 0, false
	}
	s := verifySecondsRe.FindStringSubmatch(body)
	if s == nil {
		return m[1], 0, true
	}
	n, _ := strconv.Atoi(s[1])
	return m[1], n, true
}

const browserUA = "Mozilla/5.0 (X11; Linux x86_64) fast-oj-test-browser"

// postShape 用一个指定的形状发一次表单 POST。这一条只为本文件服务：要证的正是
// "带不带浏览器那几个头"的区别，塞进通用夹具里反而会把别的用例也一起改掉。
// lang 非空时带上 Accept-Language——真浏览器一定会带，`curl -A Mozilla/5.0` 不会。
func (h *harness) postShape(path string, values url.Values, ua string, sameOrigin bool, lang string) (int, string, http.Header) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(values.Encode()))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	if sameOrigin {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, string(out), resp.Header
}

// clickVerify 是"点那一下"：浏览器形状、脚本走的那一路（js=1，只要状态码）。
func (h *harness) clickVerify(kind, token string) (int, string) {
	h.t.Helper()
	return h.clickVerifyAs(kind, token, browserUA, true, "")
}

// clickVerifyAs 用指定形状点那一下：ua 为空表示连 User-Agent 都不带，sameOrigin 为 false
// 表示不带 Sec-Fetch-*。
func (h *harness) clickVerifyAs(kind, token, ua string, sameOrigin bool, lang string) (int, string) {
	h.t.Helper()
	code, body, _ := h.postShape("/verify", url.Values{
		"verify": {token}, "kind": {kind}, "js": {"1"},
	}, ua, sameOrigin, lang)
	return code, body
}

// registerThrough 走完整的一条：开页 → 等够 → 点那一下 → 交表单。
func (h *harness) registerThrough(handle, email, device string) (int, string, http.Header) {
	h.t.Helper()
	_, page := h.htmlGet("/register", "")
	token, seconds, present := humanToken(page)
	if !present {
		h.t.Fatalf("注册页没有验证窗：\n%s", short(page))
	}
	time.Sleep(time.Duration(seconds+1) * time.Second)
	if code, _ := h.clickVerify(store.VerifyRegister, token); code != http.StatusNoContent {
		h.t.Fatalf("点那一下 = %d，应该 204:\n%s", code, short(""))
	}
	return h.postNoRedirect("/register", url.Values{
		"handle": {handle}, "email": {email}, "password": {testPassword},
		"device": {device}, "verify": {token},
	}, "")
}

func TestHumanVerifyRegisterWaitsFromSecondAccount(t *testing.T) {
	// 上限设成 1 秒：等待区间收成"必定 1 秒"，用例要等的就是确定的 1 秒而不是随机几秒。
	h := newHarnessCfg(t, func(c *config.Config) {
		c.HumanVerifyMax = 1
		c.RegisterPerMinute = 50
	})

	// 第一次：这个地址本小时还没有成功注册，页面不该有那个窗。
	_, body := h.htmlGet("/register", "")
	if _, _, present := humanToken(body); present {
		t.Fatalf("第一条注册就弹了验证窗：\n%s", short(body))
	}
	if code, _, _ := h.register("alice", "alice@example.com", goodDevice); code != http.StatusSeeOther {
		t.Fatalf("第一条注册 = %d", code)
	}

	// 第二条起必弹，而且表单里带着服务端发的令牌。
	_, body = h.htmlGet("/register", "")
	token, seconds, present := humanToken(body)
	if !present {
		t.Fatalf("第二条注册没有验证窗：\n%s", short(body))
	}
	if seconds != 1 {
		t.Fatalf("等待秒数 = %d，上限是 1", seconds)
	}
	// 页面上不该出现"还剩几秒"这种数字。
	if strings.Contains(body, "还有 ") || strings.Contains(body, "约 1 秒") {
		t.Errorf("验证窗把剩余秒数写在页面上：\n%s", short(body))
	}
	// 点击走 fetch：老浏览器没有 Sec-Fetch-*，服务端要看 Accept/Accept-Language/UA。
	// fetch 默认 Accept 是 */*，模板必须显式补成浏览器形状，否则倒计时结束后一直 403。
	for _, want := range []string{"'Accept': 'text/html'", "'Accept-Language':"} {
		if !strings.Contains(body, want) {
			t.Errorf("verify fetch missing %s；真实老浏览器会被 looksLikeBrowser 拒掉", want)
		}
	}

	values := url.Values{
		"handle": {"bob"}, "email": {"bob@example.com"}, "password": {testPassword},
		"device": {goodDevice}, "verify": {token},
	}
	// 没等够就提交：拒绝，而且不许偷偷建号。
	code, rejected, _ := h.postNoRedirect("/register", values, "")
	if code != http.StatusForbidden {
		t.Fatalf("没等够就注册成功 = %d:\n%s", code, short(rejected))
	}
	if !strings.Contains(rejected, "请先完成页面里的验证") {
		t.Errorf("被拒的那一页没说为什么要拒绝：\n%s", short(rejected))
	}
	if _, err := h.db.GetUserByHandle(ctx(), "bob"); err != store.ErrNotFound {
		t.Errorf("被验证挡住的注册还是建了账号：%v", err)
	}
	// 被打回来重填不该让人重新等一遍：沿用同一个令牌，秒数只会更少。
	if again, still, _ := humanToken(rejected); again != token {
		t.Errorf("重填的那一页换了令牌（%q 而不是 %q），等于让提交者重新等满", again, token)
	} else if still > seconds {
		t.Errorf("重填的那一页等待变长了：%d > %d", still, seconds)
	}

	// 这一条是 0029 存在的原因：睡够秒数、直接交表单——以前这就是完整的绕过路线，
	// 现在没有服务端那一次点击记录，核销一样过不去。
	time.Sleep(1100 * time.Millisecond)
	code, stillRejected, _ := h.postNoRedirect("/register", values, "")
	if code != http.StatusForbidden {
		t.Fatalf("睡够秒数就绕过复选框注册成功 = %d:\n%s", code, short(stillRejected))
	}
	if _, err := h.db.GetUserByHandle(ctx(), "bob"); err != store.ErrNotFound {
		t.Errorf("绕过路线建出了账号：%v", err)
	}

	// curl 那种形状去点也点不上：不是它等得不够，是这一眼不像浏览器发的。
	if code, _ := h.clickVerifyAs(store.VerifyRegister, token, "curl/8.6.0", false, ""); code != http.StatusForbidden {
		t.Fatalf("curl 形状盖上了章 = %d", code)
	}
	// 换个地址领的、或者根本不存在的令牌，章也盖不上。
	if code, _ := h.clickVerify(store.VerifyRegister, "not-a-real-token"); code != http.StatusForbidden {
		t.Fatalf("假令牌盖上了章 = %d", code)
	}

	// 浏览器形状点这一下：204，章盖上了。
	if code, _ := h.clickVerify(store.VerifyRegister, token); code != http.StatusNoContent {
		t.Fatalf("点那一下 = %d，应该 204", code)
	}
	// 重复点不算第二次：章已经在了。
	if code, _ := h.clickVerify(store.VerifyRegister, token); code != http.StatusForbidden {
		t.Fatalf("同一个令牌盖了两次章 = %d", code)
	}

	code, okay, hdr := h.postNoRedirect("/register", values, "")
	if code != http.StatusSeeOther {
		t.Fatalf("点过之后注册 = %d:\n%s", code, short(okay))
	}
	if q, _ := url.ParseQuery(strings.TrimPrefix(hdr.Get("Location"), "/?")); !strings.Contains(q.Get("msg"), "欢迎，bob") {
		t.Errorf("注册没有落到欢迎页：%q", hdr.Get("Location"))
	}

	// 令牌是一次性的：拿刚用过的那个再来一次，必须算没过。
	code, _, _ = h.postNoRedirect("/register", url.Values{
		"handle": {"carol"}, "email": {"carol@example.com"}, "password": {testPassword},
		"device": {goodDevice}, "verify": {token},
	}, "")
	if code != http.StatusForbidden {
		t.Fatalf("用过的令牌还能再核销一次 = %d", code)
	}
	if _, err := h.db.GetUserByHandle(ctx(), "carol"); err != store.ErrNotFound {
		t.Errorf("第二次用同一个令牌也建了号：%v", err)
	}
}

// 这一条盯的就是他点出来的那个绕过姿势：脚本把整页抓下来、睡够秒数、再交表单。
func TestHumanVerifySleepAndSubmitIsNotEnough(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) {
		c.HumanVerifyMax = 1
		c.RegisterPerMinute = 50
	})
	if code, _, _ := h.register("host", "host@example.com", goodDevice); code != http.StatusSeeOther {
		t.Fatalf("第一条注册 = %d", code)
	}
	_, page := h.htmlGet("/register", "")
	token, seconds, present := humanToken(page)
	if !present {
		t.Fatalf("第二条注册没有验证窗：\n%s", short(page))
	}
	time.Sleep(time.Duration(seconds+1) * time.Second)

	// 三条都试：直接交、拿脚本默认形状点一下再交、什么头都不带点一下再交。
	code, _, _ := h.postNoRedirect("/register", url.Values{
		"handle": {"sleepy"}, "email": {"sleepy@example.com"}, "password": {testPassword},
		"device": {goodDevice}, "verify": {token},
	}, "")
	if code != http.StatusForbidden {
		t.Errorf("睡够就直接注册上了 = %d", code)
	}
	if got, _ := h.clickVerifyAs(store.VerifyRegister, token, "", false, ""); got != http.StatusForbidden {
		t.Errorf("什么头都不带就盖上了章 = %d", got)
	}
	if got, _ := h.clickVerifyAs(store.VerifyRegister, token, "python-requests/2.32", false, ""); got != http.StatusForbidden {
		t.Errorf("python-requests 盖上了章 = %d", got)
	}
	if _, err := h.db.GetUserByHandle(ctx(), "sleepy"); err != store.ErrNotFound {
		t.Errorf("绕过路线建出了账号：%v", err)
	}
	// 只改 UA 的那一行命令（`curl -A Mozilla/5.0`）不算浏览器：它不会带 Accept-Language，
	// 而这正是老 Safari 那条宽松分支要求的第三条。真人浏览器三条同时缺的概率极低。
	if got, _ := h.clickVerifyAs(store.VerifyRegister, token, "Mozilla/5.0 (Windows NT 10.0)", false, ""); got != http.StatusForbidden {
		t.Errorf("只改 UA 不改别的就盖上了章 = %d", got)
	}
	// 老浏览器（不带 Sec-Fetch-* 的那批）仍然要放行：UA + 收 HTML + 有语言偏好。
	if got, _ := h.clickVerifyAs(store.VerifyRegister, token, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605.1.15", false, "zh-CN,zh;q=0.9"); got != http.StatusNoContent {
		t.Errorf("老 Safari 的形状被当成脚本拒了 = %d", got)
	}
	code, out, _ := h.postNoRedirect("/register", url.Values{
		"handle": {"sleepy"}, "email": {"sleepy@example.com"}, "password": {testPassword},
		"device": {goodDevice}, "verify": {token},
	}, "")
	if code != http.StatusSeeOther {
		t.Fatalf("点过之后还是注册不上 = %d:\n%s", code, short(out))
	}
	// 章只能盖一次：已经用掉的窗口再点也算没过。
	if got, _ := h.clickVerifyAs(store.VerifyRegister, token, browserUA, true, "zh-CN"); got != http.StatusForbidden {
		t.Errorf("同一个令牌盖了两次章 = %d", got)
	}
}

func TestHumanVerifyRegisterWaitsLongerAsItGoes(t *testing.T) {
	// 上限放到 8 秒，才看得出它在按频繁度往上长：第 2 次注册是 1..5，第 3 次 1..6。
	h := newHarnessCfg(t, func(c *config.Config) {
		c.HumanVerifyMax = 8
		c.RegisterPerMinute = 50
	})
	if code, _, _ := h.register("dave", "dave@example.com", goodDevice); code != http.StatusSeeOther {
		t.Fatalf("第一条注册 = %d", code)
	}
	_, body := h.htmlGet("/register", "")
	_, first, present := humanToken(body)
	if !present || first < 1 || first > 5 {
		t.Fatalf("第二条注册的等待秒数 = %d（窗存在 %v），应该在 1..5", first, present)
	}

	// 让第二条号真正注册成功，计数才会往前走（被拒的那些不算）。
	token, _, _ := humanToken(body)
	time.Sleep(time.Duration(first+1) * time.Second)
	if code, _ := h.clickVerify(store.VerifyRegister, token); code != http.StatusNoContent {
		t.Fatalf("第二条号点那一下 = %d", code)
	}
	code, out, _ := h.postNoRedirect("/register", url.Values{
		"handle": {"erin"}, "email": {"erin@example.com"}, "password": {testPassword},
		"device": {"z1a2b3c4d5e6f7g8h9i0j1k2l3m4n5o6"}, "verify": {token},
	}, "")
	if code != http.StatusSeeOther {
		t.Fatalf("第二条注册 = %d:\n%s", code, short(out))
	}
	_, body = h.htmlGet("/register", "")
	_, second, present := humanToken(body)
	if !present {
		t.Fatalf("第三条注册没有验证窗：\n%s", short(body))
	}
	if second < 1 || second > 6 {
		t.Fatalf("第三条注册的等待秒数 = %d，应该在 1..6（比上一条的上限多 1）", second)
	}
}

func TestHumanVerifyLoginStartsAtThreeFailures(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) { c.HumanVerifyMax = 1 })
	if _, err := h.db.CreateAccount(ctx(), store.AccountInput{
		Handle: "target2", Email: "target2@example.com", Password: testPassword,
	}); err != nil {
		t.Fatal(err)
	}

	// 打错两次密码还不该有窗：那是学生日常，不是机器。
	for i := 1; i <= 2; i++ {
		_, body := h.htmlGet("/login", "")
		if _, _, present := humanToken(body); present {
			t.Fatalf("第 %d 次失败之后就弹窗了：\n%s", i, short(body))
		}
		if _, out := h.formPage("/login", url.Values{"identifier": {"target2"}, "password": {"wrong-password-here"}}); !strings.Contains(out, "账号或密码不正确") {
			t.Fatalf("第 %d 次失败没有给出正常报错：\n%s", i, short(out))
		}
	}
	// 第三次失败才是门槛：攒到 3 次起，这一页就要先验证。
	if _, out := h.formPage("/login", url.Values{"identifier": {"target2"}, "password": {"wrong-password-here"}}); !strings.Contains(out, "账号或密码不正确") {
		t.Fatalf("第三次失败没有给出正常报错：\n%s", short(out))
	}
	_, body := h.htmlGet("/login", "")
	token, seconds, present := humanToken(body)
	if !present {
		t.Fatalf("三次失败之后登录页还是没有窗：\n%s", short(body))
	}
	if seconds != 1 {
		t.Fatalf("登录的等待秒数 = %d，上限是 1", seconds)
	}

	// 没等够就提交：密码是对的也不放行，而且这一次不算密码错误（不该把人推向那条真锁）。
	if code, out, _ := h.postNoRedirect("/login", url.Values{
		"identifier": {"target2"}, "password": {testPassword}, "verify": {"not-a-real-token"},
	}, ""); code != http.StatusForbidden || !strings.Contains(out, "请先完成页面里的验证") {
		t.Fatalf("假令牌登录 = %d:\n%s", code, short(out))
	}
	hits, err := h.db.RateHits(ctx(), "login:127.0.0.1", loginWindow)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 3 {
		t.Fatalf("被验证挡住的登录也算了一次密码错误：失败次数 = %d，应该还是 3", hits)
	}

	// 睡够秒数但不点：登录也一样过不去。
	time.Sleep(1100 * time.Millisecond)
	if code, _, _ := h.postNoRedirect("/login", url.Values{
		"identifier": {"target2"}, "password": {testPassword}, "verify": {token},
	}, ""); code != http.StatusForbidden {
		t.Fatalf("睡够秒数就绕过复选框登录成功 = %d", code)
	}
	if code, _ := h.clickVerify(store.VerifyLogin, token); code != http.StatusNoContent {
		t.Fatalf("登录页点那一下 = %d", code)
	}
	code, _, hdr := h.postNoRedirect("/login", url.Values{
		"identifier": {"target2"}, "password": {testPassword}, "verify": {token},
	}, "")
	if code != http.StatusSeeOther || cookieValue(hdr.Values("Set-Cookie"), "fastoj_session") == "" {
		t.Fatalf("点过之后登录 = %d %v", code, hdr.Values("Set-Cookie"))
	}
	// 登录成功把那串失败清零：下一次登录页不该再有窗。
	if _, body := h.htmlGet("/login", ""); len(body) > 0 {
		if _, _, present := humanToken(body); present {
			t.Errorf("登录成功后登录页还带着验证窗：\n%s", short(body))
		}
	}
}

// 没有 JavaScript 的人靠这张表单里那个真按钮：POST /verify 不带 js=1，服务端盖完章
// 把他送回表单页，页面上那一段"验证好了"就是他能提交了的凭据。
func TestHumanVerifyNoScriptPathCompletes(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) {
		c.HumanVerifyMax = 1
		c.RegisterPerMinute = 50
	})
	if code, _, _ := h.register("plain", "plain@example.com", goodDevice); code != http.StatusSeeOther {
		t.Fatalf("第一条注册 = %d", code)
	}
	_, page := h.htmlGet("/register", "")
	token, _, present := humanToken(page)
	if !present {
		t.Fatalf("第二条注册没有验证窗：\n%s", short(page))
	}
	time.Sleep(1100 * time.Millisecond)
	code, _, hdr := h.postShape("/verify", url.Values{"verify": {token}, "kind": {"register"}}, browserUA, true, "zh-CN")
	if code != http.StatusSeeOther {
		t.Fatalf("没有 JavaScript 的那一次点 = %d", code)
	}
	if loc := hdr.Get("Location"); !strings.HasPrefix(loc, "/register?") || !strings.Contains(loc, "msg=") {
		t.Fatalf("点完之后没被送回注册页：%q", loc)
	}
	_, back := h.htmlGet("/register", "")
	if !strings.Contains(back, "验证好了") {
		t.Errorf("送回注册页之后没有显示验证已完成：\n%s", short(back))
	}
	if strings.Contains(back, "data-seconds") {
		t.Errorf("验证完成后那个倒计时窗还挡着：\n%s", short(back))
	}
	// 表单里的隐藏字段还是同一个令牌：人只需要再点一次提交。
	newest, _, _ := humanToken(back)
	if newest != token {
		t.Errorf("回来后令牌换了（%q 而不是 %q）", newest, token)
	}
	code, out, _ := h.postNoRedirect("/register", url.Values{
		"handle": {"done"}, "email": {"done@example.com"}, "password": {testPassword},
		"device": {goodDevice}, "verify": {token},
	}, "")
	if code != http.StatusSeeOther {
		t.Fatalf("无 JavaScript 路径最终注册 = %d:\n%s", code, short(out))
	}
}

func TestHumanVerifyDisabledMeansJustThrottling(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) {
		c.HumanVerifyMax = 0
		c.RegisterPerMinute = 50
	})
	_, page := h.htmlGet("/register", "")
	if _, _, present := humanToken(page); present {
		t.Fatalf("关掉整套之后注册页还有验证窗：\n%s", short(page))
	}
	for i, name := range []string{"frank", "grace"} {
		code, out, _ := h.register(name, name+"@example.com", goodDevice)
		if code != http.StatusSeeOther {
			t.Fatalf("第 %d 条注册 = %d:\n%s", i+1, code, short(out))
		}
	}
	if n := h.userCount(); n != 3 {
		t.Errorf("注册了两个账号之后用户数 = %d，应该是 3（含 claim 那个管理员）", n)
	}
	// 整套关掉时，那一下点击也就没有意义了：没有窗口可盖，一律算没过。
	if code, _ := h.clickVerify(store.VerifyRegister, "whatever"); code != http.StatusForbidden {
		t.Errorf("关掉整套后仍然盖上了章 = %d", code)
	}
}
