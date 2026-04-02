package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_GetSet(t *testing.T) {
	c := New(time.Second, false)
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
	c := New(50*time.Millisecond, false)
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
	c := New(50*time.Millisecond, false)
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
	c := New(time.Second, false)
	defer c.Close()

	c.Set("key1", []byte("value1"))
	c.Delete("key1")

	if _, ok := c.Get("key1"); ok {
		t.Error("expected cache miss after delete")
	}
}

func TestRegKey(t *testing.T) {
	key := RegKey(1, 0x03, 100)
	expected := "1:3:100"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

func TestRangeKey(t *testing.T) {
	key := RangeKey(1, 0x03, 100, 10)
	expected := "1:3:100:10"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

func TestCache_GetRange(t *testing.T) {
	c := New(time.Second, false)
	defer c.Close()

	// Store 3 registers
	c.Set(RegKey(1, 0x03, 10), []byte{0x00, 0x01})
	c.Set(RegKey(1, 0x03, 11), []byte{0x00, 0x02})
	c.Set(RegKey(1, 0x03, 12), []byte{0x00, 0x03})

	// Full range hit
	values, ok := c.GetRange(1, 0x03, 10, 3)
	if !ok {
		t.Error("expected range hit")
	}
	if len(values) != 3 {
		t.Fatalf("expected 3 values, got %d", len(values))
	}
	for i, expected := range []byte{0x01, 0x02, 0x03} {
		if values[i][1] != expected {
			t.Errorf("value[%d]: expected 0x%02X, got 0x%02X", i, expected, values[i][1])
		}
	}

	// Partial range miss
	_, ok = c.GetRange(1, 0x03, 10, 5)
	if ok {
		t.Error("expected range miss (registers 13-14 not cached)")
	}
}

func TestCache_SetRange(t *testing.T) {
	c := New(time.Second, false)
	defer c.Close()

	values := [][]byte{{0x00, 0x0A}, {0x00, 0x0B}}
	c.SetRange(1, 0x03, 100, values)

	// Each register should be independently accessible
	data, ok := c.Get(RegKey(1, 0x03, 100))
	if !ok {
		t.Error("expected hit for register 100")
	}
	if data[1] != 0x0A {
		t.Errorf("expected 0x0A, got 0x%02X", data[1])
	}

	data, ok = c.Get(RegKey(1, 0x03, 101))
	if !ok {
		t.Error("expected hit for register 101")
	}
	if data[1] != 0x0B {
		t.Errorf("expected 0x0B, got 0x%02X", data[1])
	}
}

func TestCache_DeleteRange(t *testing.T) {
	c := New(time.Second, false)
	defer c.Close()

	values := [][]byte{{0x00, 0x0A}, {0x00, 0x0B}, {0x00, 0x0C}}
	c.SetRange(1, 0x03, 100, values)

	// Delete middle register
	c.DeleteRange(1, 0x03, 101, 1)

	// Register 100 still cached
	if _, ok := c.Get(RegKey(1, 0x03, 100)); !ok {
		t.Error("register 100 should still be cached")
	}
	// Register 101 deleted
	if _, ok := c.Get(RegKey(1, 0x03, 101)); ok {
		t.Error("register 101 should be deleted")
	}
	// Register 102 still cached
	if _, ok := c.Get(RegKey(1, 0x03, 102)); !ok {
		t.Error("register 102 should still be cached")
	}
	// Full range now misses
	if _, ok := c.GetRange(1, 0x03, 100, 3); ok {
		t.Error("expected range miss after deleting register 101")
	}
}

func TestCache_GetRangeStale(t *testing.T) {
	c := New(50*time.Millisecond, false)
	defer c.Close()

	c.SetRange(1, 0x03, 10, [][]byte{{0x00, 0x01}, {0x00, 0x02}})
	time.Sleep(100 * time.Millisecond)

	// Fresh get should miss
	if _, ok := c.GetRange(1, 0x03, 10, 2); ok {
		t.Error("expected range miss after TTL")
	}

	// Stale get should succeed
	values, ok := c.GetRangeStale(1, 0x03, 10, 2)
	if !ok {
		t.Error("expected stale range hit")
	}
	if len(values) != 2 {
		t.Fatalf("expected 2 stale values, got %d", len(values))
	}
}

func TestCache_Coalesce(t *testing.T) {
	c := New(time.Second, false)
	defer c.Close()

	ctx := context.Background()
	fetchCount := 0
	fetch := func(ctx context.Context) ([]byte, error) {
		fetchCount++
		return []byte("fetched"), nil
	}

	data, err := c.Coalesce(ctx, "key1", fetch)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if string(data) != "fetched" {
		t.Errorf("expected fetched, got %s", string(data))
	}
	if fetchCount != 1 {
		t.Errorf("expected 1 fetch, got %d", fetchCount)
	}
}

func TestCache_CoalescingConcurrent(t *testing.T) {
	c := New(time.Second, false)
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
		results[0], errors[0] = c.Coalesce(ctx, "key1", fetch)
	}()

	// Wait for fetch to start
	<-fetchStarted

	// Start two more requests while first is in-flight
	for i := 1; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errors[i] = c.Coalesce(ctx, "key1", func(ctx context.Context) ([]byte, error) {
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
	c := New(time.Second, false)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	fetchStarted := make(chan struct{})

	// Start a slow fetch
	go func() {
		c.Coalesce(ctx, "key1", func(ctx context.Context) ([]byte, error) {
			close(fetchStarted)
			time.Sleep(time.Second)
			return []byte("fetched"), nil
		})
	}()

	<-fetchStarted

	// Start another request and cancel it
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2() // Cancel immediately

	_, err := c.Coalesce(ctx2, "key1", func(ctx context.Context) ([]byte, error) {
		return []byte("should not be called"), nil
	})

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	cancel()
}

func TestCache_DataIsolation(t *testing.T) {
	c := New(time.Second, false)
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

func TestCache_RangeDataIsolation(t *testing.T) {
	c := New(time.Second, false)
	defer c.Close()

	original := [][]byte{{0x00, 0x01}, {0x00, 0x02}}
	c.SetRange(1, 0x03, 0, original)

	// Mutate original
	original[0][0] = 0xFF

	// Cache should be unaffected
	values, ok := c.GetRange(1, 0x03, 0, 2)
	if !ok {
		t.Error("expected range hit")
	}
	if values[0][0] != 0x00 {
		t.Error("cache data was mutated via original slice")
	}
}

func TestCache_KeepStale(t *testing.T) {
	// With keepStale=false, cleanup removes expired entries
	c := New(50*time.Millisecond, false)
	c.Set("key1", []byte("value1"))
	time.Sleep(100 * time.Millisecond)

	c.cleanupOnce()

	if _, ok := c.GetStale("key1"); ok {
		t.Error("expected stale data to be gone after cleanup with keepStale=false")
	}
	c.Close()

	// With keepStale=true, cleanup skips deletion
	c2 := New(50*time.Millisecond, true)
	c2.Set("key1", []byte("value1"))
	time.Sleep(100 * time.Millisecond)

	c2.cleanupOnce()

	// Entry should still be accessible via GetStale after cleanup
	data, ok := c2.GetStale("key1")
	if !ok {
		t.Error("expected stale data to survive cleanup with keepStale=true")
	}
	if string(data) != "value1" {
		t.Errorf("expected value1, got %s", string(data))
	}
	c2.Close()
}
