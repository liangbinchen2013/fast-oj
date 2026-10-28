package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/protocol"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// claim 是测试用的判机心跳。它报的能力与现在这版判机一样（protocol.Capabilities）：
// 服务器照自报的能力派单，测试要是只报个名字就变成在测"老判机"，SPJ 与选过版本的工单
// 根本派不下来，那些测试会红得莫名其妙。
func (h *harness) claim(name string) (int, string) {
	return h.json(http.MethodPost, "/judge/claim", h.judge, claimBody(name, protocol.Capabilities))
}

func claimBody(name string, caps []string) []byte {
	raw, err := json.Marshal(protocol.Greeting{
		Name: name, Languages: []string{"cpp"}, Capabilities: caps, Note: "test box",
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// 隔离状态是判机每次心跳重报的"此刻"事实。没有一台机器有牢房时 /judges 必须当面警告：
// 那时提交是在判机自己的身份下、在宿主文件系统上跑的，而这件事过去只写在启动日志里。
func TestJudgesPageWarnsWhenNobodyHasAJail(t *testing.T) {
	h := newHarness(t)
	jailed := true

	if code, body := h.json(http.MethodPost, "/judge/claim", h.judge,
		claimBody("box-nojail", protocol.Capabilities)); code != http.StatusNotFound {
		t.Fatalf("空队列领单 = %d：\n%s", code, short(body))
	}
	_, page := h.get("/judges", h.adminToken())
	if !strings.Contains(page, "没有一台会把提交关进牢房") {
		t.Errorf("没有一台有牢房却没警告：\n%s", short(page))
	}
	if !strings.Contains(page, "未报（老判机）") {
		t.Errorf("没报隔离状态的机器应当写成未报：\n%s", short(page))
	}

	// 一台报了"有牢房"的机器进来，警告就该消失——红字留在页面上反而会让下次真出事时被当成噪音。
	raw, err := json.Marshal(protocol.Greeting{
		Name: "box-jailed", Languages: []string{"cpp"}, Capabilities: protocol.Capabilities,
		Jailed: &jailed, Note: "沙盒已启用",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := h.json(http.MethodPost, "/judge/claim", h.judge, raw); code != http.StatusNotFound {
		t.Fatalf("领单 = %d：\n%s", code, short(body))
	}
	_, page = h.get("/judges", h.adminToken())
	if strings.Contains(page, "没有一台会把提交关进牢房") {
		t.Errorf("有一台有牢房却还在警告：\n%s", short(page))
	}
	if !strings.Contains(page, "有牢房") {
		t.Errorf("那台有牢房的机器没标出来：\n%s", short(page))
	}
}

func (h *harness) claimTask(t *testing.T, name string) protocol.Task {
	t.Helper()
	code, body := h.claim(name)
	if code != http.StatusOK {
		t.Fatalf("claim %s = %d %q", name, code, body)
	}
	var task protocol.Task
	if err := json.Unmarshal([]byte(body), &task); err != nil {
		t.Fatalf("claim payload: %v (%s)", err, body)
	}
	return task
}

// 老判机（不报能力的那种）不能领到需要新能力的工单。让它领就是让它按逐字节比较去交一份
// 页面上写着「本题由 SPJ 判定」的结果，或按自己那套硬编码去编一份「C++14 · O2 关」。
func TestOldJudgeNotHandedWorkItCannotHonour(t *testing.T) {
	h := newHarness(t)
	keeper := h.author("keeper2100", permTestcaseManage, permJudgeProgram)
	student := h.signIn("stu2100")
	h.readyProblem("P2100")
	if code, body := h.postAs(keeper, "/problems/P2100/judge_program", url.Values{
		"kind": {"checker"}, "language": {"cpp"}, "source": {sampleChecker},
	}); code != http.StatusOK {
		t.Fatalf("存判题器 = %d：\n%s", code, short(body))
	}
	if code, _ := h.submitCode(student, "P2100", "cpp", "int main(){}"); code != http.StatusOK {
		t.Fatalf("提交 = %d", code)
	}

	// 报名字、报语言，但不报能力——正是功能上线之前那一版判机的样子。
	if code, body := h.json(http.MethodPost, "/judge/claim", h.judge,
		claimBody("box-old", nil)); code != http.StatusNotFound {
		t.Fatalf("老判机领到了 SPJ 工单 = %d：\n%s", code, short(body))
	}
	// 队列里那条还在等一台判得了的机器，而不是被将就判掉。
	task := h.claimTask(t, "box-new")
	if task.PID != "P2100" {
		t.Fatalf("会 SPJ 的判机应当拿到这条工单，得到 %s", task.PID)
	}
	if task.Checker == nil || !strings.Contains(task.Checker.Source, "argv") {
		t.Errorf("工单没把判题器带给会 SPJ 的判机：%+v", task.Checker)
	}

	_, page := h.get("/judges", h.adminToken())
	if !strings.Contains(page, "未自报") {
		t.Errorf("/judges 没把这台没自报能力的判机标出来：\n%s", short(page))
	}
}

func (h *harness) postJSON(path, token string, body string) (int, string) {
	return h.json(http.MethodPost, path, token, []byte(body))
}

func TestJudgeClaimWorkOrder(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	h.readyProblem("P3000")

	// An empty queue is a 404, not an error.
	if code, body := h.claim("box-1"); code != http.StatusNotFound || !strings.Contains(body, `"queue":"empty"`) {
		t.Fatalf("empty claim = %d %q", code, body)
	}
	if code, _ := h.postJSON("/judge/claim", "", `{"name":"box-1"}`); code != http.StatusUnauthorized {
		t.Fatalf("anonymous claim = %d", code)
	}
	if code, _ := h.postJSON("/judge/claim", "not-a-token", `{"name":"box-1"}`); code != http.StatusUnauthorized {
		t.Fatalf("unknown bearer = %d, want 401", code)
	}
	// A logged-in browser is not a judge box, cookie included.
	if code, _ := h.postAs(alice, "/judge/claim", url.Values{"name": {"box-1"}}); code != http.StatusForbidden {
		t.Fatalf("signed-in user claimed queue work = %d, want 403", code)
	}
	// The admin token may drive the queue by hand.
	if code, _ := h.json(http.MethodPost, "/judge/claim", h.adminToken(), []byte(`{"name":""}`)); code != http.StatusBadRequest {
		t.Fatalf("blank judge name = %d, want 400", code)
	}

	if code, body := h.submitCode(alice, "P3000", "cpp", "int main(){}"); code != http.StatusOK {
		t.Fatalf("submit = %d %q", code, body)
	}
	task := h.claimTask(t, "box-1")
	if task.SubmissionID == 0 || task.PID != "P3000" || task.Language != "cpp" || task.Source != "int main(){}" {
		t.Fatalf("task = %+v", task)
	}
	if task.TimeLimitMS != 1000 || task.MemLimitKB != 262144 {
		t.Fatalf("limits = %+v", task)
	}
	// sample1 (public) + 1,2 (judged), samples first.
	if len(task.Cases) != 3 {
		t.Fatalf("cases = %+v", task.Cases)
	}
	if !task.Cases[0].IsSample || task.Cases[0].Score != 0 {
		t.Fatalf("sample row = %+v", task.Cases[0])
	}
	if task.Cases[1].IsSample || task.Cases[1].Score != 1 {
		t.Fatalf("judged row = %+v", task.Cases[1])
	}

	// 判机能取到这份工单的每一条正文，包括判题用例。地址是「这条提交的第 N 条」，
	// 不是「用例 id 为 X」——后者让一个拿到令牌的人顺着 id 把全站答案抄走。
	for _, tc := range task.Cases {
		base := "/judge/submissions/" + itoa(int(task.SubmissionID)) + "/cases/" + itoa(tc.Ordinal)
		if code, body := h.get(base+"/in?judge=box-1", h.judge); code != http.StatusOK {
			t.Fatalf("case #%d in = %d %q", tc.Ordinal, code, body)
		}
		if code, body := h.get(base+"/out?judge=box-1", h.judge); code != http.StatusOK {
			t.Fatalf("case #%d out = %d %q", tc.Ordinal, code, body)
		}
	}
	if code, _ := h.get("/judge/submissions/1/cases/1/in", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous case fetch = %d", code)
	}
	// 没报机名的取件请求直接拒：判机身份是这一道门的另一半。
	if code, body := h.get("/judge/submissions/"+itoa(int(task.SubmissionID))+"/cases/1/in", h.judge); code != http.StatusBadRequest {
		t.Fatalf("case fetch without judge name = %d %q", code, body)
	}
	if code, _ := h.get("/judge/submissions/"+itoa(int(task.SubmissionID))+"/cases/999/in?judge=box-1", h.judge); code != http.StatusNotFound {
		t.Fatalf("unknown ordinal = %d", code)
	}
	if code, body := h.get("/judge/submissions/"+itoa(int(task.SubmissionID))+"/cases/1/other?judge=box-1", h.judge); code != http.StatusBadRequest {
		t.Fatalf("bad which = %d %q", code, body)
	}
	// 别的判机想读同一条提交的用例：它没派到这份工单，就读不到。
	if code, body := h.get("/judge/submissions/"+itoa(int(task.SubmissionID))+"/cases/1/in?judge=box-9", h.judge); code != http.StatusForbidden {
		t.Fatalf("a box that never claimed it = %d %q，要 403", code, body)
	}

	// The box shows up in the registry.
	code, listed := h.get("/judges", h.adminToken())
	if code != http.StatusOK || !strings.Contains(listed, "box-1") || !strings.Contains(listed, "cpp") {
		t.Fatalf("judges = %d %q", code, listed)
	}
	if code, _ := h.getAs(alice, "/judges"); code != http.StatusForbidden {
		t.Fatalf("user may not list judges = %d", code)
	}
	// Nothing else is queued.
	if code, _ := h.claim("box-1"); code != http.StatusNotFound {
		t.Fatalf("second claim = %d", code)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestJudgeStatusTransitions(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	h.readyProblem("P3001")
	h.submitCode(alice, "P3001", "cpp", "x")
	task := h.claimTask(t, "box-1")
	path := "/judge/submissions/" + itoa(int(task.SubmissionID)) + "/status"

	if code, body := h.postJSON(path, h.judge, `{"name":"box-9","state":"running"}`); code != http.StatusForbidden {
		t.Fatalf("unknown box = %d %q", code, body)
	}
	if code, body := h.postJSON(path, h.judge, `{"name":"box-1","state":"done"}`); code != http.StatusBadRequest {
		t.Fatalf("terminal state via status = %d %q", code, body)
	}
	// claim stamped compiling, so compiling -> running is the legal next step.
	if code, body := h.postJSON(path, h.judge, `{"name":"box-1","state":"running","detail":"on case 2"}`); code != http.StatusOK {
		t.Fatalf("progress = %d %q", code, body)
	}
	// Repeating it is now an illegal transition.
	if code, body := h.postJSON(path, h.judge, `{"name":"box-1","state":"compiling"}`); code != http.StatusConflict {
		t.Fatalf("running -> compiling = %d %q", code, body)
	}
	_, view := h.getAs(alice, "/submissions/"+itoa(int(task.SubmissionID)))
	if !strings.Contains(view, "判题中") {
		t.Fatalf("progress not visible to the owner: %q", view)
	}
	if code, _ := h.postAs(alice, path, url.Values{"name": {"box-1"}, "state": {"running"}}); code != http.StatusForbidden {
		t.Fatalf("a signed-in browser may not report judgements = %d", code)
	}
}

func TestJudgeResultAndScoring(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	h.readyProblem("P3002")
	h.submitCode(alice, "P3002", "cpp", "int main(){}")
	task := h.claimTask(t, "box-1")
	sid := itoa(int(task.SubmissionID))
	result := "/judge/submissions/" + sid + "/result"

	judged := task.Cases[1].Name
	other := task.Cases[2].Name
	sample := task.Cases[0].Name

	body := `{"name":"box-1","verdict":"AC","time_ms":12,"mem_kb":3072,"runs":[` +
		`{"ordinal":1,"case_name":"` + sample + `","verdict":"AC","time_ms":1,"mem_kb":1024},` +
		`{"ordinal":2,"case_name":"` + judged + `","verdict":"AC","time_ms":12,"mem_kb":3072},` +
		`{"ordinal":3,"case_name":"` + other + `","verdict":"AC","time_ms":9,"mem_kb":3000}]}`
	code, out := h.postJSON(result, h.judge, body)
	if code != http.StatusOK || !strings.Contains(out, `"verdict":"AC"`) || !strings.Contains(out, `"score":2`) {
		t.Fatalf("AC result = %d %q", code, out)
	}
	_, view := h.getAs(alice, "/submissions/"+sid)
	for _, want := range []string{"AC", "2/2", "12ms", "3.00M"} {
		if !strings.Contains(view, want) {
			t.Fatalf("owner view missing %q:\n%s", want, view)
		}
	}

	// A judge cannot report twice.
	if code, out := h.postJSON(result, h.judge, body); code != http.StatusConflict {
		t.Fatalf("repeat result = %d %q", code, out)
	}

	// An AC claim with a failing judged row is downgraded by the server.
	h.submitCode(alice, "P3002", "cpp", "int main(){return 0;}")
	task2 := h.claimTask(t, "box-1")
	sid2 := itoa(int(task2.SubmissionID))
	bad := `{"name":"box-1","verdict":"AC","time_ms":30,"mem_kb":2048,"runs":[` +
		`{"case_name":"` + sample + `","verdict":"AC"},` +
		`{"case_name":"` + judged + `","verdict":"WA"},` +
		`{"case_name":"` + other + `","verdict":"AC"}]}`
	code, out = h.postJSON("/judge/submissions/"+sid2+"/result", h.judge, bad)
	if code != http.StatusOK || !strings.Contains(out, `"verdict":"WA"`) || !strings.Contains(out, `"score":1`) ||
		!strings.Contains(out, `"failed_case":"`+judged+`"`) {
		t.Fatalf("downgrade = %d %q", code, out)
	}

	// Unknown verdict strings become IE.
	h.submitCode(alice, "P3002", "cpp", "x")
	task3 := h.claimTask(t, "box-2")
	sid3 := itoa(int(task3.SubmissionID))
	code, out = h.postJSON("/judge/submissions/"+sid3+"/result", h.judge,
		`{"name":"box-2","verdict":"MEMORY_EXPLODED","time_ms":-1,"mem_kb":-1}`)
	if code != http.StatusOK || !strings.Contains(out, `"verdict":"IE"`) {
		t.Fatalf("unknown verdict = %d %q", code, out)
	}
	// box-2 owns that row, so box-1 must not be able to report on it.
	h.submitCode(alice, "P3002", "cpp", "x")
	task4 := h.claimTask(t, "box-3")
	if code, out := h.postJSON("/judge/submissions/"+itoa(int(task4.SubmissionID))+"/result", h.judge,
		`{"name":"box-1","verdict":"AC"}`); code != http.StatusForbidden {
		t.Fatalf("wrong owner result = %d %q", code, out)
	}
	if code, _ := h.postJSON("/judge/submissions/99999/result", h.judge, `{"name":"box-1","verdict":"AC"}`); code != http.StatusNotFound {
		t.Fatalf("unknown submission result = %d", code)
	}
	if code, body := h.postJSON("/judge/submissions/abc/result", h.judge, `{"name":"box-1"}`); code != http.StatusBadRequest {
		t.Fatalf("bad id = %d %q", code, body)
	}
}

func TestTallyResultMath(t *testing.T) {
	cases := []store.Testcase{
		{Name: "sample1", IsSample: true, Score: 0},
		{Name: "1", Score: 40},
		{Name: "2", Score: 60},
	}
	ac := protocol.RunRow{CaseName: "1", Verdict: "AC"}
	wa := protocol.RunRow{CaseName: "2", Verdict: "WA"}

	// Honest AC.
	v, runs, score, total, failed := tallyResult("AC", []protocol.RunRow{
		{CaseName: "sample1", Verdict: "AC"}, ac, {CaseName: "2", Verdict: "AC"},
	}, cases)
	if v != "AC" || score != 100 || total != 100 || failed != "" || len(runs) != 3 {
		t.Fatalf("honest ac: %v %d %d %q", v, score, total, failed)
	}

	// Lying AC.
	v, _, score, total, failed = tallyResult("AC", []protocol.RunRow{ac, wa}, cases)
	if v != "WA" || score != 40 || total != 100 || failed != "2" {
		t.Fatalf("lying ac: %v %d %d %q", v, score, total, failed)
	}

	// Compile error: no rows, no score, nothing failed by case.
	v, _, score, _, failed = tallyResult("CE", nil, cases)
	if v != "CE" || score != 0 || failed != "" {
		t.Fatalf("ce: %v %d %q", v, score, failed)
	}

	// Garbage verdict string collapses to IE.
	v, _, _, _, _ = tallyResult("AC", []protocol.RunRow{{CaseName: "1", Verdict: "boom"}}, cases)
	if v != "IE" {
		t.Fatalf("garbage verdict = %v", v)
	}

	// A row for a deleted case cannot certify AC.
	v, _, _, _, failed = tallyResult("AC", []protocol.RunRow{
		{CaseName: "1", Verdict: "AC"}, {CaseName: "ghost", Verdict: "RE"},
	}, cases)
	if v != "RE" || failed != "ghost" {
		t.Fatalf("ghost row: %v %q", v, failed)
	}

	// Sample rows never add points, and reporting only them has not judged the
	// scored cases: that cannot be certified AC.
	v, _, score, total, failed = tallyResult("AC", []protocol.RunRow{{CaseName: "sample1", Verdict: "AC"}}, cases)
	if v != "WA" || score != 0 || total != 100 || failed != "1" {
		t.Fatalf("skipped judged cases: %v %d %d %q", v, score, total, failed)
	}

	// A bundle pays only when every case in it passes, so the AC row of a broken
	// group earns nothing even though the row itself passed.
	grouped := []store.Testcase{
		{Name: "sub1", Score: 30, GroupName: "sub"},
		{Name: "sub2", Score: 70, GroupName: "sub"},
		{Name: "solo", Score: 5},
	}
	v, runs, score, total, failed = tallyResult("AC", []protocol.RunRow{
		{CaseName: "sub1", Verdict: "AC"}, {CaseName: "sub2", Verdict: "TLE"}, {CaseName: "solo", Verdict: "AC"},
	}, grouped)
	if v != "TLE" || score != 5 || total != 105 || failed != "sub2" {
		t.Fatalf("broken bundle: %v %d %d %q", v, score, total, failed)
	}
	if len(runs) != 3 || runs[0].Score != 0 || runs[2].Score != 5 {
		t.Fatalf("per-row points = %+v", runs)
	}

	// The same group, fully passed, pays the sum of its cases.
	v, _, score, _, failed = tallyResult("AC", []protocol.RunRow{
		{CaseName: "sub1", Verdict: "AC"}, {CaseName: "sub2", Verdict: "AC"}, {CaseName: "solo", Verdict: "AC"},
	}, grouped)
	if v != "AC" || score != 105 || failed != "" {
		t.Fatalf("intact bundle: %v %d %q", v, score, failed)
	}

	// A group whose hard case was never reported is not passed either.
	v, _, score, _, failed = tallyResult("AC", []protocol.RunRow{
		{CaseName: "sub1", Verdict: "AC"}, {CaseName: "solo", Verdict: "AC"},
	}, grouped)
	if v != "WA" || score != 5 || failed != "sub2" {
		t.Fatalf("unreported bundle case: %v %d %q", v, score, failed)
	}
}
