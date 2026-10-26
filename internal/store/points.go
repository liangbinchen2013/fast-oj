package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// points.go 是"积分"这一维度的算法与写回。
//
// 四个维度，每个都是"数一件事的次数 × 单位分，再按上限截断"：
//
//	ac   做对的题（去重后的 AC 题数）
//	sub  提交次数（含没过的：这是"动手了多少"，不是"会不会做题"）
//	cts  参加过的比赛数（在名单里算，赛内有没有提交另说）
//	com  社区发言：动态 + 帖子 + 评论
//
// 总分为四者之和，再乘一个"闲了多久"的衰减。衰减挂在重算那一步而不是读的时候：读侧要的
// 只是一个整数，而排行榜本来就在读 users 这一行（见 0033）。
//
// 所有参数都由管理员在 /admin/points 上调（存在 site_settings 里，跟着站点配置那份 2 秒
// 缓存走），所以这里只给默认值和归一化——任何一处拿到脏值都不会让算法跑飞。

// PointsConfig 是那一组可调参数。
type PointsConfig struct {
	// Enabled 关掉时后台不再重算也不改颜色：留着一个总开关，是因为这套东西在校园站上是
	// "锦上添花"，出问题时要能一键停，而不是靠改参数绕。
	Enabled bool `json:"enabled"`
	// RefreshMinutes 是重算间隔。它同时是"积分最多滞后多久"的口径，所以页面上要说清。
	RefreshMinutes int `json:"refresh_minutes"`
	// DecayPerDay 是每闲一天乘掉百分之几。0 就是不衰减。
	DecayPerDay int `json:"decay_per_day"`

	ProblemUnit   int `json:"problem_unit"`
	ProblemCap    int `json:"problem_cap"`
	SubmitUnit    int `json:"submit_unit"`
	SubmitCap     int `json:"submit_cap"`
	ContestUnit   int `json:"contest_unit"`
	ContestCap    int `json:"contest_cap"`
	CommunityUnit int `json:"community_unit"`
	CommunityCap  int `json:"community_cap"`

	// 三档门槛（分数）：低于 Green 是蓝，≥Green 绿，≥Orange 橙，≥Red 红。
	Green  int `json:"green"`
	Orange int `json:"orange"`
	Red    int `json:"red"`
}

// DefaultPoints 是一个新站上的默认口径。这些数字是"能看出差别"优先：一个 AC 十分、一次
// 提交一分，做题明显比灌水值钱；每档都有上限，免得有人靠刷提交把分堆到天上去。
func DefaultPoints() PointsConfig {
	return PointsConfig{
		Enabled: true, RefreshMinutes: 10, DecayPerDay: 5,
		ProblemUnit: 10, ProblemCap: 300,
		SubmitUnit: 1, SubmitCap: 60,
		ContestUnit: 20, ContestCap: 200,
		CommunityUnit: 2, CommunityCap: 120,
		Green: 30, Orange: 80, Red: 160,
	}
}

const (
	minRefreshMinutes, maxRefreshMinutes = 1, 1440
	maxUnit, maxCap, maxThreshold        = 1000, 100000, 100000
)

