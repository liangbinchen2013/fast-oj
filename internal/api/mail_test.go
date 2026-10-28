package api

// M25f 的网页侧（发信配置这一半）：SMTP 配置在站点设置页上保存，密码只显示成掩码、
// 永远不回显；「发送测试邮件」把服务器回的原话贴在页面上，而不是转成一句谁都看不懂的
// "发送失败"。验证流本身在 verify_test.go。
//
// 127.0.0.1:1 是这套用例里"一台存在的服务器"的替身：那里没有监听，所以连接立刻被拒，
// 失败原因是真的从网络层带回来的，而不是我们编的一句文案。

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const mailPassword = "hunter2-smtp-password"

// smtpForm is the settings page posting its mail block. Everything else on the form is
// the ordinary all-switches-on save, so a test of one half cannot silently change the
// other half.
func smtpForm(password string) url.Values {
	v := siteForm("allow_register", "allow_feed", "allow_topics", "allow_contests", "allow_dm")
	v.Set("smtp_host", "127.0.0.1")
	v.Set("smtp_port", "1")
	v.Set("smtp_username", "mail@example.com")
	v.Set("smtp_from", "mail@example.com")
	v.Set("smtp_tls", "starttls")
	if password != "" {
		v.Set("smtp_password", password)
	}
	return v
}

