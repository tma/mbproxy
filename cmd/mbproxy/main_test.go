package main

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/tma/mbproxy/internal/config"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCheckUpstreamHealth_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	cfg := &config.Config{
		Upstream: ln.Addr().String(),
		Timeout:  time.Second,
	}
	if err := checkUpstreamHealth(cfg, newTestLogger()); err != nil {
		t.Fatalf("expected health check to succeed, got %v", err)
	}

	<-acceptDone
}

func TestCheckUpstreamHealth_Failure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := &config.Config{
		Upstream: addr,
		Timeout:  100 * time.Millisecond,
	}
	if err := checkUpstreamHealth(cfg, newTestLogger()); err == nil {
		t.Fatal("expected health check to fail")
	}
}
