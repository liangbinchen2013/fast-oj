package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

const (
	sessionCookie = "fastoj_session"
	csrfField     = "csrf"
	csrfHeader    = "X-CSRF-Token"
)

type actor struct {
	User store.User
	// Admin and Judge are capabilities, not identities: the bootstrap admin
	// token grants the former without being a user account.
	Admin   bool
	Judge   bool
	Session store.Session
	// perms is the effective capability set, loaded once per request: role
	// baseline plus this user's individual grants and removals.
	perms map[string]bool
	// Cookie records that the credentials came from a cookie, which is exactly
	// the case where a cross-site request could ride along.
	Cookie bool
	// Unread is how many private messages are waiting, and is only counted for
	// cookie sessions: it drives the nav bar badge.
	Unread int
	// Tickets is the open-ticket queue, counted only for someone who may handle
	// tickets: a normal user has no business seeing how much work is pending.
	Tickets int
}

var (
	errNoCredentials = errors.New("no credentials")
	errForbidden     = errors.New("admin token required")
	errJudgeOnly     = errors.New("judge token required")
	errCSRF          = errors.New("csrf token missing or incorrect")
)

// authenticate resolves credentials from either the Authorization header (API,
// CLI, judge boxes) or the session cookie (browser). There is deliberately no
// "API token in a cookie" path: a cookie-based bearer would need its own CSRF
// secret, and real sessions make it redundant.
func (s *Server) authenticate(r *http.Request) (actor, error) {
	if token := bearerToken(r); token != "" {
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AdminToken)) == 1 {
			u, err := s.db.EnsureUser(r.Context(), store.BootstrapHandle, "admin")
			if err != nil {
				return actor{}, err
			}
			return s.withPerms(r, u, true, false)
		}
		if s.cfg.JudgeToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.JudgeToken)) == 1 {
			// A judge box is a machine, not a person: no capabilities, and the
			// judge endpoints are its only surface.
			return actor{Judge: true}, nil
		}
	}

	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		u, sess, err := s.db.SessionUser(r.Context(), c.Value)
		if err != nil {
			return actor{}, err
		}
		a, err := s.withPerms(r, u, u.Role == "admin", true)
		if err != nil {
			return actor{}, err
		}
		a.Session = sess
		return a, nil
	}
	return actor{}, errNoCredentials
}

// withPerms attaches the effective capability set for a user identity.
func (s *Server) withPerms(r *http.Request, u store.User, admin, cookie bool) (actor, error) {
	perms, err := s.db.EffectivePermissions(r.Context(), u.ID, u.Role)
	if err != nil {
		return actor{}, err
	}
	a := actor{User: u, Admin: admin, perms: perms, Cookie: cookie}
	if cookie {
		// The badge only matters to a browser, so the API paths do not pay for it.
		if a.Unread, err = s.db.CountDMUnread(r.Context(), u.ID); err != nil {
			return actor{}, err
		}
		if a.can(permTicketHandle) {
			if a.Tickets, err = s.db.CountOpenTickets(r.Context()); err != nil {
				return actor{}, err
			}
		}
	}
	return a, nil
}

func (s *Server) requireAdmin(r *http.Request) (actor, error) {
	a, err := s.authenticate(r)
	if err != nil {
		return a, err
	}
	if !a.Admin {
		return a, errForbidden
	}
	return a, nil
}

// requireJudge accepts the judge token, and the admin token so one operator can
// drive the queue by hand.
func (s *Server) requireJudge(r *http.Request) (actor, error) {
	a, err := s.authenticate(r)
	if err != nil {
		return a, err
	}
	if !a.Judge && !a.Admin {
		return a, errJudgeOnly
	}
	return a, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// writeAuthError answers a refused credential. The judge protocol refusals also go
// into the audit trail: a holder of a wrong token poking at /judge/claim is the one
// thing an operator has to be able to ask about afterwards, and until now the only
// trace was the generic request log (method, path, status), which names nobody and
// says nothing about why.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	// The reason doubles as what the visitor reads and what the trail records — they
	// are the same sentence on purpose. No credential value travels into either: the
	// detail is one of these four fixed strings, and the actor column only ever says
	// what shape the presented credential had.
	var (
		reason string
		status int
	)
	switch {
	case errors.Is(err, errForbidden):
		reason, status = "没有这个权限", http.StatusForbidden
	case errors.Is(err, errJudgeOnly):
		reason, status = "需要判机令牌", http.StatusForbidden
	case errors.Is(err, errCSRF):
		reason, status = "表单已过期，请刷新页面重试", http.StatusForbidden
	default:
		reason, status = "请先登录或提供有效的令牌", http.StatusUnauthorized
	}
	if isJudgeRoute(r) {
		s.auditDenied(r, reason)
	}
	s.respondError(w, r, status, reason)
}

