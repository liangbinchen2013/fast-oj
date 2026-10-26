package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// audit.go is the durable half of 「安全加固与审计」: every privileged action already
// writes a slog line, and this writes the same fact into the database so it outlives
// the process. The slog line stays — an operator watching stderr while a change lands
// is worth more than a page they have to open afterwards.
//
// What gets recorded is the privileged surface only (permissions, roles, bans,
// cancellations, site settings, judge programs, deletions, ticket handling). Ordinary
// content writes a person makes about their own things are not audit material and
// would only drown the entries that are.

// auditTimeout bounds the detached write. Losing the trail line is worth waiting a
// moment for, but holding a database connection past the response is not.
const auditTimeout = 3 * time.Second

// audit appends one trail line for the acting identity.
//
// A failure here is logged and dropped: the action it describes has already changed
// real state, and refusing the request because the trail could not be written would
// hand anyone who can fill a disk (or stall SQLite) a switch for the whole site.
// An audit trail that can DoS the site is the wrong trade — the gap must be visible
// in the log, not felt as an outage.
func (s *Server) audit(r *http.Request, a actor, action, target, detail string) {
	e := store.AuditEvent{
		ActorID: a.User.ID,
		Actor:   a.User.Handle,
		Action:  action,
		Target:  target,
		Detail:  detail,
		IP:      clientIP(r),
	}
	// A credential that is not a person (the judge token) still has to leave a name
	// behind, otherwise the line reads as if nobody did anything. What it leaves is
	// the shape of the credential, never its value.
	if e.Actor == "" {
		e.Actor = credentialKind(r)
	}
	// WithoutCancel: an abandoned page must not take the trail line down with it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditTimeout)
	defer cancel()
	if err := s.db.Audit(ctx, e); err != nil {
		s.log.Error("audit write failed", "action", action, "target", target, "actor", e.Actor, "err", err)
	}
}

// auditJudge records what a judge box *did* on the privileged surface: which work order
// it took, and what verdict it wrote back.
//
// 这两条要入库的理由是判机令牌的特殊性：拿着它的人能领走任意一份提交的源码与题目的
// 隐藏数据，也能写回判定。事后要回答的两个问题就是"那段时间谁领走了什么"和"那条判定
// 是谁写的"——而它们今天只在 stderr 里，进程一重启就没有了。
//
// 代价说清楚：每条提交都会写两行，这张表会长得比只记特权动作时快。行很小（百来字节），
// 一所学校一天的量在千行以下，但真要长期跑还是得配轮转（README 的限制表里写着）。
func (s *Server) auditJudge(box, action, target, detail string) {
	e := store.AuditEvent{
		Action: action,
		Actor:  "判机 " + box,
		Target: target,
		Detail: detail,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), auditTimeout)
	defer cancel()
	if err := s.db.Audit(ctx, e); err != nil {
		s.log.Error("audit write failed", "action", action, "target", target, "actor", e.Actor, "err", err)
	}
}

// auditDenied records a refused judge-protocol call.
//
// 判机令牌能领工单、能写判定结果，所以"有人拿不对的凭据来敲门"是这一站最要紧的一条线索，
// 而它今天只混在通用请求日志里（只有方法、路径、状态码）。这里只记判机协议那一路：
// 网页上的 401 是普通访客没登录，量大且没有特权动作发生，记下来只会把真正的那几条冲淡。
func (s *Server) auditDenied(r *http.Request, reason string) {
	e := store.AuditEvent{
		Action: "judge.denied",
		Actor:  credentialKind(r),
		Target: r.URL.Path,
		Detail: reason,
		IP:     clientIP(r),
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditTimeout)
	defer cancel()
	if err := s.db.Audit(ctx, e); err != nil {
		s.log.Error("audit write failed", "action", e.Action, "target", e.Target, "err", err)
	}
}

// credentialKind says what shape the presented credential had, never its value: a
// token or a password must not end up in a row that staff pages read.
func credentialKind(r *http.Request) string {
	if bearerToken(r) != "" {
		return "bearer 令牌"
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return "会话 Cookie"
	}
	return "无凭据"
}

// userStatusLabel turns an account status into the words an operator reads: an audit
// line is evidence, and "banned" next to "封禁" is the difference between a line you
// can act on and one you have to decode.
func userStatusLabel(status string) string {
	switch status {
	case "active":
		return "启用"
	case "banned":
		return "封禁"
	case "deleted":
		return "已注销"
	}
	return status
}

// siteSwitches turns one save of the site settings into a readable summary. The audit
// line has to answer "what did that save change", not merely "someone saved": a site
// silently taken off 注册 is exactly the thing an operator asks about afterwards.
//
// The prose (首页语、页脚、API 提示) is deliberately left out — it can run to thousands
// of characters, and the row's value is the switches and the name.
func siteSwitches(c store.SiteConfig) string {
	state := func(name string, on bool) string {
		if on {
			return name + "=开"
		}
		return name + "=关"
	}
	return strings.Join([]string{
		state("注册", c.AllowRegister), state("动态", c.AllowFeed), state("帖子", c.AllowTopics),
		state("比赛", c.AllowContests), state("私信", c.AllowDM),
		fmt.Sprintf("友情链接 %d 条", len(c.Links)),
	}, " ")
}

