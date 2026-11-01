package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// createTopic posts the new-topic form as a browser and returns the page it lands on.
func (h *harness) createTopic(s session, values url.Values) (int, string) {
	h.t.Helper()
	if values.Get("kind") == "" {
		values.Set("kind", "community")
	}
	return h.postAs(s, "/topics", values)
}

func TestTopicPostingThroughTheWeb(t *testing.T) {
	h := newHarness(t)
	writer := h.signIn("poster")

	// Browsing is open to everyone, and says so.
	if code, body := h.htmlGet("/topics", ""); code != http.StatusOK || !strings.Contains(body, "还没有社区贴") {
		t.Fatalf("empty list = %d %s", code, body)
	}
	if _, page := h.htmlGet("/topics", ""); !strings.Contains(page, "登录</a> 后才能发帖") {
		t.Errorf("an anonymous reader should be told posting needs a login:\n%s", page)
	}
	// The write doors lead to the login page with the destination kept.
	code, _, hdr := h.postNoRedirect("/topics", url.Values{"kind": {"community"}, "title": {"匿名"}}, "")
	loc := hdr.Get("Location")
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?") {
		t.Errorf("anonymous create = %d %q", code, loc)
	}
	// The form sends an anonymous browser to the login page, destination included.
	_, page := h.htmlGet("/topics/new", "")
	if !strings.Contains(page, "<h1>登录</h1>") || strings.Contains(page, `name="title"`) {
		t.Errorf("the anonymous form should be the login page:\n%s", page)
	}

	values := url.Values{"kind": {"community"}, "title": {"周末一起刷题吗"},
		"body": {"## 约题\n\n$O(n\\log n)$ 的那道今晚一起打。"}}
	code, body := h.createTopic(writer, values)
	if code != http.StatusOK || !strings.Contains(body, "已发布") {
		t.Fatalf("create = %d %s", code, body)
	}
	for _, want := range []string{"周末一起刷题吗", "poster", "社区", `<span class="math inline">`} {
		if !strings.Contains(body, want) {
			t.Errorf("topic page missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "全部社区贴") {
		t.Errorf("the post should point back at its list:\n%s", body)
	}
	// The list page now carries it.
	if _, page := h.getAs(writer, "/topics"); !strings.Contains(page, "周末一起刷题吗") || !strings.Contains(page, "发帖") {
		t.Errorf("the list lost the new topic:\n%s", page)
	}
	// A signed-in reader gets the real form, with both kinds offered.
	if _, page := h.getAs(writer, "/topics/new"); !strings.Contains(page, `value="community"`) ||
		!strings.Contains(page, `value="help"`) {
		t.Errorf("the form lost the kind selector:\n%s", page)
	}
}

func TestHelpTopicsHangOnTheirProblem(t *testing.T) {
	h := newHarness(t)
	asker := h.signIn("asker")
	h.readyProblem("P8000")

	// The problem page advertises the 求助贴 entry.
	if _, page := h.getAs(asker, "/problems/P8000"); !strings.Contains(page, "/problems/P8000/topics") {
		t.Errorf("the problem page lost the topic entry:\n%s", page)
	}
	// Entering from there locks the form onto that problem and the help kind.
	form := h.getPage(asker, "/problems/P8000/topics")
	if !strings.Contains(form, "这道题还没有人提问") {
		t.Errorf("empty help list:\n%s", form)
	}
	newLink := h.getPage(asker, "/topics/new?problem=P8000")
	for _, want := range []string{`name="locked" value="1"`, `value="P8000"`, "readonly", `value="help" checked`} {
		if !strings.Contains(newLink, want) {
			t.Errorf("the locked form is missing %q:\n%s", want, newLink)
		}
	}

	code, body := h.createTopic(asker, url.Values{
		"kind": {"help"}, "title": {"P8000 为什么 WA"}, "body": {"取模写错了？"},
		"problem": {"P8000"}, "locked": {"1"},
	})
	if code != http.StatusOK || !strings.Contains(body, "已发布") {
		t.Fatalf("help create = %d %s", code, body)
	}
	if !strings.Contains(body, "求助") || !strings.Contains(body, "/problems/P8000") {
		t.Errorf("the help post should name its kind and its problem:\n%s", body)
	}
	if !strings.Contains(body, "这道题的全部求助") {
		t.Errorf("no way back to the problem's list:\n%s", body)
	}
	// It shows on the problem's list and not on the 社区贴 page.
	if _, page := h.getAs(asker, "/problems/P8000/topics"); !strings.Contains(page, "P8000 为什么 WA") {
		t.Errorf("the problem list lost it:\n%s", page)
	}
	if _, page := h.getAs(asker, "/topics"); strings.Contains(page, "P8000 为什么 WA") {
		t.Errorf("a 求助贴 leaked onto the 社区贴 page:\n%s", page)
	}
}

func TestTopicFormValidation(t *testing.T) {
	h := newHarness(t)
	writer := h.signIn("checker2")
	h.readyProblem("P8010")

	cases := []struct {
		values url.Values
		want   string
	}{
		{url.Values{"kind": {"community"}, "title": {"  "}}, "标题不能是空的"},
		{url.Values{"kind": {"poll"}, "title": {"投票贴"}}, "帖子类型只认求助或社区"},
		{url.Values{"kind": {"help"}, "title": {"没挂题"}}, "求助贴要写清是哪道题"},
		{url.Values{"kind": {"help"}, "title": {"挂错题"}, "problem": {"NOPE"}}, "找不到题目 NOPE"},
		{url.Values{"kind": {"community"}, "title": {strings.Repeat("长", 201)}}, "标题最长 200 个字符"},
	}
	for _, c := range cases {
		c.values.Set("body", "正文")
		code, body := h.createTopic(writer, c.values)
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%v = %d, want %q:\n%s", c.values, code, c.want, body)
		}
		// A rejected post keeps what was typed.
		if !strings.Contains(body, `name="body"`) {
			t.Errorf("the body textarea vanished:\n%s", body)
		}
	}
	if n := h.topicCount(); n != 0 {
		t.Errorf("rejected forms created %d topics", n)
	}
}

