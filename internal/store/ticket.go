package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Tickets are the help desk: a user files one (判题申诉、故障、举报、账号问题), a
// holder of ticket.handle works it. Unlike topics these are private — the reader
// rule lives in the API layer, but the shape here is what makes it possible: a
// ticket belongs to its author and to whoever picks it up, and never to a feed.

const (
	TicketAppeal  = "appeal"  // 判题申诉：数据弱、判定不对、想让人复核
	TicketBug     = "bug"     // 故障与建议
	TicketReport  = "report"  // 举报他人
	TicketAccount = "account" // 账号问题：邮箱、封禁、忘记密码之外的事
	TicketOther   = "other"

	TicketOpen   = "open"   // 待处理
	TicketDoing  = "doing"  // 有人在办
	TicketDone   = "done"   // 已解决
	TicketClosed = "closed" // 已关闭（作者自己关，或管理员关）

	MaxTicketTitle    = 200
	MaxTicketBody     = 16000
	MaxTicketReply    = 4000
	DefaultTicketPage = 30
	MaxTicketPage     = 200
)

func ValidTicketKind(kind string) bool {
	switch kind {
	case TicketAppeal, TicketBug, TicketReport, TicketAccount, TicketOther:
		return true
	}
	return false
}

func ValidTicketStatus(status string) bool {
	switch status {
	case TicketOpen, TicketDoing, TicketDone, TicketClosed:
		return true
	}
	return false
}

func TicketKindLabel(kind string) string {
	switch kind {
	case TicketAppeal:
		return "申诉"
	case TicketBug:
		return "故障建议"
	case TicketReport:
		return "举报"
	case TicketAccount:
		return "账号"
	}
	return "其他"
}

// TicketStatusLabel is what a reader sees; the badge class is the same information
// in CSS's vocabulary, so the two can never drift apart.
func TicketStatusLabel(status string) string {
	switch status {
	case TicketOpen:
		return "待处理"
	case TicketDoing:
		return "处理中"
	case TicketDone:
		return "已解决"
	case TicketClosed:
		return "已关闭"
	}
	return status
}

// TicketStatusClass is the badge class for a status. 未办完的两档用 warn，
// 让人在列表里一眼扫出队列的头在哪里。
func TicketStatusClass(status string) string {
	switch status {
	case TicketOpen, TicketDoing:
		return "warn"
	case TicketDone:
		return "ok"
	}
	return ""
}

// OpenTicketStatuses is the queue a handler sees by default: anything not finished.
func OpenTicketStatuses() []string { return []string{TicketOpen, TicketDoing} }

type Ticket struct {
	ID          int64
	Kind        string
	Title       string
	Body        string
	AuthorID    int64
	Author      string
	DisplayName string
	// 挂着的题目与提交，0 表示没挂。题目/提交被删时外键置空，工单本身留着。
	ProblemID    int64
	ProblemPID   string
	ProblemTitle string
	SubmissionID int64
	Status       string
	HandlerID    int64
	Handler      string
	Replies      int
	CreatedAt    int64
	UpdatedAt    int64
}

type TicketReply struct {
	ID          int64
	TicketID    int64
	AuthorID    int64
	Author      string
	DisplayName string
	Body        string
	// Event 非空时这是一条系统流水（status/assign），不是谁写的话。
	Event     string
	CreatedAt int64
}

// TicketFilter is everything ListTickets can narrow by. The zero value is the
// newest-first walk over every ticket, which only a handler may ask for.
type TicketFilter struct {
	AuthorID  int64
	Status    string
	OpenOnly  bool
	Search    string
	After     int64
	Limit     int
	WithCount bool
}

var ticketColumns = `t.id, t.kind, t.title, t.body, t.author_id, u.handle, ` + personSQL(`u`) + `,
		IFNULL(t.problem_id, 0), IFNULL(p.pid, ''), IFNULL(p.title, ''),
		IFNULL(t.submission_id, 0), t.status, IFNULL(t.handler_id, 0), IFNULL(h.handle, ''),
		t.created_at, t.updated_at`

const ticketFrom = ` FROM tickets t
	JOIN users u ON u.id = t.author_id
	LEFT JOIN problems p ON p.id = t.problem_id
	LEFT JOIN users h ON h.id = t.handler_id`

