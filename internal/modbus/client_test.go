package modbus

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridmodbus "github.com/grid-x/modbus"
)

type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

type fakeResult struct {
	data     []byte
	err      error
	started  chan struct{}
	release  chan struct{}
	complete func()
}

type fakeRequestClient struct {
	mu      sync.Mutex
	results []fakeResult
	calls   int
}

func (c *fakeRequestClient) next(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	result := c.results[c.calls]
	c.calls++
	c.mu.Unlock()
	if result.started != nil {
		close(result.started)
	}
	if result.release != nil {
		select {
		case <-result.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if result.complete != nil {
		result.complete()
	}
	return result.data, result.err
}

func (c *fakeRequestClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *fakeRequestClient) ReadCoils(ctx context.Context, _, _ uint16) ([]byte, error) {
	return c.next(ctx)
}
func (c *fakeRequestClient) ReadDiscreteInputs(ctx context.Context, _, _ uint16) ([]byte, error) {
	return c.next(ctx)
}
func (c *fakeRequestClient) ReadHoldingRegisters(ctx context.Context, _, _ uint16) ([]byte, error) {
	return c.next(ctx)
}
func (c *fakeRequestClient) ReadInputRegisters(ctx context.Context, _, _ uint16) ([]byte, error) {
	return c.next(ctx)
}
func (c *fakeRequestClient) WriteSingleCoil(ctx context.Context, _, _ uint16) ([]byte, error) {
	return c.next(ctx)
}
func (c *fakeRequestClient) WriteSingleRegister(ctx context.Context, _, _ uint16) ([]byte, error) {
	return c.next(ctx)
}
func (c *fakeRequestClient) WriteMultipleCoils(ctx context.Context, _, _ uint16, _ []byte) ([]byte, error) {
	return c.next(ctx)
}
func (c *fakeRequestClient) WriteMultipleRegisters(ctx context.Context, _, _ uint16, _ []byte) ([]byte, error) {
	return c.next(ctx)
}

type fakeSession struct {
	client      *fakeRequestClient
	connectErr  error
	connectHook func(context.Context)
	beginHook   func(context.Context)
	finishErr   error
	connects    *atomic.Int32
	closes      *atomic.Int32
}

func (c *fakeSession) Connect(ctx context.Context) error {
	c.connects.Add(1)
	if c.connectHook != nil {
		c.connectHook(ctx)
	}
	return c.connectErr
}
func (c *fakeSession) Close() error {
	c.closes.Add(1)
	return nil
}
func (c *fakeSession) SetSlave(byte) {}
func (c *fakeSession) SendRaw(ctx context.Context, _ []byte) ([]byte, error) {
	if c.client == nil {
		return nil, errors.New("unsupported function code")
	}
	return c.client.next(ctx)
}
func (c *fakeSession) BeginRequest(ctx context.Context, _ time.Duration) func() error {
	if c.beginHook != nil {
		c.beginHook(ctx)
	}
	return func() error { return c.finishErr }
}

type fakeSessionSet struct {
	client      *fakeRequestClient
	connectErrs []error
	connectHook func(context.Context)
	sessions    atomic.Int32
	connects    atomic.Int32
	closes      atomic.Int32
	finishErr   error
}

func (s *fakeSessionSet) factory() (clientSession, requestClient) {
	index := int(s.sessions.Add(1)) - 1
	var err error
	if index < len(s.connectErrs) {
		err = s.connectErrs[index]
	}
	return &fakeSession{
		client:      s.client,
		connectErr:  err,
		connectHook: s.connectHook,
		finishErr:   s.finishErr,
		connects:    &s.connects,
		closes:      &s.closes,
	}, s.client
}

func newFakeClient(results []fakeResult) (*Client, *fakeSessionSet) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewClient("upstream:502", time.Second, 0, 0, logger)
	sessions := &fakeSessionSet{client: &fakeRequestClient{results: results}}
	client.newSession = sessions.factory
	return client, sessions
}

func readRequest() *Request {
	return &Request{SlaveID: 1, FunctionCode: FuncReadHoldingRegisters, Address: 32000, Quantity: 1}
}

func vendorRequest() *Request {
	return &Request{
		SlaveID:      1,
		FunctionCode: 0x41,
		PDU:          []byte{0x41, 0x00, 0x01, 0x02},
	}
}

func writeRequest() *Request {
	return &Request{
		SlaveID:      1,
		FunctionCode: FuncWriteSingleRegister,
		Address:      10,
		Quantity:     1,
		Data:         []byte{0, 1},
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func newDiagnosticLogger() (*bytes.Buffer, *slog.Logger) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &output, logger
}

func findLogEntry(t *testing.T, output *bytes.Buffer, message string) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log entry: %v", err)
		}
		if entry["msg"] == message {
			return entry
		}
	}
	t.Fatalf("log message %q not found in %s", message, output.String())
	return nil
}

func requireLifecycleFields(t *testing.T, entry map[string]any) {
	t.Helper()
	for _, field := range []string{
		"slave_id", "func", "addr", "qty", "write", "attempt", "attempts",
		"queue_duration", "attempt_duration", "reconnect_duration",
		"total_duration", "error_kind", "will_retry",
	} {
		if _, ok := entry[field]; !ok {
			t.Errorf("log entry missing %q: %+v", field, entry)
		}
	}
}

