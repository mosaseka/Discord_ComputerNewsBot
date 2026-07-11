package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/app"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/brief"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/config"
)

func main() { os.Exit(run()) }

func run() int {
	var mode config.Mode
	var feedPath string
	flag.BoolVar(&mode.Once, "once", false, "generate one briefing and exit")
	flag.BoolVar(&mode.DryRun, "dry-run", false, "do not post to Discord; print the briefing")
	flag.StringVar(&mode.Fixture, "fixture", "", "offline fixture directory (requires --once and forces --dry-run)")
	flag.StringVar(&feedPath, "config", "config/feeds.yaml", "feeds YAML path")
	flag.Parse()

	cfg, err := config.Load(mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	var feeds []config.Feed
	if cfg.Mode.Fixture == "" {
		feeds, err = config.LoadFeeds(feedPath)
		if err != nil {
			logger.Error("load feeds", "error", err)
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.New(ctx, cfg, feeds, logger)
	if err != nil {
		logger.Error("initialize application", "error", err)
		return 1
	}
	defer application.Close()

	if cfg.Mode.Once {
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		result, err := application.RunOnce(runCtx)
		if err != nil {
			logger.Error("one-time briefing failed", "error", err)
			return 1
		}
		if cfg.Mode.DryRun {
			fmt.Print(brief.FormatText(result))
		}
		logger.Info("one-time briefing completed", "fetched", result.FetchedCount, "selected", result.SelectedCount, "posted", result.PostedCount, "outcome", result.Outcome)
		return 0
	}
	if err := application.RunNormal(ctx); err != nil && ctx.Err() == nil {
		logger.Error("application stopped", "error", err)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel}))
}
