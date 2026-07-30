package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tma/mbproxy/internal/cache"
	"github.com/tma/mbproxy/internal/config"
	"github.com/tma/mbproxy/internal/modbus"
)

// mockClient implements a mock upstream client for testing
type mockClient struct {
	response []byte
	err      error
	calls    int
}

type interleavingClient struct {
	mu               sync.Mutex
	executeMu        sync.Mutex
	readCalls        int
	firstReadStarted chan struct{}
	releaseFirstRead chan struct{}
	writeQueued      chan struct{}
	writeStarted     chan struct{}
	writeCompleted   chan struct{}
	secondReadQueued chan struct{}
}

type writeWindowClient struct {
	writeEntered chan struct{}
	releaseWrite chan struct{}
	readExecuted chan struct{}
	writeErr     error
}

func (c *interleavingClient) Connect() error { return nil }
func (c *interleavingClient) Close() error   { return nil }
func (c *interleavingClient) Healthy() error { return nil }

func (c *interleavingClient) Execute(_ context.Context, req *modbus.Request) ([]byte, error) {
	if modbus.IsWriteFunction(req.FunctionCode) {
		close(c.writeQueued)
		c.executeMu.Lock()
		defer c.executeMu.Unlock()
		close(c.writeStarted)
		close(c.writeCompleted)
		return []byte{req.FunctionCode, byte(req.Address >> 8), byte(req.Address), 0, 2}, nil
	}

	c.mu.Lock()
	c.readCalls++
	call := c.readCalls
	c.mu.Unlock()
	if call == 1 {
		c.executeMu.Lock()
		defer c.executeMu.Unlock()
		close(c.firstReadStarted)
		<-c.releaseFirstRead
		return []byte{req.FunctionCode, 2, 0, 1}, nil
	}
	close(c.secondReadQueued)
	<-c.writeCompleted
	c.executeMu.Lock()
	defer c.executeMu.Unlock()
	return []byte{req.FunctionCode, 2, 0, 2}, nil
}

func (c *writeWindowClient) Connect() error { return nil }
func (c *writeWindowClient) Close() error   { return nil }
func (c *writeWindowClient) Healthy() error { return nil }

func (c *writeWindowClient) Execute(_ context.Context, req *modbus.Request) ([]byte, error) {
	if modbus.IsWriteFunction(req.FunctionCode) {
		close(c.writeEntered)
		<-c.releaseWrite
		if c.writeErr != nil {
			return nil, c.writeErr
		}
		return []byte{req.FunctionCode, byte(req.Address >> 8), byte(req.Address), 0, 2}, nil
	}

	close(c.readExecuted)
	return []byte{req.FunctionCode, 2, 0, 1}, nil
}

func (m *mockClient) Connect() error { return nil }

func (m *mockClient) Close() error { return nil }

func (m *mockClient) Healthy() error { return nil }

func (m *mockClient) Execute(ctx context.Context, req *modbus.Request) ([]byte, error) {
	m.calls++
	return m.response, m.err
}

func TestProxy_HandleReadCacheHit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()

	p := &Proxy{
		cfg: &config.Config{
			CacheTTL:        time.Second,
			CacheServeStale: false,
			ReadOnly:        config.ReadOnlyOn,
		},
		logger: logger,
		cache:  c,
	}

	// Pre-populate cache with per-register values
	c.SetRange(1, modbus.FuncReadHoldingRegisters, 0, [][]byte{
		{0x00, 0x01},
		{0x00, 0x02},
	})

	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncReadHoldingRegisters,
		Address:      0,
		Quantity:     2,
	}

	resp, err := p.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected assembled response: funcCode + byteCount + reg0 + reg1
	expected := []byte{0x03, 0x04, 0x00, 0x01, 0x00, 0x02}
	if len(resp) != len(expected) {
		t.Fatalf("expected %d bytes, got %d", len(expected), len(resp))
	}
	for i := range expected {
		if resp[i] != expected[i] {
			t.Errorf("byte %d: expected 0x%02X, got 0x%02X", i, expected[i], resp[i])
		}
	}
}

