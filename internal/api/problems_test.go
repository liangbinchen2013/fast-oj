package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

func zipOf(files map[string]string) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			panic(err)
		}
		if _, err := f.Write([]byte(files[name])); err != nil {
			panic(err)
		}
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestProblemAuthBoundaries(t *testing.T) {
	h := newHarness(t)

	// An anonymous authoring attempt is not a dead end: the browser is sent to the
	// login page with the form it was on as the return target.
	code, _, hdr := h.postNoRedirect("/problems", url.Values{"pid": {"P1"}, "title": {"t"}}, "")
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login?next=") {
		t.Fatalf("anonymous create = %d %q, want a login redirect", code, hdr.Get("Location"))
	}
	if code, _ := h.createProblem(url.Values{"pid": {"P1"}, "title": {"t"}}); code != http.StatusOK {
		t.Fatalf("admin create = %d", code)
	}

	// A signed-in person without the capability cannot author.
	tester := h.signIn("tester")
	if code, _ := h.postAs(tester, "/problems", url.Values{"pid": {"P2"}, "title": {"t2"}}); code != http.StatusForbidden {
		t.Fatalf("no-permission create = %d, want 403", code)
	}
	// ...but can read the public list.
	if code, body := h.getAs(tester, "/problems"); code != http.StatusOK || !strings.Contains(body, "P1") {
		t.Fatalf("user list = %d %q", code, body)
	}
}

