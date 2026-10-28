package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// IP 封禁与按地址流量。测试要从"真实来源地址"打进来，所以走 handler 直连：
// 经过真实监听端口时，对端永远是 127.0.0.1，功能会被自己的闸门收起，测不出东西。

// from 发一个指定来源地址的请求，返回状态码与页面文本。
func (h *harness) from(method, path, remoteAddr, token string, values ...url.Values) (int, string) {
	h.t.Helper()
	form := url.Values{}
	if len(values) > 0 && values[0] != nil {
		form = values[0]
	}
	formBody := ""
	if method == http.MethodPost {
		formBody = form.Encode()
	}
	req := httptest.NewRequest(method, path, strings.NewReader(formBody))
	req.RemoteAddr = remoteAddr
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	code, body := rec.Code, rec.Body.String()
	// 这些处理器写完后是 303，消息在跳转目标里。跟一跳，测试就能像浏览器一样
	// 断言"人最后看到的那句话"。
	if code == http.StatusSeeOther {
		if loc := rec.Header().Get("Location"); loc != "" {
			u, err := url.Parse(loc)
			if err == nil {
				// 跟这一跳是"第二次请求"：运维令牌一次只用得了一次，所以带过去的必须是
				// 当下那一枚，而不是刚刚用掉的那一串（真实世界里运维从日志里抄下一枚）。
				next := token
				if token != "" {
					next = h.adminToken()
				}
				return h.from(http.MethodGet, u.Path+"?"+u.RawQuery, remoteAddr, next)
			}
		}
	}
	return code, body
}

// openSite 让站点看起来"收到过真实访客"：先打一个公网来源的请求，
// 这一页的闸门就会打开。
func (h *harness) openSite(ip string) {
	h.t.Helper()
	h.from(http.MethodGet, "/problems", ip+":8080", "", nil)
}

func TestIPBanBlocksWholeSite(t *testing.T) {
	h := newHarness(t)
	h.openSite("203.0.113.7")

	if code, _ := h.from(http.MethodGet, "/problems", "203.0.113.7:51234", h.adminToken()); code != http.StatusOK {
		t.Fatalf("封禁前该能读题库 = %d", code)
	}
	if _, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(),
		url.Values{"ip": {"203.0.113.7"}, "note": {"脚本刷注册"}}); !strings.Contains(body, "已封禁") {
		t.Fatalf("封禁没给出结果：\n%s", short(body))
	}

	// 整站拦死：公开页、探活、静态资源、登录都拦。
	for _, path := range []string{"/problems", "/healthz", "/login", "/static/style.css", "/"} {
		if code, body := h.from(http.MethodGet, path, "203.0.113.7:51234", ""); code != http.StatusForbidden {
			t.Errorf("封禁后 %s = %d, want 403:\n%s", path, code, short(body))
		}
	}
	if _, body := h.from(http.MethodGet, "/problems", "203.0.113.7:1", ""); strings.Contains(body, "203.0.113") {
		t.Error("被拒的页面泄露了命中的规则，等于告诉攻击者边界在哪")
	}
	// 判机回调也一样拦：这是"整站"的一部分，管理员必须知道自己能封到自己。
	if code, _ := h.from(http.MethodPost, "/judge/claim", "203.0.113.7:1", h.judge, url.Values{"name": {"box"}}); code != http.StatusForbidden {
		t.Errorf("被封地址的判机回调 = %d, want 403", code)
	}
	// 别的地址不受影响。
	if code, _ := h.from(http.MethodGet, "/problems", "203.0.113.8:1", ""); code != http.StatusOK {
		t.Errorf("邻居地址也被拦了 = %d", code)
	}

	// 解封立刻生效（缓存要跟着失效）。
	if _, body := h.from(http.MethodPost, "/admin/ips/unban/203.0.113.7", "127.0.0.1:1", h.adminToken(), url.Values{}); !strings.Contains(body, "已解封") {
		t.Fatalf("解封没给出结果：\n%s", short(body))
	}
	if code, _ := h.from(http.MethodGet, "/problems", "203.0.113.7:1", ""); code != http.StatusOK {
		t.Errorf("解封后仍然被拦 = %d", code)
	}
}

