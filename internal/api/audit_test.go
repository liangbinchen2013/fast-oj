package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/protocol"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 审计：特权动作要留下一行活得比进程久的记录。这些用例断言的是"确实入库了、入库的是
// 谁做的、对谁做的"，不是页面上那句话。

// auditRowMark 是审计页里"一行表格记录"的标记：筛选下拉里也写着同样的动作键，
// 数行得认这段渲染结果。
const auditRowMark = `<div class="meta">perm.grant</div>`

// auditRows 读某个动作的全部审计行。
func (h *harness) auditRows(action string) []store.AuditRow {
	h.t.Helper()
	rows, err := h.db.ListAudit(context.Background(), store.AuditFilter{Action: action, Limit: 50})
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func TestAuditPermissionGrant(t *testing.T) {
	h := newHarness(t)
	h.author("auditee")

	if code, body := h.postBearer("/admin/perms/u/auditee",
		url.Values{"key": {permStatsView}, "action": {"grant"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("授予 = %d %s", code, short(body))
	}
	rows := h.auditRows("perm.grant")
	if len(rows) != 1 {
		t.Fatalf("perm.grant 行数 = %d, want 1：%+v", len(rows), rows)
	}
	r := rows[0]
	// 令牌进来的是 admin 这个账号：操作者要写下当时的名字，而不是"某个人"。
	if r.Actor != store.BootstrapHandle || r.ActorID == 0 {
		t.Errorf("操作者 = %q/%d", r.Actor, r.ActorID)
	}
	if r.Target != "auditee" || r.Detail != permStatsView {
		t.Errorf("授予的那一行 = %+v", r)
	}
	if r.IP == "" {
		t.Error("审计行要带来源地址，不然同一台机器上两个人分不清")
	}

	// 撤销与清除同样是特权动作。
	if code, _ := h.postBearer("/admin/perms/u/auditee",
		url.Values{"key": {permStatsView}, "action": {"revoke"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("撤销 = %d", code)
	}
	if rows := h.auditRows("perm.revoke"); len(rows) != 1 || rows[0].Detail != permStatsView {
		t.Errorf("perm.revoke = %+v", rows)
	}
	// 没做成的动作不算：不认识的能力键被拒之后不该多出一行。
	if code, _ := h.postBearer("/admin/perms/u/auditee",
		url.Values{"key": {"no.such.key"}, "action": {"grant"}}, h.adminToken()); code == http.StatusOK {
		t.Fatalf("不存在的权限键居然成功了：%d", code)
	}
	if rows := h.auditRows("perm.grant"); len(rows) != 1 {
		t.Errorf("被拒的授予也记了一行：%+v", rows)
	}
}

// 封号、改角色、注销、封 IP、存判题器、删题：每一条都要留下行。
func TestAuditPrivilegedSites(t *testing.T) {
	h := newHarness(t)
	h.author("victim")
	if code, _ := h.postBearer("/admin/users/victim/status", url.Values{"status": {"banned"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("封号 = %d", code)
	}
	if code, _ := h.postBearer("/admin/users/victim/role", url.Values{"role": {"admin"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("改角色 = %d", code)
	}
	rows := h.auditRows("user.status")
	if len(rows) != 1 || rows[0].Target != "victim" || !strings.Contains(rows[0].Detail, "封禁") {
		t.Errorf("user.status = %+v", rows)
	}
	if got := h.auditRows("role.change"); len(got) != 1 || !strings.Contains(got[0].Detail, "user") {
		t.Errorf("role.change 要写明从哪个角色改来：%+v", got)
	}

	// 封 IP：原因是有文本的，也要在审计里。
	h.openSite("203.0.113.60")
	if code, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(),
		url.Values{"ip": {"203.0.113.60"}, "note": {"脚本刷注册"}}); code != http.StatusOK || !strings.Contains(body, "已封禁") {
		t.Fatalf("封 IP = %d", code)
	}
	if got := h.auditRows("ip.ban"); len(got) != 1 || got[0].Target != "203.0.113.60" ||
		!strings.Contains(got[0].Detail, "脚本刷注册") {
		t.Errorf("ip.ban = %+v", got)
	}
	if code, _ := h.from(http.MethodPost, "/admin/ips/unban/203.0.113.60", "127.0.0.1:1", h.adminToken(), url.Values{}); code != http.StatusOK {
		t.Fatalf("解封 = %d", code)
	}
	if got := h.auditRows("ip.unban"); len(got) != 1 || got[0].Target != "203.0.113.60" {
		t.Errorf("ip.unban = %+v", got)
	}

	// 站点设置：审计要看得出改了哪些开关，而不是只看到"有人保存过设置"。
	if code, _ := h.postBearer("/admin/site", url.Values{
		"title": {"测试站"}, "tagline": {"首页语"}, "footer": {""}, "api_hint": {""},
		"links": {""}, "allow_register": {"1"}, "allow_topics": {"1"},
	}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("保存站点设置 = %d", code)
	}
	saved := h.auditRows("site.save")
	if len(saved) != 1 || saved[0].Target != "测试站" {
		t.Fatalf("site.save = %+v", saved)
	}
	if !strings.Contains(saved[0].Detail, "帖子=开") || !strings.Contains(saved[0].Detail, "注册=开") ||
		!strings.Contains(saved[0].Detail, "比赛=关") {
		t.Errorf("站点设置那一行没写清开关：%q", saved[0].Detail)
	}

	// 判题器：这是"把别人写的代码送上判机"，最该留痕的一项。
	writer := h.author("spjwriter", permProblemCreate, permTestcaseManage, permJudgeProgram)
	if code, body := h.postAs(writer, "/problems", problemValues("P7700", "判题器题")); code != http.StatusOK {
		t.Fatalf("建题 = %d %s", code, short(body))
	}
	spj := url.Values{"kind": {"checker"}, "language": {"cpp"}, "source": {"int main(){return 0;}"}}
	if code, body := h.postAs(writer, "/problems/P7700/judge_program", spj); code != http.StatusOK {
		t.Fatalf("存判题器 = %d %s", code, short(body))
	}
	savedProgram := h.auditRows("judge_program.save")
	if len(savedProgram) != 1 || savedProgram[0].Target != "P7700" ||
		!strings.Contains(savedProgram[0].Detail, "checker") {
		t.Fatalf("judge_program.save = %+v", savedProgram)
	}
	if code, _ := h.postAs(writer, "/problems/P7700/judge_program",
		url.Values{"kind": {"checker"}, "language": {"cpp"}, "source": {""}}); code != http.StatusOK {
		t.Fatalf("撤判题器 = %d", code)
	}
	if got := h.auditRows("judge_program.remove"); len(got) != 1 || got[0].Target != "P7700" {
		t.Errorf("judge_program.remove = %+v", got)
	}

	// 删题：题目没了以后，这一行是唯一还说得出"谁删的、删的是哪道题"的地方。
	if code, _ := h.postAs(writer, "/problems/P7700/delete", url.Values{}); code != http.StatusOK {
		t.Fatalf("删题 = %d", code)
	}
	deleted := h.auditRows("problem.delete")
	if len(deleted) != 1 || deleted[0].Target != "P7700" || !strings.Contains(deleted[0].Detail, "判题器题") {
		t.Errorf("problem.delete = %+v", deleted)
	}
}

// 判机协议上的鉴权失败：今天它只混在通用请求日志里（方法、路径、状态码），既没有
// 操作者也没有原因。
func TestAuditJudgeAuthDenials(t *testing.T) {
	h := newHarness(t)

	// 拿一个不对的 bearer 来领工单：401，而且要记得下来。
	if code, _ := h.from(http.MethodPost, "/judge/claim", "203.0.113.77:51234", "not-the-judge-token",
		url.Values{"name": {"box"}}); code != http.StatusUnauthorized {
		t.Fatalf("错令牌的 claim = %d, want 401", code)
	}
	rows := h.auditRows("judge.denied")
	if len(rows) != 1 {
		t.Fatalf("judge.denied 行数 = %d, want 1：%+v", len(rows), rows)
	}
	r := rows[0]
	if r.Target != "/judge/claim" {
		t.Errorf("被拒的路径 = %q", r.Target)
	}
	if r.IP != "203.0.113.77" {
		t.Errorf("来源地址 = %q", r.IP)
	}
	if !strings.Contains(r.Detail, "令牌") {
		t.Errorf("被拒的原因要写进审计行：%q", r.Detail)
	}
	// 令牌的值绝不进库：那一行只说凭据的形态。
	if strings.Contains(r.Actor+r.Detail, "not-the-judge-token") || strings.Contains(r.Actor+r.Detail, h.judge) {
		t.Errorf("审计行泄露了令牌：%+v", r)
	}
	if r.Actor != "bearer 令牌" || r.ActorID != 0 {
		t.Errorf("操作者 = %q/%d", r.Actor, r.ActorID)
	}

	// 登录了但没有判机权力，用 Cookie 打判机接口：403，同样要记。
	visitor := h.author("nojudge")
	if code, _ := h.postAs(visitor, "/judge/claim", url.Values{"name": {"box"}}); code != http.StatusForbidden {
		t.Fatalf("普通人打判机接口 = %d, want 403", code)
	}
	rows = h.auditRows("judge.denied")
	if len(rows) != 2 {
		t.Fatalf("judge.denied 行数 = %d, want 2", len(rows))
	}
	// 新的在前。
	if !strings.Contains(rows[0].Detail, "判机令牌") || rows[0].Actor != "会话 Cookie" {
		t.Errorf("Cookie 那一条 = %+v", rows[0])
	}
	if !strings.Contains(rows[1].Detail, "令牌") {
		t.Errorf("第一条被改写了：%+v", rows[1])
	}

	// 判机令牌正常领单不该记：那是每天的流量，不是被拒的敲门。（队列是空的，所以 404，
	// 但鉴权是过了的。）
	if code, _ := h.json(http.MethodPost, "/judge/claim", h.judge,
		[]byte(`{"name":"box","languages":["cpp"]}`)); code != http.StatusNotFound {
		t.Fatalf("空队列的 claim = %d, want 404", code)
	}
	if n := len(h.auditRows("judge.denied")); n != 2 {
		t.Errorf("成功的判机调用也记了一行：%+v", h.auditRows("judge.denied"))
	}
}

// 记不上账不能挡住正事：一张写不进去的审计表不该变成整站的开关。
func TestAuditFailureDoesNotBlockAction(t *testing.T) {
	h := newHarness(t)
	h.author("auditee2")
	// 把表抽掉，等于审计每次都写失败（磁盘满、库被锁）。
	if _, err := h.db.SQL.ExecContext(context.Background(), `DROP TABLE audit`); err != nil {
		t.Fatal(err)
	}
	if code, body := h.postBearer("/admin/perms/u/auditee2",
		url.Values{"key": {permStatsView}, "action": {"grant"}}, h.adminToken()); code != http.StatusOK {
		t.Fatalf("审计写不进去时动作被挡了：%d %s", code, short(body))
	}
	u := h.user("auditee2")
	perms, err := h.db.EffectivePermissions(context.Background(), u.ID, u.Role)
	if err != nil || !perms[permStatsView] {
		t.Fatalf("授予本身没落库：%v %+v", err, perms)
	}
}

// 查看页：门是 ip.manage（能看安全态势的那一项），分页沿用「更早」那套游标。
func TestAuditPageGateAndPaging(t *testing.T) {
	h := newHarness(t)
	plain := h.author("plainreader")
	if code, _ := h.getAs(plain, "/admin/audit"); code != http.StatusForbidden {
		t.Errorf("普通人进审计页 = %d, want 403", code)
	}
	if _, home := h.getAs(plain, "/"); strings.Contains(home, "/admin/audit") {
		t.Error("导航栏给普通人露出了审计入口")
	}
	// 只有统计权的人看得见运营数字，不该因此看到谁被谁封了号。
	only := h.author("stats_only", permStatsView)
	if code, _ := h.getAs(only, "/admin/audit"); code != http.StatusForbidden {
		t.Errorf("只持 stats.view 的人进审计页 = %d, want 403", code)
	}
	watcher := h.author("watcher", permIPManage)
	// 封禁入口会被"来源全是环回"收起，审计入口不该：越是在那种部署里越要查得动。
	if _, home := h.getAs(watcher, "/"); !strings.Contains(home, "/admin/audit") {
		t.Error("持 ip.manage 的人导航栏里没有审计入口")
	}

	// 两次授予，好让分页有东西可分。
	for _, key := range []string{permStatsView, permJudgeView} {
		if code, _ := h.postBearer("/admin/perms/u/plainreader",
			url.Values{"key": {key}, "action": {"grant"}}, h.adminToken()); code != http.StatusOK {
			t.Fatalf("授予 %s = %d", key, code)
		}
	}
	_, page := h.getAs(watcher, "/admin/audit")
	for _, want := range []string{"安全审计", "授予能力", "plainreader", store.BootstrapHandle} {
		if !strings.Contains(page, want) {
			t.Errorf("审计页缺 %q：\n%s", want, short(page))
		}
	}
	// 每行的动作键渲染在 <div class="meta">键</div> 里；筛选下拉也写着同样的键，
	// 所以数行要认这段标记，不能直接数字符串。
	if n := strings.Count(page, auditRowMark); n != 2 {
		t.Errorf("perm.grant 行数 = %d, want 2（标记 %q）", n, auditRowMark)
	}
	if strings.Contains(page, "problem.delete</div>") {
		t.Error("没删过题，页面上却出现了删除题目的行")
	}

	_, one := h.getAs(watcher, "/admin/audit?limit=1")
	if n := strings.Count(one, auditRowMark); n != 1 {
		t.Errorf("limit=1 应当只有一行，实际 %d：\n%s", n, short(one))
	}
	if !strings.Contains(one, "更早 →") {
		t.Errorf("还有更早的记录时没有「更早」链接：\n%s", short(one))
	}
	// 游标是更靠后的那一行（新的在前，所以 after=最新 id 拿到的是较早的一条）。
	rows, err := h.db.ListAudit(context.Background(), store.AuditFilter{Action: "perm.grant", Limit: 5})
	if err != nil || len(rows) != 2 {
		t.Fatalf("审计行数 = %d %v", len(rows), err)
	}
	_, older := h.getAs(watcher, "/admin/audit?action=perm.grant&limit=1&after="+
		strconv.FormatInt(rows[0].ID, 10))
	if !strings.Contains(older, permStatsView) || strings.Contains(older, permJudgeView) {
		t.Errorf("第二页拿到的不是较早那条：\n%s", short(older))
	}

	// 按动作筛：没发生过的动作是空表，不是报错。
	_, none := h.getAs(watcher, "/admin/audit?action=problem.delete")
	if !strings.Contains(none, "还没有记录") || strings.Contains(none, auditRowMark) {
		t.Errorf("筛选不存在的动作：\n%s", short(none))
	}
	// 不认识的动作键退回"全部"，而不是给一个空页让人以为没记录。
	_, junk := h.getAs(watcher, "/admin/audit?action=no.such")
	if n := strings.Count(junk, auditRowMark); n != 2 {
		t.Errorf("不认识的动作键应当当全部处理，行数 = %d：\n%s", n, short(junk))
	}
	// /about 的路由清单要有这一项：漏了它，下一个忘了这个页面存在的人无从找回来。
	if _, about := h.htmlGet("/about", ""); !strings.Contains(about, "/admin/audit") {
		t.Error("/about 的接口表里没有审计页")
	}
}

// 判机协议上"谁领走了什么"与"谁写了这条判定"要入库：拿着判机令牌的人能领走任意一份
// 源码与题目的隐藏数据，也能写回判定，而这两件事今天只在 stderr 里，进程一重启就没有了。
func TestAuditJudgeClaimAndResult(t *testing.T) {
	h := newHarness(t)
	student := h.signIn("auditee2")
	h.readyProblem("P2250")
	if code, body := h.submitCode(student, "P2250", "cpp", "int main(){}"); code != http.StatusOK {
		t.Fatalf("提交 = %d：\n%s", code, short(body))
	}
	task := h.claimTask(t, "box-audit")

	claims := h.auditRows("judge.claim")
	if len(claims) != 1 {
		t.Fatalf("领单记录 = %d 条，要 1 条", len(claims))
	}
	if !strings.Contains(claims[0].Detail, "P2250") || !strings.Contains(claims[0].Detail, "用例 3 条") {
		t.Errorf("领单记录没写清带走了什么：%q", claims[0].Detail)
	}
	if claims[0].Actor != "判机 box-audit" {
		t.Errorf("领单记录的 actor = %q，要写明是哪台判机", claims[0].Actor)
	}

	raw, _ := json.Marshal(protocol.Result{
		Name: "box-audit", Verdict: "AC", TimeMS: 12, MemKB: 1024, Toolchain: "C++17 · O2 开",
		Runs: []protocol.RunRow{{Ordinal: task.Cases[0].Ordinal, CaseName: task.Cases[0].Name,
			Verdict: "AC", TimeMS: 12, MemKB: 1024}},
	})
	sid := strconv.FormatInt(task.SubmissionID, 10)
	if code, body := h.postJSON("/judge/submissions/"+sid+"/result", h.judge, string(raw)); code != http.StatusOK {
		t.Fatalf("回报判定 = %d：\n%s", code, short(body))
	}
	results := h.auditRows("judge.result")
	if len(results) != 1 {
		t.Fatalf("判定记录 = %d 条，要 1 条", len(results))
	}
	// 判机自报的工具链要留在那一行里：争议时要能看出这条判定是"用哪条命令行判的"。
	if !strings.Contains(results[0].Detail, "C++17 · O2 开") {
		t.Errorf("判定记录没带上判机自报的口径：%q", results[0].Detail)
	}
}
