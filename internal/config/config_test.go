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
