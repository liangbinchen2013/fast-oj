package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

func validLanguageTag(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '+', r == '#', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleProblemList(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := pageLimit(r.URL.Query().Get("limit"))
	search := r.URL.Query().Get("q")
	page, err := s.db.ListProblems(r.Context(), after, limit, search, !a.can(permProblemEditAny))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.problemsPage(w, r, a.viewer(), page, limit, search)
}

func (s *Server) handleProblemGet(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	p, ok := s.problemByKey(w, r)
	if !ok {
		return
	}
	// A problem mounted in a contest is readable by that contest's members even when
	// it is unpublished: mounting was already gated on the right to show it.
	contest := s.contestOfProblem(r, a, p)
	if !p.Visible && !a.canEditProblem(p) && !a.can(permProblemPublish) && contest.ID == 0 {
		s.respondError(w, r, http.StatusNotFound, "problem "+p.PID+" not found")
		return
	}
	s.problemPage(w, r, a, p, contest)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// formatMS keeps every duration in milliseconds: a list that mixes "15ms" with
// "1s" makes the numbers impossible to scan side by side. A lower bound (the run
// was killed before it finished) is printed as ">1050ms" rather than passed off as
// a measurement.
func formatMS(ms int, min bool) string {
	if ms < 0 {
		return "-"
	}
	return prefixMin(strconv.Itoa(ms)+"ms", min)
}

// formatKB shows memory in K below a megabyte and in M above it, always to two
// decimals so 6.73046875M never sprawls across the column.
func formatKB(kb int, min bool) string {
	if kb < 0 {
		return "-"
	}
	if kb < 1024 {
		return prefixMin(strconv.Itoa(kb)+"K", min)
	}
	return prefixMin(strconv.FormatFloat(float64(kb)/1024, 'f', 2, 64)+"M", min)
}

func prefixMin(text string, min bool) string {
	if min {
		return ">" + text
	}
	return text
}

// orDash fills a table cell that would otherwise read as a blank.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, ",")
}

// problemByKey resolves /problems/{key} into a row or writes the error.
func (s *Server) problemByKey(w http.ResponseWriter, r *http.Request) (store.Problem, bool) {
	key := r.PathValue("key")
	p, err := s.db.FindProblem(r.Context(), key)
	if err != nil {
		s.writeStoreError(w, r, err)
		return store.Problem{}, false
	}
	return p, true
}

func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	return s.decodeBodyLimit(w, r, dst, s.cfg.MaxSource)
}

func (s *Server) decodeBodyLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		s.respondError(w, r, http.StatusBadRequest, "unreadable body")
		return false
	}
	if int64(len(body)) > limit {
		s.respondError(w, r, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("body over %d bytes", limit))
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		s.respondError(w, r, http.StatusBadRequest, "empty body")
		return false
	}
	// Reject invalid UTF-8 at the boundary: json.Unmarshal would silently turn
	// each bad byte into U+FFFD and permanently corrupt statements on write.
	if !utf8.Valid(body) {
		s.respondError(w, r, http.StatusBadRequest, "body must be valid UTF-8 (Windows cmd: chcp 65001)")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		s.respondError(w, r, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.respondError(w, r, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		s.respondError(w, r, http.StatusConflict, err.Error())
	default:
		s.log.Error("store", "err", err, "path", r.URL.Path)
		s.respondError(w, r, http.StatusInternalServerError, "database error")
	}
}

// casePair is one zip-sourced test case.
type casePair struct {
	Name          string
	Input, Output []byte
}

// collectCases pairs <stem>.in with <stem>.out or <stem>.ans inside a zip archive.
// Entries are flattened to their base name, so a nested path cannot escape anywhere.
func (s *Server) collectCases(zipBody []byte) ([]casePair, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBody), int64(len(zipBody)))
	if err != nil {
		return nil, fmt.Errorf("not a zip archive: %w", err)
	}
	byName := map[string]*casePair{}
	answerExt := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := path.Base(f.Name)
		ext := strings.ToLower(path.Ext(base))
		stem := strings.TrimSuffix(base, path.Ext(base))
		if stem == "" || (ext != ".in" && ext != ".out" && ext != ".ans") {
			continue
		}
		if f.UncompressedSize64 > uint64(s.cfg.MaxCase) {
			return nil, fmt.Errorf("case %s over %d bytes", base, s.cfg.MaxCase)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(rc, s.cfg.MaxCase+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > s.cfg.MaxCase {
			return nil, fmt.Errorf("case %s over %d bytes", base, s.cfg.MaxCase)
		}
		pair := byName[stem]
		if pair == nil {
			pair = &casePair{Name: stem}
			byName[stem] = pair
		}
		if ext == ".in" {
			pair.Input = body
			continue
		}
		if prev := answerExt[stem]; prev != "" && prev != ext {
			return nil, fmt.Errorf("case %s has both .out and .ans; keep only one answer file", stem)
		}
		answerExt[stem] = ext
		pair.Output = body
	}

	var missing, out []casePair
	for _, pair := range byName {
		if pair.Input == nil || pair.Output == nil {
			missing = append(missing, *pair)
			continue
		}
		out = append(out, *pair)
	}
	if len(missing) > 0 {
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = m.Name
		}
		sort.Strings(names)
		return nil, fmt.Errorf("incomplete pairs (need both .in and .out/.ans): %s", strings.Join(names, ", "))
	}
	// Samples sort first so a sample-only re-run stays cheap.
	sort.Slice(out, func(i, j int) bool {
		si, sj := isSampleName(out[i].Name), isSampleName(out[j].Name)
		if si != sj {
			return si
		}
		return out[i].Name < out[j].Name
	})
	if len(out) == 0 {
		return nil, errors.New("no <name>.in / <name>.out or .ans pairs found")
	}
	return out, nil
}

