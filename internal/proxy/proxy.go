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
		client: modbus.NewClient(cfg.Upstream, cfg.Timeout, logger),
		cache:  cache.New(cfg.CacheTTL),
	}

	p.server = modbus.NewServer(p, logger)

	return p, nil
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
	start := time.Now()

	if modbus.IsWriteFunction(req.FunctionCode) {
		return p.handleWrite(ctx, req, start)
	}

	if modbus.IsReadFunction(req.FunctionCode) {
		return p.handleRead(ctx, req, start)
	}

	// Unknown function code
	p.logger.Debug("unknown function code",
		"func", fmt.Sprintf("0x%02X", req.FunctionCode),
		"slave_id", req.SlaveID,
	)
	return modbus.BuildExceptionResponse(req.FunctionCode, modbus.ExcIllegalFunction), nil
}

func (p *Proxy) handleRead(ctx context.Context, req *modbus.Request, start time.Time) ([]byte, error) {
	key := cache.Key(req.SlaveID, req.FunctionCode, req.Address, req.Quantity)

	// Use GetOrFetch for request coalescing
	data, cacheHit, err := p.cache.GetOrFetch(ctx, key, func(ctx context.Context) ([]byte, error) {
		p.logger.Debug("cache miss",
			"slave_id", req.SlaveID,
			"func", fmt.Sprintf("0x%02X", req.FunctionCode),
			"addr", req.Address,
			"qty", req.Quantity,
		)

		resp, err := p.client.Execute(ctx, req)
		if err != nil {
			return nil, err
		}

		p.logger.Debug("upstream read",
			"slave_id", req.SlaveID,
			"func", fmt.Sprintf("0x%02X", req.FunctionCode),
			"addr", req.Address,
			"qty", req.Quantity,
			"duration", time.Since(start),
		)

		return resp, nil
	})

	if err != nil {
		// Try serving stale data if configured
		if p.cfg.CacheServeStale {
			if stale, ok := p.cache.GetStale(key); ok {
				p.logger.Warn("upstream error, serving stale",
					"slave_id", req.SlaveID,
					"error", err,
				)
				return stale, nil
			}
		}
		return nil, err
	}

	if cacheHit {
		p.logger.Debug("cache hit",
			"slave_id", req.SlaveID,
			"func", fmt.Sprintf("0x%02X", req.FunctionCode),
			"addr", req.Address,
			"qty", req.Quantity,
		)
	}

	return data, nil
}

func (p *Proxy) handleWrite(ctx context.Context, req *modbus.Request, start time.Time) ([]byte, error) {
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

		p.logger.Debug("upstream write",
			"slave_id", req.SlaveID,
			"func", fmt.Sprintf("0x%02X", req.FunctionCode),
			"addr", req.Address,
			"qty", req.Quantity,
			"duration", time.Since(start),
		)

		// Invalidate exact matching cache entries for all read function codes
		p.invalidateCache(req)

		return resp, nil
	}

	return nil, fmt.Errorf("unknown readonly mode: %s", p.cfg.ReadOnly)
}

func (p *Proxy) invalidateCache(req *modbus.Request) {
	// Invalidate exact matches for all read function codes that could overlap
	readFuncs := []byte{
		modbus.FuncReadCoils,
		modbus.FuncReadDiscreteInputs,
		modbus.FuncReadHoldingRegisters,
		modbus.FuncReadInputRegisters,
	}

	for _, fc := range readFuncs {
		key := cache.Key(req.SlaveID, fc, req.Address, req.Quantity)
		p.cache.Delete(key)
	}
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
