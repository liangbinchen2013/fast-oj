package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// maxInlineCase bounds what the browser editor will load into a textarea. Beyond
// it the case is still listed and can be replaced by an archive import, but the
// page refuses to show half a file that saving would silently truncate.
const maxInlineCase = 256 << 10

type caseRow struct {
	ID      int64
	Ordinal int
	Name    string
	Kind    string
	Group   string
	Score   int
	// IsSample drives the batch grid's checkbox; Kind is what the read-only table shows.
	IsSample        bool
	InSize, OutSize string
	Missing         bool
}

type casesData struct {
	PID, Title string
	Rows       []caseRow
	Groups     []groupRow
	Judged     int
	Total      int
	// Programs 是用例页下面"判题方式"那几块的数据：判题器与交互器各一份，共用一个表单。
	Programs []programEditor
	// Mode 是这一页顶部那句"本题怎么判"，包括两个都配了时判机会拒判的提醒。
	Mode string
}

// programEditor 是一份判机自带程序在出题页上的样子。
type programEditor struct {
	Kind, KindName string
	Language       string
	Source         string
	// Languages 是判机真能编的语言；候选来自判机的工具表，不是这里现编的列表。
	Languages []string
	// CanEdit 为假时页面只读，并说明为什么：让出题人看到"这儿本来是能填的，是你没这项
	// 权力"，比把整块藏起来让他以为这题不支持 SPJ 好。
	CanEdit bool
	// Hint 是输入框里的样子，把 argv 顺序与退出码约定写在离手最近的地方。
	Hint string
}

// modeBadge 是题面页上那一行短到能一眼看完的判定说明。空串表示普通比较，不用打扰学生。
func modeBadge(programs map[string]store.JudgeProgram) string {
	checker := strings.TrimSpace(programs[store.ProgramChecker].Source) != ""
	interactor := strings.TrimSpace(programs[store.ProgramInteractor].Source) != ""
	switch {
	case checker && interactor:
		// 学生这一眼只需要知道"这题现在交上去也判不了"，改掉它是出题人的事，
		// 不该把那句指令写在一堵学生看的页面上。
		return "本题的判题配置冲突（判题器与交互器同时存在），现在判不了题：" +
			"提交会判成判机故障，等出题人修正后再交。"
	case interactor:
		return "交互题：你的程序与判机端的交互器一问一答，判定与评语都由交互器给出；" +
			"用时按两侧 CPU 之和算，内存只算你这一份。"
	case checker:
		return "SPJ 判题器判定：输出与答案文件不一样也可能是对的，判题器的话会写在详情里。"
	}
	return ""
}

// programLanguages 是判机能编的判题器语言。候选直接来自判机的工具表，与提交页那套同源：
// 判题器只能用**判机真有的**工具链编，写一个学生提交都不一定能用的语言更没有意义。
func programLanguages() []string {
	out := make([]string, 0, len(versionTools))
	for name := range versionTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// groupRow summarises one bundle: how many cases it holds and what the group is
// worth when all of them pass.
type groupRow struct {
	Name   string
	Cases  int
	Points int
}

// caseEdit is one case opened for editing.
type caseEdit struct {
	ID              int64
	Name            string
	Group           string
	Score           int
	IsSample        bool
	Input, Output   string
	Inline          bool
	InSize, OutSize string
	Ordinal         int
}

// loadCases reads the rows plus the two sizes each row needs, in one place so the
// list page and the edit page cannot disagree about what a case costs.
func (s *Server) loadCases(r *http.Request, p store.Problem) ([]store.Testcase, []caseRow, error) {
	cases, err := s.db.ListTestcases(r.Context(), p.ID, false)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]caseRow, 0, len(cases))
	for _, tc := range cases {
		in, errIn := s.cases.Size(tc.InputRel)
		out, errOut := s.cases.Size(tc.OutputRel)
		kind := "判题"
		if tc.IsSample {
			kind = "样例"
		}
		rows = append(rows, caseRow{
			ID: tc.ID, Ordinal: tc.Ordinal, Name: tc.Name, Score: tc.Score, Group: tc.GroupName,
			Kind:     kind,
			IsSample: tc.IsSample,
			InSize:   formatBytes(in, errIn),
			OutSize:  formatBytes(out, errOut),
			Missing:  errIn != nil || errOut != nil,
		})
	}
	return cases, rows, nil
}

