package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 注销账号：抹掉身份、释放用户名，记录留着并署名成封禁用户；
// 下一个用同一个用户名注册的人，接不到上一任的任何东西。

func TestCancelAccountFreesTheNameAndKeepsTheRecords(t *testing.T) {
	h := newHarness(t)
	ghost := h.author("reused")
	ghostID := h.user("reused").ID
	h.readyProblem("P8100")
	code, body := h.submitCode(ghost, "P8100", "cpp", "int main(){return 0;}")
	if code != http.StatusOK {
		t.Fatalf("提交 = %d\n%s", code, short(body))
	}
	sid := h.submissionID(body)
	h.scoreSubmission(sid, "AC", 10)
	// 他留下的帖子与动态也要跟着署名走。
	topicID := h.createTopicID(ghost, url.Values{"title": {"上一任的帖子"}, "body": {"正文"}})
	if code, _ := h.postAs(ghost, "/feed", url.Values{"body": {"上一任的动态"}}); code != http.StatusOK {
		t.Fatalf("发动态 = %d", code)
	}

	// 没填对用户名不放行：这一步不可逆。
	if _, body := h.postBearer("/admin/users/reused/cancel", url.Values{"confirm": {"wrong"}}, h.adminToken()); !strings.Contains(body, "把用户名原样填进确认框") {
		t.Errorf("确认框没拦住：\n%s", short(body))
	}
	if u := h.user("reused"); u.IsCancelled() {
		t.Fatal("没确认就注销了")
	}

	if _, body := h.postBearer("/admin/users/reused/cancel", url.Values{"confirm": {"reused"}}, h.adminToken()); !strings.Contains(body, "已注销") {
		t.Fatalf("注销没给出结果：\n%s", short(body))
	}
	// 注销之后按老用户名已经查不到了（那个名字自由了），只能按 id 读这行墓碑。
	old, err := h.db.GetUserByID(ctx(), ghostID)
	if err != nil {
		t.Fatal(err)
	}
	if !old.IsCancelled() || old.Email != "" || old.HasPassword {
		t.Fatalf("身份没抹干净：%+v", old)
	}
	if old.DisplayName != store.AnonymousName {
		t.Errorf("昵称 = %q, want %q", old.DisplayName, store.AnonymousName)
	}

	// 记录一条不少，而且署名不再是那个已经自由的用户名。
	needle := `href="/submissions/` + strconv.FormatInt(sid, 10) + `"`
	_, list := h.htmlGet("/submissions", "")
	if !strings.Contains(list, needle) {
		t.Errorf("注销后提交记录不见了：\n%s", short(list))
	}
	if !strings.Contains(list, store.AnonymousName) {
		t.Errorf("提交列表没署名成%s：\n%s", store.AnonymousName, short(list))
	}
	// 帖子还留着，作者显示封禁用户。
	if _, page := h.htmlGet("/topics/"+strconv.FormatInt(topicID, 10), ""); !strings.Contains(page, "上一任的帖子") ||
		!strings.Contains(page, store.AnonymousName) {
		t.Errorf("帖子没了或署名不对")
	}
	// 动态也留着。
	if _, page := h.htmlGet("/feed", ""); !strings.Contains(page, "上一任的动态") ||
		strings.Contains(page, ">reused<") {
		t.Errorf("动态署名不对，或者原来的用户名还在冒充")
	}

	// 现在同名注册一个新人：他看不到上一任的任何东西。
	fresh := h.signIn("reused")
	newbie := h.user("reused")
	if newbie.ID == old.ID {
		t.Fatal("释放的用户名没有回到注册通道")
	}
	_, profile := h.htmlGet("/u/reused", "")
	if strings.Contains(profile, "上一任的动态") || strings.Contains(profile, "上一任的帖子") {
		t.Errorf("新人的主页漏出了上一任的内容：\n%s", short(profile))
	}
	if !strings.Contains(profile, "提交 <b>0</b>") {
		t.Errorf("新人主页该是空的：\n%s", short(profile))
	}
	// 按用户名筛提交也只见新人自己的：上一任挂在墓碑名下。
	if _, page := h.htmlGet("/submissions?user=reused", ""); strings.Contains(page, needle) {
		t.Errorf("?user=reused 把上一任的提交算给了新人：\n%s", short(page))
	}
	// 新人不能给上一任的帖子顶楼时冒充作者——署名仍然是各自的用户名。
	if _, page := h.getAs(fresh, "/u/"+old.Handle); !strings.Contains(page, store.AnonymousName) {
		t.Errorf("墓碑主页没说明这是已注销的账号：\n%s", short(page))
	}
}