func TestIPBanAcceptsCIDRAndRejectsNonsense(t *testing.T) {
	h := newHarness(t)
	h.openSite("198.51.100.1")

	if _, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(),
		url.Values{"ip": {"198.51.100.0/30"}, "note": {"机房一角"}}); !strings.Contains(body, "已封禁 198.51.100.0/30") {
		t.Fatalf("网段封禁：\n%s", short(body))
	}
	// /30 里的三个可用地址都拦下，网段外不受影响。
	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		if code, _ := h.from(http.MethodGet, "/problems", ip+":1", ""); code != http.StatusForbidden {
			t.Errorf("网段里的 %s = %d, want 403", ip, code)
		}
	}
	if code, _ := h.from(http.MethodGet, "/problems", "198.51.100.4:1", ""); code != http.StatusOK {
		t.Errorf("网段外被误拦 = %d", code)
	}

	// 写法不对要给出可读原因，不要静悄悄。
	for _, bad := range []string{"example.com", "198.51.100.999", "", "not an ip"} {
		_, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(), url.Values{"ip": {bad}})
		if !strings.Contains(body, "IP") {
			t.Errorf("封 %q 没给出写法原因：\n%s", bad, short(body))
		}
	}
	// 环回地址不能封：那等于把整站连自己一起锁死。
	if _, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(), url.Values{"ip": {"127.0.0.1"}}); !strings.Contains(body, "环回") {
		t.Errorf("封 127.0.0.1 没被拦住：\n%s", short(body))
	}
	if _, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(), url.Values{"ip": {"::1"}}); !strings.Contains(body, "环回") {
		t.Errorf("封 ::1 没被拦住：\n%s", short(body))
	}
	// 封全世界那条也一样拦死：按下去的人连同这一页一起被 403，之后就没人解封得了。
	for _, selfLockout := range []string{"0.0.0.0/0", "::/0"} {
		if _, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(), url.Values{"ip": {selfLockout}}); !strings.Contains(body, "全世界") {
			t.Errorf("封 %s 没被拦住：\n%s", selfLockout, short(body))
		}
	}
	if n := h.countIPBans(); n != 1 {
		t.Errorf(" ip_bans 行数 = %d, want 1（只有那个网段）", n)
	}
}

