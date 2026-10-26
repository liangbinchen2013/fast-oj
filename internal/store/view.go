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
	Color         string `json:"color,omitempty"`
	// DetailLevel and ProblemOwner come from the problem row: they decide how much
	// of this submission's judging detail the reader is shown.
	DetailLevel  string `json:"detail_level"`
	ProblemOwner int64  `json:"problem_owner"`
	// ContestCID is empty when the contest was deleted, which is how the page can
	// say 比赛已删除 instead of linking at nothing.
	ContestCID string `json:"contest_cid"`
}

var detailColumns = `s.id, s.user_id, s.problem_id, IFNULL(s.contest_id, 0), s.language, s.options, s.status, s.verdict,
		s.time_ms, s.mem_kb, s.time_min, s.mem_min, s.score, s.total_score, s.failed_case,
		IFNULL(s.judge_id, 0),
		s.created_at, IFNULL(s.started_at, 0), IFNULL(s.judged_at, 0), s.toolchain,
		p.pid, p.title, u.handle, ` + personSQL(`u`) + `, ` + colorSQL(`u`) + `,
		IFNULL(p.detail_level, 'diff'), IFNULL(p.owner_id, 0), IFNULL(ct.cid, '')`

func scanDetail(row scanner) (SubmissionDetail, error) {
	var (
		d               SubmissionDetail
		judgeID         int64
		timeMin, memMin int
	)
	err := row.Scan(&d.ID, &d.UserID, &d.ProblemID, &d.ContestID, &d.Language, &d.Options, &d.Status, &d.Verdict,
		&d.TimeMS, &d.MemKB, &timeMin, &memMin, &d.Score, &d.TotalScore, &d.FailedCase, &judgeID,
		&d.CreatedAt, &d.StartedAt, &d.JudgedAt, &d.Toolchain, &d.PID, &d.Title, &d.Handle, &d.Name, &d.Color,
		&d.DetailLevel, &d.ProblemOwner, &d.ContestCID)
	d.JudgeID = judgeID
	d.TimeMin, d.MemMin = timeMin != 0, memMin != 0
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
	AfterID   int64
	Limit     int
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
	if f.AfterID != 0 {
		q += ` AND s.id<?`
		args = append(args, f.AfterID)
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
