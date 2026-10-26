package api

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

//go:embed templates/*.html static/*
var assetFS embed.FS

// assetVersions fingerprints the embedded assets. The binary is the release, so a
// changed file always means a changed hash, which is what lets /static be cached
// for a year without a rebuild ever serving a stale stylesheet.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	entries, err := fs.ReadDir(assetFS, "static")
	if err != nil {
		return out
	}
	for _, e := range entries {
		body, err := assetFS.ReadFile("static/" + e.Name())
		if err != nil {
			continue
		}
		sum := sha256.Sum256(body)
		out[e.Name()] = hex.EncodeToString(sum[:])[:8]
	}
	return out
}()

// staticURL is the template helper behind {{static "style.css"}}.
func staticURL(name string) string {
	v, ok := assetVersions[name]
	if !ok {
		return "/static/" + url.PathEscape(name)
	}
	return "/static/" + url.PathEscape(name) + "?v=" + v
}

// Templates ship inside the binary, so a parse failure is a build-time bug and
// is allowed to stop the process rather than degrade one page at runtime.
var views = mustLoadViews()

// viewer is the nav-bar identity. Handle is the URL slug, Name what a human
// reads; CSRF travels with it because every form a signed-in browser sees must
// carry its token.
type viewer struct {
	Handle string
	Name   string
	// Color 是这个人自己的活跃度档位类名：导航栏上的名字也该是那个颜色，
	// 一个人不该在每一页看到自己变成别的档位。
	Color string
	Admin bool
	// Manage and Site are the two capabilities the nav bar renders links for, so
	// a delegate with only one of the consoles sees only that one.
	Manage bool
	Site   bool
	// Author is the 出题 entry: creating a problem or editing anyone's problem.
	Author bool
	// Contests is the 办赛 entry: creating a contest or managing anyone's.
	Contests bool
	// Unread is the private-message badge in the nav bar.
	Unread int
	// Tickets is the open-ticket badge, and only a handler ever has a number here.
	Tickets int
	// HandleTickets is the 工单 entry: a user sees their own queue, a handler sees
	// the one that needs work.
	HandleTickets bool
	// IPs is the 封禁 entry. It is set per render, not from a capability: the page
	// hides itself when every client address looks like the loopback (a same-host
	// proxy), because banning by address is meaningless there.
	IPs bool
	// Audit is the 审计 entry: the durable trail of privileged actions. It rides on
	// ip.manage (the capability that already means "may see the security state"),
	// but unlike IPs it is never hidden by the loopback gate — a trail is worth most
	// exactly when every address looks local.
	Audit bool
	// Stats is the 统计 entry: only someone holding stats.view sees the link.
	Stats bool
	CSRF  string
	// ThemeMode 与 ThemeStyle 是这个人在 /settings 里给自己挑的观感：前者是明暗底色
	// （空=跟着系统），后者是已经过白名单校验的自定义属性串。匿名访客两个都是空，页面
	// 与没有这个功能时一模一样，也就多不出一分钱开销。
	ThemeMode  string
	ThemeStyle template.CSS
}

// shell is the envelope every page template sees: layout fields plus the page
// payload under .Data.
type shell struct {
	Title string
	// Site carries the operator-editable title, tagline and footer.
	Site  store.SiteConfig
	Me    *viewer
	Flash string
	Data  any
}

type viewEngine struct {
	pages map[string]*template.Template
}

func mustLoadViews() *viewEngine {
	v, err := loadViews()
	if err != nil {
		panic("templates: " + err.Error())
	}
	return v
}

func loadViews() (*viewEngine, error) {
	// Templates render operator/user prose (tagline, footer, feed) through the
	// same Markdown+KaTeX pipeline as problem statements, so there is exactly one
	// escaping policy in the app.
	funcs := template.FuncMap{"markdown": renderMarkdownHTML, "static": staticURL}
	base, err := template.New("base").Funcs(funcs).ParseFS(assetFS, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return nil, err
	}
	v := &viewEngine{pages: map[string]*template.Template{}}
	err = fs.WalkDir(assetFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == "templates/layout.html" || !strings.HasSuffix(path, ".html") {
			return err
		}
		t, err := base.Clone()
		if err != nil {
			return err
		}
		t, err = t.ParseFS(assetFS, path)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(path, "templates/"), ".html")
		v.pages[name] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(v.pages) == 0 {
		return nil, errors.New("no page templates found")
	}
	return v, nil
}

func (v *viewEngine) render(w http.ResponseWriter, status int, page string, data shell) error {
	t, ok := v.pages[page]
	if !ok {
		return errors.New("unknown page template " + page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

// page renders an HTML view, falling back to the text error shape if the
// template blows up, so a rendering bug cannot turn into a blank 200.
func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, title, name string, me *viewer, flash string, data any) {
	// 导航栏的封禁入口要同时满足"有这个能力"和"这一页现在有意义"，所以每次渲染时填。
	s.injectIPGate(me, r)
	err := views.render(w, status, name, shell{Title: title, Site: s.site(r.Context()), Me: me, Flash: flash, Data: data})
	if err != nil {
		s.log.Error("template", "page", name, "err", err)
		s.respondError(w, r, http.StatusInternalServerError, "page rendering failed")
	}
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !strings.HasSuffix(name, ".css") && !strings.HasSuffix(name, ".js") {
		s.respondError(w, r, http.StatusNotFound, "no such asset")
		return
	}
	body, err := assetFS.ReadFile("static/" + name)
	if err != nil {
		s.respondError(w, r, http.StatusNotFound, "no such asset")
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	default:
		s.respondError(w, r, http.StatusNotFound, "no such asset")
		return
	}
	// The URL carries the content hash, so this can be cached for a year: a new
	// binary produces a new ?v=, and an old page never asks for the old name.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// errorPage is the HTML form of an error reply, used only for browsers.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.page(w, r, status, http.StatusText(status), "error", s.viewerOf(r), "", map[string]any{
		"Status":  statusLine(status),
		"Class":   statusClass(status),
		"Message": msg,
	})
}
