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
	AttemptTimeout  time.Duration
	RequestTimeout  time.Duration
	RequestDelay    time.Duration
	ConnectDelay    time.Duration
	ShutdownTimeout time.Duration
	HealthListen    string
	LogLevel        string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Listen:          GetEnv("MODBUS_LISTEN", ":5502"),
		Upstream:        os.Getenv("MODBUS_UPSTREAM"),
		DefaultSlaveID:  1,
		CacheTTL:        10 * time.Second,
		CacheServeStale: false,
		ReadOnly:        ReadOnlyOn,
		AttemptTimeout:  10 * time.Second,
		RequestTimeout:  30 * time.Second,
		RequestDelay:    0,
		ConnectDelay:    0,
		ShutdownTimeout: 30 * time.Second,
		HealthListen:    os.Getenv("HEALTH_LISTEN"),
		LogLevel:        GetEnv("LOG_LEVEL", "INFO"),
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

	attemptTimeout, attemptTimeoutSet, err := parsePositiveDuration("MODBUS_ATTEMPT_TIMEOUT")
	if err != nil {
		return nil, err
	}
	legacyTimeout, legacyTimeoutSet, err := parsePositiveDuration("MODBUS_TIMEOUT")
	if err != nil {
		return nil, err
	}
	if attemptTimeoutSet && legacyTimeoutSet && attemptTimeout != legacyTimeout {
		return nil, fmt.Errorf(
			"MODBUS_ATTEMPT_TIMEOUT (%s) and deprecated MODBUS_TIMEOUT (%s) must match when both are set",
			attemptTimeout,
			legacyTimeout,
		)
	}
	if attemptTimeoutSet {
		cfg.AttemptTimeout = attemptTimeout
	} else if legacyTimeoutSet {
		cfg.AttemptTimeout = legacyTimeout
	}

	if s := os.Getenv("MODBUS_REQUEST_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_REQUEST_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid MODBUS_REQUEST_TIMEOUT: must be greater than zero")
		}
		cfg.RequestTimeout = d
	}

	// Parse request delay
	if s := os.Getenv("MODBUS_REQUEST_DELAY"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_REQUEST_DELAY: %w", err)
		}
		cfg.RequestDelay = d
	}

	// Parse connect delay
	if s := os.Getenv("MODBUS_CONNECT_DELAY"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MODBUS_CONNECT_DELAY: %w", err)
		}
		cfg.ConnectDelay = d
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

func parsePositiveDuration(name string) (time.Duration, bool, error) {
	s := os.Getenv(name)
	if s == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s: %w", name, err)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("invalid %s: must be greater than zero", name)
	}
	return d, true, nil
}

// GetEnv returns the value of the environment variable named by key,
// or defaultValue if the variable is not set.
func GetEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