func scanTicket(row scanner) (Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.Kind, &t.Title, &t.Body, &t.AuthorID, &t.Author, &t.DisplayName,
		&t.ProblemID, &t.ProblemPID, &t.ProblemTitle, &t.SubmissionID, &t.Status,
		&t.HandlerID, &t.Handler, &t.CreatedAt, &t.UpdatedAt)
	return t, notFound(err)
}

// CreateTicket files one ticket. kind 与 status 都由这里把关：状态新单只能是待处理，
// "提交工单的人顺手把自己标成已解决"不是想要的行为。
func (db *DB) CreateTicket(ctx context.Context, t *Ticket) (Ticket, error) {
	if !ValidTicketKind(t.Kind) {
		return Ticket{}, fmt.Errorf("不认识的工单类型：%q", t.Kind)
	}
	t.Title = strings.TrimSpace(t.Title)
	t.Body = strings.TrimRight(t.Body, " \t\r\n")
	switch {
	case t.Title == "":
		return Ticket{}, errors.New("标题不能是空的")
	case utf8.RuneCountInString(t.Title) > MaxTicketTitle:
		return Ticket{}, fmt.Errorf("标题最长 %d 个字符", MaxTicketTitle)
	case utf8.RuneCountInString(t.Body) > MaxTicketBody:
		return Ticket{}, fmt.Errorf("正文最长 %d 个字符", MaxTicketBody)
	case t.AuthorID == 0:
		return Ticket{}, errors.New("工单要有作者")
	}
	var problem, sub any
	if t.ProblemID != 0 {
		problem = t.ProblemID
	}
	if t.SubmissionID != 0 {
		sub = t.SubmissionID
	}
	at := nowMS()
	res, err := db.SQL.ExecContext(ctx,
		`INSERT INTO tickets (kind, title, body, author_id, problem_id, submission_id,
		                      status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.Title, t.Body, t.AuthorID, problem, sub, TicketOpen, at, at)
	if err != nil {
		return Ticket{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Ticket{}, err
	}
	t.ID, t.Status, t.CreatedAt, t.UpdatedAt = id, TicketOpen, at, at
	return *t, nil
}

// FindTicket reads one by id. It does not decide who may see it — that is the
// caller's job, and the API layer is the only one that knows the actor.
func (db *DB) FindTicket(ctx context.Context, id int64) (Ticket, error) {
	var t Ticket
	err := db.SQL.QueryRowContext(ctx,
		`SELECT `+ticketColumns+ticketFrom+` WHERE t.id = ?`, id).Scan(
		&t.ID, &t.Kind, &t.Title, &t.Body, &t.AuthorID, &t.Author, &t.DisplayName,
		&t.ProblemID, &t.ProblemPID, &t.ProblemTitle, &t.SubmissionID, &t.Status,
		&t.HandlerID, &t.Handler, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return t, notFound(err)
	}
	return t, nil
}

// ListTickets walks tickets newest first. The cursor is the id, which is also the
// sort key: a page boundary that disagrees with the ORDER BY would repeat or drop
// rows.
func (db *DB) ListTickets(ctx context.Context, f TicketFilter) ([]Ticket, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = DefaultTicketPage
	case limit > MaxTicketPage:
		limit = MaxTicketPage
	}
	where := []string{"1=1"}
	var args []any
	if f.AuthorID != 0 {
		where = append(where, "t.author_id = ?")
		args = append(args, f.AuthorID)
	}
	if f.Status != "" {
		if !ValidTicketStatus(f.Status) {
			return nil, fmt.Errorf("不认识的工单状态：%q", f.Status)
		}
		where = append(where, "t.status = ?")
		args = append(args, f.Status)
	}
	if f.OpenOnly {
		where = append(where, "t.status IN ('"+TicketOpen+"','"+TicketDoing+"')")
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		where = append(where, "(t.title LIKE ? OR t.body LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if f.After > 0 {
		where = append(where, "t.id < ?")
		args = append(args, f.After)
	}
	sql := `SELECT ` + ticketColumns +
		`, (SELECT COUNT(*) FROM ticket_replies r WHERE r.ticket_id = t.id)` +
		ticketFrom + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY t.id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.SQL.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ticket
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.Kind, &t.Title, &t.Body, &t.AuthorID, &t.Author, &t.DisplayName,
			&t.ProblemID, &t.ProblemPID, &t.ProblemTitle, &t.SubmissionID, &t.Status,
			&t.HandlerID, &t.Handler, &t.CreatedAt, &t.UpdatedAt, &t.Replies); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountOpenTickets drives the badge in the nav bar: a handler should see the size
// of the queue without having to open it.
func (db *DB) CountOpenTickets(ctx context.Context) (int, error) {
	var n int
	err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tickets WHERE status IN ('`+TicketOpen+`','`+TicketDoing+`')`).Scan(&n)
	return n, err
}

