package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// 运维令牌改成一次性之后，最要紧的一条证据是：**同一个串第二次不动作**。以前它是启动
// 参数里的固定串，谁拿到就能无限次改掉 admin 的密码、无限次以运维身份建题——泄露一次
// 等于永久交出这个站，而且看不出来有没有人用过。
//
// 断言都用要运维身份才打得开的页面（/admin/perms），不用公开页：GET /problems 匿名也
// 是 200，拿它测鉴权只会测出一个假绿。

func TestOperatorTokenIsSingleUseOnTheBearerPath(t *testing.T) {
	h := newHarness(t)
	token := h.adminToken()

	if code, _ := h.htmlGet("/admin/perms", token); code != http.StatusOK {
		t.Fatalf("第一次用该能进控制台 = %d", code)
	}
	// 同一串第二次：不再是当前那一枚，按"没有有效凭据"处理。
	if code, _ := h.htmlGet("/admin/perms", token); code != http.StatusUnauthorized {
		t.Fatalf("用过的运维令牌第二次还能动 = %d", code)
	}
	// 用掉它的时候服务端生成了下一枚，所以运维不会被锁在门外。
	if code, _ := h.htmlGet("/admin/perms", h.adminToken()); code != http.StatusOK {
		t.Fatalf("下一枚不可用 = %d", code)
	}
	// 一次性这件事要能在库里查出来：三次核对之后，表里应当躺着"用过的"若干行，而当前
	// 可用的始终只有一行。
	var used, armed int
	if err := h.db.SQL.QueryRowContext(ctx(),
		`SELECT COUNT(*), IFNULL(SUM(CASE WHEN used_at=0 THEN 1 ELSE 0 END), 0) FROM admin_tokens`).
		Scan(&used, &armed); err != nil {
		t.Fatal(err)
	}
	if armed != 1 {
		t.Errorf("当前可用的令牌不是恰好一枚：%d", armed)
	}
	if n := strings.Count(hist(t, h), "/admin/perms"); n < 2 {
		t.Errorf("用掉的令牌没有各自记下用它做了什么：%s", hist(t, h))
	}
	// 成功的 bearer 动作不写审计行（每一步改动各自都有条目，见 admintoken.go 里的理由）。
	if rows := h.auditRows("admin.token.use"); len(rows) != 0 {
		t.Errorf("用运维令牌读了一页就进审计表，会把真正的安全事件冲淡：%+v", rows)
	}
}

// hist 把令牌表里"用它做了什么"那一列读成一段文本，方便断言。
func hist(t *testing.T, h *harness) string {
	t.Helper()
	rows, err := h.db.SQL.QueryContext(ctx(), `SELECT used_by FROM admin_tokens`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		b.WriteString(s + "\n")
	}
	return b.String()
}

// 建题是这枚令牌最值钱的用途之一：同一个串连建两题必须失败，而且要能在库里看不出来。
func TestOperatorTokenCannotAuthorTwiceWithOneValue(t *testing.T) {
	h := newHarness(t)
	token := h.adminToken()

	if code, _ := h.postBearer("/problems", url.Values{"pid": {"P7001"}, "title": {"一次性"}, "time_ms": {"1000"}, "mem_mb": {"256"}, "visible": {"1"}}, token); code != http.StatusOK {
		t.Fatalf("第一次建题 = %d", code)
	}
	code, body := h.postBearer("/problems", url.Values{"pid": {"P7002"}, "title": {"第二次"}, "time_ms": {"1000"}, "mem_mb": {"256"}, "visible": {"1"}}, token)
	if _, err := h.db.FindProblem(ctx(), "P7002"); err == nil {
		t.Fatalf("同一个运维令牌连建了两道题（第二次 %d）：\n%s", code, short(body))
	}
	// 被弹回登录页就是"这一串不算凭据了"的可见形状。
	if !strings.Contains(body, "登录") {
		t.Errorf("重用的那次没有弹回登录：\n%s", short(body))
	}
}

