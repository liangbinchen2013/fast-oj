package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 积分与颜色这一套的网页侧：管理员调参数、看一眼预览、把颜色锁在两个人身上。
// 算法本身在 internal/store/points_test.go 里测，这里测的是"页面说的话和库里的值一致"。

// pointsValues 把一份参数摊成表单那十三格。键名就是 store 那一份 Writes 的键名去掉前缀，
// 所以加一个参数时这里跟着走，不会漏。
func pointsValues(cfg store.PointsConfig, do string) url.Values {
	v := url.Values{"do": {do}}
	for k, raw := range cfg.Writes() {
		if k == "points.enabled" {
			if raw == "1" {
				v.Set("enabled", "1")
			}
			continue
		}
		v.Set(strings.TrimPrefix(k, "points."), raw)
	}
	return v
}

// sitePoints 读库里那一套参数的现状：断言"被拒的保存没动库"要看的就是它。
func (h *harness) sitePoints(t *testing.T) store.PointsConfig {
	t.Helper()
	site, err := h.db.Site(ctx())
	if err != nil {
		t.Fatal(err)
	}
	return site.Points
}

func TestAdminPointsPageTunesAndRecolors(t *testing.T) {
	h := newHarness(t)
	boss := h.author("boss_p", permSiteSettings)
	worker := h.signIn("worker_p")
	h.readyProblem("P4100")
	h.submitCode(worker, "P4100", "cpp", "int main(){}")
	h.submitCode(worker, "P4100", "cpp", "int main(){}")
	h.finishSubmission(1, store.Result{Verdict: "AC", TimeMS: 9, MemKB: 2048, Score: 2, TotalScore: 2}, nil)

	if code, page := h.getAs(boss, "/admin/points"); code != http.StatusOK ||
		!strings.Contains(page, "积分与颜色") || !strings.Contains(page, "还没重算过") {
		t.Fatalf("调参页打不开 = %d：\n%s", code, short(page))
	}

	// 默认口径下一题 10 分、一次提交 1 分 = 12 分，离绿线 30 还远：先按默认存一次并重算。
	cfg := store.DefaultPoints()
	if _, page := h.postAs(boss, "/admin/points", pointsValues(cfg, "save_run")); !strings.Contains(page, "重算完成") {
		t.Fatalf("保存并立即重算没成：\n%s", short(page))
	}
	got := h.user("worker_p")
	if got.Points != 12 || got.Color != "" {
		t.Fatalf("默认口径下这个人 = %+v（12 分、该还是最低一档）", got)
	}

	// 把三档门槛往下调，同一个人就该变成绿的——这就是"管理员能调难度"的全部含义。
	cfg.Green, cfg.Orange, cfg.Red = 10, 20, 30
	if _, page := h.postAs(boss, "/admin/points", pointsValues(cfg, "save_run")); !strings.Contains(page, "重算完成") {
		t.Fatalf("第二次重算没成：\n%s", short(page))
	}
	if got = h.user("worker_p"); got.Color != "green" {
		t.Fatalf("门槛调到 10 之后 = %+v，应该是绿的", got)
	}
	if _, page := h.htmlGet("/rank", ""); !strings.Contains(page, `class="pt-green"`) {
		t.Errorf("排行榜上的名字没跟着变绿：\n%s", short(page))
	}
	if _, page := h.htmlGet("/rank", ""); !strings.Contains(page, ">12<") {
		t.Errorf("排行榜上没有积分那一列：\n%s", short(page))
	}

	// 只重算不改参数：走的是已经存着的那一份，不该把框里没保存的数字偷偷用上。
	if _, page := h.postAs(boss, "/admin/points", pointsValues(store.DefaultPoints(), "run")); !strings.Contains(page, "重算") {
		t.Fatalf("只重算那条路没回应：\n%s", short(page))
	}
	if got = h.user("worker_p"); got.Color != "green" {
		t.Fatalf("“只重算”把已存参数换成了表单里的： %+v", got)
	}

	// 有一格不是整数：整页拒绝并说清是哪一格，库里那套一个字都不动。
	bad := pointsValues(cfg, "save")
	bad.Set("problem_unit", "")
	if code, page := h.postAs(boss, "/admin/points", bad); code != http.StatusBadRequest ||
		!strings.Contains(page, "这些格要填整数") || !strings.Contains(page, "每题分值") {
		t.Fatalf("空着数字框 = %d，没有说清楚是哪一格：\n%s", code, short(page))
	}
	if again := h.sitePoints(t); again.Green != 10 {
		t.Errorf("被拒的保存把库改了： %+v", again)
	}

	// 关掉总开关：颜色要收回去，紫/棕那种手动的不受影响（这里没人被锁）。
	off := cfg
	off.Enabled = false
	if _, page := h.postAs(boss, "/admin/points", pointsValues(off, "save_run")); !strings.Contains(page, "总开关是关着的") {
		t.Fatalf("关掉之后没说清它在干什么：\n%s", short(page))
	}
	if got = h.user("worker_p"); got.Color != "" {
		t.Errorf("关掉开关之后名字还带着活跃度颜色：%+v", got)
	}
	if got.Points != 12 {
		t.Errorf("关掉开关把分值清了：%+v", got)
	}

	// 审计里要有这两条：谁存的参数、谁按的重算。
	if rows := h.auditRows("points.save"); len(rows) == 0 || !strings.Contains(rows[0].Detail, "绿 10") {
		t.Errorf("审计里没有 points.save 或写得看不出改了啥： %+v", rows)
	}
	if rows := h.auditRows("points.refresh"); len(rows) == 0 {
		t.Errorf("审计里没有 points.refresh： %+v", rows)
	}

	// /about 的接口表要认得这一页，否则下一个人无从找回它存在。
	if _, page := h.htmlGet("/about", ""); !strings.Contains(page, "/admin/points") ||
		!strings.Contains(page, "/admin/users/{handle}/color") {
		t.Error("/about 的接口表里少了积分或颜色那几条")
	}
}

