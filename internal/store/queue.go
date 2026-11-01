package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ClaimedTask is everything a judge box needs to start work on one submission.
type ClaimedTask struct {
	SubmissionID int64
	ProblemID    int64
	PID          string
	Language     string
	// Options is the version selection this submission asked for ("std=c++14,O=0");
	// empty means the judge box's own default.
	Options     string
	Source      string
	TimeLimitMS int
	MemLimitKB  int
}

// Caps 是一台判机这次领单时能做的事。它是判机**自报**的（见 Judge.Has），服务器照它派单。
//
// 语言不在这份里，这是有意的区别：判机不认识某门语言时，它自己会如实回一条判机故障
// （"这台判机没有 X 工具链"），那是一个看得见也查得到的失败。这里挡的三样不一样——
// 一台旧判机会**照判**，只是用错了口径，交回来的是一条看起来完全正常的假 AC / 假判定。
type Caps struct {
	Options    bool // 认 submissions.options，会按选的那条命令行编
	Checker    bool // 会现编并跑题目自带的判题器
	Interactor bool // 会跑交互器
}

// CapsAll 是"这台机器什么都会"。测试用它；生产里的 Caps 一律来自判机自报，
// 没有一个服务器可以自己假设它会。
var CapsAll = Caps{Options: true, Checker: true, Interactor: true}

// Claims 报告这份工单这台机器判得了吗。判不了就不是"降级判"而是"别派给我"：
// 一台旧判机不认识 options / checker / interactor，派过去它会照自己那套编、按逐字节比较判，
// 然后正常回报成功——页面上那句「C++14 · O2 关」「本题由 SPJ 判定」就成了当着的谎。
func (c Caps) Claims(t ClaimedTask, hasChecker, hasInteractor bool) bool {
	if t.Options != "" && !c.Options {
		return false
	}
	if hasChecker && !c.Checker {
		return false
	}
	if hasInteractor && !c.Interactor {
		return false
	}
	return true
}

// ClaimNext hands out the oldest queued submission **this box can actually judge**,
// or sql.ErrNoRows when there is none.
//
// The claim is one statement: the inner SELECT picks the row, the outer UPDATE
// stamps it, and the status guard inside the subquery means two boxes racing
// for the same row cannot both get a write. RETURNING gives the winner back
// without a second round trip.
//
// 派单条件写在 SQL 里而不是"取回来再看"：取回来再退回会把那条提交的状态从 queued 变成
// compiling，而判机回报失败时它就已经不是"没派出去"了。
func (db *DB) ClaimNext(ctx context.Context, judgeID int64, caps Caps) (ClaimedTask, error) {
	var t ClaimedTask
	var status string
	err := db.SQL.QueryRowContext(ctx,
		`UPDATE submissions
		 SET status='compiling', judge_id=?, started_at=?
		 WHERE id = (SELECT s.id FROM submissions s
		   WHERE s.status='queued'
		     AND (s.options='' OR ?)
		     AND (NOT EXISTS (SELECT 1 FROM judge_programs jp
		                          WHERE jp.problem_id=s.problem_id AND jp.kind='checker'
		                    AND TRIM(jp.source)<>'') OR ?)
		     AND (NOT EXISTS (SELECT 1 FROM judge_programs jp
		                          WHERE jp.problem_id=s.problem_id AND jp.kind='interactor'
		                    AND TRIM(jp.source)<>'') OR ?)
		   ORDER BY s.id LIMIT 1)
		 RETURNING id, problem_id, status`,
		judgeID, nowMS(), boolInt(caps.Options), boolInt(caps.Checker), boolInt(caps.Interactor)).
		Scan(&t.SubmissionID, &t.ProblemID, &status)
	if err != nil {
		return t, notFound(err)
	}
	if status != "compiling" {
		return t, fmt.Errorf("claim returned status %q", status)
	}

	// Source can be large, so it is fetched only now, for the judge that owns
	// the row.
	err = db.SQL.QueryRowContext(ctx,
		`SELECT s.language, s.options, s.source, p.pid, p.time_limit_ms, p.mem_limit_kb
		 FROM submissions s JOIN problems p ON p.id=s.problem_id
		 WHERE s.id=? AND s.judge_id=?`, t.SubmissionID, judgeID).
		Scan(&t.Language, &t.Options, &t.Source, &t.PID, &t.TimeLimitMS, &t.MemLimitKB)
	if err != nil {
		return t, notFound(err)
	}
	return t, nil
}