func (h *harness) countIPBans() int {
	h.t.Helper()
	var n int
	if err := h.db.SQL.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM ip_bans`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// 权限：能封人的人才能看见访客地址；闸门关掉时这一页自己说明为什么。
func TestIPPageNeedsIPManageAndTellsWhyWhenUseless(t *testing.T) {
	h := newHarness(t)

	plain := h.author("ip0")
	if code, _ := h.getAs(plain, "/admin/ips"); code != http.StatusForbidden {
		t.Errorf("普通人进 IP 页 = %d, want 403", code)
	}
	if _, page := h.htmlGet("/", ""); strings.Contains(page, "/admin/ips") {
		t.Error("普通人导航栏里出现了封禁入口")
	}

	watcher := h.author("ip1", permIPManage)
	// 测试里所有真实请求都来自 127.0.0.1，闸门应当是关的。
	h2 := newHarness(t)
	keeper := h2.author("ip2", permIPManage)
	code, page := h2.getAs(keeper, "/admin/ips")
	if code != http.StatusOK {
		t.Fatalf("持权限者进 IP 页 = %d", code)
	}
	if !strings.Contains(page, "这一页现在是收起的") || !strings.Contains(page, "127.0.0.1") {
		t.Errorf("纯环回来源时没有说明为什么关闭：\n%s", short(page))
	}
	// 导航入口也跟着收起，但持权限的人仍然能直接打开这一页看到解释。
	if _, home := h2.getAs(keeper, "/"); strings.Contains(home, "/admin/ips") {
		t.Error("闸门关着时导航栏还留着入口")
	}
	// 一旦收到过真实来源，入口回来。
	h2.from(http.MethodGet, "/problems", "203.0.113.20:1", "")
	if _, home := h2.getAs(keeper, "/"); !strings.Contains(home, "/admin/ips") {
		t.Error("已经有真实访客，导航栏却还没有封禁入口")
	}
	_ = watcher
	_ = plain
}

// 流量表：按地址数 GET/POST/注册，并且真的排出 Top。
func TestTrafficCountsPerAddress(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		h.from(http.MethodGet, "/problems", "203.0.113.30:1", "")
	}
	h.from(http.MethodGet, "/rank", "203.0.113.31:1", "")
	if _, body := h.from(http.MethodPost, "/admin/ips/ban", "127.0.0.1:1", h.adminToken(), url.Values{"ip": {"203.0.113.99"}}); !strings.Contains(body, "已封禁") {
		t.Fatalf("封禁失败：\n%s", short(body))
	}
	for i := 0; i < 2; i++ {
		h.from(http.MethodGet, "/problems", "203.0.113.99:1", "")
	}

	keeper := h.author("ip3", permIPManage)
	_, page := h.getAs(keeper, "/admin/ips")
	for _, want := range []string{"203.0.113.30", "203.0.113.31", "203.0.113.99", "流量 Top"} {
		if !strings.Contains(page, want) {
			t.Errorf("流量页缺 %q：\n%s", want, short(page))
		}
	}
	// 203.0.113.30 打了三次，应当排在只打一次的 .31 前面。
	i30, i31 := strings.Index(page, "203.0.113.30"), strings.Index(page, "203.0.113.31")
	if i30 > i31 {
		t.Errorf("Top 排序不对：.30 出现 %d，.31 出现 %d", i30, i31)
	}
	// 被拦的请求也要单独计数，那正是封禁有没有在起作用。
	rows, _ := h.app.traffic.top(50)
	var blocked, gets uint64
	for _, r := range rows {
		switch r.IP {
		case "203.0.113.99":
			blocked = r.Blocked
		case "203.0.113.30":
			gets = r.Get
		}
	}
	if blocked != 2 {
		t.Errorf("被封地址的被拒数 = %d, want 2", blocked)
	}
	if gets != 3 {
		t.Errorf("203.0.113.30 的 GET 数 = %d, want 3", gets)
	}
}

func TestNormalizeIPRule(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"203.0.113.9", "203.0.113.9", true},
		{" 203.0.113.009 ", "", false}, // 前导零不是合法写法，别让它变成第二条规则
		{"203.0.113.0/24", "203.0.113.0/24", true},
		{"203.0.113.7/24", "203.0.113.0/24", true}, // 存的是网段本身，不是那个地址
		{"::ffff:203.0.113.9", "203.0.113.9", true},
		{"::ffff:203.0.113.0/120", "203.0.113.0/24", true}, // 存成它本来的 v4 写法，别留一条谁都匹配不上的死规则
		{"127.0.0.1", "", false},
		{"::1", "", false},
		{"127.0.0.1/8", "", false},
		{"::ffff:127.0.0.1", "", false},
		// 封全世界的那两条：按下去的人自己也进不来，而且这一页再也打不开，没人能解封。
		{"0.0.0.0/0", "", false},
		{"::/0", "", false},
		{"255.0.0.0/0", "", false}, // 掩成全零之后是同一条规则
		{"0.0.0.0", "0.0.0.0", true},
		{"10.0.0.0/8", "10.0.0.0/8", true},   // 校园内网地址是有意义的封禁粒度
		{"128.0.0.0/1", "128.0.0.0/1", true}, // 只挡 /0 这一条线，更宽的网段仍归操作者判断
		{"example.com", "", false},
	} {
		got, _, err := store.NormalizeIPRule(c.in)
		if !c.ok {
			if err == nil {
				t.Errorf("%q 应当被拒绝", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 被拒绝了：%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeIPRule(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
