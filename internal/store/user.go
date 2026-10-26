package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BootstrapHandle is the account the operator token maps to. It is not a person:
// the row appears the first time anything presents that token, so it can never be
// the one that claims the site.
const BootstrapHandle = "admin"

// userColumns never selects passhash: a hash must not ride along in a struct
// that might get marshalled to JSON.
//
// email_verified reads with IFNULL because "unread" and "unverified" must never be the
// same answer: a row this feature did not write — an account created before M25f, one
// made by 批量建号, the token account — reads as verified, and nobody is locked out by
// a column that was only ever meant to gate new registrations.
// 积分与颜色一起读出来：它们是"这个人现在该显示成什么样子"的一部分，而每个显示名字的地方
// 本来就拿着这一行。分开查会让每一页多一次查询，只为了拿两个已经在 users 上的标量。
const userColumns = `id, handle, IFNULL(email, ''), display_name, bio, role, status, dm_policy,
		(passhash <> '') AS has_password, IFNULL(email_verified, 1), created_at, IFNULL(last_login_at, 0),
		IFNULL(theme, ''), IFNULL(tags, ''), IFNULL(points, 0), IFNULL(points_parts, ''), IFNULL(color, '')`

func scanUser(row scanner) (User, error) {
	var (
		u          User
		hasPass    int
		verifiedMS int
	)
	err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.DisplayName, &u.Bio, &u.Role, &u.Status,
		&u.DMPolicy, &hasPass, &verifiedMS, &u.CreatedAt, &u.LastLoginAt,
		&u.Theme, &u.Tags, &u.Points, &u.PointsParts, &u.Color)
	u.HasPassword = hasPass != 0
	u.EmailVerified = verifiedMS != 0
	return u, notFound(err)
}

// EnsureUser returns the existing row or creates it, so the judge and CLI can
// refer to people by handle without a registration flow.
func (db *DB) EnsureUser(ctx context.Context, handle, role string) (User, error) {
	if handle == "" {
		return User{}, fmt.Errorf("handle must not be empty")
	}
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "admin" {
		return User{}, fmt.Errorf("unknown role %q", role)
	}
	_, err := db.SQL.ExecContext(ctx,
		`INSERT INTO users (handle, role, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(handle) DO NOTHING`, handle, role, nowMS())
	if err != nil {
		return User{}, err
	}
	return db.GetUserByHandle(ctx, handle)
}

func (db *DB) GetUserByHandle(ctx context.Context, handle string) (User, error) {
	return scanUser(db.SQL.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE handle=? COLLATE NOCASE`, handle))
}

func (db *DB) GetUserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(db.SQL.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id=?`, id))
}