// Normalized 把配置收进安全的范围。
//
// 每个维度至少要有单位分才能算，所以单位分为 0 时退回默认值而不是当成"这一档不记分"——
// 管理员把某个框清空时，他大概不是在关掉那一档，而是在改数字；真要关掉有 Enabled。
func (c PointsConfig) Normalized() PointsConfig {
	d := DefaultPoints()
	clamp := func(v, fallback, lo, hi int) int {
		if v < lo || v > hi {
			return fallback
		}
		return v
	}
	c.RefreshMinutes = clamp(c.RefreshMinutes, d.RefreshMinutes, minRefreshMinutes, maxRefreshMinutes)
	c.DecayPerDay = clamp(c.DecayPerDay, d.DecayPerDay, 0, 90)
	c.ProblemUnit = clamp(c.ProblemUnit, d.ProblemUnit, 0, maxUnit)
	c.SubmitUnit = clamp(c.SubmitUnit, d.SubmitUnit, 0, maxUnit)
	c.ContestUnit = clamp(c.ContestUnit, d.ContestUnit, 0, maxUnit)
	c.CommunityUnit = clamp(c.CommunityUnit, d.CommunityUnit, 0, maxUnit)
	c.ProblemCap = clamp(c.ProblemCap, d.ProblemCap, 0, maxCap)
	c.SubmitCap = clamp(c.SubmitCap, d.SubmitCap, 0, maxCap)
	c.ContestCap = clamp(c.ContestCap, d.ContestCap, 0, maxCap)
	c.CommunityCap = clamp(c.CommunityCap, d.CommunityCap, 0, maxCap)
	if c.ProblemUnit == 0 {
		c.ProblemUnit = d.ProblemUnit
	}
	if c.SubmitUnit == 0 {
		c.SubmitUnit = d.SubmitUnit
	}
	if c.ContestUnit == 0 {
		c.ContestUnit = d.ContestUnit
	}
	if c.CommunityUnit == 0 {
		c.CommunityUnit = d.CommunityUnit
	}
	// 档位必须单调，否则会出现"橙比绿门槛低"这种没人能解释的颜色。低了是往上抬而不是退回
	// 默认值：管理员把绿调到 200 之后，默认的 80 反而是错的那个。
	floor := func(v, fallback, lo int) int {
		if v < 0 || v > maxThreshold {
			v = fallback
		}
		if v < lo {
			v = lo
		}
		return v
	}
	c.Green = floor(c.Green, d.Green, 0)
	c.Orange = floor(c.Orange, d.Orange, c.Green)
	c.Red = floor(c.Red, d.Red, c.Orange)
	return c
}

// ParsePoints 读 settings 表里的字符串。任何一行坏掉都退回那一行的默认值——半套参数
// 也比一次 panic 好，而这套东西的每一个值都是人敲进输入框的。
func ParsePoints(values map[string]string) PointsConfig {
	d := DefaultPoints()
	c := PointsConfig{Enabled: values["points.enabled"] != "0", DecayPerDay: d.DecayPerDay}
	num := func(key string, dst *int, fallback int) {
		if raw := strings.TrimSpace(values[key]); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				*dst = n
				return
			}
		}
		*dst = fallback
	}
	num("points.refresh_minutes", &c.RefreshMinutes, d.RefreshMinutes)
	num("points.decay_per_day", &c.DecayPerDay, d.DecayPerDay)
	num("points.problem_unit", &c.ProblemUnit, d.ProblemUnit)
	num("points.problem_cap", &c.ProblemCap, d.ProblemCap)
	num("points.submit_unit", &c.SubmitUnit, d.SubmitUnit)
	num("points.submit_cap", &c.SubmitCap, d.SubmitCap)
	num("points.contest_unit", &c.ContestUnit, d.ContestUnit)
	num("points.contest_cap", &c.ContestCap, d.ContestCap)
	num("points.community_unit", &c.CommunityUnit, d.CommunityUnit)
	num("points.community_cap", &c.CommunityCap, d.CommunityCap)
	num("points.color_green", &c.Green, d.Green)
	num("points.color_orange", &c.Orange, d.Orange)
	num("points.color_red", &c.Red, d.Red)
	return c.Normalized()
}

// Writes 是保存时写给 settings 表的键值。与 ParsePoints 成对：加一个参数就改这两处。
func (c PointsConfig) Writes() map[string]string {
	on := "1"
	if !c.Enabled {
		on = "0"
	}
	return map[string]string{
		"points.enabled":         on,
		"points.refresh_minutes": strconv.Itoa(c.RefreshMinutes),
		"points.decay_per_day":   strconv.Itoa(c.DecayPerDay),
		"points.problem_unit":    strconv.Itoa(c.ProblemUnit),
		"points.problem_cap":     strconv.Itoa(c.ProblemCap),
		"points.submit_unit":     strconv.Itoa(c.SubmitUnit),
		"points.submit_cap":      strconv.Itoa(c.SubmitCap),
		"points.contest_unit":    strconv.Itoa(c.ContestUnit),
		"points.contest_cap":     strconv.Itoa(c.ContestCap),
		"points.community_unit":  strconv.Itoa(c.CommunityUnit),
		"points.community_cap":   strconv.Itoa(c.CommunityCap),
		"points.color_green":     strconv.Itoa(c.Green),
		"points.color_orange":    strconv.Itoa(c.Orange),
		"points.color_red":       strconv.Itoa(c.Red),
	}
}

