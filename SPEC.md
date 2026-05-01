# Standalone Modbus Proxy with In-Memory Caching

## Overview

A lightweight, standalone Modbus TCP proxy server that caches register values in memory. Designed to reduce load on downstream Modbus devices by serving cached responses to multiple clients (e.g., Home Assistant, EVCC, other energy management systems).

## Motivation

Many Modbus devices (inverters, meters, battery systems) have limited polling capacity or slow response times. When multiple consumers need the same data, each polling independently can overload the device or cause timeouts. A caching proxy allows frequent polling from multiple clients while minimizing upstream device load.

## Architecture

```
┌─────────────────┐     ┌──────────────────────────┐     ┌─────────────────┐
│  Modbus Client  │────▶│   Modbus Proxy Server    │────▶│  Modbus Device  │
│  (HA, EVCC...)  │◀────│   (with in-memory cache) │◀────│  (Inverter...)  │
└─────────────────┘     └──────────────────────────┘     └─────────────────┘
         ▲                         │
         │                    ┌────┴────┐
         │                    │  Cache  │
         │                    │ (Memory)│
         └────────────────────┴─────────┘
```

## Core Features

### 1. Modbus TCP Server
- Listen on configurable TCP port
- Support multiple concurrent client connections
- Handle standard Modbus function codes:
  - `0x01` Read Coils
  - `0x02` Read Discrete Inputs
  - `0x03` Read Holding Registers
  - `0x04` Read Input Registers
  - `0x05` Write Single Coil
  - `0x06` Write Single Register
  - `0x0F` Write Multiple Coils
  - `0x10` Write Multiple Registers

### 2. Upstream Connection
- Connect to downstream Modbus device via TCP/IP only
- Support multiple slave IDs through single connection
- Support clients requesting different slave IDs through the proxy
- Auto-reconnect on connection failure (unlimited retries, no backoff)
- Request pacing: configurable delay between upstream requests to prevent overwhelming slow devices
- TCP keep-alive enabled (30s interval) for connection health monitoring
- Connect delay: optional silent period after establishing connection for device settling

### 3. In-Memory Cache

#### Cache Key Structure

Values are cached per register/coil:
```
{slave_id}:{function_code}:{address}
```

Request coalescing still uses the requested range as its key:
```
{slave_id}:{function_code}:{start_address}:{quantity}
```

#### Cache Entry
```go
type CacheEntry struct {
    Data      []byte        // one register (2 bytes) or one coil/input bit (1 byte: 0 or 1)
    Timestamp time.Time
    TTL       time.Duration
}
```

#### Cache Behavior
- **Read Operations**: Check the per-register/coil cache first. Return from cache only if every value in the requested range is present and not expired.
- **Cache Misses**: If any value in the requested range is missing or expired, fetch the full requested range from upstream, then decompose the response into per-register/coil cache entries.
- **Write Operations**: Always forward to the device when writes are allowed, then invalidate each cached register/coil in the written address range. This prevents overlapping cached read ranges from serving stale values after frequent writes.
- **TTL**: Configurable (default: 10 seconds)
- **Cleanup**: Time-based expiration. Expired entries are removed during cleanup unless stale serving is enabled.
- **Staleness**: Option to serve stale data on upstream failure (default: off). When enabled, expired entries are retained so they remain available for fallback.

### Request Coalescing
- Identical in-flight range requests are coalesced (same slave_id, function, address, quantity)
- Second request arriving while first is pending will wait for and share the first's response
- Prevents thundering herd on cache miss

### Request Pacing
- Configurable delay after each successful upstream request
- Protects slow Modbus devices that cannot handle rapid-fire requests
- Delay is context-aware: cancelled if the request context is cancelled
- Only applied after successful requests (not during error recovery/reconnection)
- Logged at DEBUG level when applied

### 4. Read-Only Mode
Three modes:
- `false`: Full read/write passthrough
- `true` (default): Silently ignore write requests, return success
- `deny`: Reject write requests with Modbus exception (illegal function)

### 5. Graceful Shutdown
- Handle SIGTERM/SIGINT signals
- Complete in-flight requests before shutdown (with configurable timeout, default: 30s)
- Close upstream connection cleanly

## Environment Variables

| Variable | Description | Default | Example |
|----------|-------------|---------|---------|
| `MODBUS_LISTEN` | TCP address and port to listen on | `:5502` | `:5502`, `0.0.0.0:502` |
| `MODBUS_UPSTREAM` | Upstream Modbus device address | (required) | `192.168.1.100:502` |
| `MODBUS_SLAVE_ID` | Default slave ID for upstream | `1` | `1` |
| `MODBUS_CACHE_TTL` | Cache time-to-live | `10s` | `10s`, `1m`, `500ms` |
| `MODBUS_CACHE_SERVE_STALE` | Serve stale data on upstream error | `false` | `true`, `false` |
| `MODBUS_READONLY` | Read-only mode | `true` | `false`, `true`, `deny` |
| `MODBUS_TIMEOUT` | Upstream connection timeout | `10s` | `5s`, `30s` |
| `MODBUS_REQUEST_DELAY` | Delay after each upstream request | `0` (disabled) | `100ms`, `500ms` |
| `MODBUS_CONNECT_DELAY` | Silent period after connecting to upstream | `0` (disabled) | `500ms`, `2s` |
| `MODBUS_SHUTDOWN_TIMEOUT` | Graceful shutdown timeout | `30s` | `10s`, `60s` |
| `LOG_LEVEL` | Log level | `INFO` | `INFO`, `DEBUG` |

The container health check runs `mbproxy -health`, which performs an internal upstream connectivity check without binding a separate local TCP port.

