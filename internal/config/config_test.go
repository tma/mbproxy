package config

import (
	"os"
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
	os.Unsetenv("MODBUS_TIMEOUT")
	os.Unsetenv("MODBUS_SHUTDOWN_TIMEOUT")
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
	if cfg.Timeout != 10*time.Second {
		t.Errorf("expected 10s timeout, got %v", cfg.Timeout)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("expected 30s shutdown timeout, got %v", cfg.ShutdownTimeout)
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
	os.Setenv("MODBUS_TIMEOUT", "5s")
	os.Setenv("MODBUS_SHUTDOWN_TIMEOUT", "60s")
	os.Setenv("LOG_LEVEL", "DEBUG")

	defer func() {
		os.Unsetenv("MODBUS_UPSTREAM")
		os.Unsetenv("MODBUS_LISTEN")
		os.Unsetenv("MODBUS_SLAVE_ID")
		os.Unsetenv("MODBUS_CACHE_TTL")
		os.Unsetenv("MODBUS_CACHE_SERVE_STALE")
		os.Unsetenv("MODBUS_READONLY")
		os.Unsetenv("MODBUS_TIMEOUT")
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
	if cfg.Timeout != 5*time.Second {
		t.Errorf("expected 5s timeout, got %v", cfg.Timeout)
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

	tests := []string{"MODBUS_CACHE_TTL", "MODBUS_TIMEOUT", "MODBUS_SHUTDOWN_TIMEOUT"}
	for _, envVar := range tests {
		os.Setenv(envVar, "invalid")
		_, err := Load()
		if err == nil {
			t.Errorf("expected error for invalid %s", envVar)
		}
		os.Unsetenv(envVar)
	}
}
