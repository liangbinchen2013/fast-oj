package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// points.go 是积分那一套的网页侧：管理员调参数的那一页，和按他设的间隔自己走的那条腿。
//
// 参数存在 site_settings 里（跟着站点配置那份 2 秒缓存走），算法在 internal/store/points.go。
// 这一页同时是"预览"：调完数字点一下重算，立刻能看见谁因此变绿、谁掉回蓝——这些数只有在
// 人名上才看得出合不合适。

// pointsStartDelay 是进程起来到第一次重算之间的等待：刚开机时判机正在领活，这一次全表
// 聚合不必抢在前面；但它也不能太久——升到这一版起 users.points 全是 0，名字要在几分钟内
// 变成真的颜色。
const pointsStartDelay = time.Minute

// RunPoints 按管理员设的间隔重算所有人的积分与自动色，直到 ctx 结束。
//
// 为什么是进程内的计时器，而不是"渲染时顺手算"：那一套要从四张事实表里聚合，挂在请求上
// 等于把排行榜的代价付在每一页（迁移 0033 里算过这笔账）。为什么不是 cron 或 systemd
// 定时器：这个项目的部署口径是"一个二进制 + 一个数据目录"，多一样要装的东西就多一处会
// 忘配的地方。间隔每轮重新读，所以管理员改完参数不需要重启。
func (s *Server) RunPoints(ctx context.Context) {
	delay := pointsStartDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = s.pointsTick(ctx)
	}
}

// pointsTick 跑一轮重算，回下一轮该等多久（读的是刚存着的参数，所以管理员改完就跟着变）。
func (s *Server) pointsTick(ctx context.Context) time.Duration {
	cfg := s.siteFresh(ctx).Points
	delay := time.Duration(cfg.RefreshMinutes) * time.Minute
	if !cfg.Enabled {
		return delay
	}
	n, err := s.db.RefreshPoints(ctx, cfg)
	switch {
	case err != nil:
		s.log.Error("points refresh failed", "err", err)
	case n > 0:
		s.log.Info("points refreshed", "changed", n)
	}
	return delay
}

// --- 调参数那一页 ---

type pointsPageData struct {
	Cfg    store.PointsConfig
	Rows   []store.PointsRow
	LastAt string
	// Tiers 是四档门槛的读法，页面顶部先说清楚"多少分是什么颜色"，管理员才知道自己在
	// 把哪些人推过线。
	Tiers []tierRow
	// Defaults 让"改坏了"有一个去处：页面上说明默认口径是什么，不必翻 README。
	Defaults store.PointsConfig
}

type tierRow struct {
	Class string
	Label string
	From  int
	Note  string
}

func tierRows(c store.PointsConfig) []tierRow {
	return []tierRow{
		{"blue", "蓝", 0, "还没活动"},
		{"green", "绿", c.Green, "起步"},
		{"orange", "橙", c.Orange, "常用"},
		{"red", "红", c.Red, "很活跃"},
		{"purple", "紫", 0, "管理员指定"},
		{"brown", "棕", 0, "管理员指定（处罚）"},
	}
}

