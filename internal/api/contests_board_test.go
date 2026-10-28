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

// --- M14d: the independent board ---

// scoreSubmission finishes a submission with real per-case points, so a board test writes what
// a judge would have written instead of a summary number.
func (h *harness) scoreSubmission(sid int64, verdict string, earned ...int) {
	h.t.Helper()
	if _, err := h.db.UpsertJudge(ctx(), "box-contest", []string{"cpp"}, nil, "", nil); err != nil {
		h.t.Fatal(err)
	}
	if ok, err := h.db.SetStatus(ctx(), sid, "queued", "compiling", 1); err != nil || !ok {
		h.t.Fatalf("claim = %v %v", ok, err)
	}
	runs := make([]store.Run, 0, len(earned))
	res := store.Result{Verdict: verdict, TimeMS: 10, MemKB: 100}
	for i, points := range earned {
		caseVerdict := "AC"
		if points == 0 {
			caseVerdict = "WA"
			if res.FailedCase == "" {
				res.FailedCase = "c" + strconv.Itoa(i+1)
			}
		}
		res.Score += points
		runs = append(runs, store.Run{
			Ordinal: i + 1, CaseName: "c" + strconv.Itoa(i+1), Verdict: caseVerdict,
			Score: points, TimeMS: 10, MemKB: 100,
		})
	}
	if err := h.db.RecordResult(ctx(), sid, res, runs); err != nil {
		h.t.Fatal(err)
	}
}

