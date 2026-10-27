package main

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"time"
)

const (
	defaultCacheTTL             = 24 * time.Hour
	defaultCacheCleanupInterval = 10 * time.Minute
	defaultCacheMaxEntries      = 10_000
)

type cacheEntry struct {
	username    string
	upstreamURL string
	expiresAt   time.Time
}

type upstreamCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      *list.List
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
}

func newUpstreamCache(maxEntries int, ttl time.Duration) *upstreamCache {
	return &upstreamCache{
		entries:    make(map[string]*list.Element),
		order:      list.New(),
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
	}
}

func (c *upstreamCache) Load(username string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[username]
	if !ok {
		return "", false
	}
	entry := element.Value.(cacheEntry)
	if !c.now().Before(entry.expiresAt) {
		c.remove(element)
		return "", false
	}
	return entry.upstreamURL, true
}

func (c *upstreamCache) Store(username, upstreamURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.removeExpired(now)
	// Avoid retaining the full request query through a username substring.
	entry := cacheEntry{username: strings.Clone(username), upstreamURL: upstreamURL, expiresAt: now.Add(c.ttl)}
	if element, ok := c.entries[username]; ok {
		element.Value = entry
		c.order.MoveToFront(element)
		return
	}
	if len(c.entries) >= c.maxEntries {
		c.remove(c.order.Back())
	}
	c.entries[entry.username] = c.order.PushFront(entry)
}

func (c *upstreamCache) cleanupExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeExpired(c.now())
}

// Entries are ordered by successful authentication; reads never extend their lifetime.
func (c *upstreamCache) removeExpired(now time.Time) {
	for element := c.order.Back(); element != nil; element = c.order.Back() {
		if now.Before(element.Value.(cacheEntry).expiresAt) {
			return
		}
		c.remove(element)
	}
}

func (c *upstreamCache) remove(element *list.Element) {
	delete(c.entries, element.Value.(cacheEntry).username)
	c.order.Remove(element)
}

func (c *upstreamCache) runCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.cleanupExpired()
		}
	}
}
