package store

import (
	"context"
	"testing"
	"time"
)

// PublicView 是公开列表/个人主页共用的可见性闸门：隐藏题与进行中 OI 的别人成绩不能出现。
func TestListSubmissionDetailsHonoursPublicView(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	u1 := mustUser(t, db, "author")
	u2 := mustUser(t, db, "contestant")

	visible, err := db.CreateProblem(ctx, &Problem{
		PID: "P1", Title: "visible", TimeLimitMS: 1000, MemLimitKB: 262144, Visible: true, OwnerID: u1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := db.CreateProblem(ctx, &Problem{
		PID: "P2", Title: "hidden", TimeLimitMS: 1000, MemLimitKB: 262144, Visible: false, OwnerID: u1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	contest, err := db.CreateContest(ctx, &Contest{
		CID: "RUNNING", Title: "running OI", StartAt: now - 60_000, EndAt: now + 60_000,
		Mode: ContestOI, Visible: true, TeamSize: 1, OwnerID: u1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddContestProblem(ctx, &ContestProblem{ContestID: contest.ID, ProblemID: visible.ID}); err != nil {
		t.Fatal(err)
	}

	subs := []Submission{
		{UserID: u1.ID, ProblemID: visible.ID, Language: "cpp", Source: "int main(){}", TotalScore: 10},
		{UserID: u2.ID, ProblemID: visible.ID, ContestID: contest.ID, Language: "cpp", Source: "int main(){}", TotalScore: 10},
		{UserID: u2.ID, ProblemID: hidden.ID, Language: "cpp", Source: "int main(){}", TotalScore: 10},
	}
	for i := range subs {
		if _, err := db.CreateSubmission(ctx, &subs[i]); err != nil {
			t.Fatal(err)
		}
	}

	ids := func(rows []SubmissionDetail) map[int64]bool {
		out := map[int64]bool{}
		for _, row := range rows {
			out[row.ID] = true
		}
		return out
	}
	public, err := db.ListSubmissionDetails(ctx, DetailFilter{PublicView: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(public); len(got) != 1 || !got[subs[0].ID] {
		t.Fatalf("匿名公开列表 = %v，只该有非比赛的公开题提交", got)
	}

	own, err := db.ListSubmissionDetails(ctx, DetailFilter{PublicView: true, ViewerID: u2.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(own); len(got) != 3 {
		t.Fatalf("参赛者看不到自己的两条提交或公开题提交：%v", got)
	}

	author, err := db.ListSubmissionDetails(ctx, DetailFilter{PublicView: true, ViewerID: u1.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(author); len(got) != 3 {
		t.Fatalf("题主看不到自己隐藏题上的提交：%v", got)
	}

	all, err := db.ListSubmissionDetails(ctx, DetailFilter{PublicView: true, SeeAll: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); len(got) != 3 {
		t.Fatalf("全见权限没有看到全部：%v", got)
	}

	// 已通过题单也走同一套规则：匿名看隐藏题/进行中 OI 的别人成绩时，题目标题与编号
	// 都不该出现；本人和题主仍能看到。
	for _, sub := range subs {
		if _, err := db.SQL.ExecContext(ctx,
			`UPDATE submissions SET status='done', verdict='AC', judged_at=? WHERE id=?`, now, sub.ID); err != nil {
			t.Fatal(err)
		}
	}
	solved, err := db.SolvedProblems(ctx, u2.ID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(solved) != 0 {
		t.Fatalf("匿名看到的已通过题单 = %+v，隐藏题与进行中 OI 都该消失", solved)
	}
	ownSolved, err := db.SolvedProblems(ctx, u2.ID, u2.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownSolved) != 2 {
		t.Fatalf("本人看不到自己的题单：%+v", ownSolved)
	}
}
