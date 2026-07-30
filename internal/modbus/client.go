package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	gridmodbus "github.com/grid-x/modbus"
)

type requestClient interface {
	ReadCoils(context.Context, uint16, uint16) ([]byte, error)
	ReadDiscreteInputs(context.Context, uint16, uint16) ([]byte, error)
	ReadHoldingRegisters(context.Context, uint16, uint16) ([]byte, error)
	ReadInputRegisters(context.Context, uint16, uint16) ([]byte, error)
	WriteSingleCoil(context.Context, uint16, uint16) ([]byte, error)
	WriteSingleRegister(context.Context, uint16, uint16) ([]byte, error)
	WriteMultipleCoils(context.Context, uint16, uint16, []byte) ([]byte, error)
	WriteMultipleRegisters(context.Context, uint16, uint16, []byte) ([]byte, error)
}

type clientSession interface {
	Connect(context.Context) error
	Close() error
	SetSlave(byte)
	BeginRequest(context.Context, time.Duration) func() error
}

type sessionFactory func() (clientSession, requestClient)

type guardedConn struct {
	net.Conn

	mu           sync.Mutex
	ctx          context.Context
	maxDeadline  time.Time
	canceledErr  error
	writeStarted bool
}

func newGuardedConn(conn net.Conn) *guardedConn {
	return &guardedConn{Conn: conn}
}

func (c *guardedConn) bind(ctx context.Context, maxDeadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ctx = ctx
	c.maxDeadline = maxDeadline
	c.canceledErr = contextError(ctx)
	c.writeStarted = false
	return c.Conn.SetDeadline(c.clampDeadlineLocked(maxDeadline))
}

func (c *guardedConn) unbind() {
	c.mu.Lock()
	c.ctx = nil
	c.maxDeadline = time.Time{}
	c.canceledErr = nil
	c.writeStarted = false
	c.mu.Unlock()
}

func (c *guardedConn) cancel(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ctx == nil {
		return nil
	}
	c.canceledErr = err
	return c.Conn.SetDeadline(time.Now())
}

func (c *guardedConn) requestErrorLocked() error {
	if c.ctx == nil {
		return nil
	}
	if c.canceledErr != nil {
		return c.canceledErr
	}
	if err := contextError(c.ctx); err != nil {
		return err
	}
	if !c.maxDeadline.IsZero() && !time.Now().Before(c.maxDeadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *guardedConn) clampDeadlineLocked(deadline time.Time) time.Time {
	if c.requestErrorLocked() != nil {
		return time.Unix(1, 0)
	}
	if c.ctx != nil && (deadline.IsZero() || deadline.After(c.maxDeadline)) {
		return c.maxDeadline
	}
	return deadline
}

func (c *guardedConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.SetDeadline(c.clampDeadlineLocked(deadline))
}

func (c *guardedConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.SetReadDeadline(c.clampDeadlineLocked(deadline))
}

func (c *guardedConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.SetWriteDeadline(c.clampDeadlineLocked(deadline))
}

func (c *guardedConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	if err := c.requestErrorLocked(); err != nil {
		c.mu.Unlock()
		return 0, err
	}
	c.writeStarted = true
	conn := c.Conn
	c.mu.Unlock()
	return conn.Write(data)
}

type tcpSession struct {
	handler *gridmodbus.TCPClientHandler
	mu      sync.Mutex
	conn    *guardedConn
}

func newTCPSession(address string, attemptTimeout, connectDelay time.Duration) (*tcpSession, requestClient) {
	session := &tcpSession{}
	dialer := &net.Dialer{
		Timeout:   attemptTimeout,
		KeepAlive: 30 * time.Second,
	}
	handler := gridmodbus.NewTCPClientHandler(address, gridmodbus.WithDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		netConn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		guardedConn := newGuardedConn(netConn)
		session.mu.Lock()
		session.conn = guardedConn
		session.mu.Unlock()
		return guardedConn, nil
	}))
	handler.Timeout = attemptTimeout
	// mbproxy owns reconnect decisions so a hidden idle reconnect cannot escape
	// the active request context or retry policy.
	handler.IdleTimeout = -1
	handler.ConnectDelay = connectDelay
	session.handler = handler
	return session, gridmodbus.NewClient(handler)
}

