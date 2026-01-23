package proxy

import (
	"context"
	"io"
	"log/slog"
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

func (m *mockClient) Execute(ctx context.Context, req *modbus.Request) ([]byte, error) {
	m.calls++
	return m.response, m.err
}

func TestProxy_HandleReadCacheHit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(time.Second)

	p := &Proxy{
		cfg: &config.Config{
			CacheTTL:        time.Second,
			CacheServeStale: false,
			ReadOnly:        config.ReadOnlyOn,
		},
		logger: logger,
		cache:  c,
	}

	// Pre-populate cache
	key := cache.Key(1, modbus.FuncReadHoldingRegisters, 0, 10)
	c.Set(key, []byte{0x03, 0x14, 0x00, 0x01}) // Function code + byte count + data

	req := &modbus.Request{
		SlaveID:      1,
		FunctionCode: modbus.FuncReadHoldingRegisters,
		Address:      0,
		Quantity:     10,
	}

	resp, err := p.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(resp) != string([]byte{0x03, 0x14, 0x00, 0x01}) {
		t.Errorf("unexpected response: %v", resp)
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
			p := &Proxy{
				cfg: &config.Config{
					ReadOnly: tt.mode,
				},
				logger: logger,
				cache:  cache.New(time.Second),
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

	p := &Proxy{
		cfg: &config.Config{
			ReadOnly: config.ReadOnlyOn,
		},
		logger: logger,
		cache:  cache.New(time.Second),
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
