package modbus

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	gridmodbus "github.com/grid-x/modbus"
)

// mockHandler implements Handler for testing
type mockHandler struct {
	response []byte
	err      error
}

type testCoalescedError struct {
	err       error
	coalesced bool
	wait      time.Duration
}

func (e *testCoalescedError) Error() string {
	return e.err.Error()
}

func (e *testCoalescedError) Unwrap() error {
	return e.err
}

func (e *testCoalescedError) Coalesced() bool {
	return e.coalesced
}

func (e *testCoalescedError) CoalescedWaitDuration() time.Duration {
	return e.wait
}

func (h *mockHandler) HandleRequest(ctx context.Context, req *Request) ([]byte, error) {
	return h.response, h.err
}

func TestServer_AcceptConnections(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := &mockHandler{
		response: []byte{0x03, 0x02, 0x00, 0x01}, // Read holding registers response
	}

	server := NewServer(handler, time.Second, logger)
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	addr := server.listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)
	defer server.Close()

	// Connect as client
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Send a read holding registers request
	req := buildMBAPRequest(1, 1, 0x03, 0, 1)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	// Read response
	resp := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	// Verify MBAP header
	if n < 9 {
		t.Fatalf("response too short: %d bytes", n)
	}

	// Transaction ID should match
	txID := binary.BigEndian.Uint16(resp[0:2])
	if txID != 1 {
		t.Errorf("expected transaction ID 1, got %d", txID)
	}

	// Function code should be 0x03
	if resp[7] != 0x03 {
		t.Errorf("expected function code 0x03, got 0x%02X", resp[7])
	}
}

func TestServer_ParsePDU(t *testing.T) {
	tests := []struct {
		name     string
		funcCode byte
		pdu      []byte
		wantAddr uint16
		wantQty  uint16
		wantData []byte
	}{
		{
			name:     "read coils",
			funcCode: FuncReadCoils,
			pdu:      []byte{0x01, 0x00, 0x13, 0x00, 0x25},
			wantAddr: 19,
			wantQty:  37,
		},
		{
			name:     "read holding registers",
			funcCode: FuncReadHoldingRegisters,
			pdu:      []byte{0x03, 0x00, 0x6B, 0x00, 0x03},
			wantAddr: 107,
			wantQty:  3,
		},
		{
			name:     "write single coil",
			funcCode: FuncWriteSingleCoil,
			pdu:      []byte{0x05, 0x00, 0xAC, 0xFF, 0x00},
			wantAddr: 172,
			wantQty:  1,
			wantData: []byte{0xFF, 0x00},
		},
		{
			name:     "write single register",
			funcCode: FuncWriteSingleRegister,
			pdu:      []byte{0x06, 0x00, 0x01, 0x00, 0x03},
			wantAddr: 1,
			wantQty:  1,
			wantData: []byte{0x00, 0x03},
		},
		{
			name:     "write multiple registers",
			funcCode: FuncWriteMultipleRegs,
			pdu:      []byte{0x10, 0x00, 0x01, 0x00, 0x02, 0x04, 0x00, 0x0A, 0x01, 0x02},
			wantAddr: 1,
			wantQty:  2,
			wantData: []byte{0x00, 0x0A, 0x01, 0x02},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(nil, time.Second, logger)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &Request{FunctionCode: tt.funcCode}
			err := server.parsePDU(req, tt.pdu)
			if err != nil {
				t.Fatalf("parsePDU error: %v", err)
			}

			if req.Address != tt.wantAddr {
				t.Errorf("address: got %d, want %d", req.Address, tt.wantAddr)
			}
			if req.Quantity != tt.wantQty {
				t.Errorf("quantity: got %d, want %d", req.Quantity, tt.wantQty)
			}
			if tt.wantData != nil {
				if len(req.Data) != len(tt.wantData) {
					t.Errorf("data length: got %d, want %d", len(req.Data), len(tt.wantData))
				}
				for i := range tt.wantData {
					if req.Data[i] != tt.wantData[i] {
						t.Errorf("data[%d]: got 0x%02X, want 0x%02X", i, req.Data[i], tt.wantData[i])
					}
				}
			}
		})
	}
}

