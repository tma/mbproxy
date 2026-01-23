package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_GetSet(t *testing.T) {
	c := New(time.Second)
	defer c.Close()

	// Test miss
	if _, ok := c.Get("missing"); ok {
		t.Error("expected cache miss for missing key")
	}

	// Test set and get
	c.Set("key1", []byte("value1"))
	data, ok := c.Get("key1")
	if !ok {
		t.Error("expected cache hit")
	}
	if string(data) != "value1" {
		t.Errorf("expected value1, got %s", string(data))
	}
}

func TestCache_TTL(t *testing.T) {
	c := New(50 * time.Millisecond)
	defer c.Close()

	c.Set("key1", []byte("value1"))

	// Should hit immediately
	if _, ok := c.Get("key1"); !ok {
		t.Error("expected cache hit")
	}

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	// Should miss after TTL
	if _, ok := c.Get("key1"); ok {
		t.Error("expected cache miss after TTL")
	}
}

func TestCache_GetStale(t *testing.T) {
	c := New(50 * time.Millisecond)
	defer c.Close()

	c.Set("key1", []byte("value1"))

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	// GetStale should return expired data
	data, ok := c.GetStale("key1")
	if !ok {
		t.Error("expected stale data to be returned")
	}
	if string(data) != "value1" {
		t.Errorf("expected value1, got %s", string(data))
	}
}

func TestCache_Delete(t *testing.T) {
	c := New(time.Second)
	defer c.Close()

	c.Set("key1", []byte("value1"))
	c.Delete("key1")

	if _, ok := c.Get("key1"); ok {
		t.Error("expected cache miss after delete")
	}
}

func TestCache_Key(t *testing.T) {
	key := Key(1, 0x03, 100, 10)
	expected := "1:3:100:10"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

func TestCache_GetOrFetch(t *testing.T) {
	c := New(time.Second)
	defer c.Close()

	ctx := context.Background()
	fetchCount := 0
	fetch := func(ctx context.Context) ([]byte, error) {
		fetchCount++
		return []byte("fetched"), nil
	}

	// First call should fetch (cache miss)
	data, hit, err := c.GetOrFetch(ctx, "key1", fetch)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if hit {
		t.Error("expected cache miss on first call")
	}
	if string(data) != "fetched" {
		t.Errorf("expected fetched, got %s", string(data))
	}
	if fetchCount != 1 {
		t.Errorf("expected 1 fetch, got %d", fetchCount)
	}

	// Second call should hit cache
	data, hit, err = c.GetOrFetch(ctx, "key1", fetch)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !hit {
		t.Error("expected cache hit on second call")
	}
	if string(data) != "fetched" {
		t.Errorf("expected fetched, got %s", string(data))
	}
	if fetchCount != 1 {
		t.Errorf("expected 1 fetch (cache hit), got %d", fetchCount)
	}
}

func TestCache_RequestCoalescing(t *testing.T) {
	c := New(time.Second)
	defer c.Close()

	ctx := context.Background()
	var fetchCount int32
	fetchStarted := make(chan struct{})
	fetchContinue := make(chan struct{})

	fetch := func(ctx context.Context) ([]byte, error) {
		atomic.AddInt32(&fetchCount, 1)
		close(fetchStarted)
		<-fetchContinue
		return []byte("fetched"), nil
	}

	var wg sync.WaitGroup
	results := make([][]byte, 3)
	errors := make([]error, 3)

	// Start first request
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], _, errors[0] = c.GetOrFetch(ctx, "key1", fetch)
	}()

	// Wait for fetch to start
	<-fetchStarted

	// Start two more requests while first is in-flight
	for i := 1; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _, errors[i] = c.GetOrFetch(ctx, "key1", func(ctx context.Context) ([]byte, error) {
				atomic.AddInt32(&fetchCount, 1)
				return []byte("should not be called"), nil
			})
		}()
	}

	// Give time for requests to queue up
	time.Sleep(50 * time.Millisecond)

	// Allow fetch to complete
	close(fetchContinue)
	wg.Wait()

	// All requests should succeed with same data
	for i := 0; i < 3; i++ {
		if errors[i] != nil {
			t.Errorf("request %d: unexpected error: %v", i, errors[i])
		}
		if string(results[i]) != "fetched" {
			t.Errorf("request %d: expected fetched, got %s", i, string(results[i]))
		}
	}

	// Only one fetch should have happened
	if fetchCount != 1 {
		t.Errorf("expected 1 fetch (coalesced), got %d", fetchCount)
	}
}

func TestCache_ContextCancellation(t *testing.T) {
	c := New(time.Second)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	fetchStarted := make(chan struct{})

	// Start a slow fetch
	go func() {
		c.GetOrFetch(ctx, "key1", func(ctx context.Context) ([]byte, error) {
			close(fetchStarted)
			time.Sleep(time.Second)
			return []byte("fetched"), nil
		})
	}()

	<-fetchStarted

	// Start another request and cancel it
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2() // Cancel immediately

	_, _, err := c.GetOrFetch(ctx2, "key1", func(ctx context.Context) ([]byte, error) {
		return []byte("should not be called"), nil
	})

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	cancel()
}

func TestCache_DataIsolation(t *testing.T) {
	c := New(time.Second)
	defer c.Close()

	original := []byte("original")
	c.Set("key1", original)

	// Modify original after setting
	original[0] = 'X'

	// Retrieved data should be unchanged
	data, _ := c.Get("key1")
	if string(data) != "original" {
		t.Error("cache data was mutated")
	}

	// Modify retrieved data
	data[0] = 'Y'

	// Cache should be unchanged
	data2, _ := c.Get("key1")
	if string(data2) != "original" {
		t.Error("cache data was mutated via returned slice")
	}
}
