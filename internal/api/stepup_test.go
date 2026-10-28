package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 改权限之前的这道身份再确认：会话被拿走的人过不去，而"两分钟"这件事由服务端说，
// 不由页面上写了什么说。下面这几条盯的就是这两点。

// granted 报告这个人现在是否真的拿着某一项权限（生效值，不是覆盖行）。
func (h *harness) granted(handle, key string) bool {
	h.t.Helper()
	u := h.user(handle)
	perms, err := h.db.EffectivePermissions(ctx(), u.ID, u.Role)
	if err != nil {
		h.t.Fatal(err)
	}
	return perms[key]
}

// unlock 替这个会话把密码再输一遍。
func (h *harness) unlock(s session, next string) string {
	h.t.Helper()
	_, page := h.postAs(s, "/admin/perms/unlock", url.Values{
		"step_up": {testPassword}, "next": {next},
	})
	return page
}

// expireStepUp 把这个会话的确认时刻推到窗口之外，免得用例真等两分钟。
func (h *harness) expireStepUp(s session, ago time.Duration) {
	h.t.Helper()
	id := strings.TrimPrefix(s.cookie, "fastoj_session=")
	_, err := h.db.SQL.ExecContext(ctx(), `UPDATE sessions SET step_up_at=? WHERE id=?`,
		time.Now().Add(-ago).UnixMilli(), id)
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) stepUpAt(s session) int64 {
	h.t.Helper()
	id := strings.TrimPrefix(s.cookie, "fastoj_session=")
	var at int64
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT step_up_at FROM sessions WHERE id=?`, id).Scan(&at); err != nil {
		h.t.Fatal(err)
	}
	return at
}

func TestStepUpGatesEveryPermissionConsoleWrite(t *testing.T) {
	h := newHarness(t)
	chief := h.author("chief01", permPermissionManage, permUserManage)
	h.author("pleb01")

	// 没确认过：这一页是只读的，密码框站在按钮前面。
	_, page := h.getAs(chief, "/admin/perms/u/pleb01")
	if !strings.Contains(page, "先确认是你本人") {
		t.Fatalf("没确认时页面没有要密码的那一段：\n%s", short(page))
	}
	if strings.Contains(page, `name="action"`) {
		t.Errorf("没确认就把授予/收回的按钮放出来了：\n%s", short(page))
	}

	// 四个写动作都挡：逐项权限、角色、状态、注销。
	for _, c := range []struct {
		path   string
		values url.Values
		what   string
	}{
		{"/admin/perms/u/pleb01", url.Values{"key": {permProblemCreate}, "action": {"grant"}}, "权限"},
		{"/admin/users/pleb01/role", url.Values{"role": {"admin"}}, "角色"},
		{"/admin/users/pleb01/status", url.Values{"status": {"banned"}}, "状态"},
		{"/admin/users/pleb01/cancel", url.Values{"confirm": {"pleb01"}}, "注销"},
	} {
		_, page := h.postAs(chief, c.path, c.values)
		if !strings.Contains(page, "请输入你的密码") {
			t.Errorf("改%s 没被身份确认挡住，或者没给出原因：\n%s", c.what, short(page))
		}
	}
	// 一条都没生效：权限没给、角色没改、没被封、没被注销。
	if h.granted("pleb01", permProblemCreate) {
		t.Error("没确认就授出了 problem.create")
	}
	if u := h.user("pleb01"); u.Role != "user" || u.Status != "active" || u.IsCancelled() {
		t.Errorf("没确认就把账号改成了 %+v", u)
	}

	// 确认一次，四个动作从此在 2 分钟内放行。
	if page := h.unlock(chief, "/admin/perms/u/pleb01"); !strings.Contains(page, "身份已确认") {
		t.Fatalf("输对密码之后没有确认：\n%s", short(page))
	}
	if at := h.stepUpAt(chief); at == 0 || at > time.Now().UnixMilli() {
		t.Errorf("step_up_at = %d，应该是一个刚写入的时刻", at)
	}
	_, page = h.getAs(chief, "/admin/perms/u/pleb01")
	if strings.Contains(page, "先确认是你本人") || !strings.Contains(page, `name="action"`) {
		t.Errorf("确认之后这一页还是没有按钮：\n%s", short(page))
	}
	if _, page := h.postAs(chief, "/admin/perms/u/pleb01",
		url.Values{"key": {permProblemCreate}, "action": {"grant"}}); !strings.Contains(page, "pleb01 → problem.create") {
		t.Fatalf("确认之后授予权限没成：\n%s", short(page))
	}
	if !h.granted("pleb01", permProblemCreate) {
		t.Fatal("授予之后生效值还是无")
	}
	if _, page := h.postAs(chief, "/admin/users/pleb01/status", url.Values{"status": {"banned"}}); !strings.Contains(page, "状态已设为 banned") {
		t.Fatalf("确认之后改状态没成：\n%s", short(page))
	}
}

// 这一条是那 2 分钟的全部意义：窗口之外必须重新问，而"窗口"是服务端那个时刻，
// 不是页面藏了什么。
func TestStepUpExpiresAndAsksAgain(t *testing.T) {
	h := newHarness(t)
	chief := h.author("chief02", permPermissionManage)
	h.author("pleb02")

	if page := h.unlock(chief, "/admin/perms/u/pleb02"); !strings.Contains(page, "身份已确认") {
		t.Fatalf("确认没成：\n%s", short(page))
	}
	h.expireStepUp(chief, stepUpWindow+time.Minute)

	if _, page := h.postAs(chief, "/admin/perms/u/pleb02",
		url.Values{"key": {permTopicPost}, "action": {"revoke"}}); !strings.Contains(page, "请输入你的密码") {
		t.Fatalf("窗口过了还在放行：\n%s", short(page))
	}
	if !h.granted("pleb02", permTopicPost) {
		t.Error("被身份确认挡住的那次收回却生效了")
	}
	// 再确认一次就又能动。
	if page := h.unlock(chief, "/admin/perms/u/pleb02"); !strings.Contains(page, "身份已确认") {
		t.Fatalf("第二次确认没成：\n%s", short(page))
	}
	if _, page := h.postAs(chief, "/admin/perms/u/pleb02",
		url.Values{"key": {permTopicPost}, "action": {"revoke"}}); strings.Contains(page, "请输入你的密码") {
		t.Fatalf("重新确认之后还在挡：\n%s", short(page))
	}
}

// reLogin 拿同一个账号的第二个会话——相当于本人在另一台电脑上再登一次。harness 里的
// signIn 是"注册并登录"，账号已经存在时它开不出第二个会话。
func (h *harness) reLogin(handle string) session {
	h.t.Helper()
	code, _, hdr := h.postNoRedirect("/login", url.Values{"identifier": {handle}, "password": {testPassword}}, "")
	if code != http.StatusSeeOther {
		h.t.Fatalf("重新登录 %s = %d", handle, code)
	}
	cookie := "fastoj_session=" + cookieValue(hdr.Values("Set-Cookie"), "fastoj_session")
	_, page := h.htmlWithCookie(http.MethodGet, "/settings", cookie)
	return session{handle: handle, cookie: cookie, csrf: extractCSRF(page)}
}

// 确认记在会话上，不是记在账号上：本人另一台设备上的登录不会替他背书。
func TestStepUpIsPerSessionNotPerAccount(t *testing.T) {
	h := newHarness(t)
	desk := h.author("chief03", permPermissionManage)
	h.author("plain03")
	// 同一个账号、同一个密码，第二个会话（相当于另一台电脑）。
	phone := h.reLogin("chief03")

	if page := h.unlock(desk, "/admin/perms"); !strings.Contains(page, "身份已确认") {
		t.Fatalf("桌上的会话没确认成：\n%s", short(page))
	}
	if _, page := h.postAs(phone, "/admin/perms/u/plain03", url.Values{"key": {permProblemCreate}, "action": {"grant"}}); page == "" {
		t.Fatal("手机上的会话请求没有页面")
	} else if !strings.Contains(page, "请输入你的密码") {
		t.Fatalf("另一个会话的确认被这个会话用上了：\n%s", short(page))
	}
	if h.granted("plain03", permProblemCreate) {
		t.Error("第二个会话借用了第一个会话的确认")
	}
}

// 连错五次就把这个会话踢掉，而且只踢这一个会话——被偷走的会话该断，本人的另一台设备不该陪葬。
func TestStepUpFailuresKillThatSessionOnly(t *testing.T) {
	h := newHarness(t)
	chief := h.author("chief04", permPermissionManage)
	h.author("pleb04")
	id := strings.TrimPrefix(chief.cookie, "fastoj_session=")

	for i := 1; i <= maxStepUpFailures-1; i++ {
		_, page := h.postAs(chief, "/admin/perms/unlock", url.Values{
			"step_up": {"wrong-password-here"}, "next": {"/admin/perms/u/pleb04"},
		})
		want := "密码不正确（这次确认还剩 " + strconv.Itoa(maxStepUpFailures-i) + " 次机会）"
		if !strings.Contains(page, want) {
			t.Fatalf("第 %d 次输错的提示不对：\n%s", i, short(page))
		}
	}
	// 第五次：会话当场作废。
	_, page := h.postAs(chief, "/admin/perms/unlock", url.Values{
		"step_up": {"wrong-password-here"}, "next": {"/admin/perms/u/pleb04"},
	})
	if !strings.Contains(page, "登录") {
		t.Errorf("第五次之后没有把人送到登录页：\n%s", short(page))
	}
	var live int
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT COUNT(*) FROM sessions WHERE id=?`, id).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("五次输错之后这个会话还活着（%d 行）", live)
	}
	if n := len(h.auditRows("step_up.denied")); n != 1 {
		t.Errorf("审计里的确认失败记录 = %d 行，应该有 1 行", n)
	}
	// 本人另一台设备上的会话不受影响：重新登录一个，仍然能正常确认。
	fresh := h.reLogin("chief04")
	if page := h.unlock(fresh, "/admin/perms"); !strings.Contains(page, "身份已确认") {
		t.Fatalf("新会话确认不了：\n%s", short(page))
	}
}