func (c *tcpSession) Connect(ctx context.Context) error {
	return c.handler.Connect(ctx)
}

func (c *tcpSession) Close() error {
	err := c.handler.Close()
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	return err
}

func (c *tcpSession) SetSlave(slaveID byte) {
	c.handler.SetSlave(slaveID)
}

func (c *tcpSession) BeginRequest(ctx context.Context, attemptTimeout time.Duration) func() error {
	c.handler.Timeout = attemptTimeout

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return func() error { return nil }
	}

	maxDeadline := time.Now().Add(attemptTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(maxDeadline) {
		maxDeadline = deadline
	}
	bindErr := conn.bind(ctx, maxDeadline)
	stop := make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			stopped <- conn.cancel(ctx.Err())
		case <-stop:
			stopped <- nil
		}
	}()

	return func() error {
		close(stop)
		watchdogErr := <-stopped
		conn.unbind()
		return errors.Join(bindErr, watchdogErr)
	}
}

// Client wraps a Modbus TCP client with classified retry behavior.
type Client struct {
	address        string
	attemptTimeout time.Duration
	requestDelay   time.Duration
	connectDelay   time.Duration
	logger         *slog.Logger

	owner         chan struct{}
	session       clientSession
	client        requestClient
	newSession    sessionFactory
	nextRequestAt time.Time

	healthMu  sync.RWMutex
	lastErr   error
	connected bool
}

// NewClient creates a new Modbus TCP client.
func NewClient(address string, attemptTimeout, requestDelay, connectDelay time.Duration, logger *slog.Logger) *Client {
	c := &Client{
		address:        address,
		attemptTimeout: attemptTimeout,
		requestDelay:   requestDelay,
		connectDelay:   connectDelay,
		logger:         logger,
		owner:          make(chan struct{}, 1),
	}
	c.owner <- struct{}{}
	c.newSession = func() (clientSession, requestClient) {
		return newTCPSession(address, attemptTimeout, connectDelay)
	}
	return c
}

// Connect establishes a connection to the upstream Modbus device.
func (c *Client) Connect() error {
	if err := c.acquire(context.Background()); err != nil {
		return err
	}
	defer c.release()
	return c.connectLocked(context.Background())
}

func (c *Client) connectLocked(ctx context.Context) error {
	if c.session != nil {
		if err := c.session.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			c.logger.Debug("closing old upstream connection failed", "error", err)
		}
	}

	session, client := c.newSession()
	if err := session.Connect(ctx); err != nil {
		wrapped := fmt.Errorf("connect to %s: %w", c.address, err)
		if closeErr := session.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			c.logger.Debug("closing failed connection attempt failed", "error", closeErr)
		}
		c.healthMu.Lock()
		c.connected = false
		c.lastErr = wrapped
		c.healthMu.Unlock()
		c.session = nil
		c.client = nil
		return wrapped
	}

	c.session = session
	c.client = client
	c.healthMu.Lock()
	c.connected = true
	c.lastErr = nil
	c.healthMu.Unlock()

	if c.connectDelay > 0 {
		c.logger.Debug("applied connect delay", "delay", c.connectDelay)
	}
	c.logger.Info("connected to upstream", "address", c.address)
	return nil
}

func (c *Client) disconnectLocked() {
	if c.session != nil {
		if err := c.session.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			c.logger.Debug("closing failed upstream connection failed", "error", err)
		}
	}
	c.session = nil
	c.client = nil
	c.healthMu.Lock()
	c.connected = false
	c.healthMu.Unlock()
}

// Close closes the connection to the upstream device.
func (c *Client) Close() error {
	if err := c.acquire(context.Background()); err != nil {
		return err
	}
	defer c.release()

	var err error
	if c.session != nil {
		err = c.session.Close()
		c.session = nil
		c.client = nil
	}
	c.healthMu.Lock()
	c.connected = false
	c.healthMu.Unlock()
	return err
}

