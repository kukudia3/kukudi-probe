// probe-agent 采集本机指标并上报给 probe-server。
//
// 只读 /proc、/sys 与 statfs，不监听任何端口，不执行任何命令。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"probe/internal/agent"
	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "probe-agent: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.ParseAgent(args, os.Getenv, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if cfg.Version {
		fmt.Printf("probe-agent %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
		return nil
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	if cfg.PrintJSON {
		return printJSON(cfg)
	}
	if !cfg.PlatformSupported() {
		return fmt.Errorf("probe-agent 正式运行目前只支持 Linux（当前 %s）；"+
			"如需在其它平台检查采集逻辑，可用 --print-json 并让 --root 指向一份根文件系统快照", runtime.GOOS)
	}
	return runAgent(cfg, logger)
}

// runAgent 是正式运行路径：加载 Token 与流量 checkpoint，然后一直保持连接。
func runAgent(cfg config.Agent, logger *slog.Logger) error {
	token, warning, err := cfg.ResolveToken()
	if err != nil {
		return err
	}
	if warning != "" {
		logger.Warn(warning)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	statePath := filepath.Join(cfg.StateDir, "state.json")
	traffic, warn, err := agent.LoadTraffic(statePath)
	if err != nil {
		return err
	}
	if warn != "" {
		logger.Warn(warn)
	}
	cp := traffic.Checkpoint()
	logger.Info("流量基线已加载",
		"path", statePath, "iface", cp.Iface,
		"total_rx", cp.TotalRx, "total_tx", cp.TotalTx, "saved_at", cp.SavedAt)

	collector := agent.New(cfg.Root, cfg.Iface, cfg.Disk, traffic)
	client := agent.NewClient(agent.ClientConfig{
		ServerURL:          cfg.Server,
		Token:              token,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		AllowPlaintext:     cfg.AllowPlaintext,
		Once:               cfg.Once,
	}, collector, traffic, logger)

	if cfg.InsecureSkipVerify {
		logger.Warn("已关闭 TLS 证书校验（--insecure-skip-verify）：连接可能被中间人窃听或篡改")
	}
	logger.Info("probe-agent 启动",
		"version", version.Version, "server", cfg.Server,
		"interval_sec", int(cfg.Interval.Seconds()), "iface", orAuto(cfg.Iface), "disk", cfg.Disk)

	if err := client.Run(ctx); err != nil {
		return err
	}
	// 退出前把流量 checkpoint 落盘，避免丢掉最后一段增量。
	if _, err := traffic.MaybeSave(time.Now(), true); err != nil {
		logger.Error("退出时保存流量 checkpoint 失败", "err", err)
	}
	logger.Info("probe-agent 已退出")
	return nil
}

func orAuto(iface string) string {
	if iface == "" {
		return "auto"
	}
	return iface
}

// diagInfo / diagSample 是 --print-json 的输出结构，形状与将来的协议帧一致：
// 先一行 info（对应 hello），再每行一份 metrics。
type diagInfo struct {
	Info protocol.Info `json:"info"`
}

type diagSample struct {
	Metrics  protocol.Metrics `json:"metrics"`
	Warnings []string         `json:"warnings,omitempty"`
}

func printJSON(cfg config.Agent) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 诊断模式使用仅内存的流量统计：不在磁盘上留下任何状态文件。
	traffic, warn, err := agent.LoadTraffic("")
	if err != nil {
		return err
	}
	if warn != "" {
		fmt.Fprintln(os.Stderr, "probe-agent:", warn)
	}
	collector := agent.New(cfg.Root, cfg.Iface, cfg.Disk, traffic)

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	info, err := collector.Info()
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe-agent: 静态信息读取不完整:", err)
	}
	if err := enc.Encode(diagInfo{Info: info}); err != nil {
		return err
	}

	for i := 0; i < cfg.Samples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(cfg.Interval):
			}
		}
		metrics, warnings, err := collector.Sample(time.Now())
		if err != nil {
			return err
		}
		if err := enc.Encode(diagSample{Metrics: metrics, Warnings: warnings}); err != nil {
			return err
		}
	}
	return nil
}

// newLogger 把日志写到 stderr：--print-json 用 stdout 输出数据，两者不能混。
func newLogger(cfg config.Agent) *slog.Logger {
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
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}