func TestAdminPointsNeedsTheRight(t *testing.T) {
	h := newHarness(t)
	plain := h.author("plain_p")
	if code, page := h.getAs(plain, "/admin/points"); !strings.Contains(page, "缺少权限") {
		t.Errorf("普通人打得开调参页 = %d：\n%s", code, short(page))
	}
	// 只有 user.manage 的人能给人锁颜色，但碰不到站点参数：两件事分开授权。
	chief := h.author("chief_p2", permUserManage)
	if _, page := h.getAs(chief, "/admin/points"); !strings.Contains(page, "缺少权限") {
		t.Errorf("user.manage 竟然能调积分参数：\n%s", short(page))
	}
}

func TestAdminColorIsLimitedToTheTwoLocks(t *testing.T) {
	h := newHarness(t)
	chief := h.author("chief_c", permUserManage, permSiteSettings)
	h.signIn("victim_c")

	if _, page := h.postAs(chief, "/admin/users/victim_c/color", url.Values{"color": {"purple"}}); !strings.Contains(page, "紫（管理员）") {
		t.Fatalf("设紫色没成：\n%s", short(page))
	}
	if got := h.user("victim_c"); got.Color != store.ColorAdmin {
		t.Fatalf("库里不是 purple： %+v", got)
	}
	// 全站的名字都跟着：排行榜上那一行该是紫的。
	if _, page := h.htmlGet("/rank", ""); !strings.Contains(page, `class="pt-purple"`) {
		t.Errorf("排行榜上的名字没变紫：\n%s", short(page))
	}

	// 四档自动色不能手选：谁都能把自己"设成红的"，那一维就废了。
	for _, banned := range []string{"red", "green", "orange", "blue", "javascript:alert(1)"} {
		if _, page := h.postAs(chief, "/admin/users/victim_c/color", url.Values{"color": {banned}}); !strings.Contains(page, "只能选") {
			t.Errorf("竟然接受了颜色 %q：\n%s", banned, short(page))
		}
		if got := h.user("victim_c"); got.Color != store.ColorAdmin {
			t.Fatalf("失败的那次设色把库改了： %+v", got)
		}
	}

	// 重算不会把紫色冲掉：紫是状态，不是活跃度的读数。
	boss := h.author("boss_c", permSiteSettings)
	cfg := store.DefaultPoints()
	cfg.Green = 1
	// 这一轮里谁都不该被改（锁着的紫不动，没活动的人本来就在最低档），所以页面说的是
	// "没有任何人需要改"，而不是报一个虚高的数字。
	if _, page := h.postAs(boss, "/admin/points", pointsValues(cfg, "save_run")); !strings.Contains(page, "重算") {
		t.Fatalf("重算那条路没回应：\n%s", short(page))
	}
	if got := h.user("victim_c"); got.Color != store.ColorAdmin {
		t.Errorf("自动重算把管理员锁的紫色改了：%+v", got)
	}

	// 放回自动。
	if _, page := h.postAs(chief, "/admin/users/victim_c/color", url.Values{"color": {""}}); !strings.Contains(page, "跟着积分自动") {
		t.Fatalf("放回自动没成：\n%s", short(page))
	}
	if got := h.user("victim_c"); got.Color != "" {
		t.Errorf("没放回自动：%+v", got)
	}
	rows := h.auditRows("user.color")
	if len(rows) < 2 || rows[0].Target != "victim_c" {
		t.Errorf("审计里缺这一条： %+v", rows)
	}

	// 超级管理员的颜色别人设不了（控制台上那条一贯的规则）。
	if _, page := h.postAs(chief, "/admin/users/"+claimHandle+"/color", url.Values{"color": {"brown"}}); !strings.Contains(page, "超级管理员") {
		t.Errorf("给站主设颜色竟然没被拦：\n%s", short(page))
	}
}