func requireDurationField(t *testing.T, entry map[string]any, field string, want time.Duration) {
	t.Helper()
	value, ok := entry[field].(float64)
	if !ok {
		t.Fatalf("%s has type %T, want JSON number", field, entry[field])
	}
	if got := time.Duration(value); got != want {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
}

func TestClient_ProtocolExceptionIsNotRetried(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{{err: &gridmodbus.Error{
		FunctionCode:  FuncReadHoldingRegisters | 0x80,
		ExceptionCode: ExcIllegalAddress,
	}}})

	_, err := client.Execute(t.Context(), readRequest())
	if err == nil {
		t.Fatal("expected protocol exception")
	}
	if sessions.client.callCount() != 1 || sessions.sessions.Load() != 1 {
		t.Fatalf("protocol exception retried: calls=%d sessions=%d", sessions.client.callCount(), sessions.sessions.Load())
	}
	if ErrorKindOf(err) != ErrorProtocolException {
		t.Fatalf("expected protocol exception, got %s", ErrorKindOf(err))
	}
	if code := DownstreamException(err); code != ExcIllegalAddress {
		t.Fatalf("expected exception 0x%02X, got 0x%02X", ExcIllegalAddress, code)
	}
}

func TestClient_ReadTimeoutRetriesAfterReconnect(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{data: []byte{0x12, 0x34}},
		{data: []byte{0x56, 0x78}},
	})

	resp, err := client.Execute(t.Context(), readRequest())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sessions.client.callCount() != 2 || sessions.sessions.Load() != 2 {
		t.Fatalf("expected one retry and reconnect: calls=%d sessions=%d", sessions.client.callCount(), sessions.sessions.Load())
	}
	if len(resp) != 4 || resp[2] != 0x12 || resp[3] != 0x34 {
		t.Fatalf("unexpected response: % x", resp)
	}
	if err := client.Healthy(); err != nil {
		t.Fatalf("recovered retry should remain available: %v", err)
	}

	if _, err := client.Execute(t.Context(), readRequest()); err != nil {
		t.Fatalf("first-attempt recovery: %v", err)
	}
}

func TestClient_SecondReadTimeoutReturnsGatewayFailure(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{err: fakeTimeoutError{}},
	})

	_, err := client.Execute(t.Context(), readRequest())
	if err == nil {
		t.Fatal("expected timeout")
	}
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected RequestError, got %T", err)
	}
	if reqErr.Kind != ErrorTransportTimeout || reqErr.Attempts != 2 {
		t.Fatalf("unexpected request error: %+v", reqErr)
	}
	if DownstreamException(err) != ExcGatewayTargetFailed {
		t.Fatalf("expected gateway target failure, got 0x%02X", DownstreamException(err))
	}

	if sessions.client.callCount() != 2 {
		t.Fatalf("expected two attempts, got %d", sessions.client.callCount())
	}
}

func TestClient_WriteTimeoutIsNotRetried(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{data: []byte{0x00, 0x01}},
	})
	write := &Request{
		SlaveID:      1,
		FunctionCode: FuncWriteSingleRegister,
		Address:      10,
		Quantity:     1,
		Data:         []byte{0x00, 0x01},
	}

	if _, err := client.Execute(t.Context(), write); err == nil {
		t.Fatal("expected write timeout")
	}
	if sessions.client.callCount() != 1 || sessions.sessions.Load() != 1 {
		t.Fatalf("write was retried: calls=%d sessions=%d", sessions.client.callCount(), sessions.sessions.Load())
	}

	if _, err := client.Execute(t.Context(), readRequest()); err != nil {
		t.Fatalf("next request did not reconnect: %v", err)
	}
	if sessions.sessions.Load() != 2 {
		t.Fatalf("expected reconnect for next request, got %d sessions", sessions.sessions.Load())
	}
}

func TestClient_RetryDiagnosticsSeparateTiming(t *testing.T) {
	clock := &testClock{
		now: time.Unix(1000, 0),
	}
	queueStarted := make(chan struct{})
	output, logger := newDiagnosticLogger()
	client, sessions := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}, complete: func() { clock.Advance(3 * time.Second) }},
		{data: []byte{0x12, 0x34}, complete: func() { clock.Advance(3 * time.Second) }},
	})
	client.logger = logger
	client.now = clock.Now
	client.beforeAcquire = func() { close(queueStarted) }
	sessions.connectHook = func(context.Context) { clock.Advance(2 * time.Second) }

	<-client.owner
	result := make(chan error, 1)
	go func() {
		_, err := client.Execute(context.Background(), readRequest())
		result <- err
	}()
	<-queueStarted
	clock.Advance(time.Second)
	client.release()
	if err := <-result; err != nil {
		t.Fatalf("execute: %v", err)
	}

	firstFailure := findLogEntry(t, output, "upstream request attempt failed")
	requireLifecycleFields(t, firstFailure)
	if firstFailure["level"] != "DEBUG" ||
		firstFailure["attempts"] != float64(1) ||
		firstFailure["error_kind"] != string(ErrorTransportTimeout) ||
		firstFailure["will_retry"] != true {
		t.Fatalf("unexpected first failure log: %+v", firstFailure)
	}
	requireDurationField(t, firstFailure, "queue_duration", time.Second)
	requireDurationField(t, firstFailure, "attempt_duration", 3*time.Second)
	requireDurationField(t, firstFailure, "reconnect_duration", 2*time.Second)
	requireDurationField(t, firstFailure, "total_duration", 6*time.Second)

	recovered := findLogEntry(t, output, "upstream request completed")
	requireLifecycleFields(t, recovered)
	if recovered["level"] != "DEBUG" ||
		recovered["attempts"] != float64(2) ||
		recovered["error_kind"] != "" ||
		recovered["will_retry"] != false {
		t.Fatalf("unexpected recovered retry log: %+v", recovered)
	}
	requireDurationField(t, recovered, "queue_duration", time.Second)
	requireDurationField(t, recovered, "attempt_duration", 6*time.Second)
	requireDurationField(t, recovered, "reconnect_duration", 4*time.Second)
	requireDurationField(t, recovered, "total_duration", 11*time.Second)
}

