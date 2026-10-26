package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type Counts struct {
	Problems    int `json:"problems"`
	Users       int `json:"users"`
	Submissions int `json:"submissions"`
	Pending     int `json:"pending"`
	Judges      int `json:"judges"`
}

func (db *DB) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	for _, q := range []struct {
		dst *int
		sql string
	}{
		{&c.Problems, `SELECT COUNT(*) FROM problems`},
		{&c.Users, `SELECT COUNT(*) FROM users`},
		{&c.Submissions, `SELECT COUNT(*) FROM submissions`},
		{&c.Pending, `SELECT COUNT(*) FROM submissions WHERE status='queued'`},
		{&c.Judges, `SELECT COUNT(*) FROM judges`},
	} {
		if err := db.SQL.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return c, fmt.Errorf("count %s: %w", q.sql, err)
		}
	}
	return c, nil
}

// ProblemCounts backs the web problem list: how many submissions a problem has
// drawn and how many distinct people have solved it.
type ProblemCounts struct {
	Submits int `json:"submits"`
	Solved  int `json:"solved"`
}

func (db *DB) ProblemCounts(ctx context.Context, problemIDs []int64) (map[int64]ProblemCounts, error) {
	out := make(map[int64]ProblemCounts, len(problemIDs))
	if len(problemIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(problemIDs))
	for i, id := range problemIDs {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(problemIDs)), ",")

	rows, err := db.SQL.QueryContext(ctx,
		`SELECT problem_id, COUNT(*), COUNT(DISTINCT user_id)
		 FROM submissions WHERE problem_id IN (`+placeholders+`) GROUP BY problem_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id          int64
			submits     int
			distinctAll int
		)
		if err := rows.Scan(&id, &submits, &distinctAll); err != nil {
			return nil, err
		}
		out[id] = ProblemCounts{Submits: submits}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Accepted solvers are counted separately so a user with ten AC rows on one
	// problem still counts once.
	rows, err = db.SQL.QueryContext(ctx,
		`SELECT problem_id, COUNT(DISTINCT user_id)
		 FROM submissions WHERE verdict='AC' AND problem_id IN (`+placeholders+`) GROUP BY problem_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var solved int
		if err := rows.Scan(&id, &solved); err != nil {
			return nil, err
		}
		c := out[id]
		c.Solved = solved
		out[id] = c
	}
	return out, rows.Err()
}

// RankRow is one leaderboard entry.
type RankRow struct {
	UserID int64  `json:"user_id"`
	Handle string `json:"handle"`
	// Name is who the row prints as; for a cancelled account it is the tombstone
	// label, while Handle still addresses the page that holds their records.
	Name    string `json:"name"`
	Solved  int    `json:"solved"`
	Submits int    `json:"submits"`
	LastAC  int64  `json:"last_ac"`
	// Points 与 Color 是后台算好的活跃度与它的读数：排行榜本来就在读这一行 users，
	// 带出来不多一次查询，而"名字的颜色"在这里必须和别处一致。
	Points int    `json:"points"`
	Color  string `json:"color,omitempty"`
}