func TestProxy_HandleReadMissFetchesAndCaches(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()

	upstream := &mockClient{
		response: []byte{0x03, 0x04, 0x00, 0x0A, 0x00, 0x0B},
	}
	p := &Proxy{
		cfg: &config.Config{
			CacheTTL:        time.Second,
			CacheServeStale: false,
			ReadOnly:        config.ReadOnlyOn,
		},
		logger: logger,
		client: upstream,
		cache:  c,
	}

	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncReadHoldingRegisters,
		Address:      10,
		Quantity:     2,
	}

	resp, err := p.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []byte{0x03, 0x04, 0x00, 0x0A, 0x00, 0x0B}
	if !bytes.Equal(resp, expected) {
		t.Fatalf("first response: expected %v, got %v", expected, resp)
	}
	if upstream.calls != 1 {
		t.Fatalf("expected 1 upstream call after miss, got %d", upstream.calls)
	}

	values, ok := c.GetRange(1, modbus.FuncReadHoldingRegisters, 10, 2)
	if !ok {
		t.Fatal("expected fetched response to be cached per register")
	}
	if !bytes.Equal(values[0], []byte{0x00, 0x0A}) || !bytes.Equal(values[1], []byte{0x00, 0x0B}) {
		t.Fatalf("unexpected cached values: %v", values)
	}

	// Change the upstream response. The second request should be served from cache,
	// so the upstream should not be called again and the response should stay the same.
	upstream.response = []byte{0x03, 0x04, 0x00, 0xFF, 0x00, 0xFF}
	resp, err = p.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on cached read: %v", err)
	}
	if !bytes.Equal(resp, expected) {
		t.Fatalf("cached response: expected %v, got %v", expected, resp)
	}
	if upstream.calls != 1 {
		t.Fatalf("expected cached read to avoid upstream call, got %d calls", upstream.calls)
	}
}

func TestProxy_HandleReadServesStaleOnUpstreamError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(10*time.Millisecond, true)
	defer c.Close()

	c.SetRange(1, modbus.FuncReadHoldingRegisters, 20, [][]byte{
		{0x00, 0x01},
		{0x00, 0x02},
	})
	time.Sleep(20 * time.Millisecond)

	upstreamErr := errors.New("upstream unavailable")
	upstream := &mockClient{err: upstreamErr}
	p := &Proxy{
		cfg: &config.Config{
			CacheTTL:        10 * time.Millisecond,
			CacheServeStale: true,
			ReadOnly:        config.ReadOnlyOn,
		},
		logger: logger,
		client: upstream,
		cache:  c,
	}

	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncReadHoldingRegisters,
		Address:      20,
		Quantity:     2,
	}

	resp, err := p.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("expected stale response, got error: %v", err)
	}
	if upstream.calls != 1 {
		t.Fatalf("expected one failed upstream call before serving stale, got %d", upstream.calls)
	}

	expected := []byte{0x03, 0x04, 0x00, 0x01, 0x00, 0x02}
	if !bytes.Equal(resp, expected) {
		t.Fatalf("stale response: expected %v, got %v", expected, resp)
	}
}

func TestProxy_HandleReadDoesNotHideProtocolExceptionWithStaleData(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Millisecond, true)
	defer c.Close()
	c.SetRange(1, modbus.FuncReadHoldingRegisters, 20, [][]byte{{0x00, 0x01}})
	time.Sleep(2 * time.Millisecond)

	upstreamErr := &modbus.RequestError{
		Kind:          modbus.ErrorProtocolException,
		ExceptionCode: modbus.ExcIllegalAddress,
		Err:           errors.New("upstream exception"),
	}
	p := &Proxy{
		cfg: &config.Config{
			CacheServeStale: true,
			ReadOnly:        config.ReadOnlyOn,
		},
		logger: logger,
		client: &mockClient{err: upstreamErr},
		cache:  c,
	}
	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncReadHoldingRegisters,
		Address:      20,
		Quantity:     1,
	}

	if _, err := p.HandleRequest(t.Context(), req); !errors.Is(err, upstreamErr) {
		t.Fatalf("expected protocol exception, got %v", err)
	}
}

func TestProxy_HandleWriteReadOnlyMode(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name    string
		mode    config.ReadOnlyMode
		wantExc bool // Whether to expect exception response
	}{
		{"readonly on", config.ReadOnlyOn, false},
		{"readonly deny", config.ReadOnlyDeny, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cache.New(time.Second, false)
			defer c.Close()

			p := &Proxy{
				cfg: &config.Config{
					ReadOnly: tt.mode,
				},
				logger: logger,
				cache:  c,
			}

			req := &modbus.Request{
				SlaveID:      1,
				FunctionCode: modbus.FuncWriteSingleRegister,
				Address:      0,
				Quantity:     1,
				Data:         []byte{0x00, 0x0A},
			}

			resp, err := p.HandleRequest(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			isException := resp[0]&0x80 != 0
			if isException != tt.wantExc {
				t.Errorf("exception response: got %v, want %v", isException, tt.wantExc)
			}
		})
	}
}

