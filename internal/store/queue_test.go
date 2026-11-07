package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func queueFixture(t *testing.T) (*DB, User, Problem, Judge) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	u := mustUser(t, db, "alice")
	p := mustProblem(t, db, "P1", "Queue problem")
	j, err := db.UpsertJudge(ctx, "box-1", []string{"cpp"}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return db, u, p, j
}

func submitSomething(t *testing.T, db *DB, u User, p Problem, source string) Submission {
	t.Helper()
	s, err := db.CreateSubmission(context.Background(), &Submission{
		UserID: u.ID, ProblemID: p.ID, Language: "cpp", Source: source, TotalScore: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClaimNextIsFifoAndStampsOwner(t *testing.T) {
	ctx := context.Background()
	db, u, p, j := queueFixture(t)
	j2, err := db.UpsertJudge(ctx, "box-2", nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	first := submitSomething(t, db, u, p, "int main(){}")
	second := submitSomething(t, db, u, p, "int main(){return 1;}")

	task, err := db.ClaimNext(ctx, j.ID, CapsAll)
	if err != nil {
		t.Fatal(err)
	}
	if task.SubmissionID != first.ID || task.ProblemID != p.ID || task.PID != "P1" {
		t.Fatalf("first claim = %+v", task)
	}
	if task.Language != "cpp" || task.Source != "int main(){}" || task.TimeLimitMS != 1000 || task.MemLimitKB != 262144 {
		t.Fatalf("claim payload = %+v", task)
	}
	row, err := db.GetSubmission(ctx, first.ID)
	if err != nil || row.Status != "compiling" || row.JudgeID != j.ID || row.StartedAt == 0 {
		t.Fatalf("claimed row = %+v, %v", row, err)
	}

	// A second box takes the next row, not the one already owned.
	other, err := db.ClaimNext(ctx, j2.ID, CapsAll)
	if err != nil {
		t.Fatal(err)
	}
	if other.SubmissionID != second.ID {
		t.Fatalf("second claim = %+v, want %d", other, second.ID)
	}

	if _, err := db.ClaimNext(ctx, j.ID, CapsAll); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty queue err = %v, want ErrNotFound", err)
	}
}

func TestConcurrentClaimsNeverDuplicate(t *testing.T) {
	ctx := context.Background()
	db, u, p, _ := queueFixture(t)

	const jobs = 24
	for i := 0; i < jobs; i++ {
		submitSomething(t, db, u, p, "x")
	}
	boxes := make([]int64, 4)
	for i := range boxes {
		j, err := db.UpsertJudge(ctx, string(rune('a'+i))+":box", nil, nil, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		boxes[i] = j.ID
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[int64]int{}
		errs []error
	)
	for _, id := range boxes {
		wg.Add(1)
		go func(judgeID int64) {
			defer wg.Done()
			for {
				task, err := db.ClaimNext(ctx, judgeID, CapsAll)
				if errors.Is(err, ErrNotFound) {
					return
				}
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				mu.Lock()
				seen[task.SubmissionID]++
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("claim errors: %v", errs)
	}
	if len(seen) != jobs {
		t.Fatalf("claimed %d distinct rows, queued %d", len(seen), jobs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("submission %d claimed %d times", id, n)
		}
	}
}

func TestReportStatusOnlyByOwnerAndLegalStep(t *testing.T) {
	ctx := context.Background()
	db, u, p, j := queueFixture(t)
	j2, err := db.UpsertJudge(ctx, "box-2", nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := submitSomething(t, db, u, p, "x")
	if _, err := db.ClaimNext(ctx, j.ID, CapsAll); err != nil {
		t.Fatal(err)
	}

	// Another box may not touch it.
	if ok, err := db.ReportStatus(ctx, s.ID, j2.ID, "compiling", "running", ""); err != nil || ok {
		t.Fatalf("non-owner report = %v %v", ok, err)
	}
	// Wrong source state.
	if ok, err := db.ReportStatus(ctx, s.ID, j.ID, "queued", "running", ""); err != nil || ok {
		t.Fatalf("wrong from-state = %v %v", ok, err)
	}
	if ok, err := db.ReportStatus(ctx, s.ID, j.ID, "compiling", "running", "on case 3"); err != nil || !ok {
		t.Fatalf("owner report = %v %v", ok, err)
	}
	row, err := db.GetSubmission(ctx, s.ID)
	if err != nil || row.Status != "running" {
		t.Fatalf("row after progress = %+v %v", row, err)
	}
	// detail is deliberately absent from the default column set; it comes back
	// through the payload read.
	if _, detail, err := db.SubmissionPayload(ctx, s.ID); err != nil || detail != "on case 3" {
		t.Fatalf("detail = %q %v", detail, err)
	}
	// An empty detail must not erase what is already there.
	if ok, err := db.ReportStatus(ctx, s.ID, j.ID, "running", "running", ""); err != nil || !ok {
		t.Fatalf("self transition = %v %v", ok, err)
	}
	if _, detail, err := db.SubmissionPayload(ctx, s.ID); err != nil || detail != "on case 3" {
		t.Fatalf("detail was cleared: %q %v", detail, err)
	}
}

func TestReapStaleAndRequeue(t *testing.T) {
	ctx := context.Background()
	db, u, p, j := queueFixture(t)
	s := submitSomething(t, db, u, p, "x")
	if _, err := db.ClaimNext(ctx, j.ID, CapsAll); err != nil {
		t.Fatal(err)
	}
	// Simulate a judge box that died an hour ago.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE submissions SET started_at=? WHERE id=?`, nowMS()-3600_000, s.ID); err != nil {
		t.Fatal(err)
	}

	n, err := db.ReapStale(ctx, 60_000)
	if err != nil || n != 1 {
		t.Fatalf("reaped = %d %v", n, err)
	}
	row, err := db.GetSubmission(ctx, s.ID)
	if err != nil || row.Status != "abandoned" || row.Verdict != "IE" {
		t.Fatalf("reaped row = %+v %v", row, err)
	}
	// A reaped row is out of the pending queue.
	if pending, err := db.PendingCount(ctx); err != nil || pending != 0 {
		t.Fatalf("pending after reap = %d %v", pending, err)
	}

	if ok, err := db.RequeueAbandoned(ctx, s.ID); err != nil || !ok {
		t.Fatalf("requeue = %v %v", ok, err)
	}
	row, err = db.GetSubmission(ctx, s.ID)
	if err != nil || row.Status != "queued" || row.Verdict != "" || row.JudgeID != 0 || row.StartedAt != 0 {
		t.Fatalf("requeued row = %+v %v", row, err)
	}
	// Requeueing a settled row must fail; only abandoned rows go back.
	if err := db.RecordResult(ctx, s.ID, Result{Verdict: "AC", TimeMS: 1, MemKB: 1, Score: 1, TotalScore: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RequeueAbandoned(ctx, s.ID); err != nil || ok {
		t.Fatalf("done row requeued: %v %v", ok, err)
	}
}

func TestReapLeavesFreshWorkAlone(t *testing.T) {
	ctx := context.Background()
	db, u, p, j := queueFixture(t)
	s := submitSomething(t, db, u, p, "x")
	if _, err := db.ClaimNext(ctx, j.ID, CapsAll); err != nil {
		t.Fatal(err)
	}
	n, err := db.ReapStale(ctx, 60_000)
	if err != nil || n != 0 {
		t.Fatalf("fresh claim reaped: %d %v", n, err)
	}
	if row, _ := db.GetSubmission(ctx, s.ID); row.Status != "compiling" {
		t.Fatalf("row = %+v", row)
	}
}

// 提交选的版本要一路走到判机手里：这一列在写库、读列表、领取工单任何一环被丢掉，
// 页面上写的"C++14"就和实际编译用的东西不是一回事了。
func TestClaimNextCarriesVersionOptions(t *testing.T) {
	ctx := context.Background()
	db, u, p, j := queueFixture(t)

	chosen, err := db.CreateSubmission(ctx, &Submission{
		UserID: u.ID, ProblemID: p.ID, Language: "cpp", Options: "std=c++14,O=0",
		Source: "int main(){}", TotalScore: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.ClaimNext(ctx, j.ID, CapsAll)
	if err != nil {
		t.Fatal(err)
	}
	if task.Options != "std=c++14,O=0" {
		t.Errorf("领到的工单带的版本选项 = %q，要 std=c++14,O=0", task.Options)
	}
	if row, err := db.GetSubmission(ctx, chosen.ID); err != nil || row.Options != "std=c++14,O=0" {
		t.Fatalf("写库后读回 = %+v, %v", row, err)
	}

	// 不选的老路：空串一路是空串，判定基准交给判机的默认。
	plain := submitSomething(t, db, u, p, "int main(){}")
	list, err := db.ListSubmissions(ctx, SubmissionFilter{UserID: u.ID})
	if err != nil || len(list) != 2 {
		t.Fatalf("列表 = %d 条, %v", len(list), err)
	}
	options := map[int64]string{}
	for _, s := range list {
		options[s.ID] = s.Options
	}
	if options[chosen.ID] != "std=c++14,O=0" {
		t.Errorf("列表里 #%d 的版本选项 = %q", chosen.ID, options[chosen.ID])
	}
	if options[plain.ID] != "" {
		t.Errorf("列表里 #%d 没选版本却带上了 %q", plain.ID, options[plain.ID])
	}
	box2, err := db.UpsertJudge(ctx, "box-2", nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := db.ClaimNext(ctx, box2.ID, CapsAll)
	if err != nil {
		t.Fatal(err)
	}
	if next.SubmissionID != plain.ID || next.Options != "" {
		t.Errorf("领取到的工单 = %+v，要那条没选版本的", next)
	}
}

// 一台不会 SPJ 的判机不该领到 SPJ 题。它不认识工单里的判题器，会按逐字节比较判掉
// 然后正常回报成功——于是页面上那句「本题由 SPJ 判定」成了谎。领不到就排队等一台
// 判得了的机器，而不是"降级判"。
func TestClaimNextSkipsWorkTheBoxCannotHonour(t *testing.T) {
	ctx := context.Background()
	db, u, plain1, _ := queueFixture(t)
	spj := mustProblem(t, db, "P9", "SPJ problem")
	if err := db.PutJudgeProgram(ctx, JudgeProgram{
		ProblemID: spj.ID, Kind: ProgramChecker, Language: "cpp", Source: "int main(){return 0;}",
	}); err != nil {
		t.Fatal(err)
	}

	// 队首是 SPJ 题的提交，队尾是普通题的。
	first := submitSomething(t, db, u, spj, "int main(){}")
	second := submitSomething(t, db, u, plain1, "int main(){}")

	old, err := db.UpsertJudge(ctx, "box-old", []string{"cpp"}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.ClaimNext(ctx, old.ID, Caps{})
	if err != nil {
		t.Fatalf("没有 checker 能力的判机领单失败：%v", err)
	}
	if task.SubmissionID != second.ID {
		t.Fatalf("没有 checker 能力却领到了 SPJ 题的 #%d（要 #%d）", task.SubmissionID, second.ID)
	}
	if row, err := db.GetSubmission(ctx, first.ID); err != nil || row.Status != "queued" {
		t.Fatalf("SPJ 题那条应当还在排队 = %+v, %v", row, err)
	}

	newBox, err := db.UpsertJudge(ctx, "box-new", []string{"cpp"},
		[]string{"options", "checker", "interactor"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !newBox.Has("checker") {
		t.Fatalf("自报的能力没存下来：%+v", newBox)
	}
	task, err = db.ClaimNext(ctx, newBox.ID, Caps{
		Options: newBox.Has("options"), Checker: newBox.Has("checker"),
		Interactor: newBox.Has("interactor"),
	})
	if err != nil {
		t.Fatalf("会 SPJ 的判机领单失败：%v", err)
	}
	if task.SubmissionID != first.ID {
		t.Fatalf("会 SPJ 的判机应当拿回队首那条 SPJ 工单，得到 #%d", task.SubmissionID)
	}
}

// 选过版本的提交不该派给一台不认版本的判机：它会按自己那套硬编码编，而页面留着
// 「C++14 · O2 关」那句话——那是判机没说过的话。
func TestClaimNextSkipsOptionsForBoxThatIgnoresThem(t *testing.T) {
	ctx := context.Background()
	db, u, p, _ := queueFixture(t)
	chosen, err := db.CreateSubmission(ctx, &Submission{
		UserID: u.ID, ProblemID: p.ID, Language: "cpp",
		Options: "std=c++14", Source: "int main(){}", TotalScore: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := submitSomething(t, db, u, p, "int main(){}")

	box, err := db.UpsertJudge(ctx, "box-nocap", []string{"cpp"}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.ClaimNext(ctx, box.ID, Caps{})
	if err != nil {
		t.Fatal(err)
	}
	if task.SubmissionID != plain.ID {
		t.Fatalf("不认版本的判机领走了选了版本的 #%d（应当是那条没选的 #%d）",
			task.SubmissionID, plain.ID)
	}
	if row, err := db.GetSubmission(ctx, chosen.ID); err != nil || row.Status != "queued" {
		t.Fatalf("选了版本的那条应当还在排队 = %+v, %v", row, err)
	}
}

// 语言不参与派单，这是有意的：判机领到一门它没有的语言时会如实回一条判机故障
// （"这台判机没有 X 工具链"），学生看得到、操作员查得到。把它扣在队列里"等一台会判的机器"
// 反而是更坏的失败——站点上只有一台判机时，那条提交会永远排队。
func TestClaimNextDoesNotGateOnLanguage(t *testing.T) {
	ctx := context.Background()
	db, u, p, _ := queueFixture(t)
	want, err := db.CreateSubmission(ctx, &Submission{
		UserID: u.ID, ProblemID: p.ID, Language: "python", Source: "print(3)", TotalScore: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 一台只自报会判 cpp 的机器。
	box, err := db.UpsertJudge(ctx, "box-cpp", []string{"cpp"},
		[]string{"options", "checker", "interactor"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.ClaimNext(ctx, box.ID, CapsAll)
	if err != nil {
		t.Fatalf("语言名单不该挡派单：%v", err)
	}
	if task.SubmissionID != want.ID || task.Language != "python" {
		t.Fatalf("领到 %+v，要那条 python 提交（由判机自己回实话说没有工具链）", task)
	}
}

// 判机的隔离状态要能区分"没说"与"说了没有"：升级后还没心跳的老二进制报不了这一项，
// 把它显示成"确认没有隔离"就是替一台没开口的机器编一句话。
func TestUpsertJudgeKeepsJailedUnknownApartFromNo(t *testing.T) {
	ctx := context.Background()
	db, _, _, _ := queueFixture(t)
	tr, fl := true, false

	old, err := db.UpsertJudge(ctx, "box-silent", []string{"cpp"}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if old.Jailed || old.JailedKnown {
		t.Fatalf("没报过隔离状态却读出了结论：%+v", old)
	}
	yes, err := db.UpsertJudge(ctx, "box-jailed", []string{"cpp"}, nil, "", &tr)
	if err != nil {
		t.Fatal(err)
	}
	if !yes.Jailed || !yes.JailedKnown {
		t.Fatalf("有牢房没记下来：%+v", yes)
	}
	// 同一台机器从"有"翻到"没有"必须盖掉旧值——沙盒开不起来是当下的事实，不是历史。
	no, err := db.UpsertJudge(ctx, "box-jailed", []string{"cpp"}, nil, "开不起来", &fl)
	if err != nil {
		t.Fatal(err)
	}
	if no.Jailed || !no.JailedKnown {
		t.Fatalf("翻过去了没盖掉旧值：%+v", no)
	}
	list, err := db.ListJudges(ctx)
	if err != nil || len(list) != 3 { // 加上 queueFixture 里那台 box-1
		t.Fatalf("ListJudges = %d 条, %v", len(list), err)
	}
	for _, j := range list {
		switch j.Name {
		case "box-silent":
			if j.JailedKnown {
				t.Errorf("未报的机器被当成了报过的：%+v", j)
			}
		case "box-jailed":
			if j.Jailed || !j.JailedKnown {
				t.Errorf("报了开不起来的机器显示成了有牢房：%+v", j)
			}
		}
	}
}

func TestQueueStatusCountsAndWaits(t *testing.T) {
	ctx := context.Background()
	db, u, p, j := queueFixture(t)

	submitSomething(t, db, u, p, "int main(){}")
	second := submitSomething(t, db, u, p, "int main(){return 1;}")

	q, err := db.QueueStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q.Queued != 2 || q.Running != 0 || q.Abandoned != 0 {
		t.Fatalf("两条都在排队 = %+v", q)
	}
	if q.WaitMS < 0 {
		t.Errorf("最久等待不该是负数：%+v", q)
	}

	if _, err := db.ClaimNext(ctx, j.ID, CapsAll); err != nil {
		t.Fatal(err)
	}
	q, err = db.QueueStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q.Queued != 1 || q.Running != 1 {
		t.Fatalf("领走一条后 = %+v", q)
	}

	// 剩下那条被判机领走后死掉，停在 abandoned：它既不是"还在排"也不是"正在判"，
	// 但确实是堵住队列的一条，要单独数出来。
	if _, err := db.SQL.ExecContext(ctx, `UPDATE submissions SET status='abandoned' WHERE id=?`, second.ID); err != nil {
		t.Fatal(err)
	}
	q, err = db.QueueStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q.Queued != 0 || q.Running != 1 || q.Abandoned != 1 {
		t.Fatalf("孤儿那条也要数进来：%+v", q)
	}
}

func TestQueueStatusOnEmptyQueue(t *testing.T) {
	ctx := context.Background()
	db, _, _, _ := queueFixture(t)
	q, err := db.QueueStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q != (QueueStatus{}) {
		t.Fatalf("空队列该全是 0，拿到 %+v", q)
	}
}