// Dim 是这一个人在某一维度上的得分（未衰减），供个人页与管理页显示"分是怎么来的"。
type Dim struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Score int    `json:"score"`
	Count int64  `json:"count"`
}

// dimLabels 是四个维度的名字与页面顺序。键是写进 points_parts 的那四个字母，改键就是改
// 数据格式，所以这里只有一份。
var dimLabels = [...]struct{ Key, Label string }{
	{"ac", "做题"},
	{"sub", "提交"},
	{"cts", "比赛"},
	{"com", "社区"},
}

// DimLabel 给一个维度一个人类看得懂的名字；认不出的键原样返回（它只可能来自库里，
// 那里只可能是这四个键之一）。
func DimLabel(key string) string {
	for _, l := range dimLabels {
		if l.Key == key {
			return l.Label
		}
	}
	return key
}

// ParseParts 还原 points_parts 那一串明细。认不出的键、超长的数字都丢掉：这一列是程序写的，
// 但它可能在人手里改过，而它接下来要被显示出来。
func ParseParts(raw string) []Dim {
	var out []Dim
	for _, part := range strings.Split(raw, ",") {
		key, score, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(score))
		if err != nil || n < 0 {
			continue
		}
		out = append(out, Dim{Key: key, Label: DimLabel(key), Score: n})
	}
	return out
}

// Dims 是这个人页面上要显示的四行明细。
func (u User) Dims() []Dim { return ParseParts(u.PointsParts) }

// UserPoints 是一个人算出来的结果。
type UserPoints struct {
	Total   int   `json:"total"` // 已经乘过衰减，写进 users.points 的就是它
	Decayed bool  `json:"decayed"`
	Idle    int64 `json:"idle_days"`
	Dims    []Dim `json:"dims"`
}

// ColorAuto 是"跟着积分走"，ColorAdmin 与 ColorPunish 是管理员手动锁上去的两种颜色：
// 自动那一路永远不碰后两个。
const (
	ColorAuto   = ""
	ColorAdmin  = "purple"
	ColorPunish = "brown"
)

// colorTokens 是能进 class 的那几个词，白名单只有这一份：Go 侧（ColorClass）和 SQL 侧
// （colorSQL）都从它生成，所以加一档只改这一行，不会出现"页面认得、查询不认得"的色名。
var colorTokens = []string{"blue", "green", "orange", "red", "purple", "brown"}

// colorSQL 是 ColorClass 在查询里的形状。名字出现在哪，档位就得跟到哪，而那一路只有 SQL 可用：
// 库里存的空串（"自动且最低一档"）落进 ELSE 分支，读出来就是 blue。
func colorSQL(alias string) string {
	return `CASE WHEN ` + alias + `.color IN ('` + strings.Join(colorTokens[1:], `','`) +
		`') THEN ` + alias + `.color ELSE 'blue' END`
}

// UserColors 是控制台那一列里能选的东西。自动 + 紫 + 棕，没有别的：其余四档由积分算出来。
var UserColors = []string{ColorAuto, ColorAdmin, ColorPunish}

// ValidColor 判断一个颜色词是不是管理员可以指定的那些。
func ValidColor(v string) bool {
	for _, c := range UserColors {
		if c == v {
			return true
		}
	}
	return false
}

// IsLocked 报告这一列是不是管理员指定过的（自动那一路该跳过它）。
//
// 判断写成白名单而不是"非空即锁"：库里被手改成别的词时，自动那一路应该能把它纠正回来，
// 而不是因为一个不认识的词永久跳过这个人。
func IsLocked(color string) bool { return color == ColorAdmin || color == ColorPunish }

