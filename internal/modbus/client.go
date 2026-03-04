package modbus

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/grid-x/modbus"
)

// Client wraps a Modbus TCP client with auto-reconnect capability.
type Client struct {
	address      string
	timeout      time.Duration
	requestDelay time.Duration
	connectDelay time.Duration
	logger       *slog.Logger

	mu     sync.Mutex
	client modbus.Client
	conn   *modbus.TCPClientHandler

	healthMu  sync.RWMutex
	lastErr   error
	connected bool
}

// NewClient creates a new Modbus TCP client.
func NewClient(address string, timeout, requestDelay, connectDelay time.Duration, logger *slog.Logger) *Client {
	return &Client{
		address:      address,
		timeout:      timeout,
		requestDelay: requestDelay,
		connectDelay: connectDelay,
		logger:       logger,
	}
}

// Connect establishes a connection to the upstream Modbus device.
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.connectLocked(context.Background())
}

func (c *Client) connectLocked(ctx context.Context) error {
	if c.conn != nil {
		c.conn.Close()
	}

	// Custom dialer with TCP keep-alive for connection health monitoring
	dialer := &net.Dialer{
		Timeout:   c.timeout,
		KeepAlive: 30 * time.Second,
	}

	handler := modbus.NewTCPClientHandler(c.address, modbus.WithDialer(dialer.DialContext))
	handler.Timeout = c.timeout
	handler.IdleTimeout = c.timeout
	handler.ConnectDelay = c.connectDelay

	if err := handler.Connect(ctx); err != nil {
		c.healthMu.Lock()
		c.connected = false
		c.lastErr = fmt.Errorf("connect to %s: %w", c.address, err)
		c.healthMu.Unlock()
		return c.lastErr
	}

	c.conn = handler
	c.client = modbus.NewClient(handler)

	c.healthMu.Lock()
	c.connected = true
	c.lastErr = nil
	c.healthMu.Unlock()

	if c.connectDelay > 0 {
		c.logger.Debug("applying connect delay", "delay", c.connectDelay)
	}

	c.logger.Info("connected to upstream", "address", c.address)
	return nil
}

// Close closes the connection to the upstream device.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.client = nil
	}

	c.healthMu.Lock()
	c.connected = false
	c.healthMu.Unlock()

	return nil
}

// Healthy reports whether the upstream connection is healthy.
// It returns the last observed error, or nil if the last operation succeeded.
func (c *Client) Healthy() error {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()

	if !c.connected {
		if c.lastErr != nil {
			return c.lastErr
		}
		return fmt.Errorf("not connected")
	}
	return c.lastErr
}

// Execute sends a Modbus request and returns the response.
// It automatically reconnects on connection failure.
func (c *Client) Execute(ctx context.Context, req *Request) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Ensure connected
	if c.conn == nil {
		if err := c.connectLocked(ctx); err != nil {
			return nil, err
		}
	}

	// Set slave ID
	c.conn.SlaveID = req.SlaveID

	// Execute request and measure time
	start := time.Now()
	resp, err := c.executeRequest(ctx, req)
	if err != nil {
		// Try reconnect once
		c.logger.Debug("upstream request failed, reconnecting", "error", err)
		if reconnErr := c.connectLocked(ctx); reconnErr != nil {
			return nil, fmt.Errorf("reconnect failed: %w", reconnErr)
		}
		c.conn.SlaveID = req.SlaveID
		start = time.Now() // Reset timer for retry
		resp, err = c.executeRequest(ctx, req)
		if err != nil {
			c.healthMu.Lock()
			c.lastErr = err
			c.healthMu.Unlock()
			return nil, err
		}
	}
	duration := time.Since(start)

	c.healthMu.Lock()
	c.lastErr = nil
	c.healthMu.Unlock()

	c.logger.Debug("upstream request completed",
		"slave_id", req.SlaveID,
		"func", fmt.Sprintf("0x%02X", req.FunctionCode),
		"addr", req.Address,
		"qty", req.Quantity,
		"duration", duration,
	)

	// Apply request delay if configured (only after successful requests)
	if c.requestDelay > 0 {
		c.logger.Debug("applying request delay", "delay", c.requestDelay)
		select {
		case <-time.After(c.requestDelay):
		case <-ctx.Done():
			// Context cancelled during delay - still return the successful result
		}
	}

	return resp, nil
}

func (c *Client) executeRequest(ctx context.Context, req *Request) ([]byte, error) {
	switch req.FunctionCode {
	case FuncReadCoils:
		results, err := c.client.ReadCoils(ctx, req.Address, req.Quantity)
		if err != nil {
			return nil, err
		}
		return c.buildReadResponse(req.FunctionCode, results), nil

	case FuncReadDiscreteInputs:
		results, err := c.client.ReadDiscreteInputs(ctx, req.Address, req.Quantity)
		if err != nil {
			return nil, err
		}
		return c.buildReadResponse(req.FunctionCode, results), nil

	case FuncReadHoldingRegisters:
		results, err := c.client.ReadHoldingRegisters(ctx, req.Address, req.Quantity)
		if err != nil {
			return nil, err
		}
		return c.buildReadResponse(req.FunctionCode, results), nil

	case FuncReadInputRegisters:
		results, err := c.client.ReadInputRegisters(ctx, req.Address, req.Quantity)
		if err != nil {
			return nil, err
		}
		return c.buildReadResponse(req.FunctionCode, results), nil

	case FuncWriteSingleCoil:
		value := binary.BigEndian.Uint16(req.Data)
		results, err := c.client.WriteSingleCoil(ctx, req.Address, value)
		if err != nil {
			return nil, err
		}
		return c.buildWriteResponse(req.FunctionCode, req.Address, results), nil

	case FuncWriteSingleRegister:
		value := binary.BigEndian.Uint16(req.Data)
		results, err := c.client.WriteSingleRegister(ctx, req.Address, value)
		if err != nil {
			return nil, err
		}
		return c.buildWriteResponse(req.FunctionCode, req.Address, results), nil

	case FuncWriteMultipleCoils:
		results, err := c.client.WriteMultipleCoils(ctx, req.Address, req.Quantity, req.Data)
		if err != nil {
			return nil, err
		}
		return c.buildWriteResponse(req.FunctionCode, req.Address, results), nil

	case FuncWriteMultipleRegs:
		results, err := c.client.WriteMultipleRegisters(ctx, req.Address, req.Quantity, req.Data)
		if err != nil {
			return nil, err
		}
		return c.buildWriteResponse(req.FunctionCode, req.Address, results), nil

	default:
		return nil, fmt.Errorf("unsupported function code: 0x%02X", req.FunctionCode)
	}
}

func (c *Client) buildReadResponse(funcCode byte, data []byte) []byte {
	resp := make([]byte, 2+len(data))
	resp[0] = funcCode
	resp[1] = byte(len(data))
	copy(resp[2:], data)
	return resp
}

func (c *Client) buildWriteResponse(funcCode byte, address uint16, data []byte) []byte {
	resp := make([]byte, 5)
	resp[0] = funcCode
	binary.BigEndian.PutUint16(resp[1:3], address)
	if len(data) >= 2 {
		copy(resp[3:5], data[:2])
	}
	return resp
}
