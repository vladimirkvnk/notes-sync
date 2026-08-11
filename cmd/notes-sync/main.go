package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vladimirkvnk/notes-sync/internal/config"
	"github.com/vladimirkvnk/notes-sync/internal/daemon"
	"github.com/vladimirkvnk/notes-sync/internal/gitx"
	notification "github.com/vladimirkvnk/notes-sync/internal/notify"
)

// main starts the command-line application.
func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute runs one CLI invocation and returns its exit code.
func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	command := args[0]
	if command == "help" || command == "-h" || command == "--help" {
		printUsage(stdout)
		return 0
	}
	if command != "run" && command != "check" {
		fmt.Fprintf(stderr, "unknown command %q\n\n", command)
		printUsage(stderr)
		return 2
	}

	defaultConfig, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "notes-sync: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfig, "path to the JSON configuration file")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "notes-sync: unexpected positional arguments")
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "notes-sync: %v\n", err)
		return 1
	}
	logger := newLogger(stderr, cfg.LogLevel)
	git, err := gitx.NewClient()
	if err != nil {
		logger.Error("startup failed", "error", err)
		return 1
	}

	if command == "check" {
		return checkConfig(context.Background(), stdout, logger, cfg, git)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manager, err := daemon.Prepare(ctx, cfg, git, notification.New(cfg.Notifications), logger)
	if err != nil {
		logger.Error("startup validation failed", "error", err)
		return 1
	}
	if err := manager.Run(ctx); err != nil {
		logger.Error("daemon stopped unexpectedly", "error", err)
		return 1
	}
	return 0
}

// checkConfig validates configured repositories without network access.
func checkConfig(ctx context.Context, stdout io.Writer, logger *slog.Logger, cfg config.Config, git *gitx.Client) int {
	infos, err := daemon.InspectRepositories(ctx, cfg, git)
	if err != nil {
		logger.Error("configuration check failed", "error", err)
		return 1
	}
	for i, info := range infos {
		if err := daemon.ValidateWorkingState(ctx, cfg.Repositories[i], info, git, cfg.GitTimeout); err != nil {
			logger.Error("repository is not ready", "repository", info.Name, "path", info.Root, "error", err)
			return 1
		}
		fmt.Fprintf(stdout, "ok: %s (%s -> %s)\n", info.Root, info.Branch, info.Upstream)
	}
	return 0
}

// newLogger creates the service logger at the configured level.
func newLogger(w io.Writer, levelName string) *slog.Logger {
	level := slog.LevelInfo
	switch levelName {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// printUsage writes the supported command syntax.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  notes-sync run   [--config PATH]")
	fmt.Fprintln(w, "  notes-sync check [--config PATH]")
}