// ColorFor 按分数给出四档之一。0 分（从没活动过）是蓝。
func (c PointsConfig) ColorFor(points int) string {
	switch {
	case points >= c.Red:
		return "red"
	case points >= c.Orange:
		return "orange"
	case points >= c.Green:
		return "green"
	}
	return "blue"
}

// activity 是合成积分要数的几件事。
type activity struct {
	ac, subs, contests, community int64
	lastAt                        int64
}

// score 按当前参数算某一维的得分（截断到上限）。
func dimScore(count int64, unit, cap int) int {
	v := count * int64(unit)
	if cap > 0 && v > int64(cap) {
		v = int64(cap)
	}
	return int(v)
}

// totals 把四个维度合成总分，并给出明细（未衰减）。
func (c PointsConfig) totals(a activity) UserPoints {
	dims := []Dim{
		{Key: "ac", Label: DimLabel("ac"), Score: dimScore(a.ac, c.ProblemUnit, c.ProblemCap), Count: a.ac},
		{Key: "sub", Label: DimLabel("sub"), Score: dimScore(a.subs, c.SubmitUnit, c.SubmitCap), Count: a.subs},
		{Key: "cts", Label: DimLabel("cts"), Score: dimScore(a.contests, c.ContestUnit, c.ContestCap), Count: a.contests},
		{Key: "com", Label: DimLabel("com"), Score: dimScore(a.community, c.CommunityUnit, c.CommunityCap), Count: a.community},
	}
	raw := 0
	for _, d := range dims {
		raw += d.Score
	}
	out := UserPoints{Total: raw, Dims: dims}
	if a.lastAt == 0 {
		return out
	}
	idleMS := nowMS() - a.lastAt
	if idleMS <= 0 {
		return out
	}
	days := float64(idleMS) / float64(24*3600*1000)
	out.Idle = int64(math.Ceil(days))
	if c.DecayPerDay > 0 {
		factor := math.Pow(1-float64(c.DecayPerDay)/100, days)
		out.Total = int(math.Round(float64(raw) * factor))
		out.Decayed = out.Total != raw
	}
	return out
}

// PartsText 是写进 points_parts 的明细串。
func (p UserPoints) PartsText() string {
	var b strings.Builder
	for _, d := range p.Dims {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%d", d.Key, d.Score)
	}
	return b.String()
}

// scanActivity 把"一条 SELECT user_id, 三个计数, 最后活动时间"的结果并进按人索引的 map。
// 四条查询都用同一个形状（用不上的位置填 0），所以合并逻辑只有一份。
func scanActivity(rows *sql.Rows, m map[int64]*activity, pick func(*activity, int64, int64, int64)) error {
	for rows.Next() {
		var (
			id              int64
			a, b, c, lastAt int64
		)
		if err := rows.Scan(&id, &a, &b, &c, &lastAt); err != nil {
			return err
		}
		p := m[id]
		if p == nil {
			p = &activity{}
			m[id] = p
		}
		pick(p, a, b, lastAt)
		if lastAt > p.lastAt {
			p.lastAt = lastAt
		}
	}
	return rows.Err()
}