// Leaderboard ranks people by the number of distinct problems they have AC'd.
func (db *DB) Leaderboard(ctx context.Context, limit int) ([]RankRow, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT u.id, u.handle, `+personSQL("u")+`,
		        (SELECT COUNT(DISTINCT s.problem_id) FROM submissions s WHERE s.user_id=u.id AND s.verdict='AC'),
		        (SELECT COUNT(*) FROM submissions s WHERE s.user_id=u.id),
		        (SELECT MAX(s.judged_at) FROM submissions s WHERE s.user_id=u.id AND s.verdict='AC'),
		        IFNULL(u.points, 0), `+colorSQL("u")+`
		 FROM users u
		 WHERE u.role <> 'judge'
		 ORDER BY 4 DESC, 5 ASC, u.id ASC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RankRow
	for rows.Next() {
		var (
			r      RankRow
			lastAC sql.NullInt64
		)
		if err := rows.Scan(&r.UserID, &r.Handle, &r.Name, &r.Solved, &r.Submits, &lastAC,
			&r.Points, &r.Color); err != nil {
			return nil, err
		}
		r.LastAC = nullTime(lastAC)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GlobalStats is what the admin statistics page answers: how many people, how many
// submissions, and what the judge actually said.
type GlobalStats struct {
	Users       int `json:"users"`
	ActiveUsers int `json:"active_users"`
	BannedUsers int `json:"banned_users"`
	Admins      int `json:"admins"`

	Submissions int `json:"submissions"`
	Submitters  int `json:"submitters"`
	ACs         int `json:"acs"`
	Queued      int `json:"queued"`

	Problems  int `json:"problems"`
	Visible   int `json:"visible_problems"`
	Testcases int `json:"testcases"`

	Posts        int `json:"posts"`
	Topics       int `json:"topics"`
	Comments     int `json:"comments"`
	Tickets      int `json:"tickets"`
	OpenTickets  int `json:"open_tickets"`
	Contests     int `json:"contests"`
	ContestSubs  int `json:"contest_submissions"`
	JudgesOnline int `json:"judges_online"`
}

// AverageSubmits is 人均提交. The denominator is the people who actually submitted
// something, not the headcount: dividing by every registered account would blend
// "never opened the site" into the average and tell an operator nothing.
func (g GlobalStats) AverageSubmits() float64 {
	if g.Submitters == 0 {
		return 0
	}
	return float64(g.Submissions) / float64(g.Submitters)
}

// ACRate is over finished runs only, so a queue full of in-flight submissions does
// not look like a collapse.
func (g GlobalStats) ACRate() float64 {
	done := g.Submissions - g.Queued
	if done <= 0 {
		return 0
	}
	return float64(g.ACs) / float64(done) * 100
}

// NameCount is one row of a "how many of each" table.
type NameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func (db *DB) GlobalStats(ctx context.Context) (GlobalStats, error) {
	var g GlobalStats
	scans := []struct {
		dst *int
		q   string
	}{
		{&g.Users, `SELECT COUNT(*) FROM users`},
		{&g.ActiveUsers, `SELECT COUNT(*) FROM users WHERE status='active'`},
		{&g.BannedUsers, `SELECT COUNT(*) FROM users WHERE status<>'active'`},
		{&g.Admins, `SELECT COUNT(*) FROM users WHERE role='admin'`},
		{&g.Submissions, `SELECT COUNT(*) FROM submissions`},
		{&g.Submitters, `SELECT COUNT(DISTINCT user_id) FROM submissions`},
		{&g.ACs, `SELECT COUNT(*) FROM submissions WHERE verdict='AC'`},
		{&g.Queued, `SELECT COUNT(*) FROM submissions WHERE status IN ('queued','compiling','running')`},
		{&g.Problems, `SELECT COUNT(*) FROM problems`},
		{&g.Visible, `SELECT COUNT(*) FROM problems WHERE visible=1`},
		{&g.Testcases, `SELECT COUNT(*) FROM testcases WHERE is_sample=0`},
		{&g.Posts, `SELECT COUNT(*) FROM posts`},
		{&g.Topics, `SELECT COUNT(*) FROM topics`},
		{&g.Comments, `SELECT COUNT(*) FROM topic_comments`},
		{&g.Tickets, `SELECT COUNT(*) FROM tickets`},
		{&g.OpenTickets, `SELECT COUNT(*) FROM tickets WHERE status IN ('open','doing')`},
		{&g.Contests, `SELECT COUNT(*) FROM contests`},
		{&g.ContestSubs, `SELECT COUNT(*) FROM submissions WHERE contest_id <> 0`},
	}
	for _, s := range scans {
		if err := db.SQL.QueryRowContext(ctx, s.q).Scan(s.dst); err != nil {
			return g, fmt.Errorf("stats %s: %w", s.q, err)
		}
	}
	// 在线判机数需要一个时间点参数，所以放在没有参数的那批外面单独问。
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM judges WHERE last_seen_at >= ?`, nowMS()-int64(90*time.Second.Milliseconds())).
		Scan(&g.JudgesOnline); err != nil {
		return g, fmt.Errorf("stats judges: %w", err)
	}
	return g, nil
}

// VerdictCounts is the whole-site judgement mix, including the in-flight statuses so
// an operator can see a stuck queue as a pile of "queued".
func (db *DB) VerdictCounts(ctx context.Context) ([]NameCount, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT CASE WHEN verdict='' THEN status ELSE verdict END AS k, COUNT(*)
		 FROM submissions GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var n NameCount
		if err := rows.Scan(&n.Name, &n.Count); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SubmitsByDay counts submissions per local calendar day, newest first. Days with no
// activity are simply absent: an operator reading "周二 12 条" does not need the zeros.
func (db *DB) SubmitsByDay(ctx context.Context, days int) ([]NameCount, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	cutoff := nowMS() - int64(days)*24*3600*1000
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', created_at/1000, 'unixepoch', 'localtime') AS d, COUNT(*)
		 FROM submissions WHERE created_at >= ?
		 GROUP BY d ORDER BY d DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var n NameCount
		if err := rows.Scan(&n.Name, &n.Count); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TopSubmittedProblems is the "which problem is people beating on" list.
func (db *DB) TopSubmittedProblems(ctx context.Context, limit int) ([]NameCount, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT p.pid || '　' || p.title, COUNT(s.id)
		 FROM submissions s JOIN problems p ON p.id=s.problem_id
		 GROUP BY s.problem_id, p.pid, p.title
		 ORDER BY 2 DESC, p.pid LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var n NameCount
		if err := rows.Scan(&n.Name, &n.Count); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SolvedProblem is one entry on a user's solved list.
type SolvedProblem struct {
	PID      string `json:"pid"`
	Title    string `json:"title"`
	BestTime int    `json:"best_time_ms"`
	SolvedAt int64  `json:"solved_at"`
}

func (db *DB) SolvedProblems(ctx context.Context, userID int64) ([]SolvedProblem, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT p.pid, p.title, MIN(s.time_ms), MAX(s.judged_at)
		 FROM submissions s JOIN problems p ON p.id=s.problem_id
		 WHERE s.user_id=? AND s.verdict='AC'
		 GROUP BY s.problem_id, p.pid, p.title
		 ORDER BY MAX(s.judged_at) DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SolvedProblem
	for rows.Next() {
		var sp SolvedProblem
		if err := rows.Scan(&sp.PID, &sp.Title, &sp.BestTime, &sp.SolvedAt); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// VerdictTally counts a user's submissions by verdict, for the profile summary.
func (db *DB) VerdictTally(ctx context.Context, userID int64) (map[string]int, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT CASE WHEN verdict='' THEN status ELSE verdict END, COUNT(*)
		 FROM submissions WHERE user_id=? GROUP BY 1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		out[key] = n
	}
	return out, rows.Err()
}

// nullTime turns a nullable column into zero so view structs stay plain.
func nullTime(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}
