package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Contests live beside the problem bank: a contest owns a window, a roster and a
// scoreboard, while the points of a submission keep coming from runs.score exactly
// the way M13 computes them for practice.

const (
	// ContestOI is the CSP-J/S / NOIP / NOI rule: the last submission counts, equal
	// totals share the rank, and nothing is published while the clock runs.
	ContestOI = "oi"
	// ContestIOI is the rule 0013 wrongly called "oi": every problem pays its best
	// submission, and ties are broken by total time.
	ContestIOI = "ioi"
	ContestACM = "acm"

	MaxCIDLen      = 32
	MaxTeamNameLen = 40
	MaxJoinCodeLen = 32

	MinTeamSize     = 1
	MaxTeamSize     = 5
	DefaultPenaltyS = 1200 // 二十分钟一次未过尝试，ACM 的通行罚时
)

// Contest errors already read the way a person needs to see them in the flash, so
// the web layer shows the text instead of translating codes of its own.
var (
	ErrContestClosed    = errors.New("这场比赛已经结束，不能再加入")
	ErrContestHidden    = errors.New("这场比赛不存在")
	ErrContestCode      = errors.New("加入码不对")
	ErrTeamFull         = errors.New("队伍人数已满")
	ErrTeamUnknown      = errors.New("没有这支队伍，或者它不属于这场比赛")
	ErrTeamDisabled     = errors.New("这场比赛是个人赛，不用组队")
	ErrNotContestMember = errors.New("你还不是这场比赛的成员")
)

func ValidContestMode(mode string) bool {
	return mode == ContestOI || mode == ContestIOI || mode == ContestACM
}

type Contest struct {
	ID           int64
	CID          string
	Title        string
	Statement    string
	StartAt      int64
	EndAt        int64
	Mode         string
	JoinCode     string
	StandingOpen bool
	Visible      bool
	TeamSize     int
	PenaltyMS    int64
	OwnerID      int64
	CreatedAt    int64
	UpdatedAt    int64
}

// Phase is read off the clock and never stored, so no scheduled job can leave a
// contest in a state its own times no longer describe.
type Phase int

const (
	PhaseUpcoming Phase = iota
	PhaseRunning
	PhaseOver
)

func (c Contest) Phase(now int64) Phase {
	switch {
	case now < c.StartAt:
		return PhaseUpcoming
	case now >= c.EndAt:
		return PhaseOver
	}
	return PhaseRunning
}

// BoardLive says whether the scoreboard may be shown at all. OI is closed-book: a
// real NOI contest publishes nothing until the clock stops, so showing a live board
// would be the wrong shape of the sport rather than a nicer default.
func (c Contest) BoardLive(now int64) bool {
	if c.Mode == ContestOI && c.Phase(now) != PhaseOver {
		return false
	}
	return true
}

func (p Phase) Label() string {
	switch p {
	case PhaseUpcoming:
		return "未开始"
	case PhaseRunning:
		return "进行中"
	}
	return "已结束"
}

// Badge is the CSS class the phase is coloured with. 进行中 is the one a reader
// acts on, so it is the only phase with an accent.
func (p Phase) Badge() string {
	switch p {
	case PhaseUpcoming:
		return "warn"
	case PhaseRunning:
		return "ok"
	}
	return "muted"
}

type ContestProblem struct {
	ContestID, ProblemID int64
	Ordinal              int
	Points               int
	Label                string
	PID, Title           string
	TimeLimitMS          int
	MemLimitKB           int
	TotalScore           int
	ProblemVisible       bool
}

// ContestLabel is the slug a contest problem is reached by: the label the operator
// typed, or the letter its position implies.
func ContestLabel(label string, ordinal int) string {
	if label != "" {
		return label
	}
	return ordinalLetter(ordinal)
}

func ordinalLetter(n int) string {
	if n >= 1 && n <= 26 {
		return string(rune('A' + n - 1))
	}
	return fmt.Sprint(n)
}

type ContestTeam struct {
	ID        int64
	ContestID int64
	Name      string
	CreatedAt int64
}

type ContestMember struct {
	ContestID int64
	UserID    int64
	TeamID    int64
	TeamName  string
	Handle    string
	Role      string
	JoinedAt  int64
}

// DisplayName is what a standings row is called: the team when the competitor has
// one, otherwise the handle.
func (m ContestMember) DisplayName() string {
	if m.TeamName != "" {
		return m.TeamName
	}
	return m.Handle
}

const contestColumns = `id, cid, title, statement, start_at, end_at, mode, join_code,
		standing_open, visible, team_size, penalty_ms, IFNULL(owner_id, 0), created_at, updated_at`

func scanContest(row scanner) (Contest, error) {
	var (
		p                 Contest
		standing, visible int
	)
	err := row.Scan(&p.ID, &p.CID, &p.Title, &p.Statement, &p.StartAt, &p.EndAt, &p.Mode, &p.JoinCode,
		&standing, &visible, &p.TeamSize, &p.PenaltyMS, &p.OwnerID, &p.CreatedAt, &p.UpdatedAt)
	p.StandingOpen, p.Visible = standing != 0, visible != 0
	return p, notFound(err)
}