// activityByUser 一次扫完四张事实表，按人合并。
//
// 为什么是四条 GROUP BY 而不是"每个用户四个相关子查询"：后者是 O(用户数 × 表)，用户上千
// 就开始难受；这四条各自把那张表走一遍，成本跟用户数无关。
func (db *DB) activityByUser(ctx context.Context) (map[int64]*activity, error) {
	type q struct {
		sql  string
		pick func(a *activity, x, y, at int64)
	}
	queries := []q{
		{`SELECT user_id,
		        COUNT(DISTINCT CASE WHEN verdict='AC' THEN problem_id END),
		        COUNT(*), 0,
		        MAX(IFNULL(NULLIF(judged_at, 0), created_at))
		 FROM submissions GROUP BY user_id`,
			func(a *activity, ac, subs, _ int64) { a.ac, a.subs = ac, subs }},
		{`SELECT user_id, COUNT(DISTINCT contest_id), 0, 0, MAX(joined_at)
		 FROM contest_members GROUP BY user_id`,
			func(a *activity, contests, _, _ int64) { a.contests = contests }},
		{`SELECT author_id, COUNT(*), 0, 0, MAX(created_at)
		 FROM posts WHERE visible=1 GROUP BY author_id`,
			func(a *activity, posts, _, _ int64) { a.community = posts }},
		{`SELECT author_id, COUNT(*), 0, 0, MAX(created_at)
		 FROM topics GROUP BY author_id`,
			func(a *activity, n, _, _ int64) { a.community += n }},
		{`SELECT author_id, COUNT(*), 0, 0, MAX(created_at)
		 FROM topic_comments GROUP BY author_id`,
			func(a *activity, n, _, _ int64) { a.community += n }},
	}
	out := map[int64]*activity{}
	for _, one := range queries {
		rows, err := db.SQL.QueryContext(ctx, one.sql)
		if err != nil {
			return nil, err
		}
		err = scanActivity(rows, out, one.pick)
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RefreshPoints 按当前参数重算所有人的积分与自动色，只写真的有变的那些行。
// 返回被改写的人数。
//
// 总开关关掉时它不动分值，只把自动那一档的颜色收回去：关掉这套东西的意思应该是"名字上
// 看不出活跃度"，而不是留下一片再也没人解释得了的红名。分值停在最后一次算出的样子，
// 重新打开就接着算。管理员手动锁的紫/棕两种情况都照旧不碰。
func (db *DB) RefreshPoints(ctx context.Context, cfg PointsConfig) (int, error) {
	var (
		people map[int64]*activity
		err    error
	)
	if cfg.Enabled {
		if people, err = db.activityByUser(ctx); err != nil {
			return 0, err
		}
	}
	// 当前存着的分值和颜色（也要读一遍才知道要不要写）。
	type stored struct {
		points int
		parts  string
		color  string
	}
	have := map[int64]stored{}
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT id, points, points_parts, color FROM users WHERE role <> 'judge' AND status='active'`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var (
			id     int64
			points int
			parts  string
			color  string
		)
		if err := rows.Scan(&id, &points, &parts, &color); err != nil {
			rows.Close()
			return 0, err
		}
		have[id] = stored{points, parts, color}
	}
	rows.Close()

	type write struct {
		id     int64
		points int
		parts  string
		color  string
	}
	var todo []write
	now := nowMS()
	for id, cur := range have {
		next, points, parts := cur.color, cur.points, cur.parts
		if cfg.Enabled {
			var a activity
			if got, ok := people[id]; ok {
				a = *got
			}
			res := cfg.totals(a)
			points, parts = res.Total, res.PartsText()
			if !IsLocked(cur.color) {
				next = cfg.ColorFor(res.Total)
				if next == "blue" {
					// 蓝是最低一档，库里存空串而不是 'blue'：新账号本来就该是空，
					// 把每一页的自动值都写成字面量只会让这一列变长。
					next = ColorAuto
				}
			}
		} else if !IsLocked(cur.color) {
			next = ColorAuto
		}
		if cur.points == points && cur.color == next {
			continue
		}
		todo = append(todo, write{id, points, parts, next})
	}
	if len(todo) == 0 {
		return 0, nil
	}
	// 顺序固定，免得同样的数据在不同时刻产生两份不同形状的写回（也方便测试断言条数）。
	sort.Slice(todo, func(i, j int) bool { return todo[i].id < todo[j].id })
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		for _, w := range todo {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET points=?, points_parts=?, color=?, points_at=? WHERE id=?`,
				w.points, w.parts, w.color, now, w.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(todo), nil
}

// SetUserColor 是管理员手动指定的那两种颜色（紫、棕），以及把它们放回自动。
func (db *DB) SetUserColor(ctx context.Context, userID int64, color string) error {
	if !ValidColor(color) {
		return fmt.Errorf("unknown color %q", color)
	}
	if err := db.guardMutable(ctx, userID); err != nil {
		return err
	}
	_, err := db.SQL.ExecContext(ctx, `UPDATE users SET color=? WHERE id=?`, color, userID)
	return err
}
