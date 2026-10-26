package store

import "strings"

type scanner interface{ Scan(dest ...any) error }

type User struct {
	ID          int64  `json:"id"`
	Handle      string `json:"handle"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Bio         string `json:"bio,omitempty"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	// DMPolicy is who may send this person a message: everyone, mutual (only
	// people they follow back), following (only people they follow) or off.
	DMPolicy    string `json:"dm_policy"`
	HasPassword bool   `json:"has_password"`
	// EmailVerified is whether this address has been confirmed through the link in a
	// mail. Rows written before M25f read as verified (migration 0024 explains why);
	// only a registration made while the switch was on starts out at false.
	EmailVerified bool  `json:"email_verified"`
	CreatedAt     int64 `json:"created_at"`
	LastLoginAt   int64 `json:"last_login_at"`
	// Theme 是这个人自己的配色原文（一串 key=value，见 theme.go）。它跟着每次 authenticate
	// 一起读出来，所以渲染一个人的页面不需要第二条查询；空串是他什么都没调。
	Theme string `json:"-"`
	// Tags 是管理员贴在这个人身上的标签原文（英文逗号分隔，见 tags.go）。它已经在 personSQL
	// 里跟着名字拼好了，这里留着是给 Go 侧自己拼名字的几处用的（关注列表、私信、导航栏）。
	Tags string `json:"tags,omitempty"`
	// Points 是后台按当前参数算出来的活跃度（含衰减），Color 是它的读数（或者管理员锁
	// 上去的 purple/brown）。两列都在 users 行上，所以显示它们不多一次查询。
	// PointsParts 是四个维度的明细原文（ac=..,sub=..,…），个人页用它把"分是怎么来的"摊开。
	Points      int    `json:"points"`
	PointsParts string `json:"-"`
	Color       string `json:"color,omitempty"`
}

// ColorClass 是渲染名字时用的那一档：库里存的空串表示"最低一档"，页面需要一个类名，
// 于是这里补上 blue。除 blue 外的每个词都直接来自库里，而库里只可能是这六个词之一
// （写入侧 ValidColor / RefreshPoints 都拦过），所以它能安全地拼进 class。
func (u User) ColorClass() string { return ColorClass(u.Color) }

// ColorClass 是 ColorClass 的自由函数版本，给只拿到一个字符串（不是整个 User）的视图用。
func ColorClass(color string) string {
	for _, c := range colorTokens {
		if c == color {
			return c
		}
	}
	return "blue"
}

// TagSuffix 是名字后面那一段：` [作弊者] [新生]`，没有标签时是空串。
//
// SQL 那一路在 personSQL 里拼，Go 这一路调它，两边格式必须一样——名字后面的方括号是管理员
// 贴上去的话的可见形状，不该因为走的是哪条查询而变样。
func (u User) TagSuffix() string {
	tags := ParseTags(u.Tags)
	if len(tags) == 0 {
		return ""
	}
	return " [" + strings.Join(tags, "] [") + "]"
}

