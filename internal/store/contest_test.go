package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var hctx = context.Background()

// minuteMS is the unit the contest clock tests are written in.
const minuteMS = int64(time.Minute / time.Millisecond)

// defaultContest fills the boring fields so a test writes only what it asserts: a
// window that is currently open, one person per competitor, no join code.
func defaultContest(c *Contest) *Contest {
	now := time.Now().UnixMilli()
	if c.Title == "" {
		c.Title = "比赛 " + c.CID
	}
	if c.StartAt == 0 {
		c.StartAt = now - minuteMS
	}
	if c.EndAt == 0 {
		c.EndAt = now + 60*minuteMS
	}
	if c.Mode == "" {
		c.Mode = ContestOI
	}
	if c.TeamSize == 0 {
		c.TeamSize = 1
	}
	if c.PenaltyMS == 0 {
		c.PenaltyMS = 20 * minuteMS
	}
	// The database default is hidden (a half-filled form should not publish a
	// contest), but the test fixtures are normal public contests unless a test
	// explicitly asks for a hidden one via seedHiddenContest.
	c.Visible = true
	return c
}

func seedContest(t *testing.T, db *DB, c *Contest) Contest {
	t.Helper()
	out, err := db.CreateContest(hctx, defaultContest(c))
	if err != nil {
		t.Fatalf("create contest: %v", err)
	}
	return out
}

func seedHiddenContest(t *testing.T, db *DB, c *Contest) Contest {
	t.Helper()
	out := seedContest(t, db, c)
	out.Visible = false
	updated, err := db.UpdateContest(hctx, out)
	if err != nil {
		t.Fatalf("hide contest: %v", err)
	}
	return updated
}

// seedProblem makes a problem with judged cases worth the listed scores. The files
// are named but never read: the store only cares about the weights.
func seedProblem(t *testing.T, db *DB, pid string, scores ...int) Problem {
	t.Helper()
	p := mustProblem(t, db, pid, "题 "+pid)
	for i, score := range scores {
		tc := &Testcase{
			ProblemID: p.ID, Ordinal: i + 1, Name: fmt.Sprintf("c%d", i+1),
			Score: score, InputRel: "in", OutputRel: "out",
		}
		if _, err := db.AddTestcase(hctx, tc); err != nil {
			t.Fatalf("case %s/%d: %v", pid, i+1, err)
		}
	}
	return p
}

// seedSubmission writes one finished submission with its per-case rows, at a chosen
// moment. The board reads runs.score, so a test that wants a score has to write the
// rows the judge would have written rather than a summary number.
func seedSubmission(t *testing.T, db *DB, contestID, userID, problemID int64, verdict string, at int64, earned ...int) int64 {
	t.Helper()
	res, err := db.SQL.ExecContext(hctx,
		`INSERT INTO submissions (user_id, problem_id, contest_id, language, source, status, verdict,
		                         score, total_score, created_at, judged_at)
		 VALUES (?, ?, ?, 'cpp', 'int main(){}', 'done', ?, 0, 0, ?, ?)`,
		userID, problemID, contestID, verdict, at, at)
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("submission id: %v", err)
	}
	for i, points := range earned {
		caseVerdict := "AC"
		if points == 0 {
			caseVerdict = "WA"
		}
		_, err := db.SQL.ExecContext(hctx,
			`INSERT INTO runs (submission_id, ordinal, case_name, verdict, score, time_ms, mem_kb, is_sample)
			 VALUES (?, ?, ?, ?, ?, 10, 100, 0)`, id, i+1, fmt.Sprintf("c%d", i+1), caseVerdict, points)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
	}
	return id
}

func mustJoin(t *testing.T, db *DB, c Contest, userID int64, code, team string) ContestMember {
	t.Helper()
	mm, err := db.JoinContest(hctx, c.ID, userID, code, team)
	if err != nil {
		t.Fatalf("join %d: %v", userID, err)
	}
	return mm
}

func findStanding(t *testing.T, rows []StandingRow, name string) StandingRow {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no competitor %q in board: %+v", name, rows)
	return StandingRow{}
}

func cellOf(row StandingRow, label string) Cell {
	for _, c := range row.Cells {
		if c.Label == label {
			return c
		}
	}
	return Cell{}
}