// ReportStatus moves a claimed submission along the state machine. Only the
// judge that owns the row may report, and only from the state it is really in.
// An empty detail leaves the previous note alone.
// errStaleStatus is a control signal, not a failure: the row moved on while this
// judge was working, so its report no longer applies.
var errStaleStatus = errors.New("submission status moved on")

func (db *DB) ReportStatus(ctx context.Context, submissionID, judgeID int64, from, to, detail string) (bool, error) {
	return db.ReportProgress(ctx, submissionID, judgeID, from, to, detail, nil)
}

// ReportProgress moves the state and, when a finished case rides along, writes that
// row in the same transaction. Either both land or neither does, so the detail page
// can never show a case row belonging to a state the submission has moved past.
func (db *DB) ReportProgress(ctx context.Context, submissionID, judgeID int64, from, to, detail string, run *Run) (bool, error) {
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE submissions
			 SET status=?, detail=CASE WHEN ?='' THEN detail ELSE ? END
			 WHERE id=? AND judge_id=? AND status=?`,
			to, detail, detail, submissionID, judgeID, from)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errStaleStatus
		}
		if run != nil {
			run.SubmissionID = submissionID
			if err := writeRun(ctx, tx, *run); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errStaleStatus) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ReapStale marks in-flight submissions that no judge is working on any more as
// abandoned, so a dead judge box cannot wedge the queue forever.
func (db *DB) ReapStale(ctx context.Context, leaseMS int64) (int64, error) {
	res, err := db.SQL.ExecContext(ctx,
		`UPDATE submissions SET status='abandoned', verdict='IE', detail=?
		 WHERE status IN ('compiling','running') AND started_at IS NOT NULL
		   AND started_at < ?`,
		"judge lease expired", nowMS()-leaseMS)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RequeueAbandoned puts an abandoned row back at the tail of the queue.
// ClearRuns drops the case rows of an attempt that is about to be judged again,
// so a re-run never shows rows from the attempt that died.
func (db *DB) ClearRuns(ctx context.Context, submissionID int64) error {
	_, err := db.SQL.ExecContext(ctx, `DELETE FROM runs WHERE submission_id=?`, submissionID)
	return err
}

// RequeueAbandoned puts a dead attempt back in the queue and drops its case rows,
// so a re-run never shows results from the attempt that died.
func (db *DB) RequeueAbandoned(ctx context.Context, submissionID int64) (bool, error) {
	requeued := false
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE submissions SET status='queued', verdict='', judge_id=NULL, detail='',
			           time_ms=-1, mem_kb=-1, score=0, started_at=NULL, judged_at=NULL
			 WHERE id=? AND status='abandoned'`, submissionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errStaleStatus
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE submission_id=?`, submissionID); err != nil {
			return err
		}
		requeued = true
		return nil
	})
	if errors.Is(err, errStaleStatus) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return requeued, nil
}

// Rejudge 把一条**已经有结论**的提交退回队列，并清掉上一次的判定与逐用例行。
//
// 判题口径是会变的（见 0036 那份迁移），而库里的判定是当时那一次运行的结果，不会因为判机
// 升级就自己改判——重判是管理员唯一的补救：退回 queued，判机按今天的口径重跑一次。
//
// 已经在飞的那一次盖不掉新结果：这里把 judge_id 清成 NULL，旧判机回报时先在归属检查上吃
// 403，即使同一台判机重新领到它，status='queued' 也不允许直接跳 Done。toolchain 一起清掉，
// 因为那句"我这次实际用的口径"是上一台判机说的，得让判过新的那台重新说。
func (db *DB) Rejudge(ctx context.Context, submissionID int64) (bool, error) {
	requeued := false
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE submissions SET status='queued', verdict='', judge_id=NULL, detail='',
			           time_ms=-1, mem_kb=-1, time_min=0, mem_min=0, score=0, failed_case='',
			           toolchain='', started_at=NULL, judged_at=NULL
			 WHERE id=? AND status<>'queued'`, submissionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errStaleStatus
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE submission_id=?`, submissionID); err != nil {
			return err
		}
		requeued = true
		return nil
	})
	if errors.Is(err, errStaleStatus) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return requeued, nil
}