// 那条自己走的腿必须跟着进程一起停：收到信号后还有一个全表扫描在跑是不可接受的，
// 而它等的是管理员设的间隔，最长能到一天。
func TestRunPointsExitsWithItsContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.app.RunPoints(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunPoints 没跟着 context 停下来")
	}
}

// 一轮 tick 就是"按现在存着的参数算一次"：它是那条腿唯一真正做事的部分。
func TestPointsTickUsesTheSavedConfig(t *testing.T) {
	h := newHarness(t)
	boss := h.author("boss_t", permSiteSettings)
	worker := h.signIn("worker_t")
	h.readyProblem("P4101")
	h.submitCode(worker, "P4101", "cpp", "int main(){}")
	h.finishSubmission(1, store.Result{Verdict: "AC", TimeMS: 9, MemKB: 2048, Score: 2, TotalScore: 2}, nil)

	cfg := store.DefaultPoints()
	cfg.Green = 5
	if _, page := h.postAs(boss, "/admin/points", pointsValues(cfg, "save")); !strings.Contains(page, "已保存") {
		t.Fatalf("保存没成：\n%s", short(page))
	}
	if got := h.user("worker_t"); got.Color != "" {
		t.Fatalf("只保存不该就动颜色：%+v", got)
	}
	if d := h.app.pointsTick(ctx()); d != time.Duration(cfg.RefreshMinutes)*time.Minute {
		t.Errorf("下一轮该等 %d 分钟，回的是 %v", cfg.RefreshMinutes, d)
	}
	if got := h.user("worker_t"); got.Points != 11 || got.Color != "green" {
		t.Errorf("这一轮没按存着的参数算： %+v", got)
	}

	// 关掉之后 tick 什么都不做，也不用等第二次扫描。
	off := cfg
	off.Enabled = false
	if _, page := h.postAs(boss, "/admin/points", pointsValues(off, "save")); !strings.Contains(page, "已保存") {
		t.Fatalf("关掉没成：\n%s", short(page))
	}
	before := h.user("worker_t")
	if d := h.app.pointsTick(ctx()); d <= 0 {
		t.Errorf("关掉之后回的这个间隔没法用：%v", d)
	}
	if after := h.user("worker_t"); after.Color != before.Color {
		t.Errorf("关掉总开关之后自动那一轮还在动颜色：%+v", after)
	}
}