func TestProxy_HandleUnknownFunction(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()

	p := &Proxy{
		cfg: &config.Config{
			ReadOnly: config.ReadOnlyOn,
		},
		logger: logger,
		cache:  c,
	}

	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: 0x99, // Unknown function
		Address:      0,
		Quantity:     1,
	}

	resp, err := p.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return exception response
	if resp[0] != 0x99|0x80 {
		t.Errorf("expected exception function code 0x%02X, got 0x%02X", 0x99|0x80, resp[0])
	}
	if resp[1] != modbus.ExcIllegalFunction {
		t.Errorf("expected illegal function exception, got 0x%02X", resp[1])
	}
}

func TestProxy_InvalidReadQuantityReturnsIllegalValueWithoutUpstream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()
	upstream := &mockClient{}
	p := &Proxy{
		cfg:    &config.Config{ReadOnly: config.ReadOnlyOn},
		logger: logger,
		client: upstream,
		cache:  c,
	}
	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncReadHoldingRegisters,
		Address:      0,
		Quantity:     0,
	}

	resp, err := p.HandleRequest(t.Context(), req)
	if err != nil {
		t.Fatalf("handle request: %v", err)
	}
	if !bytes.Equal(resp, []byte{modbus.FuncReadHoldingRegisters | 0x80, modbus.ExcIllegalValue}) {
		t.Fatalf("unexpected validation response: % x", resp)
	}
	if upstream.calls != 0 {
		t.Fatalf("invalid request reached upstream %d times", upstream.calls)
	}
}

func TestProxy_AddressRangeOverflowDoesNotReachUpstream(t *testing.T) {
	tests := []struct {
		name string
		req  *modbus.Request
	}{
		{
			name: "read",
			req: &modbus.Request{
				FunctionCode: modbus.FuncReadHoldingRegisters,
				Address:      65535,
				Quantity:     2,
			},
		},
		{
			name: "write multiple",
			req: &modbus.Request{
				FunctionCode: modbus.FuncWriteMultipleRegs,
				Address:      65535,
				Quantity:     2,
				Data:         []byte{0, 1, 0, 2},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cache.New(time.Second, false)
			defer c.Close()
			upstream := &mockClient{}
			p := &Proxy{
				cfg:    &config.Config{ReadOnly: config.ReadOnlyOff},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				client: upstream,
				cache:  c,
			}

			resp, err := p.HandleRequest(t.Context(), tt.req)
			if err != nil {
				t.Fatalf("handle request: %v", err)
			}
			if !bytes.Equal(resp, []byte{tt.req.FunctionCode | 0x80, modbus.ExcIllegalAddress}) {
				t.Fatalf("unexpected validation response: % x", resp)
			}
			if upstream.calls != 0 {
				t.Fatalf("invalid range reached upstream %d times", upstream.calls)
			}
		})
	}
}

func TestProxy_BuildFakeWriteResponse(t *testing.T) {
	p := &Proxy{}

	tests := []struct {
		name    string
		req     *modbus.Request
		wantLen int
	}{
		{
			name: "write single coil",
			req: &modbus.Request{
				FunctionCode: modbus.FuncWriteSingleCoil,
				Address:      100,
				Data:         []byte{0xFF, 0x00},
			},
			wantLen: 5,
		},
		{
			name: "write single register",
			req: &modbus.Request{
				FunctionCode: modbus.FuncWriteSingleRegister,
				Address:      50,
				Data:         []byte{0x00, 0x10},
			},
			wantLen: 5,
		},
		{
			name: "write multiple registers",
			req: &modbus.Request{
				FunctionCode: modbus.FuncWriteMultipleRegs,
				Address:      0,
				Quantity:     5,
			},
			wantLen: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := p.buildFakeWriteResponse(tt.req)
			if len(resp) != tt.wantLen {
				t.Errorf("response length: got %d, want %d", len(resp), tt.wantLen)
			}
			if resp[0] != tt.req.FunctionCode {
				t.Errorf("function code: got 0x%02X, want 0x%02X", resp[0], tt.req.FunctionCode)
			}
		})
	}
}