func TestClient_FinalFailureLogHasCompleteClassification(t *testing.T) {
	output, logger := newDiagnosticLogger()
	client, _ := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{err: fakeTimeoutError{}},
	})
	client.logger = logger

	_, err := client.Execute(t.Context(), readRequest())
	if err == nil {
		t.Fatal("expected final failure")
	}
	entry := findLogEntry(t, output, "upstream request failed")
	requireLifecycleFields(t, entry)
	if entry["level"] != "DEBUG" ||
		entry["attempts"] != float64(2) ||
		entry["error_kind"] != string(ErrorTransportTimeout) ||
		entry["will_retry"] != false ||
		entry["downstream_exception"] != "0x0B" {
		t.Fatalf("unexpected final failure log: %+v", entry)
	}
}

func TestClient_ProtocolExceptionLogPreservesCode(t *testing.T) {
	output, logger := newDiagnosticLogger()
	client, sessions := newFakeClient([]fakeResult{{err: &gridmodbus.Error{
		FunctionCode:  FuncReadHoldingRegisters | 0x80,
		ExceptionCode: ExcIllegalAddress,
	}}})
	client.logger = logger

	_, err := client.Execute(t.Context(), readRequest())
	if ErrorKindOf(err) != ErrorProtocolException {
		t.Fatalf("expected protocol exception, got %v", err)
	}
	entry := findLogEntry(t, output, "upstream request failed")
	requireLifecycleFields(t, entry)
	if entry["level"] != "DEBUG" ||
		entry["attempts"] != float64(1) ||
		entry["exception_code"] != "0x02" ||
		entry["downstream_exception"] != "0x02" ||
		entry["will_retry"] != false {
		t.Fatalf("unexpected protocol exception log: %+v", entry)
	}
	if sessions.client.callCount() != 1 || sessions.sessions.Load() != 1 {
		t.Fatalf("protocol exception reconnected or retried: calls=%d sessions=%d", sessions.client.callCount(), sessions.sessions.Load())
	}
}

func TestClient_WriteDiagnosticsNeverLogPayload(t *testing.T) {
	const payload = "sensitivepayload1234"
	output, logger := newDiagnosticLogger()
	client, _ := newFakeClient([]fakeResult{{err: errors.New(payload)}})
	client.logger = logger
	req := &Request{
		SlaveID:      7,
		FunctionCode: FuncWriteMultipleRegs,
		Address:      42,
		Quantity:     10,
		Data:         []byte(payload),
	}

	if _, err := client.Execute(t.Context(), req); err == nil {
		t.Fatal("expected write failure")
	}
	if bytes.Contains(output.Bytes(), []byte(payload)) {
		t.Fatalf("write payload leaked into logs: %s", output.String())
	}
	entry := findLogEntry(t, output, "upstream request failed")
	requireLifecycleFields(t, entry)
	if entry["write"] != true || entry["slave_id"] != float64(7) || entry["addr"] != float64(42) {
		t.Fatalf("write identity missing from diagnostics: %+v", entry)
	}
}

func TestClient_HealthDiagnosticsRetainAndClearRecoveredRetry(t *testing.T) {
	clock := &testClock{now: time.Unix(1000, 0)}
	client, _ := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{data: []byte{0x12, 0x34}},
		{data: []byte{0x56, 0x78}},
	})
	client.now = clock.Now

	if _, err := client.Execute(t.Context(), readRequest()); err != nil {
		t.Fatalf("recovered retry: %v", err)
	}
	stats := client.HealthStats()
	if stats.TotalRetries != 1 ||
		stats.RecoveredRetries != 1 ||
		stats.ConsecutiveFirstAttemptFailure != 1 ||
		stats.ConsecutiveFinalFailure != 0 ||
		!stats.Degraded ||
		stats.SustainedDegradation {
		t.Fatalf("unexpected recovered retry diagnostics: %+v", stats)
	}
	if err := client.Healthy(); err != nil {
		t.Fatalf("isolated recovered retry failed health: %v", err)
	}
	status, details := client.HealthStatus()
	if status != "ok" || details["degraded"] != true || details["sustained_degradation"] != false {
		t.Fatalf("isolated retry status=%q details=%+v", status, details)
	}

	clock.Advance(degradedHealthWindow)
	status, details = client.HealthStatus()
	if status != "degraded" || details["sustained_degradation"] != true {
		t.Fatalf("sustained degradation status=%q details=%+v", status, details)
	}
	if err := client.Healthy(); err != nil {
		t.Fatalf("sustained recovered degradation should remain available: %v", err)
	}

	clock.Advance(time.Second)
	if _, err := client.Execute(t.Context(), readRequest()); err != nil {
		t.Fatalf("first-attempt success: %v", err)
	}
	stats = client.HealthStats()
	if stats.Degraded ||
		stats.SustainedDegradation ||
		stats.ConsecutiveFirstAttemptFailure != 0 ||
		stats.TotalRetries != 1 ||
		stats.RecoveredRetries != 1 ||
		stats.LastFirstAttemptSuccess.IsZero() ||
		stats.LastSuccessfulRequest.IsZero() {
		t.Fatalf("first-attempt success did not clear degradation: %+v", stats)
	}
}

