# mbproxy

A lightweight Modbus TCP proxy with in-memory caching. Designed to reduce load on Modbus devices by serving cached responses to multiple clients.

## Features

- **Caching**: In-memory cache with configurable TTL
- **Request coalescing**: Identical concurrent requests share a single upstream fetch
- **Read-only mode**: Optionally block or ignore write requests
- **Auto-reconnect**: Automatic upstream reconnection on failure
- **Stale data fallback**: Optionally serve stale cache on upstream errors
- **Graceful shutdown**: Complete in-flight requests before terminating
- **Minimal footprint**: ~6MB Docker image (scratch base)

## Quick Start

```bash
docker run --rm \
  -e MODBUS_UPSTREAM=192.168.1.100:502 \
  -p 5502:5502 \
  ghcr.io/tma/mbproxy
```

## Configuration

All configuration is via environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `MODBUS_LISTEN` | TCP address to listen on | `:5502` |
| `MODBUS_UPSTREAM` | Upstream Modbus device address | (required) |
| `MODBUS_SLAVE_ID` | Default slave ID | `1` |
| `MODBUS_CACHE_TTL` | Cache time-to-live | `10s` |
| `MODBUS_CACHE_SERVE_STALE` | Serve stale data on upstream error | `false` |
| `MODBUS_READONLY` | Read-only mode: `false`, `true`, `deny` | `true` |
| `MODBUS_TIMEOUT` | Upstream connection timeout | `10s` |
| `MODBUS_REQUEST_DELAY` | Delay after each upstream request | `0` (disabled) |
| `MODBUS_CONNECT_DELAY` | Silent period after connecting to upstream | `0` (disabled) |
| `MODBUS_SHUTDOWN_TIMEOUT` | Graceful shutdown timeout | `30s` |
| `LOG_LEVEL` | Log level: `INFO`, `DEBUG` | `INFO` |

`/mbproxy -health` performs an internal upstream connectivity check and does not open a separate local TCP health port.

### Read-Only Modes

- `false`: Full read/write passthrough to upstream device
- `true`: Silently ignore write requests, return success response
- `deny`: Reject write requests with Modbus illegal function exception

## Docker Compose Examples

### Basic Setup

```yaml
services:
  mbproxy:
    image: ghcr.io/tma/mbproxy
    ports:
      - "5502:5502"
    environment:
      MODBUS_UPSTREAM: "192.168.1.100:502"
    restart: unless-stopped
```

### All Configuration Options

```yaml
services:
  mbproxy:
    image: ghcr.io/tma/mbproxy
    ports:
      - "5502:5502"
    environment:
      MODBUS_LISTEN: ":5502"
      MODBUS_UPSTREAM: "192.168.1.100:502"
      MODBUS_SLAVE_ID: "1"
      MODBUS_CACHE_TTL: "10s"
      MODBUS_CACHE_SERVE_STALE: "false"
      MODBUS_READONLY: "true"
      MODBUS_TIMEOUT: "10s"
      MODBUS_REQUEST_DELAY: "0"
      MODBUS_CONNECT_DELAY: "0"
      MODBUS_SHUTDOWN_TIMEOUT: "30s"
      LOG_LEVEL: "INFO"
    restart: unless-stopped
```

### Multiple Devices (Multiple Proxies)

```yaml
services:
  inverter-proxy:
    image: ghcr.io/tma/mbproxy
    ports:
      - "5502:5502"
    environment:
      MODBUS_UPSTREAM: "192.168.1.100:502"
      MODBUS_CACHE_TTL: "10s"

  meter-proxy:
    image: ghcr.io/tma/mbproxy
    ports:
      - "5503:5502"
    environment:
      MODBUS_UPSTREAM: "192.168.1.101:502"
      MODBUS_CACHE_TTL: "2s"
```

## Building from Source

```bash
# Build Docker image
docker build -t mbproxy .

# Run tests
docker build --target test .

# Or run tests directly
docker run --rm -v $(pwd):/app -w /app golang:1.24 go test ./...
```

## Supported Modbus Functions

| Code | Function |
|------|----------|
| 0x01 | Read Coils |
| 0x02 | Read Discrete Inputs |
| 0x03 | Read Holding Registers |
| 0x04 | Read Input Registers |
| 0x05 | Write Single Coil |
| 0x06 | Write Single Register |
| 0x0F | Write Multiple Coils |
| 0x10 | Write Multiple Registers |

## Cache Behavior

- **Key format**: values are cached per register/coil as `{slave_id}:{function_code}:{address}`
- **Read requests**: Served from cache only if every register/coil in the requested range is present and not expired
- **Cache misses**: If any value in the requested range is missing or expired, the full range is fetched from upstream and decomposed into per-register/coil cache entries
- **Write requests**: Forwarded to upstream (if allowed), then invalidate the written address range so overlapping cached reads cannot return stale values
- **Request coalescing**: Multiple identical range requests during a cache miss share a single upstream fetch using `{slave_id}:{function_code}:{start_address}:{quantity}` as the coalescing key
- **Stale fallback**: If enabled, expired entries are retained and can be served when upstream requests fail

## License

MIT