// --- the operator's page ---

// auditLabels is the Chinese name of each action key. The key itself is what the rows
// store (so filtering and code stay one language), and an unknown key falls back to
// printing the key: a row nobody named is still a row that happened, and hiding it
// would be worse than showing something raw.
var auditLabels = map[string]string{
	"perm.grant":             "授予能力",
	"perm.revoke":            "撤销能力",
	"perm.clear":             "清除单项覆盖",
	"role.change":            "改角色",
	"user.status":            "改账号状态",
	"user.tags":              "改用户标签",
	// 名字颜色：紫=以管理员身份出现在每一处署名，棕=被处罚。这两条要留痕，因为它们改的
	// 是"别人怎么看待这个人"，而不是"他能做什么"——恰恰是最容易口头否认的那类改动。
	"user.color":             "改名字颜色",
	"points.save":            "保存积分参数",
	"points.refresh":         "重算积分",
	"account.cancel":         "注销账号",
	"ip.ban":                 "封禁 IP",
	"ip.unban":               "解封 IP",
	"site.save":              "保存站点设置",
	"judge_program.save":     "保存判题器",
	"judge_program.remove":   "撤掉判题器",
	"case.delete":            "删除用例",
	"cases.wipe":             "清空用例",
	"problem.delete":         "删除题目",
	"contest.delete":         "删除比赛",
	"contest.problem.remove": "比赛移下题目",
	"ticket.status":          "工单改状态",
	"ticket.delete":          "删除工单",
	"judge.denied":           "判机鉴权被拒",
	"judge.claim":            "判机领走工单",
	"judge.result":           "判机回报判定",
	// 身份再确认被拒：有人拿着一个能改权限的会话，却连着五次答不出这个账号的密码。
	// 这是这一站最该留下的"篡改未遂"记录。
	"step_up.denied": "权限确认失败",
}

// auditActions is the filter list, ordered the way an operator reads the page: the
// things someone did on purpose first, then the refusals.
var auditActions = []string{
	"perm.grant", "perm.revoke", "perm.clear", "role.change", "user.status", "user.tags",
	"user.color", "account.cancel", "ip.ban", "ip.unban", "site.save",
	"points.save", "points.refresh",
	"judge_program.save", "judge_program.remove",
	"case.delete", "cases.wipe", "problem.delete",
	"contest.delete", "contest.problem.remove",
	"ticket.status", "ticket.delete", "judge.denied",
	"judge.claim", "judge.result", "step_up.denied",
}

func auditLabel(action string) string {
	if l, ok := auditLabels[action]; ok {
		return l
	}
	return action
}

// auditOption is one entry of the filter menu: the key the rows store plus the words
// the operator reads. Templates get no Go functions, so the pairing happens here.
type auditOption struct{ Key, Label string }

func auditOptions() []auditOption {
	out := make([]auditOption, 0, len(auditActions))
	for _, k := range auditActions {
		out = append(out, auditOption{Key: k, Label: auditLabel(k)})
	}
	return out
}

type auditRow struct {
	When  string
	Actor string
	// Link is the handle's page, empty when no account stood behind the action.
	Link   string
	Key    string
	Action string
	Target string
	Detail string
	IP     string
}

type auditPageData struct {
	Rows    []auditRow
	Shown   int
	Total   int
	Next    int64
	Limit   int
	Action  string
	Actions []auditOption
}

// handleAudit shows the trail. It is gated by ip.manage, the narrowest existing
// capability that already means "this person may see the site's security state": its
// label is 封禁 IP 与查看 IP 流量, and a trail full of source addresses belongs to
// exactly that reader. Adding a new key would only add a place to forget to grant it.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permIPManage)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := store.AuditFilter{
		Action: strings.TrimSpace(q.Get("action")),
		Limit:  pageLimit(q.Get("limit")),
	}
	f.AfterID, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	if _, known := auditLabels[f.Action]; !known {
		f.Action = "" // an unknown filter is "everything", not an empty page
	}

	rows, err := s.db.ListAudit(r.Context(), f)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	total, err := s.db.CountAudit(r.Context(), f)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	d := auditPageData{
		Shown: len(rows), Total: total, Limit: f.Limit,
		Action: f.Action, Actions: auditOptions(),
	}
	for _, e := range rows {
		link := ""
		if e.ActorID != 0 {
			link = "/u/" + url.PathEscape(e.Actor)
		}
		d.Rows = append(d.Rows, auditRow{
			When:  time.UnixMilli(e.At).Local().Format("2006-01-02 15:04:05"),
			Actor: e.Actor, Link: link, Key: e.Action, Action: auditLabel(e.Action),
			Target: e.Target, Detail: e.Detail, IP: e.IP,
		})
	}
	if len(rows) == f.Limit {
		d.Next = rows[len(rows)-1].ID
	}
	s.page(w, r, http.StatusOK, "安全审计", "admin_audit", a.viewer(), msgOf(r), d)
}
