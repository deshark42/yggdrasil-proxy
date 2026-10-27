package main

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestHasJoinedFailuresDoNotRefreshCacheTTL(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetHandlerState(t)
			now := time.Now()
			playerCache = newUpstreamCache(2, time.Hour)
			playerCache.now = func() time.Time { return now }
			cached := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			playerCache.Store("Player", cached.server.URL)
			now = now.Add(59 * time.Minute)
			got := callHasJoined("/hasJoined?username=Player&serverId=hash")
			want := http.StatusNoContent
			if status != http.StatusNoContent {
				want = http.StatusBadGateway
			}
			if got.Code != want {
				t.Fatalf("status = %d, want %d", got.Code, want)
			}
			assertCachedURL(t, "Player", cached.server.URL)
			now = now.Add(time.Minute)
			if _, ok := playerCache.Load("Player"); ok {
				t.Fatal("failed authentication extended cache lifetime")
			}
		})
	}
}

func TestHasJoinedSuccessRefreshesCacheTTL(t *testing.T) {
	resetHandlerState(t)
	now := time.Now()
	playerCache = newUpstreamCache(2, time.Hour)
	playerCache.now = func() time.Time { return now }
	cached := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, testProfile) })
	playerCache.Store("Player", cached.server.URL)
	now = now.Add(59 * time.Minute)
	got := callHasJoined("/hasJoined?username=Player&serverId=hash")
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.Code)
	}
	now = now.Add(time.Minute)
	assertCachedURL(t, "Player", cached.server.URL)
	now = now.Add(59 * time.Minute)
	if _, ok := playerCache.Load("Player"); ok {
		t.Fatal("refreshed entry survived expiry boundary")
	}
}

func TestHasJoinedExpiredCacheUsesPriorityOrder(t *testing.T) {
	resetHandlerState(t)
	now := time.Now()
	playerCache = newUpstreamCache(2, time.Hour)
	playerCache.now = func() time.Time { return now }
	var calls testCallLog
	priority := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.add("priority")
		_, _ = io.WriteString(w, testProfile)
	})
	cached := addTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.add("expired")
		_, _ = io.WriteString(w, testProfile)
	})
	playerCache.Store("Player", cached.server.URL)
	now = now.Add(time.Hour)
	got := callHasJoined("/hasJoined?username=Player&serverId=hash")
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.Code)
	}
	calls.assert(t, "priority")
	assertCachedURL(t, "Player", priority.server.URL)
}