func (c *Client) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.owner:
		if err := ctx.Err(); err != nil {
			c.release()
			return err
		}
		return nil
	}
}

func (c *Client) release() {
	c.owner <- struct{}{}
}

// Healthy reports the last observed upstream state.
func (c *Client) Healthy() error {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()
	if c.lastErr != nil {
		return c.lastErr
	}
	if !c.connected {
		return fmt.Errorf("not connected")
	}
	return nil
}

// Execute sends one Modbus request, retrying a read once only after a transport failure.
func (c *Client) Execute(ctx context.Context, req *Request) ([]byte, error) {
	if err := ValidateRequest(req); err != nil {
		return nil, requestError(err, 0)
	}

	if err := c.acquire(ctx); err != nil {
		return nil, requestError(err, 0)
	}
	defer c.release()

	var lastErr error
	requestSlotReady := false
	for attempt := 1; attempt <= 2; attempt++ {
		if c.session == nil {
			err := c.connectLocked(ctx)
			if err != nil {
				lastErr = err
				willRetry := attempt == 1 && IsReadFunction(req.FunctionCode) && isTransportError(err) && contextError(ctx) == nil
				if willRetry {
					c.logger.Debug("upstream connect failed, retrying read", "error", err)
					continue
				}
				return nil, c.finalError(lastErr, attempt)
			}
		}

		if !requestSlotReady {
			if err := c.waitForRequestSlot(ctx); err != nil {
				return nil, c.finalError(err, attempt-1)
			}
			requestSlotReady = true
		}
		if err := contextError(ctx); err != nil {
			return nil, c.finalError(err, attempt-1)
		}
		c.session.SetSlave(req.SlaveID)
		if err := contextError(ctx); err != nil {
			return nil, c.finalError(err, attempt-1)
		}
		attemptTimeout := c.socketTimeoutFor(ctx)
		finishRequest := c.session.BeginRequest(ctx, attemptTimeout)
		if err := contextError(ctx); err != nil {
			if finishErr := finishRequest(); finishErr != nil {
				c.logger.Debug("finishing canceled upstream request failed", "error", finishErr)
			}
			return nil, c.finalError(err, attempt-1)
		}
		attemptStart := time.Now()
		resp, err := c.executeRequest(ctx, req)
		responseCompletedAt := time.Now()
		attemptDuration := time.Since(attemptStart)
		if finishErr := finishRequest(); finishErr != nil {
			c.logger.Debug("finishing upstream request failed", "error", finishErr)
		}
		err = classifyRequestPathError(req, err)
		kind, _ := classifyError(err)
		if err != nil && kind != ErrorProtocolException {
			if ctxErr := contextError(ctx); ctxErr != nil {
				err = ctxErr
			}
		}

		if err == nil {
			if c.requestDelay > 0 {
				c.nextRequestAt = responseCompletedAt.Add(c.requestDelay)
			}
			if ctxErr := contextError(ctx); ctxErr != nil {
				return nil, c.finalError(ctxErr, attempt)
			}
			c.recordSuccess()
			c.logger.Debug("upstream request completed",
				"slave_id", req.SlaveID,
				"func", fmt.Sprintf("0x%02X", req.FunctionCode),
				"addr", req.Address,
				"qty", req.Quantity,
				"duration", attemptDuration,
			)
			return resp, nil
		}

		lastErr = err
		kind, _ = classifyError(err)
		willRetry := attempt == 1 &&
			IsReadFunction(req.FunctionCode) &&
			(kind == ErrorTransportTimeout || kind == ErrorTransportClosed) &&
			contextError(ctx) == nil

		if willRetry {
			c.disconnectLocked()
			c.logger.Debug("upstream request failed, retrying read", "error", err)
			continue
		}
		if kind != ErrorProtocolException {
			c.disconnectLocked()
		}
		return nil, c.finalError(lastErr, attempt)
	}

	return nil, c.finalError(lastErr, 2)
}