func TestContestCreateFindAndRename(t *testing.T) {
	db := openTestDB(t)
	owner := mustUser(t, db, "boss")

	c := seedContest(t, db, &Contest{CID: "C0001", Title: "周赛", JoinCode: "abc123", OwnerID: owner.ID})
	if c.ID == 0 {
		t.Fatal("id not filled")
	}
	if c.CreatedAt != c.UpdatedAt {
		t.Errorf("a fresh row should not already be updated: %d %d", c.CreatedAt, c.UpdatedAt)
	}
	// Both the number and the readable label find the same row, and the label is
	// case-insensitive like a problem pid.
	byID, err := db.FindContest(hctx, fmt.Sprint(c.ID))
	if err != nil || byID.ID != c.ID {
		t.Fatalf("find by id = %+v %v", byID, err)
	}
	byCid, err := db.FindContest(hctx, "c0001")
	if err != nil || byCid.ID != c.ID {
		t.Fatalf("find by cid = %+v %v", byCid, err)
	}
	if byCid.JoinCode != "abc123" {
		t.Errorf("the management page needs the code back: %q", byCid.JoinCode)
	}

	if _, err := db.CreateContest(hctx, defaultContest(&Contest{CID: "C0001", Title: "撞名"})); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate cid = %v, want ErrConflict", err)
	}

	// A blank cid is filled from the row number, in the same statement, so two
	// people creating a contest at the same moment cannot claim the same label.
	auto := seedContest(t, db, &Contest{CID: "", Title: "自动编号", StartAt: 0, EndAt: 0})
	if auto.CID == "" || auto.CID[0] != 'C' {
		t.Errorf("auto cid = %q, want C-prefixed", auto.CID)
	}

	updated, err := db.UpdateContest(hctx, Contest{
		ID: c.ID, CID: "C9999", Title: "周赛（改）", Mode: ContestACM, TeamSize: 3,
		PenaltyMS: c.PenaltyMS, StartAt: c.StartAt, EndAt: c.EndAt, Visible: true,
		StandingOpen: true, JoinCode: c.JoinCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CID != "C9999" || updated.Title != "周赛（改）" || updated.Mode != ContestACM ||
		updated.TeamSize != 3 || !updated.StandingOpen {
		t.Errorf("update lost fields: %+v", updated)
	}

	if _, err := db.FindContest(hctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing contest = %v, want ErrNotFound", err)
	}
}

func TestContestPhaseComesFromTheClock(t *testing.T) {
	now := time.Now().UnixMilli()
	c := Contest{StartAt: now + 10*minuteMS, EndAt: now + 20*minuteMS}
	if got := c.Phase(now); got != PhaseUpcoming || got.Label() != "未开始" {
		t.Errorf("before the window: %v %q", got, got.Label())
	}
	if got := c.Phase(now + 15*minuteMS); got != PhaseRunning || got.Label() != "进行中" {
		t.Errorf("inside the window: %v %q", got, got.Label())
	}
	// The end instant belongs to the contest, so exactly end_at is already over.
	if got := c.Phase(now + 20*minuteMS); got != PhaseOver {
		t.Errorf("at end_at: %v", got)
	}
}

func TestContestListHidesTheJoinCode(t *testing.T) {
	db := openTestDB(t)
	secret := "sup3r-secret-code"
	hidden := seedHiddenContest(t, db, &Contest{CID: "H1", Title: "内部赛", JoinCode: secret})
	// A full-document write means a zero bool is a decision: the list only shows the
	// contest that says so.
	seedContest(t, db, &Contest{CID: "P1", Title: "公开赛", Visible: true, JoinCode: secret})
	p := seedProblem(t, db, "LP1", 1)
	if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: hidden.ID, ProblemID: p.ID}); err != nil {
		t.Fatal(err)
	}
	alice := mustUser(t, db, "watcher")
	mustJoin(t, db, hidden, alice.ID, secret, "")

	page, err := db.ListContests(hctx, 0, 50, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].CID != "P1" {
		t.Fatalf("staff=false should show only the visible one: %+v", page.Items)
	}
	if page.Items[0].JoinCode != "" {
		t.Errorf("the list leaked the join code: %q", page.Items[0].JoinCode)
	}

	all, err := db.ListContests(hctx, 0, 50, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 2 {
		t.Fatalf("staff list = %+v", all.Items)
	}
	for _, item := range all.Items {
		if item.JoinCode != "" {
			t.Errorf("the list read a join code for %s: the projection must not select it", item.CID)
		}
	}
	// The count columns come from one grouped query per table.
	got := findSummary(t, all.Items, "H1")
	if got.ProblemCount != 1 || got.MemberCount != 1 {
		t.Errorf("counts = %+v, want one problem and one member", got)
	}
	// Search matches the label as well as the title.
	found, err := db.ListContests(hctx, 0, 50, "h1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Items) != 1 || found.Items[0].CID != "H1" {
		t.Errorf("search by cid = %+v", found.Items)
	}
	// Paging hands back the cursor it would need next time.
	first, err := db.ListContests(hctx, 0, 1, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.NextAfter == 0 {
		t.Fatalf("first page = %+v next=%d", first.Items, first.NextAfter)
	}
	second, err := db.ListContests(hctx, first.NextAfter, 1, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID {
		t.Errorf("second page repeats the first: %+v", second.Items)
	}
}

func findSummary(t *testing.T, items []ContestSummary, cid string) ContestSummary {
	t.Helper()
	for _, s := range items {
		if s.CID == cid {
			return s
		}
	}
	t.Fatalf("no contest %s in %+v", cid, items)
	return ContestSummary{}
}

func TestContestProblemMounting(t *testing.T) {
	db := openTestDB(t)
	c := seedContest(t, db, &Contest{CID: "M1"})
	a := seedProblem(t, db, "MA", 1, 1)
	b := seedProblem(t, db, "MB", 2)
	cc := seedProblem(t, db, "MC", 3)

	first, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if first.Ordinal != 1 || ContestLabel(first.Label, first.Ordinal) != "A" {
		t.Errorf("first mount = %+v", first)
	}
	if first.TotalScore != 2 {
		t.Errorf("total score should ride along from the cases: %d", first.TotalScore)
	}
	second, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: b.ID, Points: 100})
	if err != nil {
		t.Fatal(err)
	}
	if second.Ordinal != 2 || second.Points != 100 {
		t.Errorf("second mount = %+v", second)
	}
	if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: a.ID}); !errors.Is(err, ErrConflict) {
		t.Errorf("mounting twice = %v, want ErrConflict", err)
	}

	list, err := db.ListContestProblems(hctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].PID != "MA" || list[1].PID != "MB" {
		t.Fatalf("order = %+v", list)
	}

	// Moving swaps ordinals without tripping the unique index.
	if err := db.MoveContestProblem(hctx, c.ID, b.ID, true); err != nil {
		t.Fatal(err)
	}
	after, err := db.ListContestProblems(hctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].PID != "MB" || after[1].PID != "MA" {
		t.Errorf("move up did not swap: %+v", after)
	}
	// Pushing the first problem further up is a no-op, not an error.
	if err := db.MoveContestProblem(hctx, c.ID, after[0].ProblemID, true); err != nil {
		t.Fatalf("boundary move = %v", err)
	}
	if again, _ := db.ListContestProblems(hctx, c.ID); again[0].PID != "MB" {
		t.Errorf("boundary move changed the order: %+v", again)
	}

	// A third problem appends after the two, so nobody's letter moves.
	third, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: cc.ID, Label: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Ordinal != 3 || ContestLabel(third.Label, third.Ordinal) != "X" {
		t.Errorf("custom label = %+v", third)
	}

	mounted, err := db.ContestProblemMounted(hctx, c.ID, a.ID)
	if err != nil || !mounted {
		t.Errorf("mounted = %v %v", mounted, err)
	}
	if ok, _ := db.ContestProblemMounted(hctx, c.ID, 9999); ok {
		t.Error("an unknown problem reports mounted")
	}
	if err := db.RemoveContestProblem(hctx, c.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveContestProblem(hctx, c.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double remove = %v", err)
	}
}

