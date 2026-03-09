package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/tma/mbproxy/internal/config"
	"github.com/tma/mbproxy/internal/logging"
	"github.com/tma/mbproxy/internal/modbus"
	"github.com/tma/mbproxy/internal/proxy"
)

func main() {
	healthCheck := flag.Bool("health", false, "run health check and exit")
	flag.Parse()

	if *healthCheck {
		if err := runHealthCheck(); err != nil {
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

	if err := p.Shutdown(cfg.ShutdownTimeout); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	logger.Info("shutdown complete")
}

func runHealthCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	return checkUpstreamHealth(cfg, logging.New(cfg.LogLevel))
}

func checkUpstreamHealth(cfg *config.Config, logger *slog.Logger) error {
	client := modbus.NewClient(cfg.Upstream, cfg.Timeout, cfg.RequestDelay, cfg.ConnectDelay, logger)
	if err := client.Connect(); err != nil {
		return err
	}
	return client.Close()
}
