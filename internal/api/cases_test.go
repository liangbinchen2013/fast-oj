package api

import (
	"bytes"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// caseProblem authors an empty problem and returns it, so each test starts with a
// problem whose case set it can grow through the web editor.
func (h *harness) caseProblem(handle, pid string) session {
	h.t.Helper()
	setter := h.signIn(handle, permProblemCreate)
	if code, body := h.postAs(setter, "/problems", url.Values{"pid": {pid}, "title": {"用例编辑器实测"}, "time_ms": {"1000"}, "mem_mb": {"256"}, "visible": {"1"}}); code != http.StatusOK {
		h.t.Fatalf("create = %d %s", code, body)
	}
	return setter
}

func (h *harness) caseID(pid, name string) int64 {
	h.t.Helper()
	p, err := h.db.FindProblem(ctx(), pid)
	if err != nil {
		h.t.Fatal(err)
	}
	tc, err := h.db.GetTestcase(ctx(), p.ID, name)
	if err != nil {
		h.t.Fatalf("case %s: %v", name, err)
	}
	return tc.ID
}

func (h *harness) problem(pid string) store.Problem {
	h.t.Helper()
	p, err := h.db.FindProblem(ctx(), pid)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// idText and short keep the assertions readable: a failed page check should show
// a slice of the page, not 40 KB of template.
func idText(id int64) string { return strconv.FormatInt(id, 10) }

func short(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

func TestCaseEditorRoundTrip(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("caseditor", "P700")
	listPage := func() (int, string) {
		code, body := h.getAs(setter, "/problems/P700/cases")
		if code != http.StatusOK {
			t.Fatalf("case page = %d", code)
		}
		return code, body
	}

	// An empty problem says so instead of showing a blank table.
	_, page := listPage()
	if !strings.Contains(page, "还没有用例") {
		t.Fatalf("empty case page:\n%s", page)
	}

	// A sample carries no points even when the score box was left at 1, because a
	// sample that scores would silently change what "满分" means.
	if code, body := h.postAs(setter, "/problems/P700/cases", url.Values{
		"name": {"sample1"}, "sample": {"1"}, "score": {"1"}, "input": {"1 2\n"}, "output": {"3\n"},
	}); code != http.StatusOK || !strings.Contains(body, "sample1") {
		t.Fatalf("add sample = %d %s", code, body)
	}
	if tc, err := h.db.GetTestcase(ctx(), h.problem("P700").ID, "sample1"); err != nil || tc.Score != 0 || !tc.IsSample {
		t.Fatalf("stored sample = %+v %v", tc, err)
	}
	// The public problem page shows the sample body, and the case page labels it.
	_, page = listPage()
	// The sample input is four bytes, and the list shows the real size rather than
	// a count.
	for _, want := range []string{"样例", "4 B"} {
		if !strings.Contains(page, want) {
			t.Fatalf("case page missing %q:\n%s", want, page)
		}
	}
	_, detail := h.htmlGet("/problems/P700", "")
	if !strings.Contains(detail, "1 2") {
		t.Fatalf("sample did not reach the problem page:\n%s", detail)
	}

	// A judged case is worth points and stays off the problem page.
	if code, body := h.postAs(setter, "/problems/P700/cases", url.Values{
		"name": {"hidden1"}, "score": {"3"}, "input": {"4 5\n"}, "output": {"9\n"},
	}); code != http.StatusOK {
		t.Fatalf("add judged = %d %s", code, body)
	}
	if total, err := h.db.ProblemScoreTotal(ctx(), h.problem("P700").ID); err != nil || total != 3 {
		t.Fatalf("judged points = %d %v, want 3", total, err)
	}
	if _, detail := h.htmlGet("/problems/P700", ""); strings.Contains(detail, "hidden1") || strings.Contains(detail, "4 5") {
		t.Fatalf("judged case leaked onto the public page:\n%s", detail)
	}

	// The next ordinal follows the cases already there.
	if tc, err := h.db.GetTestcase(ctx(), h.problem("P700").ID, "hidden1"); err != nil || tc.Ordinal != 2 {
		t.Fatalf("ordinal = %+v %v", tc, err)
	}

	// A duplicate name is refused with the name in the message, not overwritten.
	if _, body := h.postAs(setter, "/problems/P700/cases", url.Values{
		"name": {"hidden1"}, "score": {"1"}, "input": {"x\n"}, "output": {"y\n"},
	}); !strings.Contains(body, "已经有同名用例") {
		t.Fatalf("duplicate name accepted:\n%s", body)
	}
	if n, _ := h.db.CountTestcases(ctx(), h.problem("P700").ID, false); n != 2 {
		t.Fatalf("duplicate added a row: %d cases", n)
	}

	// Editing rewrites the bytes under the same ordinal and can rename the case.
	id := h.caseID("P700", "hidden1")
	code, form := h.getAs(setter, "/problems/P700/cases/"+url.QueryEscape(idText(id))+"/edit")
	if code != http.StatusOK || !strings.Contains(form, "4 5") {
		t.Fatalf("edit form = %d %s", code, short(form))
	}
	if _, body := h.postAs(setter, "/problems/P700/cases/"+idText(id)+"/edit", url.Values{
		"name": {"renamed2"}, "score": {"5"}, "input": {"10 20\n"}, "output": {"30\n"},
	}); !strings.Contains(body, "renamed2") {
		t.Fatalf("save did not land:\n%s", short(body))
	}
	tc, err := h.db.GetTestcase(ctx(), h.problem("P700").ID, "renamed2")
	if err != nil || tc.Ordinal != 2 || tc.Score != 5 {
		t.Fatalf("after rename = %+v %v", tc, err)
	}
	if code, body := h.get("/problems/P700/cases/renamed2/in", h.adminToken()); code != http.StatusOK || body != "10 20\n" {
		t.Fatalf("edited input = %d %q", code, body)
	}

	// Turning a judged case into a sample takes its points out of the total.
	if _, body := h.postAs(setter, "/problems/P700/cases/"+idText(id)+"/edit", url.Values{
		"name": {"renamed2"}, "sample": {"1"}, "score": {"5"}, "input": {"10 20\n"}, "output": {"30\n"},
	}); !strings.Contains(body, "样例") {
		t.Fatalf("sample toggle:\n%s", short(body))
	}
	if total, _ := h.db.ProblemScoreTotal(ctx(), h.problem("P700").ID); total != 0 {
		t.Fatalf("total after toggling to sample = %d, want 0", total)
	}

	// Deleting removes the row and its files. The name still appears once, in the
	// flash that says what happened, so the table is what the assertion checks.
	code, body := h.postAs(setter, "/problems/P700/cases/"+idText(id)+"/delete", url.Values{})
	if code != http.StatusOK || !strings.Contains(body, "已删除用例") {
		t.Fatalf("delete = %d %s", code, short(body))
	}
	if _, page := h.getAs(setter, "/problems/P700/cases"); strings.Contains(page, "renamed2") {
		t.Fatalf("deleted case still in the table:\n%s", short(page))
	}
	if n, _ := h.db.CountTestcases(ctx(), h.problem("P700").ID, false); n != 1 {
		t.Fatalf("cases after delete = %d, want 1", n)
	}
	if code, _ := h.get("/problems/P700/cases/renamed2/in", h.adminToken()); code == http.StatusOK {
		t.Fatal("the deleted case file is still readable")
	}
}

func TestCaseEditorRefusesHugeFiles(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("bigcases", "P701")
	big := strings.Repeat("1234567 ", 40_000) // 320 KB, past the inline editor's ceiling
	if code, _ := h.uploadZipAs(setter, "/problems/P701/cases/upload", zipOf(map[string]string{
		"1.in": big, "1.out": "9\n",
	})); code != http.StatusOK {
		t.Fatalf("import = %d", code)
	}

	id := h.caseID("P701", "1")
	code, page := h.getAs(setter, "/problems/P701/cases/"+idText(id)+"/edit")
	if code != http.StatusOK {
		t.Fatalf("edit page = %d", code)
	}
	if strings.Contains(page, `name="input"`) {
		t.Fatal("the editor loaded a 320 KB file into a textarea")
	}
	for _, want := range []string{"太大不适合", "KB"} {
		if !strings.Contains(page, want) {
			t.Fatalf("edit page missing %q:\n%s", want, short(page))
		}
	}

	// Renaming and rescoring still work; only the bytes are out of reach here.
	if _, body := h.postAs(setter, "/problems/P701/cases/"+idText(id)+"/edit", url.Values{
		"name": {"heavy"}, "score": {"2"},
	}); !strings.Contains(body, "heavy") {
		t.Fatalf("metadata-only save:\n%s", short(body))
	}
	if code, fetched := h.get("/problems/P701/cases/heavy/in", h.adminToken()); code != http.StatusOK || fetched != big {
		t.Fatalf("case bytes changed on a metadata save: %d %d bytes", code, len(fetched))
	}
}

// 很多题包用 .ans 当答案文件；它应当和 .out 一样参与配对，且不影响样例识别。
func TestCaseUploadAcceptsAnsAnswerFiles(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("anscase", "P720")
	code, body := h.uploadZipAs(setter, "/problems/P720/cases/upload", zipOf(map[string]string{
		"sample1.in": "1 2\n", "sample1.ans": "3\n",
		"1.in": "4 5\n", "1.ans": "9\n",
	}))
	if code != http.StatusOK || !strings.Contains(body, "新增 2 条") {
		t.Fatalf("ans import = %d %s", code, short(body))
	}
	if code, fetched := h.get("/problems/P720/cases/1/out", h.adminToken()); code != http.StatusOK || fetched != "9\n" {
		t.Fatalf("ans answer = %d %q, want 9", code, fetched)
	}
}

// 同一道用例同时出现 .out 与 .ans 时不能悄悄选一个：两份答案可能不同，必须让人改。
func TestCaseUploadRefusesBothOutAndAns(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("ansconflict", "P721")
	code, body := h.uploadZipAs(setter, "/problems/P721/cases/upload", zipOf(map[string]string{
		"1.in": "1\n", "1.out": "1\n", "1.ans": "2\n",
	}))
	if code != http.StatusOK || !strings.Contains(body, "both .out and .ans") {
		t.Fatalf("conflicting answer files = %d %s", code, short(body))
	}
	if n, _ := h.db.CountTestcases(ctx(), h.problem("P721").ID, false); n != 0 {
		t.Fatalf("conflicting archive imported %d cases", n)
	}
}

func TestCaseGroupsAreEditableFromTheWeb(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("grouper", "P703")
	for _, c := range []struct {
		name, group, score string
	}{
		{"sub1", "sub", "30"}, {"sub2", "sub", "70"}, {"solo", "", "5"},
	} {
		if code, body := h.postAs(setter, "/problems/P703/cases", url.Values{
			"name": {c.name}, "group": {c.group}, "score": {c.score},
			"input": {"1\n"}, "output": {"1\n"},
		}); code != http.StatusOK {
			t.Fatalf("add %s = %d %s", c.name, code, short(body))
		}
	}
	_, page := h.getAs(setter, "/problems/P703/cases")
	for _, want := range []string{"捆绑", "sub", "100"} {
		if !strings.Contains(page, want) {
			t.Errorf("case page missing %q:\n%s", want, short(page))
		}
	}
	// The summary names the bundle once and totals its points, so an author can
	// see what a group is worth before a submission finds out the hard way.
	if !strings.Contains(page, "<td class=\"num\">2</td>") || !strings.Contains(page, "<td class=\"num\">100</td>") {
		t.Errorf("bundle summary wrong:\n%s", short(page))
	}
	p := h.problem("P703")
	if tc, err := h.db.GetTestcase(ctx(), p.ID, "sub2"); err != nil || tc.GroupName != "sub" || tc.Score != 70 {
		t.Fatalf("stored group case = %+v %v", tc, err)
	}

	// A group name with a slash would be a path, not a label.
	if _, body := h.postAs(setter, "/problems/P703/cases", url.Values{
		"name": {"bad"}, "group": {"a/b"}, "score": {"1"}, "input": {"1\n"}, "output": {"1\n"},
	}); !strings.Contains(body, "组名") {
		t.Errorf("a path-shaped group was accepted:\n%s", short(body))
	}

	// And the detail level travels with the problem form.
	if code, body := h.postAs(setter, "/problems/"+p.PID, url.Values{
		"pid": {"P703"}, "title": {"用例分组"}, "time_ms": {"1000"}, "mem_mb": {"256"},
		"detail": {"none"}, "visible": {"1"},
	}); code != http.StatusOK {
		t.Fatalf("save detail level = %d %s", code, short(body))
	}
	if after := h.problem("P703"); after.DetailLevel != "none" {
		t.Fatalf("detail level = %q, want none", after.DetailLevel)
	}
}

func TestCasePageNeedsCaseRights(t *testing.T) {
	h := newHarness(t)
	author := h.caseProblem("caseowner", "P702")
	if code, _ := h.uploadZipAs(author, "/problems/P702/cases/upload", zipOf(map[string]string{
		"sample1.in": "1\n", "sample1.out": "1\n", "1.in": "2\n", "1.out": "2\n",
	})); code != http.StatusOK {
		t.Fatalf("author import = %d", code)
	}
	id := h.caseID("P702", "1")

	// Someone with no right on this problem cannot see the test set at all.
	stranger := h.signIn("casestrandger")
	for _, path := range []string{"/problems/P702/cases", "/problems/P702/cases/" + idText(id) + "/edit"} {
		if code, _ := h.getAs(stranger, path); code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", path, code)
		}
	}
	// Anonymous gets the login wall, not the data.
	if code, _ := h.htmlGet("/problems/P702/cases", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous case page = %d, want 401", code)
	}
	// A data maintainer who cannot edit statements can still manage cases.
	maintainer := h.signIn("casemaintainer", permTestcaseManage)
	if code, page := h.getAs(maintainer, "/problems/P702/cases"); code != http.StatusOK || !strings.Contains(page, "sample1") {
		t.Fatalf("maintainer case page = %d", code)
	}
	if code, _ := h.postAs(maintainer, "/problems/P702/cases/"+idText(id)+"/delete", url.Values{}); code != http.StatusOK {
		t.Fatalf("maintainer delete = %d", code)
	}
	// And the author keeps the statement rights the maintainer does not have.
	if code, _ := h.getAs(maintainer, "/problems/P702/edit"); code != http.StatusForbidden {
		t.Fatalf("maintainer reached the statement editor = %d", code)
	}
}

// 网页上传走的是 multipart + 隐藏 csrf 字段，和 raw zip + 头是两条解析路径：
// Go 1.25 起 ParseForm 不再解 multipart，只测后者会让网页上传整体 403 而测试全绿。
func TestCaseUploadFromTheBrowserForm(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("zipform", "P704")

	code, body := h.uploadMultipart(setter, "/problems/P704/cases/upload", setter.csrf,
		zipOf(map[string]string{"1.in": "1 2\n", "1.out": "3\n"}))
	if code != http.StatusOK || !strings.Contains(body, "导入完成") {
		t.Fatalf("browser upload = %d %s", code, short(body))
	}
	if n, _ := h.db.CountTestcases(ctx(), h.problem("P704").ID, false); n != 1 {
		t.Fatalf("cases after the browser upload = %d, want 1", n)
	}

	// 修这条不能顺手把闸门拆了：令牌错或没有令牌，仍然要 403。
	for _, csrf := range []string{"", "wrong-token"} {
		if code, _ := h.uploadMultipart(setter, "/problems/P704/cases/upload", csrf,
			zipOf(map[string]string{"2.in": "1 2\n", "2.out": "3\n"})); code != http.StatusForbidden {
			t.Errorf("upload with csrf %q = %d, want 403", csrf, code)
		}
	}

	// 超上限的请求体在解析阶段就该顶回去，而不是先落一盘临时文件。
	huge := bytes.Repeat([]byte("x"), 2<<20)
	if code, body := h.uploadMultipart(setter, "/problems/P704/cases/upload", setter.csrf, huge); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized upload = %d %s, want 413", code, short(body))
	}
	if n, _ := h.db.CountTestcases(ctx(), h.problem("P704").ID, false); n != 1 {
		t.Errorf("the rejected uploads changed the case set: %d rows", n)
	}
}

// batchValues 是网页那张批量表的形状：每条用例三个字段，键上带着用例 id。
func (h *harness) batchValues(pid string, names []string, scores []string, groups []string, samples []string) url.Values {
	h.t.Helper()
	v := url.Values{}
	for i, name := range names {
		id := idText(h.caseID(pid, name))
		v.Set("score_"+id, scores[i])
		v.Set("group_"+id, groups[i])
		if samples != nil && samples[i] == "1" {
			v.Set("sample_"+id, "1")
		}
	}
	return v
}

func TestCasesBatchSavesTheWholeGrid(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("batchsetter", "P710")
	names := []string{"sample1", "1", "2", "3"}
	if code, body := h.uploadZipAs(setter, "/problems/P710/cases/upload", zipOf(map[string]string{
		"sample1.in": "1 2\n", "sample1.out": "3\n",
		"1.in": "1 2\n", "1.out": "3\n", "2.in": "2 3\n", "2.out": "5\n", "3.in": "3 4\n", "3.out": "7\n",
	})); code != http.StatusOK {
		t.Fatalf("import = %d:\n%s", code, body)
	}

	v := h.batchValues("P710", names,
		[]string{"0", "40", "30", "30"}, []string{"", "sub1", "sub1", "sub2"}, nil)
	if _, body := h.postAs(setter, "/problems/P710/cases/batch", v); !strings.Contains(body, "已保存 4 条用例") {
		t.Fatalf("batch save:\n%s", body)
	}
	type want struct {
		name, group string
		score       int
		sample      bool
	}
	for _, w := range []want{
		{"sample1", "", 0, false},
		{"1", "sub1", 40, false},
		{"2", "sub1", 30, false},
		{"3", "sub2", 30, false},
	} {
		tc, err := h.db.GetTestcase(ctx(), h.problem("P710").ID, w.name)
		if err != nil {
			t.Fatalf("case %s: %v", w.name, err)
		}
		if tc.Score != w.score || tc.GroupName != w.group {
			t.Errorf("%s = %d/%q, want %d/%q", w.name, tc.Score, tc.GroupName, w.score, w.group)
		}
	}
	if total, _ := h.db.ProblemScoreTotal(ctx(), h.problem("P710").ID); total != 100 {
		t.Errorf("total = %d, want 100", total)
	}
	// 捆绑汇总跟着新组名走：sub1 两条共 70 分。
	page := h.getPage(setter, "/problems/P710/cases")
	for _, mark := range []string{"sub1", "sub2", "70"} {
		if !strings.Contains(page, mark) {
			t.Errorf("the group summary lost %q:\n%s", mark, page)
		}
	}
}

// 样例不计分：勾上样例还填了分，保存的是 0，不是那句报错。
func TestCasesBatchSamplePaysNothing(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("sampleset", "P711")
	h.uploadZipAs(setter, "/problems/P711/cases/upload", zipOf(map[string]string{
		"1.in": "1 2\n", "1.out": "3\n", "2.in": "2 3\n", "2.out": "5\n",
	}))
	v := h.batchValues("P711", []string{"1", "2"}, []string{"50", "50"}, []string{"", ""}, []string{"1", "0"})
	if _, body := h.postAs(setter, "/problems/P711/cases/batch", v); !strings.Contains(body, "已保存 2 条") {
		t.Fatalf("batch:\n%s", body)
	}
	first, _ := h.db.GetTestcase(ctx(), h.problem("P711").ID, "1")
	if !first.IsSample || first.Score != 0 {
		t.Errorf("the sample case should pay nothing: %+v", first)
	}
	if total, _ := h.db.ProblemScoreTotal(ctx(), h.problem("P711").ID); total != 50 {
		t.Errorf("total = %d, want 50 (only the judged case counts)", total)
	}
}

// 一格填坏就整批不保存：半张分值表会让下一次提交按一个不存在的总分判。
func TestCasesBatchIsAllOrNothing(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("atomsetter", "P712")
	h.uploadZipAs(setter, "/problems/P712/cases/upload", zipOf(map[string]string{
		"1.in": "1 2\n", "1.out": "3\n", "2.in": "2 3\n", "2.out": "5\n",
	}))
	before := map[string]int{}
	for _, name := range []string{"1", "2"} {
		tc, _ := h.db.GetTestcase(ctx(), h.problem("P712").ID, name)
		before[name] = tc.Score
	}
	v := h.batchValues("P712", []string{"1", "2"}, []string{"7", "99999"}, []string{"g", "g"}, nil)
	code, body := h.postAs(setter, "/problems/P712/cases/batch", v)
	if code == http.StatusOK || !strings.Contains(body, "0..10000") {
		t.Fatalf("a bad score slipped through: %d\n%s", code, body)
	}
	for _, name := range []string{"1", "2"} {
		tc, _ := h.db.GetTestcase(ctx(), h.problem("P712").ID, name)
		if tc.Score != before[name] || tc.GroupName != "" {
			t.Errorf("case %s changed anyway: %+v", name, tc)
		}
	}
}

// 批量表只列这道题的用例：伪造别题的字段名不会改到别人的数据。
func TestCasesBatchCannotReachAnotherProblemsCases(t *testing.T) {
	h := newHarness(t)
	owner := h.caseProblem("otherowner", "P713")
	setter := h.caseProblem("batchsetter2", "P714")
	h.uploadZipAs(owner, "/problems/P713/cases/upload", zipOf(map[string]string{"1.in": "1\n", "1.out": "1\n"}))
	h.uploadZipAs(setter, "/problems/P714/cases/upload", zipOf(map[string]string{"1.in": "2\n", "1.out": "2\n"}))

	foreign := idText(h.caseID("P713", "1"))
	v := url.Values{"score_" + foreign: {"500"}, "group_" + foreign: {"偷改"}}
	own := idText(h.caseID("P714", "1"))
	v.Set("score_"+own, "9")
	if _, body := h.postAs(setter, "/problems/P714/cases/batch", v); !strings.Contains(body, "已保存 1 条") {
		t.Fatalf("batch:\n%s", body)
	}
	kept, _ := h.db.GetTestcase(ctx(), h.problem("P713").ID, "1")
	if kept.Score == 500 || kept.GroupName == "偷改" {
		t.Errorf("another problem's case was edited: %+v", kept)
	}
	moved, _ := h.db.GetTestcase(ctx(), h.problem("P714").ID, "1")
	if moved.Score != 9 {
		t.Errorf("this problem's own case did not change: %+v", moved)
	}
	// 没权限的人连这张表都提交不了。
	stranger := h.signIn("batchstranger")
	if code, _ := h.postAs(stranger, "/problems/P714/cases/batch", v); code != http.StatusForbidden {
		t.Errorf("stranger batch = %d, want 403", code)
	}
}

// 平分是个"先填好再保存"的助手：除不尽的余数摊给前面几条，且不动样例。
func TestCasesBatchSplitIsAPreviewNotAnApply(t *testing.T) {
	h := newHarness(t)
	setter := h.caseProblem("splitter", "P715")
	h.uploadZipAs(setter, "/problems/P715/cases/upload", zipOf(map[string]string{
		"sample1.in": "1\n", "sample1.out": "1\n",
		"1.in": "1\n", "1.out": "1\n", "2.in": "2\n", "2.out": "2\n", "3.in": "3\n", "3.out": "3\n",
	}))
	// 复选框在真实表单里总是带着当前状态提交的：勾着样例的那行必须继续勾上。
	v := h.batchValues("P715", []string{"sample1", "1", "2", "3"},
		[]string{"0", "1", "1", "1"}, []string{"", "", "", ""}, []string{"1", "0", "0", "0"})
	v.Set("apply", "split")
	v.Set("split_total", "100")
	_, page := h.postAs(setter, "/problems/P715/cases/batch", v)
	for _, mark := range []string{`value="34"`, `value="33"`, "平分"} {
		if !strings.Contains(page, mark) {
			t.Errorf("the preview is missing %q:\n%s", mark, page)
		}
	}
	if total, _ := h.db.ProblemScoreTotal(ctx(), h.problem("P715").ID); total != 3 {
		t.Errorf("apply=split saved to the database (total %d); it should only fill the form", total)
	}
}

// 单个测试点的存储大小默认没有上限：-max-case=0 就是不限量，存多大是出题人的事。
// 设成正数时闸门必须还在——它挡的从来不是合法的大用例，而是无界的上传。
func TestCaseSizeCapIsOffByDefaultAndStillWorksWhenSet(t *testing.T) {
	heavy := strings.Repeat("0\n", 600_000) // 1.2 MB 正文；压进 zip 只有几 KB
	archive := zipOf(map[string]string{"1.in": heavy, "1.out": "ok\n"})

	opened := newHarnessCfg(t, func(c *config.Config) { c.MaxCase = 0; c.MaxUpload = 0 })
	setter := opened.caseProblem("capoff", "P712")
	if code, body := opened.uploadZipAs(setter, "/problems/P712/cases/upload", archive); code != http.StatusOK {
		t.Fatalf("闸门关掉时这条 1.2 MB 该收：%d\n%s", code, short(body))
	}
	if id := opened.caseID("P712", "1"); id == 0 {
		t.Fatal("用例没落库")
	}

	kept := newHarnessCfg(t, func(c *config.Config) { c.MaxCase = 1 << 20; c.MaxUpload = 0 })
	keeper := kept.caseProblem("capon", "P713")
	code, body := kept.uploadZipAs(keeper, "/problems/P713/cases/upload", archive)
	if !strings.Contains(body, "over 1048576 bytes") {
		t.Fatalf("设了 1 MB 的单条闸门，1.2 MB 却照样收下了：%d\n%s", code, short(body))
	}
	if _, err := kept.db.GetTestcase(ctx(), kept.problem("P713").ID, "1"); err == nil {
		t.Error("被闸门挡住的那条还是落库了")
	}
}

// 整包闸门关掉时，请求体不能被"闸门 + 内存预算"那 4 MB 悄悄卡住：那是把不限量
// 说成 4 MB，比原来 64 MB 还小。
func TestUploadWithoutCapAcceptsALargeArchive(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) { c.MaxCase = 0; c.MaxUpload = 0 })
	setter := h.caseProblem("bigupload", "P714")
	// 约 9 MB 的压缩前内容，且不可压缩（伪随机字节），zip 本身就有几 MB。
	body := make([]byte, 4<<20)
	for i := range body {
		body[i] = byte(i*131 + i/7)
	}
	archive := zipOf(map[string]string{"1.in": string(body), "1.out": "ok\n"})
	if code, page := h.uploadZipAs(setter, "/problems/P714/cases/upload", archive); code != http.StatusOK {
		t.Fatalf("不限量时这份 %d KB 的包该收下：%d\n%s", len(archive)/1024, code, short(page))
	}
}