## Implementation Details

### Dependencies

- `github.com/grid-x/modbus` - Modbus TCP client (upstream communication)

The Modbus TCP server is implemented from scratch (~300-400 lines) rather than using an external library. This provides:
- Better fit for proxy use case (libraries like `mbserver` are designed to emulate devices, not proxies)
- Clean handler-based interface as shown below
- No dependency risk from unmaintained libraries
- Full control over connection handling and request routing

### Handler Interface

```go
type Handler interface {
    HandleCoils(req *CoilsRequest) ([]bool, error)
    HandleDiscreteInputs(req *DiscreteInputsRequest) ([]bool, error)
    HandleHoldingRegisters(req *HoldingRegistersRequest) ([]uint16, error)
    HandleInputRegisters(req *InputRegistersRequest) ([]uint16, error)
}

type CachingHandler struct {
    log      Logger
    readOnly ReadOnlyMode
    conn     Connection
    cache    *Cache
}
```

### Cache Operations

```go
type Cache struct {
    mu         sync.RWMutex
    entries    map[string]*CacheEntry
    defaultTTL time.Duration
    keepStale  bool

    // Request coalescing for identical range requests.
    inflight   map[string]*inflightRequest
    inflightMu sync.Mutex
}

func RegKey(slaveID, functionCode byte, address uint16) string {
    return fmt.Sprintf("%d:%d:%d", slaveID, functionCode, address)
}

func RangeKey(slaveID, functionCode byte, address, quantity uint16) string {
    return fmt.Sprintf("%d:%d:%d:%d", slaveID, functionCode, address, quantity)
}

func (c *Cache) GetRange(slaveID, functionCode byte, address, quantity uint16) ([][]byte, bool) {
    if quantity == 0 {
        return nil, false
    }

    c.mu.RLock()
    defer c.mu.RUnlock()

    values := make([][]byte, quantity)
    for i := uint16(0); i < quantity; i++ {
        entry, ok := c.entries[RegKey(slaveID, functionCode, address+i)]
        if !ok || entry.IsExpired() {
            return nil, false
        }
        values[i] = append([]byte(nil), entry.Data...)
    }
    return values, true
}

func (c *Cache) SetRange(slaveID, functionCode byte, address uint16, values [][]byte) {
    c.mu.Lock()
    defer c.mu.Unlock()

    now := time.Now()
    for i, value := range values {
        c.entries[RegKey(slaveID, functionCode, address+uint16(i))] = &CacheEntry{
            Data:      append([]byte(nil), value...),
            Timestamp: now,
            TTL:       c.defaultTTL,
        }
    }
}

func (c *Cache) DeleteRange(slaveID, functionCode byte, address, quantity uint16) {
    c.mu.Lock()
    defer c.mu.Unlock()

    for i := uint16(0); i < quantity; i++ {
        delete(c.entries, RegKey(slaveID, functionCode, address+i))
    }
}
```

The cache also exposes `Coalesce(ctx, rangeKey, fetch)` for request coalescing. It does not read or write cache entries directly; the proxy performs cache lookups and stores decomposed responses.

### Request Flow

1. Client sends Modbus TCP request
2. Parse request: extract slave ID, function code, address, quantity
3. **For reads**:
   - Check every per-register/coil cache key in the requested range
   - If all values are present and valid, reassemble and return the Modbus response
   - On any miss or expired value: coalesce identical in-flight range requests, then forward to upstream device
   - Decompose successful upstream responses into per-register/coil cache entries
   - Return response to client
4. **For writes**:
   - Check readonly mode
   - If allowed: forward to upstream, then invalidate every cached register/coil in the written address range
   - Return response

## Logging

### Log Levels
- **INFO** (default): Startup, shutdown, connection events
- **DEBUG**: Cache hits/misses, upstream requests, timing

### Log Format
```
level=INFO msg="starting proxy" listen=:5502 upstream=192.168.1.100:502
level=DEBUG msg="cache hit" slave_id=1 func=0x03 addr=0 qty=10
level=DEBUG msg="cache miss" slave_id=1 func=0x03 addr=0 qty=10
level=DEBUG msg="upstream request completed" slave_id=1 func=0x03 addr=0 qty=10 duration=15ms
level=DEBUG msg="applying request delay" delay=100ms
level=DEBUG msg="applying connect delay" delay=500ms
level=WARN msg="upstream error, serving stale" slave_id=1 error="timeout"
level=INFO msg="shutting down"
```

## CLI Usage

```bash
# Minimal (required: MODBUS_UPSTREAM)
MODBUS_UPSTREAM=192.168.1.100:502 modbus-proxy

# Custom listen port and TTL
MODBUS_LISTEN=:502 MODBUS_CACHE_TTL=5s MODBUS_UPSTREAM=192.168.1.100:502 modbus-proxy

# Enable writes passthrough
MODBUS_READONLY=false MODBUS_UPSTREAM=192.168.1.100:502 modbus-proxy

# Debug logging
LOG_LEVEL=DEBUG MODBUS_UPSTREAM=192.168.1.100:502 modbus-proxy
```

## Deliverables

After implementation, generate the following based on the actual code:

### Documentation
- **README.md**: User-facing documentation with Docker Compose examples showing how to run the container

### Docker
- **Dockerfile**: Multi-stage build targeting scratch base image for minimal size (~10MB)
- **.dockerignore**: Exclude unnecessary files from build context

### GitHub Actions
- **docker-publish.yml**: Build and publish to GHCR on tags and main branch, multi-arch (amd64/arm64)
- **test.yml**: Run tests and linting on PRs