func TestTopicDeletePermissions(t *testing.T) {
	h := newHarness(t)
	author := h.signIn("author3")
	stranger := h.signIn("stranger3")
	mod := h.signIn("mod3", permTopicModerate)

	id := h.createTopicID(author, url.Values{"kind": {"community"}, "title": {"我的帖子"}, "body": {"正文"}})

	// A stranger cannot take it down, and is told which key would let them.
	if code, page := h.postAs(stranger, fmt.Sprintf("/topics/%d/delete", id), url.Values{}); code != http.StatusForbidden ||
		!strings.Contains(page, permTopicModerate) {
		t.Errorf("stranger delete = %d:\n%s", code, page)
	}
	if h.topicCount() != 1 {
		t.Error("a refused delete still removed the row")
	}
	// The moderator may, and the author may delete their own.
	if _, page := h.postAs(mod, fmt.Sprintf("/topics/%d/delete", id), url.Values{}); !strings.Contains(page, "已删除帖子") {
		t.Errorf("moderator delete:\n%s", page)
	}
	if h.topicCount() != 0 {
		t.Errorf("the topic survived a moderator delete")
	}

	id = h.createTopicID(author, url.Values{"kind": {"community"}, "title": {"第二个"}, "body": {""}})
	if _, page := h.postAs(author, fmt.Sprintf("/topics/%d/delete", id), url.Values{}); !strings.Contains(page, "已删除帖子") {
		t.Errorf("author delete:\n%s", page)
	}
	// A missing post is a 404, and a nonsense id is a 400 — both as pages.
	if code, _ := h.htmlGet("/topics/99999", ""); code != http.StatusNotFound {
		t.Errorf("missing topic = %d", code)
	}
	if code, _ := h.htmlGet("/topics/not-a-number", ""); code != http.StatusBadRequest {
		t.Errorf("bad id = %d", code)
	}
	// The delete button only appears for someone who may use it.
	id = h.createTopicID(author, url.Values{"kind": {"community"}, "title": {"第三个"}, "body": {""}})
	path := fmt.Sprintf("/topics/%d", id)
	if page := h.getPage(stranger, path); strings.Contains(page, "删除帖子") {
		t.Errorf("a stranger was offered the delete button:\n%s", page)
	}
	if page := h.getPage(author, path); !strings.Contains(page, "删除帖子") {
		t.Errorf("the author lost their own delete button:\n%s", page)
	}

	// 列表页上也要能删：管理员清理版面本来就该在列表里做，而不是逐条点进详情页。
	row := fmt.Sprintf("/topics/%d/delete", id)
	if page := h.getPage(stranger, "/topics"); strings.Contains(page, row) {
		t.Errorf("a stranger was offered the delete button on the list:\n%s", page)
	}
	for _, who := range []struct {
		name string
		s    session
	}{{"author", author}, {"moderator", mod}} {
		if page := h.getPage(who.s, "/topics"); !strings.Contains(page, row) {
			t.Errorf("the %s lost the delete button on the list:\n%s", who.name, page)
		}
	}
	if _, page := h.htmlGet("/topics", ""); strings.Contains(page, "<th>操作</th>") {
		t.Errorf("anonymous readers were given an empty 操作 column:\n%s", page)
	}
	// 从列表删掉之后，详情页也一起没了。
	if _, page := h.postAs(mod, path+"/delete", url.Values{}); !strings.Contains(page, "已删除帖子") {
		t.Errorf("delete from the list page:\n%s", page)
	}
	if code, _ := h.htmlGet(path, ""); code != http.StatusNotFound {
		t.Errorf("topic after the list delete = %d, want 404", code)
	}
}