// 猜令牌要有代价。生产上那一串是 192 位随机值，猜不出来，但运维可能自己写一个短的，
// 所以失败计数照旧：超过预算之后，连真的那一枚也先拒。
func TestOperatorTokenGuessingIsThrottled(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < claimAttempts; i++ {
		if code, _ := h.htmlGet("/admin/perms", "guess-"+string(rune('a'+i))); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次乱猜 = %d，想要 401", i+1, code)
		}
	}
	// 预算用完之后，真的那一枚也先拒：403「需要管理令牌」而不是 401，说的是"你给了凭据，
	// 但这一路暂时对你关着"。
	if code, _ := h.htmlGet("/admin/perms", h.adminToken()); code != http.StatusForbidden {
		t.Fatalf("用完预算之后的真令牌 = %d，应该照样被闸门挡住", code)
	}
	if rows := h.auditRows("admin.token.denied"); len(rows) != claimAttempts {
		t.Errorf("失败的那几条没记全：%d 行", len(rows))
	}
}

// 判机令牌是另一回事：它是一台远端机器要预先知道的长期共享串，不该被运维那一路的
// 一次性规则牵连。这里连打三次，确认它还是"每次都能用"。
func TestJudgeTokenIsNotRotated(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < 3; i++ {
		code, _ := h.postBearer("/judge/claim", url.Values{}, h.judge)
		// 空队列是 404：关键是它没有被当成鉴权失败（401/403）。
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Fatalf("第 %d 次判机心跳被拒 = %d", i+1, code)
		}
	}
}

// 库里只能有哈希：令牌出现在 HTTP 请求里，但绝不落进数据库的任何一列。
func TestOperatorTokenNeverStoredInPlaintext(t *testing.T) {
	h := newHarness(t)
	token := h.adminToken()
	if _, _, err := h.db.SpendAdminToken(ctx(), token, "bearer GET /admin/perms"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.db.SQL.QueryRowContext(ctx(),
		`SELECT COUNT(*) FROM admin_tokens WHERE hash=? OR used_by LIKE ?`, token, "%"+token+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("库里有 %d 行带着那串明文", n)
	}
}

// 开机那一步（main.go 调的 ArmAdminToken）在有 HTTP 的地方测得到：全新库里，启动参数
// 那一枚可用；用掉之后再"开机"一次，绝不能把它捡回来。
func TestArmAdminTokenFollowsTheStartupRules(t *testing.T) {
	h := newHarness(t)

	// 库里一行都没有（harness 不调 ArmAdminToken，也没人生成过）：全新库认启动参数。
	if _, _, err := h.db.EnsureAdminToken(ctx(), h.cfg.AdminToken); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.htmlGet("/admin/perms", h.cfg.AdminToken); code != http.StatusOK {
		t.Fatalf("全新库该用启动参数那一枚 = %d", code)
	}
	if _, _, err := h.db.SpendAdminToken(ctx(), h.cfg.AdminToken, "claim"); err != nil {
		t.Fatal(err)
	}
	// 模拟"拿同一个 env 值重启"：先跑一遍和 main.go 相同的两步。
	if armed, err := h.db.AdminTokenArmed(ctx()); err != nil || !armed {
		t.Fatalf("用掉之后应该已经有一枚新的可用：%v %v", armed, err)
	}
	h.app.ArmAdminToken(ctx())
	if _, ok, _ := h.db.SpendAdminToken(ctx(), h.cfg.AdminToken, "claim"); ok {
		t.Fatal("重启把用掉的启动参数复活了")
	}
}

// 令牌被烧掉之后，网页上的运维入口不该再有任何一点用：接管页给出的那句话要说清"这一枚
// 已经用过了"，而不是让人以为密码写错了。
func TestUsedTokenMessageSaysItIsSpent(t *testing.T) {
	h := newHarness(t)
	token := h.adminToken()
	if _, err := h.db.EnsureUser(ctx(), claimHandle, "admin"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.postNoRedirect("/admin/claim", url.Values{"token": {token}, "password": {testPassword}}, ""); code != http.StatusSeeOther {
		t.Fatalf("第一次接管 = %d", code)
	}
	if _, page := h.postForm2("/admin/claim", url.Values{"token": {token}, "password": {testPassword}}, "", ""); !strings.Contains(page, "已经用过") {
		t.Errorf("重用的接管尝试没说清原因：\n%s", short(page))
	}
	var n int
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT COUNT(*) FROM admin_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("用掉一枚却没有生成下一枚（表里 %d 行）", n)
	}
}
