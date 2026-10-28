package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// 站点统计：多少人、多少提交、人均多少、判题判出了什么、GET/POST 各来了多少次。
// 只有持 stats.view 的人看得到——能改首页文案的人不必看到运营数据。

func TestStatsPageNeedsTheRight(t *testing.T) {
	h := newHarness(t)
	plain := h.author("stats0")
	if code, _ := h.getAs(plain, "/admin/stats"); code != http.StatusForbidden {
		t.Fatalf("普通人进统计页 = %d, want 403", code)
	}
	// 导航栏也不该给普通人留这个入口。
	if _, page := h.getAs(plain, "/"); strings.Contains(page, "/admin/stats") {
		t.Error("普通人导航栏里出现了统计入口")
	}
	watcher := h.author("stats1", permStatsView)
	code, page := h.getAs(watcher, "/admin/stats")
	if code != http.StatusOK {
		t.Fatalf("持权限的人进统计页 = %d", code)
	}
	for _, want := range []string{"站点统计", "人均提交", "GET 请求", "POST 请求", "判定分布", "本次运行以来"} {
		if !strings.Contains(page, want) {
			t.Errorf("统计页缺 %q：\n%s", want, short(page))
		}
	}
	// 持 site.settings 的人不该顺带看到运营数据。
	operator := h.author("stats2", permSiteSettings)
	if code, _ := h.getAs(operator, "/admin/stats"); code != http.StatusForbidden {
		t.Errorf("只有 site.settings 的人进了统计页 = %d", code)
	}
	// 管理员令牌那条身份照样能看。
	if code, _ := h.htmlGet("/admin/stats", h.adminToken()); code != http.StatusOK {
		t.Errorf("管理员看统计 = %d", code)
	}
}

// 数字是真的：建一个号、提交一次，页面上就要多出这些数。
func TestStatsCountsWhatHappened(t *testing.T) {
	h := newHarness(t)
	h.readyProblem("P8000")
	player := h.author("stats3")
	if code, body := h.submitCode(player, "P8000", "cpp", "int main(){return 0;}"); code != http.StatusOK {
		t.Fatalf("提交 = %d\n%s", code, short(body))
	}

	// 读这一页的人得有 stats.view，否则拿到的是 403，什么数字都看不到。
	watcher := h.author("stats4", permStatsView)
	_, page := h.getAs(watcher, "/admin/stats")
	if !strings.Contains(page, "P8000") {
		t.Errorf("提交最多的题那一栏里没有 P8000：\n%s", short(page))
	}
	// 请求计数：这一路走过来 GET 一定有数，POST 也有（提交与建号都是 POST）。
	if n := numberAfter(page, "GET 请求"); n < 3 {
		t.Errorf("GET 请求计数 = %d，看着没在数", n)
	}
	if n := numberAfter(page, "POST 请求"); n < 2 {
		t.Errorf("POST 请求计数 = %d，看着没在数", n)
	}
}

// numberAfter pulls the first number that follows a label on the page.
func numberAfter(page, label string) int {
	i := strings.Index(page, label)
	if i < 0 {
		return -1
	}
	j := i + len(label)
	for j < len(page) && (page[j] < '0' || page[j] > '9') {
		j++
	}
	start := j
	for j < len(page) && page[j] >= '0' && page[j] <= '9' {
		j++
	}
	if start == j {
		return -1
	}
	n, err := strconv.Atoi(page[start:j])
	if err != nil {
		return -1
	}
	return n
}
