package config

import (
	"path/filepath"
	"testing"
)

func TestLoadRequiresTokens(t *testing.T) {
	if _, err := Load(nil); err == nil {
		t.Fatal("expected error when tokens are missing")
	}
}

func TestLoadDefaultsAndDerivedDBPath(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load([]string{"-data", dir, "-admin-token", "a", "-judge-token", "j"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("addr = %q", cfg.Addr)
	}
	if want := filepath.Join(dir, "fast-oj.db"); cfg.DBPath != want {
		t.Errorf("db = %q, want %q", cfg.DBPath, want)
	}
	if cfg.MaxSource != 256<<10 {
		t.Errorf("max-source = %d", cfg.MaxSource)
	}
}

func TestLoadRejectsZeroMaxSource(t *testing.T) {
	if _, err := Load([]string{"-data", t.TempDir(), "-admin-token", "a", "-judge-token", "j", "-max-source", "0"}); err == nil {
		t.Fatal("expected validation error")
	}
}

// 运维令牌留空是合法的：服务端会自己生成一枚一次性的（见 internal/store/admintoken.go）。
// 判机令牌必须给——那是一台远端机器要预先知道的共享串，现场生成等于没人能连上来。
func TestAdminTokenMayBeEmpty(t *testing.T) {
	cfg, err := Load([]string{"-data", t.TempDir(), "-judge-token", "j"})
	if err != nil {
		t.Fatalf("没给运维令牌就该由服务端生成：%v", err)
	}
	if cfg.AdminToken != "" {
		t.Errorf("AdminToken = %q", cfg.AdminToken)
	}
	if _, err := Load([]string{"-data", t.TempDir(), "-admin-token", "a"}); err == nil {
		t.Fatal("判机令牌缺失也该拒绝启动")
	}
}

// 大小闸门默认是关着的：0 = 不限量。这两条曾经是"必须为正"，于是想把单个测试点放宽
// 的人只能先把数字调大，还会被"单条不得大于整包"顶回来——那道不等式只在两边都设了
// 数字时才是人能理解的话。
func TestSizeCapsDefaultToUnlimited(t *testing.T) {
	cfg, err := Load([]string{"-data", t.TempDir(), "-judge-token", "j"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxUpload != 0 || cfg.MaxCase != 0 {
		t.Errorf("默认该不限量，拿到整包 %d / 单条 %d", cfg.MaxUpload, cfg.MaxCase)
	}

	loose, err := Load([]string{"-data", t.TempDir(), "-judge-token", "j", "-max-upload", "100", "-max-case", "5000"})
	if err != nil {
		t.Fatalf("单条大于整包不该是错误：%v", err)
	}
	if loose.MaxCase != 5000 || loose.MaxUpload != 100 {
		t.Errorf("设了闸门就该照填的数走，拿到 %d/%d", loose.MaxUpload, loose.MaxCase)
	}

	// 负数是写坏了：落回"不限量"，而不是变成一个会悄悄截断的怪数。
	neg, err := Load([]string{"-data", t.TempDir(), "-judge-token", "j", "-max-case", "-1", "-max-upload", "-2"})
	if err != nil {
		t.Fatal(err)
	}
	if neg.MaxCase != 0 || neg.MaxUpload != 0 {
		t.Errorf("负数该按不限量处理，拿到 %d/%d", neg.MaxUpload, neg.MaxCase)
	}
}
