package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type upstreamCacheTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *upstreamCacheTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *upstreamCacheTestClock) Advance(d time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(d)
}

func newTestUpstreamCache(capacity int, ttl time.Duration) (*upstreamCache, *upstreamCacheTestClock) {
	clock := &upstreamCacheTestClock{now: time.Unix(1_700_000_000, 0)}
	cache := newUpstreamCache(capacity, ttl)
	cache.now = clock.Now
	return cache, clock
}

func assertUpstreamCacheValue(t *testing.T, cache *upstreamCache, username, want string, wantOK bool) {
	t.Helper()
	got, ok := cache.Load(username)
	if got != want || ok != wantOK {
		t.Fatalf("Load(%q) = (%q, %v), want (%q, %v)", username, got, ok, want, wantOK)
	}
}

func assertUpstreamCacheState(t *testing.T, cache *upstreamCache) int {
	t.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) > cache.maxEntries {
		t.Errorf("cache size = %d, exceeds capacity %d", len(cache.entries), cache.maxEntries)
		return -1
	}
	if cache.order.Len() != len(cache.entries) {
		t.Errorf("order length = %d, map length = %d", cache.order.Len(), len(cache.entries))
		return -1
	}
	seen := make(map[string]bool)
	for element := cache.order.Front(); element != nil; element = element.Next() {
		entry, ok := element.Value.(cacheEntry)
		if !ok {
			t.Errorf("list element has type %T, want cacheEntry", element.Value)
			return -1
		}
		if seen[entry.username] {
			t.Errorf("duplicate username %q in eviction order", entry.username)
			return -1
		}
		seen[entry.username] = true
		if cache.entries[entry.username] != element {
			t.Errorf("map/list disagree for username %q", entry.username)
			return -1
		}
	}
	return len(cache.entries)
}

func TestUpstreamCacheTTLBoundary(t *testing.T) {
	const ttl = 10 * time.Second
	for _, test := range []struct {
		name    string
		elapsed time.Duration
		wantOK  bool
	}{
		{name: "before expiry", elapsed: ttl - time.Nanosecond, wantOK: true},
		{name: "at expiry", elapsed: ttl},
		{name: "after expiry", elapsed: ttl + time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache, clock := newTestUpstreamCache(2, ttl)
			cache.Store("alice", "https://first.example")
			clock.Advance(test.elapsed)
			want := ""
			if test.wantOK {
				want = "https://first.example"
			}
			assertUpstreamCacheValue(t, cache, "alice", want, test.wantOK)
			wantSize := 0
			if test.wantOK {
				wantSize = 1
			}
			if size := assertUpstreamCacheState(t, cache); size != wantSize {
				t.Fatalf("cache size after lookup = %d, want %d", size, wantSize)
			}
		})
	}
}

func TestUpstreamCacheLoadsWithoutSuccessfulStoreDoNotExtendTTL(t *testing.T) {
	const ttl = 10 * time.Second
	cache, clock := newTestUpstreamCache(2, ttl)
	cache.Store("alice", "https://first.example")
	for i := 0; i < 9; i++ {
		clock.Advance(time.Second)
		assertUpstreamCacheValue(t, cache, "alice", "https://first.example", true)
		assertUpstreamCacheValue(t, cache, "missing", "", false)
	}
	clock.Advance(time.Second)
	assertUpstreamCacheValue(t, cache, "alice", "", false)
	if size := assertUpstreamCacheState(t, cache); size != 0 {
		t.Fatalf("cache size after original TTL = %d, want 0", size)
	}
}

func TestUpstreamCacheStoreUpdatesURLAndExtendsTTL(t *testing.T) {
	const ttl = 10 * time.Second
	cache, clock := newTestUpstreamCache(2, ttl)
	cache.Store("alice", "https://first.example")
	clock.Advance(6 * time.Second)
	cache.Store("alice", "https://second.example")
	if size := assertUpstreamCacheState(t, cache); size != 1 {
		t.Fatalf("cache size after updating username = %d, want 1", size)
	}
	clock.Advance(4 * time.Second)
	assertUpstreamCacheValue(t, cache, "alice", "https://second.example", true)
	clock.Advance(6*time.Second - time.Nanosecond)
	assertUpstreamCacheValue(t, cache, "alice", "https://second.example", true)
	clock.Advance(time.Nanosecond)
	assertUpstreamCacheValue(t, cache, "alice", "", false)
}

func TestUpstreamCacheEvictsOldestSuccessfulAssociation(t *testing.T) {
	t.Run("loads do not change order", func(t *testing.T) {
		cache, clock := newTestUpstreamCache(2, time.Minute)
		cache.Store("alice", "https://alice.example")
		clock.Advance(time.Second)
		cache.Store("bob", "https://bob.example")
		assertUpstreamCacheValue(t, cache, "alice", "https://alice.example", true)
		cache.Store("carol", "https://carol.example")
		assertUpstreamCacheValue(t, cache, "alice", "", false)
		assertUpstreamCacheValue(t, cache, "bob", "https://bob.example", true)
		assertUpstreamCacheValue(t, cache, "carol", "https://carol.example", true)
		assertUpstreamCacheState(t, cache)
	})

	t.Run("successful updates change order without evicting another user", func(t *testing.T) {
		cache, clock := newTestUpstreamCache(2, time.Minute)
		cache.Store("alice", "https://alice.example")
		clock.Advance(time.Second)
		cache.Store("bob", "https://bob.example")
		clock.Advance(time.Second)
		cache.Store("alice", "https://updated.example")
		assertUpstreamCacheValue(t, cache, "alice", "https://updated.example", true)
		assertUpstreamCacheValue(t, cache, "bob", "https://bob.example", true)
		if size := assertUpstreamCacheState(t, cache); size != 2 {
			t.Fatalf("cache size after updating at capacity = %d, want 2", size)
		}
		cache.Store("carol", "https://carol.example")
		assertUpstreamCacheValue(t, cache, "alice", "https://updated.example", true)
		assertUpstreamCacheValue(t, cache, "bob", "", false)
		assertUpstreamCacheValue(t, cache, "carol", "https://carol.example", true)
		assertUpstreamCacheState(t, cache)
	})
}