func TestProblemCRUDThroughTheWebForm(t *testing.T) {
	h := newHarness(t)

	values := url.Values{
		"pid": {"P1001"}, "title": {"A 加 B Problem"},
		"statement": {"Give a and b, print a+b.\nHave 3 spaces."},
		"time_ms":   {"1500"}, "mem_mb": {"128"}, "languages": {"c", "cpp"},
	}
	// Creating lands on the case page, because an empty problem cannot be judged
	// and that is the author's next job.
	if code, body := h.createProblem(values); code != http.StatusOK ||
		!strings.Contains(body, "还没有用例") || !strings.Contains(body, "加一条") {
		t.Fatalf("create = %d %q", code, body)
	}

	// A duplicate pid comes back as the form with what was typed, not a blank page.
	if code, body := h.createProblem(values); code != http.StatusConflict || !strings.Contains(body, "已经存在") {
		t.Fatalf("duplicate = %d %q", code, body)
	}

	code, detail := h.htmlGet("/problems/P1001", "")
	if code != http.StatusOK {
		t.Fatalf("get = %d %q", code, detail)
	}
	for _, want := range []string{"A 加 B Problem", "1500ms", "128.00M", "Give a and b", "提交代码"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail missing %q:\n%s", want, detail)
		}
	}

	// The edit form prefills every field, so saving again cannot silently drop a
	// value the author did not touch.
	setter := h.signIn("setter1001", permProblemEditAny)
	code, page := h.getAs(setter, "/problems/P1001/edit")
	if code != http.StatusOK || !strings.Contains(page, `value="1500"`) || !strings.Contains(page, `value="128"`) {
		t.Fatalf("edit form = %d\n%s", code, page)
	}
	if code, body := h.postAs(setter, "/problems/P1001", url.Values{
		"pid": {"P1001"}, "title": {"A 加 B (revised)"},
		"statement": {"Give a and b, print a+b.\nHave 3 spaces."},
		"time_ms":   {"1500"}, "mem_mb": {"128"}, "languages": {"c", "cpp"}, "visible": {"1"},
	}); code != http.StatusOK || !strings.Contains(body, "revised") || !strings.Contains(body, "1500ms") {
		t.Fatalf("update = %d %q", code, body)
	}

	bad := []struct {
		name   string
		values url.Values
	}{
		{"empty pid", url.Values{"pid": {""}, "title": {"x"}, "time_ms": {"1000"}, "mem_mb": {"256"}}},
		{"empty title", url.Values{"pid": {"P2"}, "title": {""}, "time_ms": {"1000"}, "mem_mb": {"256"}}},
		{"zero time", url.Values{"pid": {"P2"}, "title": {"x"}, "time_ms": {"0"}, "mem_mb": {"256"}}},
		{"junk time", url.Values{"pid": {"P2"}, "title": {"x"}, "time_ms": {"abc"}, "mem_mb": {"256"}}},
		{"zero memory", url.Values{"pid": {"P2"}, "title": {"x"}, "time_ms": {"1000"}, "mem_mb": {"0"}}},
		{"bad language", url.Values{"pid": {"P2"}, "title": {"x"}, "time_ms": {"1000"}, "mem_mb": {"256"}, "languages": {"c++!"}}},
		{"long pid", url.Values{"pid": {"way-too-long-to-be-a-pid-aaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "title": {"x"}, "time_ms": {"1000"}, "mem_mb": {"256"}}},
		{"long title", url.Values{"pid": {"P2"}, "title": {strings.Repeat("标", 201)}, "time_ms": {"1000"}, "mem_mb": {"256"}}},
	}
	for _, c := range bad {
		code, body := h.postBearer("/problems", c.values, h.adminToken())
		if code != http.StatusBadRequest {
			t.Errorf("%s: accepted with %d", c.name, code)
		}
		// What was typed stays in the boxes; the author never retypes a whole
		// statement because one number was off.
		if title := c.values.Get("title"); title != "" && !strings.Contains(body, `value="`+title+`"`) {
			t.Errorf("%s: the form lost the title:\n%s", c.name, body)
		}
	}

	// Delete goes through its own POST, so a stray GET can never destroy a problem.
	if code, body := h.postAs(setter, "/problems/P1001/delete", url.Values{}); code != http.StatusOK ||
		!strings.Contains(body, "已删除 P1001") {
		t.Fatalf("delete = %d %q", code, body)
	}
	if code, _ := h.htmlGet("/problems/P1001", ""); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", code)
	}
	if code, _ := h.postBearer("/problems/P1001/delete", url.Values{}, h.adminToken()); code != http.StatusNotFound {
		t.Fatalf("double delete = %d", code)
	}
}

func TestProblemPreviewRendersWithoutSaving(t *testing.T) {
	h := newHarness(t)
	setter := h.signIn("previewer", permProblemCreate)

	code, page := h.postAs(setter, "/problems", url.Values{
		"pid": {"PV"}, "title": {"预览题"}, "time_ms": {"1000"}, "mem_mb": {"256"},
		"statement": {"## 加法\n\n给定 $a+b$，输出和。"}, "preview": {"1"},
	})
	if code != http.StatusOK {
		t.Fatalf("preview = %d %q", code, page)
	}
	for _, want := range []string{"<h2>加法</h2>", `class="math inline"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("preview missing %q:\n%s", want, page)
		}
	}
	if _, err := h.db.FindProblem(ctx(), "PV"); err == nil {
		t.Fatal("preview saved the problem")
	}
}

func TestHiddenProblemVisibility(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.createProblem(url.Values{"pid": {"H1"}, "title": {"hidden one"}, "visible": {"0"}}); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}

	if code, _ := h.htmlGet("/problems/H1", ""); code != http.StatusNotFound {
		t.Fatalf("anon hidden detail = %d, want 404", code)
	}
	if _, listed := h.htmlGet("/problems", ""); strings.Contains(listed, "H1") {
		t.Fatalf("anon list leaked hidden:\n%s", listed)
	}
	if code, body := h.htmlGet("/problems", h.adminToken()); code != http.StatusOK || !strings.Contains(body, "H1") {
		t.Fatalf("admin list = %d %q", code, body)
	}
	// The 未公开 marker is what tells an author their problem is not in the list.
	if code, body := h.htmlGet("/problems/H1", h.adminToken()); code != http.StatusOK || !strings.Contains(body, "未公开") {
		t.Fatalf("hidden detail = %d", code)
	}
	// 隐藏题的样例也是题目存在性的把手：未登录不能通过用例 URL 探出来，维护者仍能取。
	h.uploadCases("H1")
	if code, _ := h.get("/problems/H1/cases/sample1/in", ""); code != http.StatusNotFound {
		t.Fatalf("anon hidden sample = %d, want 404", code)
	}
	if code, body := h.get("/problems/H1/cases/sample1/in", h.adminToken()); code != http.StatusOK || body != "1 2\n" {
		t.Fatalf("admin hidden sample = %d %q", code, body)
	}
}

func TestCaseUploadAndAccess(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.createProblem(url.Values{"pid": {"P1"}, "title": {"Cases"}}); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}

	archive := zipOf(map[string]string{
		"sample1.in": "1 2\n", "sample1.out": "3\n",
		"sample2.in": "10 20\n", "sample2.out": "30\n",
		"1.in": "3 4\n", "1.out": "7\n",
		"3.in": "5 6\n", "3.out": "11\n",
	})

	// Cases are invisible until an operator uploads them.
	if code, _ := h.do(http.MethodPost, "/problems/P1/cases/upload", "", archive, "application/zip"); code != http.StatusUnauthorized {
		t.Fatalf("anon upload = %d", code)
	}
	// A garbage body is refused in the author's own language and nothing is added.
	code, body := h.do(http.MethodPost, "/problems/P1/cases/upload", h.adminToken(), []byte("not a zip"), "application/zip")
	if code != http.StatusOK || !strings.Contains(body, "zip") {
		t.Fatalf("garbage upload = %d %q", code, body)
	}
	if n, _ := h.db.CountTestcases(ctx(), 1, false); n != 0 {
		t.Fatalf("a bad archive added %d cases", n)
	}
	if code, body := h.do(http.MethodPost, "/problems/P1/cases/upload", h.adminToken(), archive, "application/zip"); code != http.StatusOK {
		t.Fatalf("upload = %d %q", code, body)
	}

	// Re-uploading the same names replaces them instead of duplicating, and the
	// samples-first ordinal layout stays stable across the second pass.
	if code, body := h.do(http.MethodPost, "/problems/P1/cases/upload", h.adminToken(), archive, "application/zip"); code != http.StatusOK {
		t.Fatalf("re-upload = %d %q", code, body)
	}
	cases, err := h.db.ListTestcases(ctx(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"sample1", "sample2", "1", "3"}
	if len(cases) != 4 {
		t.Fatalf("cases = %+v", cases)
	}
	for i, tc := range cases {
		if tc.Name != wantOrder[i] || tc.Ordinal != i+1 || tc.IsSample != (i < 2) {
			t.Fatalf("case %d = %+v, want %s at ordinal %d as sample=%v", i, tc, wantOrder[i], i+1, i < 2)
		}
	}

	if code, _ := h.htmlGet("/problems/P1/cases", ""); code != http.StatusUnauthorized {
		t.Fatalf("anon case list = %d", code)
	}

	// Samples show up on the public problem page, judged data does not.
	_, detail := h.htmlGet("/problems/P1", "")
	if !strings.Contains(detail, "样例") || !strings.Contains(detail, "1 2") {
		t.Fatalf("public page lost samples:\n%s", detail)
	}
	if strings.Contains(detail, "3 4") {
		t.Fatalf("public page leaked a judged case:\n%s", detail)
	}

	if code, body := h.get("/problems/P1/cases/sample1/in", ""); code != http.StatusOK || body != "1 2\n" {
		t.Fatalf("sample input = %d %q", code, body)
	}
	if code, _ := h.get("/problems/P1/cases/1/out", ""); code != http.StatusForbidden {
		t.Fatalf("anon judged case = %d", code)
	}
	// Case "1" sorts to ordinal 3 behind the two samples.
	if code, body := h.get("/problems/P1/cases/1/out", h.adminToken()); code != http.StatusOK || body != "7\n" {
		t.Fatalf("admin judged case = %d %q", code, body)
	}
	if code, _ := h.get("/problems/P1/cases/nope/in", h.adminToken()); code != http.StatusNotFound {
		t.Fatalf("unknown case = %d", code)
	}

	// Clearing the set removes the rows along with the files.
	if code, body := h.postBearer("/problems/P1/cases/wipe", url.Values{}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("wipe = %d %q", code, body)
	}
	if rest, err := h.db.ListTestcases(ctx(), 1, false); err != nil || len(rest) != 0 {
		t.Fatalf("after wipe = %+v %v", rest, err)
	}
}

func TestIncompleteCasePairsRejected(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.createProblem(url.Values{"pid": {"P9"}, "title": {"Broken set"}}); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("lonely.in")
	f.Write([]byte("only input\n"))
	w.Close()

	code, body := h.do(http.MethodPost, "/problems/P9/cases/upload", h.adminToken(), buf.Bytes(), "application/zip")
	if code != http.StatusOK || !strings.Contains(body, "lonely") {
		t.Fatalf("unpaired upload = %d %q", code, body)
	}
	p9, err := h.db.FindProblem(ctx(), "P9")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := h.db.CountTestcases(ctx(), p9.ID, false); n != 0 {
		t.Fatalf("half a pair created %d cases", n)
	}
}

func TestListPagination(t *testing.T) {
	h := newHarness(t)
	for i := 1; i <= 7; i++ {
		if code, body := h.createProblem(url.Values{
			"pid": {fmt.Sprintf("P%02d", i)}, "title": {fmt.Sprintf("problem %d", i)},
		}); code != http.StatusOK {
			t.Fatalf("create %d = %d %q", i, code, body)
		}
	}

	_, page1 := h.htmlGet("/problems?limit=3", "")
	for _, want := range []string{"共 7 题", "下一页 →", "P01", "P02", "P03"} {
		if !strings.Contains(page1, want) {
			t.Fatalf("page1 missing %q:\n%s", want, page1)
		}
	}
	if strings.Contains(page1, "P04") {
		t.Fatalf("page1 went past its limit:\n%s", page1)
	}

	_, page2 := h.htmlGet("/problems?limit=3&after=3", "")
	if !strings.Contains(page2, "P04") || strings.Contains(page2, "P03") {
		t.Fatalf("page2:\n%s", page2)
	}

	_, last := h.htmlGet("/problems?limit=3&after=6", "")
	if !strings.Contains(last, "P07") || strings.Contains(last, "下一页") {
		t.Fatalf("last page:\n%s", last)
	}

	_, search := h.htmlGet("/problems?q=P05", "")
	if !strings.Contains(search, "P05") || strings.Contains(search, "P04") {
		t.Fatalf("search:\n%s", search)
	}
}

func TestProblemMemoryFieldRoundTrip(t *testing.T) {
	for kb, want := range map[int]string{
		262144: "256", 1024: "1", 1536: "1.5", 100: "0.098", 4194304: "4096",
	} {
		if got := memMBText(kb); got != want {
			t.Errorf("memMBText(%d) = %q, want %q", kb, got, want)
		}
	}

	// The form takes megabytes and the judge gets kilobytes: a trip through the
	// editor must not drift.
	req := httptest.NewRequest(http.MethodPost, "/problems",
		strings.NewReader(url.Values{"mem_mb": {memMBText(131072)}, "time_ms": {"1000"},
			"pid": {"X"}, "title": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f, errs := parseProblemForm(req)
	if len(errs) != 0 || f.memKB() != 131072 {
		t.Fatalf("round trip = %+v %v", f, errs)
	}
}
