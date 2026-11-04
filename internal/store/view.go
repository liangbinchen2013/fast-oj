package store

import (
	"context"
)

// SubmissionDetail joins the human-facing keys onto a submission so the page does
// not need a second query per row.
type SubmissionDetail struct {
	Submission
	PID    string `json:"pid"`
	Title  string `json:"title"`
	Handle string `json:"handle"`
	// Name is who the row should *say*, which for a cancelled account is not its
	// handle: that string may already belong to someone else. Handle still feeds
	// the profile link, because the tombstone page is where the records live.
	Name string `json:"name"`
	// Color 是这个名字的活跃度档位（见 feed.go 里同名的理由）。
	Color string `json:"color,omitempty"`
	// DetailLevel and ProblemOwner come from the problem row: they decide how much
	// of this submission's judging detail the reader is shown.
	DetailLevel  string `json:"detail_level"`
	ProblemOwner int64  `json:"problem_owner"`
	// 可见性来自 problem/contest 两张表：公开列表与详情页用它们判断“这条结果现在
	// 能不能给这个读者看”。隐藏题、进行中的 OI、封榜的比赛都不能只靠页面自己记得过滤。
	ProblemVisible  bool   `json:"problem_visible"`
	ContestMode     string `json:"contest_mode,omitempty"`
	ContestEndAt    int64  `json:"contest_end_at,omitempty"`
	ContestStanding bool   `json:"contest_standing,omitempty"`
	ContestOwnerID  int64  `json:"contest_owner_id,omitempty"`
	// ContestCID is empty when the contest was deleted, which is how the page can
	// say 比赛已删除 instead of linking at nothing.
	ContestCID string `json:"contest_cid"`
}

var detailColumns = `s.id, s.user_id, s.problem_id, IFNULL(s.contest_id, 0), s.language, s.options, s.status, s.verdict,
		s.time_ms, s.mem_kb, s.time_min, s.mem_min, s.score, s.total_score, s.failed_case,
		IFNULL(s.judge_id, 0),
		s.created_at, IFNULL(s.started_at, 0), IFNULL(s.judged_at, 0), s.toolchain,
		p.pid, p.title, u.handle, ` + personSQL(`u`) + `, ` + colorSQL(`u`) + `,
		IFNULL(p.detail_level, 'diff'), IFNULL(p.owner_id, 0), IFNULL(ct.cid, ''),
		IFNULL(p.visible, 0), IFNULL(ct.mode, ''), IFNULL(ct.end_at, 0),
		IFNULL(ct.standing_open, 0), IFNULL(ct.owner_id, 0)`

func scanDetail(row scanner) (SubmissionDetail, error) {
	var (
		d               SubmissionDetail
		judgeID         int64
		timeMin, memMin int
		visible         int
		standing        int
	)
	err := row.Scan(&d.ID, &d.UserID, &d.ProblemID, &d.ContestID, &d.Language, &d.Options, &d.Status, &d.Verdict,
		&d.TimeMS, &d.MemKB, &timeMin, &memMin, &d.Score, &d.TotalScore, &d.FailedCase, &judgeID,
		&d.CreatedAt, &d.StartedAt, &d.JudgedAt, &d.Toolchain, &d.PID, &d.Title, &d.Handle, &d.Name, &d.Color,
		&d.DetailLevel, &d.ProblemOwner, &d.ContestCID, &visible, &d.ContestMode, &d.ContestEndAt,
		&standing, &d.ContestOwnerID)
	d.JudgeID = judgeID
	d.TimeMin, d.MemMin = timeMin != 0, memMin != 0
	d.ProblemVisible = visible != 0
	d.ContestStanding = standing != 0
	return d, notFound(err)
}

const detailFrom = ` FROM submissions s
	JOIN problems p ON p.id = s.problem_id
	JOIN users u ON u.id = s.user_id
	LEFT JOIN contests ct ON ct.id = s.contest_id`

func (db *DB) SubmissionDetail(ctx context.Context, id int64) (SubmissionDetail, error) {
	return scanDetail(db.SQL.QueryRowContext(ctx,
		`SELECT `+detailColumns+detailFrom+` WHERE s.id=?`, id))
}

type DetailFilter struct {
	UserID    int64
	ProblemID int64
	// Status 按 submissions.status 筛。值是绑定参数，不是拼进 SQL 的：认不出来的状态
	// 只会得到空列表，不会意外放宽成"看全部"。管理员拿它找那些停在队列里或判机超时的人。
	Status  string
	AfterID int64
	Limit   int

	// PublicView 打开"按读者可见性过滤"那一条路。零值保留原来的原始列表语义，
	// 给测试与内部管理路径用；所有面向浏览器的入口都必须把它打开。
	PublicView bool
	// ViewerID 是当前读者，0 表示匿名。SeeAll 是 submission.view.any / problem.edit.any
	// 一类的全见权限；置真时不再过滤。
	ViewerID int64
	SeeAll   bool
}

func (db *DB) ListSubmissionDetails(ctx context.Context, f DetailFilter) ([]SubmissionDetail, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	q := `SELECT ` + detailColumns + detailFrom + ` WHERE 1=1`
	args := []any{}
	if f.UserID != 0 {
		q += ` AND s.user_id=?`
		args = append(args, f.UserID)
	}
	if f.ProblemID != 0 {
		q += ` AND s.problem_id=?`
		args = append(args, f.ProblemID)
	}
	if f.Status != "" {
		q += ` AND s.status=?`
		args = append(args, f.Status)
	}
	if f.AfterID != 0 {
		q += ` AND s.id<?`
		args = append(args, f.AfterID)
	}
	if f.PublicView && !f.SeeAll {
		// 隐藏题：只给作者、题目主人和提交者自己看；匿名读者只看公开题。
		if f.ViewerID != 0 {
			q += ` AND (p.visible=1 OR p.owner_id=? OR s.user_id=?)`
			args = append(args, f.ViewerID, f.ViewerID)
			// 比赛里的结果：自己、题主、主办方、已结束、或榜单公开的非 OI 比赛可以看；
			// 进行中的 OI 和封榜比赛（standing_open=0）对其他人一律挡掉。
			q += ` AND (s.contest_id=0 OR ct.id IS NULL
				OR s.user_id=? OR p.owner_id=? OR ct.owner_id=?
				OR ct.end_at<=? OR (ct.standing_open=1 AND ct.mode<>'oi'))`
			args = append(args, f.ViewerID, f.ViewerID, f.ViewerID, nowMS())
		} else {
			q += ` AND p.visible=1`
			q += ` AND (s.contest_id=0 OR ct.id IS NULL
				OR ct.end_at<=? OR (ct.standing_open=1 AND ct.mode<>'oi'))`
			args = append(args, nowMS())
		}
	}
	q += ` ORDER BY s.id DESC LIMIT ?`
	args = append(args, f.Limit)

	rows, err := db.SQL.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SubmissionDetail, 0, f.Limit)
	for rows.Next() {
		d, err := scanDetail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
