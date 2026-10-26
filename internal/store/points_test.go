package store

import (
	"context"
	"errors"
	"testing"
)

// 积分这一套的重点不在"能不能算出一个数"，而在管理员调的那些数字会不会把算法带坏：
// 空框、字母、比上一档还低的门槛、刷到天上去的提交数，都是人真会做出来的事。

func TestPointsConfigParseAndNormalize(t *testing.T) {
	// 一行都没有 = 全套默认值（一个新站点从没进过 /admin/points 也要能算分）。
	if got := ParsePoints(nil); got != DefaultPoints() {
		t.Fatalf("默认 = %+v", got)
	}
	// 空串、字母、越界：每一项各自退回自己那一行的默认，而不是整页报废。
	got := ParsePoints(map[string]string{
		"points.enabled":         "0",
		"points.refresh_minutes": "abc",
		"points.decay_per_day":   "999",
		"points.problem_unit":    "",
		"points.submit_unit":     "0",
		"points.problem_cap":     "-5",
	})
	d := DefaultPoints()
	if got.Enabled {
		t.Error("enabled=0 没关掉")
	}
	if got.RefreshMinutes != d.RefreshMinutes || got.ProblemCap != d.ProblemCap {
		t.Errorf("坏值没退回默认：refresh=%d cap=%d", got.RefreshMinutes, got.ProblemCap)
	}
	// 999 天%的衰减会被 clamp 到 90，而不是让总分变成 0.0001 倍。
	if got.DecayPerDay > 90 {
		t.Errorf("decay_per_day = %d，应该被卡在 90 以内", got.DecayPerDay)
	}
	// 单位分填 0 不算"关掉这一档"：那是把框清空时最常见的误伤，所以退回默认。
	if got.SubmitUnit == 0 || got.ProblemUnit == 0 {
		t.Errorf("单位分为 0：%+v", got)
	}
	// 门槛必须单调：绿被抬到 200 之后，橙和红不能还停在默认那 80、160 上。
	m := ParsePoints(map[string]string{
		"points.color_green":  "200",
		"points.color_orange": "50",
		"points.color_red":    "60",
	})
	if !(m.Green <= m.Orange && m.Orange <= m.Red) {
		t.Fatalf("档位不单调：green=%d orange=%d red=%d", m.Green, m.Orange, m.Red)
	}
	// 存的和读的是一对：保存之后再读回来必须一模一样。
	want := PointsConfig{Enabled: true, RefreshMinutes: 7, DecayPerDay: 3,
		ProblemUnit: 9, ProblemCap: 300, SubmitUnit: 2, SubmitCap: 40,
		ContestUnit: 15, ContestCap: 150, CommunityUnit: 3, CommunityCap: 90,
		Green: 25, Orange: 70, Red: 150}
	if got := ParsePoints(want.Writes()); got != want.Normalized() {
		t.Fatalf("回读 = %+v，想要 %+v", got, want.Normalized())
	}
}

func TestColorForFollowsTiers(t *testing.T) {
	cfg := DefaultPoints() // 30/80/160
	for _, c := range []struct {
		points int
		want   string
	}{{0, "blue"}, {29, "blue"}, {30, "green"}, {79, "green"}, {80, "orange"}, {160, "red"}, {9999, "red"}} {
		if got := cfg.ColorFor(c.points); got != c.want {
			t.Errorf("%d 分 → %s，想要 %s", c.points, got, c.want)
		}
	}
}

// 衰减挂在重算那一步：闲得越久分越少，但明细里那几个数还是原始的那份，
// 这样管理员看到的是"他做过什么"，页面上看到的是"现在还值多少"。
func TestDecayOnlyMovesTheTotal(t *testing.T) {
	cfg := DefaultPoints()
	cfg.DecayPerDay = 10
	cfg.ProblemCap, cfg.SubmitCap, cfg.ContestCap, cfg.CommunityCap = 0, 0, 0, 0 // 不设上限才看得出衰减
	fresh := activity{ac: 10, subs: 10, lastAt: nowMS()}
	got := cfg.totals(fresh)
	if got.Total != 110 || got.Decayed {
		t.Fatalf("刚活动过 = %+v", got)
	}
	idle := fresh
	idle.lastAt = nowMS() - 3*24*3600*1000
	after := cfg.totals(idle)
	// 110 × 0.9³ ≈ 80。
	if after.Total < 78 || after.Total > 82 {
		t.Fatalf("闲三天 = %d，应该在 80 附近", after.Total)
	}
	if after.Dims[0].Score != 100 {
		t.Errorf("明细被衰减改写了：ac 档 = %d", after.Dims[0].Score)
	}
	if after.Idle < 3 {
		t.Errorf("闲了多少天 = %d", after.Idle)
	}
	// 关掉衰减（0）就是原样。
	off := cfg
	off.DecayPerDay = 0
	if got := off.totals(idle); got.Total != 110 {
		t.Fatalf("decay=0 还在掉分：%d", got.Total)
	}
}