func (db *DB) ListUsers(ctx context.Context, limit int) ([]User, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := db.SQL.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// personSQL names a joined user row the way every history view should print it. The
// table alias is a parameter because the same rule has to hold in submissions, the
// leaderboard and follow lists, and each of those joins users under a different name.
//
// 管理员贴的标签也在这里接上：`admin [作弊者]`。这一条表达式是所有列名字视图的唯一出口，
// 接在这里意味着加标签不多一次查询、不改一处模板；注销的那一支在前面先返回，所以被抹掉身份的人
// 不会带着生前那些标签继续显示。逗号在 SQL 里就地换成 `] [`，两个标签就成了 `[a] [b]`。
func personSQL(alias string) string {
	base := `CASE WHEN ` + alias + `.display_name='' THEN ` + alias + `.handle ELSE ` + alias + `.display_name END`
	tags := `CASE WHEN IFNULL(` + alias + `.tags, '')='' THEN '' ELSE ' [' ||
		replace(IFNULL(` + alias + `.tags, ''), ',', '] [') || ']' END`
	return `CASE WHEN ` + alias + `.status='deleted' THEN '` + AnonymousName + `' ELSE ` +
		`(` + base + ` || ` + tags + `) END`
}

// AnonymousName is what every surviving record shows once an account is cancelled.
// The old handle is deliberately not used there: after cancellation that string is
// free for the next registrant, and printing it would attribute the previous
// person's submissions and posts to a stranger.
const AnonymousName = "封禁用户"

// CancelAccount erases an account's identity and frees its login name, keeping every
// record it produced.
//
// 为什么留着这一行：submissions、posts、topics、runs 都靠 user_id 找作者。删行会顺着外键
// 把判题记录一起带走，而要求恰恰是"记录依旧还在，只是标注成封禁用户"。所以这里抹掉的是
// **身份**（用户名、邮箱、密码、昵称、简介），留下的是**行为**。
//
// 用户名换成 banned-<id>：handle 上有 UNIQUE COLLATE NOCASE，而注册页的 validHandle 只收
// ASCII，所以这个串既腾出了原名、又是个人主页仍然打得开的地址，还绝不会跟后人撞车。
// 返回换出来的新 handle，让调用方能把它写进日志与提示里。
func (db *DB) CancelAccount(ctx context.Context, userID int64) (string, error) {
	if err := db.guardOwner(ctx, userID); err != nil {
		return "", err
	}
	handle := fmt.Sprintf("banned-%d", userID)
	var taken int
	if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE handle=?`, handle).Scan(&taken); err != nil {
		return "", err
	}
	if taken > 0 {
		// 有人真注册过 banned-17 这种名字，那就再加一个时间戳。
		handle = fmt.Sprintf("banned-%d-%d", userID, nowMS())
	}
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET handle=?, email=NULL, passhash='', display_name=?, bio='',
			                        status='deleted', dm_policy='off', last_login_at=NULL,
			                        theme='', tags='', points=0, points_parts='', color=''
			 WHERE id=?`, handle, AnonymousName, userID); err != nil {
			return err
		}
		// 个人权限覆盖属于那个已经不存在的身份；会话全部吊销，正在登录的浏览器立刻掉线。
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_permissions WHERE user_id=?`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
		return err
	})
	if err != nil {
		return "", err
	}
	return handle, nil
}

// OwnedCounts answers the question a deletion screen has to ask before it lets go:
// will this leave problems or contests nobody can edit any more?
func (db *DB) OwnedCounts(ctx context.Context, userID int64) (problems, contests int, err error) {
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM problems WHERE owner_id=?`, userID).Scan(&problems); err != nil {
		return 0, 0, err
	}
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM contests WHERE owner_id=?`, userID).Scan(&contests); err != nil {
		return 0, 0, err
	}
	return problems, contests, nil
}

// IsCancelled reports whether the account has already been wiped. The row stays for
// its history and refuses every further identity change, so an admin cannot resurrect
// a name that someone else may already have taken.
func (u User) IsCancelled() bool { return u.Status == "deleted" }

// guardMutable is the shared backstop for the screens that change an account. A
// cancelled one is a tombstone: its name may already belong to the next registrant,
// so nothing about it may be re-edited — including unbanning it.
func (db *DB) guardMutable(ctx context.Context, userID int64) error {
	if err := db.guardOwner(ctx, userID); err != nil {
		return err
	}
	var status string
	err := db.SQL.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "deleted" {
		return ErrAccountCancelled
	}
	return nil
}

// SetUserRole changes the tier. Sessions stay valid; the effective capability
// set is recomputed on the next request, which is what makes a demotion stick
// immediately.
func (db *DB) SetUserRole(ctx context.Context, userID int64, role string) error {
	if role != "user" && role != "admin" {
		return fmt.Errorf("unknown role %q", role)
	}
	if err := db.guardMutable(ctx, userID); err != nil {
		return err
	}
	res, err := db.SQL.ExecContext(ctx, `UPDATE users SET role=? WHERE id=?`, role, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetUserStatus activates or bans an account. Banning also drops its sessions.
func (db *DB) SetUserStatus(ctx context.Context, userID int64, status string) error {
	if status != "active" && status != "banned" {
		return fmt.Errorf("unknown status %q", status)
	}
	if err := db.guardMutable(ctx, userID); err != nil {
		return err
	}
	res, err := db.SQL.ExecContext(ctx, `UPDATE users SET status=? WHERE id=?`, status, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if status != "active" {
		return db.DropUserSessions(ctx, userID)
	}
	return nil
}
