package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tma/mbproxy/internal/config"
	"github.com/tma/mbproxy/internal/health"
	"github.com/tma/mbproxy/internal/logging"
	"github.com/tma/mbproxy/internal/proxy"
)

func main() {
	healthCheck := flag.Bool("health", false, "run health check and exit")
	flag.Parse()

	if *healthCheck {
		addr := os.Getenv("HEALTH_LISTEN")
		if addr == "" {
			fmt.Fprintln(os.Stderr, "HEALTH_LISTEN is not set")
			os.Exit(1)
		}
		if err := health.CheckHealth(addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	logger := logging.New(cfg.LogLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	p, err := proxy.New(cfg, logger)
	if err != nil {
		logger.Error("failed to create proxy", "error", err)
		os.Exit(1)
	}

	// Start health server if configured
	var hs *health.Server
	if cfg.HealthListen != "" {
		hs = health.NewServer(cfg.HealthListen, p, logger)
		hsLn, err := hs.Listen()
		if err != nil {
			logger.Error("failed to start health server", "error", err)
			os.Exit(1)
		}
		go func() {
			if err := hs.Serve(hsLn); err != nil {
				logger.Error("health server error", "error", err)
			}
		}()
	}

	// Start proxy in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Run(ctx)
	}()

	// Wait for shutdown signal or error
	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig)
	case err := <-errCh:
		if err != nil {
			logger.Error("proxy error", "error", err)
			os.Exit(1)
		}
	}

	// Graceful shutdown
	cancel()

	if hs != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			logger.Error("health server shutdown error", "error", err)
		}
	}

	if err := p.Shutdown(cfg.ShutdownTimeout); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	logger.Info("shutdown complete")
}