// 上限是这套东西不被刷爆的关键：一个人提交一万次，那一档也最多只值 cap。
func TestDimCapsClampEachDimension(t *testing.T) {
	cfg := DefaultPoints()
	got := cfg.totals(activity{ac: 99999, subs: 99999, contests: 99999, community: 99999, lastAt: nowMS()})
	want := cfg.ProblemCap + cfg.SubmitCap + cfg.ContestCap + cfg.CommunityCap
	if got.Total != want {
		t.Fatalf("总分 %d，四个上限之和 %d", got.Total, want)
	}
	if got.Dims[0].Count != 99999 {
		t.Errorf("明细丢了原始次数：%+v", got.Dims[0])
	}
}

func TestParsePartsDropsGarbage(t *testing.T) {
	dims := ParseParts("ac=120,sub=abc,cts=40,没有等号,com=-1")
	if len(dims) != 2 || dims[0].Key != "ac" || dims[1].Key != "cts" {
		t.Fatalf("解析结果 = %+v", dims)
	}
	if dims[0].Label != "做题" {
		t.Errorf("维度没有中文名：%+v", dims[0])
	}
}

// submitAC 走一遍真的判题回写，这样测试数的是 submissions 里真实存在的那种行。
func submitAC(t *testing.T, db *DB, u User, p Problem) {
	t.Helper()
	ctx := context.Background()
	s, err := db.CreateSubmission(ctx, &Submission{
		UserID: u.ID, ProblemID: p.ID, Language: "cpp", Source: "int main(){}", TotalScore: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordResult(ctx, s.ID, Result{Verdict: "AC", TimeMS: 5, MemKB: 1024,
		Score: 100, TotalScore: 100}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshPointsScoresColorsAndLocks(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	solver := mustUser(t, db, "solver")
	fresh := mustUser(t, db, "fresh")
	p1, p2 := mustProblem(t, db, "P1", "一"), mustProblem(t, db, "P2", "二")
	submitAC(t, db, solver, p1)
	submitAC(t, db, solver, p2)

	cfg := DefaultPoints() // ac 10/题、sub 1/次，绿 30
	if n, err := db.RefreshPoints(ctx, cfg); err != nil || n == 0 {
		t.Fatalf("重算 = %d 人, %v", n, err)
	}
	got, err := db.GetUserByID(ctx, solver.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 2 题 ×10 + 2 次 ×1 = 22，还没到绿的 30：库里存空串，渲染成 blue。
	if got.Points != 22 || got.Color != ColorAuto {
		t.Fatalf("22 分的人 = %+v", got)
	}
	if s := got.ColorClass(); s != "blue" {
		t.Errorf("最低一档渲染成 %s", s)
	}
	if parts := got.Dims(); len(parts) != 4 || parts[0].Score != 20 {
		t.Errorf("明细 = %+v", parts)
	}
	// 什么都没干的人是 0 分，不是 NULL、也不是负数。
	if quiet, err := db.GetUserByID(ctx, fresh.ID); err != nil || quiet.Points != 0 {
		t.Fatalf("没活动的人 = %+v, %v", quiet, err)
	}

	// 再让他发五条动态：社区档 5×2=10，总分 32 → 过绿线。
	for i := 0; i < 5; i++ {
		if _, err := db.CreatePost(ctx, solver.ID, "动态一条"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetUserByID(ctx, solver.ID)
	if got.Points != 32 || got.Color != "green" {
		t.Fatalf("过线之后 = %+v", got)
	}

	// 管理员手动锁成紫色：重算继续动分值，但颜色归他管，自动那一路不碰。
	if err := db.SetUserColor(ctx, solver.ID, ColorAdmin); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, err := db.CreatePost(ctx, solver.ID, "水一条"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetUserByID(ctx, solver.ID)
	if got.Color != ColorAdmin {
		t.Fatalf("紫色被自动重算改掉了：%q", got.Color)
	}
	// 锁色只锁颜色，不锁分：社区档 45×2=90，总分 20+2+90=112，早就过了红线。
	if got.Points != 112 {
		t.Fatalf("锁了颜色就不算了？分值 = %d", got.Points)
	}
	// 紫色那档在页面上就是紫色，跟分数无关。
	if got.ColorClass() != "purple" {
		t.Errorf("紫色渲染成 %s", got.ColorClass())
	}

	// 棕色的来路也一样：管理员指定，自动不覆盖。
	if err := db.SetUserColor(ctx, fresh.ID, ColorPunish); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if still, err := db.GetUserByID(ctx, fresh.ID); err != nil || still.Color != ColorPunish {
		t.Fatalf("棕色没了：%+v, %v", still, err)
	}

	// 四档之外的颜色不能让管理员手输进来：green/orange/red 是算出来的，不是选的。
	if err := db.SetUserColor(ctx, solver.ID, "red"); err == nil {
		t.Error("接受了自动档位作为手动颜色")
	}

	// 封掉的账号不在重算范围内：它已经没有名字要显示了。
	if err := db.SetUserStatus(ctx, solver.ID, "banned"); err != nil {
		t.Fatal(err)
	}
	before := got.Points
	if _, err := db.CreatePost(ctx, fresh.ID, "被禁的人再发也没用"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetUserByID(ctx, solver.ID)
	if after.Points != before {
		t.Errorf("封禁中的账号还在被重算：%d → %d", before, after.Points)
	}
}

// 关掉总开关：分值停在最后一次算出的样子，颜色收回去（紫/棕照旧）。
func TestRefreshPointsWhenDisabled(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	u := mustUser(t, db, "active")
	p := mustProblem(t, db, "P1", "一")
	submitAC(t, db, u, p)
	cfg := DefaultPoints()
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserColor(ctx, u.ID, ColorAdmin); err != nil {
		t.Fatal(err)
	}
	other := mustUser(t, db, "other")
	// 二十条动态 = 社区档 40 分，过绿线，这样"关掉之后颜色收回去"才有东西可收。
	for i := 0; i < 20; i++ {
		if _, err := db.CreatePost(ctx, other.ID, "动态"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	auto, _ := db.GetUserByID(ctx, other.ID)
	if auto.Color == ColorAuto {
		t.Fatal("前置条件没了：这个人本该已经是某一档颜色")
	}

	cfg.Enabled = false
	held, _ := db.GetUserByID(ctx, u.ID)
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	frozen, _ := db.GetUserByID(ctx, u.ID)
	if frozen.Points != held.Points || frozen.Points == 0 {
		t.Errorf("关掉开关把分值动了：%d → %d", held.Points, frozen.Points)
	}
	// 关掉开关时紫色的那一档还是紫色：管理员指定的颜色不参与"收回去"。
	if frozen.Color != ColorAdmin {
		t.Errorf("管理员锁的紫色被动了：%q", frozen.Color)
	}
	quiet, _ := db.GetUserByID(ctx, other.ID)
	if quiet.Color != ColorAuto {
		t.Errorf("关掉之后名字还带着活跃度颜色：%q", quiet.Color)
	}
	// 再打开就又跟着积分走。
	cfg.Enabled = true
	if _, err := db.RefreshPoints(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if back, _ := db.GetUserByID(ctx, other.ID); back.Color == ColorAuto {
		t.Errorf("重新打开之后颜色没回来： %+v", back)
	}
}

func TestPointsConfigRoundTripThroughSite(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	boss := mustUser(t, db, "boss")
	in := DefaultPoints()
	in.RefreshMinutes = 3
	in.CommunityCap = 20
	if err := db.SetPointsConfig(ctx, boss.ID, in); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if site.Points != in.Normalized() {
		t.Fatalf("存进去再读回来变了： %+v", site.Points)
	}
	// 站点设置那一页写别的键，不该顺手把积分参数改掉。
	if err := db.SetSiteAll(ctx, boss.ID, site); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.Site(ctx); again.Points != in.Normalized() {
		t.Errorf("保存站点设置把积分参数弄丢了： %+v", again.Points)
	}
}

// 注销之后什么都该没了：身份抹掉，颜色与分值也不该还挂着。
func TestCancelAccountClearsPointsAndColor(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	u := mustUser(t, db, "gone")
	if err := db.SetUserColor(ctx, u.ID, ColorPunish); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CancelAccount(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Color != "" || got.Points != 0 || got.PointsParts != "" {
		t.Fatalf("注销后还剩： %+v", got)
	}
	// 注销过的行不能再指定颜色。
	if err := db.SetUserColor(ctx, u.ID, ColorAdmin); !errors.Is(err, ErrAccountCancelled) {
		t.Errorf("给注销账号上色 = %v", err)
	}
}
