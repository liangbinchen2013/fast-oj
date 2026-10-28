package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// readyProblem creates a problem that accepts c/cpp and uploads two judged
// cases plus one sample.
func (h *harness) readyProblem(pid string) {
	h.t.Helper()
	if code, body := h.createProblem(url.Values{"pid": {pid}, "title": {"case ready"}, "languages": {"c", "cpp"}}); code != http.StatusOK {
		h.t.Fatalf("create problem = %d %q", code, body)
	}
	h.uploadCases(pid)
}

// submitRaw posts the code form without following the redirect, so the 303
// target and its flash message stay assertable.
func (h *harness) submitRaw(pid string, values url.Values, s session) (int, string, http.Header) {
	h.t.Helper()
	return h.postForm("/problems/"+url.PathEscape(pid)+"/submit", values, s.cookie, s.csrf)
}

func (h *harness) uploadCases(pid string) {
	h.t.Helper()
	archive := zipOf(map[string]string{
		"sample1.in": "1 2\n", "sample1.out": "3\n",
		"1.in": "1 2\n", "1.out": "3\n",
		"2.in": "4 5\n", "2.out": "9\n",
	})
	if code, body := h.do(http.MethodPost, "/problems/"+pid+"/cases/upload", h.adminToken(), archive, "application/zip"); code != http.StatusOK {
		h.t.Fatalf("upload cases = %d %q", code, body)
	}
}

func TestSubmissionFormValidation(t *testing.T) {
	h := newHarness(t)
	author := h.signIn("submitter")
	stranger := h.signIn("nosuch")
	h.readyProblem("P2000")

	// Someone not signed in is sent to the login page with the problem as the
	// return target, rather than being told "401".
	code, _, hdr := h.postForm("/problems/P2000/submit", url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, "", "")
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login?next=%2Fproblems%2FP2000") {
		t.Fatalf("anonymous submit = %d %q", code, hdr.Get("Location"))
	}

	cases := []struct {
		name, problem, lang, source, want string
	}{
		{"unknown problem", "NOPE", "cpp", "x", "找不到这道题"},
		{"language not accepted", "P2000", "pascal", "x", "这道题只收 c/cpp"},
		{"blank language", "P2000", "", "x", "题目与语言都要填"},
		{"empty source", "P2000", "cpp", "   ", "源代码是空的"},
		{"nul byte", "P2000", "cpp", "a\x00b", "空字节"},
	}
	for _, c := range cases {
		code, _, hdr := h.submitRaw(c.problem, url.Values{"language": {c.lang}, "source": {c.source}}, author)
		if code != http.StatusSeeOther {
			t.Errorf("%s: code = %d, want a redirect back to the problem", c.name, code)
			continue
		}
		if !strings.Contains(hdr.Get("Location"), url.QueryEscape(c.want)) {
			t.Errorf("%s: flash = %q, want it to say %q", c.name, hdr.Get("Location"), c.want)
		}
	}

	// A problem with no judged cases cannot be graded.
	h.createProblem(url.Values{"pid": {"EMPTY"}, "title": {"no cases"}})
	if _, _, hdr := h.submitRaw("EMPTY", url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, author); !strings.Contains(hdr.Get("Location"), url.QueryEscape("还没有判题用例")) {
		t.Fatalf("empty problem flash = %q", hdr.Get("Location"))
	}

	// A hidden problem stays invisible to everyone but its author.
	if _, _, hdr := h.submitRaw("HIDE", url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, stranger); !strings.Contains(hdr.Get("Location"), url.QueryEscape("找不到这道题")) {
		t.Fatalf("hidden problem flash for a stranger = %q", hdr.Get("Location"))
	}
	owner := h.signIn("hideowner", permProblemCreate)
	if code, body := h.postAs(owner, "/problems", hiddenValues("HIDE", "自己的隐藏题")); code != http.StatusOK {
		t.Fatalf("create hidden = %d %s", code, body)
	}
	h.uploadCasesAs(owner, "HIDE")
	if code, _, hdr := h.submitRaw("HIDE", url.Values{"language": {"cpp"}, "source": {"int main(){}"}}, owner); code != http.StatusSeeOther ||
		!strings.HasPrefix(hdr.Get("Location"), "/submissions/") {
		t.Fatalf("author submit to own hidden problem = %d %q", code, hdr.Get("Location"))
	}
}

