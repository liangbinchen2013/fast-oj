package store

import (
	"context"
	"testing"
)

// 判机口径变了，旧提交不会自己改判（见 0036）。重判就是管理员那一下补救：退回队列、
// 把上一次的结果清干净，判机重跑后按今天的口径重写。
//
// 这一条测的是"清干净"到底清到了什么程度——留着任何一份旧结果，页面上就还会有一句
// 上个判机说的谎（"判于 3 小时前"、"C++17 · O2 开"、那条已经不属于这次判定的判机信息）。
func TestRejudgeWipesThePreviousAttempt(t *testing.T) {
	db, u, p, j := queueFixture(t)
	ctx := context.Background()
	s := submitSomething(t, db, u, p, "x")
	if _, err := db.ClaimNext(ctx, j.ID, CapsAll); err != nil {
		t.Fatal(err)
	}
	runs := []Run{{Ordinal: 1, CaseName: "sample1", Verdict: "AC", TimeMS: 3, MemKB: 4096, IsSample: true},
		{Ordinal: 2, CaseName: "1", Verdict: "WA", TimeMS: 7, MemKB: 8192, TimeMin: true, MemMin: true}}
	if err := db.RecordResult(ctx, s.ID, Result{
		Verdict: "WA", TimeMS: 7, MemKB: 8192, TimeMin: true, MemMin: true,
		Score: 0, TotalScore: 100, FailedCase: "1", Detail: "line 1: got [4] want [3]",
		Toolchain: "C++17 · O2 开",
	}, runs); err != nil {
		t.Fatal(err)
	}

	if ok, err := db.Rejudge(ctx, s.ID); err != nil || !ok {
		t.Fatalf("重判一条已完成的提交 = %v %v，想要成功", ok, err)
	}
	row, err := db.GetSubmission(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "queued" || row.Verdict != "" || row.Score != 0 || row.TimeMS != -1 || row.MemKB != -1 {
		t.Errorf("重判后还留着旧判定：%+v", row)
	}
	if row.TimeMin || row.MemMin {
		t.Errorf("旧那次的「下界」标记没清：%+v", row)
	}
	if row.FailedCase != "" || row.JudgeID != 0 || row.StartedAt != 0 || row.JudgedAt != 0 {
		t.Errorf("重判后还在替上一次判机报话：%+v", row)
	}
	if _, detail, err := db.SubmissionPayload(ctx, s.ID); err != nil || detail != "" {
		t.Errorf("判机信息没清干净：%q %v", detail, err)
	}
	if row.Toolchain != "" {
		t.Errorf("口径那一格还写着上一台判机说的 %q", row.Toolchain)
	}
	// 逐用例行属于那一次已经不算数的运行。
	if got, err := db.ListRuns(ctx, s.ID); err != nil || len(got) != 0 {
		t.Errorf("逐用例没清掉：%+v %v", got, err)
	}
	// total_score 是题目的分值表，不是判机的产出，重判不该动它。
	if row.TotalScore != 100 {
		t.Errorf("题目总分被改了：%d", row.TotalScore)
	}

	// 退回队列的提交重新可领：判机照旧按 id 取最老的那条 queued。
	claimed, err := db.ClaimNext(ctx, j.ID, CapsAll)
	if err != nil || claimed.SubmissionID != s.ID {
		t.Fatalf("重判后领不到单：%+v %v", claimed, err)
	}
}

// 重判不是"再来一次"的按钮：已经在队列里的那条没有上一次可清，重复点它必须是空操作，
// 不存在的 id 也不能凭空造出一条 queued。
func TestRejudgeRefusesRowsWithNothingToClear(t *testing.T) {
	db, u, p, _ := queueFixture(t)
	ctx := context.Background()

	fresh := submitSomething(t, db, u, p, "x")
	if ok, err := db.Rejudge(ctx, fresh.ID); err != nil || ok {
		t.Errorf("重判一条 queued = %v %v，想要空操作", ok, err)
	}
	if ok, err := db.Rejudge(ctx, fresh.ID+9999); err != nil || ok {
		t.Errorf("重判不存在的 id = %v %v，想要空操作", ok, err)
	}
	if pending, err := db.PendingCount(ctx); err != nil || pending != 1 {
		t.Errorf("待判数被改动了：%d %v", pending, err)
	}
}

// 重判一条**正在被判**的提交是这项功能最危险的一下：那台判机还在跑，它跑完来报的那一句
// 必须回不来。靠的是重判把 judge_id 清成 NULL——ReportProgress 的归属与状态两道 CAS 都够它拒绝。
// （更上层的 Transition 拒绝在 api 侧测：TestRejudgedSubmissionIgnoresTheOldJudge。）
func TestRejudgeDuringAFlightRunRejectsTheOldJudge(t *testing.T) {
	db, u, p, j := queueFixture(t)
	ctx := context.Background()
	s := submitSomething(t, db, u, p, "x")
	if _, err := db.ClaimNext(ctx, j.ID, CapsAll); err != nil {
		t.Fatal(err)
	}

	if ok, err := db.Rejudge(ctx, s.ID); err != nil || !ok {
		t.Fatalf("重判一条 compiling = %v %v，想要成功", ok, err)
	}
	if ok, err := db.ReportStatus(ctx, s.ID, j.ID, "compiling", "running", "stale"); err != nil || ok {
		t.Errorf("旧判机的回报竟然被收下了：%v %v", ok, err)
	}
	if ok, err := db.ReportProgress(ctx, s.ID, j.ID, "running", "running", "stale",
		&Run{Ordinal: 1, CaseName: "1", Verdict: "AC", TimeMS: 1, MemKB: 1}); err != nil || ok {
		t.Errorf("旧判机带着逐用例回报进来了：%v %v", ok, err)
	}
	row, err := db.GetSubmission(ctx, s.ID)
	if err != nil || row.Status != "queued" || row.Verdict != "" {
		t.Fatalf("旧判机改写了这一行：%+v %v", row, err)
	}
	if got, err := db.ListRuns(ctx, s.ID); err != nil || len(got) != 0 {
		t.Errorf("旧判机写下了用例行：%+v %v", got, err)
	}
}

// 列表按状态筛是管理员找"停在队列里/判机超时"的入口；认不出的值只能得到空列表，
// 不能因为参数没匹配上就退化成"看全部"。
func TestListSubmissionDetailsFiltersByStatus(t *testing.T) {
	db, u, p, _ := queueFixture(t)
	ctx := context.Background()
	waiting := submitSomething(t, db, u, p, "x")
	judged := submitSomething(t, db, u, p, "y")
	// 直接落一条已判完的：这一条测的是筛选项，不走领单那一步，免得把 waiting 那条一起带走。
	if err := db.RecordResult(ctx, judged.ID, Result{Verdict: "AC", TimeMS: 1, MemKB: 1, Score: 1, TotalScore: 1}, nil); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListSubmissionDetails(ctx, DetailFilter{Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("不加筛选项 = %+v %v", all, err)
	}
	done, err := db.ListSubmissionDetails(ctx, DetailFilter{Limit: 10, Status: "done"})
	if err != nil || len(done) != 1 || done[0].ID != judged.ID {
		t.Fatalf("筛 done = %+v %v", done, err)
	}
	queued, err := db.ListSubmissionDetails(ctx, DetailFilter{Limit: 10, Status: "queued"})
	if err != nil || len(queued) != 1 || queued[0].ID != waiting.ID {
		t.Fatalf("筛 queued = %+v %v", queued, err)
	}
	// 与用户筛选项叠加时是"且"，不是各查各的。
	both, err := db.ListSubmissionDetails(ctx, DetailFilter{Limit: 10, UserID: u.ID, Status: "done"})
	if err != nil || len(both) != 1 || both[0].ID != judged.ID {
		t.Fatalf("筛 user+done = %+v %v", both, err)
	}
	none, err := db.ListSubmissionDetails(ctx, DetailFilter{Limit: 10, Status: "not-a-status"})
	if err != nil || len(none) != 0 {
		t.Fatalf("乱填的状态值 = %+v %v，想要空列表", none, err)
	}
}