func TestClient_DiagnosticCountersTrackConsecutiveFinalFailures(t *testing.T) {
	client, _ := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{err: fakeTimeoutError{}},
		{err: fakeTimeoutError{}},
		{err: fakeTimeoutError{}},
		{data: []byte{0x12, 0x34}},
	})

	for range 2 {
		if _, err := client.Execute(t.Context(), readRequest()); err == nil {
			t.Fatal("expected final failure")
		}
	}
	stats := client.HealthStats()
	if stats.TotalRetries != 2 ||
		stats.RecoveredRetries != 0 ||
		stats.ConsecutiveFirstAttemptFailure != 2 ||
		stats.ConsecutiveFinalFailure != 2 {
		t.Fatalf("unexpected failure counters: %+v", stats)
	}

	if _, err := client.Execute(t.Context(), readRequest()); err != nil {
		t.Fatalf("first-attempt recovery: %v", err)
	}
	stats = client.HealthStats()
	if stats.ConsecutiveFirstAttemptFailure != 0 || stats.ConsecutiveFinalFailure != 0 {
		t.Fatalf("success did not clear consecutive counters: %+v", stats)
	}
}

func TestClient_ExpiredQueueRequestNeverExecutes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client, sessions := newFakeClient([]fakeResult{
		{data: []byte{0x00, 0x01}, started: started, release: release},
	})

	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Execute(context.Background(), readRequest())
		firstDone <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Execute(ctx, readRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected queue deadline, got %v", err)
	}
	if sessions.client.callCount() != 1 {
		t.Fatalf("expired queued request executed: %d calls", sessions.client.callCount())
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first request: %v", err)
	}
	if sessions.client.callCount() != 1 {
		t.Fatalf("expired request executed after ownership release: %d calls", sessions.client.callCount())
	}
}