func TestServer_PreservesUpstreamException(t *testing.T) {
	handler := &mockHandler{err: &RequestError{
		Kind:          ErrorProtocolException,
		ExceptionCode: ExcIllegalAddress,
		Attempts:      1,
		Err: &gridmodbus.Error{
			FunctionCode:  FuncReadHoldingRegisters | 0x80,
			ExceptionCode: ExcIllegalAddress,
		},
	}}
	resp := executeServerRequest(t, handler, time.Second)
	if resp[7] != FuncReadHoldingRegisters|0x80 || resp[8] != ExcIllegalAddress {
		t.Fatalf("unexpected exception response: % x", resp[7:9])
	}
}

func TestServer_CoalescedFailureDoesNotClaimLeaderAttempts(t *testing.T) {
	output, logger := newDiagnosticLogger()
	server := NewServer(nil, time.Second, logger)
	req := &Request{
		SlaveID:      9,
		FunctionCode: FuncReadHoldingRegisters,
		Address:      400,
		Quantity:     2,
	}
	err := &testCoalescedError{
		err: &RequestError{
			Kind:              ErrorTransportTimeout,
			Attempts:          2,
			QueueDuration:     time.Second,
			AttemptDuration:   2 * time.Second,
			ReconnectDuration: 3 * time.Second,
			TotalDuration:     6 * time.Second,
			Err:               fakeTimeoutError{},
		},
		coalesced: true,
		wait:      4 * time.Second,
	}

	server.logHandlerError(req, err, ExcGatewayTargetFailed, 5*time.Second)

	entry := findLogEntry(t, output, "request failed")
	requireLifecycleFields(t, entry)
	if entry["level"] != "WARN" ||
		entry["attempts"] != float64(0) ||
		entry["attempt_duration"] != float64(0) ||
		entry["reconnect_duration"] != float64(0) ||
		entry["coalesced"] != true ||
		entry["coalesced_waited"] != true ||
		entry["downstream_exception"] != "0x0B" {
		t.Fatalf("coalesced follower claimed leader diagnostics: %+v", entry)
	}
	requireDurationField(t, entry, "queue_duration", 4*time.Second)
	requireDurationField(t, entry, "coalesced_wait_duration", 4*time.Second)
	requireDurationField(t, entry, "total_duration", 5*time.Second)
}

func TestServer_ReplacementLeaderFailureIncludesWaitAndAttempts(t *testing.T) {
	output, logger := newDiagnosticLogger()
	server := NewServer(nil, time.Second, logger)
	req := &Request{
		SlaveID:      9,
		FunctionCode: FuncReadHoldingRegisters,
		Address:      400,
		Quantity:     2,
	}
	err := &testCoalescedError{
		err: &RequestError{
			Kind:              ErrorTransportTimeout,
			Attempts:          2,
			QueueDuration:     time.Second,
			AttemptDuration:   2 * time.Second,
			ReconnectDuration: 3 * time.Second,
			TotalDuration:     6 * time.Second,
			Err:               fakeTimeoutError{},
		},
		wait: 4 * time.Second,
	}

	server.logHandlerError(req, err, ExcGatewayTargetFailed, 10*time.Second)

	entry := findLogEntry(t, output, "request failed")
	if entry["level"] != "WARN" ||
		entry["attempts"] != float64(2) ||
		entry["coalesced"] != false ||
		entry["coalesced_waited"] != true {
		t.Fatalf("replacement leader attribution missing: %+v", entry)
	}
	requireDurationField(t, entry, "queue_duration", 5*time.Second)
	requireDurationField(t, entry, "attempt_duration", 2*time.Second)
	requireDurationField(t, entry, "reconnect_duration", 3*time.Second)
	requireDurationField(t, entry, "total_duration", 10*time.Second)
}

