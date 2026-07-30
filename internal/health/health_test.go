package health

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"
)

// mockChecker implements Checker for testing.
type mockChecker struct {
	err error
}

type diagnosticChecker struct {
	mockChecker
	status  string
	details map[string]any
}

type atomicDiagnosticChecker struct {
	healthyCalls int
	statusCalls  int
	reportCalls  int
}

func (m *mockChecker) Healthy() error {
	return m.err
}

func (c *diagnosticChecker) HealthStatus() (string, map[string]any) {
	return c.status, c.details
}

func (c *atomicDiagnosticChecker) Healthy() error {
	c.healthyCalls++
	return fmt.Errorf("separate health snapshot used")
}

func (c *atomicDiagnosticChecker) HealthStatus() (string, map[string]any) {
	c.statusCalls++
	return "unhealthy", nil
}

func (c *atomicDiagnosticChecker) HealthReport() (string, map[string]any, error) {
	c.reportCalls++
	return "degraded", map[string]any{"total_retries": uint64(2)}, nil
}

func TestHandleHealth_OK(t *testing.T) {
	checker := &mockChecker{err: nil}
	srv := NewServer(":0", checker, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status ok, got %s", resp.Status)
	}
	if resp.Error != "" {
		t.Errorf("expected no error, got %s", resp.Error)
	}
}

func TestHandleHealth_DegradedDiagnosticsRemainAvailable(t *testing.T) {
	checker := &diagnosticChecker{
		status: "degraded",
		details: map[string]any{
			"total_retries":         uint64(2),
			"recovered_retries":     uint64(2),
			"sustained_degradation": true,
		},
	}
	srv := NewServer(":0", checker, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("degraded health returned status %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "degraded" ||
		resp.Details["total_retries"] != float64(2) ||
		resp.Details["sustained_degradation"] != true {
		t.Fatalf("unexpected degraded response: %+v", resp)
	}
}

func TestHandleHealth_PrefersAtomicReport(t *testing.T) {
	checker := &atomicDiagnosticChecker{}
	srv := NewServer(":0", checker, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("atomic health report returned status %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "degraded" || resp.Details["total_retries"] != float64(2) {
		t.Fatalf("unexpected atomic report: %+v", resp)
	}
	if checker.reportCalls != 1 || checker.healthyCalls != 0 || checker.statusCalls != 0 {
		t.Fatalf("health endpoint mixed snapshots: %+v", checker)
	}
}

func TestHandleHealth_UnhealthyIncludesDiagnostics(t *testing.T) {
	checker := &diagnosticChecker{
		mockChecker: mockChecker{err: fmt.Errorf("upstream unavailable")},
		status:      "degraded",
		details: map[string]any{
			"consecutive_final_failures": float64(3),
		},
	}
	srv := NewServer(":0", checker, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy status = %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "unhealthy" ||
		resp.Details["consecutive_final_failures"] != float64(3) {
		t.Fatalf("unhealthy response omitted diagnostics: %+v", resp)
	}
}

func TestHandleHealth_Unhealthy(t *testing.T) {
	checker := &mockChecker{err: fmt.Errorf("upstream unreachable")}
	srv := NewServer(":0", checker, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "unhealthy" {
		t.Errorf("expected status unhealthy, got %s", resp.Status)
	}
	if resp.Error != "upstream unreachable" {
		t.Errorf("expected 'upstream unreachable', got %s", resp.Error)
	}
}

func TestListenAndServe(t *testing.T) {
	checker := &mockChecker{err: nil}
	srv := NewServer("127.0.0.1:0", checker, slog.Default())

	// Use a listener on port 0 to get a random available port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv.httpServer.Addr = addr

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	// Wait for server to be ready by polling
	var resp *http.Response
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		resp, err = http.Get(fmt.Sprintf("http://%s/health", addr))
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("server never became ready: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	srv.Shutdown(t.Context())

	if err := <-errCh; err != nil {
		t.Errorf("unexpected server error: %v", err)
	}
}

func TestCheckHealth_Success(t *testing.T) {
	// Start a test server that returns 200
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Response{Status: "ok"})
	}))
	defer ts.Close()

	// Extract host:port from test server URL
	addr := ts.Listener.Addr().String()
	if err := CheckHealth(addr); err != nil {
		t.Errorf("expected healthy, got error: %v", err)
	}
}

func TestCheckHealth_Failure(t *testing.T) {
	// Start a test server that returns 503
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	if err := CheckHealth(addr); err == nil {
		t.Error("expected error for unhealthy response")
	}
}

func TestCheckHealth_ConnectionRefused(t *testing.T) {
	// Use a port that nothing is listening on
	if err := CheckHealth("127.0.0.1:1"); err == nil {
		t.Error("expected error for connection refused")
	}
}

func TestCheckHealth_WildcardAddresses(t *testing.T) {
	// Start a test server bound to localhost so wildcard addresses can reach it.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Response{Status: "ok"})
	}))
	defer ts.Close()

	_, port, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to parse test server address: %v", err)
	}

	// Each of these listen-style addresses should be normalized to localhost.
	wildcards := []string{
		":" + port,        // empty host (":8080")
		"0.0.0.0:" + port, // IPv4 wildcard
		"[::]:" + port,    // IPv6 wildcard
	}
	for _, addr := range wildcards {
		if err := CheckHealth(addr); err != nil {
			t.Errorf("CheckHealth(%q) expected success, got: %v", addr, err)
		}
	}
}

func TestCheckHealth_IPv6Loopback(t *testing.T) {
	// Start a test server bound to the IPv6 loopback address.
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback not available:", err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Response{Status: "ok"})
	}))
	ts.Listener = ln
	ts.Start()
	defer ts.Close()

	addr := ts.Listener.Addr().String() // "[::1]:PORT"
	if err := CheckHealth(addr); err != nil {
		t.Errorf("CheckHealth(%q) expected success for IPv6 loopback, got: %v", addr, err)
	}
}
