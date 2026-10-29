package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// theme.go 是 /settings 里那张配色卡：每个人自己调这九个变量，互不影响。
//
// 为什么只有九个槽位而不是"任意 CSS"：这一段文本会进 <html style="…">，那是 CSS 上下文——
// 放开成任意样式就等于把"改写这台站点的页面"交给每个注册用户。九个 `#rrggbb` 是表达力
// 与"没人能用样式做点什么"之间唯一说得过去的折中。校验在 store 那一层（ParseTheme/Set），
// 这里只负责把它摆成人能看懂的样子。

// themeSwatch 是九个变量各自的中文名、它管哪一块、以及两种底色下的默认值。
type themeSwatch struct {
	Key      string
	Label    string
	Hint     string
	Value    string // 这个人当前生效的值（没调过就是所选底色的默认值）
	Custom   bool   // 这个格子是他自己调过的（默认值与他调出来的可能撞色，所以按存储判断）
	DefaultS string // 所选底色（或浅色）下的默认值，"改回去"用得上
}

type themeView struct {
	Mode     string
	Swatches []themeSwatch
	// Custom 是"这一页上有任何一格被他自己调过"，决定要不要给出那行恢复默认的提示。
	Custom bool
}

// themeDefaults 镜像 style.css 里的两套调色板。**改了 style.css 必须同步这里**，否则取色器
// 的初值就不是"当前生效的颜色"而是猜的。它只影响初值显示：存不存、存什么由下面的表单决定，
// 所以这里写错最坏的后果是"没调过的人看到一格偏了的初值"，不会渲染出错。
var themeDefaults = map[string][2]string{
	"fg":     {"#1c1c1c", "#e8e8e8"},
	"muted":  {"#626262", "#aaaaaa"},
	"line":   {"#e6e6e6", "#303030"},
	"bg":     {"#f8f8f8", "#171717"},
	"code":   {"#f0f0f0", "#282828"},
	"accent": {"#1d5fbf", "#8bb8f5"},
	"ok":     {"#2e7d32", "#86cc92"},
	"bad":    {"#b4232c", "#f2969c"},
	"warn":   {"#855514", "#dfbd83"},
}

var themeLabels = []struct{ Key, Label, Hint string }{
	{"bg", "背景", "整页的底"},
	{"fg", "正文", "普通文字与主按钮"},
	{"accent", "强调", "链接、焦点与选中控件"},
	{"muted", "次要文字", "时间、说明、表头"},
	{"line", "分隔线", "边框与表格线"},
	{"code", "代码底色", "代码块、表格悬停与导航当前项"},
	{"ok", "通过", "AC 与成功提示"},
	{"bad", "失败", "WA/错误与危险按钮"},
	{"warn", "警告", "倒计时、待处理这类提醒"},
}

// buildThemeView 把库里那一串摆成表单。dark 参数是"这一页现在按哪套底色显示"：他自己选了
// 就照他选的，没选就按浅色给初值（服务器不知道来访浏览器的深浅色，猜也不会比默认更准）。
func buildThemeView(raw string) themeView {
	t := store.ParseTheme(raw)
	dark := t.Mode == "dark"
	v := themeView{Mode: t.Mode, Custom: len(t.Colors) > 0}
	for _, l := range themeLabels {
		def := themeDefaults[l.Key][0]
		if dark {
			def = themeDefaults[l.Key][1]
		}
		value, custom := t.Colors[l.Key], false
		if value != "" {
			custom = true
		} else {
			value = def
		}
		v.Swatches = append(v.Swatches, themeSwatch{
			Key: l.Key, Label: l.Label, Hint: l.Hint,
			Value: value, Custom: custom, DefaultS: def,
		})
	}
	return v
}

// handleThemeSave 存下一个人的配色。表单每次都把九格都交回来（原生取色器没有"留空"这个状态），
// 所以这里是整份重建而不是增量合并：想退回默认就调"全部恢复默认"那一个按钮。
func (s *Server) handleThemeSave(w http.ResponseWriter, r *http.Request) {
	a, err := s.requireSignedIn(w, r)
	if err != nil {
		return
	}
	msg := "配色已保存。"
	var t store.Theme
	if r.FormValue("clear") == "1" {
		t = store.Theme{}
		msg = "配色已恢复默认。"
	} else {
		t.Set("mode", strings.TrimSpace(r.FormValue("mode")))
		for _, l := range themeLabels {
			t.Set(l.Key, r.FormValue(l.Key))
		}
	}
	if err := s.db.UpdateTheme(r.Context(), a.User.ID, t); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}