// 拿着管理令牌的那一路不需要这道门：它每一次请求都自带那个秘密。
func TestStepUpDoesNotApplyToBearerToken(t *testing.T) {
	h := newHarness(t)
	h.author("pleb05")
	code, body := h.postBearer("/admin/perms/u/pleb05",
		url.Values{"key": {permProblemCreate}, "action": {"grant"}}, h.adminToken())
	if code != http.StatusOK || !strings.Contains(body, "pleb05 → problem.create") {
		t.Fatalf("令牌那一路被这道门挡了 = %d:\n%s", code, short(body))
	}
}

// 密码不进日志、不进审计、不回显：提示里只该有"还剩几次"。
func TestStepUpNeverEchoesThePassword(t *testing.T) {
	h := newHarness(t)
	chief := h.author("chief06", permPermissionManage)
	h.author("pleb06")
	const secret = "a-password-that-must-come-back"
	_, page := h.postAs(chief, "/admin/perms/unlock", url.Values{
		"step_up": {secret}, "next": {"/admin/perms/u/pleb06"},
	})
	if strings.Contains(page, secret) {
		t.Errorf("页面把密码回显了：\n%s", short(page))
	}
	// 先拿一条确实会进审计的动作垫底（令牌那一路不受这道门管），不然这个断言是空转的。
	if _, page := h.postBearer("/admin/perms/u/pleb06",
		url.Values{"key": {permTopicPost}, "action": {"grant"}}, h.adminToken()); !strings.Contains(page, "pleb06 → topic.post") {
		t.Fatalf("垫底的审计动作没成：\n%s", short(page))
	}
	rows, err := h.db.ListAudit(ctx(), store.AuditFilter{Limit: 200})
	if err != nil {
		h.t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("这条用例没读到任何审计行，断言本身失效了")
	}
	for _, row := range rows {
		for _, cell := range []string{row.Actor, row.Target, row.Detail, row.IP} {
			if strings.Contains(cell, secret) {
				t.Fatalf("审计第 %d 行里出现了密码：%q", row.ID, cell)
			}
		}
	}
	// 权限当然也没动。
	if _, page := h.postAs(chief, "/admin/perms/u/pleb06",
		url.Values{"key": {permProblemCreate}, "action": {"grant"}}); !strings.Contains(page, "请输入你的密码") {
		t.Fatalf("没确认就改成了权限：\n%s", short(page))
	}
}
