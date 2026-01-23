// Package modbus provides a Modbus TCP server implementation.
package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Modbus function codes
const (
	FuncReadCoils            = 0x01
	FuncReadDiscreteInputs   = 0x02
	FuncReadHoldingRegisters = 0x03
	FuncReadInputRegisters   = 0x04
	FuncWriteSingleCoil      = 0x05
	FuncWriteSingleRegister  = 0x06
	FuncWriteMultipleCoils   = 0x0F
	FuncWriteMultipleRegs    = 0x10
)

// Modbus exception codes
const (
	ExcIllegalFunction = 0x01
	ExcIllegalAddress  = 0x02
	ExcIllegalValue    = 0x03
	ExcServerFailure   = 0x04
)

// MBAP header size (Modbus Application Protocol)
const mbapHeaderSize = 7

// Connection read timeout
const connReadTimeout = 60 * time.Second

// Request represents a Modbus request from a client.
type Request struct {
	TransactionID uint16
	SlaveID       byte
	FunctionCode  byte
	Address       uint16
	Quantity      uint16
	Data          []byte // For write operations
}

// Handler interface for processing Modbus requests.
type Handler interface {
	HandleRequest(ctx context.Context, req *Request) ([]byte, error)
}

// Server is a Modbus TCP server.
type Server struct {
	listener net.Listener
	handler  Handler
	logger   *slog.Logger

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	shutdown bool
	wg       sync.WaitGroup
}

// NewServer creates a new Modbus TCP server.
func NewServer(handler Handler, logger *slog.Logger) *Server {
	return &Server{
		handler: handler,
		logger:  logger,
		conns:   make(map[net.Conn]struct{}),
	}
}

// Listen starts listening on the specified address.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.listener = ln
	return nil
}

// Serve accepts and handles client connections.
func (s *Server) Serve(ctx context.Context) error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			shutdown := s.shutdown
			s.mu.Unlock()
			if shutdown {
				return nil
			}
			s.logger.Error("accept error", "error", err)
			continue
		}

		s.mu.Lock()
		if s.shutdown {
			s.mu.Unlock()
			conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go s.handleConn(ctx, conn)
	}
}

// Close stops accepting new connections and closes existing ones.
func (s *Server) Close() error {
	s.mu.Lock()
	s.shutdown = true
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()

	if s.listener != nil {
		s.listener.Close()
	}
	return nil
}

// Wait waits for all connections to finish.
func (s *Server) Wait() {
	s.wg.Wait()
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		s.wg.Done()
	}()

	s.logger.Debug("client connected", "remote", conn.RemoteAddr().String())

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Set read deadline to prevent slow/malicious clients from holding connections
		conn.SetReadDeadline(time.Now().Add(connReadTimeout))

		req, err := s.readRequest(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				s.logger.Debug("client disconnected", "remote", conn.RemoteAddr().String())
				return
			}
			// Check for timeout - this is normal, just disconnect silently
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				s.logger.Debug("client timeout", "remote", conn.RemoteAddr().String())
				return
			}
			s.logger.Error("read request error", "error", err, "remote", conn.RemoteAddr().String())
			return
		}

		resp, err := s.handler.HandleRequest(ctx, req)
		if err != nil {
			s.logger.Debug("handler error", "error", err, "func", fmt.Sprintf("0x%02X", req.FunctionCode))
			// Send exception response
			excResp := s.buildExceptionResponse(req, ExcServerFailure)
			s.writeResponse(conn, req.TransactionID, req.SlaveID, excResp)
			continue
		}

		if err := s.writeResponse(conn, req.TransactionID, req.SlaveID, resp); err != nil {
			s.logger.Error("write response error", "error", err)
			return
		}
	}
}

func (s *Server) readRequest(conn net.Conn) (*Request, error) {
	// Read MBAP header
	header := make([]byte, mbapHeaderSize)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}

	transactionID := binary.BigEndian.Uint16(header[0:2])
	protocolID := binary.BigEndian.Uint16(header[2:4])
	length := binary.BigEndian.Uint16(header[4:6])
	slaveID := header[6]

	// Protocol ID must be 0 for Modbus TCP
	if protocolID != 0 {
		return nil, fmt.Errorf("invalid protocol id: %d", protocolID)
	}

	// Read PDU (Protocol Data Unit)
	pduLen := int(length) - 1 // Subtract 1 for slave ID already read
	if pduLen < 1 || pduLen > 253 {
		return nil, fmt.Errorf("invalid pdu length: %d", pduLen)
	}

	pdu := make([]byte, pduLen)
	if _, err := io.ReadFull(conn, pdu); err != nil {
		return nil, err
	}

	req := &Request{
		TransactionID: transactionID,
		SlaveID:       slaveID,
		FunctionCode:  pdu[0],
	}

	// Parse based on function code
	if err := s.parsePDU(req, pdu); err != nil {
		return nil, err
	}

	return req, nil
}

func (s *Server) parsePDU(req *Request, pdu []byte) error {
	switch req.FunctionCode {
	case FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters:
		if len(pdu) < 5 {
			return fmt.Errorf("pdu too short for read request")
		}
		req.Address = binary.BigEndian.Uint16(pdu[1:3])
		req.Quantity = binary.BigEndian.Uint16(pdu[3:5])

	case FuncWriteSingleCoil, FuncWriteSingleRegister:
		if len(pdu) < 5 {
			return fmt.Errorf("pdu too short for write single request")
		}
		req.Address = binary.BigEndian.Uint16(pdu[1:3])
		req.Quantity = 1
		req.Data = pdu[3:5]

	case FuncWriteMultipleCoils, FuncWriteMultipleRegs:
		if len(pdu) < 6 {
			return fmt.Errorf("pdu too short for write multiple request")
		}
		req.Address = binary.BigEndian.Uint16(pdu[1:3])
		req.Quantity = binary.BigEndian.Uint16(pdu[3:5])
		byteCount := pdu[5]
		if len(pdu) < 6+int(byteCount) {
			return fmt.Errorf("pdu too short for write data")
		}
		req.Data = pdu[6 : 6+byteCount]

	default:
		// Unknown function code - let handler deal with it
	}

	return nil
}

func (s *Server) writeResponse(conn net.Conn, transactionID uint16, slaveID byte, pdu []byte) error {
	// Build MBAP header + PDU
	resp := make([]byte, mbapHeaderSize+len(pdu))

	binary.BigEndian.PutUint16(resp[0:2], transactionID)
	binary.BigEndian.PutUint16(resp[2:4], 0) // Protocol ID
	binary.BigEndian.PutUint16(resp[4:6], uint16(len(pdu)+1))
	resp[6] = slaveID
	copy(resp[7:], pdu)

	_, err := conn.Write(resp)
	return err
}

func (s *Server) buildExceptionResponse(req *Request, exceptionCode byte) []byte {
	return []byte{req.FunctionCode | 0x80, exceptionCode}
}

// BuildExceptionResponse creates an exception response PDU.
func BuildExceptionResponse(functionCode byte, exceptionCode byte) []byte {
	return []byte{functionCode | 0x80, exceptionCode}
}

// IsWriteFunction returns true if the function code is a write operation.
func IsWriteFunction(fc byte) bool {
	switch fc {
	case FuncWriteSingleCoil, FuncWriteSingleRegister, FuncWriteMultipleCoils, FuncWriteMultipleRegs:
		return true
	default:
		return false
	}
}

// IsReadFunction returns true if the function code is a read operation.
func IsReadFunction(fc byte) bool {
	switch fc {
	case FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters:
		return true
	default:
		return false
	}
}
