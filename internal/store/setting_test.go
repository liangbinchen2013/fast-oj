package store

import (
	"context"
	"testing"
)

func TestSiteSettingsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := mustUser(t, db, "admin9")

	site, err := db.Site(ctx)
	if err != nil || site.Title != "fast oj" || !site.AllowRegister || len(site.Links) != 0 {
		t.Fatalf("defaults = %+v %v", site, err)
	}
	if site.APIHint != DefaultAPIHint {
		t.Errorf("api hint default = %q", site.APIHint)
	}

	if err := db.SetSiteAll(ctx, admin.ID, SiteConfig{
		Title: "我的 OJ", Tagline: "欢迎", Footer: "出题请走管理台",
		Links: []SiteLink{{Title: "洛谷", URL: "https://luogu.com.cn"}},
	}); err != nil {
		t.Fatal(err)
	}
	site, err = db.Site(ctx)
	if err != nil || site.Title != "我的 OJ" || site.Footer == "" {
		t.Fatalf("after save = %+v %v", site, err)
	}
	if len(site.Links) != 1 || site.Links[0].Title != "洛谷" {
		t.Fatalf("links = %+v", site.Links)
	}

	// Only an explicit "0" locks registration, and absent links stay an empty
	// list rather than a null the UI has to special-case.
	if err := db.SetSiteAll(ctx, admin.ID, SiteConfig{Title: "我的 OJ"}); err != nil {
		t.Fatal(err)
	}
	site, err = db.Site(ctx)
	if err != nil || site.AllowRegister {
		t.Fatalf("allow_register = %v %v", site.AllowRegister, err)
	}
	if site.Links == nil || len(site.Links) != 0 {
		t.Errorf("links = %#v, want an empty list", site.Links)
	}
	// Saving with the field cleared writes an empty row: the default is only for a
	// key that was never set, otherwise "hide this line" could not be saved.
	if site.APIHint != "" {
		t.Errorf("api hint after clearing = %q, want empty", site.APIHint)
	}
}

// 提交速率那两条数字是设置而不是开关：缺行读出来必须是"没设"（0），而"没设"在
// 单用户那条上的含义是回到启动参数，在全站那条上是"不限量"。写坏的数字不能把整份
// 设置读不出来——这一页存在的意义就是把填错的改对。
func TestSiteRateSettingsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	admin := mustUser(t, db, "admin11")

	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if site.SubmitPerMinute != 0 || site.SiteSubmitPerMinute != 0 {
		t.Fatalf("defaults = %d/%d, want 0/0", site.SubmitPerMinute, site.SiteSubmitPerMinute)
	}

	if err := db.SetSiteAll(ctx, admin.ID, SiteConfig{
		Title: "我的 OJ", SubmitPerMinute: 3, SiteSubmitPerMinute: 200,
	}); err != nil {
		t.Fatal(err)
	}
	site, err = db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if site.SubmitPerMinute != 3 || site.SiteSubmitPerMinute != 200 {
		t.Fatalf("after save = %d/%d", site.SubmitPerMinute, site.SiteSubmitPerMinute)
	}

	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE site_settings SET value='乱七八糟' WHERE key='site.submit_per_minute'`); err != nil {
		t.Fatal(err)
	}
	site, err = db.Site(ctx)
	if err != nil {
		t.Fatalf("一行写坏的数字不该让整份设置读不出来：%v", err)
	}
	if site.SubmitPerMinute != 0 {
		t.Errorf("坏值该落回 0，拿到 %d", site.SubmitPerMinute)
	}
	if site.SiteSubmitPerMinute != 200 {
		t.Errorf("另一条不该被牵连，拿到 %d", site.SiteSubmitPerMinute)
	}
}
