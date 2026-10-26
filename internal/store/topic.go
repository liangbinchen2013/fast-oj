package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Topics are the forum: a 求助贴 hangs on one problem, a 社区贴 does not and is read
// from /topics. They are kept apart from the feed (动态) on purpose: a feed entry is
// one line on a timeline, a topic has a title, a body and a comment thread.

const (
	TopicHelp      = "help"
	TopicCommunity = "community"

	MaxTopicTitle    = 200
	MaxTopicBody     = 16000
	MaxTopicComment  = 4000
	DefaultTopicPage = 30
	MaxTopicPage     = 200
)

func ValidTopicKind(kind string) bool {
	return kind == TopicHelp || kind == TopicCommunity
}

// TopicKindLabel is the short word a reader sees next to a post.
func TopicKindLabel(kind string) string {
	if kind == TopicHelp {
		return "求助"
	}
	return "社区"
}

type Topic struct {
	ID          int64
	Kind        string
	Title       string
	Body        string
	AuthorID    int64
	Author      string
	DisplayName string
	// Color 是这个名字的活跃度档位，读名字的时候一起带出来（见 feed.go 里同名的理由）。
	Color string
	// ProblemID is 0 for a 社区贴. The two text fields come along in the same join,
	// so a list page never needs a second query per row.
	ProblemID    int64
	ProblemPID   string
	ProblemTitle string
	Comments     int
	CreatedAt    int64
}

type TopicComment struct {
	ID          int64
	TopicID     int64
	AuthorID    int64
	Author      string
	DisplayName string
	Color       string
	Body        string
	CreatedAt   int64
}

// TopicFilter is everything ListTopics can narrow by. The zero value is the plain
// newest-first walk over both kinds.
type TopicFilter struct {
	Kind      string
	ProblemID int64
	AuthorID  int64
	Search    string
	After     int64
	Limit     int
}

var topicColumns = `t.id, t.kind, t.title, t.body, t.author_id, u.handle, ` + personSQL(`u`) + `, ` + colorSQL(`u`) + `,
		IFNULL(t.problem_id, 0), IFNULL(p.pid, ''), IFNULL(p.title, ''), t.created_at`

const topicFrom = ` FROM topics t
	JOIN users u ON u.id = t.author_id
	LEFT JOIN problems p ON p.id = t.problem_id`

func scanTopic(row scanner) (Topic, error) {
	var t Topic
	err := row.Scan(&t.ID, &t.Kind, &t.Title, &t.Body, &t.AuthorID, &t.Author, &t.DisplayName, &t.Color,
		&t.ProblemID, &t.ProblemPID, &t.ProblemTitle, &t.CreatedAt)
	return t, notFound(err)
}