func TestUpstreamCacheCapacityOne(t *testing.T) {
	cache, _ := newTestUpstreamCache(1, time.Hour)
	cache.Store("alice", "first")
	cache.Store("alice", "updated")
	assertUpstreamCacheValue(t, cache, "alice", "updated", true)
	cache.Store("bob", "second")
	assertUpstreamCacheValue(t, cache, "alice", "", false)
	assertUpstreamCacheValue(t, cache, "bob", "second", true)
	if size := assertUpstreamCacheState(t, cache); size != 1 {
		t.Fatalf("cache size = %d, want 1", size)
	}
}

func TestUpstreamCacheStorePurgesExpiredEntriesBeforeCapacityEviction(t *testing.T) {
	cache, clock := newTestUpstreamCache(3, 10*time.Second)
	cache.Store("alice", "https://alice.example")
	clock.Advance(time.Second)
	cache.Store("bob", "https://bob.example")
	clock.Advance(8 * time.Second)
	cache.Store("carol", "https://carol.example")
	clock.Advance(2 * time.Second)
	cache.Store("dave", "https://dave.example")
	if size := assertUpstreamCacheState(t, cache); size != 2 {
		t.Fatalf("cache size after purging expired tail = %d, want 2", size)
	}
	assertUpstreamCacheValue(t, cache, "alice", "", false)
	assertUpstreamCacheValue(t, cache, "bob", "", false)
	assertUpstreamCacheValue(t, cache, "carol", "https://carol.example", true)
	assertUpstreamCacheValue(t, cache, "dave", "https://dave.example", true)
}

func TestUpstreamCacheCleanupExpired(t *testing.T) {
	cache, clock := newTestUpstreamCache(4, 10*time.Second)
	cache.cleanupExpired()
	cache.Store("alice", "https://alice.example")
	clock.Advance(time.Second)
	cache.Store("bob", "https://bob.example")
	clock.Advance(8 * time.Second)
	cache.Store("carol", "https://carol.example")
	clock.Advance(2 * time.Second)
	cache.cleanupExpired()
	if size := assertUpstreamCacheState(t, cache); size != 1 {
		t.Fatalf("cache size after cleanup = %d, want 1", size)
	}
	assertUpstreamCacheValue(t, cache, "carol", "https://carol.example", true)
	clock.Advance(8 * time.Second)
	cache.cleanupExpired()
	cache.cleanupExpired()
	if size := assertUpstreamCacheState(t, cache); size != 0 {
		t.Fatalf("cache size after all entries expire = %d, want 0", size)
	}
}

func TestUpstreamCachePeriodicCleanupAndCancellation(t *testing.T) {
	cache, clock := newTestUpstreamCache(2, time.Second)
	cache.Store("alice", "https://alice.example")
	clock.Advance(time.Second)
	ticks := make(chan struct{}, 1)
	cache.now = func() time.Time {
		now := clock.Now()
		select {
		case ticks <- struct{}{}:
		default:
		}
		return now
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.runCleanup(ctx, time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("runCleanup did not stop after context cancellation")
		}
	})

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ticks:
			if assertUpstreamCacheState(t, cache) == 0 {
				cancel()
				select {
				case <-done:
				case <-deadline.C:
					t.Fatal("runCleanup did not return after context cancellation")
				}
				return
			}
		case <-deadline.C:
			t.Fatal("periodic cleanup did not remove expired entry without a request")
		}
	}
}

func TestUpstreamCacheCleanupWithCanceledContextReturns(t *testing.T) {
	cache, _ := newTestUpstreamCache(2, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.runCleanup(ctx, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runCleanup waited for a tick despite an already canceled context")
	}
}

func TestUpstreamCacheConcurrentAccessMaintainsCapacity(t *testing.T) {
	const ttl = time.Second
	cache, clock := newTestUpstreamCache(16, ttl)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 250; i++ {
				username := fmt.Sprintf("user-%d", (worker*17+i)%64)
				cache.Store(username, fmt.Sprintf("https://upstream-%d.example", worker))
				cache.Load(username)
				cache.Load("missing")
				if i%7 == 0 {
					clock.Advance(time.Millisecond)
					cache.cleanupExpired()
				}
				assertUpstreamCacheState(t, cache)
			}
		}(worker)
	}
	workers.Wait()
	assertUpstreamCacheState(t, cache)
	clock.Advance(ttl)
	cache.cleanupExpired()
	if size := assertUpstreamCacheState(t, cache); size != 0 {
		t.Fatalf("cache size after concurrent work and expiry = %d, want 0", size)
	}
}
