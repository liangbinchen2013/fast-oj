package api

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// diskWalkLimit 是数磁盘这一趟最多走这么多条目。一题一目录、一例一文件，正常站点远远
// 到不了这个数；真到了就说明这块盘不该由一个网页来数，宁可停下来说"至少这么多"，
// 而不是把管理员点进来一次就卡住几十秒。
const diskWalkLimit = 200_000

// diskTTL 故意宽松：这一块数字不是拿来报警的，是拿来决定"今天要不要清一清用例"的。
const diskTTL = 30 * time.Second

// DiskUsage 是 fast-oj 各部分在这块盘上占的地方，加上这个目录所在盘的总量与剩余。
//
// 分开数是因为它们的清理办法完全不同：数据库要 VACUUM，用例正文是管理员传上去的文件
// （删了就没了），其余那堆（WAL、日志、临时东西）通常自己会走。混在一个数字里就谁也没法
// 决定该动哪一刀。
type DiskUsage struct {
	DBBytes    int64
	CaseBytes  int64
	CaseFiles  int
	OtherBytes int64
	// TotalBytes/FreeBytes 是所在盘的数字，拿不到（平台不认识、目录还没落盘）时为 -1。
	TotalBytes int64
	FreeBytes  int64
	// Partial 说这一趟是被条目上限截断的：页面上要把"至少"两个字写出来。
	Partial bool
}

type diskCache struct {
	mu     sync.Mutex
	value  DiskUsage
	loaded time.Time
}

// diskUsage 是带缓存的那一份：状态页会被反复打开，而遍历整棵用例目录是磁盘上的活。
func (s *Server) diskUsage(ctx context.Context) (DiskUsage, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if time.Since(s.disk.loaded) < diskTTL {
		return s.disk.value, nil
	}
	usage, err := scanDataDir(ctx, s.cfg.DataDir, s.cfg.DBPath)
	if err != nil {
		return usage, err
	}
	s.disk.value, s.disk.loaded = usage, time.Now()
	return usage, nil
}

// Rows 是给模板的那几张行：名字、写好人话的大小、占数据目录的比例。
func (d DiskUsage) Rows() []diskRow {
	rows := []diskRow{
		{Name: "数据库 fast-oj.db", Bytes: d.DBBytes},
		{Name: "用例正文 cases/", Bytes: d.CaseBytes},
		{Name: "其余（WAL、日志、临时）", Bytes: d.OtherBytes},
	}
	total := d.DBBytes + d.CaseBytes + d.OtherBytes
	for i := range rows {
		rows[i].Size = formatBytes(rows[i].Bytes, nil)
		if total > 0 {
			rows[i].Pct = int(rows[i].Bytes * 100 / total)
		}
	}
	return rows
}

type diskRow struct {
	Name  string
	Size  string
	Bytes int64
	Pct   int
}

// FreeText/TotalText 是盘那一行的两个人话数字，拿不到时如实写"未知"而不是 0——
// "剩余 0 B" 看起来像磁盘满了，那是最不该由一个 bug 说出来的话。
func (d DiskUsage) FreeText() string  { return orUnknown(d.FreeBytes) }
func (d DiskUsage) TotalText() string { return orUnknown(d.TotalBytes) }

func orUnknown(n int64) string {
	if n < 0 {
		return "未知"
	}
	return formatBytes(n, nil)
}

// scanDataDir 走一遍数据目录。符号链接一律不跟：目录里的东西是管理员传上来的，
// 一个指向 /dev/zero 或指向上一题目录的链接，不该让这一趟跟着跑掉。
func scanDataDir(ctx context.Context, dataDir, dbPath string) (DiskUsage, error) {
	usage := DiskUsage{TotalBytes: -1, FreeBytes: -1}
	info, err := os.Stat(dataDir)
	if err != nil {
		return usage, err
	}
	if !info.IsDir() {
		return usage, errors.New("数据目录不是目录")
	}
	usage.TotalBytes, usage.FreeBytes = diskSpace(dataDir)

	dbFiles := map[string]bool{
		dbPath: true, dbPath + "-wal": true, dbPath + "-shm": true,
	}
	casesRoot := filepath.Join(dataDir, "cases") + string(filepath.Separator)

	seen := 0
	walkErr := filepath.WalkDir(dataDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// 一个读不到的子目录（权限、正在被删）不该让整块盘变成"未知"。
			return nil //nolint:nilerr // 少算一个目录比整页报错诚实
		}
		if entry.IsDir() {
			seen++
			if seen > diskWalkLimit {
				usage.Partial = true
				return filepath.SkipAll
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		seen++
		if seen > diskWalkLimit {
			usage.Partial = true
			return filepath.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			return nil //nolint:nilerr // 同上：拿不到大小就跳过这一份
		}
		size := info.Size()
		switch {
		case dbFiles[path]:
			usage.DBBytes += size
		case filepath.Dir(path) == dbPath:
			// 同名兄弟（备份、journal）留在"其余"那一格，别混进数据库。
			usage.OtherBytes += size
		case strings.HasPrefix(path, casesRoot):
			usage.CaseBytes += size
			usage.CaseFiles++
		default:
			usage.OtherBytes += size
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return usage, walkErr
	}
	return usage, nil
}
