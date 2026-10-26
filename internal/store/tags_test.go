package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTagsNormalizeAndReject(t *testing.T) {
	// 中文逗号、多余空格都要归一化：管理员是从键盘上打出来的，不该要求他英文输入法。
	if got := SplitTags(" 作弊者 ，退役选手,新生"); len(got) != 3 || got[0] != "作弊者" || got[2] != "新生" {
		t.Fatalf("切分 = %q", got)
	}
	if got := TagsTextOf("作弊者，退役选手"); got != "作弊者,退役选手" {
		t.Fatalf("规范形式 = %q", got)
	}
	// 分隔符不能穿过标签本身：否则一个人能给自己名字里塞出第二个标签。
	for _, bad := range []string{"a,b", "a[b", "a]b", "a，b", "带\n换行", strings.Repeat("长", MaxTagLen+1)} {
		if validTag(bad) {
			t.Errorf("validTag(%q) 放行了", bad)
		}
	}
	// 读出侧也一样：脏值只会被丢掉，不会把渲染带坏。
	if got := ParseTags("好标签,]恶意[,超长超长超长超长超长超长超长,第二个"); len(got) != 2 || got[0] != "好标签" {
		t.Fatalf("ParseTags = %q", got)
	}
	if got := ParseTags(strings.Repeat("a,"+strings.Repeat("字", 12)+",", 10)); len(got) > MaxTags {
		t.Fatalf("标签数量没被卡住：%d", len(got))
	}
}

func TestSetUserTagsStoresTheCanonicalForm(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	u := mustUser(t, db, "tagged")

	if err := db.SetUserTags(ctx, u.ID, " 作弊者 ， 退役选手 "); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tags != "作弊者,退役选手" {
		t.Fatalf("存下来的是 %q", got.Tags)
	}
	// 超数和带分隔符的都报错，而且不改库里已有的值。
	if err := db.SetUserTags(ctx, u.ID, "a,b,c,d"); !errors.Is(err, ErrTooManyTags) {
		t.Errorf("四个标签 = %v，应该是 ErrTooManyTags", err)
	}
	if err := db.SetUserTags(ctx, u.ID, "合法,不[合[格"); !errors.Is(err, ErrBadTag) {
		t.Errorf("带方括号 = %v，应该是 ErrBadTag", err)
	}
	if again, err := db.GetUserByID(ctx, u.ID); err != nil || again.Tags != "作弊者,退役选手" {
		t.Errorf("失败的保存把库改了：%q（err %v）", again.Tags, err)
	}
	// 清空是合法操作（标签拿错了要能摘掉）。
	if err := db.SetUserTags(ctx, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if again, err := db.GetUserByID(ctx, u.ID); err != nil || again.Tags != "" {
		t.Errorf("清空没生效：%q（err %v）", again.Tags, err)
	}
}

// 标签折进 personSQL 才有意义：那一条表达式是排行榜、提交列表、动态、帖子、评论、工单和
// 私信共同的名字出口，接在这里等于所有视图一起有，且一个查询都不多。
func TestTagsRideOnThePersonName(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	boss := mustUser(t, db, "boss")
	if err := db.SetUserTags(ctx, boss.ID, "站长，退役选手"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Leaderboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, r := range rows {
		if r.Handle == "boss" {
			name = r.Name
		}
	}
	if name != "boss [站长] [退役选手]" {
		t.Fatalf("排行榜上的名字 = %q", name)
	}

	// 注销之后身份连着标签一起没了：留下的那一行不该还挂着生前的话。
	if _, err := db.CancelAccount(ctx, boss.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetUserByID(ctx, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tags != "" {
		t.Errorf("注销后标签还在：%q", got.Tags)
	}
}
