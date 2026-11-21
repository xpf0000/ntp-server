package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/aykutsp/ntp-server/internal/config"
	"github.com/aykutsp/ntp-server/internal/httpapi"
	"github.com/aykutsp/ntp-server/internal/metrics"
	"github.com/aykutsp/ntp-server/internal/ntp"
)

var version = "dev"

func main() {
	var (
		configPath     string
		printDefault   bool
		printVersion   bool
		checkConfig    bool
		writeDefaultTo string
	)

	flag.StringVar(&configPath, "config", "", "Path to JSON config file")
	flag.BoolVar(&printDefault, "print-default-config", false, "Print default config and exit")
	flag.StringVar(&writeDefaultTo, "write-default-config", "", "Write default config to file and exit")
	flag.BoolVar(&printVersion, "version", false, "Print version")
	flag.BoolVar(&checkConfig, "check-config", false, "Validate configuration and exit")
	flag.Parse()

	if printVersion {
		fmt.Println(version)
		return
	}

	if printDefault || writeDefaultTo != "" {
		out, err := config.Default().ToPrettyJSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "default config serialize failed: %v\n", err)
			os.Exit(1)
		}
		if printDefault {
			fmt.Println(string(out))
		}
		if writeDefaultTo != "" {
			if err := os.WriteFile(writeDefaultTo, out, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "write default config failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("default config written to %s\n", writeDefaultTo)
		}
		return
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	if checkConfig {
		fmt.Println("Configuration OK")
		return
	}

	var logOutput io.Writer = os.Stdout
	if cfg.Runtime.LogFile != "" {
		path := cfg.Runtime.LogFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(configPath), path)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open log file %q: %v\n", path, err)
			os.Exit(1)
		}
		defer file.Close()
		logOutput = io.MultiWriter(os.Stdout, file)
	}
	logger := newLogger(cfg.Runtime.LogLevel, logOutput)
	runtime.GOMAXPROCS(cfg.Runtime.GOMAXPROCS)
	logger.Info("runtime configured", "gomaxprocs", cfg.Runtime.GOMAXPROCS)

	metricsRegistry := metrics.NewCounters()
	ntpServer, err := ntp.NewServer(cfg, logger, metricsRegistry)
	if err != nil {
		logger.Error("could not start NTP server", "error", err.Error())
		os.Exit(1)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntpServer.Start(rootCtx)

	var apiServer *httpapi.Server
	if cfg.API.Enabled {
		apiServer = httpapi.New(cfg.API, logger, metricsRegistry, ntpServer)
		apiServer.Start()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("shutdown signal received", "signal", sig.String())

	shutdownTimeout := 5 * time.Second
	if cfg.API.ShutdownTimeoutMilli > 0 {
		shutdownTimeout = time.Duration(cfg.API.ShutdownTimeoutMilli) * time.Millisecond
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if apiServer != nil {
		if err := apiServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("management API shutdown warning", "error", err.Error())
		}
	}
	if err := ntpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("NTP server shutdown warning", "error", err.Error())
	}

	logger.Info("shutdown complete")
}

func newLogger(level string, output io.Writer) *slog.Logger {
	level = strings.ToUpper(strings.TrimSpace(level))
	var lv slog.Level
	switch level {
	case "DEBUG":
		lv = slog.LevelDebug
	case "WARN":
		lv = slog.LevelWarn
	case "ERROR":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(output, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}
