package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// 站级功能开关。关掉的意思是"不再收新的"：已经发出去的内容、历史成绩、读这些页面
// 都照旧，考试周想安静一点就把动态、帖子、私信关掉，只留题目和比赛。

// siteForm is what the settings page posts: every field, with only the named
// switches checked. An unchecked box simply does not arrive.
func siteForm(on ...string) url.Values {
	v := url.Values{
		"title": {"fast oj"}, "tagline": {"极简、极快的在线判题。"},
		"links": {""}, "footer": {""}, "api_hint": {""},
	}
	for _, name := range on {
		v.Set(name, "1")
	}
	return v
}

var allSwitches = []string{"allow_register", "allow_feed", "allow_topics", "allow_contests", "allow_dm"}

func TestSiteFeatureSwitches(t *testing.T) {
	h := newHarness(t)
	kid := h.author("kid6")
	host := h.author("host6", permContestCreate)
	h.readyProblem("P7010")
	if _, body := h.createContest(host, contestValues("C7010", "关闭测试赛")); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("建赛:\n%s", short(body))
	}
	h.mountContestProblems(host, "C7010", "P7010")

	// 关之前先各留一条，作为"旧内容还读得到"的证据。
	if code, body := h.postAs(kid, "/feed", url.Values{"body": {"关掉之前发的动态"}}); code != http.StatusOK {
		t.Fatalf("发动态 = %d\n%s", code, short(body))
	}
	topicID := h.createTopicID(kid, url.Values{"title": {"关掉之前发的帖子"}, "body": {"正文"}})
	if code, _ := h.comment(kid, topicID, "关掉之前的评论"); code != http.StatusOK {
		t.Fatalf("评论 = %d", code)
	}
	if code, _ := h.postAs(kid, "/messages/"+host.handle, url.Values{"body": {"关掉之前的私信"}}); code != http.StatusOK {
		t.Fatalf("私信 = %d", code)
	}

	// 只留注册开着，其余四个开关全部关掉。
	values := siteForm("allow_register")
	if code, body := h.postBearer("/admin/site", values, h.adminToken()); code != http.StatusOK {
		t.Fatalf("保存站点设置 = %d\n%s", code, short(body))
	}

	for _, tried := range []struct {
		what string
		path string
		body url.Values
		want string
	}{
		{"发动态", "/feed", url.Values{"body": {"关后再发"}}, "本站已关闭动态发布。"},
		{"发帖", "/topics", url.Values{"title": {"关后再发"}, "body": {"正文"}, "kind": {"community"}}, "本站已关闭发帖。"},
		{"评论", "/topics/" + strconv.FormatInt(topicID, 10) + "/comment", url.Values{"body": {"关后再评"}}, "本站已关闭评论。"},
		{"加入比赛", "/contests/C7010/join", url.Values{"code": {""}}, "本站已关闭参加比赛。"},
		{"建比赛", "/contests", contestValues("C7011", "关后再建"), "本站已关闭建比赛。"},
		{"私信", "/messages/" + host.handle, url.Values{"body": {"关后再发"}}, "本站已关闭私信。"},
	} {
		code, body := h.postAs(kid, tried.path, tried.body)
		if code != http.StatusForbidden || !strings.Contains(body, tried.want) {
			t.Errorf("%s 关着的时候居然写进去了 = %d，页面里没有 %q:\n%s", tried.what, code, tried.want, short(body))
		}
	}
	// 赛内提交：这条走的是"回到页面给一句原因"，不是错误页。
	if code, body := h.submitContest(kid, "P7010", h.contestID("C7010"), "int main(){return 0;}"); code != http.StatusOK ||
		!strings.Contains(body, "本站已关闭比赛") {
		t.Errorf("赛内提交没有被挡住 = %d:\n%s", code, short(body))
	}

	// 读旧内容照旧：关掉不等于删掉。
	for _, page := range []struct{ path, want string }{
		{"/feed", "关掉之前发的动态"},
		{"/topics", "关掉之前发的帖子"},
		{"/contests/C7010", "关闭测试赛"},
		{"/messages", "关掉之前的私信"},
	} {
		if code, body := h.getAs(kid, page.path); code != http.StatusOK || !strings.Contains(body, page.want) {
			t.Errorf("%s 关掉后读不到旧内容 = %d", page.path, code)
		}
	}
	if code, _ := h.getAs(kid, "/topics/"+strconv.FormatInt(topicID, 10)); code != http.StatusOK {
		t.Errorf("帖子详情页 = %d，关掉开关不该挡住读", code)
	}

	// 表单页给原因，不给一个只会 403 的输入框。
	if _, page := h.getAs(kid, "/feed"); strings.Contains(page, `name="body"`) ||
		!strings.Contains(page, "本站已关闭动态发布") {
		t.Errorf("动态页还留着输入框或没给原因：\n%s", short(page))
	}
	if _, page := h.getAs(kid, "/topics/new"); !strings.Contains(page, "本站已关闭发帖") {
		t.Errorf("发帖页没给原因：\n%s", short(page))
	}

	// 全部打开之后，同一个人又能写了。
	values = siteForm(allSwitches...)
	if code, body := h.postBearer("/admin/site", values, h.adminToken()); code != http.StatusOK {
		t.Fatalf("恢复全部开关 = %d\n%s", code, short(body))
	}
	if code, body := h.postAs(kid, "/feed", url.Values{"body": {"重新打开之后又能发了"}}); code != http.StatusOK {
		t.Fatalf("重新打开后发动态 = %d\n%s", code, short(body))
	}
	if code, _ := h.joinContest(kid, "C7010", ""); code != http.StatusOK {
		t.Fatalf("重新打开后加入比赛 = %d", code)
	}
}

// 站点设置页要能把四个新开关显示成该有的样子：关掉的框不带 checked。
func TestSiteAdminPageShowsSwitches(t *testing.T) {
	h := newHarness(t)
	if code, page := h.get("/admin/site", h.adminToken()); code != http.StatusOK {
		t.Fatalf("站点设置页 = %d", code)
	} else {
		for _, name := range []string{"allow_register", "allow_feed", "allow_topics", "allow_contests", "allow_dm"} {
			if !strings.Contains(page, `name="`+name+`" value="1" checked`) {
				t.Errorf("%s 默认该是勾选状态:\n%s", name, short(page))
			}
		}
	}
	if _, page := h.postBearer("/admin/site", siteForm("allow_register"), h.adminToken()); !strings.Contains(page, "站点设置") {
		t.Error("保存之后没有回到设置页")
	}
	// 关掉的三个框不再 checked，开着的两个还是 checked。
	_, page := h.get("/admin/site", h.adminToken())
	for name, want := range map[string]bool{
		"allow_feed": false, "allow_topics": false, "allow_contests": false, "allow_dm": false, "allow_register": true,
	} {
		checked := strings.Contains(page, `name="`+name+`" value="1" checked`)
		if checked != want {
			t.Errorf("%s checked = %v, want %v", name, checked, want)
		}
	}
}
