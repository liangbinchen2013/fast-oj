package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// admintoken.go 是"运维令牌"这一样东西的全部状态：一次只能用一回，用完由服务端当场
// 再生成一枚。
//
// 为什么不能像以前那样拿启动参数里的固定串一直用：那一串能做这个站最狠的动作——在
// /admin/claim 随时改掉 admin 的密码（改完就是超级管理员），以及以运维身份走 bearer
// 建题、传用例、读判题数据。而它可能出现在这些地方：备份里的 env 文件、能读 /proc 或 ps
// 的本地账号、误贴到聊天或 issue 里的一次。固定串 = 一次泄露永久有效，而且没人知道有没有
// 人用过。换成一次性之后，泄露最多换来一次动作，并且每次使用都留下一行审计。
//
// 库里只存 sha256：这张表会被备份、被 dump、被人读到，暴露面比 0600 的 env 大得多；
// 验证只需要"把对方给的串哈希一次再比"，不需要能还原。明文只在生成那一行的日志里出现一次
// ——那是它唯一的投递通道，也是 run.sh 早就在做的事。

// adminTokenBytes 是一枚令牌的熵：24 字节 = 192 位，base64url 之后 32 个字符。
// 和会话 id 同一档强度：拿它的人不需要猜测，反过来猜它的人需要天文数字次尝试。
const adminTokenBytes = 24

// NewAdminToken 生成一枚令牌明文。
func NewAdminToken() (string, error) {
	buf := make([]byte, adminTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("admin token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// adminTokenHash 是把明文折成库里那一句。
func adminTokenHash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// MintAdminToken 生成一枚新的明文令牌并把它设为当前可用的那一枚：原先还留着没用的
// 那枚同时作废（否则一次重启就多一个能用的串，"一次性"成了空话）。返回明文是给
// 调用方打印一次用的，库里不留它。
func (db *DB) MintAdminToken(ctx context.Context) (string, error) {
	plain, err := NewAdminToken()
	if err != nil {
		return "", err
	}
	if err := db.ArmAdminToken(ctx, plain); err != nil {
		return "", err
	}
	return plain, nil
}

// ArmAdminToken 把 given 设为当前可用的那枚令牌（启动参数走这一条）。
//
// 明文进、哈希存，然后立刻丢掉；调用方要留着明文自己去打印。
func (db *DB) ArmAdminToken(ctx context.Context, given string) error {
	given = strings.TrimSpace(given)
	if given == "" {
		return fmt.Errorf("admin token: empty")
	}
	now := nowMS()
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE admin_tokens SET used_at=?, used_by=? WHERE used_at=0`, now, "replaced"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO admin_tokens (hash, created_at) VALUES (?, ?)`, adminTokenHash(given), now)
		return err
	})
}

// SpendAdminToken 核对 presented 是不是当前那一枚：对得上就用掉它，并在同一个事务里
// 生成下一枚，返回下一枚的明文。对不上返回 false（库里什么都不会变，包括那枚可用的）。
//
// 生成失败会连"用掉"一起回滚：宁可这一笔动作不成立，也不能让手上最后一枚令牌被烧掉之后
// 没有下一枚——那等于把自己锁在门外。
//
// purpose 是"用它做了什么"（claim、bearer POST /problems…），落进 used_by 供事后追查。
func (db *DB) SpendAdminToken(ctx context.Context, presented, purpose string) (string, bool, error) {
	if presented == "" {
		return "", false, nil
	}
	hash := adminTokenHash(presented)
	var next string
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM admin_tokens WHERE hash=? AND used_at=0`, hash).Scan(&id)
		if err != nil {
			return err
		}
		plain, err := NewAdminToken()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE admin_tokens SET used_at=?, used_by=? WHERE id=?`, nowMS(), purpose, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO admin_tokens (hash, created_at) VALUES (?, ?)`, adminTokenHash(plain), nowMS()); err != nil {
			return err
		}
		next = plain
		return nil
	})
	if err != nil {
		// 查不到那一行就是"这枚不是当前可用的"，不是错误：它可能早就用过了，也可能
		// 根本不是这个站发的串。两条都按拒绝处理，且不告诉调用方是哪一种。
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return next, true, nil
}

// AdminTokenMatches 只回答"这是不是当前那一枚可用的运维令牌"，不改动任何状态。
//
// 分开写是因为一次 HTTP 请求里"这是谁"可能被问好几遍（CSRF 闸门、限流、处理器各一次），
// 而一次性令牌只值一次动作：核对与核销必须分开，否则第二遍核对会把一个正常请求变成
// "令牌无效"。核销在响应写完之后做一次（见 internal/api/admintoken.go）。
func (db *DB) AdminTokenMatches(ctx context.Context, presented string) (bool, error) {
	if strings.TrimSpace(presented) == "" {
		return false, nil
	}
	var n int
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_tokens WHERE hash=? AND used_at=0`, adminTokenHash(presented)).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// AdminTokenArmed 报告库里现在有没有一枚可用的令牌（正常部署里恒为真；这一条给启动
// 和测试用来确认状态，不该被拿去做判断依据）。
func (db *DB) AdminTokenArmed(ctx context.Context) (bool, error) {
	var n int
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_tokens WHERE used_at=0`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// EnsureAdminToken 是开机时的第一步：保证有一枚可用令牌，并如实回告这一枚是从哪来的。
//
// 规则里最要紧的一条是**用过的绝不复活**：启动参数那一个只在全表为空（这库从没引导过）
// 时才武装进来。之后每次重启都只做"有可用的就不动，没有就现场生成一枚并打印一次"，
// 所以把同一个 env 值重启多少次，都不会让那串已经烧掉的东西重新生效。
//
// 返回值 fromFlag 为真时 token 就是参数里那一个——调用方**不该**把它再打印一遍
// （运维自己写的东西，日志里再出现一次只是多一个泄露面）。
func (db *DB) EnsureAdminToken(ctx context.Context, fromConfig string) (token string, fromFlag bool, err error) {
	var total, armed int
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*), IFNULL(SUM(CASE WHEN used_at=0 THEN 1 ELSE 0 END), 0) FROM admin_tokens`).
		Scan(&total, &armed); err != nil {
		return "", false, err
	}
	if armed > 0 {
		return "", false, nil
	}
	if total == 0 && strings.TrimSpace(fromConfig) != "" {
		if err := db.ArmAdminToken(ctx, fromConfig); err != nil {
			return "", false, err
		}
		return strings.TrimSpace(fromConfig), true, nil
	}
	plain, err := db.MintAdminToken(ctx)
	if err != nil {
		return "", false, err
	}
	return plain, false, nil
}