func TestServer_FinalFailureWarnHasCompleteDiagnostics(t *testing.T) {
	output, logger := newDiagnosticLogger()
	server := NewServer(nil, time.Second, logger)
	req := &Request{
		SlaveID:      9,
		FunctionCode: FuncReadHoldingRegisters,
		Address:      400,
		Quantity:     2,
	}
	err := &RequestError{
		Kind:              ErrorTransportTimeout,
		Attempts:          2,
		QueueDuration:     time.Second,
		AttemptDuration:   2 * time.Second,
		ReconnectDuration: 3 * time.Second,
		TotalDuration:     6 * time.Second,
		Err:               fakeTimeoutError{},
	}

	server.logHandlerError(req, err, ExcGatewayTargetFailed, 7*time.Second)

	entry := findLogEntry(t, output, "request failed")
	requireLifecycleFields(t, entry)
	if entry["level"] != "WARN" ||
		entry["attempts"] != float64(2) ||
		entry["error_kind"] != string(ErrorTransportTimeout) ||
		entry["downstream_exception"] != "0x0B" ||
		entry["will_retry"] != false {
		t.Fatalf("unexpected final failure log: %+v", entry)
	}
	requireDurationField(t, entry, "queue_duration", time.Second)
	requireDurationField(t, entry, "attempt_duration", 2*time.Second)
	requireDurationField(t, entry, "reconnect_duration", 3*time.Second)
	requireDurationField(t, entry, "total_duration", 7*time.Second)
}

func TestServer_ProtocolExceptionWarnPreservesCode(t *testing.T) {
	output, logger := newDiagnosticLogger()
	server := NewServer(nil, time.Second, logger)
	req := &Request{
		SlaveID:      9,
		FunctionCode: FuncReadHoldingRegisters,
		Address:      400,
		Quantity:     2,
	}
	err := &RequestError{
		Kind:          ErrorProtocolException,
		ExceptionCode: ExcIllegalAddress,
		Attempts:      1,
		Err: &gridmodbus.Error{
			FunctionCode:  FuncReadHoldingRegisters | 0x80,
			ExceptionCode: ExcIllegalAddress,
		},
	}

	server.logHandlerError(req, err, ExcIllegalAddress, time.Second)

	entry := findLogEntry(t, output, "request failed")
	requireLifecycleFields(t, entry)
	if entry["level"] != "WARN" ||
		entry["attempts"] != float64(1) ||
		entry["error_kind"] != string(ErrorProtocolException) ||
		entry["exception_code"] != "0x02" ||
		entry["downstream_exception"] != "0x02" {
		t.Fatalf("unexpected protocol exception log: %+v", entry)
	}
}

func TestServer_ContextCancellationIsDebug(t *testing.T) {
	output, logger := newDiagnosticLogger()
	server := NewServer(nil, time.Second, logger)
	req := &Request{
		SlaveID:      9,
		FunctionCode: FuncReadHoldingRegisters,
		Address:      400,
		Quantity:     2,
	}

	server.logHandlerError(req, context.Canceled, ExcGatewayTargetFailed, time.Second)

	entry := findLogEntry(t, output, "request canceled")
	requireLifecycleFields(t, entry)
	if entry["level"] != "DEBUG" || entry["error_kind"] != string(ErrorContextCanceled) {
		t.Fatalf("unexpected cancellation log: %+v", entry)
	}
}

func TestServer_WriteFailureDoesNotLogRawError(t *testing.T) {
	const payload = "sensitivepayload1234"
	output, logger := newDiagnosticLogger()
	server := NewServer(nil, time.Second, logger)
	req := &Request{
		SlaveID:      9,
		FunctionCode: FuncWriteMultipleRegs,
		Address:      400,
		Quantity:     10,
		Data:         []byte(payload),
	}
	err := &RequestError{
		Kind:     ErrorTransportClosed,
		Attempts: 1,
		Err:      errors.New(payload),
	}

	server.logHandlerError(req, err, ExcGatewayTargetFailed, time.Second)

	if bytes.Contains(output.Bytes(), []byte(payload)) {
		t.Fatalf("write payload leaked through raw error: %s", output.String())
	}
	entry := findLogEntry(t, output, "request failed")
	requireLifecycleFields(t, entry)
	if _, ok := entry["error"]; ok {
		t.Fatalf("write failure retained raw error: %+v", entry)
	}
}