func TestSubmissionRoundTripAndVisibility(t *testing.T) {
	h := newHarness(t)
	alice := h.signIn("alice")
	bob := h.signIn("bob")
	h.readyProblem("P2001")

	code, _, hdr := h.submitRaw("P2001", url.Values{
		"language": {"cpp"},
		"source":   {"#include <bits/stdc++.h>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b;}"},
	}, alice)
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/submissions/") {
		t.Fatalf("submit = %d %q", code, hdr.Get("Location"))
	}
	page := hdr.Get("Location")

	// The owner sees the queued row with the code they just wrote.
	_, body := h.getAs(alice, page)
	if !strings.Contains(body, "P2001") || !strings.Contains(body, "alice") {
		t.Fatalf("owner view of a queued row:\n%s", body)
	}
	if !strings.Contains(body, "std::cin") {
		t.Fatal("the author cannot read back their own code")
	}

	// Another person may see the state but never the source or the case table.
	_, foreign := h.getAs(bob, page)
	// The state stays public; the code and the per-case table do not travel.
	if strings.Contains(foreign, "std::cin") {
		t.Fatalf("a stranger leaked the submission body:\n%s", foreign)
	}
	if !strings.Contains(foreign, "只对本人、出题人和管理员可见") {
		t.Fatalf("the private-data hint is missing for a stranger:\n%s", foreign)
	}

	// A judge box claims the row and reports the verdict.
	h.finishSubmission(1, store.Result{Verdict: "AC", TimeMS: 12, MemKB: 3072, Score: 2, TotalScore: 2}, []store.Run{
		{Ordinal: 2, CaseName: "1", Verdict: "AC", TimeMS: 12, MemKB: 3072},
		{Ordinal: 3, CaseName: "2", Verdict: "AC", TimeMS: 9, MemKB: 3000},
	})

	code, body = h.getAs(alice, page)
	if code != http.StatusOK {
		t.Fatalf("judged detail = %d", code)
	}
	for _, want := range []string{"12ms", "3.00M", "2/2", "逐用例"} {
		if !strings.Contains(body, want) {
			t.Fatalf("judged view missing %q:\n%s", want, body)
		}
	}
	if _, foreign = h.getAs(bob, page); strings.Contains(foreign, "std::cin") {
		t.Fatalf("source leaked after judging:\n%s", foreign)
	}

	// The failing case and the judge's diff are the author's business alone.
	h.readyProblem("P2002")
	if code, _, _ := h.submitRaw("P2002", url.Values{"language": {"cpp"}, "source": {"x"}}, alice); code != http.StatusSeeOther {
		t.Fatalf("submit = %d", code)
	}
	h.finishSubmission(2, store.Result{
		Verdict: "WA", TimeMS: 5, MemKB: 1024, Score: 1, TotalScore: 2, FailedCase: "1",
		Detail: "实测 4\n期望 3",
	}, []store.Run{
		{Ordinal: 1, CaseName: "sample1", Verdict: "AC", TimeMS: 11, MemKB: 2048, IsSample: true, Score: 0},
		{Ordinal: 2, CaseName: "1", Verdict: "AC", TimeMS: 12, MemKB: 2048, Score: 1},
		{Ordinal: 3, CaseName: "2", Verdict: "WA", TimeMS: 12, MemKB: 2048, Score: 0},
	})
	_, mine := h.getAs(alice, "/submissions/2")
	for _, want := range []string{"失败用例", "实测 4"} {
		if !strings.Contains(mine, want) {
			t.Fatalf("WA view missing %q:\n%s", want, mine)
		}
	}
	if _, other := h.htmlGet("/submissions/2", ""); strings.Contains(other, "实测 4") {
		t.Fatalf("the judge diff leaked to an anonymous reader:\n%s", other)
	}

	// Listings.
	if code, body := h.getAs(alice, "/submissions?user=me"); code != http.StatusOK || !strings.Contains(body, "P2001") {
		t.Fatalf("my list = %d", code)
	}
	if _, byProblem := h.htmlGet("/submissions?problem=P2001", ""); !strings.Contains(byProblem, "alice") {
		t.Fatalf("problem list lost the row:\n%s", byProblem)
	}
	if _, byBob := h.htmlGet("/submissions?user=bob", ""); !strings.Contains(byBob, "还没有提交") {
		t.Fatalf("empty list for bob:\n%s", byBob)
	}
	if code, _ := h.htmlGet("/submissions?user=nobody", ""); code != http.StatusNotFound {
		t.Fatalf("unknown handle filter = %d", code)
	}
	if code, _ := h.htmlGet("/submissions/abc", ""); code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", code)
	}
	if code, _ := h.htmlGet("/submissions/9999", ""); code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", code)
	}
}

