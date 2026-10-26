package store

import (
	"context"
	"errors"
	"strings"
)

// MaxDMBody bounds one private message: shorter than a feed post on purpose, since
// a conversation is not where a paste belongs.
const MaxDMBody = 300

// Who may write to a user. The recipient decides, the sender never does.
const (
	DMPolicyEveryone  = "everyone"
	DMPolicyMutual    = "mutual"
	DMPolicyFollowing = "following"
	DMPolicyOff       = "off"
)

// ErrDMBlocked means the recipient's policy refused the message. The wording is
// left to the caller because the reason differs per policy.
var ErrDMBlocked = errors.New("message refused by the recipient's policy")

// ErrDMEmpty is what it says; sending whitespace is a user error, not a bug.
var ErrDMEmpty = errors.New("empty message")

// ErrDMTooLong is a sentinel so the caller can phrase the limit in the reader's
// language instead of leaking an error string into the page.
var ErrDMTooLong = errors.New("message too long")

func ValidDMPolicy(p string) bool {
	switch p {
	case DMPolicyEveryone, DMPolicyMutual, DMPolicyFollowing, DMPolicyOff:
		return true
	}
	return false
}

// DM is one message. FromMe is filled by the caller: the store knows the sender's
// id, not whose screen this is being rendered on.
type DM struct {
	ID          int64  `json:"id"`
	SenderID    int64  `json:"sender_id"`
	Sender      string `json:"sender"`
	RecipientID int64  `json:"recipient_id"`
	Recipient   string `json:"recipient"`
	Body        string `json:"body"`
	CreatedAt   int64  `json:"created_at"`
	ReadAt      int64  `json:"read_at,omitempty"`
	FromMe      bool   `json:"from_me"`
}

// DMThread is one row of the inbox: who, the last message, and how many are unread.
type DMThread struct {
	PeerID   int64  `json:"peer_id"`
	Peer     string `json:"peer"`
	PeerName string `json:"peer_name"`
	// PeerColor 是这个人的活跃度档位（见 feed.go 里同名的理由）。
	PeerColor string `json:"peer_color,omitempty"`
	Last      string `json:"last"`
	LastAt    int64  `json:"last_at"`
	Unread    int    `json:"unread"`
	FromMe    bool   `json:"from_me"`
}

const dmColumns = `d.id, d.sender_id, su.handle, d.recipient_id, ru.handle, d.body, d.created_at,
		IFNULL(d.read_at, 0)`

func scanDM(row scanner) (DM, error) {
	var m DM
	err := row.Scan(&m.ID, &m.SenderID, &m.Sender, &m.RecipientID, &m.Recipient, &m.Body, &m.CreatedAt,
		&m.ReadAt)
	return m, notFound(err)
}

func (db *DB) SetDMPolicy(ctx context.Context, userID int64, policy string) error {
	if !ValidDMPolicy(policy) {
		return errors.New("unknown message policy")
	}
	_, err := db.SQL.ExecContext(ctx, `UPDATE users SET dm_policy=? WHERE id=?`, policy, userID)
	return err
}

// MayMessage decides whether `sender` may write to `recipient`, given the
// recipient's policy. Mutual and following are read from the follow graph, so this
// is the one place that knows what a stranger is.
func (db *DB) MayMessage(ctx context.Context, senderID, recipientID int64) (bool, error) {
	var policy string
	if err := db.SQL.QueryRowContext(ctx, `SELECT dm_policy FROM users WHERE id=?`, recipientID).
		Scan(&policy); err != nil {
		return false, notFound(err)
	}
	switch policy {
	case DMPolicyOff:
		return false, nil
	case DMPolicyEveryone:
		return true, nil
	case DMPolicyFollowing:
		return db.IsFollowing(ctx, recipientID, senderID)
	case DMPolicyMutual:
		return db.AreMutual(ctx, recipientID, senderID)
	}
	return false, nil
}

// CleanDMBody is the message content rule, split out so a caller can check the
// text before spending a rate-limit slot: telling someone "too fast" when their
// message was simply too long sends them off to fix the wrong thing.
func CleanDMBody(raw string) (string, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return "", ErrDMEmpty
	}
	if len([]rune(body)) > MaxDMBody {
		return "", ErrDMTooLong
	}
	return body, nil
}