// pointsLastAt 把"上一次重算是什么时候"说人话；从没算过时要明确说出来，否则管理员会
// 以为页面上那串 0 分是算出来的结果。
func pointsLastAt(ms int64) string {
	if ms <= 0 {
		return "还没重算过（点下面的“保存并立即重算”，或等自动那一轮）"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

func (s *Server) renderPoints(w http.ResponseWriter, r *http.Request, a actor, status int, cfg store.PointsConfig, flash string) {
	rows, lastAt, err := s.db.PointsBoard(r.Context(), 50)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	cfg = cfg.Normalized()
	s.page(w, r, status, "积分与颜色", "admin_points", a.viewer(), flash, pointsPageData{
		Cfg: cfg, Rows: rows, LastAt: pointsLastAt(lastAt),
		Tiers: tierRows(cfg), Defaults: store.DefaultPoints(),
	})
}

func (s *Server) handleAdminPoints(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permSiteSettings)
	if !ok {
		return
	}
	s.renderPoints(w, r, a, http.StatusOK, s.siteFresh(r.Context()).Points, msgOf(r))
}

// pointsFromForm 读那一格一格的数字。任何一格不是整数就退回错误：这套参数是拿钱买时间的
// 东西（重算间隔、上限），静默把它当 0 或当默认值，比告诉管理员"第几格没填对"要危险得多。
func pointsFromForm(r *http.Request) (store.PointsConfig, string) {
	var (
		cfg store.PointsConfig
		bad []string
	)
	num := func(key string, dst *int) {
		raw := strings.TrimSpace(r.FormValue(key))
		if raw == "" {
			bad = append(bad, pointsFieldLabels[key])
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			bad = append(bad, pointsFieldLabels[key])
			return
		}
		*dst = n
	}
	cfg.Enabled = r.FormValue("enabled") == "1"
	num("refresh_minutes", &cfg.RefreshMinutes)
	num("decay_per_day", &cfg.DecayPerDay)
	num("problem_unit", &cfg.ProblemUnit)
	num("problem_cap", &cfg.ProblemCap)
	num("submit_unit", &cfg.SubmitUnit)
	num("submit_cap", &cfg.SubmitCap)
	num("contest_unit", &cfg.ContestUnit)
	num("contest_cap", &cfg.ContestCap)
	num("community_unit", &cfg.CommunityUnit)
	num("community_cap", &cfg.CommunityCap)
	num("color_green", &cfg.Green)
	num("color_orange", &cfg.Orange)
	num("color_red", &cfg.Red)
	if len(bad) > 0 {
		return cfg, "这些格要填整数：" + strings.Join(bad, "、")
	}
	return cfg, ""
}

// pointsFieldLabels 是报错时要给管理员看的那一格的名字。键名是给代码看的，中文是给他的。
var pointsFieldLabels = map[string]string{
	"refresh_minutes": "重算间隔",
	"decay_per_day":   "每天衰减",
	"problem_unit":    "每题分值",
	"problem_cap":     "做题上限",
	"submit_unit":     "每次提交分值",
	"submit_cap":      "提交上限",
	"contest_unit":    "每场比分值",
	"contest_cap":     "比赛上限",
	"community_unit":  "每条发言分值",
	"community_cap":   "社区上限",
	"color_green":     "绿的门槛",
	"color_orange":    "橙的门槛",
	"color_red":       "红的门槛",
}

// pointsSummary 把一次保存写成一行审计：事后翻审计的人要能回答"那时候的口径是什么"，
// 而不是只知道有人点过保存。
func pointsSummary(c store.PointsConfig) string {
	state := "开着"
	if !c.Enabled {
		state = "已关闭"
	}
	return state + "；每 " + strconv.Itoa(c.RefreshMinutes) + " 分钟重算，闲一天扣 " +
		strconv.Itoa(c.DecayPerDay) + "%；题 " + strconv.Itoa(c.ProblemUnit) + "/道 上限 " +
		strconv.Itoa(c.ProblemCap) + "，提交 " + strconv.Itoa(c.SubmitUnit) + "/次 上限 " +
		strconv.Itoa(c.SubmitCap) + "，比赛 " + strconv.Itoa(c.ContestUnit) + "/场 上限 " +
		strconv.Itoa(c.ContestCap) + "，发言 " + strconv.Itoa(c.CommunityUnit) + "/条 上限 " +
		strconv.Itoa(c.CommunityCap) + "；档位 绿 " + strconv.Itoa(c.Green) + " 橙 " +
		strconv.Itoa(c.Orange) + " 红 " + strconv.Itoa(c.Red)
}

// handleAdminPointsSave 一个入口三件事：只保存、只重算、保存并立即重算。
//
// 页面上是三个按钮（form 里的 do 字段），共用同一份校验和同一句审计。只重算走的是**已经存着**
// 的参数：那样做的人想的是"按现在的口径再算一遍"，而不是顺手把框里没保存的数字用上。
func (s *Server) handleAdminPointsSave(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permSiteSettings)
	if !ok {
		return
	}
	ctx := r.Context()
	cfg, bad := pointsFromForm(r)
	if bad != "" {
		s.renderPoints(w, r, a, http.StatusBadRequest, cfg, bad)
		return
	}
	switch do := r.FormValue("do"); do {
	case "save", "save_run":
		if err := s.db.SetPointsConfig(ctx, a.User.ID, cfg); err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		// 缓存作废：下一个请求就要看到新数字，包括下面那次重算读到的那份。
		s.cache.invalidate()
		s.log.Info("points config saved", "by", a.User.Handle, "detail", pointsSummary(cfg.Normalized()))
		s.audit(r, a, "points.save", "积分参数", pointsSummary(cfg.Normalized()))
		if do == "save" {
			http.Redirect(w, r, "/admin/points?msg="+url.QueryEscape("已保存。下一次自动重算在最多 "+
				strconv.Itoa(cfg.Normalized().RefreshMinutes)+" 分钟后。"), http.StatusSeeOther)
			return
		}
		s.refreshNow(w, r, a, cfg.Normalized())
	case "run":
		s.refreshNow(w, r, a, s.siteFresh(ctx).Points)
	default:
		http.Redirect(w, r, "/admin/points?msg="+url.QueryEscape("不认的动作："+do), http.StatusSeeOther)
	}
}