func TestSMTPSettingsSaveAndMaskThePassword(t *testing.T) {
	h := newHarness(t)

	if code, body := h.postBearer("/admin/site", smtpForm(mailPassword), h.adminToken()); code != http.StatusOK ||
		!strings.Contains(body, "已保存") {
		t.Fatalf("save = %d\n%s", code, short(body))
	}
	site, err := h.db.Site(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if site.SMTP.Host != "127.0.0.1" || site.SMTP.Port != 1 || site.SMTP.From != "mail@example.com" ||
		site.SMTP.Username != "mail@example.com" || site.SMTP.TLSMode != "starttls" {
		t.Fatalf("发信配置没存全: %+v", site.SMTP)
	}
	// 库里存的是能直接用的那一条：这一层不做二次加密，加密的是"它绝不出页面"。
	if site.SMTP.Password != mailPassword {
		t.Fatalf("stored password = %q", site.SMTP.Password)
	}

	_, page := h.htmlGet("/admin/site", h.adminToken())
	if strings.Contains(page, mailPassword) {
		t.Errorf("设置页把 SMTP 密码打出来了：\n%s", short(page))
	}
	if !strings.Contains(page, "••••••（已保存，输入新值以替换）") {
		t.Errorf("设置页没有给出已保存的掩码：\n%s", short(page))
	}
	if !strings.Contains(page, `name="smtp_password" value=""`) {
		t.Errorf("密码框被填上了值（应该永远是空的）：\n%s", short(page))
	}

	// 空着密码框再保存一次：沿用库里那条。这是"改个首页语就把密码洗掉"的反面。
	keep := smtpForm("")
	keep.Set("title", "换个标题")
	if code, body := h.postBearer("/admin/site", keep, h.adminToken()); code != http.StatusOK ||
		!strings.Contains(body, "已保存") {
		t.Fatalf("第二次保存 = %d\n%s", code, short(body))
	}
	site, err = h.db.Site(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if site.SMTP.Password != mailPassword {
		t.Fatalf("留空保存把密码清掉了: %q", site.SMTP.Password)
	}
	if site.Title != "换个标题" {
		t.Fatalf("标题没存进去: %q", site.Title)
	}

	// 输入新值才替换。
	replace := smtpForm("brand-new-secret")
	if code, body := h.postBearer("/admin/site", replace, h.adminToken()); code != http.StatusOK ||
		!strings.Contains(body, "已保存") {
		t.Fatalf("第三次保存 = %d\n%s", code, short(body))
	}
	if site, err = h.db.Site(ctx()); err != nil || site.SMTP.Password != "brand-new-secret" {
		t.Fatalf("新密码没替换掉: %+v %v", site.SMTP, err)
	}

	// 端口写错、加密方式写错都在这里被挡回来，而不是留到发送那一刻挂一分钟。
	bad := smtpForm("x")
	bad.Set("smtp_port", "70000")
	if code, body := h.postBearer("/admin/site", bad, h.adminToken()); code != http.StatusBadRequest ||
		!strings.Contains(body, "SMTP 端口要填 1..65535") {
		t.Fatalf("坏端口 = %d\n%s", code, short(body))
	}
	wrongTLS := smtpForm("x")
	wrongTLS.Set("smtp_tls", "tls1.3")
	if code, body := h.postBearer("/admin/site", wrongTLS, h.adminToken()); code != http.StatusBadRequest ||
		!strings.Contains(body, "加密方式只认 starttls / ssl / none") {
		t.Fatalf("坏加密方式 = %d\n%s", code, short(body))
	}
	// 被拒的那一次没有动库：上一次存的还是 brand-new-secret。
	if site, err = h.db.Site(ctx()); err != nil || site.SMTP.Password != "brand-new-secret" {
		t.Fatalf("失败的保存改了库: %+v %v", site.SMTP, err)
	}
}

func TestVerificationSwitchNeedsAMailer(t *testing.T) {
	h := newHarness(t)

	// 没有发信配置就勾"注册需要验证邮箱"：直接拒绝。这个组合的结果是新注册的人收不到信、
	// 又被闸门拦住，而这正是这一页要避免的事。
	v := siteForm("allow_register", "require_email_verification")
	if code, body := h.postBearer("/admin/site", v, h.adminToken()); code != http.StatusBadRequest ||
		!strings.Contains(body, "要验证邮箱得先填 SMTP 服务器地址和发件地址") {
		t.Fatalf("没配发信就开验证 = %d\n%s", code, short(body))
	}
	if site, err := h.db.Site(ctx()); err != nil || site.RequireEmailVerify {
		t.Fatalf("被拒的保存还是把开关打开了: %+v %v", site, err)
	}
}

func TestSMTPTestSendSurfacesTheFailure(t *testing.T) {
	h := newHarness(t)
	boss := h.signIn("mailboss", permSiteSettings)

	// 填好配置（127.0.0.1:1 那里没人监听），然后按页面那颗按钮发一封。
	if code, body := h.postAs(boss, "/admin/site", smtpForm(mailPassword)); code != http.StatusOK ||
		!strings.Contains(body, "已保存") {
		t.Fatalf("保存发信配置 = %d\n%s", code, short(body))
	}
	form := smtpForm("")
	form.Set("to", "someone@example.com")
	form.Set("body", "测试一下")
	code, page := h.postAs(boss, "/admin/site/test-email", form)
	if code != http.StatusOK {
		t.Fatalf("测试发送 = %d\n%s", code, short(page))
	}
	if !strings.Contains(page, "测试邮件没发出去") {
		t.Errorf("页面上没有失败结论：\n%s", short(page))
	}
	// 服务器/系统回的原话贴在页面上，管理员才不用猜。
	if !strings.Contains(page, "连不上 127.0.0.1:1") {
		t.Errorf("页面上没有连接失败的原话：\n%s", short(page))
	}
	if strings.Contains(page, mailPassword) {
		t.Errorf("失败页把密码显示出来了：\n%s", short(page))
	}
	// 结果是渲染出来的，不是跳转带的：一条 URL 里不该装着 SMTP 的报错与地址。
	if strings.Contains(page, "test-email?msg=") {
		t.Errorf("测试结果被塞进了 URL：\n%s", short(page))
	}

	// 收件地址没填也要说得出口，而不是闷着连一次。
	code, page = h.postAs(boss, "/admin/site/test-email", smtpForm(""))
	if code != http.StatusOK || !strings.Contains(page, "收件邮箱格式不正确") {
		t.Fatalf("空收件地址 = %d\n%s", code, short(page))
	}

	// 这颗按钮是一个 POST，跟其它管理表单一样过 CSRF 闸门（M20 就是在这里漏的）。
	code, page, _ = h.htmlForm("/admin/site/test-email", url.Values{"to": {"someone@example.com"}}, boss.cookie)
	if code != http.StatusForbidden || !strings.Contains(page, "表单已过期") {
		t.Fatalf("少了 csrf 的测试发送 = %d\n%s", code, short(page))
	}
	// 普通用户连这个端口的边都碰不到。
	kid := h.signIn("mailkid")
	code, _ = h.postAs(kid, "/admin/site/test-email", form)
	if code != http.StatusForbidden {
		t.Fatalf("没有 site.settings 的人发测试邮件 = %d", code)
	}
}

