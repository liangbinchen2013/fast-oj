package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

func ctx() context.Context { return context.Background() }

// testConfig mirrors what Load would produce, without touching the process env.
// It takes testing.TB so benchmarks can use it too.
func testConfig(t testing.TB) config.Config {
	t.Helper()
	dir := t.TempDir()
	return config.Config{
		Addr:          "127.0.0.1:0",
		DataDir:       dir,
		DBPath:        dir + "/test.db",
		AdminToken:    "admin-secret",
		JudgeToken:    "judge-secret",
		MaxSource:     1 << 20,
		MaxUpload:     1 << 20,
		MaxCase:       1 << 20,
		BCryptCost:    bcrypt.MinCost, // test speed, never a production default
		AllowRegister: true,
		// 一个用例里常常要注册好几个账号，全部来自同一个 127.0.0.1。生产默认是每分钟 1 次，
		// 这里放开，测的就是别的逻辑；限流本身有用例单独测（newHarnessCfg 调回 1）。
		RegisterPerMinute: 50,
		// 提交与建题的速率限制同理放开：一个用例里常常连建十几道题、连交十几份代码，
		// 限流本身有专门的用例（newHarnessCfg 调小）。
		SubmitPerMinute:  200,
		ProblemPerMinute: 200,
	}
}

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	db    *store.DB
	cfg   config.Config
	judge string
	// app + handler are the same chain the listening server uses, kept so a test can
	// drive a request with a chosen source address. Over the real listener every peer
	// is 127.0.0.1, which would make the IP feature tests unable to say anything.
	app     *Server
	handler http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessCfg(t, nil)
}

// newHarnessCfg builds a server with an adjusted config, for tests that need a
// different operator setting (registration closed, smaller limits, ...).
func newHarnessCfg(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	db, err := store.Open(ctx(), cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	// 真实部署里第一个当上管理员的人就是站主。测试站点先把他立好，否则每个用例里
	// 第一个注册的同学都会白拿最高权限，那些"普通人应该被拒"的断言就没意义了。
	operator, err := db.EnsureUser(ctx(), claimHandle, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimOwner(ctx(), operator.ID); err != nil {
		t.Fatal(err)
	}
	app := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), db)
	handler := app.Handler()
	hs := httptest.NewServer(handler)
	// 运维令牌一次只用得了一次，而 httptest 的客户端默认会**跟着 303 再发一次请求**，
	// 并把同一个 Authorization 带过去——那一跳就是"第二次请求"，真实世界里运维要拿的是
	// 日志里刚生成的下一枚。所以每一跳都换成当下那一枚：测的还是产品行为，只是把
	// "同一串重复出示"这件事交给客户端做（真重用的用例在 admintoken_test.go 里显式发两遍）。
	hs.Client().CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.Header.Get("Authorization") == "" {
			return nil
		}
		token, err := db.MintAdminToken(context.Background())
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	t.Cleanup(func() {
		hs.Close()
		db.Close()
	})
	return &harness{t: t, srv: hs, db: db, cfg: cfg, judge: cfg.JudgeToken, app: app, handler: handler}
}

// adminToken 现取一枚一次性运维令牌。
//
// 运维令牌用一次即作废（见 store/admintoken.go），所以测试里不能再像以前那样把一个串
// 存进 harness 反复用：每一次 bearer 调用都是一次独立的特权动作，得先让服务端生成
// 当下这一枚。故意用错的用例传的是字面量，不走这里。
func (h *harness) adminToken() string {
	h.t.Helper()
	token, err := h.db.MintAdminToken(ctx())
	if err != nil {
		h.t.Fatal(err)
	}
	return token
}

// do issues a request and returns status plus body as text.
func (h *harness) do(method, path, token string, body []byte, contentType string) (int, string) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (h *harness) get(path, token string) (int, string) {
	return h.do(http.MethodGet, path, token, nil, "")
}

func (h *harness) json(method, path, token string, payload []byte) (int, string) {
	return h.do(method, path, token, payload, "application/json")
}

func TestHealthzIsPlainText(t *testing.T) {
	h := newHarness(t)
	code, body := h.get("/healthz", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body != "ok\n" {
		t.Fatalf("body = %q, want %q", body, "ok\n")
	}
}

func TestAboutIsAPage(t *testing.T) {
	h := newHarness(t)
	code, body := h.htmlGet("/about", "")
	if code != http.StatusOK {
		t.Fatalf("about = %d", code)
	}
	for _, want := range []string{"运行概览", "已运行", "wal", "POST /judge/claim", "/problems/new"} {
		if !strings.Contains(body, want) {
			t.Errorf("about page missing %q", want)
		}
	}
	// It is a page, not a data dump: no request can shake a JSON document out of it.
	if strings.Contains(body, `"uptime":`) {
		t.Fatalf("about leaked json:\n%s", body)
	}
}

func TestHealthzFailsWhenDatabaseIsGone(t *testing.T) {
	cfg := testConfig(t)
	db, err := store.Open(ctx(), cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), db).Handler()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %q", rec.Code, rec.Body.String())
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	h := newHarness(t)
	code, body := h.get("/nope", "")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body %q", code, body)
	}
}

func TestMethodNotAllowedIs405(t *testing.T) {
	h := newHarness(t)
	code, _ := h.do(http.MethodPost, "/healthz", "", []byte("x"), "text/plain")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", code)
	}
}
