package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// submitQueued 交一份代码上去，停在 queued，返回它的 id（Location 上就是它）。
func submitQueued(t *testing.T, h *harness, who session, pid string) int64 {
	t.Helper()
	code, _, hdr := h.submitRaw(pid, url.Values{"language": {"cpp"}, "source": {"int main(){return 0;}"}}, who)
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/submissions/") {
		t.Fatalf("提交 = %d %q，想要跳到那条提交", code, hdr.Get("Location"))
	}
	id, err := strconv.ParseInt(strings.SplitN(strings.TrimPrefix(hdr.Get("Location"), "/submissions/"), "?", 2)[0], 10, 64)
	if err != nil {
		t.Fatalf("提交跳转地址读不出 id：%q", hdr.Get("Location"))
	}
	return id
}

// judgedSubmission 走一遍真链路：交上去、判机领走、回报 AC 与三个用例。
//
// 重判这件事只有"真的判完过"的提交才测得出来——排队中的那条没有上一次可清，按下去应当
// 只是空操作，那是一条独立的用例。
func judgedSubmission(t *testing.T, h *harness, who session, pid string, box string) int64 {
	t.Helper()
	id := submitQueued(t, h, who, pid)
	task := h.claimTask(t, box)
	if task.SubmissionID != id {
		t.Fatalf("领到的是 #%d，交上去的是 #%d", task.SubmissionID, id)
	}
	var runs []string
	for _, c := range task.Cases {
		runs = append(runs, `{"ordinal":`+strconv.Itoa(c.Ordinal)+`,"case_name":"`+c.Name+
			`","verdict":"AC","time_ms":12,"mem_kb":3072}`)
	}
	sid := strconv.FormatInt(id, 10)
	body := `{"name":"` + box + `","verdict":"AC","time_ms":12,"mem_kb":3072,"runs":[` + strings.Join(runs, ",") + `]}`
	if code, out := h.postJSON("/judge/submissions/"+sid+"/result", h.judge, body); code != http.StatusOK {
		t.Fatalf("回报 = %d %q", code, out)
	}
	return id
}