func isSampleName(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "sample")
}

func (s *Server) handleCaseUpload(w http.ResponseWriter, r *http.Request) {
	a, p, ok := s.requireCaseAccess(w, r, r.PathValue("key"))
	if !ok {
		return
	}
	zipBody, ok := s.readUpload(w, r)
	if !ok {
		return
	}
	pairs, err := s.collectCases(zipBody)
	if err != nil {
		s.backToCases(w, r, p, err.Error())
		return
	}

	existing, err := s.db.ListTestcases(r.Context(), p.ID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	byName := map[string]store.Testcase{}
	for _, tc := range existing {
		byName[tc.Name] = tc
	}
	next := 0
	for _, tc := range existing {
		if tc.Ordinal > next {
			next = tc.Ordinal
		}
	}

	var added, replaced int
	for _, pair := range pairs {
		ordinal := next + 1
		score := 1
		if old, found := byName[pair.Name]; found {
			ordinal = old.Ordinal
			score = old.Score
			replaced++
		} else {
			next = ordinal
			added++
			if isSampleName(pair.Name) {
				score = 0
			}
		}
		relIn, relOut, err := s.cases.Put(p.ID, ordinal, pair.Input, pair.Output)
		if err != nil {
			s.respondError(w, r, http.StatusInternalServerError, "case write failed: "+err.Error())
			return
		}
		_, err = s.db.PutTestcase(r.Context(), &store.Testcase{
			ProblemID: p.ID, Ordinal: ordinal, Name: pair.Name,
			IsSample: isSampleName(pair.Name), Score: score,
			InputRel: relIn, OutputRel: relOut,
		})
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
	}

	total, err := s.db.CountTestcases(r.Context(), p.ID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("cases imported", "pid", p.PID, "added", added, "replaced", replaced, "by", a.User.Handle)
	s.casesDone(w, r, p, fmt.Sprintf("导入完成：新增 %d 条，覆盖 %d 条，共 %d 条", added, replaced, total))
}

// readUpload accepts a raw zip body, or a zip inside a multipart file part.
func (s *Server) readUpload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	defer r.Body.Close()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(s.cfg.MaxUpload); err != nil {
			s.respondError(w, r, http.StatusBadRequest, "bad multipart upload: "+err.Error())
			return nil, false
		}
		file, _, err := r.FormFile("cases")
		if err != nil {
			file, _, err = r.FormFile("file")
		}
		if err != nil {
			s.respondError(w, r, http.StatusBadRequest, "no file part named cases")
			return nil, false
		}
		defer file.Close()
		body, err := io.ReadAll(io.LimitReader(file, s.cfg.MaxUpload+1))
		if err != nil || int64(len(body)) > s.cfg.MaxUpload {
			s.respondError(w, r, http.StatusRequestEntityTooLarge, "upload too large")
			return nil, false
		}
		return body, true
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxUpload+1))
	if err != nil {
		s.respondError(w, r, http.StatusBadRequest, "unreadable body")
		return nil, false
	}
	if int64(len(body)) > s.cfg.MaxUpload {
		s.respondError(w, r, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload over %d bytes", s.cfg.MaxUpload))
		return nil, false
	}
	if len(body) == 0 {
		s.respondError(w, r, http.StatusBadRequest, "empty upload")
		return nil, false
	}
	return body, true
}

// handleCaseBody streams one case file. Sample data is public; judged data needs
// an admin token.
func (s *Server) handleCaseBody(w http.ResponseWriter, r *http.Request) {
	a, _ := s.authenticate(r)
	p, ok := s.problemByKey(w, r)
	if !ok {
		return
	}
	if !p.Visible && !a.canSeeTestcases(p) {
		// 隐藏题的样例也不该成为枚举题目的把手；作者与用例维护者仍能看。
		s.respondError(w, r, http.StatusNotFound, "problem "+p.PID+" not found")
		return
	}
	tc, err := s.db.GetTestcase(r.Context(), p.ID, r.PathValue("name"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if !tc.IsSample && !a.canSeeTestcases(p) {
		s.respondError(w, r, http.StatusForbidden, "case "+tc.Name+" is not a sample")
		return
	}
	rel := tc.OutputRel
	if r.PathValue("which") == "in" {
		rel = tc.InputRel
	}
	body, err := s.cases.Read(rel)
	if err != nil {
		s.respondError(w, r, http.StatusNotFound, "case file missing")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
