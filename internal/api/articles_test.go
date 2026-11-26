package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestSolutionsAndColumns(t *testing.T) {
	h := newHarness(t)
	author := h.signIn("writer")
	other := h.signIn("readonly2")
	if code, body := h.createProblem(problemValues("AR1", "题解题")); code != http.StatusOK {
		t.Fatalf("create problem: %d %s", code, body)
	}

	// 题解：挂在题上，公开后出现在题目题解页。
	if code, body := h.postAs(author, "/solutions", url.Values{
		"problem": {"AR1"}, "title": {"从暴力到正解"},
		"body": {"先写 $O(n^2)$，再用前缀和。"}, "visible": {"1"},
	}); code != http.StatusOK || !strings.Contains(body, "从暴力到正解") {
		t.Fatalf("create solution: %d %s", code, body)
	}
	solutionID := latestArticleID(t, h)
	if code, body := h.getAs(other, "/problems/AR1/solutions"); code != http.StatusOK ||
		!strings.Contains(body, "从暴力到正解") {
		t.Fatalf("solution list: %d %s", code, body)
	}
	if code, body := h.getAs(other, "/solutions/"+itoa64(solutionID)); code != http.StatusOK ||
		!strings.Contains(body, "前缀和") {
		t.Fatalf("solution detail: %d %s", code, body)
	}

	// 别人不能改；作者能改。
	if code, _ := h.postAs(other, "/solutions/"+itoa64(solutionID), url.Values{
		"title": {"劫持"}, "body": {"x"}, "visible": {"1"},
	}); code != http.StatusForbidden {
		t.Fatalf("foreign update: %d", code)
	}
	if code, body := h.postAs(author, "/solutions/"+itoa64(solutionID), url.Values{
		"title": {"从暴力到正解（修订）"}, "body": {"改成线性。"}, "visible": {"1"},
	}); code != http.StatusOK || !strings.Contains(body, "修订") {
		t.Fatalf("author update: %d %s", code, body)
	}

	// 草稿：作者看得到，陌生人 404，列表里也没有。
	if _, body := h.postAs(author, "/solutions", url.Values{
		"problem": {"AR1"}, "title": {"草稿思路"}, "body": {"还没想好"}, "visible": {"0"},
	}); !strings.Contains(body, "草稿思路") {
		t.Fatalf("create draft: %s", body)
	}
	draftID := latestArticleID(t, h)
	if code, _ := h.getAs(other, "/solutions/"+itoa64(draftID)); code != http.StatusNotFound {
		t.Fatalf("draft detail for stranger: %d", code)
	}
	if _, body := h.getAs(author, "/solutions/"+itoa64(draftID)); !strings.Contains(body, "草稿") {
		t.Fatalf("author should see draft: %s", body)
	}

	// 专栏：不挂题。
	if code, body := h.postAs(author, "/columns", url.Values{
		"title": {"怎么读题"}, "body": {"先看数据范围。"}, "visible": {"1"},
	}); code != http.StatusOK || !strings.Contains(body, "怎么读题") {
		t.Fatalf("create column: %d %s", code, body)
	}
	columnID := latestArticleID(t, h)
	if code, body := h.getAs(other, "/columns"); code != http.StatusOK ||
		!strings.Contains(body, "怎么读题") || strings.Contains(body, "草稿思路") ||
		!strings.Contains(body, "写一篇专栏") || strings.Contains(body, "写一篇column") {
		t.Fatalf("column list: %d %s", code, body)
	}
	if code, _ := h.getAs(other, "/solutions/"+itoa64(columnID)); code != http.StatusNotFound {
		t.Fatalf("column read through solution URL: %d", code)
	}

	// 审核者能删别人的；作者不能删别人的。
	mod := h.signIn("columnmod", permTopicModerate)
	if code, _ := h.postAs(author, "/solutions/"+itoa64(draftID)+"/delete", url.Values{}); code != http.StatusOK {
		t.Fatalf("author delete own draft: %d", code)
	}
	if code, body := h.postAs(mod, "/solutions/"+itoa64(solutionID)+"/delete", url.Values{}); code != http.StatusOK ||
		!strings.Contains(body, "已删除") {
		t.Fatalf("moderator delete: %d %s", code, body)
	}
}

func latestArticleID(t *testing.T, h *harness) int64 {
	t.Helper()
	var id int64
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT id FROM articles ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func itoa64(n int64) string {
	return strconv.FormatInt(n, 10)
}
