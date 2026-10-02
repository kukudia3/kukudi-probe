// probe-server 是极简 VPS 探针的服务端：单进程、单二进制、SQLite，无中间件。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	// 内嵌时区数据库：精简的 Linux 镜像里没有 tzdata 时，--timezone Asia/Shanghai 依然可用。
	_ "time/tzdata"

	"probe/internal/config"
	"probe/internal/server"
	"probe/internal/store"
	"probe/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "probe-server: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Parse(args, os.Getenv, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if cfg.ShowVersion {
		fmt.Printf("probe-server %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
		return nil
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("加载时区 %q 失败: %w", cfg.Timezone, err)
	}

	ctx := context.Background()
	dbPath := filepath.Join(cfg.DataDir, "probe.db")
	// 救援命令先确认"库真的在那儿"。store.Open 会顺手把库建出来 ——
	// --data-dir 填错时那会留下一个空库，而用户看到的是"重置成功、本来就没启用"，
	// 于是以为事情办好了。宁可在这里直接报错。
	if cfg.Reset2FA {
		if _, err := os.Stat(dbPath); err != nil {
			return fmt.Errorf("找不到数据库 %s：--data-dir 是不是填错了？"+
				"（安装脚本用的是 /var/lib/probe-server，可用 systemctl cat probe-server 核对）", dbPath)
		}
	}
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("关闭数据库失败", "err", err)
		}
	}()

	schemaVersion, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	logger.Info("数据库已就绪", "path", dbPath, "schema_version", schemaVersion)

	// --reset-2fa：本机救援命令。做完就退出，不启动服务
	// （用户接下来要做的是"用密码登录面板"，而不是让一个后台进程再抢一次数据库）。
	if cfg.Reset2FA {
		return server.ResetTwoFactor(ctx, db, cfg.DataDir, logger)
	}

	srv := server.New(cfg, db, logger, loc)
	return srv.Run(ctx)
}

func newLogger(cfg config.Server) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