func (db *DB) GetContestByID(ctx context.Context, id int64) (Contest, error) {
	return scanContest(db.SQL.QueryRowContext(ctx,
		`SELECT `+contestColumns+` FROM contests WHERE id=?`, id))
}

// FindContest takes the numeric row id or the readable cid, so one route can serve
// both /contests/12 and /contests/C0012.
func (db *DB) FindContest(ctx context.Context, key string) (Contest, error) {
	if onlyDigits(key) {
		if id, err := strconv.ParseInt(key, 10, 64); err == nil {
			return db.GetContestByID(ctx, id)
		}
	}
	return scanContest(db.SQL.QueryRowContext(ctx,
		`SELECT `+contestColumns+` FROM contests WHERE cid=? COLLATE NOCASE`, key))
}

func onlyDigits(s string) bool {
	if s == "" {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// CreateContest inserts one row. A blank cid is filled inside the same statement
// with C plus the next row number, so two people creating a contest at the same
// moment cannot both claim the same label.
func (db *DB) CreateContest(ctx context.Context, c *Contest) (Contest, error) {
	now := nowMS()
	c.CreatedAt, c.UpdatedAt = now, now
	if c.Mode == "" {
		c.Mode = ContestOI
	}
	var cid any
	if c.CID != "" {
		cid = c.CID
	}
	res, err := db.SQL.ExecContext(ctx,
		`INSERT INTO contests (cid, title, statement, start_at, end_at, mode, join_code,
		                      standing_open, visible, team_size, penalty_ms, owner_id, created_at, updated_at)
		 SELECT COALESCE(?, 'C' || substr('0000' || (SELECT IFNULL(MAX(id), 0) + 1 FROM contests), -4)),
		        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`,
		cid, c.Title, c.Statement, c.StartAt, c.EndAt, c.Mode, c.JoinCode,
		boolInt(c.StandingOpen), boolInt(c.Visible), c.TeamSize, c.PenaltyMS,
		nullInt64(c.OwnerID), c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return Contest{}, conflict(err, "比赛编号 "+c.CID)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Contest{}, err
	}
	// Read the row back: it holds the generated cid, and reading beats deriving.
	return db.GetContestByID(ctx, id)
}

// UpdateContest writes every editable column, because the edit form posts a full
// document rather than a patch.
func (db *DB) UpdateContest(ctx context.Context, c Contest) (Contest, error) {
	c.UpdatedAt = nowMS()
	_, err := db.SQL.ExecContext(ctx,
		`UPDATE contests SET cid=?, title=?, statement=?, start_at=?, end_at=?, mode=?, join_code=?,
		        standing_open=?, visible=?, team_size=?, penalty_ms=?, updated_at=? WHERE id=?`,
		c.CID, c.Title, c.Statement, c.StartAt, c.EndAt, modeColumn(c.Mode), c.JoinCode,
		boolInt(c.StandingOpen), boolInt(c.Visible), c.TeamSize, c.PenaltyMS, c.UpdatedAt, c.ID)
	if err != nil {
		return Contest{}, conflict(err, "比赛编号 "+c.CID)
	}
	return db.GetContestByID(ctx, c.ID)
}

// modeColumn keeps an empty mode from tripping the CHECK constraint, the way
// detailColumn does for problems.
func modeColumn(mode string) string {
	if ValidContestMode(mode) {
		return mode
	}
	return ContestOI
}

// DeleteContest drops the contest, its mount table and its roster. Submissions keep
// their contest_id: history is not rewritten because a contest was taken down, and
// the submission page says 比赛已删除 instead of linking at nothing.
func (db *DB) DeleteContest(ctx context.Context, id int64) error {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM contests WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetJoinCode replaces the join code on its own, so regenerating it cannot ride
// along with a form that did not intend to change it.
func (db *DB) SetJoinCode(ctx context.Context, id int64, code string) error {
	_, err := db.SQL.ExecContext(ctx,
		`UPDATE contests SET join_code=?, updated_at=? WHERE id=?`, code, nowMS(), id)
	return err
}

// ContestSummary is one row of the list page. The list query does not even read
// join_code, so a leaked code through the list is structurally impossible.
type ContestSummary struct {
	Contest
	ProblemCount int
	MemberCount  int
}

type ContestPage struct {
	Items     []ContestSummary
	NextAfter int64
	Total     int
}

// listCols is the list projection without join_code.
const listCols = `id, cid, title, statement, start_at, end_at, mode,
		standing_open, visible, team_size, penalty_ms, IFNULL(owner_id, 0), created_at, updated_at`

// ListContests pages by row id descending. The cursor has to be the sort key, so
// the newest contest created leads and the phase badge tells the state.
func (db *DB) ListContests(ctx context.Context, after int64, limit int, search string, staff bool) (ContestPage, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT ` + listCols + ` FROM contests WHERE 1=1`
	args := []any{}
	if after > 0 {
		q += ` AND id<?`
		args = append(args, after)
	}
	if !staff {
		q += ` AND visible=1`
	}
	if search != "" {
		like := "%" + escapeLike(search) + "%"
		q += ` AND (cid LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := db.SQL.QueryContext(ctx, q, args...)
	if err != nil {
		return ContestPage{}, err
	}
	defer rows.Close()

	var page ContestPage
	for rows.Next() {
		var (
			c                 Contest
			standing, visible int
		)
		err := rows.Scan(&c.ID, &c.CID, &c.Title, &c.Statement, &c.StartAt, &c.EndAt, &c.Mode,
			&standing, &visible, &c.TeamSize, &c.PenaltyMS, &c.OwnerID, &c.CreatedAt, &c.UpdatedAt)
		if err != nil {
			return page, err
		}
		c.StandingOpen, c.Visible = standing != 0, visible != 0
		page.Items = append(page.Items, ContestSummary{Contest: c})
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.NextAfter = page.Items[limit-1].ID
		page.Items = page.Items[:limit]
	}

	if err := db.contestCounts(ctx, page.Items); err != nil {
		return page, err
	}

	totalWhere := "WHERE 1=1"
	if !staff {
		totalWhere += " AND visible=1"
	}
	if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM contests `+totalWhere).Scan(&page.Total); err != nil {
		return page, err
	}
	return page, nil
}

// contestCounts fills the problem and member counts for a whole page in two
// grouped queries, so the list is never one query per row.
func (db *DB) contestCounts(ctx context.Context, items []ContestSummary) error {
	if len(items) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
	args := make([]any, len(items))
	byID := make(map[int64]*ContestSummary, len(items))
	for i := range items {
		args[i] = items[i].ID
		byID[items[i].ID] = &items[i]
	}
	for _, one := range []struct {
		dst func(*ContestSummary, int)
		q   string
	}{
		{func(s *ContestSummary, n int) { s.ProblemCount = n },
			`SELECT contest_id, COUNT(*) FROM contest_problems WHERE contest_id IN (` + placeholders + `) GROUP BY contest_id`},
		{func(s *ContestSummary, n int) { s.MemberCount = n },
			`SELECT contest_id, COUNT(*) FROM contest_members WHERE contest_id IN (` + placeholders + `) GROUP BY contest_id`},
	} {
		rows, err := db.SQL.QueryContext(ctx, one.q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				id int64
				n  int
			)
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return err
			}
			if s, ok := byID[id]; ok {
				one.dst(s, n)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// --- mounting problems ---

const contestProblemColumns = `cp.contest_id, cp.problem_id, cp.ordinal, cp.points, cp.label,
		p.pid, p.title, p.time_limit_ms, p.mem_limit_kb,
		(SELECT IFNULL(SUM(t.score), 0) FROM testcases t WHERE t.problem_id = p.id AND t.is_sample = 0),
		p.visible`

func scanContestProblem(row scanner) (ContestProblem, error) {
	var (
		cp  ContestProblem
		vis int
	)
	err := row.Scan(&cp.ContestID, &cp.ProblemID, &cp.Ordinal, &cp.Points, &cp.Label,
		&cp.PID, &cp.Title, &cp.TimeLimitMS, &cp.MemLimitKB, &cp.TotalScore, &vis)
	cp.ProblemVisible = vis != 0
	return cp, notFound(err)
}

func (db *DB) GetContestProblem(ctx context.Context, contestID, problemID int64) (ContestProblem, error) {
	return scanContestProblem(db.SQL.QueryRowContext(ctx,
		`SELECT `+contestProblemColumns+`
		 FROM contest_problems cp JOIN problems p ON p.id = cp.problem_id
		 WHERE cp.contest_id=? AND cp.problem_id=?`, contestID, problemID))
}

func (db *DB) ListContestProblems(ctx context.Context, contestID int64) ([]ContestProblem, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT `+contestProblemColumns+`
		 FROM contest_problems cp JOIN problems p ON p.id = cp.problem_id
		 WHERE cp.contest_id=? ORDER BY cp.ordinal`, contestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ContestProblem
	for rows.Next() {
		cp, err := scanContestProblem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// ContestProblemMounted is what the submit gate checks: a submission may only be
// attributed to a contest that really carries the problem.
func (db *DB) ContestProblemMounted(ctx context.Context, contestID, problemID int64) (bool, error) {
	var one int
	err := db.SQL.QueryRowContext(ctx,
		`SELECT 1 FROM contest_problems WHERE contest_id=? AND problem_id=?`, contestID, problemID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AddContestProblem mounts a problem. Ordinal 0 means "at the end", so appending a
// problem cannot renumber the letters the contestants are already using.
func (db *DB) AddContestProblem(ctx context.Context, cp *ContestProblem) (ContestProblem, error) {
	if cp.Ordinal == 0 {
		var max int
		err := db.SQL.QueryRowContext(ctx,
			`SELECT IFNULL(MAX(ordinal), 0) FROM contest_problems WHERE contest_id=?`, cp.ContestID).Scan(&max)
		if err != nil {
			return ContestProblem{}, err
		}
		cp.Ordinal = max + 1
	}
	_, err := db.SQL.ExecContext(ctx,
		`INSERT INTO contest_problems (contest_id, problem_id, ordinal, points, label) VALUES (?, ?, ?, ?, ?)`,
		cp.ContestID, cp.ProblemID, cp.Ordinal, cp.Points, cp.Label)
	if err != nil {
		// Both the primary key and the ordinal index can fire here, and the
		// operator needs to be told which of the two they have to fix.
		if strings.Contains(err.Error(), "uq_contest_problem_ordinal") {
			return ContestProblem{}, fmt.Errorf("%w: 第 %d 个位置已经有题了", ErrConflict, cp.Ordinal)
		}
		return ContestProblem{}, conflict(err, cp.PID)
	}
	return db.GetContestProblem(ctx, cp.ContestID, cp.ProblemID)
}

// UpdateContestProblem changes only the two settings the contest owns: the score
// override and the label. The problem itself is edited on its own page.
func (db *DB) UpdateContestProblem(ctx context.Context, cp ContestProblem) (ContestProblem, error) {
	_, err := db.SQL.ExecContext(ctx,
		`UPDATE contest_problems SET points=?, label=? WHERE contest_id=? AND problem_id=?`,
		cp.Points, cp.Label, cp.ContestID, cp.ProblemID)
	if err != nil {
		return ContestProblem{}, conflict(err, cp.PID)
	}
	return db.GetContestProblem(ctx, cp.ContestID, cp.ProblemID)
}

func (db *DB) RemoveContestProblem(ctx context.Context, contestID, problemID int64) error {
	res, err := db.SQL.ExecContext(ctx,
		`DELETE FROM contest_problems WHERE contest_id=? AND problem_id=?`, contestID, problemID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveContestProblem swaps a problem with its neighbour. The ordinals travel
// through a negative value: the (contest, ordinal) index is unique, so writing the
// neighbour's number straight onto this row would collide before the swap finished.
func (db *DB) MoveContestProblem(ctx context.Context, contestID, problemID int64, up bool) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		var mine int
		err := tx.QueryRowContext(ctx,
			`SELECT ordinal FROM contest_problems WHERE contest_id=? AND problem_id=?`,
			contestID, problemID).Scan(&mine)
		if err != nil {
			return notFound(err)
		}
		q := `SELECT ordinal FROM contest_problems WHERE contest_id=? AND ordinal<? ORDER BY ordinal DESC LIMIT 1`
		if !up {
			q = `SELECT ordinal FROM contest_problems WHERE contest_id=? AND ordinal>? ORDER BY ordinal ASC LIMIT 1`
		}
		var neighbour int
		err = tx.QueryRowContext(ctx, q, contestID, mine).Scan(&neighbour)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already first or last: a no-op, not an error
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE contest_problems SET ordinal=-ordinal WHERE contest_id=? AND problem_id=?`,
			contestID, problemID); err != nil {
			return err
		}
		// A's number is free now, so B can take it before A moves onto B's old one.
		if _, err := tx.ExecContext(ctx,
			`UPDATE contest_problems SET ordinal=? WHERE contest_id=? AND ordinal=?`,
			mine, contestID, neighbour); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE contest_problems SET ordinal=? WHERE contest_id=? AND problem_id=?`,
			neighbour, contestID, problemID)
		return err
	})
}

// --- roster ---

const memberColumns = `m.contest_id, m.user_id, IFNULL(m.team_id, 0), IFNULL(t.name, ''), u.handle, m.role, m.joined_at`

const memberFrom = ` FROM contest_members m
	JOIN users u ON u.id = m.user_id
	LEFT JOIN contest_teams t ON t.id = m.team_id`

func scanMember(row scanner) (ContestMember, error) {
	var mm ContestMember
	err := row.Scan(&mm.ContestID, &mm.UserID, &mm.TeamID, &mm.TeamName, &mm.Handle, &mm.Role, &mm.JoinedAt)
	return mm, notFound(err)
}

// ContestMemberOf is the gate question for one reader. The bool is the answer, so
// a missing row is not an error a caller has to tell apart from a bug.
func (db *DB) ContestMemberOf(ctx context.Context, contestID, userID int64) (ContestMember, bool) {
	return memberIn(ctx, db.SQL, contestID, userID)
}

// querier lets one lookup run either on the pool or inside a transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func memberIn(ctx context.Context, q querier, contestID, userID int64) (ContestMember, bool) {
	mm, err := scanMember(q.QueryRowContext(ctx,
		`SELECT `+memberColumns+memberFrom+` WHERE m.contest_id=? AND m.user_id=?`, contestID, userID))
	if err != nil {
		return ContestMember{}, false
	}
	return mm, true
}

func (db *DB) ListContestMembers(ctx context.Context, contestID int64) ([]ContestMember, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT `+memberColumns+memberFrom+` WHERE m.contest_id=? ORDER BY IFNULL(m.team_id, 0), u.handle`, contestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ContestMember
	for rows.Next() {
		mm, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, mm)
	}
	return out, rows.Err()
}

// JoinContest admits a person, and is the only place the join rules live so the
// page and the form cannot drift apart. Joining twice succeeds quietly: a
// double-clicked button must not look like a rejection.
func (db *DB) JoinContest(ctx context.Context, contestID, userID int64, code, teamName string) (ContestMember, error) {
	c, err := db.GetContestByID(ctx, contestID)
	if err != nil {
		return ContestMember{}, err
	}
	if existing, ok := db.ContestMemberOf(ctx, contestID, userID); ok {
		return existing, nil
	}
	// 未公开且没有加入码的比赛（草稿/内部赛）不是公开入口：不知道编号的人不该通过
	// join 这条路由把它变成“能看题目、能看名单”的成员。隐藏但有码的私有赛仍按码放行；
	// 已有成员在上面直接返回，不受这条影响。
	if !c.Visible && c.JoinCode == "" {
		return ContestMember{}, ErrContestHidden
	}
	// A constant-time comparison costs nothing here and keeps the difference
	// between "wrong code" and "not yet tried that code" out of the timings.
	if c.JoinCode != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(code)), []byte(c.JoinCode)) != 1 {
		return ContestMember{}, ErrContestCode
	}
	// 未开始也可以报名，已结束不行：赛后补进榜单 would rewrite the field.
	if c.Phase(nowMS()) == PhaseOver {
		return ContestMember{}, ErrContestClosed
	}
	var handle string
	if err := db.SQL.QueryRowContext(ctx, `SELECT handle FROM users WHERE id=?`, userID).Scan(&handle); err != nil {
		return ContestMember{}, notFound(err)
	}

	var out ContestMember
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		// Re-read inside the write: two tabs joining at the same moment would
		// otherwise both pass the check above.
		if existing, ok := memberIn(ctx, tx, contestID, userID); ok {
			out = existing
			return nil
		}
		teamID, err := resolveTeamInTx(ctx, tx, c, teamName, handle)
		if err != nil {
			return err
		}
		role := "member"
		if c.OwnerID == userID {
			role = "owner"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO contest_members (contest_id, user_id, team_id, role, joined_at) VALUES (?, ?, ?, ?, ?)`,
			contestID, userID, nullInt64(teamID), role, nowMS()); err != nil {
			return conflict(err, handle)
		}
		out, _ = memberIn(ctx, tx, contestID, userID)
		return nil
	})
	if err != nil {
		return ContestMember{}, err
	}
	return out, nil
}

// resolveTeamInTx works out which team a newcomer lands in. A solo contest has no
// teams at all; a team contest uses the team they asked for, or one named after
// them, so nobody is left outside the board in a team round.
func resolveTeamInTx(ctx context.Context, tx *sql.Tx, c Contest, want, handle string) (int64, error) {
	if c.TeamSize <= MinTeamSize {
		if strings.TrimSpace(want) != "" {
			return 0, ErrTeamDisabled
		}
		return 0, nil
	}
	if strings.TrimSpace(want) == "" {
		want = handle
	}
	var (
		id int64
		n  int
	)
	err := tx.QueryRowContext(ctx,
		`SELECT t.id, (SELECT COUNT(*) FROM contest_members m WHERE m.team_id = t.id)
		 FROM contest_teams t WHERE t.contest_id=? AND t.name=? COLLATE NOCASE`, c.ID, want).Scan(&id, &n)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO contest_teams (contest_id, name, created_at) VALUES (?, ?, ?)`, c.ID, want, nowMS())
		if err != nil {
			return 0, conflict(err, want)
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	if n >= c.TeamSize {
		return 0, ErrTeamFull
	}
	return id, nil
}

// LeaveContest removes the person and tidies a team that no longer has anybody in
// it, so an empty shell cannot haunt the team list.
func (db *DB) LeaveContest(ctx context.Context, contestID, userID int64) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		mm, ok := memberIn(ctx, tx, contestID, userID)
		if !ok {
			return ErrNotContestMember
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM contest_members WHERE contest_id=? AND user_id=?`, contestID, userID); err != nil {
			return err
		}
		if mm.TeamID == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM contest_teams WHERE id=? AND NOT EXISTS
			 (SELECT 1 FROM contest_members m WHERE m.team_id = contest_teams.id)`, mm.TeamID)
		return err
	})
}

// --- teams ---

func (db *DB) CreateTeam(ctx context.Context, contestID int64, name string) (ContestTeam, error) {
	now := nowMS()
	res, err := db.SQL.ExecContext(ctx,
		`INSERT INTO contest_teams (contest_id, name, created_at) VALUES (?, ?, ?)`, contestID, name, now)
	if err != nil {
		return ContestTeam{}, conflict(err, name)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ContestTeam{}, err
	}
	return ContestTeam{ID: id, ContestID: contestID, Name: name, CreatedAt: now}, nil
}

func (db *DB) FindTeam(ctx context.Context, contestID int64, name string) (ContestTeam, error) {
	var t ContestTeam
	err := db.SQL.QueryRowContext(ctx,
		`SELECT id, contest_id, name, created_at FROM contest_teams WHERE contest_id=? AND name=? COLLATE NOCASE`,
		contestID, name).Scan(&t.ID, &t.ContestID, &t.Name, &t.CreatedAt)
	return t, notFound(err)
}

func (db *DB) ListTeams(ctx context.Context, contestID int64) ([]ContestTeam, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT id, contest_id, name, created_at FROM contest_teams WHERE contest_id=? ORDER BY name`, contestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ContestTeam
	for rows.Next() {
		var t ContestTeam
		if err := rows.Scan(&t.ID, &t.ContestID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTeam moves one member into a team, or drops them out of it when teamID is 0.
// Lowering team_size later does not eject anyone already inside an over-sized team,
// so the cap is only checked against the head this call would add.
func (db *DB) SetTeam(ctx context.Context, contestID, userID, teamID int64) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		var (
			teamSize int
			current  sql.NullInt64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT c.team_size, m.team_id FROM contests c
			 JOIN contest_members m ON m.contest_id = c.id
			 WHERE c.id=? AND m.user_id=?`, contestID, userID).Scan(&teamSize, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotContestMember
		}
		if err != nil {
			return err
		}
		if teamSize <= MinTeamSize {
			return ErrTeamDisabled
		}
		if teamID == 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE contest_members SET team_id=NULL WHERE contest_id=? AND user_id=?`, contestID, userID); err != nil {
				return err
			}
			if current.Int64 == 0 {
				return nil
			}
			_, err = tx.ExecContext(ctx,
				`DELETE FROM contest_teams WHERE id=? AND NOT EXISTS
				 (SELECT 1 FROM contest_members m WHERE m.team_id = contest_teams.id)`, current.Int64)
			return err
		}
		if current.Int64 == teamID {
			return nil
		}
		var (
			belongs int64
			room    int
		)
		err = tx.QueryRowContext(ctx,
			`SELECT t.contest_id, ? - (SELECT COUNT(*) FROM contest_members m WHERE m.team_id = t.id)
			 FROM contest_teams t WHERE t.id=?`, teamSize, teamID).Scan(&belongs, &room)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTeamUnknown
		}
		if err != nil {
			return err
		}
		if belongs != contestID {
			return ErrTeamUnknown
		}
		if room <= 0 {
			return ErrTeamFull
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE contest_members SET team_id=? WHERE contest_id=? AND user_id=?`, teamID, contestID, userID); err != nil {
			return err
		}
		// Moving out of the old team must not leave an empty shell in the list.
		if current.Int64 != 0 {
			_, err = tx.ExecContext(ctx,
				`DELETE FROM contest_teams WHERE id=? AND NOT EXISTS
				 (SELECT 1 FROM contest_members m WHERE m.team_id = contest_teams.id)`, current.Int64)
		}
		return err
	})
}

// ContestRoster is one competitor: a team, or a person outside every team.
type ContestRoster struct {
	TeamID  int64
	Name    string
	Handles []string
	UserIDs []int64
}

// TeamRoster groups members into competitors. Order is by display name, so two
// calls over the same roster agree.
func TeamRoster(members []ContestMember) []ContestRoster {
	byID := map[int64]int{}
	var out []ContestRoster
	for _, mm := range members {
		if mm.TeamID == 0 {
			out = append(out, ContestRoster{
				Name: mm.Handle, Handles: []string{mm.Handle}, UserIDs: []int64{mm.UserID},
			})
			continue
		}
		at, seen := byID[mm.TeamID]
		if !seen {
			byID[mm.TeamID] = len(out)
			out = append(out, ContestRoster{TeamID: mm.TeamID, Name: mm.TeamName})
			at = len(out) - 1
		}
		out[at].Handles = append(out[at].Handles, mm.Handle)
		out[at].UserIDs = append(out[at].UserIDs, mm.UserID)
	}
	for _, r := range out {
		sort.Strings(r.Handles)
		sort.Slice(r.UserIDs, func(i, j int) bool { return r.UserIDs[i] < r.UserIDs[j] })
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].TeamID < out[j].TeamID
	})
	return out
}

// --- scoring ---

// Cell is one competitor's record on one problem. Seen separates "has not touched
// it" from "tried and scored nothing", which is the difference a board reader cares
// about.
type Cell struct {
	Label    string
	PID      string
	Title    string
	Seen     bool
	Attempts int
	Points   int
	Max      int
	Solved   bool
	TimeMS   int64
	Removed  bool // 题目已被移出比赛，历史成绩照算
}

// StandingRow is one competitor on the board.
type StandingRow struct {
	Rank    int
	TeamID  int64
	Name    string
	Handles []string
	UserIDs []int64
	Solved  int
	Score   int
	TimeMS  int64 // OI: 总用时；ACM: 总罚时
	Cells   []Cell
}

// subEvent is one judged submission as the board sees it.
type subEvent struct {
	at      int64
	verdict string
	raw     int
}

type problemStat struct {
	seen     bool
	attempts int
	penalise int
	best     int
	bestAt   int64
	// last is the submission a real OI contest scores you on: the newest one the
	// judge graded, however good or bad it turned out. A CE counts — that broken
	// program is what was handed in.
	last     int
	solvedAt int64
}

// statOf folds a competitor's submissions on one problem into what the three modes
// need. Nothing here relies on the arrival order: the first AC and the best score
// are picked by judged time, so two submissions finishing in the same second cannot
// disagree about the answer.
func statOf(evs []subEvent) problemStat {
	var st problemStat
	last := int64(-1)
	lastOK := int64(-1)
	for _, e := range evs {
		st.seen = true
		if e.verdict != "IE" {
			st.attempts++ // 判机故障不算选手的尝试
		}
		if e.verdict == "AC" && (st.solvedAt == 0 || e.at < st.solvedAt) {
			st.solvedAt = e.at
		}
		switch {
		case e.raw > st.best:
			st.best, st.bestAt = e.raw, e.at
		case e.raw == st.best && e.raw > 0 && e.at < st.bestAt:
			st.bestAt = e.at
		}
		if e.at >= last {
			last = e.at
		}
		// An IE is the judge breaking down, so it is not "the last submission" an OI
		// board scores on: it must not wipe out a good answer that was already in.
		if e.verdict != "IE" && e.at >= lastOK {
			lastOK, st.last = e.at, e.raw
		}
	}
	if st.best == 0 && last > 0 {
		// Nobody scored on this problem; the time it took is the last attempt, not
		// the first.
		st.bestAt = last
	}
	if st.solvedAt > 0 {
		for _, e := range evs {
			switch e.verdict {
			case "AC", "CE", "IE":
			default:
				if e.at < st.solvedAt {
					st.penalise++
				}
			}
		}
	}
	return st
}

// contestEvents reads every judged submission of the contest in one query, with the
// points each one earned.
func (db *DB) contestEvents(ctx context.Context, contestID int64) (map[int64]map[int64][]subEvent, map[int64]string, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT s.user_id, u.handle, s.problem_id, s.verdict,
		        IFNULL(NULLIF(s.judged_at, 0), s.created_at),
		        (SELECT IFNULL(SUM(r.score), 0) FROM runs r WHERE r.submission_id = s.id AND r.is_sample = 0)
		 FROM submissions s JOIN users u ON u.id = s.user_id
		 WHERE s.contest_id=? AND s.status='done'
		 ORDER BY s.user_id, s.problem_id, s.id`, contestID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	events := map[int64]map[int64][]subEvent{}
	handles := map[int64]string{}
	for rows.Next() {
		var (
			userID, problemID, at int64
			verdict, handle       string
			raw                   int
		)
		if err := rows.Scan(&userID, &handle, &problemID, &verdict, &at, &raw); err != nil {
			return nil, nil, err
		}
		handles[userID] = handle
		byProblem := events[userID]
		if byProblem == nil {
			byProblem = map[int64][]subEvent{}
			events[userID] = byProblem
		}
		byProblem[problemID] = append(byProblem[problemID], subEvent{at: at, verdict: verdict, raw: raw})
	}
	return events, handles, rows.Err()
}

// ContestScoreboard is three queries — the mount table, the roster, the judged
// submissions — and the arithmetic in Go. A window function per mode would be less
// readable and still need the same fold.
func (db *DB) ContestScoreboard(ctx context.Context, c Contest) ([]StandingRow, error) {
	problems, err := db.ListContestProblems(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	members, err := db.ListContestMembers(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	events, handles, err := db.contestEvents(ctx, c.ID)
	if err != nil {
		return nil, err
	}

	mounts := make(map[int64]ContestProblem, len(problems))
	for _, cp := range problems {
		mounts[cp.ProblemID] = cp
	}
	// A problem taken out of the contest does not lose its history: those
	// submissions still pay, into a column marked 已移除.
	removed := map[int64]bool{}
	var orphans []int64
	for problemID := range events {
		if _, ok := mounts[problemID]; !ok {
			orphans = append(orphans, problemID)
		}
	}
	if len(orphans) > 0 {
		extra, err := db.bareProblems(ctx, orphans)
		if err != nil {
			return nil, err
		}
		for _, cp := range extra {
			mounts[cp.ProblemID] = cp
			removed[cp.ProblemID] = true
		}
	}

	order := make([]int64, 0, len(mounts))
	for id := range mounts {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := mounts[order[i]], mounts[order[j]]
		if a.Ordinal != b.Ordinal {
			return a.Ordinal < b.Ordinal
		}
		return a.ProblemID < b.ProblemID
	})

	roster := TeamRoster(members)
	// Someone who submitted and then left is still a competitor: their result was
	// earned inside the contest and attribution does not unwind.
	listed := map[int64]bool{}
	for _, r := range roster {
		for _, id := range r.UserIDs {
			listed[id] = true
		}
	}
	for userID := range events {
		if !listed[userID] {
			roster = append(roster, ContestRoster{
				Name: handles[userID], Handles: []string{handles[userID]}, UserIDs: []int64{userID},
			})
		}
	}
	sort.Slice(roster, func(i, j int) bool {
		if roster[i].Name != roster[j].Name {
			return roster[i].Name < roster[j].Name
		}
		return roster[i].TeamID < roster[j].TeamID
	})

	rows := make([]StandingRow, 0, len(roster))
	for _, r := range roster {
		row := StandingRow{TeamID: r.TeamID, Name: r.Name, Handles: r.Handles, UserIDs: r.UserIDs}
		for _, problemID := range order {
			cp := mounts[problemID]
			var evs []subEvent
			for _, userID := range r.UserIDs {
				for _, e := range events[userID][problemID] {
					// The contest may rescale a problem; fold before comparing.
					e.raw = ContestAward(e.raw, cp.Points, cp.TotalScore)
					evs = append(evs, e)
				}
			}
			// A team plays one problem as one competitor: the best answer counts, and
			// every member's attempts feed the shared penalty.
			sort.SliceStable(evs, func(i, j int) bool { return evs[i].at < evs[j].at })
			st := statOf(evs)
			cell := Cell{
				Label: ContestLabel(cp.Label, cp.Ordinal), PID: cp.PID, Title: cp.Title,
				Seen: st.seen, Attempts: st.attempts, Max: contestMax(cp),
				Removed: removed[problemID],
			}
			switch c.Mode {
			case ContestACM:
				cell.Solved = st.solvedAt > 0
				if cell.Solved {
					cell.Points = cell.Max
					cell.TimeMS = clampAge(st.solvedAt, c.StartAt)
					row.Solved++
					row.TimeMS += cell.TimeMS + int64(st.penalise)*c.PenaltyMS
				}
			case ContestOI:
				// 最后一次提交说了算，也不累计用时：OI 榜上没有罚时这一说。
				cell.Points = st.last
				cell.Solved = cell.Max > 0 && st.last == cell.Max
			default:
				cell.Points = st.best
				cell.Solved = cell.Max > 0 && st.best == cell.Max
				cell.TimeMS = clampAge(st.bestAt, c.StartAt)
				row.TimeMS += cell.TimeMS
			}
			row.Score += cell.Points
			row.Cells = append(row.Cells, cell)
		}
		// Only people who have actually been judged are on the board: a roster of
		// hundreds would otherwise bury the dozen who played, and a sealed board would
		// hand out a first place to someone who never submitted.
		if !rowTouched(row) {
			continue
		}
		rows = append(rows, row)
	}

	sortStandings(rows, c.Mode)
	return rows, nil
}

// bareProblems names the problems a contest still owes a column for after they were
// unmounted. They have no ordinal or override, so they sort after the live ones.
func (db *DB) bareProblems(ctx context.Context, ids []int64) ([]ContestProblem, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT p.id, p.pid, p.title,
		        (SELECT IFNULL(SUM(t.score), 0) FROM testcases t WHERE t.problem_id = p.id AND t.is_sample = 0)
		 FROM problems p WHERE p.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ContestProblem
	for rows.Next() {
		var cp ContestProblem
		if err := rows.Scan(&cp.ProblemID, &cp.PID, &cp.Title, &cp.TotalScore); err != nil {
			return nil, err
		}
		cp.Ordinal = 1<<30 + len(out)
		cp.Label = "—"
		out = append(out, cp)
	}
	return out, rows.Err()
}

// rowTouched reports whether a competitor has at least one judged submission.
func rowTouched(row StandingRow) bool {
	for _, cell := range row.Cells {
		if cell.Seen {
			return true
		}
	}
	return false
}

// contestMax is the most a problem can pay inside a contest: the override when the
// operator set one, otherwise the problem's own total.
func contestMax(cp ContestProblem) int {
	if cp.Points > 0 {
		return cp.Points
	}
	return cp.TotalScore
}

// clampAge turns an absolute timestamp into elapsed contest time, floored at zero so
// a submission judged before the clock started cannot hand out negative time.
func clampAge(at, start int64) int64 {
	if at <= start {
		return 0
	}
	return at - start
}

// rankKey is what two competitors must agree on to share a rank. The name is not
// part of it: it only keeps the printed order stable.
type rankKey struct {
	primary   int
	secondary int64
}

// sortStandings ranks the field and writes the rank back. Equal keys share a rank,
// which is what makes a tie visible instead of arbitrary.
func sortStandings(rows []StandingRow, mode string) {
	key := func(r StandingRow) rankKey {
		switch mode {
		case ContestACM:
			return rankKey{-r.Solved, r.TimeMS}
		case ContestOI:
			// 同分就是同名次：OI 不比用时，也没有罚时可比。
			return rankKey{-r.Score, 0}
		}
		return rankKey{-r.Score, r.TimeMS}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ki, kj := key(rows[i]), key(rows[j])
		if ki.primary != kj.primary {
			return ki.primary < kj.primary
		}
		if ki.secondary != kj.secondary {
			return ki.secondary < kj.secondary
		}
		return rows[i].Name < rows[j].Name
	})
	for i := range rows {
		if i > 0 && key(rows[i-1]) == key(rows[i]) {
			rows[i].Rank = rows[i-1].Rank
			continue
		}
		rows[i].Rank = i + 1
	}
}

// ContestSelfStanding is the 封榜 answer: the whole field is ranked, but only the
// caller's own row is handed back, so a private board can still show a true place.
func (db *DB) ContestSelfStanding(ctx context.Context, c Contest, userID int64) (StandingRow, bool, error) {
	rows, err := db.ContestScoreboard(ctx, c)
	if err != nil {
		return StandingRow{}, false, err
	}
	for _, r := range rows {
		for _, id := range r.UserIDs {
			if id == userID {
				return r, true, nil
			}
		}
	}
	return StandingRow{}, false, nil
}

// ContestAward folds a submission's raw case points into the contest's own scale: an
// override splits the problem proportionally, so 比赛内分值 can never exceed what the
// code actually earned.
func ContestAward(rawScore, override, problemTotal int) int {
	if override <= 0 || problemTotal <= 0 {
		return rawScore
	}
	return int(float64(rawScore*override)/float64(problemTotal) + 0.5)
}

// ValidTeamName is the form-side rule for a team label.
func ValidTeamName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/\\\t\n") && utf8.RuneCountInString(name) <= MaxTeamNameLen
}
