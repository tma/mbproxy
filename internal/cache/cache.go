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

// Cache is a thread-safe in-memory cache with TTL.
type Cache struct {
	mu         sync.RWMutex
	entries    map[string]*Entry
	defaultTTL time.Duration

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
func New(defaultTTL time.Duration) *Cache {
	c := &Cache{
		entries:    make(map[string]*Entry),
		defaultTTL: defaultTTL,
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

// Key generates a cache key from request parameters.
func Key(slaveID byte, functionCode byte, address uint16, quantity uint16) string {
	return fmt.Sprintf("%d:%d:%d:%d", slaveID, functionCode, address, quantity)
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
	c.SetWithTTL(key, data, c.defaultTTL)
}

// SetWithTTL stores a value in the cache with a specific TTL.
func (c *Cache) SetWithTTL(key string, data []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Store a copy to prevent mutation
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	c.entries[key] = &Entry{
		Data:      dataCopy,
		Timestamp: time.Now(),
		TTL:       ttl,
	}
}

// Delete removes a value from the cache.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// GetOrFetch retrieves a value from the cache or fetches it using the provided function.
// This implements request coalescing - multiple concurrent requests for the same key
// will share a single fetch operation.
// Returns the data, a boolean indicating if it was a cache hit, and any error.
func (c *Cache) GetOrFetch(ctx context.Context, key string, fetch func(context.Context) ([]byte, error)) ([]byte, bool, error) {
	// Check cache first
	if data, ok := c.Get(key); ok {
		return data, true, nil
	}

	// Check if there's already an in-flight request
	c.inflightMu.Lock()
	if req, ok := c.inflight[key]; ok {
		c.inflightMu.Unlock()
		// Wait for the in-flight request to complete
		select {
		case <-req.done:
			if req.err != nil {
				return nil, false, req.err
			}
			// Return a copy
			data := make([]byte, len(req.result))
			copy(data, req.result)
			return data, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
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

	// Store result
	req.result = data
	req.err = err

	// Cache successful results
	if err == nil {
		c.Set(key, data)
	}

	// Clean up and notify waiters
	c.inflightMu.Lock()
	delete(c.inflight, key)
	c.inflightMu.Unlock()
	close(req.done)

	return data, false, err
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
			c.mu.Lock()
			for key, entry := range c.entries {
				if entry.IsExpired() {
					delete(c.entries, key)
				}
			}
			c.mu.Unlock()
		}
	}
}