func TestTopicRateLimit(t *testing.T) {
	h := newHarness(t)
	writer := h.signIn("flood")
	for i := 1; i <= maxTopicsHour; i++ {
		_, body := h.createTopic(writer, url.Values{"kind": {"community"},
			"title": {fmt.Sprintf("水贴 %d", i)}, "body": {""}})
		if !strings.Contains(body, "已发布") {
			t.Fatalf("post %d refused:\n%s", i, body)
		}
	}
	_, body := h.createTopic(writer, url.Values{"kind": {"community"},
		"title": {"多一条"}, "body": {""}})
	if !strings.Contains(body, "发得太快了") {
		t.Errorf("the %dth post was accepted:\n%s", maxTopicsHour+1, body)
	}
	if n := h.topicCount(); n != maxTopicsHour {
		t.Errorf("a refused post created a row: %d", n)
	}
	// The limit is per person, not global.
	other := h.signIn("quiet2")
	if _, body := h.createTopic(other, url.Values{"kind": {"community"}, "title": {"别人的"}, "body": {""}}); !strings.Contains(body, "已发布") {
		t.Errorf("another user was punished for the flood:\n%s", body)
	}
}

func (h *harness) topicCount() int {
	h.t.Helper()
	var n int
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT COUNT(*) FROM topics`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// topicProblemID reads a problem's row id, for tests that write topics directly.
func (h *harness) topicProblemID(pid string) int64 {
	h.t.Helper()
	p, err := h.db.FindProblem(ctx(), pid)
	if err != nil {
		h.t.Fatal(err)
	}
	return p.ID
}

// createTopicID posts the form without following the redirect, so the id comes from
// the Location the browser would be sent to.
func (h *harness) createTopicID(s session, values url.Values) int64 {
	h.t.Helper()
	if values.Get("kind") == "" {
		values.Set("kind", "community")
	}
	code, body, hdr := h.postForm("/topics", values, s.cookie, s.csrf)
	if code != http.StatusSeeOther {
		h.t.Fatalf("create = %d %s", code, body)
	}
	var id int64
	if _, err := fmt.Sscanf(strings.TrimPrefix(hdr.Get("Location"), "/topics/"), "%d", &id); err != nil || id <= 0 {
		h.t.Fatalf("bad redirect target %q", hdr.Get("Location"))
	}
	return id
}

// --- M17c: the comment thread ---

func (h *harness) comment(s session, id int64, body string) (int, string) {
	h.t.Helper()
	return h.postAs(s, fmt.Sprintf("/topics/%d/comment", id), url.Values{"body": {body}})
}

func TestTopicCommentsThroughTheWeb(t *testing.T) {
	h := newHarness(t)
	author := h.signIn("topic_author")
	helper := h.signIn("topic_helper")
	stranger := h.signIn("topic_stranger")

	id := h.createTopicID(author, url.Values{"kind": {"community"}, "title": {"求助：读入顺序"}, "body": {"$n$ 组数据"}})

	// An anonymous browser reads the thread but is not offered the composer.
	page := h.getPage(session{}, fmt.Sprintf("/topics/%d", id))
	if !strings.Contains(page, "登录后才能评论") && !strings.Contains(page, "登录</a> 后才能评论") {
		t.Errorf("an anonymous reader should be pointed at the login page:\n%s", page)
	}
	if strings.Contains(page, `name="body"`) {
		t.Errorf("a composer was shown to an anonymous visitor:\n%s", page)
	}
	// Writing needs a session, and the way back is kept.
	code, _, hdr := h.postNoRedirect(fmt.Sprintf("/topics/%d/comment", id), url.Values{"body": {"匿名评论"}}, "")
	if code != http.StatusSeeOther || !strings.Contains(hdr.Get("Location"), "/login?next=%2Ftopics%2F") {
		t.Errorf("anonymous comment = %d %q", code, hdr.Get("Location"))
	}

	if _, body := h.comment(helper, id, "把 `scanf` 的返回值判一下"); !strings.Contains(body, "已发布评论") {
		t.Fatalf("comment:\n%s", body)
	}
	if _, body := h.comment(author, id, "谢谢，**解决了**"); !strings.Contains(body, "已发布评论") {
		t.Fatalf("author comment:\n%s", body)
	}
	// The thread reads in the order it happened, and Markdown works in it.
	page = h.getPage(stranger, fmt.Sprintf("/topics/%d", id))
	if !strings.Contains(page, "评论（2）") || !strings.Contains(page, "<strong>解决了</strong>") {
		t.Errorf("thread wrong:\n%s", page)
	}
	if strings.Index(page, "返回值判一下") > strings.Index(page, "解决了") {
		t.Errorf("replies should be oldest first:\n%s", page)
	}
	// The list page carries the count, so a reader can pick a conversation.
	if _, page := h.getAs(stranger, "/topics"); !strings.Contains(page, "求助：读入顺序") {
		t.Errorf("the list lost the topic:\n%s", page)
	}
	if !strings.Contains(h.getPage(stranger, "/topics"), ">2<") {
		t.Errorf("the list should show two comments:\n%s", h.getPage(stranger, "/topics"))
	}

	// An empty reply is refused, and the reason is shown on the page it bounces back
	// to — not on a later fetch of the same thread, which has nothing to report.
	code, body := h.comment(helper, id, "   ")
	if strings.Contains(body, "已发布评论") {
		t.Errorf("an empty comment was accepted:\n%s", body)
	}
	if !strings.Contains(body, "说点什么吧") {
		t.Errorf("the refusal said nothing (code %d):\n%s", code, body)
	}

	// Deleting: the author of the reply may, a stranger may not.
	cid := h.commentID(helper, id, "返回值判一下")
	if code, _ := h.postAs(stranger, fmt.Sprintf("/topics/comments/%d/delete", cid), url.Values{}); code != http.StatusForbidden {
		t.Errorf("a stranger deleted someone's reply = %d", code)
	}
	if _, body := h.postAs(helper, fmt.Sprintf("/topics/comments/%d/delete", cid), url.Values{}); !strings.Contains(body, "已删除评论") {
		t.Errorf("author delete of a reply:\n%s", body)
	}
	if !strings.Contains(h.getPage(stranger, fmt.Sprintf("/topics/%d", id)), "评论（1）") {
		t.Errorf("the count did not follow the delete:\n%s", h.getPage(stranger, fmt.Sprintf("/topics/%d", id)))
	}
	// A moderator may take down someone else's reply.
	cid = h.commentID(author, id, "解决了")
	if _, body := h.postAs(h.signIn("topic_mod", permTopicModerate), fmt.Sprintf("/topics/comments/%d/delete", cid), url.Values{}); !strings.Contains(body, "已删除评论") {
		t.Errorf("moderator delete of a reply:\n%s", body)
	}
	if !strings.Contains(h.getPage(stranger, fmt.Sprintf("/topics/%d", id)), "还没有评论") {
		t.Errorf("the thread should be empty now:\n%s", h.getPage(stranger, fmt.Sprintf("/topics/%d", id)))
	}
}

// commentID finds a reply's id by the delete form it carries.
func (h *harness) commentID(s session, topicID int64, needle string) int64 {
	h.t.Helper()
	page := h.getPage(s, fmt.Sprintf("/topics/%d", topicID))
	i := strings.Index(page, needle)
	if i < 0 {
		h.t.Fatalf("no reply containing %q:\n%s", needle, page)
	}
	// The delete form for that reply appears just before its body.
	before := page[:i]
	j := strings.LastIndex(before, `/topics/comments/`)
	if j < 0 {
		h.t.Fatalf("no delete form before %q:\n%s", needle, page)
	}
	var id int64
	if _, err := fmt.Sscanf(before[j+len("/topics/comments/"):], "%d", &id); err != nil || id <= 0 {
		h.t.Fatalf("bad comment id in %q", before[j:])
	}
	return id
}

func TestTopicCommentRateLimit(t *testing.T) {
	h := newHarness(t)
	writer := h.signIn("chatty")
	id := h.createTopicID(writer, url.Values{"kind": {"community"}, "title": {"灌水贴"}, "body": {""}})
	for i := 1; i <= maxCommentsHour; i++ {
		if _, body := h.comment(writer, id, fmt.Sprintf("%d 楼", i)); !strings.Contains(body, "已发布评论") {
			t.Fatalf("comment %d refused:\n%s", i, body)
		}
	}
	_, body := h.comment(writer, id, "多一条")
	if !strings.Contains(body, "评论得太快了") {
		t.Errorf("comment %d was accepted:\n%s", maxCommentsHour+1, body)
	}
	if n := h.commentCount(); n != maxCommentsHour {
		t.Errorf("a refused comment created a row: %d", n)
	}
}

func TestCommentOnDeletedTopicGoesNowhere(t *testing.T) {
	h := newHarness(t)
	author := h.signIn("gone_author")
	other := h.signIn("gone_other")
	id := h.createTopicID(author, url.Values{"kind": {"community"}, "title": {"要被删的帖子"}, "body": {""}})

	if _, body := h.postAs(author, fmt.Sprintf("/topics/%d/delete", id), url.Values{}); !strings.Contains(body, "已删除帖子") {
		t.Fatalf("delete:\n%s", body)
	}
	// The form was open when the topic was still there; posting now must not
	// resurrect it or strand a comment.
	if _, body := h.comment(other, id, "有人吗"); !strings.Contains(body, "这个帖子已经不在了") {
		t.Errorf("a comment on a deleted topic:\n%s", body)
	}
	if n := h.commentCount(); n != 0 {
		t.Errorf("%d orphan comments", n)
	}
}

func (h *harness) commentCount() int {
	h.t.Helper()
	var n int
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT COUNT(*) FROM topic_comments`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// --- M17d: the front page ---

func TestHomeShowsTheTenNewestTopics(t *testing.T) {
	h := newHarness(t)
	h.signIn("home_author")
	h.readyProblem("P8100")
	authorID := h.user("home_author").ID

	for i := 1; i <= 12; i++ {
		kind, pid := store.TopicCommunity, int64(0)
		if i%3 == 0 {
			kind, pid = store.TopicHelp, h.topicProblemID("P8100")
		}
		if _, err := h.db.CreateTopic(ctx(), kind, fmt.Sprintf("首页贴 %02d", i), "正文", authorID, pid); err != nil {
			t.Fatal(err)
		}
	}
	page := h.getPage(session{}, "/")
	if !strings.Contains(page, `class="topic-list"`) {
		t.Fatalf("the front page lost the topic list:\n%s", page)
	}
	if strings.Count(page, `class="topic-row" href="/topics/`) != homeTopics {
		t.Errorf("expected %d topics, got %d:\n%s", homeTopics, strings.Count(page, `class="topic-row" href="/topics/`), page)
	}
	// The twelve newest are ordered newest-first, so #12 is in and #02 is out.
	if !strings.Contains(page, "首页贴 12") || strings.Contains(page, "首页贴 02") {
		t.Errorf("the window is not the ten newest:\n%s", page)
	}
	// Both kinds appear, and a 求助贴 says which problem it is about.
	if !strings.Contains(page, "求助") || !strings.Contains(page, "社区") || !strings.Contains(page, "P8100") {
		t.Errorf("the topics lost their kind or problem:\n%s", page)
	}
	if !strings.Contains(page, "/topics/new") || !strings.Contains(page, "登录后才能发") {
		t.Errorf("the anonymous hint is missing:\n%s", page)
	}
}