// refreshNow 跑一次重算并把结果说清楚：改了几个人、有没有因为总开关而只动颜色。
func (s *Server) refreshNow(w http.ResponseWriter, r *http.Request, a actor, cfg store.PointsConfig) {
	n, err := s.db.RefreshPoints(r.Context(), cfg)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.log.Info("points refreshed", "by", a.User.Handle, "changed", n)
	s.audit(r, a, "points.refresh", "积分", pointsRefreshed(cfg, n))
	http.Redirect(w, r, "/admin/points?msg="+url.QueryEscape(pointsRefreshed(cfg, n)), http.StatusSeeOther)
}

func pointsRefreshed(cfg store.PointsConfig, n int) string {
	if !cfg.Enabled {
		return "总开关是关着的：分值停在最后一次算出的样子，只把自动那一档的颜色收回去（改了 " +
			strconv.Itoa(n) + " 个人）。"
	}
	if n == 0 {
		return "重算过了，没有任何人的分值或颜色需要改。"
	}
	return "重算完成，改了 " + strconv.Itoa(n) + " 个人的分值或颜色。"
}

// colorOptions 是控制台里那一列"名字颜色"的三个可选值。四档自动色不在这里：它们是算出来的，
// 不是选出来的（见 handleAdminUserColor）。
func colorOptions() []struct{ Value, Label string } {
	out := make([]struct{ Value, Label string }, 0, len(store.UserColors))
	labels := map[string]string{
		store.ColorAuto:   "跟着积分自动（蓝 / 绿 / 橙 / 红）",
		store.ColorAdmin:  "紫 · 管理员",
		store.ColorPunish: "棕 · 被处罚",
	}
	for _, c := range store.UserColors {
		out = append(out, struct{ Value, Label string }{c, labels[c]})
	}
	return out
}

// handleAdminUserColor 是控制台里那一列"名字颜色"：只有紫和棕能选，外加"跟着积分自动"。
//
// 蓝绿橙红不在选项里——它们是积分的读数，由后台按档位算。让人手输这四档，就等于允许
// 一个人自称活跃而不用活动。
func (s *Server) handleAdminUserColor(w http.ResponseWriter, r *http.Request) {
	a, ok := s.requirePerm(w, r, permUserManage)
	if !ok {
		return
	}
	target, ok := s.adminTarget(w, r, a)
	if !ok {
		return
	}
	back := "/admin/perms/u/" + url.PathEscape(target.Handle)
	color := strings.TrimSpace(r.FormValue("color"))
	if !store.ValidColor(color) {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("名字颜色只能选：跟着积分自动、紫（管理员）、棕（处罚）。"), http.StatusSeeOther)
		return
	}
	if err := s.db.SetUserColor(r.Context(), target.ID, color); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	// 手动指定不是一次美化：紫色意味着"这个人以管理员的身份出现在每一处署名里"，棕色意味着
	// "他被处罚过"。这两件事都要留下是谁在什么时候把谁设成什么的。
	why := "跟着积分自动"
	switch color {
	case store.ColorAdmin:
		why = "紫（管理员）"
	case store.ColorPunish:
		why = "棕（处罚）"
	}
	s.log.Info("user color changed", "by", a.User.Handle, "target", target.Handle, "from", target.Color, "to", color)
	s.audit(r, a, "user.color", target.Handle, "名字颜色由 "+colorOfText(target.Color)+" 改成 "+why)
	http.Redirect(w, r, back+"?msg="+url.QueryEscape("名字颜色已设为"+why+"。"), http.StatusSeeOther)
}

func colorOfText(color string) string {
	switch color {
	case store.ColorAdmin:
		return "紫（管理员）"
	case store.ColorPunish:
		return "棕（处罚）"
	case "":
		return "自动"
	}
	return color
}