// The detail level is what lets a contest hide the failing case while still
// showing the score, and it must never hide anything from the author.
func TestDetailLevelGradesTheSubmissionPage(t *testing.T) {
	h := newHarness(t)
	author := h.signIn("lvlauthor", permProblemCreate)
	solver := h.signIn("lvlsolver")

	form := func(detail string) url.Values {
		return url.Values{
			"pid": {"P2100"}, "title": {"详情级别"}, "time_ms": {"1000"}, "mem_mb": {"256"},
			"detail": {detail}, "visible": {"1"},
		}
	}
	if code, body := h.postAs(author, "/problems", form(store.DetailNone)); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, short(body))
	}
	h.uploadCases("P2100")
	if code, _, _ := h.submitRaw("P2100", url.Values{"language": {"cpp"}, "source": {"int main(){return 0;}"}}, solver); code != http.StatusSeeOther {
		t.Fatalf("submit = %d", code)
	}
	h.finishSubmission(1, store.Result{
		Verdict: "WA", TimeMS: 12, MemKB: 2048, Score: 1, TotalScore: 3, FailedCase: "2",
		Detail: "实测 4\n期望 3",
	}, []store.Run{
		{Ordinal: 1, CaseName: "sample1", Verdict: "AC", TimeMS: 11, MemKB: 2048, IsSample: true},
		{Ordinal: 2, CaseName: "1", Verdict: "AC", TimeMS: 12, MemKB: 2048, Score: 1},
		{Ordinal: 3, CaseName: "2", Verdict: "WA", TimeMS: 12, MemKB: 2048},
	})
	page := "/submissions/1"

	// none: the score is public enough to rank by, the rest is not.
	_, mine := h.getAs(solver, page)
	for _, want := range []string{"1/3"} {
		if !strings.Contains(mine, want) {
			t.Errorf("level none lost %q:\n%s", want, mine)
		}
	}
	// The code and the score stay theirs; what the judging revealed does not.
	for _, hide := range []string{"逐用例", "实测 4", "失败用例", "判机"} {
		if !strings.Contains(mine, "int main()") {
			t.Error("the submitter lost their own code")
		}
		if strings.Contains(mine, hide) {
			t.Errorf("level none leaked %q:\n%s", hide, mine)
		}
	}

	// The author sees everything regardless of the level they set.
	_, all := h.getAs(author, page)
	for _, want := range []string{"逐用例", "int main()", "实测 4", "失败用例"} {
		if !strings.Contains(all, want) {
			t.Errorf("author lost %q:\n%s", want, all)
		}
	}

	// case: the per-case verdicts arrive, the diff and the timings do not.
	if code, body := h.postAs(author, "/problems/P2100", form(store.DetailCase)); code != http.StatusOK {
		t.Fatalf("save case level = %d %s", code, short(body))
	}
	_, mine = h.getAs(solver, page)
	if !strings.Contains(mine, "逐用例") || !strings.Contains(mine, "sample1") {
		t.Fatalf("level case did not open the per-case table:\n%s", mine)
	}
	for _, hide := range []string{"实测 4", `<th class="num">用时</th>`, "判机信息"} {
		if strings.Contains(mine, hide) {
			t.Errorf("level case leaked %q:\n%s", hide, mine)
		}
	}

	// diff: the comparison and the failing case name come with it.
	if code, body := h.postAs(author, "/problems/P2100", form(store.DetailDiff)); code != http.StatusOK {
		t.Fatalf("save diff level = %d %s", code, short(body))
	}
	_, mine = h.getAs(solver, page)
	for _, want := range []string{"实测 4", `<th class="num">用时</th>`, "失败用例"} {
		if !strings.Contains(mine, want) {
			t.Errorf("level diff lost %q:\n%s", want, mine)
		}
	}

	// An anonymous reader never gets the code, at any level.
	if _, anon := h.htmlGet(page, ""); strings.Contains(anon, "int main()") {
		t.Fatal("the source leaked to an anonymous reader")
	}
}

func TestPageLimitClamps(t *testing.T) {
	for in, want := range map[string]int{"": 50, "0": 50, "-1": 50, "10": 10, "9999": 500} {
		if got := pageLimit(in); got != want {
			t.Errorf("pageLimit(%q) = %d, want %d", in, got, want)
		}
	}
}