func TestDecomposeResponse_Registers(t *testing.T) {
	// Response: FC 0x03, byteCount=4, reg0=0x0001, reg1=0x0002
	data := []byte{0x03, 0x04, 0x00, 0x01, 0x00, 0x02}
	values := decomposeResponse(modbus.FuncReadHoldingRegisters, 2, data)

	if len(values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(values))
	}
	if values[0][0] != 0x00 || values[0][1] != 0x01 {
		t.Errorf("reg0: expected 0x0001, got 0x%02X%02X", values[0][0], values[0][1])
	}
	if values[1][0] != 0x00 || values[1][1] != 0x02 {
		t.Errorf("reg1: expected 0x0002, got 0x%02X%02X", values[1][0], values[1][1])
	}
}

func TestDecomposeResponse_Coils(t *testing.T) {
	// Response: FC 0x01, byteCount=2, coils 0-9
	// 0xCD = 1100_1101: coils 0,2,3,6,7 on
	// 0x01 = 0000_0001: coil 8 on
	data := []byte{0x01, 0x02, 0xCD, 0x01}
	values := decomposeResponse(modbus.FuncReadCoils, 10, data)

	if len(values) != 10 {
		t.Fatalf("expected 10 values, got %d", len(values))
	}

	expected := []byte{1, 0, 1, 1, 0, 0, 1, 1, 1, 0}
	for i, exp := range expected {
		if values[i][0] != exp {
			t.Errorf("coil %d: expected %d, got %d", i, exp, values[i][0])
		}
	}
}

func TestAssembleResponse_Registers(t *testing.T) {
	values := [][]byte{{0x00, 0x01}, {0x00, 0x02}}
	resp := assembleResponse(modbus.FuncReadHoldingRegisters, 2, values)

	expected := []byte{0x03, 0x04, 0x00, 0x01, 0x00, 0x02}
	if len(resp) != len(expected) {
		t.Fatalf("expected %d bytes, got %d", len(expected), len(resp))
	}
	for i := range expected {
		if resp[i] != expected[i] {
			t.Errorf("byte %d: expected 0x%02X, got 0x%02X", i, expected[i], resp[i])
		}
	}
}

func TestAssembleResponse_Coils(t *testing.T) {
	// Coils 0,2,3,6,7 on, 8 on — should produce 0xCD 0x01
	values := [][]byte{{1}, {0}, {1}, {1}, {0}, {0}, {1}, {1}, {1}, {0}}
	resp := assembleResponse(modbus.FuncReadCoils, 10, values)

	expected := []byte{0x01, 0x02, 0xCD, 0x01}
	if len(resp) != len(expected) {
		t.Fatalf("expected %d bytes, got %d", len(expected), len(resp))
	}
	for i := range expected {
		if resp[i] != expected[i] {
			t.Errorf("byte %d: expected 0x%02X, got 0x%02X", i, expected[i], resp[i])
		}
	}
}

func TestDecomposeAssemble_Roundtrip(t *testing.T) {
	tests := []struct {
		name     string
		funcCode byte
		quantity uint16
		data     []byte
	}{
		{
			name:     "holding registers",
			funcCode: modbus.FuncReadHoldingRegisters,
			quantity: 3,
			data:     []byte{0x03, 0x06, 0x00, 0x01, 0x00, 0x02, 0x00, 0x03},
		},
		{
			name:     "input registers",
			funcCode: modbus.FuncReadInputRegisters,
			quantity: 2,
			data:     []byte{0x04, 0x04, 0xFF, 0xFF, 0x00, 0x00},
		},
		{
			name:     "coils",
			funcCode: modbus.FuncReadCoils,
			quantity: 10,
			data:     []byte{0x01, 0x02, 0xCD, 0x01},
		},
		{
			name:     "discrete inputs",
			funcCode: modbus.FuncReadDiscreteInputs,
			quantity: 8,
			data:     []byte{0x02, 0x01, 0xAC},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := decomposeResponse(tt.funcCode, tt.quantity, tt.data)
			if values == nil {
				t.Fatal("decomposeResponse returned nil")
			}

			reassembled := assembleResponse(tt.funcCode, tt.quantity, values)
			if len(reassembled) != len(tt.data) {
				t.Fatalf("length mismatch: expected %d, got %d", len(tt.data), len(reassembled))
			}
			for i := range tt.data {
				if reassembled[i] != tt.data[i] {
					t.Errorf("byte %d: expected 0x%02X, got 0x%02X", i, tt.data[i], reassembled[i])
				}
			}
		})
	}
}

