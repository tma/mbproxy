package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Set required env var
	os.Setenv("MODBUS_UPSTREAM", "192.168.1.100:502")
	defer os.Unsetenv("MODBUS_UPSTREAM")

	// Clear all optional vars
	os.Unsetenv("MODBUS_LISTEN")
	os.Unsetenv("MODBUS_SLAVE_ID")
	os.Unsetenv("MODBUS_CACHE_TTL")
	os.Unsetenv("MODBUS_CACHE_SERVE_STALE")
	os.Unsetenv("MODBUS_READONLY")
	os.Unsetenv("MODBUS_ATTEMPT_TIMEOUT")
	os.Unsetenv("MODBUS_TIMEOUT")
	os.Unsetenv("MODBUS_REQUEST_TIMEOUT")
	os.Unsetenv("MODBUS_SHUTDOWN_TIMEOUT")
	os.Unsetenv("HEALTH_LISTEN")
	os.Unsetenv("LOG_LEVEL")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Listen != ":5502" {
		t.Errorf("expected :5502, got %s", cfg.Listen)
	}
	if cfg.Upstream != "192.168.1.100:502" {
		t.Errorf("expected 192.168.1.100:502, got %s", cfg.Upstream)
	}
	if cfg.DefaultSlaveID != 1 {
		t.Errorf("expected slave ID 1, got %d", cfg.DefaultSlaveID)
	}
	if cfg.CacheTTL != 10*time.Second {
		t.Errorf("expected 10s TTL, got %v", cfg.CacheTTL)
	}
	if cfg.CacheServeStale != false {
		t.Error("expected serve stale false")
	}
	if cfg.ReadOnly != ReadOnlyOn {
		t.Errorf("expected readonly true, got %s", cfg.ReadOnly)
	}
	if cfg.AttemptTimeout != 10*time.Second {
		t.Errorf("expected 10s attempt timeout, got %v", cfg.AttemptTimeout)
	}
	if cfg.RequestTimeout != 30*time.Second {
		t.Errorf("expected 30s request timeout, got %v", cfg.RequestTimeout)
	}
	if cfg.RequestDelay != 0 {
		t.Errorf("expected 0 request delay, got %v", cfg.RequestDelay)
	}
	if cfg.ConnectDelay != 0 {
		t.Errorf("expected 0 connect delay, got %v", cfg.ConnectDelay)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("expected 30s shutdown timeout, got %v", cfg.ShutdownTimeout)
	}
	if cfg.HealthListen != "" {
		t.Errorf("expected empty health listen, got %s", cfg.HealthListen)
	}
	if cfg.LogLevel != "INFO" {
		t.Errorf("expected INFO log level, got %s", cfg.LogLevel)
	}
}

func TestLoad_MissingUpstream(t *testing.T) {
	os.Unsetenv("MODBUS_UPSTREAM")

	_, err := Load()
	if err == nil {
		t.Error("expected error for missing MODBUS_UPSTREAM")
	}
}

