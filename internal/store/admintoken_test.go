package store

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
)

// 运维令牌这一套的全部意义就是"一个串只值一次动作"。这几条用例盯着三件事：用过即废、
// 废了不会因为重启复活、库里不留明文。

func TestAdminTokenIsSingleUse(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.ArmAdminToken(ctx, "flag-token-1"); err != nil {
		t.Fatal(err)
	}
	// 不对的串不能改动任何状态：那一枚可用的必须还在。
	if _, ok, err := db.SpendAdminToken(ctx, "guess", "bearer"); err != nil || ok {
		t.Fatalf("错误令牌 = %v %v", ok, err)
	}
	next, ok, err := db.SpendAdminToken(ctx, "flag-token-1", "claim")
	if err != nil || !ok {
		t.Fatalf("正确令牌第一次就该能用：%v %v", ok, err)
	}
	if next == "" || next == "flag-token-1" {
		t.Fatalf("用完没有另生成一枚：%q", next)
	}
	// 同一个串第二次不再算数——这就是"无限修改"被堵死的那一下。
	if _, ok, err := db.SpendAdminToken(ctx, "flag-token-1", "claim"); err != nil || ok {
		t.Fatalf("用过之后又生效了：%v %v", ok, err)
	}
	// 新生成的那一枚可用，且也是一次性的。
	third, ok, err := db.SpendAdminToken(ctx, next, "bearer POST /problems")
	if err != nil || !ok {
		t.Fatalf("第二枚不可用：%v %v", ok, err)
	}
	if _, ok, _ := db.SpendAdminToken(ctx, next, "bearer"); ok {
		t.Fatal("第二枚被用了两次")
	}
	if _, ok, err := db.SpendAdminToken(ctx, "", "x"); err != nil || ok {
		t.Errorf("空串不该报错也不该通过：%v %v", ok, err)
	}
	_ = third
}

func TestArmRetiresThePreviousToken(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	first, err := db.MintAdminToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.MintAdminToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("两次生成拿到同一个串")
	}
	// 只留一枚可用的：上一枚没人用也作废，否则一次重启就多一个能用的串，
	// "一次性"数不过来也就无从审计。
	if _, ok, _ := db.SpendAdminToken(ctx, first, "bearer"); ok {
		t.Fatal("被替换掉的那一枚还能用")
	}
	if _, ok, _ := db.SpendAdminToken(ctx, second, "bearer"); !ok {
		t.Fatal("当前那一枚反而不可用")
	}
	if n, err := db.AdminTokenArmed(ctx); err != nil || !n {
		t.Fatalf("状态读错了：%v %v", n, err)
	}
}

func TestEnsureAdminTokenArmsTheFlagOnlyOnce(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	tok, fromFlag, err := db.EnsureAdminToken(ctx, "env-token")
	if err != nil {
		t.Fatal(err)
	}
	if !fromFlag || tok != "env-token" {
		t.Fatalf("全新的库该认启动参数那一个：%q fromFlag=%v", tok, fromFlag)
	}
	// 用掉它：这一笔同时生成下一枚可用的。
	next, ok, err := db.SpendAdminToken(ctx, "env-token", "claim")
	if err != nil || !ok {
		t.Fatalf("第一次接管失败：%v %v", ok, err)
	}
	// 重启，env 里还是那一个用过的串：已经有可用令牌，Ensure 不该再动任何东西。
	again, fromFlag, err := db.EnsureAdminToken(ctx, "env-token")
	if err != nil {
		t.Fatal(err)
	}
	if again != "" || fromFlag {
		t.Fatalf("已经有可用令牌时又生成了一枚：%q fromFlag=%v", again, fromFlag)
	}
	if _, ok, _ := db.SpendAdminToken(ctx, "env-token", "claim"); ok {
		t.Fatal("用掉的启动参数又能动了")
	}
	if _, ok, _ := db.SpendAdminToken(ctx, next, "claim"); !ok {
		t.Fatal("用完旧串时生成的那一枚不可用")
	}

	// 库里一行都没有可用的了（被人手工改过、或那种"用了但没记下替代品"的极端状态），
	// 这时重启**也不能**把启动参数那个串捡回来——那正是这次要堵的复活路径。
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE admin_tokens SET used_at=1, used_by='forced' WHERE used_at=0`); err != nil {
		t.Fatal(err)
	}
	fresh, fromFlag, err := db.EnsureAdminToken(ctx, "env-token")
	if err != nil {
		t.Fatal(err)
	}
	if fromFlag || fresh == "env-token" || fresh == "" {
		t.Fatalf("用过的启动参数被复活了：%q fromFlag=%v", fresh, fromFlag)
	}
	if _, ok, _ := db.SpendAdminToken(ctx, fresh, "claim"); !ok {
		t.Fatal("现场生成的那一枚不可用")
	}
}

func TestEnsureAdminTokenLeavesAnArmedOneAlone(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	first, fromFlag, err := db.EnsureAdminToken(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if fromFlag || len(first) < 32 {
		t.Fatalf("没给启动参数就该现场生成一枚：%q fromFlag=%v", first, fromFlag)
	}
	// 还没被用过：下一次启动不该换掉它，否则运维照着日志抄的那个串会莫名其妙失效。
	second, fromFlag, err := db.EnsureAdminToken(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if second != "" || fromFlag {
		t.Fatalf("已有可用令牌时不该再生成：%q fromFlag=%v", second, fromFlag)
	}
	if _, ok, _ := db.SpendAdminToken(ctx, first, "claim"); !ok {
		t.Fatal("重启把还没用的令牌换掉了")
	}
}

// 这张表会被备份、被 dump、被人读到：里面只可能是哈希，不可能是那串能直接用的东西。
func TestAdminTokensAreStoredAsHashes(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.ArmAdminToken(ctx, "super-secret-token"); err != nil {
		t.Fatal(err)
	}
	var hash, used string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT hash, used_by FROM admin_tokens WHERE used_at=0`).Scan(&hash, &used); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "super-secret-token") {
		t.Fatal("库里存了明文")
	}
	if len(hash) != 64 {
		t.Fatalf("不像 sha256 的十六进制：%d 位", len(hash))
	}
	if _, err := hex.DecodeString(hash); err != nil {
		t.Fatalf("哈希串不合法：%v", err)
	}
	// 用掉之后要留下"用它做了什么"，这是事后唯一能说清那次动作是谁的线索。
	if _, ok, err := db.SpendAdminToken(ctx, "super-secret-token", "claim"); err != nil || !ok {
		t.Fatalf("接管失败：%v %v", ok, err)
	}
	var usedBy string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT used_by FROM admin_tokens WHERE used_at<>0 ORDER BY id LIMIT 1`).Scan(&usedBy); err != nil {
		t.Fatal(err)
	}
	if usedBy != "claim" {
		t.Errorf("used_by = %q", usedBy)
	}
}

func TestArmRejectsEmptyToken(t *testing.T) {
	db := openTestDB(t)
	if err := db.ArmAdminToken(context.Background(), "   "); err == nil {
		t.Fatal("空令牌被武装了")
	}
}
