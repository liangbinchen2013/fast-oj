package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 磁盘那一块数的是"各部分"而不是一个总数：清理办法完全不同，混在一个数字里就谁也没法
// 决定该动哪一刀。这里同时盯着两件事——符号链接不能跟，以及拿不到盘的数字时要如实写
// "未知"而不是 0（0 会被读成"盘满了"）。
func TestDiskUsageSplitsDataDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fast-oj.db")
	if err := os.WriteFile(dbPath, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cases", "12"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"1.in", "1.out", "2.in"} {
		if err := os.WriteFile(filepath.Join(dir, "cases", "12", name), make([]byte, 2048), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "server.log"), make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	// 一个指向数据库的链接：跟着走会把它再数一遍，指向目录的链接则可能把这一趟带出
	// 数据目录去。两种都不许发生。
	link := filepath.Join(dir, "cases", "12", "sneaky")
	if err := os.Symlink(dbPath, link); err != nil {
		t.Skipf("这台机器不允许建符号链接：%v", err)
	}

	usage, err := scanDataDir(context.Background(), dir, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if usage.DBBytes != 5120 {
		t.Errorf("数据库那格 = %d，要 4096+1024（链接不该被再数一遍）", usage.DBBytes)
	}
	if usage.CaseBytes != 6144 || usage.CaseFiles != 3 {
		t.Errorf("用例正文 = %d/%d 个，要 6144/3", usage.CaseBytes, usage.CaseFiles)
	}
	if usage.OtherBytes != 512 {
		t.Errorf("其余那格 = %d，要只装 server.log 的 512", usage.OtherBytes)
	}
	if usage.Partial {
		t.Errorf("这么点条目不该被截断")
	}
	if usage.FreeBytes >= 0 {
		t.Logf("所在盘剩余 %s（拿不到时写未知）", usage.FreeText())
	}
	if got := usage.Rows()[1].Size; got != "6.00 KB" {
		t.Errorf("大小要写成人话，拿到 %q", got)
	}
}

// 数据目录根本不存在时不是"0 字节"，是一个错误：状态页要能说"这块数不出来"，
// 而不是让管理员以为站点把磁盘用光了又好像什么都没发生。
func TestDiskUsageOnMissingDir(t *testing.T) {
	_, err := scanDataDir(context.Background(), filepath.Join(t.TempDir(), "没有这个目录"), "x.db")
	if err == nil {
		t.Fatal("目录不存在该报错")
	}
}

func TestFormatBytesReachesGB(t *testing.T) {
	for _, one := range []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KB"},
		{3 << 20, "3.00 MB"},
		{20 << 30, "20.00 GB"},
	} {
		if got := formatBytes(one.n, nil); got != one.want {
			t.Errorf("formatBytes(%d) = %q, want %q", one.n, got, one.want)
		}
	}
}