func formatBytes(n int64, err error) string {
	if err != nil {
		return "缺文件"
	}
	switch {
	// GB 那一档是给磁盘那几行用的：一个 20 GB 的数据目录写成 "20480.00 MB" 就得让人
	// 心算一次除法，而这一页的存在理由正是"一眼看出该清哪一块"。
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 2, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 2, 64) + " MB"
	case n >= 1024:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

func (s *Server) handleCasesPage(w http.ResponseWriter, r *http.Request) {
	a, p, ok := s.requireCaseAccess(w, r, r.PathValue("key"))
	if !ok {
		return
	}
	_, rows, err := s.loadCases(r, p)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.renderCases(w, r, a, p, rows, msgOf(r))
}

// renderCases draws the case page from whatever rows the caller has: the ones in the
// database, or the ones a batch form is about to save. The preview and the saved
// view must be the same table, or "核对一眼再保存" is a lie.
func (s *Server) renderCases(w http.ResponseWriter, r *http.Request, a actor, p store.Problem, rows []caseRow, msg string) {
	judged, total := 0, 0
	// Groups keep the order their cases are declared in, so the summary does not
	// reshuffle between two visits to the same page.
	var groups []groupRow
	index := map[string]int{}
	for _, row := range rows {
		if row.IsSample {
			continue
		}
		judged++
		total += row.Score
		if row.Group == "" {
			continue
		}
		at, seen := index[row.Group]
		if !seen {
			at = len(groups)
			index[row.Group] = at
			groups = append(groups, groupRow{Name: row.Group})
		}
		groups[at].Cases++
		groups[at].Points += row.Score
	}
	code := http.StatusOK
	if strings.HasPrefix(msg, "批量修改没有保存") {
		code = http.StatusBadRequest
	}
	// 判题器与交互器跟着用例页走：它们判的就是这批文件，出题人改用例时顺手就该看见现在用的是谁。
	programs, err := s.db.JudgePrograms(r.Context(), p.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	editorFor := func(kind, kindName, hint string) programEditor {
		pg := programs[kind]
		return programEditor{
			Kind: kind, KindName: kindName, Language: pg.Language, Source: pg.Source,
			Languages: programLanguages(), CanEdit: a.can(permJudgeProgram), Hint: hint,
		}
	}
	s.page(w, r, code, "用例 "+p.PID, "problem_cases", a.viewer(), msg, casesData{
		PID: p.PID, Title: p.Title, Rows: rows, Groups: groups, Judged: judged, Total: total,
		Programs: []programEditor{
			editorFor(store.ProgramChecker, "判题器", checkerHint),
			editorFor(store.ProgramInteractor, "交互器", interactorHint),
		},
		Mode: modeLine(programs),
	})
}

// modeLine 是出题页与题面页上那句"这题怎么判"。它必须与判机那边的实现一字不差地对得上：
// 出题人是照这句话写判题器与交互器的，说错了就是让他照一个不存在的约定写。
func modeLine(programs map[string]store.JudgeProgram) string {
	checker := strings.TrimSpace(programs[store.ProgramChecker].Source) != ""
	interactor := strings.TrimSpace(programs[store.ProgramInteractor].Source) != ""
	switch {
	case checker && interactor:
		return "同时配了判题器与交互器：这种题目判机会直接拒绝判题，两者只能留一个。" +
			"交互题由交互器判定，判题器不会被跑到。"
	case interactor:
		return "本题是交互题：选手的程序与交互器用标准输入输出对话，判定由交互器的退出码给出" +
			"（0 算正确、1 算错误、7 算选手程序出错，其他算判机故障），它打印的内容当作评语。" +
			"交互器 argv 依次是输入文件、选手输出转写、答案文件；那份转写是对话结束后才写进去的，" +
			"运行时去读只会读到空文件，要跟选手说话就走标准输入输出。" +
			"用时按两侧 CPU 之和算，内存只算选手那一份。" +
			"题面要写清问几轮、每轮什么格式：学生看不到交互器，只能照题面对话。"
	case checker:
		return "本题由 SPJ 判题器判定：不拿标准输出与答案文件逐字节比，而是由那份程序说了算。" +
			"判题器 argv 依次是输入文件、选手输出文件、答案文件，退出码" +
			"0 算正确、1 算错误、其他算判机故障；它打印的内容当作评语。" +
			"答案不唯一，所以判机不再给「第几行你输出什么、答案是什么」那种逐行对照，" +
			"学生能看到的只有判题器打印的那一句。"
	default:
		return "本题按标准输出与答案宽松比较判定（忽略行尾空白与末尾空行）。" +
			"答案不唯一、要判精度或特判格式时，在下面贴一份判题器就换成 SPJ；要一问一答的题就贴交互器。"
	}
}

// 约定写在离手最近的地方：这两句话与判机那边的实现（internal/judge 的 checkWithSPJ 与
// RunDuo）必须一字不差地对得上，出题人是照这里写的程序。
const (
	checkerHint    = "argv：输入文件 选手输出文件 答案文件；退出码 0=对 1=错 其他=判机故障；stdout 当评语"
	interactorHint = "argv：输入文件 选手输出转写 答案文件；退出码 0=对 1=错 7=选手程序出错 其他=判机故障；与选手用 stdin/stdout 对话"
)

// bothProgramsWarn 是"两个都配了"的那句提醒。判机遇到这种题目会直接拒绝判题（它不能悄悄
// 挑一个来用），所以与其等学生撞上一条判机故障，不如出题人在保存时就看见。
func bothProgramsWarn(checker, interactor store.JudgeProgram) string {
	if strings.TrimSpace(checker.Source) == "" || strings.TrimSpace(interactor.Source) == "" {
		return ""
	}
	return "判题器与交互器同时配了：这种题目判机会直接拒绝判题，两者只能留一个。"
}

// handleJudgeProgram 保存题目自带的判题器。
//
// 这里**不**当场编译验证：判题器是判机用自己的工具链编的，Web 进程所在机器上有没有 g++
// 与判定无关，替判机说"这份能编"才是假保证。所以编不过的判题器要等到学生提交才暴露——
// 那条工单会被判成判机故障并写明是这个判题器的问题，学生的代码不背这个锅，但表单下方的
// 提示要把这件事说在前面，让出题人自己先检查。
func (s *Server) handleJudgeProgram(w http.ResponseWriter, r *http.Request) {
	a, p, ok := s.requireJudgeProgram(w, r, r.PathValue("key"))
	if !ok {
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	if !store.ValidProgramKind(kind) {
		s.backToCases(w, r, p, "不认识的判机程序种类："+kind)
		return
	}
	who := programKindName(kind)
	lang := strings.ToLower(strings.TrimSpace(r.FormValue("language")))
	if lang == "" {
		lang = "cpp"
	}
	if _, ok := versionTool(lang); !ok {
		s.backToCases(w, r, p, who+"只能用判机认识的语言写："+strings.Join(programLanguages(), "/"))
		return
	}
	src := r.FormValue("source")
	if len(src) > store.MaxProgramSource {
		s.backToCases(w, r, p, who+"源码太长，上限 "+strconv.Itoa(store.MaxProgramSource)+" 字节")
		return
	}
	if strings.ContainsRune(src, 0) {
		s.backToCases(w, r, p, "源码里不能有空字节")
		return
	}
	if err := s.db.PutJudgeProgram(r.Context(), store.JudgeProgram{
		ProblemID: p.ID, Kind: kind, Language: lang, Source: src,
	}); err != nil {
		s.backToCases(w, r, p, "没保存上："+err.Error())
		return
	}
	// 撤掉一份之后是谁在判，得看剩下那一份还是不是空：两份都配了时撤掉一份不是回到逐字节
	// 比较，而是换成另一份判定。说错这一句，出题人就照错的约定去改题面了。
	programs, err := s.db.JudgePrograms(r.Context(), p.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	left := programs[otherProgramKind(kind)]
	switch {
	case strings.TrimSpace(src) == "":
		s.log.Info("judge program removed", "pid", p.PID, "kind", kind, "by", a.User.Handle)
		// 撤掉一份判机程序等于把这道题的判定方式换回去：它改变的是所有后来的提交怎么被
		// 判对，所以跟"存了一份"同样是审计材料。detail 里写清撤掉之后是谁在判——
		// 两份都在时撤掉一份不是回到逐字节比较，而是换成另一份。
		leftNote := "判定回到标准输出与答案宽松比"
		if strings.TrimSpace(left.Source) != "" {
			leftNote = "这题改由" + programKindName(otherProgramKind(kind)) + "判定"
		}
		s.audit(r, a, "judge_program.remove", p.PID, kind+" 已撤掉，"+leftNote)
		if strings.TrimSpace(left.Source) != "" {
			s.backToCases(w, r, p, "已撤掉"+who+"：这题现在由"+
				programKindName(otherProgramKind(kind))+"判定。")
		} else {
			s.backToCases(w, r, p, "已撤掉"+who+"：这道题回到「标准输出与答案宽松比」。")
		}
	default:
		s.log.Info("judge program saved", "pid", p.PID, "kind", kind, "bytes", len(src), "by", a.User.Handle)
		s.audit(r, a, "judge_program.save", p.PID,
			fmt.Sprintf("%s（%s）已保存，%d 字节，下一次判题起生效", kind, lang, len(src)))
		msg := who + "已存好，下一次判题起生效（判机现场编译）。"
		if warn := bothProgramsWarn(programs[store.ProgramChecker],
			programs[store.ProgramInteractor]); warn != "" {
			msg += warn
		}
		s.backToCases(w, r, p, msg)
	}
}

// otherProgramKind 是另一种判机程序：判题器与交互器只有这两个种类，互为对方。
func otherProgramKind(kind string) string {
	if kind == store.ProgramChecker {
		return store.ProgramInteractor
	}
	return store.ProgramChecker
}

// programKindName 是权限、提示语里给这两种程序起的人类名字，与判机那边一致。
func programKindName(kind string) string {
	if kind == store.ProgramInteractor {
		return "交互器"
	}
	return "判题器"
}

// caseForm is one case as the editor posts it.
type caseForm struct {
	Name, Group   string
	Score         int
	IsSample      bool
	Input, Output string
	HasContent    bool
}

// parseCaseForm reads the case editor. A sample carries no points by definition,
// so the score field is ignored rather than left for the author to notice.
func parseCaseForm(r *http.Request) (caseForm, string) {
	f := caseForm{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Group:    strings.TrimSpace(r.FormValue("group")),
		IsSample: r.FormValue("sample") == "1",
	}
	if score, err := strconv.Atoi(strings.TrimSpace(r.FormValue("score"))); err != nil {
		return f, "分值要填整数"
	} else if score < 0 || score > 10000 {
		return f, "分值要 0..10000"
	} else {
		f.Score = score
	}
	if f.Name == "" || len(f.Name) > 64 || strings.ContainsAny(f.Name, "/\\ \t") || f.Name == "." || f.Name == ".." {
		return f, "用例名要 1..64 个字符，不能含空格或斜杠"
	}
	f.Input, f.Output = r.FormValue("input"), r.FormValue("output")
	// The textareas are only rendered for small files, so the fields being absent
	// is how "keep the bytes that are already on disk" arrives.
	_, hasIn := r.Form["input"]
	_, hasOut := r.Form["output"]
	f.HasContent = hasIn || hasOut
	if len(f.Group) > 40 || strings.ContainsAny(f.Group, "/\\") {
		return f, "组名最多 40 个字符，不能含斜杠"
	}
	if len(f.Input) > maxInlineCase || len(f.Output) > maxInlineCase {
		return f, "这一条太大，网页里改不了；用 zip 导入同名覆盖"
	}
	if f.IsSample {
		f.Score = 0
	}
	return f, ""
}

func (s *Server) handleCaseCreate(w http.ResponseWriter, r *http.Request) {
	a, p, ok := s.requireCaseAccess(w, r, r.PathValue("key"))
	if !ok {
		return
	}
	f, bad := parseCaseForm(r)
	if bad != "" {
		s.backToCases(w, r, p, bad)
		return
	}
	if !f.HasContent {
		s.backToCases(w, r, p, "输入与输出都要写点什么")
		return
	}
	cases, err := s.db.ListTestcases(r.Context(), p.ID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if _, dup := findCase(cases, f.Name); dup {
		s.backToCases(w, r, p, "已经有同名用例："+f.Name)
		return
	}
	ordinal := 0
	for _, tc := range cases {
		if tc.Ordinal > ordinal {
			ordinal = tc.Ordinal
		}
	}
	ordinal++
	relIn, relOut, err := s.cases.Put(p.ID, ordinal, []byte(f.Input), []byte(f.Output))
	if err != nil {
		s.backToCases(w, r, p, "写入失败："+err.Error())
		return
	}
	if _, err := s.db.AddTestcase(r.Context(), &store.Testcase{
		ProblemID: p.ID, Ordinal: ordinal, Name: f.Name, IsSample: f.IsSample,
		Score: f.Score, GroupName: f.Group, InputRel: relIn, OutputRel: relOut,
	}); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("case added", "pid", p.PID, "name", f.Name, "by", a.User.Handle)
	s.casesDone(w, r, p, "已加一条用例 "+f.Name)
}

func findCase(cases []store.Testcase, name string) (store.Testcase, bool) {
	for _, tc := range cases {
		if tc.Name == name {
			return tc, true
		}
	}
	return store.Testcase{}, false
}

// caseForEdit resolves /problems/{key}/cases/{id} against the problem it belongs
// to, so an id from another problem cannot be edited through this URL.
func (s *Server) caseForEdit(w http.ResponseWriter, r *http.Request) (actor, store.Problem, store.Testcase, bool) {
	a, p, ok := s.requireCaseAccess(w, r, r.PathValue("key"))
	if !ok {
		return a, p, store.Testcase{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.backToCases(w, r, p, "用例编号要是数字")
		return a, p, store.Testcase{}, false
	}
	tc, err := s.db.GetTestcaseByID(r.Context(), id)
	if err != nil || tc.ProblemID != p.ID {
		// An id from another problem must not be reachable through this URL.
		s.backToCases(w, r, p, "没有这条用例，或者它不属于这道题")
		return a, p, tc, false
	}
	return a, p, tc, true
}

func (s *Server) handleCaseEditPage(w http.ResponseWriter, r *http.Request) {
	a, p, tc, ok := s.caseForEdit(w, r)
	if !ok {
		return
	}
	in, errIn := s.cases.Read(tc.InputRel)
	out, errOut := s.cases.Read(tc.OutputRel)
	edit := caseEdit{
		ID: tc.ID, Name: tc.Name, Group: tc.GroupName, Score: tc.Score, IsSample: tc.IsSample, Ordinal: tc.Ordinal,
		InSize:  formatBytes(int64(len(in)), errIn),
		OutSize: formatBytes(int64(len(out)), errOut),
		Inline:  errIn == nil && errOut == nil && len(in) <= maxInlineCase && len(out) <= maxInlineCase,
	}
	if edit.Inline {
		edit.Input, edit.Output = string(in), string(out)
	}
	s.page(w, r, http.StatusOK, "改用例 "+tc.Name, "case_edit", a.viewer(), msgOf(r), map[string]any{
		"PID": p.PID, "Title": p.Title, "Edit": edit,
	})
}

func (s *Server) handleCaseSave(w http.ResponseWriter, r *http.Request) {
	a, p, tc, ok := s.caseForEdit(w, r)
	if !ok {
		return
	}
	f, bad := parseCaseForm(r)
	if bad != "" {
		s.backToCases(w, r, p, bad)
		return
	}
	next := tc
	next.Name, next.Score, next.IsSample, next.GroupName = f.Name, f.Score, f.IsSample, f.Group
	if f.HasContent {
		relIn, relOut, err := s.cases.Put(p.ID, tc.Ordinal, []byte(f.Input), []byte(f.Output))
		if err != nil {
			s.backToCases(w, r, p, "写入失败："+err.Error())
			return
		}
		next.InputRel, next.OutputRel = relIn, relOut
	}
	if _, err := s.db.UpdateTestcase(r.Context(), &next); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.backToCases(w, r, p, "已经有同名用例："+f.Name)
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("case updated", "pid", p.PID, "name", f.Name, "by", a.User.Handle)
	s.casesDone(w, r, p, "已保存用例 "+f.Name)
}

func (s *Server) handleCaseDelete(w http.ResponseWriter, r *http.Request) {
	a, p, tc, ok := s.caseForEdit(w, r)
	if !ok {
		return
	}
	if _, err := s.db.DeleteTestcase(r.Context(), tc.ID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	// The files are shared by ordinal, so they only go away once no remaining row
	// points at them.
	rest, err := s.db.ListTestcases(r.Context(), p.ID, false)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	for _, rel := range []string{tc.InputRel, tc.OutputRel} {
		shared := false
		for _, other := range rest {
			if other.InputRel == rel || other.OutputRel == rel {
				shared = true
			}
		}
		if !shared {
			if err := s.cases.Remove(rel); err != nil {
				s.log.Error("case file after delete", "pid", p.PID, "rel", rel, "err", err)
			}
		}
	}
	s.log.Info("case deleted", "pid", p.PID, "name", tc.Name, "by", a.User.Handle)
	s.audit(r, a, "case.delete", p.PID, "用例 "+tc.Name+" 已删除（记录与文件一起）")
	s.casesDone(w, r, p, "已删除用例 "+tc.Name)
}

// handleCaseWipe drops every case of a problem, rows and files together. It is a
// POST with its own button because the alternative is a stray link prefetch
// deleting an author's test set.
func (s *Server) handleCaseWipe(w http.ResponseWriter, r *http.Request) {
	a, p, ok := s.requireCaseAccess(w, r, r.PathValue("key"))
	if !ok {
		return
	}
	n, err := s.db.DeleteTestcases(r.Context(), p.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	// 记在删文件之前：数据行这时候已经没了，接着哪怕文件系统出错，这一条也得留下——
	// "谁把这道题的用例清空了"正是要问的那个问题。
	s.audit(r, a, "cases.wipe", p.PID, fmt.Sprintf("清空用例 %d 条（记录与文件）", n))
	if err := s.cases.RemoveProblem(p.ID); err != nil {
		s.log.Error("case files after wipe", "pid", p.PID, "err", err)
		s.casesDone(w, r, p, fmt.Sprintf("清掉了 %d 条记录，但用例文件没删掉：%s", n, err))
		return
	}
	s.log.Info("cases wiped", "pid", p.PID, "removed", n, "by", a.User.Handle)
	s.casesDone(w, r, p, fmt.Sprintf("已清空用例（%d 条）", n))
}

// casesDone is the one redirect every case write ends with, so the flash always
// lands on the page the author was working on.
func (s *Server) casesDone(w http.ResponseWriter, r *http.Request, p store.Problem, msg string) {
	http.Redirect(w, r, "/problems/"+url.PathEscape(p.PID)+"/cases?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// backToCases is the rejected-write half of casesDone.
func (s *Server) backToCases(w http.ResponseWriter, r *http.Request, p store.Problem, msg string) {
	s.casesDone(w, r, p, msg)
}

// handleCasesBatch 一次改一整批用例的分值与捆绑。
//
// 要改 20 个测试点还是一条一条点开改，是折磨出题人。这里唯一不能省的是"以库里的
// 用例为准"：行 id 不从浏览器收，浏览器只能改它被允许看到的那道题名下的行，
// 而且整批一个事务——半张分值表比慢一点改完危险得多。
func (s *Server) handleCasesBatch(w http.ResponseWriter, r *http.Request) {
	a, p, ok := s.requireCaseAccess(w, r, r.PathValue("key"))
	if !ok {
		return
	}
	_, rows, err := s.loadCases(r, p)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	var errs []string
	for i := range rows {
		row := &rows[i]
		field := func(suffix string) string {
			return r.FormValue(suffix + "_" + strconv.FormatInt(row.ID, 10))
		}
		raw := strings.TrimSpace(field("score"))
		if raw == "" {
			raw = "0"
		}
		score, err := strconv.Atoi(raw)
		if err != nil || score < 0 || score > 10000 {
			errs = append(errs, row.Name+"：分值要 0..10000 的整数")
			continue
		}
		group := strings.TrimSpace(field("group"))
		if len(group) > 40 || strings.ContainsAny(group, "/\\") {
			errs = append(errs, row.Name+"：组名最多 40 个字符，不能含斜杠")
			continue
		}
		row.IsSample = field("sample") == "1"
		if row.IsSample {
			score = 0 // 样例不计分，否则总分读起来是假的
		}
		row.Score, row.Group = score, group
	}
	if len(errs) > 0 {
		s.renderCases(w, r, a, p, rows, "批量修改没有保存："+strings.Join(errs, "；"))
		return
	}
	// 两个助手不直接落库：它们只是把表填好，让人核对一眼再点保存。
	switch r.FormValue("apply") {
	case "split":
		splitScores(rows, r.FormValue("split_total"))
		s.renderCases(w, r, a, p, rows, "已按平分填好，核对后点「保存整批」")
		return
	case "group":
		name := strings.TrimSpace(r.FormValue("set_group"))
		for i := range rows {
			if !rows[i].IsSample {
				rows[i].Group = name
			}
		}
		s.renderCases(w, r, a, p, rows, "已把判题用例的组名填成同一个，核对后点「保存整批」")
		return
	}
	meta := make([]store.TestcaseMeta, 0, len(rows))
	for _, row := range rows {
		meta = append(meta, store.TestcaseMeta{ID: row.ID, Score: row.Score, GroupName: row.Group, IsSample: row.IsSample})
	}
	if err := s.db.UpdateTestcaseMeta(r.Context(), p.ID, meta); err != nil {
		s.renderCases(w, r, a, p, rows, "批量修改没有保存："+err.Error())
		return
	}
	s.log.Info("cases batch-updated", "pid", p.PID, "rows", len(meta), "by", a.User.Handle)
	total, err := s.db.ProblemScoreTotal(r.Context(), p.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.backToCases(w, r, p, fmt.Sprintf("已保存 %d 条用例，判题总分 %d", len(meta), total))
}

// splitScores 把总分摊到判题用例上：除不尽的余数摊给前面几条，这样 100 分 3 条是
// 34/33/33，而不是最后一条莫名其妙少了 1 分没人察觉。
func splitScores(rows []caseRow, rawTotal string) {
	n := 0
	for _, row := range rows {
		if !row.IsSample {
			n++
		}
	}
	if n == 0 {
		return
	}
	total, err := strconv.Atoi(strings.TrimSpace(rawTotal))
	if err != nil || total <= 0 {
		total = 100
	}
	base, rem := total/n, total%n
	i := 0
	for k := range rows {
		if rows[k].IsSample {
			continue
		}
		rows[k].Score = base
		if i < rem {
			rows[k].Score++
		}
		i++
	}
}
