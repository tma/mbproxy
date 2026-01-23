// Package logging provides structured logging.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New creates a new logger with the specified level.
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToUpper(level) {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "WARN", "WARNING":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: lvl,
	}

	handler := slog.NewTextHandler(os.Stdout, opts)
	return slog.New(handler)
}