func TestClient_ContextDeadlineCapsAttempt(t *testing.T) {
	started := make(chan struct{})
	client, _ := newFakeClient([]fakeResult{
		{started: started, release: make(chan struct{})},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := client.Execute(ctx, readRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected request deadline, got %v", err)
	}
	if ErrorKindOf(err) != ErrorContextDeadline {
		t.Fatalf("expected context deadline classification, got %s", ErrorKindOf(err))
	}
	if stats := client.HealthStats(); stats.ConsecutiveFinalFailure != 0 {
		t.Fatalf("request deadline counted as upstream final failure: %+v", stats)
	}
	if client.session != nil {
		t.Fatal("deadline-failed connection was retained")
	}
}

func TestClient_CancellationAfterConnectPreventsAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requests := &fakeRequestClient{results: []fakeResult{{data: []byte{0x12, 0x34}}}}
	var connects atomic.Int32
	var closes atomic.Int32
	client := NewClient("upstream:502", time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.newSession = func() (clientSession, requestClient) {
		return &fakeSession{
			connectHook: func(context.Context) { cancel() },
			connects:    &connects,
			closes:      &closes,
		}, requests
	}

	_, err := client.Execute(ctx, readRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after connect, got %v", err)
	}
	if requests.callCount() != 0 {
		t.Fatalf("expired request executed %d wire attempts", requests.callCount())
	}
}

func TestClient_CancellationDuringBeginRequestPreventsAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requests := &fakeRequestClient{results: []fakeResult{{data: []byte{0x12, 0x34}}}}
	var connects atomic.Int32
	var closes atomic.Int32
	client := NewClient("upstream:502", time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.newSession = func() (clientSession, requestClient) {
		return &fakeSession{
			beginHook: func(context.Context) { cancel() },
			connects:  &connects,
			closes:    &closes,
		}, requests
	}

	_, err := client.Execute(ctx, readRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation before wire attempt, got %v", err)
	}
	if requests.callCount() != 0 {
		t.Fatalf("expired request executed %d wire attempts", requests.callCount())
	}
}

func TestClient_SuccessRacingCancellationReturnsCancellationAndKeepsConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client, _ := newFakeClient([]fakeResult{{
		data:     []byte{0x12, 0x34},
		complete: cancel,
	}})

	_, err := client.Execute(ctx, readRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after response, got %v", err)
	}
	if client.session == nil {
		t.Fatal("matching successful response unnecessarily closed connection")
	}
	if stats := client.HealthStats(); stats.ConsecutiveFinalFailure != 0 {
		t.Fatalf("post-response cancellation counted as upstream final failure: %+v", stats)
	}
}

func TestClient_SuccessReturnsBeforePacingDelay(t *testing.T) {
	tests := []struct {
		name   string
		req    *Request
		result []byte
	}{
		{name: "read", req: readRequest(), result: []byte{0x12, 0x34}},
		{name: "acknowledged write", req: writeRequest(), result: []byte{0, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, sessions := newFakeClient([]fakeResult{{data: tt.result}})
			client.requestDelay = time.Second
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			if _, err := client.Execute(ctx, tt.req); err != nil {
				t.Fatalf("successful request was delayed into failure: %v", err)
			}
			if sessions.client.callCount() != 1 {
				t.Fatalf("wire calls = %d, want 1", sessions.client.callCount())
			}
		})
	}
}

func TestClient_NextRequestPacingUsesNextRequestBudget(t *testing.T) {
	nextStarted := make(chan struct{})
	client, sessions := newFakeClient([]fakeResult{
		{data: []byte{0x12, 0x34}},
		{data: []byte{0x56, 0x78}, started: nextStarted},
	})
	client.requestDelay = 100 * time.Millisecond

	if _, err := client.Execute(t.Context(), readRequest()); err != nil {
		t.Fatalf("first request: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.Execute(ctx, readRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected next request to expire during pacing, got %v", err)
	}
	if sessions.client.callCount() != 1 {
		t.Fatalf("expired paced request made %d wire calls", sessions.client.callCount())
	}
	if stats := client.HealthStats(); stats.ConsecutiveFinalFailure != 0 {
		t.Fatalf("pacing deadline counted as upstream final failure: %+v", stats)
	}

	laterDone := make(chan error, 1)
	go func() {
		_, err := client.Execute(context.Background(), readRequest())
		laterDone <- err
	}()
	select {
	case <-nextStarted:
		t.Fatal("later request executed before pacing interval")
	case <-time.After(10 * time.Millisecond):
	}
	select {
	case err := <-laterDone:
		if err != nil {
			t.Fatalf("later request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("later request did not execute after pacing interval")
	}
	if sessions.client.callCount() != 2 {
		t.Fatalf("wire calls = %d, want 2", sessions.client.callCount())
	}
}

func TestClient_ReadRetryIsNotPaced(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{data: []byte{0x12, 0x34}},
	})
	client.requestDelay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := client.Execute(ctx, readRequest()); err != nil {
		t.Fatalf("retry was incorrectly paced: %v", err)
	}
	if sessions.client.callCount() != 2 {
		t.Fatalf("wire calls = %d, want 2", sessions.client.callCount())
	}
}

func TestClient_FinishRequestErrorDoesNotDiscardResponse(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{{data: []byte{0x12, 0x34}}})
	sessions.finishErr = errors.New("deadline cleanup failed")

	resp, err := client.Execute(t.Context(), readRequest())
	if err != nil {
		t.Fatalf("cleanup error discarded response: %v", err)
	}
	if len(resp) != 4 || resp[2] != 0x12 || resp[3] != 0x34 {
		t.Fatalf("unexpected response: % x", resp)
	}
}

func TestClient_FinishRequestErrorDoesNotReplaceRequestError(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{{err: fakeTimeoutError{}}})
	sessions.finishErr = errors.New("deadline cleanup failed")

	_, err := client.Execute(t.Context(), writeRequest())
	if ErrorKindOf(err) != ErrorTransportTimeout {
		t.Fatalf("cleanup error replaced request error: %v", err)
	}
}

func TestClient_ExpiredContextDoesNotAcquireAvailableOwnership(t *testing.T) {
	client, sessions := newFakeClient(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Execute(ctx, readRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if sessions.sessions.Load() != 0 || sessions.client.callCount() != 0 {
		t.Fatalf("expired request executed: sessions=%d calls=%d", sessions.sessions.Load(), sessions.client.callCount())
	}
}

func TestClient_CancellationWinningWithOwnershipReleaseRestoresToken(t *testing.T) {
	client, _ := newFakeClient(nil)
	for i := 0; i < 100; i++ {
		<-client.owner
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			result <- client.acquire(ctx)
		}()
		cancel()
		client.release()

		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d acquired expired ownership: %v", i, err)
		}
		select {
		case <-client.owner:
		default:
			t.Fatalf("iteration %d lost ownership token", i)
		}
		client.release()
	}
}

func TestClient_LocalValidationReturnsIllegalValueWithoutConnecting(t *testing.T) {
	client, sessions := newFakeClient(nil)
	req := readRequest()
	req.Quantity = 0

	_, err := client.Execute(t.Context(), req)
	if ErrorKindOf(err) != ErrorLocal {
		t.Fatalf("expected local error, got %s", ErrorKindOf(err))
	}
	if DownstreamException(err) != ExcIllegalValue {
		t.Fatalf("expected illegal value, got 0x%02X", DownstreamException(err))
	}
	if sessions.sessions.Load() != 0 {
		t.Fatalf("validation error connected upstream %d times", sessions.sessions.Load())
	}
}