type Problem struct {
	ID          int64    `json:"id"`
	PID         string   `json:"pid"`
	Title       string   `json:"title"`
	Statement   string   `json:"statement,omitempty"`
	TimeLimitMS int      `json:"time_limit_ms"`
	MemLimitKB  int      `json:"mem_limit_kb"`
	Languages   []string `json:"languages,omitempty"`
	Visible     bool     `json:"visible"`
	// DetailLevel is how much of the judging detail the submitter gets to see:
	// none, case, diff or full. The author and the operator always see everything.
	DetailLevel string `json:"detail_level"`
	OwnerID     int64  `json:"owner_id"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type Testcase struct {
	ID        int64  `json:"id"`
	ProblemID int64  `json:"problem_id"`
	Ordinal   int    `json:"ordinal"`
	Name      string `json:"name"`
	IsSample  bool   `json:"is_sample"`
	Score     int    `json:"score"`
	// GroupName bundles cases: the whole group pays only when every case in it
	// passes. Empty means the case pays on its own.
	GroupName string `json:"group_name"`
	InputRel  string `json:"input_rel"`
	OutputRel string `json:"output_rel"`
}

type Submission struct {
	ID        int64 `json:"id"`
	UserID    int64 `json:"user_id"`
	ProblemID int64 `json:"problem_id"`
	// ContestID is the contest this submission was made inside, 0 for practice. It
	// is fixed when the row is created: leaving a team or unmounting a problem does
	// not unwind it.
	ContestID int64  `json:"contest_id"`
	Language  string `json:"language"`
	// Options is what this submission asked to be built with, in the judge's own
	// allowlist notation ("std=c++14,O=0"). Empty means the box's default, which is
	// the same command line every pre-M28 submission used.
	Options string `json:"options,omitempty"`
	// Toolchain 是判机回报的"我这次实际用的口径"。空 = 这台判机没回报（老二进制），
	// 页面要写「未回报」而不是替它说一句。
	Toolchain string `json:"toolchain,omitempty"`
	Source    string `json:"source,omitempty"`
	Status    string `json:"status"`
	Verdict   string `json:"verdict"`
	TimeMS    int    `json:"time_ms"`
	MemKB     int    `json:"mem_kb"`
	// TimeMin/MemMin: the headline numbers came from a run that was cut off, so
	// they are lower bounds and the views prefix them with ">".
	TimeMin    bool   `json:"time_min,omitempty"`
	MemMin     bool   `json:"mem_min,omitempty"`
	Score      int    `json:"score"`
	TotalScore int    `json:"total_score"`
	FailedCase string `json:"failed_case"`
	Detail     string `json:"detail,omitempty"`
	JudgeID    int64  `json:"judge_id"`
	CreatedAt  int64  `json:"created_at"`
	StartedAt  int64  `json:"started_at"`
	JudgedAt   int64  `json:"judged_at"`
}

type Run struct {
	ID           int64  `json:"id"`
	SubmissionID int64  `json:"submission_id"`
	Ordinal      int    `json:"ordinal"`
	CaseName     string `json:"case_name"`
	Verdict      string `json:"verdict"`
	// Score is what this row actually earned, which is not the case weight when a
	// bundle was broken by another case in the same group.
	Score    int  `json:"score"`
	TimeMS   int  `json:"time_ms"`
	MemKB    int  `json:"mem_kb"`
	IsSample bool `json:"is_sample"`
	// TimeMin/MemMin mark lower bounds: the run was cut off, so the real cost was
	// at least this much. The views print a leading ">" for them.
	TimeMin bool `json:"time_min,omitempty"`
	MemMin  bool `json:"mem_min,omitempty"`
}

type Judge struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Languages []string `json:"languages,omitempty"`
	// Capabilities 是这台判机自报的"我这版代码真会做什么"：options / checker / interactor。
	// 老判机不送这一列，于是它是空的，服务器就不会把需要新能力的工单派给它。
	Capabilities []string `json:"capabilities,omitempty"`
	// Jailed 是这台判机心跳里报上来的"此刻会不会把一次运行关进牢房"。JailedKnown 分清
	// 了"没说"与"说了没有"：老二进制不会报这一项，那种情况不能显示成"确认没有隔离"。
	Jailed      bool   `json:"jailed"`
	JailedKnown bool   `json:"jailed_known"`
	Note        string `json:"note"`
	LastSeenAt  int64  `json:"last_seen_at"`
}

// Has 报告自报能力里有没有这一项。词表在 internal/protocol（那是两端之间的一句话），
// 这里只按字符串查。
func (j Judge) Has(cap string) bool {
	for _, c := range j.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func joinList(items []string) string { return strings.Join(items, ",") }

func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	out := make([]string, 0, strings.Count(raw, ",")+1)
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
