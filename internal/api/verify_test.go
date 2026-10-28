package api

// M25f 的验证流：注册出来的新账号是先未验证的，邮件里那条链接点一次才算数（第二次点
// 就是"用过了"），没验证的账号能读不能交、也不能占一个参赛名额，而站点开关一关就把
// 已经存在的未验证账号放回去。
//
// 这一页的用例都不真的发信：127.0.0.1:1 那里没有监听，所以每一步都是"配置填全了但
// 服务器连不上"的样子——注册照样成功，闸门照样拦，页面上看得见原因。

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// emailVerifiedOf reads the column straight from the row, so a test can tell
// "the gate is off" apart from "this account was quietly verified".
func (h *harness) emailVerifiedOf(id int64) bool {
	h.t.Helper()
	var n int
	if err := h.db.SQL.QueryRowContext(ctx(),
		`SELECT email_verified FROM users WHERE id=?`, id).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n != 0
}

// openVerifySwitch turns 注册需要验证邮箱 on through the real form, which is also the
// only way to do it: the page refuses the tick while the mailer is not filled in.
func (h *harness) openVerifySwitch() {
	h.t.Helper()
	v := smtpForm(mailPassword)
	v.Set("require_email_verification", "1")
	if code, body := h.postBearer("/admin/site", v, h.adminToken()); code != http.StatusOK ||
		!strings.Contains(body, "已保存") {
		h.t.Fatalf("保存发信配置 = %d\n%s", code, short(body))
	}
	site, err := h.db.Site(ctx())
	if err != nil || !site.RequireEmailVerify {
		h.t.Fatalf("开关没存进去: %+v %v", site, err)
	}
}
func TestUnverifiedEmailIsRefusedASubmission(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P9100")
	h.openVerifySwitch()

	kid := h.signIn("gatekid") // 通过 /register 注册：开关开着，所以这个号是未验证的
	u := h.user("gatekid")
	if u.EmailVerified {
		t.Fatal("开关开着，注册却给了一个已验证的账号")
	}

	// 题面页上不出现提交框，出现的是原因和一个能点的地方。
	page := h.getPage(kid, "/problems/P9100")
	if !strings.Contains(page, "你的邮箱还没验证") {
		t.Errorf("题面页没说明为什么不能提交：\n%s", short(page))
	}
	if strings.Contains(page, `value="提交判题"`) {
		t.Errorf("没资格的那一种情况下还留着提交框：\n%s", short(page))
	}

	// 写的那一步被挡住，而且没有落库。
	if code, body := h.submitCode(kid, "P9100", "cpp", "int main(){}"); code != http.StatusOK ||
		!strings.Contains(body, "你的邮箱还没验证") {
		t.Fatalf("未验证的提交 = %d\n%s", code, short(body))
	}
	if n := h.submissionCount(); n != 0 {
		t.Fatalf("被拒的提交还是落了库: n=%d", n)
	}

	// 加入比赛同样挡，理由是同一句。
	host := h.signIn("gatehost", permContestCreate)
	if _, body := h.createContest(host, contestValues("C9100", "验证测试赛")); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("建赛:\n%s", short(body))
	}
	if code, body := h.joinContest(kid, "C9100", ""); code != http.StatusOK ||
		!strings.Contains(body, "你的邮箱还没验证") {
		t.Fatalf("未验证的加入比赛 = %d\n%s", code, short(body))
	}

	// 读的部分一点没受影响：题目、题面、动态、发帖都照旧。
	if code, _ := h.getAs(kid, "/problems"); code != http.StatusOK {
		t.Fatalf("未验证的人看题库 = %d", code)
	}
	if code, body := h.postAs(kid, "/feed", url.Values{"body": {"还没验证也能发动态"}}); code != http.StatusOK {
		t.Fatalf("未验证的人发动态 = %d\n%s", code, short(body))
	}

	// 开关关掉：同一个未验证的账号立刻能交——闸门认的是开关，不是账号上的历史。
	off := smtpForm(mailPassword)
	if code, body := h.postBearer("/admin/site", off, h.adminToken()); code != http.StatusOK ||
		!strings.Contains(body, "已保存") {
		t.Fatalf("关掉开关 = %d\n%s", code, short(body))
	}
	if verified := h.emailVerifiedOf(u.ID); verified {
		t.Fatal("测试前提变了：这个账号应该还没验证")
	}
	if code, body := h.submitCode(kid, "P9100", "cpp", "int main(){}"); code != http.StatusOK ||
		strings.Contains(body, "你的邮箱还没验证") {
		t.Fatalf("开关关了还不让提交 = %d\n%s", code, short(body))
	}
	if n := h.submissionCount(); n != 1 {
		t.Fatalf("开关关了之后该有一条提交: n=%d", n)
	}
}