func TestJoinContestRefusesHiddenWithoutCode(t *testing.T) {
	db := openTestDB(t)
	u := mustUser(t, db, "hidden_joiner")
	c := seedHiddenContest(t, db, &Contest{CID: "J0", Title: "隐藏草稿"})
	if _, err := db.JoinContest(hctx, c.ID, u.ID, "", ""); !errors.Is(err, ErrContestHidden) {
		t.Fatalf("hidden contest without code = %v, want ErrContestHidden", err)
	}
}

func TestJoinContestGates(t *testing.T) {
	db := openTestDB(t)
	alice, bob := mustUser(t, db, "alice"), mustUser(t, db, "bob")

	open := seedContest(t, db, &Contest{CID: "J1", Title: "公开赛"})
	if _, err := db.JoinContest(hctx, open.ID, alice.ID, "", ""); err != nil {
		t.Fatalf("open contest needs no code: %v", err)
	}
	// Joining twice is a success, and returns the same roster row.
	again, err := db.JoinContest(hctx, open.ID, alice.ID, "", "")
	if err != nil {
		t.Fatalf("re-join = %v", err)
	}
	if again.Handle != "alice" {
		t.Errorf("re-join lost the row: %+v", again)
	}

	private := seedContest(t, db, &Contest{CID: "J2", Title: "私有赛", JoinCode: "s3cret", OwnerID: alice.ID})
	if _, err := db.JoinContest(hctx, private.ID, bob.ID, "wrong", ""); !errors.Is(err, ErrContestCode) {
		t.Errorf("wrong code = %v, want ErrContestCode", err)
	}
	if _, err := db.JoinContest(hctx, private.ID, bob.ID, "", ""); !errors.Is(err, ErrContestCode) {
		t.Errorf("empty code = %v, want ErrContestCode", err)
	}
	mm, err := db.JoinContest(hctx, private.ID, bob.ID, " s3cret ", "")
	if err != nil {
		t.Fatalf("right code: %v", err)
	}
	if mm.Role != "member" {
		t.Errorf("a plain joiner should not be owner: %+v", mm)
	}
	ownerRow, err := db.JoinContest(hctx, private.ID, alice.ID, "s3cret", "")
	if err != nil {
		t.Fatal(err)
	}
	if ownerRow.Role != "owner" {
		t.Errorf("the contest owner should carry role=owner: %+v", ownerRow)
	}

	// A contest that has not started yet accepts sign-ups; one that has ended does
	// not, because 赛后补进榜 would rewrite the field.
	later := seedContest(t, db, &Contest{CID: "J3", Title: "还没开始",
		StartAt: time.Now().UnixMilli() + 10*minuteMS, EndAt: time.Now().UnixMilli() + 30*minuteMS})
	if _, err := db.JoinContest(hctx, later.ID, bob.ID, "", ""); err != nil {
		t.Errorf("signing up early = %v, want success", err)
	}
	over := seedContest(t, db, &Contest{CID: "J4", Title: "已结束",
		StartAt: time.Now().UnixMilli() - 30*minuteMS, EndAt: time.Now().UnixMilli() - minuteMS})
	if _, err := db.JoinContest(hctx, over.ID, bob.ID, "", ""); !errors.Is(err, ErrContestClosed) {
		t.Errorf("joining after the end = %v, want ErrContestClosed", err)
	}

	if _, ok := db.ContestMemberOf(hctx, open.ID, bob.ID); ok {
		t.Error("bob is not in J1")
	}
	members, err := db.ListContestMembers(hctx, private.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("roster = %+v", members)
	}
}