func (db *DB) SendDM(ctx context.Context, senderID, recipientID int64, raw string) (DM, error) {
	body, err := CleanDMBody(raw)
	if err != nil {
		return DM{}, err
	}
	res, err := db.SQL.ExecContext(ctx,
		`INSERT INTO dms (sender_id, recipient_id, body, created_at) VALUES (?, ?, ?, ?)`,
		senderID, recipientID, body, nowMS())
	if err != nil {
		return DM{}, err
	}
	id, _ := res.LastInsertId()
	return db.GetDM(ctx, id)
}

func (db *DB) GetDM(ctx context.Context, id int64) (DM, error) {
	return scanDM(db.SQL.QueryRowContext(ctx,
		`SELECT `+dmColumns+` FROM dms d
		 JOIN users su ON su.id=d.sender_id JOIN users ru ON ru.id=d.recipient_id WHERE d.id=?`, id))
}

// ListDMThreads walks the inbox by most recent activity. The CTE collapses each
// conversation to its newest message so the page costs two indexed passes rather
// than one scan per person.
func (db *DB) ListDMThreads(ctx context.Context, meID int64, limit int) ([]DMThread, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := db.SQL.QueryContext(ctx,
		`WITH m AS (
		   SELECT CASE WHEN sender_id=? THEN recipient_id ELSE sender_id END AS peer,
		          id, body, created_at, sender_id,
		          CASE WHEN recipient_id=? AND read_at IS NULL THEN 1 ELSE 0 END AS unread
		     FROM dms WHERE sender_id=? OR recipient_id=?
		 ), g AS (
		   SELECT peer, MAX(id) AS last_id, SUM(unread) AS unread FROM m GROUP BY peer
		   ORDER BY last_id DESC LIMIT ?
		 )
		 SELECT g.peer, g.unread, m.body, m.created_at, m.sender_id, u.handle, `+personSQL(`u`)+`, `+colorSQL(`u`)+`
		   FROM g JOIN m ON m.id=g.last_id JOIN users u ON u.id=g.peer
		  ORDER BY g.last_id DESC`, meID, meID, meID, meID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DMThread
	for rows.Next() {
		var (
			t        DMThread
			senderID int64
		)
		if err := rows.Scan(&t.PeerID, &t.Unread, &t.Last, &t.LastAt, &senderID, &t.Peer, &t.PeerName, &t.PeerColor); err != nil {
			return nil, err
		}
		t.FromMe = senderID == meID
		if t.PeerName == "" {
			t.PeerName = t.Peer
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListDMConversation returns the last `limit` messages between two people, oldest
// first, so the page can print them in reading order.
func (db *DB) ListDMConversation(ctx context.Context, meID, peerID int64, limit int) ([]DM, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT `+dmColumns+` FROM dms d
		 JOIN users su ON su.id=d.sender_id JOIN users ru ON ru.id=d.recipient_id
		 WHERE (d.sender_id=? AND d.recipient_id=?) OR (d.sender_id=? AND d.recipient_id=?)
		 ORDER BY d.id DESC LIMIT ?`, meID, peerID, peerID, meID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DM
	for rows.Next() {
		m, err := scanDM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// MarkDMRead is scoped to one conversation and only ever touches the reader's own
// unreceived messages, so reading cannot mark someone else's inbox.
func (db *DB) MarkDMRead(ctx context.Context, meID, peerID int64) (int, error) {
	res, err := db.SQL.ExecContext(ctx,
		`UPDATE dms SET read_at=? WHERE recipient_id=? AND sender_id=? AND read_at IS NULL`,
		nowMS(), meID, peerID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountDMUnread backs the nav-bar badge; the partial index means read messages do
// not slow it down as the archive grows.
func (db *DB) CountDMUnread(ctx context.Context, meID int64) (int, error) {
	var n int
	err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dms WHERE recipient_id=? AND read_at IS NULL`, meID).Scan(&n)
	return n, err
}