func TestLoad_CustomValues(t *testing.T) {
	os.Setenv("MODBUS_UPSTREAM", "10.0.0.1:502")
	os.Setenv("MODBUS_LISTEN", ":502")
	os.Setenv("MODBUS_SLAVE_ID", "5")
	os.Setenv("MODBUS_CACHE_TTL", "30s")
	os.Setenv("MODBUS_CACHE_SERVE_STALE", "true")
	os.Setenv("MODBUS_READONLY", "false")
	os.Setenv("MODBUS_ATTEMPT_TIMEOUT", "5s")
	os.Unsetenv("MODBUS_TIMEOUT")
	os.Setenv("MODBUS_REQUEST_TIMEOUT", "9s")
	os.Setenv("MODBUS_REQUEST_DELAY", "100ms")
	os.Setenv("MODBUS_CONNECT_DELAY", "200ms")
	os.Setenv("MODBUS_SHUTDOWN_TIMEOUT", "60s")
	os.Setenv("LOG_LEVEL", "DEBUG")

	defer func() {
		os.Unsetenv("MODBUS_UPSTREAM")
		os.Unsetenv("MODBUS_LISTEN")
		os.Unsetenv("MODBUS_SLAVE_ID")
		os.Unsetenv("MODBUS_CACHE_TTL")
		os.Unsetenv("MODBUS_CACHE_SERVE_STALE")
		os.Unsetenv("MODBUS_READONLY")
		os.Unsetenv("MODBUS_ATTEMPT_TIMEOUT")
		os.Unsetenv("MODBUS_TIMEOUT")
		os.Unsetenv("MODBUS_REQUEST_TIMEOUT")
		os.Unsetenv("MODBUS_REQUEST_DELAY")
		os.Unsetenv("MODBUS_CONNECT_DELAY")
		os.Unsetenv("MODBUS_SHUTDOWN_TIMEOUT")
		os.Unsetenv("LOG_LEVEL")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Listen != ":502" {
		t.Errorf("expected :502, got %s", cfg.Listen)
	}
	if cfg.DefaultSlaveID != 5 {
		t.Errorf("expected slave ID 5, got %d", cfg.DefaultSlaveID)
	}
	if cfg.CacheTTL != 30*time.Second {
		t.Errorf("expected 30s TTL, got %v", cfg.CacheTTL)
	}
	if cfg.CacheServeStale != true {
		t.Error("expected serve stale true")
	}
	if cfg.ReadOnly != ReadOnlyOff {
		t.Errorf("expected readonly false, got %s", cfg.ReadOnly)
	}
	if cfg.AttemptTimeout != 5*time.Second {
		t.Errorf("expected 5s attempt timeout, got %v", cfg.AttemptTimeout)
	}
	if cfg.RequestTimeout != 9*time.Second {
		t.Errorf("expected 9s request timeout, got %v", cfg.RequestTimeout)
	}
	if cfg.RequestDelay != 100*time.Millisecond {
		t.Errorf("expected 100ms request delay, got %v", cfg.RequestDelay)
	}
	if cfg.ConnectDelay != 200*time.Millisecond {
		t.Errorf("expected 200ms connect delay, got %v", cfg.ConnectDelay)
	}
	if cfg.ShutdownTimeout != 60*time.Second {
		t.Errorf("expected 60s shutdown timeout, got %v", cfg.ShutdownTimeout)
	}
	if cfg.LogLevel != "DEBUG" {
		t.Errorf("expected DEBUG log level, got %s", cfg.LogLevel)
	}
}

func TestLoad_ReadOnlyModes(t *testing.T) {
	os.Setenv("MODBUS_UPSTREAM", "localhost:502")
	defer os.Unsetenv("MODBUS_UPSTREAM")

	tests := []struct {
		value    string
		expected ReadOnlyMode
	}{
		{"false", ReadOnlyOff},
		{"true", ReadOnlyOn},
		{"deny", ReadOnlyDeny},
		{"FALSE", ReadOnlyOff},
		{"TRUE", ReadOnlyOn},
		{"DENY", ReadOnlyDeny},
	}

	for _, tt := range tests {
		os.Setenv("MODBUS_READONLY", tt.value)
		cfg, err := Load()
		if err != nil {
			t.Errorf("value %s: unexpected error: %v", tt.value, err)
			continue
		}
		if cfg.ReadOnly != tt.expected {
			t.Errorf("value %s: expected %s, got %s", tt.value, tt.expected, cfg.ReadOnly)
		}
	}
	os.Unsetenv("MODBUS_READONLY")
}

func TestLoad_InvalidReadOnly(t *testing.T) {
	os.Setenv("MODBUS_UPSTREAM", "localhost:502")
	os.Setenv("MODBUS_READONLY", "invalid")
	defer func() {
		os.Unsetenv("MODBUS_UPSTREAM")
		os.Unsetenv("MODBUS_READONLY")
	}()

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid MODBUS_READONLY")
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	os.Setenv("MODBUS_UPSTREAM", "localhost:502")
	defer os.Unsetenv("MODBUS_UPSTREAM")

	tests := []string{"MODBUS_CACHE_TTL", "MODBUS_ATTEMPT_TIMEOUT", "MODBUS_TIMEOUT", "MODBUS_REQUEST_TIMEOUT", "MODBUS_REQUEST_DELAY", "MODBUS_CONNECT_DELAY", "MODBUS_SHUTDOWN_TIMEOUT"}
	for _, envVar := range tests {
		os.Unsetenv("MODBUS_ATTEMPT_TIMEOUT")
		os.Unsetenv("MODBUS_TIMEOUT")
		os.Setenv(envVar, "invalid")
		_, err := Load()
		if err == nil {
			t.Errorf("expected error for invalid %s", envVar)
		}
		os.Unsetenv(envVar)
	}
}

func TestLoad_NonPositiveTimeouts(t *testing.T) {
	t.Setenv("MODBUS_UPSTREAM", "localhost:502")
	for _, envVar := range []string{"MODBUS_ATTEMPT_TIMEOUT", "MODBUS_TIMEOUT", "MODBUS_REQUEST_TIMEOUT"} {
		t.Run(envVar, func(t *testing.T) {
			t.Setenv("MODBUS_ATTEMPT_TIMEOUT", "")
			t.Setenv("MODBUS_TIMEOUT", "")
			t.Setenv(envVar, "0")
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for zero %s", envVar)
			}
		})
	}
}

