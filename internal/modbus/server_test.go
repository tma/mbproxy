package modbus

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// mockHandler implements Handler for testing
type mockHandler struct {
	response []byte
	err      error
}

func (h *mockHandler) HandleRequest(ctx context.Context, req *Request) ([]byte, error) {
	return h.response, h.err
}

func TestServer_AcceptConnections(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := &mockHandler{
		response: []byte{0x03, 0x02, 0x00, 0x01}, // Read holding registers response
	}

	server := NewServer(handler, logger)
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
	server := NewServer(nil, logger)

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
	binary.BigEndian.PutUint16(frame[0:2], txID)     // Transaction ID
	binary.BigEndian.PutUint16(frame[2:4], 0)        // Protocol ID (Modbus = 0)
	binary.BigEndian.PutUint16(frame[4:6], uint16(len(pdu)+1)) // Length
	frame[6] = slaveID
	copy(frame[7:], pdu)

	return frame
}
