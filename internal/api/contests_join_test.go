package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- M14c: joining, teams, and who may see what ---

// contestID reads the numeric id the submit form carries.
func (h *harness) contestID(cid string) int64 {
	h.t.Helper()
	c, err := h.db.FindContest(ctx(), cid)
	if err != nil {
		h.t.Fatalf("find contest %s: %v", cid, err)
	}
	return c.ID
}

func (h *harness) joinContest(s session, cid, code string) (int, string) {
	h.t.Helper()
	return h.postAs(s, "/contests/"+url.PathEscape(cid)+"/join", url.Values{"code": {code}})
}

func (h *harness) submitContest(s session, pid string, contestID int64, source string) (int, string) {
	h.t.Helper()
	return h.postAs(s, "/problems/"+url.PathEscape(pid)+"/submit", url.Values{
		"language": {"cpp"}, "source": {source}, "contest": {strconv.FormatInt(contestID, 10)},
	})
}

// submissionCount lets a test prove that a rejected write stored nothing.
func (h *harness) submissionCount() int {
	h.t.Helper()
	var n int
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT COUNT(*) FROM submissions`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// getPage fetches a page as a session, or anonymously for the zero session.
func (h *harness) getPage(s session, path string) string {
	h.t.Helper()
	if s.handle == "" {
		_, page := h.htmlGet(path, "")
		return page
	}
	_, page := h.getAs(s, path)
	return page
}

func TestJoinContestThroughTheWeb(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostj", permContestCreate)
	player := h.signIn("player")
	h.readyProblem("P6000")

	if _, body := h.createContest(host, contestValues("C6000", "练习赛")); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	if _, body := h.postAs(host, "/contests/C6000/problems", url.Values{"problem": {"P6000"}}); !strings.Contains(body, "已挂上") {
		t.Fatalf("mount:\n%s", body)
	}

	// Before joining, a public contest hides its problems and offers the join form.
	code, page := h.getAs(player, "/contests/C6000")
	if code != http.StatusOK {
		t.Fatalf("home = %d", code)
	}
	if strings.Contains(page, "P6000") {
		t.Errorf("the problem list leaked to a non-member:\n%s", page)
	}
	for _, want := range []string{"加入比赛", `name="code"`, "加入这场比赛之后就能看题目"} {
		if !strings.Contains(page, want) {
			t.Errorf("home missing %q:\n%s", want, page)
		}
	}

	if _, body := h.joinContest(player, "C6000", ""); !strings.Contains(body, "已加入「练习赛」") {
		t.Fatalf("join:\n%s", body)
	}
	// Joining twice is not an error: the button may be double-clicked.
	if _, body := h.joinContest(player, "C6000", ""); !strings.Contains(body, "你已加入") {
		t.Errorf("re-join:\n%s", body)
	}
	if _, page := h.getAs(player, "/contests/C6000"); !strings.Contains(page, "P6000") || !strings.Contains(page, "退出比赛") {
		t.Errorf("a member should see the problems and the way out:\n%s", page)
	}
	// The host sees the roster.
	if page := h.getPage(host, "/contests/C6000"); !strings.Contains(page, "名单（1 人）") || !strings.Contains(page, "player") {
		t.Errorf("roster missing the member:\n%s", page)
	}

	// The contest entry link carries the contest into the problem page.
	got, entry := h.htmlWithCookie(http.MethodGet, "/contests/C6000/p/A", player.cookie)
	if got != http.StatusOK || !strings.Contains(entry, "本场比赛") || !strings.Contains(entry, `name="contest"`) {
		t.Errorf("the contest entry should land on the problem page with the contest attached: %d\n%s", got, entry)
	}
	// The letter is case-insensitive, like the rest of the keys in this app.
	if got, _ := h.htmlWithCookie(http.MethodGet, "/contests/C6000/p/a", player.cookie); got != http.StatusOK {
		t.Errorf("lower-case letter = %d", got)
	}
	page = h.getPage(player, "/problems/P6000?contest="+strconv.FormatInt(h.contestID("C6000"), 10))
	for _, want := range []string{"本场比赛", "C6000", `name="contest"`} {
		if !strings.Contains(page, want) {
			t.Errorf("problem page missing %q:\n%s", want, page)
		}
	}
	// A wrong letter bounces back to the contest instead of 404ing at a URL.
	if body := h.getPage(player, "/contests/C6000/p/Z"); !strings.Contains(body, "这场比赛没有第 Z 题") {
		t.Errorf("unknown letter:\n%s", body)
	}

	// Leaving works, and the ex-member loses the read access again.
	if _, body := h.postAs(player, "/contests/C6000/leave", url.Values{}); !strings.Contains(body, "已退出这场比赛") {
		t.Errorf("leave:\n%s", body)
	}
	if page := h.getPage(player, "/contests/C6000"); strings.Contains(page, "P6000") {
		t.Errorf("an ex-member can still read the problems:\n%s", page)
	}
	// An anonymous join is bounced to the login page with the contest as the way back.
	_, _, hdr := h.postNoRedirect("/contests/C6000/join", url.Values{"code": {"x"}}, "")
	if loc := hdr.Get("Location"); !strings.HasPrefix(loc, "/login?next=%2Fcontests%2FC6000") {
		t.Errorf("anonymous join = %q", loc)
	}
}

func TestPrivateContestNeedsTheCode(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostp", permContestCreate)
	peeker := h.signIn("sneaky")
	h.readyProblem("P6010")

	values := contestValues("C6010", "内部赛")
	values.Set("join_code", "SECRETCODE")
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	if _, body := h.postAs(host, "/contests/C6010/problems", url.Values{"problem": {"P6010"}}); !strings.Contains(body, "已挂上") {
		t.Fatalf("mount:\n%s", body)
	}

	if _, body := h.joinContest(peeker, "C6010", "guess"); !strings.Contains(body, "加入码不对") {
		t.Errorf("wrong code:\n%s", body)
	}
	if _, body := h.joinContest(peeker, "C6010", ""); !strings.Contains(body, "加入码不对") {
		t.Errorf("blank code:\n%s", body)
	}
	// The code appears on exactly one page: the host's own edit form.
	for _, page := range []string{
		h.getPage(peeker, "/contests/C6010"),
		h.getPage(host, "/contests/C6010"),
		h.getPage(session{}, "/contests"),
		h.getPage(host, "/contests"),
	} {
		if strings.Contains(page, "SECRETCODE") {
			t.Errorf("the join code leaked:\n%s", page)
		}
	}
	if page := h.getPage(host, "/contests/C6010/edit"); !strings.Contains(page, "SECRETCODE") {
		t.Errorf("the host could not see their own code:\n%s", page)
	}
	if _, body := h.joinContest(peeker, "C6010", "SECRETCODE"); !strings.Contains(body, "已加入") {
		t.Errorf("right code:\n%s", body)
	}
}

func TestContestSubmissionAttribution(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hosta", permContestCreate)
	player := h.signIn("playera")
	h.readyProblem("P6020")
	h.readyProblem("P6021")

	if _, body := h.createContest(host, contestValues("C6020", "归属赛")); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	if _, body := h.postAs(host, "/contests/C6020/problems", url.Values{"problem": {"P6020"}}); !strings.Contains(body, "已挂上") {
		t.Fatalf("mount:\n%s", body)
	}
	cid := h.contestID("C6020")

	// A non-member cannot claim a contest by naming it in the form.
	if _, body := h.submitContest(player, "P6020", cid, "int main(){}"); !strings.Contains(body, "你还不是这场比赛的成员") {
		t.Errorf("stranger claim:\n%s", body)
	}
	if n := h.submissionCount(); n != 0 {
		t.Errorf("a rejected contest claim stored %d rows", n)
	}

	if _, body := h.joinContest(player, "C6020", ""); !strings.Contains(body, "已加入") {
		t.Fatalf("join:\n%s", body)
	}
	code, body := h.submitContest(player, "P6020", cid, "int main(){}")
	if code != http.StatusOK || !strings.Contains(body, "提交 #1") {
		t.Fatalf("member submit:\n%s", body)
	}
	sub, err := h.db.GetSubmission(ctx(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if sub.ContestID != cid {
		t.Errorf("the submission did not carry the contest: %+v", sub)
	}
	// The result page names the contest and links back to it.
	page := h.getPage(player, "/submissions/1")
	if !strings.Contains(page, "比赛") || !strings.Contains(page, "/contests/C6020/standings") {
		t.Errorf("the submission page lost the contest badge:\n%s", page)
	}

	// The same person submitting outside the contest is practice, contest 0.
	if _, body := h.submitCode(player, "P6020", "cpp", "int main(){}"); !strings.Contains(body, "提交 #2") {
		t.Fatalf("practice submit:\n%s", body)
	}
	if sub, _ := h.db.GetSubmission(ctx(), 2); sub.ContestID != 0 {
		t.Errorf("a plain submit must not be attributed: %+v", sub)
	}

	// A problem that is not mounted cannot be claimed into the contest either.
	if _, body := h.submitContest(player, "P6021", cid, "int main(){}"); !strings.Contains(body, "这道题不在这场比赛里") {
		t.Errorf("unmounted problem:\n%s", body)
	}
	// A contest id from somewhere else is refused, not silently ignored.
	if _, body := h.submitContest(player, "P6020", 987654, "int main(){}"); !strings.Contains(body, "没有这场比赛") {
		t.Errorf("unknown contest id:\n%s", body)
	}
	if n := h.submissionCount(); n != 2 {
		t.Errorf("rejected claims stored rows: %d", n)
	}

	// The window is enforced on the server: a future contest refuses members, and the
	// host may still test their own.
	future := contestValues("C6021", "还没开始")
	future.Set("start_at", time.Now().Add(2*time.Hour).Format(contestLayoutMini))
	future.Set("end_at", time.Now().Add(3*time.Hour).Format(contestLayoutMini))
	if _, body := h.createContest(host, future); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("future create:\n%s", body)
	}
	if _, body := h.postAs(host, "/contests/C6021/problems", url.Values{"problem": {"P6020"}}); !strings.Contains(body, "已挂上") {
		t.Fatalf("future mount:\n%s", body)
	}
	if _, body := h.joinContest(player, "C6021", ""); !strings.Contains(body, "已加入") {
		t.Fatalf("signing up early:\n%s", body)
	}
	if _, body := h.submitContest(player, "P6020", h.contestID("C6021"), "int main(){}"); !strings.Contains(body, "这场比赛还没开始") {
		t.Errorf("submitting before the start:\n%s", body)
	}
	// The member is told why the editor is missing.
	page = h.getPage(player, "/problems/P6020?contest="+strconv.FormatInt(h.contestID("C6021"), 10))
	if !strings.Contains(page, "还没开始") || strings.Contains(page, `<textarea name="source"`) {
		t.Errorf("the problem page should explain the closed editor:\n%s", page)
	}
	if _, body := h.submitContest(host, "P6020", h.contestID("C6021"), "int main(){}"); !strings.Contains(body, "提交 #") {
		t.Errorf("a host should be able to test their own contest:\n%s", body)
	}
}

func TestContestTeamsThroughTheWeb(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostt", permContestCreate)
	one := h.signIn("mate1")
	two := h.signIn("mate2")
	three := h.signIn("mate3")
	four := h.signIn("mate4")
	h.readyProblem("P6030")

	values := contestValues("C6030", "团队赛")
	values.Set("team_size", "2")
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	for _, p := range []session{one, two, three, four} {
		if _, body := h.joinContest(p, "C6030", ""); !strings.Contains(body, "已加入") {
			t.Fatalf("join %s:\n%s", p.handle, body)
		}
	}
	// A team contest puts a newcomer in a team named after them, so nobody ends up
	// outside the board. The roster shows those one-person teams until they group up.
	page := h.getPage(host, "/contests/C6030")
	for _, want := range []string{"mate1", "mate4", "名单（4 人）"} {
		if !strings.Contains(page, want) {
			t.Errorf("roster missing %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "个人：") {
		t.Errorf("everyone got a team, so the solo line should be gone:\n%s", page)
	}
	// The team buttons belong to a member, not to the host looking at their own page.
	matePage := h.getPage(one, "/contests/C6030")
	for _, want := range []string{"建队并进队", "加入同名队", "退出队伍"} {
		if !strings.Contains(matePage, want) {
			t.Errorf("member page missing %q:\n%s", want, matePage)
		}
	}

	teams := func(s session, op, name string) string {
		_, body := h.postAs(s, "/contests/C6030/teams", url.Values{"op": {op}, "name": {name}})
		return body
	}
	if body := teams(one, "create", "红队"); !strings.Contains(body, "已创建队伍 红队") {
		t.Errorf("create:\n%s", body)
	}
	if body := teams(two, "join", "红队"); !strings.Contains(body, "已加入队伍 红队") {
		t.Errorf("join:\n%s", body)
	}
	// Two is the cap, so the third is refused with a reason.
	if body := teams(three, "join", "红队"); !strings.Contains(body, "队伍人数已满") {
		t.Errorf("over the cap:\n%s", body)
	}
	if body := teams(one, "create", "红队"); !strings.Contains(body, "已经有队伍叫 红队") {
		t.Errorf("duplicate name:\n%s", body)
	}
	if body := teams(four, "join", "不存在的队"); !strings.Contains(body, "没有队伍叫 不存在的队") {
		t.Errorf("unknown team:\n%s", body)
	}
	if body := teams(two, "leave", ""); !strings.Contains(body, "已退出队伍") {
		t.Errorf("leave the team:\n%s", body)
	}
	// The roster groups the pair under the team name now.
	page = h.getPage(host, "/contests/C6030")
	if !strings.Contains(page, "红队") {
		t.Errorf("the team is missing from the roster:\n%s", page)
	}

	// A solo contest never offers the team form, and forging the POST is refused in
	// words a person can read.
	single := h.signIn("hostsolo", permContestCreate)
	if _, body := h.createContest(single, contestValues("C6031", "个人赛")); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("solo create:\n%s", body)
	}
	lonely := h.signIn("lonely")
	if _, body := h.joinContest(lonely, "C6031", ""); !strings.Contains(body, "已加入") {
		t.Fatalf("solo join:\n%s", body)
	}
	if page := h.getPage(lonely, "/contests/C6031"); strings.Contains(page, "建队并进队") {
		t.Errorf("a solo contest should not offer teams:\n%s", page)
	}
	if _, body := h.postAs(lonely, "/contests/C6031/teams", url.Values{"op": {"create"}, "name": {"我想组队"}}); !strings.Contains(body, "个人赛") {
		t.Errorf("forged team create:\n%s", body)
	}
	// A non-member cannot use the team form either.
	outsider := h.signIn("nobody")
	if _, body := h.postAs(outsider, "/contests/C6031/teams", url.Values{"op": {"create"}, "name": {"蹭队"}}); !strings.Contains(body, "你还不是这场比赛的成员") {
		t.Errorf("team form as a non-member:\n%s", body)
	}
}

func TestContestMembersMayReadAnUnpublishedProblem(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hosth", permContestCreate, permProblemCreate)
	member := h.signIn("insider")
	stranger := h.signIn("outsider")

	if _, body := h.postAs(host, "/problems", hiddenValues("P6040", "未公开的题目")); !strings.Contains(body, "题目已建好") {
		t.Fatalf("hidden problem:\n%s", body)
	}
	if _, body := h.createContest(host, contestValues("C6040", "封闭赛")); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	if _, body := h.postAs(host, "/contests/C6040/problems", url.Values{"problem": {"P6040"}}); !strings.Contains(body, "已挂上") {
		t.Fatalf("mount:\n%s", body)
	}
	cid := strconv.FormatInt(h.contestID("C6040"), 10)

	if _, body := h.joinContest(member, "C6040", ""); !strings.Contains(body, "已加入") {
		t.Fatalf("join:\n%s", body)
	}
	// The problem is hidden from the bank but readable inside the contest.
	if _, page := h.htmlGet("/problems/P6040", ""); !strings.Contains(page, "404") {
		t.Errorf("an unmounted read of a hidden problem should 404:\n%s", page)
	}
	if code, page := h.htmlWithCookie(http.MethodGet, "/problems/P6040?contest="+cid, member.cookie); code != http.StatusOK ||
		!strings.Contains(page, "未公开的题目") {
		t.Errorf("a member must be able to read the mounted problem: %d\n%s", code, page)
	}
	if code, _ := h.htmlWithCookie(http.MethodGet, "/problems/P6040?contest="+cid, stranger.cookie); code != http.StatusNotFound {
		t.Errorf("a stranger reading a hidden contest problem = %d, want 404", code)
	}
	// Reading is not judging: a stranger cannot submit into the contest either.
	if _, body := h.submitContest(stranger, "P6040", h.contestID("C6040"), "int main(){}"); !strings.Contains(body, "你还不是这场比赛的成员") {
		t.Errorf("a stranger submitting into a private contest:\n%s", body)
	}
	// A member reading it without the contest link still gets 404: the contest is the
	// only door.
	if code, _ := h.getAs(member, "/problems/P6040"); code != http.StatusNotFound {
		t.Errorf("the plain URL of a hidden problem = %d, want 404", code)
	}
}

func TestContestProblemPageShowsOnlyYourOwnRecentRows(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostr", permContestCreate)
	first := h.signIn("rfirst")
	second := h.signIn("rsecond")
	h.readyProblem("P6050")

	if _, body := h.createContest(host, contestValues("C6050", "排行榜前置赛")); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	if _, body := h.postAs(host, "/contests/C6050/problems", url.Values{"problem": {"P6050"}}); !strings.Contains(body, "已挂上") {
		t.Fatalf("mount:\n%s", body)
	}
	cid := h.contestID("C6050")
	for _, p := range []session{first, second} {
		if _, body := h.joinContest(p, "C6050", ""); !strings.Contains(body, "已加入") {
			t.Fatalf("join %s:\n%s", p.handle, body)
		}
		if _, body := h.submitContest(p, "P6050", cid, "int main(){}"); !strings.Contains(body, "提交 #") {
			t.Fatalf("submit %s:\n%s", p.handle, body)
		}
	}
	// 比赛还在进行、又封着榜：普通题面页也不能把别人的成绩当成公开动态露出来。
	plain := h.getPage(first, "/problems/P6050")
	if strings.Contains(plain, "rsecond") {
		t.Errorf("the ordinary problem page leaked another player's verdict during a running OI:\n%s", plain)
	}
	if !strings.Contains(plain, "rfirst") {
		t.Errorf("the ordinary problem page lost your own row:\n%s", plain)
	}
	// Inside the contest it is only yours, because other verdicts are the answer sheet.
	inside := h.getPage(first, "/problems/P6050?contest="+strconv.FormatInt(cid, 10))
	if strings.Contains(inside, "rsecond") {
		t.Errorf("the contest view leaked another member's verdicts:\n%s", inside)
	}
	if !strings.Contains(inside, "rfirst") {
		t.Errorf("the contest view lost your own rows:\n%s", inside)
	}
}