func TestValidateRequest_AddressRangeBoundaries(t *testing.T) {
	tests := []struct {
		name string
		req  *Request
		code byte
	}{
		{
			name: "read final address accepted",
			req:  &Request{FunctionCode: FuncReadHoldingRegisters, Address: 65535, Quantity: 1},
		},
		{
			name: "read one past rejected",
			req:  &Request{FunctionCode: FuncReadHoldingRegisters, Address: 65535, Quantity: 2},
			code: ExcIllegalAddress,
		},
		{
			name: "write multiple final address accepted",
			req:  &Request{FunctionCode: FuncWriteMultipleRegs, Address: 65535, Quantity: 1, Data: []byte{0, 1}},
		},
		{
			name: "write multiple one past rejected",
			req:  &Request{FunctionCode: FuncWriteMultipleRegs, Address: 65535, Quantity: 2, Data: []byte{0, 1, 0, 2}},
			code: ExcIllegalAddress,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequest(tt.req)
			if tt.code == 0 {
				if err != nil {
					t.Fatalf("expected valid boundary request, got %v", err)
				}
				return
			}
			if DownstreamException(err) != tt.code {
				t.Fatalf("expected exception 0x%02X, got error %v", tt.code, err)
			}
		})
	}
}

func TestValidateRequest_VendorFunctionCode(t *testing.T) {
	tests := []struct {
		name string
		req  *Request
		code byte
	}{
		{
			name: "huawei login pdu accepted",
			req:  &Request{FunctionCode: 0x41, PDU: []byte{0x41, 0x00, 0x01, 0x02}},
		},
		{
			name: "missing pdu rejected",
			req:  &Request{FunctionCode: 0x41},
			code: ExcIllegalFunction,
		},
		{
			name: "mismatched pdu rejected",
			req:  &Request{FunctionCode: 0x41, PDU: []byte{0x03, 0x00, 0x01}},
			code: ExcIllegalFunction,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequest(tt.req)
			if tt.code == 0 {
				if err != nil {
					t.Fatalf("expected valid vendor request, got %v", err)
				}
				return
			}
			if DownstreamException(err) != tt.code {
				t.Fatalf("expected exception 0x%02X, got error %v", tt.code, err)
			}
		})
	}
}

func TestClient_VendorFunctionReturnsRawPDU(t *testing.T) {
	want := []byte{0x41, 0x00, 0xAA}
	client, sessions := newFakeClient([]fakeResult{{data: want}})

	resp, err := client.Execute(t.Context(), vendorRequest())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !bytes.Equal(resp, want) {
		t.Fatalf("response = % x, want % x", resp, want)
	}
	if sessions.client.callCount() != 1 {
		t.Fatalf("wire calls = %d, want 1", sessions.client.callCount())
	}
}

func TestClient_VendorFunctionDoesNotRetryTransportFailure(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{
		{err: fakeTimeoutError{}},
		{data: []byte{0x41, 0x00, 0xAA}},
	})

	_, err := client.Execute(t.Context(), vendorRequest())
	if ErrorKindOf(err) != ErrorTransportTimeout {
		t.Fatalf("expected transport timeout, got %v", err)
	}
	if sessions.client.callCount() != 1 || sessions.sessions.Load() != 1 {
		t.Fatalf("vendor function retried, calls=%d sessions=%d", sessions.client.callCount(), sessions.sessions.Load())
	}
}

func TestClient_LoopbackVendorFunction(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	wantPDU := []byte{0x41, 0x00, 0x01, 0x02}
	wantResp := []byte{0x41, 0x00, 0xAA}
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		header := make([]byte, mbapHeaderSize)
		if _, readErr := io.ReadFull(conn, header); readErr != nil {
			serverErr <- readErr
			return
		}
		pduLen := int(binary.BigEndian.Uint16(header[4:6])) - 1
		pdu := make([]byte, pduLen)
		if _, readErr := io.ReadFull(conn, pdu); readErr != nil {
			serverErr <- readErr
			return
		}
		if !bytes.Equal(pdu, wantPDU) {
			serverErr <- fmt.Errorf("upstream pdu = % x, want % x", pdu, wantPDU)
			return
		}
		response := make([]byte, mbapHeaderSize+len(wantResp))
		copy(response[:2], header[:2])
		binary.BigEndian.PutUint16(response[4:6], uint16(len(wantResp)+1))
		response[6] = header[6]
		copy(response[7:], wantResp)
		_, writeErr := conn.Write(response)
		serverErr <- writeErr
	}()

	client := NewClient(listener.Addr().String(), time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	resp, err := client.Execute(t.Context(), vendorRequest())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !bytes.Equal(resp, wantResp) {
		t.Fatalf("response = % x, want % x", resp, wantResp)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("loopback server: %v", err)
	}
}

func TestClient_LoopbackVendorException(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		request := make([]byte, 11)
		if _, readErr := io.ReadFull(conn, request); readErr != nil {
			serverErr <- readErr
			return
		}
		response := make([]byte, 9)
		copy(response[:2], request[:2])
		binary.BigEndian.PutUint16(response[4:6], 3)
		response[6] = request[6]
		response[7] = request[7] | 0x80
		response[8] = ExcIllegalValue
		_, writeErr := conn.Write(response)
		serverErr <- writeErr
	}()

	client := NewClient(listener.Addr().String(), time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	_, err = client.Execute(t.Context(), vendorRequest())
	if ErrorKindOf(err) != ErrorProtocolException {
		t.Fatalf("expected protocol exception, got %v", err)
	}
	if DownstreamException(err) != ExcIllegalValue {
		t.Fatalf("expected preserved exception, got 0x%02X", DownstreamException(err))
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("loopback server: %v", err)
	}
}

