// Package proxy implements the Modbus caching proxy.
package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/tma/mbproxy/internal/cache"
	"github.com/tma/mbproxy/internal/config"
	"github.com/tma/mbproxy/internal/modbus"
)

// Proxy is a caching Modbus proxy server.
type Proxy struct {
	cfg    *config.Config
	logger *slog.Logger
	server *modbus.Server
	client *modbus.Client
	cache  *cache.Cache
}

// New creates a new proxy instance.
func New(cfg *config.Config, logger *slog.Logger) (*Proxy, error) {
	p := &Proxy{
		cfg:    cfg,
		logger: logger,
		client: modbus.NewClient(cfg.Upstream, cfg.Timeout, cfg.RequestDelay, cfg.ConnectDelay, logger),
		cache:  cache.New(cfg.CacheTTL, cfg.CacheServeStale),
	}

	p.server = modbus.NewServer(p, logger)

	return p, nil
}

// Healthy reports whether the proxy's upstream connection is healthy.
func (p *Proxy) Healthy() error {
	return p.client.Healthy()
}

// Run starts the proxy server.
func (p *Proxy) Run(ctx context.Context) error {
	p.logger.Info("starting proxy",
		"listen", p.cfg.Listen,
		"upstream", p.cfg.Upstream,
		"readonly", p.cfg.ReadOnly,
		"cache_ttl", p.cfg.CacheTTL,
	)

	if err := p.server.Listen(p.cfg.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Connect to upstream
	if err := p.client.Connect(); err != nil {
		p.logger.Warn("initial upstream connection failed, will retry on first request", "error", err)
	}

	return p.server.Serve(ctx)
}

// Shutdown gracefully shuts down the proxy.
func (p *Proxy) Shutdown(timeout time.Duration) error {
	p.logger.Info("shutting down", "timeout", timeout)

	// Stop accepting new connections
	p.server.Close()

	// Wait for in-flight requests with timeout
	done := make(chan struct{})
	go func() {
		p.server.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Debug("all connections closed")
	case <-time.After(timeout):
		p.logger.Warn("shutdown timeout, forcing close")
	}

	// Close cache cleanup goroutine
	p.cache.Close()

	// Close upstream connection
	return p.client.Close()
}

// HandleRequest implements modbus.Handler interface.
func (p *Proxy) HandleRequest(ctx context.Context, req *modbus.Request) ([]byte, error) {
	if modbus.IsWriteFunction(req.FunctionCode) {
		return p.handleWrite(ctx, req)
	}

	if modbus.IsReadFunction(req.FunctionCode) {
		return p.handleRead(ctx, req)
	}

	// Unknown function code
	p.logger.Debug("unknown function code",
		"func", fmt.Sprintf("0x%02X", req.FunctionCode),
		"slave_id", req.SlaveID,
	)
	return modbus.BuildExceptionResponse(req.FunctionCode, modbus.ExcIllegalFunction), nil
}

func (p *Proxy) handleRead(ctx context.Context, req *modbus.Request) ([]byte, error) {
	// Check per-register cache
	values, cacheHit := p.cache.GetRange(req.SlaveID, req.FunctionCode, req.Address, req.Quantity)
	if cacheHit {
		p.logger.Debug("cache hit",
			"slave_id", req.SlaveID,
			"func", fmt.Sprintf("0x%02X", req.FunctionCode),
			"addr", req.Address,
			"qty", req.Quantity,
		)
		return assembleResponse(req.FunctionCode, req.Quantity, values), nil
	}

	// Cache miss — fetch with coalescing
	p.logger.Debug("cache miss",
		"slave_id", req.SlaveID,
		"func", fmt.Sprintf("0x%02X", req.FunctionCode),
		"addr", req.Address,
		"qty", req.Quantity,
	)

	rangeKey := cache.RangeKey(req.SlaveID, req.FunctionCode, req.Address, req.Quantity)
	data, err := p.cache.Coalesce(ctx, rangeKey, func(ctx context.Context) ([]byte, error) {
		return p.client.Execute(ctx, req)
	})

	if err != nil {
		// Try serving stale data if configured
		if p.cfg.CacheServeStale {
			if staleValues, ok := p.cache.GetRangeStale(req.SlaveID, req.FunctionCode, req.Address, req.Quantity); ok {
				p.logger.Warn("upstream error, serving stale",
					"slave_id", req.SlaveID,
					"error", err,
				)
				return assembleResponse(req.FunctionCode, req.Quantity, staleValues), nil
			}
		}
		return nil, err
	}

	// Decompose response and store per-register
	regValues := decomposeResponse(req.FunctionCode, req.Quantity, data)
	if regValues != nil {
		p.cache.SetRange(req.SlaveID, req.FunctionCode, req.Address, regValues)
	}

	return data, nil
}

func (p *Proxy) handleWrite(ctx context.Context, req *modbus.Request) ([]byte, error) {
	switch p.cfg.ReadOnly {
	case config.ReadOnlyOn:
		// Silently ignore, return success response
		p.logger.Debug("write ignored (readonly mode)",
			"slave_id", req.SlaveID,
			"func", fmt.Sprintf("0x%02X", req.FunctionCode),
			"addr", req.Address,
		)
		return p.buildFakeWriteResponse(req), nil

	case config.ReadOnlyDeny:
		// Reject with exception
		p.logger.Debug("write denied (readonly mode)",
			"slave_id", req.SlaveID,
			"func", fmt.Sprintf("0x%02X", req.FunctionCode),
			"addr", req.Address,
		)
		return modbus.BuildExceptionResponse(req.FunctionCode, modbus.ExcIllegalFunction), nil

	case config.ReadOnlyOff:
		// Forward to upstream
		resp, err := p.client.Execute(ctx, req)
		if err != nil {
			return nil, err
		}

		// Invalidate per-register cache entries for the written range
		p.invalidateCache(req)

		return resp, nil
	}

	return nil, fmt.Errorf("unknown readonly mode: %s", p.cfg.ReadOnly)
}

func (p *Proxy) invalidateCache(req *modbus.Request) {
	// Invalidate per-register entries for all read function codes
	readFuncs := []byte{
		modbus.FuncReadCoils,
		modbus.FuncReadDiscreteInputs,
		modbus.FuncReadHoldingRegisters,
		modbus.FuncReadInputRegisters,
	}

	for _, fc := range readFuncs {
		p.cache.DeleteRange(req.SlaveID, fc, req.Address, req.Quantity)
	}
}

// decomposeResponse extracts per-register/coil values from a Modbus read response.
// Response format: [funcCode, byteCount, data...]
// For registers (FC 0x03, 0x04): each register is 2 bytes.
// For coils/discrete inputs (FC 0x01, 0x02): each coil is 1 bit, stored as 1 byte (0 or 1).
func decomposeResponse(functionCode byte, quantity uint16, data []byte) [][]byte {
	if len(data) < 2 {
		return nil
	}

	payload := data[2:] // Skip funcCode and byteCount

	switch functionCode {
	case modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters:
		values := make([][]byte, quantity)
		for i := uint16(0); i < quantity; i++ {
			offset := i * 2
			if int(offset+2) > len(payload) {
				return nil
			}
			reg := make([]byte, 2)
			copy(reg, payload[offset:offset+2])
			values[i] = reg
		}
		return values

	case modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs:
		values := make([][]byte, quantity)
		for i := uint16(0); i < quantity; i++ {
			byteIdx := i / 8
			bitIdx := i % 8
			if int(byteIdx) >= len(payload) {
				return nil
			}
			if payload[byteIdx]&(1<<bitIdx) != 0 {
				values[i] = []byte{1}
			} else {
				values[i] = []byte{0}
			}
		}
		return values
	}

	return nil
}

// assembleResponse reconstructs a Modbus read response from per-register/coil values.
func assembleResponse(functionCode byte, quantity uint16, values [][]byte) []byte {
	switch functionCode {
	case modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters:
		byteCount := quantity * 2
		resp := make([]byte, 2+byteCount)
		resp[0] = functionCode
		resp[1] = byte(byteCount)
		for i, v := range values {
			if len(v) >= 2 {
				resp[2+i*2] = v[0]
				resp[2+i*2+1] = v[1]
			}
		}
		return resp

	case modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs:
		byteCount := (quantity + 7) / 8
		resp := make([]byte, 2+byteCount)
		resp[0] = functionCode
		resp[1] = byte(byteCount)
		for i, v := range values {
			if len(v) > 0 && v[0] != 0 {
				byteIdx := i / 8
				bitIdx := uint(i % 8)
				resp[2+byteIdx] |= 1 << bitIdx
			}
		}
		return resp
	}

	return nil
}

func (p *Proxy) buildFakeWriteResponse(req *modbus.Request) []byte {
	switch req.FunctionCode {
	case modbus.FuncWriteSingleCoil, modbus.FuncWriteSingleRegister:
		// Echo back the request: func + address + value
		resp := make([]byte, 5)
		resp[0] = req.FunctionCode
		binary.BigEndian.PutUint16(resp[1:3], req.Address)
		if len(req.Data) >= 2 {
			copy(resp[3:5], req.Data[:2])
		}
		return resp

	case modbus.FuncWriteMultipleCoils, modbus.FuncWriteMultipleRegs:
		// Return: func + address + quantity
		resp := make([]byte, 5)
		resp[0] = req.FunctionCode
		binary.BigEndian.PutUint16(resp[1:3], req.Address)
		binary.BigEndian.PutUint16(resp[3:5], req.Quantity)
		return resp

	default:
		// Should not happen, but return empty success
		return []byte{req.FunctionCode}
	}
}