func TestProxy_WriteInvalidatesOverlappingReads(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()

	p := &Proxy{
		cfg: &config.Config{
			ReadOnly: config.ReadOnlyOn,
		},
		logger: logger,
		cache:  c,
	}

	// Cache registers 0-9 (simulating a previous read of range 0-9)
	regs := make([][]byte, 10)
	for i := range regs {
		regs[i] = []byte{0x00, byte(i)}
	}
	c.SetRange(1, modbus.FuncReadHoldingRegisters, 0, regs)

	// Write to register 5 — should invalidate register 5
	p.invalidateCache(&modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncWriteSingleRegister,
		Address:      5,
		Quantity:     1,
	})

	// Full range 0-9 should now miss (register 5 is gone)
	_, ok := c.GetRange(1, modbus.FuncReadHoldingRegisters, 0, 10)
	if ok {
		t.Error("expected range miss after write invalidation of register 5")
	}

	// Registers 0-4 and 6-9 should still be cached individually
	for i := uint16(0); i < 10; i++ {
		_, ok := c.Get(cache.RegKey(1, modbus.FuncReadHoldingRegisters, i))
		if i == 5 {
			if ok {
				t.Error("register 5 should be invalidated")
			}
		} else {
			if !ok {
				t.Errorf("register %d should still be cached", i)
			}
		}
	}
}

func TestProxy_WriteInvalidatesMultipleRegisters(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()

	p := &Proxy{
		cfg: &config.Config{
			ReadOnly: config.ReadOnlyOn,
		},
		logger: logger,
		cache:  c,
	}

	// Cache registers 0-9
	regs := make([][]byte, 10)
	for i := range regs {
		regs[i] = []byte{0x00, byte(i)}
	}
	c.SetRange(1, modbus.FuncReadHoldingRegisters, 0, regs)

	// Write to registers 3-5 (write multiple)
	p.invalidateCache(&modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncWriteMultipleRegs,
		Address:      3,
		Quantity:     3,
	})

	// Registers 3,4,5 should be gone
	for i := uint16(3); i <= 5; i++ {
		if _, ok := c.Get(cache.RegKey(1, modbus.FuncReadHoldingRegisters, i)); ok {
			t.Errorf("register %d should be invalidated", i)
		}
	}

	// Registers 0,1,2,6,7,8,9 should still be cached
	for _, i := range []uint16{0, 1, 2, 6, 7, 8, 9} {
		if _, ok := c.Get(cache.RegKey(1, modbus.FuncReadHoldingRegisters, i)); !ok {
			t.Errorf("register %d should still be cached", i)
		}
	}
}

func TestProxy_AmbiguousWriteInvalidatesCache(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()
	c.SetRange(1, modbus.FuncReadHoldingRegisters, 5, [][]byte{{0x00, 0x01}})

	upstreamErr := &modbus.RequestError{
		Kind:     modbus.ErrorTransportTimeout,
		Attempts: 1,
		Err:      errors.New("timeout"),
	}
	p := &Proxy{
		cfg:    &config.Config{ReadOnly: config.ReadOnlyOff},
		logger: logger,
		client: &mockClient{err: upstreamErr},
		cache:  c,
	}
	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncWriteSingleRegister,
		Address:      5,
		Quantity:     1,
		Data:         []byte{0x00, 0x02},
	}

	if _, err := p.HandleRequest(t.Context(), req); !errors.Is(err, upstreamErr) {
		t.Fatalf("expected ambiguous write error, got %v", err)
	}
	if _, ok := c.Get(cache.RegKey(1, modbus.FuncReadHoldingRegisters, 5)); ok {
		t.Fatal("ambiguous write retained pre-write cache value")
	}
}

