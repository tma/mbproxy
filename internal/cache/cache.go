// Package cache provides an in-memory cache with TTL and request coalescing.
package cache

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Entry represents a cached value with timestamp and TTL.
type Entry struct {
	Data      []byte
	Timestamp time.Time
	TTL       time.Duration
}

// IsExpired returns true if the entry has expired.
func (e *Entry) IsExpired() bool {
	return time.Since(e.Timestamp) > e.TTL
}

// Cache is a thread-safe in-memory cache with TTL and per-register storage.
type Cache struct {
	mu         sync.RWMutex
	entries    map[string]*Entry
	defaultTTL time.Duration
	keepStale  bool // when true, cleanup won't delete expired entries

	// For request coalescing
	inflight   map[string]*inflightRequest
	inflightMu sync.Mutex

	// For cleanup goroutine shutdown
	done chan struct{}
}

type inflightRequest struct {
	done   chan struct{}
	result []byte
	err    error
}

// New creates a new cache with the specified default TTL.
// If keepStale is true, expired entries are retained for stale serving.
func New(defaultTTL time.Duration, keepStale bool) *Cache {
	c := &Cache{
		entries:    make(map[string]*Entry),
		defaultTTL: defaultTTL,
		keepStale:  keepStale,
		inflight:   make(map[string]*inflightRequest),
		done:       make(chan struct{}),
	}

	// Start background cleanup goroutine
	go c.cleanup()

	return c
}

// Close stops the cache cleanup goroutine.
func (c *Cache) Close() {
	close(c.done)
}

// RegKey generates a cache key for a single register or coil.
func RegKey(slaveID byte, functionCode byte, address uint16) string {
	return fmt.Sprintf("%d:%d:%d", slaveID, functionCode, address)
}

// RangeKey generates a coalescing key for a request range.
func RangeKey(slaveID byte, functionCode byte, startAddr uint16, quantity uint16) string {
	return fmt.Sprintf("%d:%d:%d:%d", slaveID, functionCode, startAddr, quantity)
}

// Get retrieves a value from the cache.
// Returns the data and true if found and not expired, nil and false otherwise.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok || entry.IsExpired() {
		return nil, false
	}

	// Return a copy to prevent mutation
	data := make([]byte, len(entry.Data))
	copy(data, entry.Data)
	return data, true
}

// GetStale retrieves a value from the cache even if expired.
// Returns the data and true if found (regardless of expiration), nil and false otherwise.
func (c *Cache) GetStale(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	data := make([]byte, len(entry.Data))
	copy(data, entry.Data)
	return data, true
}

// Set stores a value in the cache with the default TTL.
func (c *Cache) Set(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Store a copy to prevent mutation
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	c.entries[key] = &Entry{
		Data:      dataCopy,
		Timestamp: time.Now(),
		TTL:       c.defaultTTL,
	}
}

// Delete removes a value from the cache.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// GetRange retrieves all values for a contiguous register range.
// Returns the per-register/coil values and true only if ALL are cached and fresh.
func (c *Cache) GetRange(slaveID byte, functionCode byte, startAddr uint16, quantity uint16) ([][]byte, bool) {
	if quantity == 0 {
		return nil, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	values := make([][]byte, quantity)
	for i := uint16(0); i < quantity; i++ {
		key := RegKey(slaveID, functionCode, startAddr+i)
		entry, ok := c.entries[key]
		if !ok || entry.IsExpired() {
			return nil, false
		}
		data := make([]byte, len(entry.Data))
		copy(data, entry.Data)
		values[i] = data
	}
	return values, true
}

// GetRangeStale retrieves all values for a contiguous register range, ignoring TTL.
// Returns the per-register/coil values and true only if ALL are present (even if expired).
func (c *Cache) GetRangeStale(slaveID byte, functionCode byte, startAddr uint16, quantity uint16) ([][]byte, bool) {
	if quantity == 0 {
		return nil, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	values := make([][]byte, quantity)
	for i := uint16(0); i < quantity; i++ {
		key := RegKey(slaveID, functionCode, startAddr+i)
		entry, ok := c.entries[key]
		if !ok {
			return nil, false
		}
		data := make([]byte, len(entry.Data))
		copy(data, entry.Data)
		values[i] = data
	}
	return values, true
}

// SetRange stores individual values for a contiguous register range.
// All entries are stored with the same timestamp for consistency.
func (c *Cache) SetRange(slaveID byte, functionCode byte, startAddr uint16, values [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for i, v := range values {
		key := RegKey(slaveID, functionCode, startAddr+uint16(i))
		dataCopy := make([]byte, len(v))
		copy(dataCopy, v)
		c.entries[key] = &Entry{
			Data:      dataCopy,
			Timestamp: now,
			TTL:       c.defaultTTL,
		}
	}
}

// DeleteRange removes all entries for a contiguous register range.
func (c *Cache) DeleteRange(slaveID byte, functionCode byte, startAddr uint16, quantity uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := uint16(0); i < quantity; i++ {
		key := RegKey(slaveID, functionCode, startAddr+i)
		delete(c.entries, key)
	}
}

// Coalesce ensures only one fetch runs for a given key at a time.
// Other callers with the same key wait for and share the first caller's result.
// This handles request coalescing only — it does not interact with cache storage.
func (c *Cache) Coalesce(ctx context.Context, key string, fetch func(context.Context) ([]byte, error)) ([]byte, error) {
	c.inflightMu.Lock()
	if req, ok := c.inflight[key]; ok {
		c.inflightMu.Unlock()
		// Wait for the in-flight request to complete
		select {
		case <-req.done:
			if req.err != nil {
				return nil, req.err
			}
			// Return a copy
			data := make([]byte, len(req.result))
			copy(data, req.result)
			return data, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Create new in-flight request
	req := &inflightRequest{
		done: make(chan struct{}),
	}
	c.inflight[key] = req
	c.inflightMu.Unlock()

	// Fetch the data
	data, err := fetch(ctx)

	// Store result for waiters
	req.result = data
	req.err = err

	// Clean up and notify waiters
	c.inflightMu.Lock()
	delete(c.inflight, key)
	c.inflightMu.Unlock()
	close(req.done)

	if err != nil {
		return nil, err
	}

	result := make([]byte, len(data))
	copy(result, data)
	return result, nil
}

// cleanupOnce runs a single cleanup pass, removing expired entries.
// Skips deletion when keepStale is true.
func (c *Cache) cleanupOnce() {
	if c.keepStale {
		return
	}
	c.mu.Lock()
	for key, entry := range c.entries {
		if entry.IsExpired() {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
}

// cleanup periodically removes expired entries.
func (c *Cache) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.cleanupOnce()
		}
	}
}