// AddTicketReply appends one reply (or one system line) and bumps the ticket's
// updated_at, so "有动静的单" is findable without a second sort key.
func (db *DB) AddTicketReply(ctx context.Context, r *TicketReply) (TicketReply, error) {
	r.Body = strings.TrimRight(r.Body, " \t\r\n")
	if r.Event == "" && r.Body == "" {
		return TicketReply{}, errors.New("回复不能是空的")
	}
	if utf8.RuneCountInString(r.Body) > MaxTicketReply {
		return TicketReply{}, fmt.Errorf("回复最长 %d 个字符", MaxTicketReply)
	}
	switch r.Event {
	case "", "status", "assign":
	default:
		return TicketReply{}, fmt.Errorf("不认识的流水类型：%q", r.Event)
	}
	if r.AuthorID == 0 {
		return TicketReply{}, errors.New("回复要有作者")
	}
	at := nowMS()
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO ticket_replies (ticket_id, author_id, body, event, created_at)
			 VALUES (?, ?, ?, ?, ?)`, r.TicketID, r.AuthorID, r.Body, r.Event, at)
		if err != nil {
			// 外键挡住的两件事都要说清：工单不在了，或者作者不在了。
			if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
				return ErrNotFound
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tickets SET updated_at = ? WHERE id = ?`, at, r.TicketID); err != nil {
			return err
		}
		r.ID, r.CreatedAt = id, at
		return nil
	})
	if err != nil {
		return TicketReply{}, err
	}
	return *r, nil
}

// SetTicketStatus moves a ticket and writes the move into the thread. Two statements
// that must not be seen apart, so they share one transaction.
func (db *DB) SetTicketStatus(ctx context.Context, id int64, status string, handlerID int64, note string) (Ticket, error) {
	if !ValidTicketStatus(status) {
		return Ticket{}, fmt.Errorf("不认识的工单状态：%q", status)
	}
	var t Ticket
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tickets SET status = ?, handler_id = ?, updated_at = ? WHERE id = ?`,
			status, nullID(handlerID), nowMS(), id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		body := "状态改为「" + TicketStatusLabel(status) + "」"
		if strings.TrimSpace(note) != "" {
			body += "：" + strings.TrimSpace(note)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ticket_replies (ticket_id, author_id, body, event, created_at)
			 VALUES (?, ?, ?, 'status', ?)`, id, handlerID, body, nowMS()); err != nil {
			return err
		}
		var scanErr error
		t, scanErr = scanTicket(tx.QueryRowContext(ctx,
			`SELECT `+ticketColumns+ticketFrom+` WHERE t.id = ?`, id))
		return scanErr
	})
	return t, err
}

// DeleteTicket removes a ticket and, by cascade, its replies.
func (db *DB) DeleteTicket(ctx context.Context, id int64) (bool, error) {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM tickets WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListTicketReplies is the thread, oldest first: the story of a ticket reads top
// down like everything else a human wrote.
func (db *DB) ListTicketReplies(ctx context.Context, ticketID int64) ([]TicketReply, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT r.id, r.ticket_id, r.author_id, u.handle, `+personSQL(`u`)+`, r.body, r.event, r.created_at
		 FROM ticket_replies r JOIN users u ON u.id = r.author_id
		 WHERE r.ticket_id = ? ORDER BY r.id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TicketReply
	for rows.Next() {
		var r TicketReply
		if err := rows.Scan(&r.ID, &r.TicketID, &r.AuthorID, &r.Author, &r.DisplayName,
			&r.Body, &r.Event, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