// 判机口径变了，旧提交不会自己改判。管理员那一下补救要真的把上一次清干净：页面上
// 不能再留着旧判定、旧得分，也不能再留着上一台判机说过的那两句（口径、判于何时）。
func TestRejudgeClearsThePreviousVerdict(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	boss := h.signIn("boss", permRejudge)
	h.readyProblem("P5001")
	id := judgedSubmission(t, h, alice, "P5001", "box-1")
	sid := strconv.FormatInt(id, 10)

	if _, before := h.getAs(alice, "/submissions/"+sid); !strings.Contains(before, "AC") {
		t.Fatal("重判之前这条本该是 AC")
	}
	code, after := h.postAs(boss, "/submissions/"+sid+"/rejudge", url.Values{})
	if code != http.StatusOK {
		t.Fatalf("重判 = %d:\n%s", code, after)
	}
	for _, want := range []string{"排队中", "已退回队列"} {
		if !strings.Contains(after, want) {
			t.Errorf("重判后这一页没写到 %q：\n%s", want, after)
		}
	}
	for _, not := range []string{"12ms", "3.00M"} {
		if strings.Contains(after, not) {
			t.Errorf("旧那次的数字还留在页面上（%q）：\n%s", not, after)
		}
	}

	row, err := h.db.GetSubmission(ctx(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "queued" || row.Verdict != "" || row.Score != 0 || row.TimeMS != -1 || row.MemKB != -1 {
		t.Errorf("库里还留着旧判定：%+v", row)
	}
	if row.JudgeID != 0 || row.JudgedAt != 0 || row.Toolchain != "" {
		t.Errorf("还在替上一台判机报话：%+v", row)
	}
	if runs, err := h.db.ListRuns(ctx(), id); err != nil || len(runs) != 0 {
		t.Errorf("逐用例行没清掉：%d 行 %v", len(runs), err)
	}

	// 这一条要留痕：事后得答得出这条成绩被谁重判过、重判之前是什么。
	rows := h.auditRows("submission.rejudge")
	if len(rows) != 1 {
		t.Fatalf("审计里重判的条数 = %d，想要 1", len(rows))
	}
	if rows[0].Target != "submission:"+sid || !strings.Contains(rows[0].Detail, "AC") ||
		!strings.Contains(rows[0].Detail, "退回队列") {
		t.Errorf("重判那条流水没写清楚：%+v", rows[0])
	}
	if rows[0].Actor != "boss" {
		t.Errorf("流水里没记下是谁按的：%+v", rows[0])
	}

	// 退回队列的这条重新可领：判机再领走它，判完就是今天的答案。
	if task := h.claimTask(t, "box-2"); task.SubmissionID != id {
		t.Fatalf("重判后领不到这一条：%+v", task)
	}
}

// 重判不是"再来一次"：排队中的那条没有上一次可清，按下去只会原地踏步，所以页面上
// 不该摆那条按钮，硬按也不许改动任何状态（比如把已判完的写成 done 的伪装）。
func TestRejudgeIsANoOpForARowStillQueued(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	boss := h.signIn("boss", permRejudge)
	h.readyProblem("P5004")
	id := submitQueued(t, h, alice, "P5004")
	sid := strconv.FormatInt(id, 10)

	if _, page := h.getAs(boss, "/submissions/"+sid); strings.Contains(page, "重新判题") {
		t.Error("排队中的那条不该给重判按钮")
	}
	if code, body := h.postAs(boss, "/submissions/"+sid+"/rejudge", url.Values{}); code != http.StatusOK ||
		!strings.Contains(body, "已经在队列里") {
		t.Fatalf("重判一条排队中的 = %d，想要一句实话：\n%s", code, body)
	}
	// 空操作不许改动任何东西：它本来就什么都没清。
	row, err := h.db.GetSubmission(ctx(), id)
	if err != nil || row.Status != "queued" {
		t.Fatalf("这一行被改动了：%+v %v", row, err)
	}
}

// 列表页那条按钮：只给持键的人，且不给还排在队列里的那条。
func TestRejudgeButtonOnlyAppearsForHolders(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	boss := h.signIn("boss", permRejudge)
	h.readyProblem("P5005")
	judged := judgedSubmission(t, h, alice, "P5005", "box-1")
	waiting := submitQueued(t, h, alice, "P5005")

	_, asBoss := h.getAs(boss, "/submissions")
	if !strings.Contains(asBoss, ">重判</button>") {
		t.Error("持键的人在列表上看不到那条按钮")
	}
	if strings.Contains(asBoss, "/submissions/"+strconv.FormatInt(waiting, 10)+"/rejudge") {
		t.Error("排队中的那条也给了按钮，按下去只会原地踏步")
	}
	_, asAlice := h.getAs(alice, "/submissions")
	if strings.Contains(asAlice, "重判") {
		t.Errorf("无键的人在列表上看见了重判按钮：\n%s", asAlice)
	}
	_, anon := h.htmlGet("/submissions", "")
	if strings.Contains(anon, "重判") {
		t.Error("没登录的人看见了重判按钮")
	}
	if strings.Contains(asAlice, "/submissions/"+strconv.FormatInt(judged, 10)+"/rejudge") {
		t.Error("表单发给了没有权限的人")
	}
}

// 这一条是"重判一条正在被判的提交"最危险的一下的证据：那台判机还在跑，它跑完来报的
// 那一句必须回不来，否则旧判定会盖在管理员刚退回的这条上。
func TestRejudgedSubmissionIgnoresTheOldJudge(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	boss := h.signIn("boss", permRejudge)
	h.readyProblem("P5003")
	id := submitQueued(t, h, alice, "P5003")
	sid := strconv.FormatInt(id, 10)

	if task := h.claimTask(t, "box-old"); task.SubmissionID != id {
		t.Fatalf("领到的不是这一条：%+v", task)
	}
	if code, body := h.postAs(boss, "/submissions/"+sid+"/rejudge", url.Values{}); code != http.StatusOK {
		t.Fatalf("重判一条在飞的 = %d:\n%s", code, body)
	}

	// 那台判机不知道已经被退回了，跑完照样来报。
	if code, out := h.postJSON("/judge/submissions/"+sid+"/result", h.judge,
		`{"name":"box-old","verdict":"WA","time_ms":9,"mem_kb":1024}`); code != http.StatusForbidden {
		t.Fatalf("旧判机的回报被收下了 = %d %q，想要 403", code, out)
	}
	row, err := h.db.GetSubmission(ctx(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "queued" || row.Verdict != "" {
		t.Fatalf("旧判机改写了这一行：%+v", row)
	}
	// 新的那一次照旧领得到。
	if task := h.claimTask(t, "box-new"); task.SubmissionID != id {
		t.Fatalf("重判后新的判机领不到：%+v", task)
	}
}

// 重判这把钥匙单独设：能看别人源码与逐用例的人（submission.view.any）不该顺手改掉别人的成绩。
func TestRejudgeNeedsItsOwnKey(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	watcher := h.signIn("watcher", permSubmissionViewAny)
	h.readyProblem("P5002")
	id := judgedSubmission(t, h, alice, "P5002", "box-1")
	sid := strconv.FormatInt(id, 10)

	// 本人也不行：重判改的是"结果是什么"，不是"这是谁的提交"。
	if code, body := h.postAs(alice, "/submissions/"+sid+"/rejudge", url.Values{}); code != http.StatusForbidden ||
		!strings.Contains(body, "缺少权限："+permRejudge) {
		t.Fatalf("作者重判 = %d，想要 403 并点出键名：\n%s", code, body)
	}
	if code, _ := h.postAs(watcher, "/submissions/"+sid+"/rejudge", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("只有 view.any 的人重判 = %d，想要 403", code)
	}
	// 没登录就是 401：这条按钮只出现在持键者的页面上，它不是一条"路人也会撞上来"的路由，
	// 所以按站上其他管理侧 POST 的口径给一句话，而不是替访客编一个登录跳转。
	if code, body, _ := h.postForm("/submissions/"+sid+"/rejudge", url.Values{}, "", ""); code != http.StatusUnauthorized ||
		!strings.Contains(body, "请先登录") {
		t.Fatalf("匿名重判 = %d，想要 401 并说清要先登录：\n%s", code, body)
	}
	// 带着会话但不带 CSRF 的 POST 也过不去：那条按钮不等于"任何 POST 都算重判"。
	if code, _, _ := h.postNoRedirect("/submissions/"+sid+"/rejudge", url.Values{}, alice.cookie); code != http.StatusForbidden {
		t.Errorf("没有 CSRF 的重判 = %d，想要 403", code)
	}
	row, err := h.db.GetSubmission(ctx(), id)
	if err != nil || row.Status != "done" || row.Verdict != "AC" {
		t.Fatalf("这些被拒的请求改动了这一行：%+v %v", row, err)
	}
}

// 状态筛选项是管理员找"卡住的提交"的入口：停在队列里的、判机超时的，混在一堆已完成的
// 记录里翻不出来。填错了要给一句话，而不是让他对着一张空表猜。
func TestSubmissionListFiltersByStatus(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	h.readyProblem("P5006")
	judged := judgedSubmission(t, h, alice, "P5006", "box-1")
	waiting := submitQueued(t, h, alice, "P5006")
	judgedLink, waitingLink := "/submissions/"+strconv.FormatInt(judged, 10), "/submissions/"+strconv.FormatInt(waiting, 10)

	_, all := h.getAs(alice, "/submissions")
	if !strings.Contains(all, judgedLink) || !strings.Contains(all, waitingLink) {
		t.Fatalf("不筛时两条都该在：\n%s", all)
	}
	_, done := h.getAs(alice, "/submissions?status=done")
	if !strings.Contains(done, judgedLink) || strings.Contains(done, waitingLink) {
		t.Fatalf("筛 done 的两条不对：\n%s", done)
	}
	_, queued := h.getAs(alice, "/submissions?status=queued")
	if !strings.Contains(queued, waitingLink) || strings.Contains(queued, judgedLink) {
		t.Fatalf("筛 queued 的两条不对：\n%s", queued)
	}
	// 选中的那一项要留在表单上：筛完之后看不见自己筛了什么，比没有这个筛选项更误事。
	if !strings.Contains(done, `value="done" selected`) {
		t.Error("筛选项没留在表单上")
	}
	if code, body := h.getAs(alice, "/submissions?status=nope"); code != http.StatusBadRequest ||
		!strings.Contains(body, "queued, compiling, running, done, abandoned") {
		t.Fatalf("乱填的状态 = %d，想要一句能读懂的拒绝：\n%s", code, body)
	}
	// 翻页的"更早"链接要带上筛选项，不然第二页就悄悄变成全部了。
	if _, only := h.getAs(alice, "/submissions?limit=1&status=done"); !strings.Contains(only, "status=done") {
		t.Error("翻页链接丢了状态筛选项")
	}
}