func (c *Client) waitForRequestSlot(ctx context.Context) error {
	delay := time.Until(c.nextRequestAt)
	if delay <= 0 {
		return contextError(ctx)
	}

	c.logger.Debug("waiting for upstream request slot", "delay", delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return contextError(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func classifyRequestPathError(req *Request, err error) error {
	if err == nil {
		return nil
	}

	var mbErr *gridmodbus.Error
	if errors.As(err, &mbErr) {
		if mbErr.FunctionCode == req.FunctionCode|0x80 && mbErr.ExceptionCode != 0 {
			return err
		}
		return &upstreamCommunicationError{err: err}
	}

	kind, _ := classifyError(err)
	if kind == ErrorLocal {
		return &upstreamCommunicationError{err: err}
	}
	return err
}

func (c *Client) socketTimeoutFor(ctx context.Context) time.Duration {
	timeout := c.attemptTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return time.Nanosecond
	}
	return timeout
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *Client) finalError(err error, attempts int) error {
	reqErr := requestError(err, attempts)
	if reqErr.Kind == ErrorProtocolException {
		c.healthMu.Lock()
		c.lastErr = nil
		c.connected = true
		c.healthMu.Unlock()
	} else if reqErr.Kind != ErrorContextCanceled {
		c.healthMu.Lock()
		c.lastErr = reqErr
		c.healthMu.Unlock()
	}
	return reqErr
}

func (c *Client) recordSuccess() {
	c.healthMu.Lock()
	c.lastErr = nil
	c.connected = true
	c.healthMu.Unlock()
}

func isTransportError(err error) bool {
	kind, _ := classifyError(err)
	return kind == ErrorTransportTimeout || kind == ErrorTransportClosed
}

// ValidateRequest checks local Modbus constraints without contacting upstream.
func ValidateRequest(req *Request) error {
	if req == nil {
		return fmt.Errorf("request is nil")
	}
	switch req.FunctionCode {
	case FuncReadCoils, FuncReadDiscreteInputs:
		if req.Quantity < 1 || req.Quantity > 2000 {
			return newValidationError(ExcIllegalValue, "quantity %d must be between 1 and 2000", req.Quantity)
		}
	case FuncReadHoldingRegisters, FuncReadInputRegisters:
		if req.Quantity < 1 || req.Quantity > 125 {
			return newValidationError(ExcIllegalValue, "quantity %d must be between 1 and 125", req.Quantity)
		}
	case FuncWriteSingleCoil:
		if len(req.Data) < 2 {
			return newValidationError(ExcIllegalValue, "write data requires 2 bytes")
		}
		value := binary.BigEndian.Uint16(req.Data)
		if value != 0x0000 && value != 0xFF00 {
			return newValidationError(ExcIllegalValue, "coil value 0x%04X must be 0x0000 or 0xFF00", value)
		}
	case FuncWriteSingleRegister:
		if len(req.Data) < 2 {
			return newValidationError(ExcIllegalValue, "write data requires 2 bytes")
		}
	case FuncWriteMultipleCoils:
		if req.Quantity < 1 || req.Quantity > 1968 {
			return newValidationError(ExcIllegalValue, "quantity %d must be between 1 and 1968", req.Quantity)
		}
		expected := int(req.Quantity+7) / 8
		if len(req.Data) != expected {
			return newValidationError(ExcIllegalValue, "write data has %d bytes, expected %d", len(req.Data), expected)
		}
	case FuncWriteMultipleRegs:
		if req.Quantity < 1 || req.Quantity > 123 {
			return newValidationError(ExcIllegalValue, "quantity %d must be between 1 and 123", req.Quantity)
		}
		expected := int(req.Quantity) * 2
		if len(req.Data) != expected {
			return newValidationError(ExcIllegalValue, "write data has %d bytes, expected %d", len(req.Data), expected)
		}
	default:
		return newValidationError(ExcIllegalFunction, "unsupported function code: 0x%02X", req.FunctionCode)
	}
	if uint32(req.Address)+uint32(req.Quantity) > 65536 {
		return newValidationError(ExcIllegalAddress, "address range exceeds 0xFFFF")
	}
	return nil
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