func TestProxy_WriteGenerationPreventsStaleReadRepopulationAndCoalescing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second, false)
	defer c.Close()
	upstream := &interleavingClient{
		firstReadStarted: make(chan struct{}),
		releaseFirstRead: make(chan struct{}),
		writeQueued:      make(chan struct{}),
		writeStarted:     make(chan struct{}),
		writeCompleted:   make(chan struct{}),
		secondReadQueued: make(chan struct{}),
	}
	p := &Proxy{
		cfg:    &config.Config{ReadOnly: config.ReadOnlyOff},
		logger: logger,
		client: upstream,
		cache:  c,
	}
	read := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncReadHoldingRegisters,
		Address:      5,
		Quantity:     1,
	}
	write := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncWriteSingleRegister,
		Address:      5,
		Quantity:     1,
		Data:         []byte{0, 2},
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := p.HandleRequest(context.Background(), read)
		firstDone <- err
	}()
	<-upstream.firstReadStarted

	writeDone := make(chan error, 1)
	go func() {
		_, err := p.HandleRequest(context.Background(), write)
		writeDone <- err
	}()
	<-upstream.writeQueued

	secondDone := make(chan struct{})
	var secondResp []byte
	var secondErr error
	go func() {
		secondResp, secondErr = p.HandleRequest(context.Background(), read)
		close(secondDone)
	}()
	select {
	case <-upstream.secondReadQueued:
	case <-time.After(time.Second):
		t.Fatal("post-write read joined pre-write coalesced fetch")
	}
	select {
	case <-secondDone:
		t.Fatal("post-write read bypassed serialized write")
	default:
	}

	close(upstream.releaseFirstRead)
	select {
	case <-upstream.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not follow pre-write read")
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("post-write read did not complete after write")
	}
	if secondErr != nil || !bytes.Equal(secondResp, []byte{modbus.FuncReadHoldingRegisters, 2, 0, 2}) {
		t.Fatalf("unexpected post-write response % x, err=%v", secondResp, secondErr)
	}

	if err := <-firstDone; err != nil {
		t.Fatalf("pre-write read: %v", err)
	}
	values, ok := c.GetRange(1, modbus.FuncReadHoldingRegisters, 5, 1)
	if ok && !bytes.Equal(values[0], []byte{0, 2}) {
		t.Fatalf("pre-write read repopulated stale cache: %v, present=%v", values, ok)
	}
}

func TestProxy_PostWriteInvalidationClosesSchedulingWindow(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "protocol error", err: &modbus.RequestError{
			Kind:          modbus.ErrorProtocolException,
			ExceptionCode: modbus.ExcIllegalAddress,
			Attempts:      1,
			Err:           errors.New("protocol exception"),
		}},
		{name: "transport error", err: &modbus.RequestError{
			Kind:     modbus.ErrorTransportClosed,
			Attempts: 1,
			Err:      errors.New("connection closed"),
		}},
		{name: "context error", err: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			c := cache.New(time.Second, false)
			defer c.Close()
			upstream := &writeWindowClient{
				writeEntered: make(chan struct{}),
				releaseWrite: make(chan struct{}),
				readExecuted: make(chan struct{}),
				writeErr:     tt.err,
			}
			p := &Proxy{
				cfg:    &config.Config{ReadOnly: config.ReadOnlyOff},
				logger: logger,
				client: upstream,
				cache:  c,
			}
			read := &modbus.Request{
				SlaveID:      1,
				FunctionCode: modbus.FuncReadHoldingRegisters,
				Address:      5,
				Quantity:     1,
			}
			write := &modbus.Request{
				SlaveID:      1,
				FunctionCode: modbus.FuncWriteSingleRegister,
				Address:      5,
				Quantity:     1,
				Data:         []byte{0, 2},
			}

			writeDone := make(chan error, 1)
			go func() {
				_, err := p.HandleRequest(context.Background(), write)
				writeDone <- err
			}()
			<-upstream.writeEntered

			if _, err := p.HandleRequest(context.Background(), read); err != nil {
				t.Fatalf("read in scheduling window: %v", err)
			}
			<-upstream.readExecuted
			if values, ok := c.GetRange(1, modbus.FuncReadHoldingRegisters, 5, 1); !ok || !bytes.Equal(values[0], []byte{0, 1}) {
				t.Fatalf("read did not cache pre-write value: %v, present=%v", values, ok)
			}

			close(upstream.releaseWrite)
			err := <-writeDone
			if !errors.Is(err, tt.err) {
				t.Fatalf("write error = %v, want %v", err, tt.err)
			}
			if _, ok := c.GetRange(1, modbus.FuncReadHoldingRegisters, 5, 1); ok {
				t.Fatal("post-write cleanup retained value read during scheduling window")
			}
		})
	}
}

func TestSaturatingDurationSum(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	if got := saturatingDurationSum(maxDuration-1, 2); got != maxDuration {
		t.Fatalf("expected saturated duration, got %v", got)
	}
	if got := saturatingDurationSum(time.Second, 2*time.Second); got != 3*time.Second {
		t.Fatalf("expected normal sum, got %v", got)
	}
	if got := saturatingDurationSum(-time.Second, time.Second); got != time.Second {
		t.Fatalf("expected negative duration to be ignored, got %v", got)
	}
}