// csrfOK compares the submitted token against the session's, in constant time.
// Bearer-authenticated calls have no session and are immune to CSRF, so they pass.
func csrfOK(r *http.Request, a actor) bool {
	if !a.Cookie {
		return true
	}
	got := r.Header.Get(csrfHeader)
	if got == "" {
		got = r.FormValue(csrfField)
	}
	if got == "" || a.Session.CSRF == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.Session.CSRF)) == 1
}

// multipartMemory 是 multipart 请求体的内存预算：超出部分由 net/http 落到临时文件，
// 所以几十 MB 的用例包不必整个压在内存里。
const multipartMemory = 4 << 20

// formHeadroom 是普通表单在源码上限之外留的余量：urlencoded 的换行会变成 %0A（一个字节
// 变三个），字段名与 csrf 也要地方。真正的封顶靠的是这个数存在本身——没有它，一条 POST
// 就能拿任意大的请求体去换一次 ParseForm 与一行日志。
const formHeadroom = 1 << 20

// guardCSRF is the single choke point for state-changing browser requests. It
// parses the form first so both the field and the header are visible, and fails
// closed for any cookie-authenticated write without a matching token.
func (s *Server) guardCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case isMultipart(r):
			// Go 1.25 起 ParseForm 与 FormValue 都不再看 multipart 请求体，隐藏字段里的
			// csrf 于是永远读不到，网页上传用例包一律 403「表单已过期」。这一路只能自己解。
			// 顺手给整个请求体封顶：不封的话匿名请求也能拿超大文件去堆磁盘上的临时文件。
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload+multipartMemory)
			if err := r.ParseMultipartForm(multipartMemory); err != nil {
				var over *http.MaxBytesError
				if errors.As(err, &over) {
					s.respondError(w, r, http.StatusRequestEntityTooLarge,
						fmt.Sprintf("上传太大，上限 %d MB", s.cfg.MaxUpload>>20))
					return
				}
				s.respondError(w, r, http.StatusBadRequest, "表单解析失败: "+err.Error())
				return
			}
		case isForm(r):
			// 表单也封顶。Go 自己会在 10MB 处报错，但那是"解析到那么多才停"；
			// 未封顶的请求体配上未截断的日志字段，一条 POST 就能把日志与响应撑大。
			// 上限取源码上限加一点余量：提交表单里最大的字段就是源码，其余表单远小于它。
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxSource+formHeadroom)
			if err := r.ParseForm(); err != nil {
				var over *http.MaxBytesError
				if errors.As(err, &over) {
					s.respondError(w, r, http.StatusRequestEntityTooLarge,
						fmt.Sprintf("表单太大，上限 %d KB", (s.cfg.MaxSource+formHeadroom)>>10))
					return
				}
				s.respondError(w, r, http.StatusBadRequest, "表单解析失败: "+err.Error())
				return
			}
		}
		a, err := s.authenticate(r)
		if err == nil && a.Cookie && !csrfOK(r, a) {
			s.log.Warn("csrf rejected", "path", r.URL.Path, "user", a.User.Handle)
			s.writeAuthError(w, r, errCSRF)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isForm(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
}

func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// viewerOf describes the logged-in identity for the nav bar.
func (s *Server) viewerOf(r *http.Request) *viewer {
	a, err := s.authenticate(r)
	if err != nil || a.User.ID == 0 {
		return nil
	}
	return a.viewer()
}

// viewer turns an actor into what the templates need.
func (a actor) viewer() *viewer {
	if a.User.ID == 0 {
		return nil
	}
	name := a.User.Handle
	if a.User.DisplayName != "" {
		name = a.User.DisplayName
	}
	theme := store.ParseTheme(a.User.Theme)
	return &viewer{
		Handle: a.User.Handle, Name: name, Color: a.User.ColorClass(), Admin: a.Admin,
		Manage: a.can(permPermissionManage), Site: a.can(permSiteSettings),
		Author:   a.can(permProblemCreate) || a.can(permProblemEditAny),
		Contests: a.canHostContests(),
		Unread:   a.Unread, Tickets: a.Tickets, CSRF: a.Session.CSRF,
		HandleTickets: a.can(permTicketHandle),
		Stats:         a.can(permStatsView),
		IPs:           a.can(permIPManage),
		Audit:         a.can(permIPManage),
		// 配色不查第二遍：它已经在 authenticate 读出的那一行 users 里，这里只是把那串文本
		// 解析成"能进 <html style> 的形状"（解析本身只认白名单，脏值掉出去）。
		ThemeMode:  theme.Mode,
		ThemeStyle: template.CSS(theme.Style()),
	}
}