func TestClient_LoopbackVendorMalformedResponsesMapGatewayWithoutRetry(t *testing.T) {
	tests := []struct {
		name          string
		buildResponse func([]byte) []byte
	}{
		{
			name: "wrong normal function code",
			buildResponse: func(request []byte) []byte {
				response := make([]byte, 11)
				copy(response[:2], request[:2])
				binary.BigEndian.PutUint16(response[4:6], 5)
				response[6] = request[6]
				response[7] = FuncReadHoldingRegisters
				response[8] = 2
				response[9] = 0x12
				response[10] = 0x34
				return response
			},
		},
		{
			name: "exception missing code",
			buildResponse: func(request []byte) []byte {
				response := make([]byte, 8)
				copy(response[:2], request[:2])
				binary.BigEndian.PutUint16(response[4:6], 2)
				response[6] = request[6]
				response[7] = request[7] | 0x80
				return response
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer listener.Close()

			var accepts atomic.Int32
			serverErr := make(chan error, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					serverErr <- acceptErr
					return
				}
				accepts.Add(1)
				defer conn.Close()
				request := make([]byte, 11)
				if _, readErr := io.ReadFull(conn, request); readErr != nil {
					serverErr <- readErr
					return
				}
				_, writeErr := conn.Write(tt.buildResponse(request))
				serverErr <- writeErr
			}()

			client := NewClient(listener.Addr().String(), time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Errorf("close client: %v", err)
				}
			})
			_, err = client.Execute(t.Context(), vendorRequest())
			if ErrorKindOf(err) != ErrorTransportClosed {
				t.Fatalf("expected transport closed, got %v", err)
			}
			if DownstreamException(err) != ExcGatewayTargetFailed {
				t.Fatalf("expected gateway exception, got 0x%02X", DownstreamException(err))
			}
			if accepts.Load() != 1 {
				t.Fatalf("vendor malformed response retried, accepts=%d", accepts.Load())
			}
			if err := <-serverErr; err != nil {
				t.Fatalf("loopback server: %v", err)
			}
		})
	}
}

func TestClient_UnknownUpstreamErrorsRetryReadsAndMapToGatewayFailure(t *testing.T) {
	client, sessions := newFakeClient([]fakeResult{
		{err: errors.New("modbus: transaction id mismatch")},
		{err: errors.New("modbus: response length mismatch")},
	})

	_, err := client.Execute(t.Context(), readRequest())
	if ErrorKindOf(err) != ErrorTransportClosed {
		t.Fatalf("expected transport classification, got %v", err)
	}
	if DownstreamException(err) != ExcGatewayTargetFailed {
		t.Fatalf("expected gateway exception, got 0x%02X", DownstreamException(err))
	}
	if sessions.client.callCount() != 2 || sessions.sessions.Load() != 2 {
		t.Fatalf("expected reconnect and retry, calls=%d sessions=%d", sessions.client.callCount(), sessions.sessions.Load())
	}
}

func TestClient_LoopbackProtocolException(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		request := make([]byte, 12)
		if _, readErr := io.ReadFull(conn, request); readErr != nil {
			serverErr <- readErr
			return
		}
		response := make([]byte, 9)
		copy(response[:2], request[:2])
		binary.BigEndian.PutUint16(response[4:6], 3)
		response[6] = request[6]
		response[7] = request[7] | 0x80
		response[8] = ExcIllegalAddress
		_, writeErr := conn.Write(response)
		serverErr <- writeErr
	}()

	client := NewClient(listener.Addr().String(), time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	_, err = client.Execute(t.Context(), readRequest())
	if ErrorKindOf(err) != ErrorProtocolException {
		t.Fatalf("expected protocol exception, got %v", err)
	}
	if DownstreamException(err) != ExcIllegalAddress {
		t.Fatalf("expected preserved exception, got 0x%02X", DownstreamException(err))
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("loopback server: %v", err)
	}
}

func TestClient_LoopbackMalformedModbusErrorsReconnectAndMapGateway(t *testing.T) {
	tests := []struct {
		name          string
		buildResponse func([]byte) []byte
	}{
		{
			name: "wrong normal function code",
			buildResponse: func(request []byte) []byte {
				response := make([]byte, 11)
				copy(response[:2], request[:2])
				binary.BigEndian.PutUint16(response[4:6], 5)
				response[6] = request[6]
				response[7] = FuncReadInputRegisters
				response[8] = 2
				response[9] = 0x12
				response[10] = 0x34
				return response
			},
		},
		{
			name: "exception missing code",
			buildResponse: func(request []byte) []byte {
				response := make([]byte, 8)
				copy(response[:2], request[:2])
				binary.BigEndian.PutUint16(response[4:6], 2)
				response[6] = request[6]
				response[7] = request[7] | 0x80
				return response
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer listener.Close()

			serverErr := make(chan error, 1)
			var accepts atomic.Int32
			go func() {
				for range 2 {
					conn, acceptErr := listener.Accept()
					if acceptErr != nil {
						serverErr <- acceptErr
						return
					}
					accepts.Add(1)
					request := make([]byte, 12)
					if _, readErr := io.ReadFull(conn, request); readErr != nil {
						conn.Close()
						serverErr <- readErr
						return
					}
					_, writeErr := conn.Write(tt.buildResponse(request))
					closeErr := conn.Close()
					if writeErr != nil {
						serverErr <- writeErr
						return
					}
					if closeErr != nil {
						serverErr <- closeErr
						return
					}
				}
				serverErr <- nil
			}()

			client := NewClient(listener.Addr().String(), time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Errorf("close client: %v", err)
				}
			})
			_, err = client.Execute(t.Context(), readRequest())
			if ErrorKindOf(err) != ErrorTransportClosed {
				t.Fatalf("expected framing failure, got %v", err)
			}
			if code := DownstreamException(err); code != ExcGatewayTargetFailed {
				t.Fatalf("expected gateway exception, got 0x%02X", code)
			}
			if accepts.Load() != 2 {
				t.Fatalf("accepted %d connections, want 2", accepts.Load())
			}
			if err := <-serverErr; err != nil {
				t.Fatalf("loopback server: %v", err)
			}
		})
	}
}

