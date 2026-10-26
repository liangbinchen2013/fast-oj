package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/liangbinchen2013/fast-oj/internal/api"
	"github.com/liangbinchen2013/fast-oj/internal/config"
	"github.com/liangbinchen2013/fast-oj/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fast-oj:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	if err := checkArgvSecrets(args, cfg); err != nil {
		return err
	}

	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	db, err := store.Open(context.Background(), cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	app := api.New(cfg, log, db)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 积分那一套自己走：按管理员在 /admin/points 设的间隔重算所有人的分值与颜色。
	// 挂在同一个 ctx 上，收到信号就和 HTTP 一起停。
	go app.RunPoints(ctx)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	log.Info("listening", "addr", ln.Addr().String(), "data", cfg.DataDir, "db", cfg.DBPath)

	serveDone := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveDone <- err
			return
		}
		serveDone <- nil
	}()

	select {
	case err := <-serveDone:
		return err
	case <-ctx.Done():
		log.Info("signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// checkArgvSecrets 拦住"把令牌写在命令行上"这种启动方式。Linux 上它是硬拒绝：
// /proc/<pid>/cmdline 所有本地账号可读，判机上还跑着别人的代码，写在 argv 里就等于把
// 令牌发给了整台机器。其他平台（Windows 开发机：没有 /proc，单机一个人用）只警告，
// 免得 README 里的快速上手变成砖。
func checkArgvSecrets(args []string, cfg config.Config) error {
	leaked := config.ArgvSecrets(args, map[string]string{
		"-admin-token": cfg.AdminToken, "-judge-token": cfg.JudgeToken,
	})
	if len(leaked) == 0 {
		return nil
	}
	if runtime.GOOS == "linux" {
		return fmt.Errorf("令牌不能写在命令行上（%s）：/proc/<pid>/cmdline 这台机器上谁都读得到。"+
			"改用环境变量 FASTOJ_ADMIN_TOKEN / FASTOJ_JUDGE_TOKEN——systemd 用 EnvironmentFile 提供，"+
			"不要出现在 ExecStart 里", strings.Join(leaked, "、"))
	}
	fmt.Fprintf(os.Stderr, "fast-oj: 警告：%s 的值出现在命令行上，本机上其他账号看得到；"+
		"正式部署请改用环境变量 FASTOJ_ADMIN_TOKEN / FASTOJ_JUDGE_TOKEN\n", strings.Join(leaked, "、"))
	return nil
}