func TestLoad_AttemptTimeoutMigration(t *testing.T) {
	tests := []struct {
		name        string
		preferred   string
		legacy      string
		want        time.Duration
		errContains string
	}{
		{name: "preferred only", preferred: "4s", want: 4 * time.Second},
		{name: "legacy only", legacy: "6s", want: 6 * time.Second},
		{name: "both equal after parsing", preferred: "5s", legacy: "5000ms", want: 5 * time.Second},
		{name: "both conflict", preferred: "5s", legacy: "6s", errContains: "must match when both are set"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MODBUS_UPSTREAM", "localhost:502")
			t.Setenv("MODBUS_ATTEMPT_TIMEOUT", tt.preferred)
			t.Setenv("MODBUS_TIMEOUT", tt.legacy)
			t.Setenv("MODBUS_REQUEST_TIMEOUT", "17s")

			cfg, err := Load()
			if tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("expected error containing %q, got %v", tt.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.AttemptTimeout != tt.want {
				t.Fatalf("expected attempt timeout %v, got %v", tt.want, cfg.AttemptTimeout)
			}
			if cfg.RequestTimeout != 17*time.Second {
				t.Fatalf("attempt timeout changed request timeout to %v", cfg.RequestTimeout)
			}
		})
	}
}

func TestLoad_HealthListenCustom(t *testing.T) {
	// Ensure optional env vars that Load() may read do not inherit
	// potentially invalid values from the surrounding environment.
	t.Setenv("MODBUS_LISTEN", "")
	t.Setenv("MODBUS_READONLY", "")
	t.Setenv("MODBUS_CACHE_TTL", "")
	t.Setenv("MODBUS_ATTEMPT_TIMEOUT", "")
	t.Setenv("MODBUS_TIMEOUT", "")
	t.Setenv("MODBUS_REQUEST_TIMEOUT", "")
	t.Setenv("MODBUS_REQUEST_DELAY", "")
	t.Setenv("MODBUS_CONNECT_DELAY", "")
	t.Setenv("MODBUS_SHUTDOWN_TIMEOUT", "")

	// Set required and explicitly tested env vars.
	t.Setenv("MODBUS_UPSTREAM", "localhost:502")
	t.Setenv("HEALTH_LISTEN", ":9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HealthListen != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.HealthListen)
	}
}