// submissionID reads the number out of the page the browser landed on.
func (h *harness) submissionID(body string) int64 {
	h.t.Helper()
	i := strings.Index(body, "提交 #")
	if i < 0 {
		h.t.Fatalf("no submission number in:\n%s", body)
	}
	rest := body[i+len("提交 #"):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	n, err := strconv.ParseInt(rest[:j], 10, 64)
	if err != nil {
		h.t.Fatalf("bad submission number in:\n%s", body)
	}
	return n
}

// submitAndJudge is the one-line way to put a scored attempt on a board.
func (h *harness) submitAndJudge(s session, pid string, contestID int64, verdict string, earned ...int) {
	h.t.Helper()
	_, body := h.submitContest(s, pid, contestID, "int main(){}")
	h.scoreSubmission(h.submissionID(body), verdict, earned...)
}

// mountContestProblems hangs the listed problems on a contest as its host.
func (h *harness) mountContestProblems(host session, cid string, pids ...string) {
	h.t.Helper()
	for _, pid := range pids {
		if _, body := h.postAs(host, "/contests/"+cid+"/problems", url.Values{"problem": {pid}}); !strings.Contains(body, "已挂上") {
			h.t.Fatalf("mount %s into %s:\n%s", pid, cid, body)
		}
	}
}

func (h *harness) joinAll(cid string, players ...session) {
	h.t.Helper()
	for _, p := range players {
		if _, body := h.joinContest(p, cid, ""); !strings.Contains(body, "已加入") {
			h.t.Fatalf("join %s into %s:\n%s", p.handle, cid, body)
		}
	}
}

func TestContestStandingsIOIMode(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostoi", permContestCreate)
	best := h.signIn("oi_best")
	bundler := h.signIn("oi_bundler")
	half := h.signIn("oi_half")
	// P7000 is a single 10-point case: the host sets that weight through the case form.
	if _, body := h.createProblem(url.Values{"pid": {"P7000"}, "title": {"十分题"}}); !strings.Contains(body, "题目已建好") {
		t.Fatalf("problem:\n%s", body)
	}
	if code, body := h.do(http.MethodPost, "/problems/P7000/cases/upload", h.adminToken(),
		zipOf(map[string]string{"1.in": "1\n", "1.out": "1\n"}), "application/zip"); code != http.StatusOK {
		t.Fatalf("cases = %d %s", code, body)
	}
	if _, body := h.postBearer("/problems/P7000/cases/1/edit", url.Values{
		"name": {"1"}, "score": {"10"}, "group": {""}, "input": {"1\n"}, "output": {"1\n"},
	}, h.adminToken()); !strings.Contains(body, "已保存用例") {
		t.Fatalf("case weight:\n%s", body)
	}
	h.readyProblem("P7001") // two judged cases, worth 1 each until the contest rescales it

	values := contestValues("C7000", "IOI 榜")
	values.Set("mode", store.ContestIOI)
	values.Set("standing_open", "1")
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	h.mountContestProblems(host, "C7000", "P7000")
	// The bundle is mounted with a 10-point override, so one passed case of two pays 5.
	if _, body := h.postAs(host, "/contests/C7000/problems", url.Values{
		"problem": {"P7001"}, "points": {"10"},
	}); !strings.Contains(body, "已挂上") {
		t.Fatalf("mount with override:\n%s", body)
	}
	cid := h.contestID("C7000")
	h.joinAll("C7000", best, bundler, half)

	h.submitAndJudge(best, "P7000", cid, "AC", 10)
	h.submitAndJudge(bundler, "P7001", cid, "AC", 1, 1)
	h.submitAndJudge(half, "P7000", cid, "WA", 0)
	h.submitAndJudge(half, "P7001", cid, "WA", 1, 0)

	code, page := h.getAs(best, "/contests/C7000/standings")
	if code != http.StatusOK {
		t.Fatalf("standings = %d", code)
	}
	for _, want := range []string{"成绩榜", "IOI 榜", "oi_best", "oi_half", "每题取最高分"} {
		if !strings.Contains(page, want) {
			t.Errorf("board missing %q:\n%s", want, page)
		}
	}
	// 满分那格在色阶最绿的端点，半分那格还在红→绿中间；数字不再带 /100 的分母。
	if !strings.Contains(page, `class="score sc9" href="/problems/P7000" title="十分题 · 本题满分 10">10</a>`) {
		t.Errorf("a full-mark cell should be the green end of the scale:\n%s", page)
	}
	if !strings.Contains(page, `class="score sc4" href="/problems/P7001"`) || !strings.Contains(page, `>5</a>`) {
		t.Errorf("a half-scored cell should sit mid-scale and print only its number:\n%s", page)
	}
	// 时间是灰字另起一行，自己不带颜色。
	if !strings.Contains(page, `<span class="when">`) {
		t.Errorf("the clock should be printed under the score:\n%s", page)
	}
	// The perfect score leads, the half-scorer trails.
	if strings.Index(page, ">oi_best</td>") > strings.Index(page, ">oi_half</td>") {
		t.Errorf("oi_best should be listed above oi_half:\n%s", page)
	}
	if !strings.Contains(page, `class="me"`) {
		t.Errorf("your own row should be highlighted:\n%s", page)
	}
}

func TestContestStandingsSealed(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostseal", permContestCreate)
	first := h.signIn("seal_first")
	second := h.signIn("seal_second")
	h.readyProblem("P7010")

	values := contestValues("C7010", "封榜赛")
	values.Set("mode", store.ContestIOI)
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	h.mountContestProblems(host, "C7010", "P7010")
	cid := h.contestID("C7010")
	h.joinAll("C7010", first, second)
	h.submitAndJudge(first, "P7010", cid, "AC", 1, 1)
	h.submitAndJudge(second, "P7010", cid, "WA", 0)

	// Sealed: each member sees exactly one row, their own, with a true rank.
	leader := h.getPage(first, "/contests/C7010/standings")
	for _, want := range []string{"封榜中", "你现在第 1 名", "seal_first"} {
		if !strings.Contains(leader, want) {
			t.Errorf("sealed board missing %q:\n%s", want, leader)
		}
	}
	if strings.Contains(leader, "seal_second") {
		t.Errorf("the sealed board leaked another competitor:\n%s", leader)
	}
	trailer := h.getPage(second, "/contests/C7010/standings")
	if !strings.Contains(trailer, "你现在第 2 名") || strings.Contains(trailer, "seal_first") {
		t.Errorf("the second place saw the wrong board:\n%s", trailer)
	}
	// The host sees the whole field, and knows it is sealed.
	ownerPage := h.getPage(host, "/contests/C7010/standings")
	if !strings.Contains(ownerPage, "seal_first") || !strings.Contains(ownerPage, "seal_second") ||
		!strings.Contains(ownerPage, "完整榜单") {
		t.Errorf("the host could not see the sealed field:\n%s", ownerPage)
	}
	// A stranger gets the notice, not an empty table and not the data.
	outside := h.getPage(h.signIn("seal_out"), "/contests/C7010/standings")
	if strings.Contains(outside, "seal_first") || !strings.Contains(outside, "封榜") {
		t.Errorf("an outsider read the sealed board:\n%s", outside)
	}
	// The contest page links to the board for anyone, sealed or not.
	if page := h.getPage(first, "/contests/C7010"); !strings.Contains(page, "/contests/C7010/standings") {
		t.Errorf("the contest page lost the board link:\n%s", page)
	}

	// Opening the board is a decision the host makes, never an automatic event.
	open := contestValues("C7010", "封榜赛")
	open.Set("mode", store.ContestIOI)
	open.Set("standing_open", "1")
	if _, body := h.postAs(host, "/contests/C7010", open); !strings.Contains(body, "已保存") {
		t.Fatalf("open the board:\n%s", body)
	}
	now := h.getPage(second, "/contests/C7010/standings")
	if !strings.Contains(now, "seal_first") {
		t.Errorf("after the host opened the board it should show everyone:\n%s", now)
	}
	if strings.Contains(now, "你现在第") {
		t.Errorf("an open board does not need the one-row notice:\n%s", now)
	}
}

func TestContestStandingsACM(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostacm", permContestCreate)
	fast := h.signIn("acm_fast")
	slow := h.signIn("acm_slow")
	h.readyProblem("P7020")
	h.readyProblem("P7021")

	values := contestValues("C7020", "ACM 榜")
	values.Set("mode", store.ContestACM)
	values.Set("penalty_s", "1200")
	values.Set("standing_open", "1")
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	h.mountContestProblems(host, "C7020", "P7020", "P7021")
	cid := h.contestID("C7020")
	h.joinAll("C7020", fast, slow)

	// Same number of solves; the fast one carries less penalty.
	h.submitAndJudge(fast, "P7020", cid, "AC", 1, 1)
	h.submitAndJudge(slow, "P7020", cid, "AC", 1, 1)
	h.submitAndJudge(slow, "P7021", cid, "WA", 0)
	h.submitAndJudge(slow, "P7021", cid, "WA", 0)

	page := h.getPage(fast, "/contests/C7020/standings")
	for _, want := range []string{"ACM", "解题", "罚时", "acm_fast", "acm_slow", "不罚时"} {
		if !strings.Contains(page, want) {
			t.Errorf("ACM board missing %q:\n%s", want, page)
		}
	}
	if strings.Index(page, ">acm_fast</td>") > strings.Index(page, ">acm_slow</td>") {
		t.Errorf("equal solves should be ordered by penalty:\n%s", page)
	}
	// 没做出来的题：数字是尝试次数，颜色在最红的一端，也没有时间。
	if !strings.Contains(page, `class="score sc0" href="/problems/P7021" title="case ready · 未解出">2</a>`) {
		t.Errorf("the unsolved problem should print two attempts at the red end:\n%s", page)
	}
	// 做出来的题：最绿的一端，首过时刻走下面的灰字。
	if !strings.Contains(page, `class="score sc9" href="/problems/P7020" title="case ready · 已解出">1</a>`) {
		t.Errorf("a solved problem should be green:\n%s", page)
	}
	if !strings.Contains(page, `<span class="when">`) {
		t.Errorf("the first-accept clock should be printed under the number:\n%s", page)
	}
}

func TestContestStandingsTeamMode(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostteam", permContestCreate)
	a, b := h.signIn("team_a"), h.signIn("team_b")
	lonely := h.signIn("team_c")
	h.readyProblem("P7030")

	values := contestValues("C7030", "团体榜")
	values.Set("mode", store.ContestIOI)
	values.Set("team_size", "2")
	values.Set("standing_open", "1")
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	h.mountContestProblems(host, "C7030", "P7030")
	cid := h.contestID("C7030")
	h.joinAll("C7030", a, b, lonely)

	if _, body := h.postAs(a, "/contests/C7030/teams", url.Values{"op": {"create"}, "name": {"冠军队"}}); !strings.Contains(body, "已创建队伍") {
		t.Fatalf("create team:\n%s", body)
	}
	if _, body := h.postAs(b, "/contests/C7030/teams", url.Values{"op": {"join"}, "name": {"冠军队"}}); !strings.Contains(body, "已加入队伍") {
		t.Fatalf("join team:\n%s", body)
	}

	// Each half solves it; as one competitor the team takes the full score. The
	// loner only gets half, and its row is its own handle.
	h.submitAndJudge(a, "P7030", cid, "AC", 1, 1)
	h.submitAndJudge(lonely, "P7030", cid, "WA", 1, 0)

	page := h.getPage(lonely, "/contests/C7030/standings")
	for _, want := range []string{"冠军队", "team_a、team_b", "队伍名次", "team_c", `class="score sc9" href="/problems/P7030"`} {
		if !strings.Contains(page, want) {
			t.Errorf("team board missing %q:\n%s", want, page)
		}
	}
	if strings.Index(page, ">冠军队</td>") > strings.Index(page, ">team_c</td>") {
		t.Errorf("the team that solved it should outrank the half-scorer:\n%s", page)
	}
}

func TestContestStandingsIgnoreUnfinishedRows(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("hostpend", permContestCreate)
	player := h.signIn("pending_one")
	h.readyProblem("P7040")

	values := contestValues("C7040", "判分中榜")
	values.Set("mode", store.ContestIOI)
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	h.mountContestProblems(host, "C7040", "P7040")
	cid := h.contestID("C7040")
	h.joinAll("C7040", player)

	// Submitted but not judged yet: the board is honest about having nothing.
	_, body := h.submitContest(player, "P7040", cid, "int main(){}")
	if !strings.Contains(body, "提交 #") {
		t.Fatalf("submit:\n%s", body)
	}
	sid := h.submissionID(body)
	page := h.getPage(player, "/contests/C7040/standings")
	if !strings.Contains(page, "还没有判完的提交") || strings.Contains(page, "你现在第") {
		t.Errorf("a queued submission must not reach the board:\n%s", page)
	}
	// Once judged it appears, and the reader is told where they stand.
	h.scoreSubmission(sid, "AC", 1, 1)
	page = h.getPage(player, "/contests/C7040/standings")
	if !strings.Contains(page, "pending_one") || !strings.Contains(page, "你现在第 1 名") {
		t.Errorf("the judged row should be on the board:\n%s", page)
	}
}

func TestScoreColorScale(t *testing.T) {
	// 红→绿的挡位按「本题得分百分比」走，满分必须在最绿的一档，0 分在最红的一档。
	for in, want := range map[[2]int]string{
		{0, 10}: "sc0", {1, 10}: "sc0", {9, 10}: "sc8", {10, 10}: "sc9",
		{5, 10}: "sc4", {50, 100}: "sc4", {100, 0}: "sc0", {-1, 10}: "sc0",
		{3, 7}: "sc3", {7, 7}: "sc9",
	} {
		if got := scoreClass(in[0], in[1]); got != want {
			t.Errorf("scoreClass(%d,%d) = %s, want %s", in[0], in[1], got, want)
		}
	}
	// 一个格子只该有十档之一，别生成出 style.css 里没有的类名。
	for _, cls := range []string{"sc0", "sc9"} {
		if !strings.Contains(styleSheet(), "."+cls) {
			t.Errorf("style.css lost the %s colour step", cls)
		}
	}
}

func styleSheet() string {
	body, err := assetFS.ReadFile("static/style.css")
	if err != nil {
		return ""
	}
	return string(body)
}

// TestOIBoardWaitsForTheEnd is the closed-book half of real OI: while the clock runs
// nobody but the host sees a board at all, and the board that does come out has no
// clock and no attempt counts.
func TestOIBoardWaitsForTheEnd(t *testing.T) {
	h := newHarness(t)
	host := h.signIn("oi_host", permContestCreate)
	player := h.signIn("oi_player")
	rival := h.signIn("oi_rival")
	h.readyProblem("P7050")

	values := contestValues("C7050", "NOIP 模拟赛")
	values.Set("mode", store.ContestOI)
	values.Set("standing_open", "1")
	if _, body := h.createContest(host, values); !strings.Contains(body, "比赛已建好") {
		t.Fatalf("create:\n%s", body)
	}
	h.mountContestProblems(host, "C7050", "P7050")
	cid := h.contestID("C7050")
	h.joinAll("C7050", player, rival)

	// player solves it and then breaks it; OI scores the broken one.
	h.submitAndJudge(player, "P7050", cid, "AC", 1, 1)
	h.submitAndJudge(player, "P7050", cid, "WA", 0)
	h.submitAndJudge(rival, "P7050", cid, "AC", 1, 1)

	during := h.getPage(player, "/contests/C7050/standings")
	for _, want := range []string{"OI 赛制比赛中不实时出榜", "提交看不到结果"} {
		if !strings.Contains(during, want) {
			t.Errorf("a running OI board should explain itself, missing %q:\n%s", want, during)
		}
	}
	for _, leak := range []string{">oi_player</td>", ">oi_rival</td>", "你现在第"} {
		if strings.Contains(during, leak) {
			t.Errorf("the live OI board leaked %q:\n%s", leak, during)
		}
	}
	// The host can look, because someone has to be able to check the data.
	behind := h.getPage(host, "/contests/C7050/standings")
	if !strings.Contains(behind, ">oi_player</td>") || !strings.Contains(behind, "你是主办方") {
		t.Errorf("the host should still see the field:\n%s", behind)
	}
	// The contest page says the same thing, so the reader is not sent to a blank page.
	if home := h.getPage(player, "/contests/C7050"); !strings.Contains(home, "OI 比赛中不出榜") {
		t.Errorf("the contest page should mark the board as pending:\n%s", home)
	}

	// End the window: the board comes out, and it carries no clock.
	start, end := time.Now().Add(-3*time.Hour), time.Now().Add(-time.Hour)
	after := contestValues("C7050", "NOIP 模拟赛")
	after.Set("mode", store.ContestOI)
	after.Set("standing_open", "1")
	after.Set("start_at", start.Format(contestLayoutMini))
	after.Set("end_at", end.Format(contestLayoutMini))
	if _, body := h.postAs(host, "/contests/C7050", after); !strings.Contains(body, "已保存") {
		t.Fatalf("end the contest:\n%s", body)
	}
	page := h.getPage(player, "/contests/C7050/standings")
	for _, want := range []string{">oi_player</td>", ">oi_rival</td>", "同分同名次", "已结束", "本赛制不比罚时"} {
		if !strings.Contains(page, want) {
			t.Errorf("the published OI board is missing %q:\n%s", want, page)
		}
	}
	// 罚时是 ACM 的事：OI 榜上连这个数字都不该出现。
	for _, absent := range []string{"<th class=\"num\">用时</th>", "· 2 次", "你现在第", "每次未过尝试罚"} {
		if strings.Contains(page, absent) {
			t.Errorf("an OI board must not print %q:\n%s", absent, page)
		}
	}
	// 最后一次提交说了算：破掉的题算 0 分，所以 player 全场 0 分，rival 满分。
	if !strings.Contains(page, `class="score sc0" href="/problems/P7050"`) {
		t.Errorf("the broken last submission should be at the red end:\n%s", page)
	}
	// 满分那位在格子和总分两处都是绿的一端。
	if strings.Count(page, `class="score sc9"`) != 2 {
		t.Errorf("the clean 2/2 should be green in its cell and in the total column:\n%s", page)
	}
}