func TestServer_MapsRequestDeadlineToGatewayException(t *testing.T) {
	handler := HandlerFunc(func(ctx context.Context, req *Request) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	resp := executeServerRequest(t, handler, 20*time.Millisecond)
	if resp[7] != FuncReadHoldingRegisters|0x80 || resp[8] != ExcGatewayTargetFailed {
		t.Fatalf("unexpected exception response: % x", resp[7:9])
	}
}

func TestServer_RejectsNominalSuccessAfterRequestDeadline(t *testing.T) {
	handler := HandlerFunc(func(context.Context, *Request) ([]byte, error) {
		time.Sleep(30 * time.Millisecond)
		return []byte{FuncReadHoldingRegisters, 2, 0, 1}, nil
	})
	resp := executeServerRequest(t, handler, 10*time.Millisecond)
	if resp[7] != FuncReadHoldingRegisters|0x80 || resp[8] != ExcGatewayTargetFailed {
		t.Fatalf("unexpected deadline response: % x", resp[7:9])
	}
}

type HandlerFunc func(context.Context, *Request) ([]byte, error)

func (f HandlerFunc) HandleRequest(ctx context.Context, req *Request) ([]byte, error) {
	return f(ctx, req)
}

func executeServerRequest(t *testing.T, handler Handler, timeout time.Duration) []byte {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(handler, timeout, logger)
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := server.Serve(ctx); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})

	conn, err := net.DialTimeout("tcp", server.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(buildMBAPRequest(1, 1, FuncReadHoldingRegisters, 10, 2)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	resp := make([]byte, 9)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

func TestIsWriteFunction(t *testing.T) {
	writes := []byte{FuncWriteSingleCoil, FuncWriteSingleRegister, FuncWriteMultipleCoils, FuncWriteMultipleRegs}
	for _, fc := range writes {
		if !IsWriteFunction(fc) {
			t.Errorf("expected 0x%02X to be write function", fc)
		}
	}

	reads := []byte{FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters}
	for _, fc := range reads {
		if IsWriteFunction(fc) {
			t.Errorf("expected 0x%02X to NOT be write function", fc)
		}
	}
}

func TestIsReadFunction(t *testing.T) {
	reads := []byte{FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters}
	for _, fc := range reads {
		if !IsReadFunction(fc) {
			t.Errorf("expected 0x%02X to be read function", fc)
		}
	}

	writes := []byte{FuncWriteSingleCoil, FuncWriteSingleRegister, FuncWriteMultipleCoils, FuncWriteMultipleRegs}
	for _, fc := range writes {
		if IsReadFunction(fc) {
			t.Errorf("expected 0x%02X to NOT be read function", fc)
		}
	}
}

func TestBuildExceptionResponse(t *testing.T) {
	resp := BuildExceptionResponse(0x03, ExcIllegalFunction)
	if len(resp) != 2 {
		t.Errorf("expected 2 bytes, got %d", len(resp))
	}
	if resp[0] != 0x83 {
		t.Errorf("expected function code 0x83, got 0x%02X", resp[0])
	}
	if resp[1] != ExcIllegalFunction {
		t.Errorf("expected exception code 0x%02X, got 0x%02X", ExcIllegalFunction, resp[1])
	}
}

// buildMBAPRequest builds a Modbus TCP request frame
func buildMBAPRequest(txID uint16, slaveID byte, funcCode byte, address uint16, quantity uint16) []byte {
	pdu := make([]byte, 5)
	pdu[0] = funcCode
	binary.BigEndian.PutUint16(pdu[1:3], address)
	binary.BigEndian.PutUint16(pdu[3:5], quantity)

	frame := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(frame[0:2], txID)               // Transaction ID
	binary.BigEndian.PutUint16(frame[2:4], 0)                  // Protocol ID (Modbus = 0)
	binary.BigEndian.PutUint16(frame[4:6], uint16(len(pdu)+1)) // Length
	frame[6] = slaveID
	copy(frame[7:], pdu)

	return frame
}