func TestCancelAccountRefusals(t *testing.T) {
	h := newHarness(t)

	// 需要 user.manage。
	plain := h.author("pleb20")
	if code, _ := h.getAs(plain, "/admin/perms/u/pleb20"); code != http.StatusForbidden {
		t.Errorf("普通人读别人的权限页 = %d", code)
	}
	if code, body := h.postAs(plain, "/admin/users/pleb20/cancel", url.Values{"confirm": {"pleb20"}}); code != http.StatusForbidden {
		t.Errorf("普通人提交注销 = %d, want 403\n%s", code, short(body))
	}

	// 名下还有题的账号先拦住：注销会让题没人能改。
	author := h.author("ownsastu", permProblemCreate)
	if code, _ := h.postAs(author, "/problems", problemValues("P8101", "他建的题")); code != http.StatusOK {
		t.Fatalf("建题 = %d", code)
	}
	if _, body := h.postBearer("/admin/users/ownsastu/cancel", url.Values{"confirm": {"ownsastu"}}, h.adminToken()); !strings.Contains(body, "没有作者") {
		t.Errorf("名下有题却没拦住：\n%s", short(body))
	}
	if u := h.user("ownsastu"); u.IsCancelled() {
		t.Error("被拦住的注销还是执行了")
	}

	// 站主不能被别的管理员注销；也不能注销自己（沿用控制台一贯的"不许动自己"）。
	delegate := h.author("delegate20", permUserManage)
	if _, body := h.postAs(delegate, "/admin/users/"+claimHandle+"/cancel",
		url.Values{"confirm": {claimHandle}}); !strings.Contains(body, "超级管理员") {
		t.Errorf("别的管理员注销站主没被拦住：\n%s", short(body))
	}
	if u := h.user(claimHandle); u.IsCancelled() {
		t.Fatal("站主被注销了")
	}
	if _, body := h.postBearer("/admin/users/"+claimHandle+"/cancel",
		url.Values{"confirm": {claimHandle}}, h.adminToken()); !strings.Contains(body, "自己") {
		t.Errorf("站主注销自己没被拦住：\n%s", short(body))
	}

	// 注销掉的账号登不回来，墓碑页也不给社交按钮。
	h.author("gone20")
	victimID := h.user("gone20").ID
	if _, body := h.postBearer("/admin/users/gone20/cancel", url.Values{"confirm": {"gone20"}}, h.adminToken()); !strings.Contains(body, "已注销") {
		t.Fatalf("注销失败：\n%s", short(body))
	}
	if code, page := h.formPage("/login", url.Values{"identifier": {"banned-" + strconv.FormatInt(victimID, 10)}, "password": {testPassword}}); code == http.StatusSeeOther ||
		strings.Contains(page, "发私信") {
		t.Errorf("注销后还能登录 = %d", code)
	}
	if _, page := h.htmlGet("/u/gone20", ""); strings.Contains(page, "发私信") {
		t.Errorf("墓碑页上还有私信入口")
	}
	// 权限控制台里的墓碑页不给注销按钮。
	if _, page := h.htmlGet("/admin/perms/u/banned-"+strconv.FormatInt(victimID, 10), h.adminToken()); strings.Contains(page, "/cancel") {
		t.Errorf("墓碑的权限页上还留着注销表单")
	}
}
