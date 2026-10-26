package store

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxPostBody bounds one feed entry in characters, matching the textarea limit in
// the browser, so a Chinese post is not charged triple for its bytes.
const MaxPostBody = 4000

// Timeline page bounds. Callers clamp the same way, so a short page really does
// mean the end of the feed.
const (
	DefaultPostPage = 30
	MaxPostPage     = 200
)

// Post is one feed entry. Author is joined so a timeline needs one query.
type Post struct {
	ID          int64  `json:"id"`
	AuthorID    int64  `json:"author_id"`
	Author      string `json:"author"`
	DisplayName string `json:"display_name,omitempty"`
	Body        string `json:"body"`
	CreatedAt   int64  `json:"created_at"`
	Visible     bool   `json:"visible"`
	HiddenBy    int64  `json:"hidden_by,omitempty"`
	HiddenAt    int64  `json:"hidden_at,omitempty"`
}

// postColumns 里的名字走 personSQL：那一个人"该被叫作什么"的唯一出口（注销后的墓碑署名、
// 管理员贴的标签都在这里接上），动态流不该自己复制一套规则。
var postColumns = `p.id, p.author_id, u.handle, ` + personSQL(`u`) + `, p.body, p.created_at,
		p.visible, IFNULL(p.hidden_by, 0), IFNULL(p.hidden_at, 0)`

func scanPost(row scanner) (Post, error) {
	var (
		p       Post
		visible int
	)
	err := row.Scan(&p.ID, &p.AuthorID, &p.Author, &p.DisplayName, &p.Body,
		&p.CreatedAt, &visible, &p.HiddenBy, &p.HiddenAt)
	p.Visible = visible != 0
	return p, notFound(err)
}

func (db *DB) CreatePost(ctx context.Context, authorID int64, body string) (Post, error) {
	body = strings.TrimRight(body, " \t\r\n")
	if strings.TrimSpace(body) == "" {
		return Post{}, errors.New("说点什么吧")
	}
	if utf8.RuneCountInString(body) > MaxPostBody {
		return Post{}, errors.New("太长了")
	}
	res, err := db.SQL.ExecContext(ctx,
		`INSERT INTO posts (author_id, body, created_at) VALUES (?, ?, ?)`, authorID, body, nowMS())
	if err != nil {
		return Post{}, err
	}
	id, _ := res.LastInsertId()
	return db.GetPost(ctx, id)
}

func (db *DB) GetPost(ctx context.Context, id int64) (Post, error) {
	return scanPost(db.SQL.QueryRowContext(ctx,
		`SELECT `+postColumns+` FROM posts p JOIN users u ON u.id=p.author_id WHERE p.id=?`, id))
}

// ListPosts walks newest-first with a cursor. includeHidden is only meaningful
// for a moderator viewing their own audit surface.
func (db *DB) ListPosts(ctx context.Context, limit int, after int64, authorID int64, includeHidden bool) ([]Post, error) {
	if limit <= 0 {
		limit = DefaultPostPage
	} else if limit > MaxPostPage {
		limit = MaxPostPage
	}
	q := `SELECT ` + postColumns + ` FROM posts p JOIN users u ON u.id=p.author_id WHERE 1=1`
	args := []any{}
	if !includeHidden {
		q += ` AND p.visible=1`
	}
	if authorID != 0 {
		q += ` AND p.author_id=?`
		args = append(args, authorID)
	}
	if after != 0 {
		q += ` AND p.id<?`
		args = append(args, after)
	}
	q += ` ORDER BY p.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.SQL.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Post, 0, limit)
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePost removes an entry, and only its own author may do it here; hiding
// someone else's post is a moderation action and goes through SetPostHidden.
func (db *DB) DeletePost(ctx context.Context, id, authorID int64) (bool, error) {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM posts WHERE id=? AND author_id=?`, id, authorID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeletePostAny is the moderation path: remove someone else's entry outright.
func (db *DB) DeletePostAny(ctx context.Context, id int64) (bool, error) {
	res, err := db.SQL.ExecContext(ctx, `DELETE FROM posts WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetPostHidden hides or restores an entry. Hidden entries stay in the table so
// the decision is reversible and the author can still see what was taken down.
func (db *DB) SetPostHidden(ctx context.Context, id, moderatorID int64, hidden bool) (bool, error) {
	// The guard makes the action idempotent-safe: hiding an already hidden entry
	// affects no rows, so the caller learns the post was not in that state.
	where := "AND visible=1"
	if !hidden {
		where = "AND visible=0"
	}
	res, err := db.SQL.ExecContext(ctx,
		`UPDATE posts SET visible=?, hidden_by=?, hidden_at=? WHERE id=? `+where,
		boolInt(!hidden), nullInt64(moderatorID), nullInt64(nowMSIf(hidden)), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// nowMSIf returns the current time only while hiding, so a restore does not
// leave a stale timestamp behind.
func nowMSIf(hidden bool) int64 {
	if hidden {
		return nowMS()
	}
	return 0
}

func (db *DB) CountPosts(ctx context.Context) (int, error) {
	var n int
	err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM posts WHERE visible=1`).Scan(&n)
	return n, err
}