// CreateTopic inserts one post. The table's own CHECK ties the kind to problem_id,
// so a caller that mixes them up is refused by the database rather than shipping a
// half-linked 求助贴.
func (db *DB) CreateTopic(ctx context.Context, kind, title, body string, authorID, problemID int64) (Topic, error) {
	if !ValidTopicKind(kind) {
		return Topic{}, fmt.Errorf("不认识的帖子类型：%q", kind)
	}
	title = strings.TrimSpace(title)
	body = strings.TrimRight(body, " \t\r\n")
	switch {
	case title == "":
		return Topic{}, errors.New("标题不能是空的")
	case utf8.RuneCountInString(title) > MaxTopicTitle:
		return Topic{}, fmt.Errorf("标题最长 %d 个字符", MaxTopicTitle)
	case utf8.RuneCountInString(body) > MaxTopicBody:
		return Topic{}, fmt.Errorf("正文最长 %d 个字符", MaxTopicBody)
	}
	var pid any
	if problemID != 0 {
		pid = problemID
	}
	res, err := db.SQL.ExecContext(ctx,
		`INSERT INTO topics (kind, title, body, author_id, problem_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`, kind, title, body, authorID, pid, nowMS())
	if err != nil {
		if strings.Contains(err.Error(), "CHECK constraint failed") {
			if kind == TopicHelp {
				return Topic{}, errors.New("求助贴要挂在一道题上")
			}
			return Topic{}, errors.New("社区贴不挂题目")
		}
		return Topic{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Topic{}, err
	}
	return db.GetTopic(ctx, id)
}

func (db *DB) GetTopic(ctx context.Context, id int64) (Topic, error) {
	t, err := scanTopic(db.SQL.QueryRowContext(ctx,
		`SELECT `+topicColumns+topicFrom+` WHERE t.id=?`, id))
	if err != nil {
		return t, err
	}
	counts, err := db.countTopicComments(ctx, []int64{t.ID})
	if err != nil {
		return t, err
	}
	t.Comments = counts[t.ID]
	return t, nil
}

// ListTopics walks newest-first with an id cursor, the same shape as the problem and
// submission lists, so a deep page costs no more than the first one.
func (db *DB) ListTopics(ctx context.Context, f TopicFilter) ([]Topic, error) {
	if f.Limit <= 0 {
		f.Limit = DefaultTopicPage
	}
	if f.Limit > MaxTopicPage {
		f.Limit = MaxTopicPage
	}
	q := `SELECT ` + topicColumns + topicFrom + ` WHERE 1=1`
	args := []any{}
	if f.Kind != "" {
		q += ` AND t.kind=?`
		args = append(args, f.Kind)
	}
	if f.ProblemID != 0 {
		q += ` AND t.problem_id=?`
		args = append(args, f.ProblemID)
	}
	if f.AuthorID != 0 {
		q += ` AND t.author_id=?`
		args = append(args, f.AuthorID)
	}
	if f.Search != "" {
		like := "%" + escapeLike(f.Search) + "%"
		q += ` AND (t.title LIKE ? ESCAPE '\' OR t.body LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	if f.After != 0 {
		q += ` AND t.id<?`
		args = append(args, f.After)
	}
	q += ` ORDER BY t.id DESC LIMIT ?`
	args = append(args, f.Limit)

	rows, err := db.SQL.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Topic, 0, f.Limit)
	for rows.Next() {
		t, err := scanTopic(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One grouped query for every comment count: a page of thirty must not turn
	// into thirty-one queries.
	counts, err := db.countTopicComments(ctx, topicIDs(out))
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Comments = counts[out[i].ID]
	}
	return out, nil
}

func topicIDs(topics []Topic) []int64 {
	ids := make([]int64, len(topics))
	for i, t := range topics {
		ids[i] = t.ID
	}
	return ids
}

func (db *DB) countTopicComments(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT topic_id, COUNT(*) FROM topic_comments WHERE topic_id IN (`+placeholders+`) GROUP BY topic_id`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// DeleteTopic removes a post, and only its author gets through this path; taking
// someone else's down is DeleteTopicAny behind topic.moderate. The comments go with
// it by the foreign key.
func (db *DB) DeleteTopic(ctx context.Context, id, authorID int64) (bool, error) {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM topics WHERE id=? AND author_id=?`, id, authorID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (db *DB) DeleteTopicAny(ctx context.Context, id int64) (bool, error) {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM topics WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// --- comments ---

var topicCommentColumns = `c.id, c.topic_id, c.author_id, u.handle, ` + personSQL(`u`) + `, ` + colorSQL(`u`) + `, c.body, c.created_at`

func scanTopicComment(row scanner) (TopicComment, error) {
	var c TopicComment
	err := row.Scan(&c.ID, &c.TopicID, &c.AuthorID, &c.Author, &c.DisplayName, &c.Color, &c.Body, &c.CreatedAt)
	return c, notFound(err)
}

// CreateTopicComment refuses a comment on a topic that is not there. The foreign key
// is the check: a comment racing with its own deletion is turned away by the
// database, not by a read that could go stale between here and there.
func (db *DB) CreateTopicComment(ctx context.Context, topicID, authorID int64, body string) (TopicComment, error) {
	body = strings.TrimRight(body, " \t\r\n")
	switch {
	case strings.TrimSpace(body) == "":
		return TopicComment{}, errors.New("说点什么吧")
	case utf8.RuneCountInString(body) > MaxTopicComment:
		return TopicComment{}, fmt.Errorf("评论最长 %d 个字符", MaxTopicComment)
	}
	res, err := db.SQL.ExecContext(ctx,
		`INSERT INTO topic_comments (topic_id, author_id, body, created_at) VALUES (?, ?, ?, ?)`,
		topicID, authorID, body, nowMS())
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return TopicComment{}, ErrNotFound
		}
		return TopicComment{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return TopicComment{}, err
	}
	return db.GetTopicComment(ctx, id)
}

// ListTopicComments reads the thread oldest-first: a conversation is read in the
// order it happened.
func (db *DB) ListTopicComments(ctx context.Context, topicID int64) ([]TopicComment, error) {
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT `+topicCommentColumns+` FROM topic_comments c JOIN users u ON u.id=c.author_id
		 WHERE c.topic_id=? ORDER BY c.id`, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TopicComment
	for rows.Next() {
		c, err := scanTopicComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) GetTopicComment(ctx context.Context, id int64) (TopicComment, error) {
	return scanTopicComment(db.SQL.QueryRowContext(ctx,
		`SELECT `+topicCommentColumns+` FROM topic_comments c JOIN users u ON u.id=c.author_id WHERE c.id=?`, id))
}

func (db *DB) DeleteTopicComment(ctx context.Context, id, authorID int64) (bool, error) {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM topic_comments WHERE id=? AND author_id=?`, id, authorID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (db *DB) DeleteTopicCommentAny(ctx context.Context, id int64) (bool, error) {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM topic_comments WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
