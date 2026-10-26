package api

import (
	"net/url"
	"strings"
	"testing"

	"github.com/liangbinchen2013/fast-oj/internal/store"
)

// 标签是"别人怎么称呼这个人"：管理员贴一次，凡是出现名字的地方都跟着显示，
// 而它不进那道改权限要的身份确认（见 store/tags.go 与 admin.go 的理由）。

func TestAdminTagsShowUpWhereverTheNameIsPrinted(t *testing.T) {
	h := newHarness(t)
	// 两把钥匙都给：这一条要同时看"标签不用身份确认"和"改权限要"。
	chief := h.author("chief_t", permUserManage, permPermissionManage)
	author := h.author("tagged_t")

	if _, page := h.postAs(chief, "/admin/users/tagged_t/tags", url.Values{"tags": {"作弊者"}}); !strings.Contains(page, "标签已保存") {
		t.Fatalf("保存标签没成：\n%s", short(page))
	}
	// 夹具注册时给每个号都写了昵称，所以名字是"tagged_t 同学"，标签跟在它后面。
	if _, page := h.postAs(author, "/feed", url.Values{"body": {"今天被贴了个标签"}}); !strings.Contains(page, "今天被贴了个标签") {
		t.Fatalf("发动态没成：\n%s", short(page))
	}
	_, page := h.htmlGet("/feed", "")
	if !strings.Contains(page, "tagged_t 同学 [作弊者]") {
		t.Errorf("动态流里的名字没跟着显示标签：\n%s", short(page))
	}
	if _, page := h.htmlGet("/rank", ""); !strings.Contains(page, "tagged_t 同学 [作弊者]") {
		t.Errorf("排行榜上的名字没跟着显示标签：\n%s", short(page))
	}
	rows := h.auditRows("user.tags")
	if len(rows) != 1 || rows[0].Target != "tagged_t" || !strings.Contains(rows[0].Detail, "作弊者") {
		t.Errorf("审计里没有这条： %+v", rows)
	}

	// 同一个会话、没有做过身份确认：改权限要被挡，改标签不该被挡。这就是这两个动作的区别。
	if _, page := h.postAs(chief, "/admin/perms/u/tagged_t",
		url.Values{"key": {permProblemCreate}, "action": {"grant"}}); !strings.Contains(page, "请输入你的密码") {
		t.Errorf("改权限竟然没要身份确认：\n%s", short(page))
	}
	if _, page := h.postAs(chief, "/admin/users/tagged_t/tags", url.Values{"tags": {"作弊者，退役"}}); strings.Contains(page, "请输入你的密码") {
		t.Errorf("改标签被那道身份确认挡了（它不该挡）：\n%s", short(page))
	}
}

func TestTagsRejectBadInputWithoutChangingAnything(t *testing.T) {
	h := newHarness(t)
	chief := h.author("chief_t2", permUserManage)
	h.author("tagged_t2")
	if _, page := h.postAs(chief, "/admin/users/tagged_t2/tags", url.Values{"tags": {"老样子"}}); !strings.Contains(page, "标签已保存") {
		t.Fatalf("先贴一个标签就该成：\n%s", short(page))
	}

	for _, c := range []struct{ tags, want string }{
		{"a,b,c,d", "最多 3 个"},
		{"带[方括号]", "不能用的字"},
		{"这个标签名字实在是太长了没有人能读完这种东西", "不能用的字"},
	} {
		if _, page := h.postAs(chief, "/admin/users/tagged_t2/tags", url.Values{"tags": {c.tags}}); !strings.Contains(page, c.want) {
			t.Errorf("提交 %q 没有给出 %q：\n%s", c.tags, c.want, short(page))
		}
		if u := h.user("tagged_t2"); u.Tags != "老样子" {
			t.Fatalf("失败的保存把库改了：%q", u.Tags)
		}
	}
	// 归一化：中文逗号与空格都认，存进去的是英文逗号分隔的规范形状。
	if _, page := h.postAs(chief, "/admin/users/tagged_t2/tags", url.Values{"tags": {" 新生 ， 退役选手 "}}); !strings.Contains(page, "标签已保存") {
		t.Fatalf("归一化那次保存没成：\n%s", short(page))
	}
	if u := h.user("tagged_t2"); u.Tags != "新生,退役选手" {
		t.Errorf("存下来的是 %q", u.Tags)
	}
}

func TestTagsNeedTheRightAndRefuseOwnerAndSelf(t *testing.T) {
	h := newHarness(t)
	plain := h.author("plain_t")
	if _, page := h.postAs(plain, "/admin/users/plain_t/tags", url.Values{"tags": {"self"}}); !strings.Contains(page, "缺少权限") {
		t.Errorf("普通人改标签竟然没被拒：\n%s", short(page))
	}
	chief := h.author("chief_t3", permUserManage)
	// 自己的账号：控制台一贯不许动自己（避免贴一个摘不掉的标签给自己，也避免把这条规则玩出两个版本）。
	if _, page := h.postAs(chief, "/admin/users/chief_t3/tags", url.Values{"tags": {"管理员"}}); !strings.Contains(page, "不能修改自己") {
		t.Errorf("给自己贴标签没被拦住：\n%s", short(page))
	}
	// 站主免疫：别人的标签也贴不到他身上。
	if _, page := h.postAs(chief, "/admin/users/"+claimHandle+"/tags", url.Values{"tags": {"作弊者"}}); !strings.Contains(page, "超级管理员") {
		t.Errorf("给站主贴标签没被拦住：\n%s", short(page))
	}
	if u := h.user(claimHandle); u.Tags != "" {
		t.Errorf("站主的标签列被动了：%q", u.Tags)
	}
}

// 注销之后不该还挂着生前的标签：那一行显示的是"封禁用户"，标签属于被抹掉的身份。
func TestTagsAreErasedWithTheAccount(t *testing.T) {
	h := newHarness(t)
	chief := h.author("chief_t4", permUserManage, permTicketHandle)
	h.author("victim_t4")
	if _, page := h.postAs(chief, "/admin/users/victim_t4/tags", url.Values{"tags": {"作弊者"}}); !strings.Contains(page, "标签已保存") {
		t.Fatalf("贴标签没成：\n%s", short(page))
	}
	victimID := h.user("victim_t4").ID
	// 注销是四个"要再证一次身份"的动作之一，所以这里先确认一次——这一步存在的意义就是
	// 拿着会话的人答得出密码。
	if page := h.unlock(chief, "/admin/perms/u/victim_t4"); !strings.Contains(page, "身份已确认") {
		t.Fatalf("身份确认没成：\n%s", short(page))
	}
	if _, page := h.postAs(chief, "/admin/users/victim_t4/cancel", url.Values{"confirm": {"victim_t4"}}); !strings.Contains(page, "已注销") {
		t.Fatalf("注销没成：\n%s", short(page))
	}
	var left string
	if err := h.db.SQL.QueryRowContext(ctx(), `SELECT tags FROM users WHERE id=?`, victimID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != "" {
		t.Errorf("注销后标签还在：%q", left)
	}
	_, page := h.htmlGet("/rank", "")
	if strings.Contains(page, "作弊者") {
		t.Errorf("注销之后排行榜上还挂着生前那个标签：\n%s", short(page))
	}
	if !strings.Contains(page, store.AnonymousName) {
		t.Errorf("排行榜上没有出现墓碑署名：\n%s", short(page))
	}
}
