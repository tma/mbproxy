// Package config handles configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ReadOnlyMode defines how write requests are handled.
type ReadOnlyMode string

const (
	ReadOnlyOff  ReadOnlyMode = "false" // Full read/write passthrough
	ReadOnlyOn   ReadOnlyMode = "true"  // Silently ignore writes, return success
	ReadOnlyDeny ReadOnlyMode = "deny"  // Reject writes with exception
)

// Config holds the proxy configuration.
type Config struct {
	Listen          string
	Upstream        string
	DefaultSlaveID  byte
	CacheTTL        time.Duration
	CacheServeStale bool
	ReadOnly        ReadOnlyMode
	Timeout         time.Duration
	RequestDelay    time.Duration
	ShutdownTimeout time.Duration
	LogLevel        string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Listen:          getEnv("MODBUS_LISTEN", ":5502"),
		Upstream:        os.Getenv("MODBUS_UPSTREAM"),
		DefaultSlaveID:  1,
		CacheTTL:        10 * time.Second,
		CacheServeStale: false,
		ReadOnly:        ReadOnlyOn,
		Timeout:         10 * time.Second,
		RequestDelay:    0,
		ShutdownTimeout: 30 * time.Second,
		LogLevel:        getEnv("LOG_LEVEL", "INFO"),
	}

	if cfg.Upstream == "" {
		return nil, fmt.Errorf("MODBUS_UPSTREAM is required")
	}

	// Parse slave ID
	if s := os.Getenv("MODBUS_SLAVE_ID"); s != "" {
		id, err := strconv.ParseUint(s, 10, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_SLAVE_ID: %w", err)
		}
		cfg.DefaultSlaveID = byte(id)
	}

	// Parse cache TTL
	if s := os.Getenv("MODBUS_CACHE_TTL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_CACHE_TTL: %w", err)
		}
		cfg.CacheTTL = d
	}

	// Parse serve stale
	if s := os.Getenv("MODBUS_CACHE_SERVE_STALE"); s != "" {
		cfg.CacheServeStale = strings.ToLower(s) == "true"
	}

	// Parse readonly mode
	if s := os.Getenv("MODBUS_READONLY"); s != "" {
		switch strings.ToLower(s) {
		case "false":
			cfg.ReadOnly = ReadOnlyOff
		case "true":
			cfg.ReadOnly = ReadOnlyOn
		case "deny":
			cfg.ReadOnly = ReadOnlyDeny
		default:
			return nil, fmt.Errorf("invalid MODBUS_READONLY: %s (must be false, true, or deny)", s)
		}
	}

	// Parse timeout
	if s := os.Getenv("MODBUS_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_TIMEOUT: %w", err)
		}
		cfg.Timeout = d
	}

	// Parse request delay
	if s := os.Getenv("MODBUS_REQUEST_DELAY"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_REQUEST_DELAY: %w", err)
		}
		cfg.RequestDelay = d
	}

	// Parse shutdown timeout
	if s := os.Getenv("MODBUS_SHUTDOWN_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = d
	}

	return cfg, nil
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