func TestClient_ContextDuringConnectDelayClosesSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	socketClosed := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			socketClosed <- acceptErr
			return
		}
		defer conn.Close()
		var b [1]byte
		_, readErr := conn.Read(b[:])
		socketClosed <- readErr
	}()

	client := NewClient(listener.Addr().String(), time.Second, 0, 200*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = client.Execute(ctx, readRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected connect deadline, got %v", err)
	}
	select {
	case closeErr := <-socketClosed:
		if closeErr == nil {
			t.Fatal("expected closed connection")
		}
	case <-time.After(time.Second):
		t.Fatal("connection leaked after connect-delay deadline")
	}
}

func TestClient_LoopbackDeadlineClosesActiveSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	socketClosed := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			socketClosed <- acceptErr
			return
		}
		defer conn.Close()
		request := make([]byte, 12)
		if _, readErr := io.ReadFull(conn, request); readErr != nil {
			socketClosed <- readErr
			return
		}
		var b [1]byte
		_, readErr := conn.Read(b[:])
		socketClosed <- readErr
	}()

	client := NewClient(listener.Addr().String(), time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = client.Execute(ctx, readRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("active socket ignored request deadline: %v", elapsed)
	}
	select {
	case closeErr := <-socketClosed:
		if closeErr == nil {
			t.Fatal("expected closed upstream socket")
		}
	case <-time.After(time.Second):
		t.Fatal("upstream socket was not closed after deadline")
	}
}

func TestClient_LoopbackReadTimeoutReconnectsAndRetries(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		first, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		request := make([]byte, 12)
		if _, readErr := io.ReadFull(first, request); readErr != nil {
			first.Close()
			serverErr <- readErr
			return
		}
		var b [1]byte
		if _, readErr := first.Read(b[:]); readErr == nil {
			first.Close()
			serverErr <- errors.New("expected first connection to close")
			return
		}
		if closeErr := first.Close(); closeErr != nil {
			serverErr <- closeErr
			return
		}

		second, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer second.Close()
		if _, readErr := io.ReadFull(second, request); readErr != nil {
			serverErr <- readErr
			return
		}
		response := make([]byte, 11)
		copy(response[:2], request[:2])
		binary.BigEndian.PutUint16(response[4:6], 5)
		response[6] = request[6]
		response[7] = request[7]
		response[8] = 2
		response[9] = 0x12
		response[10] = 0x34
		_, writeErr := second.Write(response)
		serverErr <- writeErr
	}()

	client := NewClient(listener.Addr().String(), 20*time.Millisecond, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := client.Execute(ctx, readRequest())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(resp) != 4 || resp[2] != 0x12 || resp[3] != 0x34 {
		t.Fatalf("unexpected retry response: % x", resp)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("loopback server: %v", err)
	}
}

func TestClient_LoopbackFramingErrorReconnectsAndRetries(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		request := make([]byte, 12)
		first, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		if _, readErr := io.ReadFull(first, request); readErr != nil {
			first.Close()
			serverErr <- readErr
			return
		}
		response := make([]byte, 11)
		binary.BigEndian.PutUint16(response[0:2], binary.BigEndian.Uint16(request[0:2])+1)
		binary.BigEndian.PutUint16(response[4:6], 5)
		response[6] = request[6]
		response[7] = request[7]
		response[8] = 2
		response[9] = 0xAA
		response[10] = 0xBB
		if _, writeErr := first.Write(response); writeErr != nil {
			first.Close()
			serverErr <- writeErr
			return
		}
		if closeErr := first.Close(); closeErr != nil {
			serverErr <- closeErr
			return
		}

		second, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer second.Close()
		if _, readErr := io.ReadFull(second, request); readErr != nil {
			serverErr <- readErr
			return
		}
		copy(response[:2], request[:2])
		response[9] = 0x12
		response[10] = 0x34
		_, writeErr := second.Write(response)
		serverErr <- writeErr
	}()

	client := NewClient(listener.Addr().String(), time.Second, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	resp, err := client.Execute(t.Context(), readRequest())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(resp) != 4 || resp[2] != 0x12 || resp[3] != 0x34 {
		t.Fatalf("unexpected retry response: % x", resp)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("loopback server: %v", err)
	}
}