func TestContestTeams(t *testing.T) {
	db := openTestDB(t)
	users := []User{mustUser(t, db, "solo"), mustUser(t, db, "two"), mustUser(t, db, "three")}

	single := seedContest(t, db, &Contest{CID: "T0", Title: "个人赛"})
	if _, err := db.JoinContest(hctx, single.ID, users[0].ID, "", "我想要一队"); !errors.Is(err, ErrTeamDisabled) {
		t.Errorf("a solo contest should refuse a team: %v", err)
	}

	team3 := seedContest(t, db, &Contest{CID: "T3", Title: "三人赛", TeamSize: 3})
	if _, err := db.JoinContest(hctx, team3.ID, users[0].ID, "", "红队"); err != nil {
		t.Fatal(err)
	}
	// Nobody named a team: they get one named after them, so the board has a row.
	joined, err := db.JoinContest(hctx, team3.ID, users[1].ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if joined.TeamID == 0 || joined.TeamName != "two" {
		t.Errorf("auto team = %+v", joined)
	}
	// Only a member can be put into a team.
	if err := db.SetTeam(hctx, team3.ID, users[2].ID, joined.TeamID); !errors.Is(err, ErrNotContestMember) {
		t.Errorf("teaming up a non-member = %v, want ErrNotContestMember", err)
	}
	// Two more people fill the red team, and the fourth is refused.
	red, err := db.FindTeam(hctx, team3.ID, "红队")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTeam(hctx, team3.ID, users[1].ID, red.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.JoinContest(hctx, team3.ID, users[2].ID, "", "红队"); err != nil {
		t.Fatal(err)
	}
	d, e := mustUser(t, db, "fourth"), mustUser(t, db, "fifth")
	if _, err := db.JoinContest(hctx, team3.ID, d.ID, "", "红队"); !errors.Is(err, ErrTeamFull) {
		t.Errorf("fourth into a 3-person team = %v, want ErrTeamFull", err)
	}
	if _, err := db.JoinContest(hctx, team3.ID, e.ID, "", "红队"); !errors.Is(err, ErrTeamFull) {
		t.Errorf("fifth = %v, want ErrTeamFull", err)
	}
	if _, err := db.CreateTeam(hctx, team3.ID, "红队"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate team name = %v, want ErrConflict", err)
	}
	// Team names are matched case-insensitively, like handles.
	if _, err := db.CreateTeam(hctx, team3.ID, "RED"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FindTeam(hctx, team3.ID, "red"); err != nil {
		t.Errorf("FindTeam should be case-insensitive: %v", err)
	}

	roster, err := TeamRosterOf(db, team3.ID)
	if err != nil {
		t.Fatal(err)
	}
	var redRow *ContestRoster
	for i := range roster {
		if roster[i].Name == "红队" {
			redRow = &roster[i]
		}
	}
	if redRow == nil || len(redRow.UserIDs) != 3 {
		t.Fatalf("red team roster = %+v", roster)
	}

	// Leaving drops the person, and an empty team goes with them.
	if err := db.LeaveContest(hctx, team3.ID, users[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.ContestMemberOf(hctx, team3.ID, users[1].ID); ok {
		t.Error("still a member after leaving")
	}
	if err := db.LeaveContest(hctx, team3.ID, users[1].ID); !errors.Is(err, ErrNotContestMember) {
		t.Errorf("leaving twice = %v", err)
	}

	// Stepping out of a team puts the person back to solo, and deletes the team if
	// they were the last one in it.
	lonely := seedContest(t, db, &Contest{CID: "T9", Title: "只剩一人", TeamSize: 2})
	nine := mustUser(t, db, "nine")
	row, err := db.JoinContest(hctx, lonely.ID, nine.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTeam(hctx, lonely.ID, nine.ID, 0); err != nil {
		t.Fatal(err)
	}
	teams, err := db.ListTeams(hctx, lonely.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 0 {
		t.Errorf("the empty auto team should be gone: %+v", teams)
	}
	if mm, _ := db.ContestMemberOf(hctx, lonely.ID, nine.ID); mm.TeamID != 0 || mm.TeamName != "" {
		t.Errorf("member should be solo again: %+v", mm)
	}
	_ = row
}

// TeamRosterOf is the roster grouping on its own, so tests and pages read the same
// competitor list as the board.
func TeamRosterOf(db *DB, contestID int64) ([]ContestRoster, error) {
	members, err := db.ListContestMembers(hctx, contestID)
	if err != nil {
		return nil, err
	}
	return TeamRoster(members), nil
}

// TestScoreboardIOI is the 取最高分 + 比用时 rule, which is IOI's, not OI's.
func TestScoreboardIOI(t *testing.T) {
	db := openTestDB(t)
	c := seedContest(t, db, &Contest{CID: "S1", Title: "IOI 赛", Mode: ContestIOI})
	a, b, d := mustUser(t, db, "aa"), mustUser(t, db, "bb"), mustUser(t, db, "dd")
	p1 := seedProblem(t, db, "S1A", 10)   // one case worth 10
	p2 := seedProblem(t, db, "S1B", 5, 5) // a bundle: two cases of 5
	for _, p := range []Problem{p1, p2} {
		if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []User{a, b, d} {
		mustJoin(t, db, c, u.ID, "", "")
	}

	start := c.StartAt
	// aa: full marks on the first problem, then a broken bundle on the second.
	seedSubmission(t, db, c.ID, a.ID, p1.ID, "AC", start+minuteMS, 10)
	seedSubmission(t, db, c.ID, a.ID, p2.ID, "WA", start+2*minuteMS, 5, 0)
	// bb: same total as aa but later, so the tie-break has to read the times.
	seedSubmission(t, db, c.ID, b.ID, p1.ID, "AC", start+9*minuteMS, 10)
	seedSubmission(t, db, c.ID, b.ID, p2.ID, "AC", start+11*minuteMS, 5, 5)
	// dd: under IOI a worse second submission must not replace the best one.
	seedSubmission(t, db, c.ID, d.ID, p1.ID, "AC", start+minuteMS, 10)
	seedSubmission(t, db, c.ID, d.ID, p1.ID, "WA", start+3*minuteMS, 0)

	rows, err := db.ContestScoreboard(hctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("board = %+v", rows)
	}
	aa := findStanding(t, rows, "aa")
	if aa.Score != 15 {
		t.Errorf("aa score = %d, want 15 (the half-passed bundle pays 5)", aa.Score)
	}
	if cell := cellOf(aa, "B"); cell.Points != 5 || cell.Max != 10 || cell.Attempts != 1 {
		t.Errorf("aa bundle cell = %+v", cell)
	}
	bb := findStanding(t, rows, "bb")
	if bb.Score != 20 || bb.Rank != 1 {
		t.Errorf("bb = %+v, want rank 1 on 20 points", bb)
	}
	dd := findStanding(t, rows, "dd")
	if dd.Score != 10 {
		t.Errorf("IOI keeps the best submission: %+v", dd)
	}
	if cell := cellOf(dd, "A"); cell.Attempts != 2 {
		t.Errorf("dd attempts = %+v, want 2", cell)
	}
	// A competitor who never touched a problem still gets a row, with empty cells.
	if cell := cellOf(dd, "B"); cell.Seen {
		t.Errorf("dd should not have tried B: %+v", cell)
	}
	if rows[0].Rank != 1 || rows[1].Rank == rows[0].Rank {
		t.Errorf("ranks not written: %+v", rows)
	}

	// An override rescales proportionally: A worth 10 becomes 30.
	if _, err := db.UpdateContestProblem(hctx, ContestProblem{
		ContestID: c.ID, ProblemID: p1.ID, Points: 30,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err = db.ContestScoreboard(hctx, c)
	if err != nil {
		t.Fatal(err)
	}
	aa = findStanding(t, rows, "aa")
	if aa.Score != 35 {
		t.Errorf("with a 30-point override aa = %d, want 30+5", aa.Score)
	}
	if cell := cellOf(aa, "A"); cell.Max != 30 || cell.Points != 30 {
		t.Errorf("overridden cell = %+v", cell)
	}

	// Unmounting a problem does not erase what it earned: the history keeps paying,
	// in a column the board marks 已移除.
	if err := db.RemoveContestProblem(hctx, c.ID, p2.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = db.ContestScoreboard(hctx, c)
	if err != nil {
		t.Fatal(err)
	}
	aa = findStanding(t, rows, "aa")
	if aa.Score != 35 {
		t.Errorf("removed problems must still count: %+v", aa)
	}
	var removed Cell
	for _, cell := range aa.Cells {
		if cell.Removed {
			removed = cell
		}
	}
	if removed.Points != 5 || removed.PID != "S1B" {
		t.Errorf("removed column lost its history: %+v", aa.Cells)
	}

	// Self standing finds the same rank the full board gives.
	row, ok, err := db.ContestSelfStanding(hctx, c, d.ID)
	if err != nil || !ok {
		t.Fatalf("self standing = %v %v", ok, err)
	}
	if row.Name != "dd" || row.Rank != findStanding(t, rows, "dd").Rank {
		t.Errorf("self standing = %+v", row)
	}
}

func TestScoreboardACM(t *testing.T) {
	db := openTestDB(t)
	c := seedContest(t, db, &Contest{CID: "AC1", Title: "ACM 赛", Mode: ContestACM, PenaltyMS: 20 * minuteMS})
	a, b := mustUser(t, db, "solver"), mustUser(t, db, "slow")
	p1 := seedProblem(t, db, "AC1A", 1)
	p2 := seedProblem(t, db, "AC1B", 1)
	for _, p := range []Problem{p1, p2} {
		if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []User{a, b} {
		mustJoin(t, db, c, u.ID, "", "")
	}
	start := c.StartAt
	// solver: a compile error (no penalty), a WA (20 minutes), then AC at +35.
	seedSubmission(t, db, c.ID, a.ID, p1.ID, "CE", start+minuteMS)
	seedSubmission(t, db, c.ID, a.ID, p1.ID, "WA", start+5*minuteMS)
	seedSubmission(t, db, c.ID, a.ID, p1.ID, "AC", start+35*minuteMS)
	// A judge failure is not an attempt and is not penalised.
	seedSubmission(t, db, c.ID, a.ID, p2.ID, "IE", start+2*minuteMS)
	seedSubmission(t, db, c.ID, a.ID, p2.ID, "AC", start+10*minuteMS)
	// slow solves one problem first try, so it should lead on penalty.
	seedSubmission(t, db, c.ID, b.ID, p2.ID, "AC", start+minuteMS)

	rows, err := db.ContestScoreboard(hctx, c)
	if err != nil {
		t.Fatal(err)
	}
	solver := findStanding(t, rows, "solver")
	if solver.Solved != 2 {
		t.Errorf("solver = %+v", solver)
	}
	// p1: 35 minutes + one WA penalty of 20; p2: 10 minutes, no penalty, CE/IE free.
	if want := int64(65 * minuteMS); solver.TimeMS != want {
		t.Errorf("solver penalty = %d, want %d (CE and IE must not count)", solver.TimeMS, want)
	}
	cell := cellOf(solver, "A")
	if !cell.Solved || cell.Attempts != 3 || cell.TimeMS != 35*minuteMS {
		t.Errorf("solver cell A = %+v", cell)
	}
	if cell := cellOf(solver, "B"); cell.Attempts != 1 || !cell.Solved {
		t.Errorf("the IE row must not count as an attempt: %+v", cell)
	}
	slow := findStanding(t, rows, "slow")
	if slow.Solved != 1 || slow.Rank != 2 {
		t.Errorf("two solves beat one: %+v", slow)
	}
	if cell := cellOf(slow, "A"); cell.Solved || cell.Attempts != 0 {
		t.Errorf("slow never touched A: %+v", cell)
	}

	// An unsolved problem still shows its attempts.
	c2 := mustUser(t, db, "stuck")
	mustJoin(t, db, c, c2.ID, "", "")
	seedSubmission(t, db, c.ID, c2.ID, p1.ID, "WA", start+minuteMS, 0)
	seedSubmission(t, db, c.ID, c2.ID, p1.ID, "TLE", start+2*minuteMS, 0)
	rows, err = db.ContestScoreboard(hctx, c)
	if err != nil {
		t.Fatal(err)
	}
	stuck := findStanding(t, rows, "stuck")
	if stuck.Solved != 0 || stuck.Rank != 3 {
		t.Errorf("stuck = %+v", stuck)
	}
	if cell := cellOf(stuck, "A"); cell.Attempts != 2 || cell.Solved || cell.TimeMS != 0 {
		t.Errorf("unsolved cell keeps attempts only: %+v", cell)
	}
}

func TestScoreboardTeamMode(t *testing.T) {
	db := openTestDB(t)
	// 队内每题取最优是 IOI 的折叠方式；OI 的「最后一次」在队里不成立，所以这场用 IOI。
	c := seedContest(t, db, &Contest{CID: "TM", Title: "团队赛", Mode: ContestIOI, TeamSize: 2})
	a, b, e := mustUser(t, db, "alpha"), mustUser(t, db, "bravo"), mustUser(t, db, "echo")
	p1 := seedProblem(t, db, "TMA", 10)
	p2 := seedProblem(t, db, "TMB", 10)
	for _, p := range []Problem{p1, p2} {
		if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}
	mustJoin(t, db, c, a.ID, "", "红队")
	mustJoin(t, db, c, b.ID, "", "红队")
	mustJoin(t, db, c, e.ID, "", "蓝队")

	start := c.StartAt
	// 红队: each half solves a different problem; the team takes the best of both.
	seedSubmission(t, db, c.ID, a.ID, p1.ID, "AC", start+minuteMS, 10)
	seedSubmission(t, db, c.ID, b.ID, p2.ID, "AC", start+2*minuteMS, 10)
	seedSubmission(t, db, c.ID, b.ID, p1.ID, "WA", start+3*minuteMS, 0)
	// 蓝队 only gets half of one problem.
	seedSubmission(t, db, c.ID, e.ID, p1.ID, "WA", start+minuteMS, 4, 0)

	rows, err := db.ContestScoreboard(hctx, c)
	if err != nil {
		t.Fatal(err)
	}
	red := findStanding(t, rows, "红队")
	if red.Score != 20 || red.Rank != 1 {
		t.Errorf("red = %+v, want 20 points as one competitor", red)
	}
	if len(red.Handles) != 2 {
		t.Errorf("team row should list its members: %+v", red.Handles)
	}
	if cell := cellOf(red, "A"); cell.Attempts != 2 || cell.Points != 10 {
		t.Errorf("team attempts are summed: %+v", cell)
	}
	blue := findStanding(t, rows, "蓝队")
	if blue.Score != 4 {
		t.Errorf("blue = %+v", blue)
	}
}

func TestDeleteContestKeepsHistory(t *testing.T) {
	db := openTestDB(t)
	c := seedContest(t, db, &Contest{CID: "D1", Title: "要删掉的比赛"})
	a := mustUser(t, db, "remember")
	p := seedProblem(t, db, "D1A", 1)
	if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: p.ID}); err != nil {
		t.Fatal(err)
	}
	mustJoin(t, db, c, a.ID, "", "")
	if _, err := db.CreateTeam(hctx, c.ID, "留个队"); err != nil {
		t.Fatal(err)
	}
	sid := seedSubmission(t, db, c.ID, a.ID, p.ID, "AC", time.Now().UnixMilli(), 1)

	if err := db.DeleteContest(hctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FindContest(hctx, "D1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("contest still there: %v", err)
	}
	for table, q := range map[string]string{
		"contest_problems": `SELECT COUNT(*) FROM contest_problems WHERE contest_id=?`,
		"contest_members":  `SELECT COUNT(*) FROM contest_members WHERE contest_id=?`,
		"contest_teams":    `SELECT COUNT(*) FROM contest_teams WHERE contest_id=?`,
	} {
		var n int
		if err := db.SQL.QueryRowContext(hctx, q, c.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s rows survived the delete: %d", table, n)
		}
	}
	// The submission stays, and its contest id points at nothing: the page has to
	// say 比赛已删除 rather than 404.
	var contestID int64
	if err := db.SQL.QueryRowContext(hctx, `SELECT contest_id FROM submissions WHERE id=?`, sid).Scan(&contestID); err != nil {
		t.Fatal(err)
	}
	if contestID != c.ID {
		t.Errorf("submission lost its attribution: %d", contestID)
	}
	d, err := db.SubmissionDetail(hctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if d.ContestID != c.ID || d.ContestCID != "" {
		t.Errorf("detail should know the contest is gone: %+v", d)
	}
}

func TestContestSubmissionAttribution(t *testing.T) {
	db := openTestDB(t)
	c := seedContest(t, db, &Contest{CID: "AT", Title: "归属"})
	a := mustUser(t, db, "author")
	p := seedProblem(t, db, "ATA", 1)
	sub, err := db.CreateSubmission(hctx, &Submission{
		UserID: a.ID, ProblemID: p.ID, ContestID: c.ID, Language: "cpp", Source: "int main(){}",
	})
	if err != nil {
		t.Fatal(err)
	}
	back, err := db.GetSubmission(hctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.ContestID != c.ID {
		t.Errorf("contest id not stored: %+v", back)
	}
	if d, err := db.SubmissionDetail(hctx, sub.ID); err != nil || d.ContestCID != "AT" {
		t.Errorf("detail join lost the cid: %+v %v", d, err)
	}
	// A practice submission still reads as contest 0.
	practice, err := db.CreateSubmission(hctx, &Submission{
		UserID: a.ID, ProblemID: p.ID, Language: "cpp", Source: "int main(){}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetSubmission(hctx, practice.ID); got.ContestID != 0 {
		t.Errorf("practice submission got a contest: %+v", got)
	}
}

// TestScoreboardOIisLastSubmission is the CSP/NOIP/NOI rule: the last submission is
// the one you are scored on, equal totals share a rank, and the board has no clock.
func TestScoreboardOIisLastSubmission(t *testing.T) {
	db := openTestDB(t)
	c := seedContest(t, db, &Contest{CID: "OI1", Title: "NOIP 风格赛", Mode: ContestOI})
	broke := mustUser(t, db, "oi_broke")
	mid := mustUser(t, db, "oi_mid")
	midLate := mustUser(t, db, "oi_mid_late")
	full := mustUser(t, db, "oi_full")
	ie := mustUser(t, db, "oi_ie")
	p1 := seedProblem(t, db, "OI1A", 10)
	p2 := seedProblem(t, db, "OI1B", 5, 5)
	for _, p := range []Problem{p1, p2} {
		if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []User{broke, mid, midLate, full, ie} {
		mustJoin(t, db, c, u.ID, "", "")
	}
	start := c.StartAt
	// oi_broke solved A and then broke it: the last submission is the one scored.
	seedSubmission(t, db, c.ID, broke.ID, p1.ID, "AC", start+minuteMS, 10)
	seedSubmission(t, db, c.ID, broke.ID, p1.ID, "WA", start+2*minuteMS, 0)
	seedSubmission(t, db, c.ID, broke.ID, p2.ID, "AC", start+3*minuteMS, 5, 5)
	// These two land on the same 15, an hour apart: OI must not use the clock.
	seedSubmission(t, db, c.ID, mid.ID, p1.ID, "AC", start+4*minuteMS, 10)
	seedSubmission(t, db, c.ID, mid.ID, p2.ID, "WA", start+5*minuteMS, 5, 0)
	seedSubmission(t, db, c.ID, midLate.ID, p1.ID, "AC", start+59*minuteMS, 10)
	seedSubmission(t, db, c.ID, midLate.ID, p2.ID, "WA", start+60*minuteMS, 5, 0)
	seedSubmission(t, db, c.ID, full.ID, p1.ID, "AC", start+61*minuteMS, 10)
	seedSubmission(t, db, c.ID, full.ID, p2.ID, "AC", start+62*minuteMS, 5, 5)
	// oi_ie: the judge broke down after the good answer. An IE is not "the last
	// submission" — it must not erase the 10 that was already earned.
	seedSubmission(t, db, c.ID, ie.ID, p1.ID, "AC", start+63*minuteMS, 10)
	seedSubmission(t, db, c.ID, ie.ID, p1.ID, "IE", start+64*minuteMS)
	seedSubmission(t, db, c.ID, ie.ID, p2.ID, "WA", start+65*minuteMS, 0, 0)

	rows, err := db.ContestScoreboard(hctx, c)
	if err != nil {
		t.Fatal(err)
	}
	bad := findStanding(t, rows, "oi_broke")
	if bad.Score != 10 {
		t.Errorf("the last submission must replace the good one: %+v", bad)
	}
	if cell := cellOf(bad, "A"); cell.Points != 0 || !cell.Seen || cell.Max != 10 || cell.Attempts != 2 {
		t.Errorf("cell A should read the failed last try, with two attempts: %+v", cell)
	}
	if cell := cellOf(bad, "B"); cell.Points != 10 || !cell.Solved {
		t.Errorf("cell B is a clean 10: %+v", cell)
	}
	a, b := findStanding(t, rows, "oi_mid"), findStanding(t, rows, "oi_mid_late")
	if a.Score != 15 || b.Score != 15 || a.Rank != b.Rank {
		t.Errorf("equal totals must share a rank: %+v / %+v", a, b)
	}
	if findStanding(t, rows, "oi_full").Rank != 1 {
		t.Errorf("the only full score should lead: %+v", rows)
	}
	// 判机故障既不把你的分数清零，也不算一次尝试。
	got := findStanding(t, rows, "oi_ie")
	if got.Score != 10 {
		t.Errorf("an IE must not erase an OI score: %+v", got)
	}
	if cell := cellOf(got, "A"); cell.Attempts != 1 || cell.Points != 10 {
		t.Errorf("the IE row should be invisible to the board: %+v", cell)
	}
	// 用时不参与 OI：既不算也不排。
	for _, r := range rows {
		if r.TimeMS != 0 {
			t.Errorf("%s accumulated %d ms on an OI board", r.Name, r.TimeMS)
		}
		for _, cell := range r.Cells {
			if cell.TimeMS != 0 {
				t.Errorf("%s/%s carries a clock: %+v", r.Name, cell.Label, cell)
			}
		}
	}
	// 并列之后的名次要跳过，和真实成绩表一样。
	if rows[0].Rank != 1 || rows[1].Rank != 2 || rows[2].Rank != 2 || rows[3].Rank != 4 {
		t.Errorf("ranks = %d %d %d %d, want 1 2 2 4", rows[0].Rank, rows[1].Rank, rows[2].Rank, rows[3].Rank)
	}
}

// TestContestModeCheckAfterTheRebuild guards 0015: those four tables were rebuilt to
// widen the mode CHECK, so the children's foreign keys have to point at the live
// tables afterwards, and the CHECK still has to bite.
func TestContestModeCheckAfterTheRebuild(t *testing.T) {
	db := openTestDB(t)
	for _, mode := range []string{ContestOI, ContestIOI, ContestACM} {
		c := seedContest(t, db, &Contest{CID: "M" + mode, Title: "赛制 " + mode, Mode: mode})
		if got, _ := db.GetContestByID(hctx, c.ID); got.Mode != mode {
			t.Errorf("mode %q did not round-trip: %+v", mode, got)
		}
	}
	_, err := db.SQL.ExecContext(hctx,
		`INSERT INTO contests (cid, title, statement, start_at, end_at, mode, join_code,
		                      standing_open, visible, team_size, penalty_ms, created_at, updated_at)
		 VALUES ('X1', '不认识的赛制', '', 1, 2, 'swiss', '', 0, 1, 1, 0, 1, 1)`)
	if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("a fourth mode slipped through the CHECK: %v", err)
	}

	// The cascade is the proof the rename kept the children attached: dropping the
	// parent must take the roster, the teams and the mount table with it.
	c := seedContest(t, db, &Contest{CID: "CASC", Title: "级联检查", TeamSize: 2})
	u := mustUser(t, db, "cascade_one")
	p := seedProblem(t, db, "CASC1", 1)
	if _, err := db.AddContestProblem(hctx, &ContestProblem{ContestID: c.ID, ProblemID: p.ID}); err != nil {
		t.Fatal(err)
	}
	mustJoin(t, db, c, u.ID, "", "级联队")
	if err := db.DeleteContest(hctx, c.ID); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"contest_problems": `SELECT COUNT(*) FROM contest_problems`,
		"contest_teams":    `SELECT COUNT(*) FROM contest_teams`,
		"contest_members":  `SELECT COUNT(*) FROM contest_members`,
	} {
		var n int
		if err := db.SQL.QueryRowContext(hctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n != 0 {
			t.Errorf("%s kept %d rows after the parent was dropped", name, n)
		}
	}
	// foreign_key_check is the direct question: any orphan left by the rebuild shows here.
	rows, err := db.SQL.QueryContext(hctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var broken []string
	for rows.Next() {
		var (
			table, index    string
			parentID, rowID int64
		)
		if err := rows.Scan(&table, &rowID, &parentID, &index); err != nil {
			t.Fatal(err)
		}
		broken = append(broken, table)
	}
	if len(broken) != 0 {
		t.Errorf("foreign_key_check found orphans in %v", broken)
	}
}