func TestVerifyLinkIsSingleUse(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P9110")
	h.openVerifySwitch()
	kid := h.signIn("verifykid")
	u := h.user("verifykid")

	token, err := h.db.CreateEmailToken(ctx(), u.ID, u.Email, store.EmailVerifyTTL)
	if err != nil {
		t.Fatal(err)
	}
	code, page := h.htmlGet("/verify?token="+url.QueryEscape(token), "")
	if code != http.StatusOK || !strings.Contains(page, "已经验证过了") {
		t.Fatalf("第一次点链接 = %d\n%s", code, short(page))
	}
	// 页面上不重复令牌本身：它已经在邮件里了，再印一次只是多一处留痕。
	if strings.Contains(page, token) {
		t.Errorf("验证页把令牌又打了一遍：\n%s", short(page))
	}

	// 同一个链接再点一次：拒绝，并且说清楚是"用过了"。
	code, page = h.htmlGet("/verify?token="+url.QueryEscape(token), "")
	if code != http.StatusOK || !strings.Contains(page, "这个链接用过了") {
		t.Fatalf("重复点同一个链接 = %d\n%s", code, short(page))
	}
	// 编出来的令牌也一样拒。
	if _, page := h.htmlGet("/verify?token=nope", ""); !strings.Contains(page, "这个链接用过了") {
		t.Errorf("无效令牌没说清楚：\n%s", short(page))
	}
	// 没有令牌的 /verify 是一个答案，不是一个 500。
	if code, page := h.htmlGet("/verify", ""); code != http.StatusOK || !strings.Contains(page, "邮箱验证") {
		t.Fatalf("空令牌的 /verify = %d\n%s", code, short(page))
	}

	// 解开之后就能交题了。
	if code, body := h.submitCode(kid, "P9110", "cpp", "int main(){}"); code != http.StatusOK ||
		strings.Contains(body, "你的邮箱还没验证") {
		t.Fatalf("验证过的提交 = %d\n%s", code, short(body))
	}
}

func TestResendVerificationForm(t *testing.T) {
	h := newHarness(t)
	h.openVerifySwitch()
	kid := h.signIn("resendkid")
	u := h.user("resendkid")

	// 设置页在没有验证的时候给出这一颗按钮。
	if page := h.getPage(kid, "/settings"); !strings.Contains(page, "重新发送验证邮件") ||
		!strings.Contains(page, `/settings/email/resend`) {
		t.Errorf("设置页没有重发表单：\n%s", short(page))
	}

	// 点一次：令牌换新的，旧链接作废（邮件本身发不出去，因为这里是 127.0.0.1:1，
	// 所以页面上出现的是那句失败原因——这一条正是"他能在浏览器里自己 debug"）。
	if code, body := h.postAs(kid, "/settings/email/resend", url.Values{}); code != http.StatusOK {
		t.Fatalf("重发 = %d\n%s", code, short(body))
	}
	var live int
	if err := h.db.SQL.QueryRowContext(ctx(),
		`SELECT COUNT(*) FROM email_verify_tokens WHERE used_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	// 发信失败也照样留下了令牌：这一步是"链接已经生成，信没送出去"，
	// 重发一次就换一个，不能堆。
	if live != 1 {
		t.Fatalf("待用的令牌数 = %d, want 1", live)
	}

	// 已经验证过的人不该看到这颗按钮，也不该能靠它再发一封。
	token, err := h.db.CreateEmailToken(ctx(), u.ID, u.Email, store.EmailVerifyTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, page := h.htmlGet("/verify?token="+url.QueryEscape(token), ""); !strings.Contains(page, "已经验证过了") {
		t.Fatalf("验证没生效:\n%s", short(page))
	}
	if page := h.getPage(kid, "/settings"); strings.Contains(page, "重新发送验证邮件") {
		t.Errorf("验证过了还给出重发按钮：\n%s", short(page))
	}
	if code, body := h.postAs(kid, "/settings/email/resend", url.Values{}); code != http.StatusOK ||
		!strings.Contains(body, "已经验证过了，不用再发") {
		t.Fatalf("验证过的人重发 = %d\n%s", code, short(body))
	}
}
