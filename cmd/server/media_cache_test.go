package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newMediaCacheTestSession(budget *mediaCacheBudget) *UserSession {
	return &UserSession{
		Client:           NewLoggedInMockClient(),
		MediaCache:       make(map[string]*mediaCacheEntry),
		mediaCacheBudget: budget,
	}
}

func assertMediaCacheAccounting(t *testing.T, budget *mediaCacheBudget, wantBytes, wantEntries int64) {
	t.Helper()
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.bytes != wantBytes || budget.entries != wantEntries {
		t.Fatalf("cache accounting = %d bytes/%d entries, want %d bytes/%d entries", budget.bytes, budget.entries, wantBytes, wantEntries)
	}
	if budget.bytes < 0 || budget.entries < 0 || budget.bytes > budget.limit {
		t.Fatalf("invalid cache accounting: %+v", budget)
	}
}

func TestMediaCacheProcessBudgetConcurrentSessions(t *testing.T) {
	const (
		limit     = int64(1024)
		entrySize = 128
		workers   = 64
	)
	budget := &mediaCacheBudget{limit: limit}
	sessions := []*UserSession{
		newMediaCacheTestSession(budget),
		newMediaCacheTestSession(budget),
		newMediaCacheTestSession(budget),
		newMediaCacheTestSession(budget),
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			sessions[i%len(sessions)].mediaCachePut(fmt.Sprintf("message-%d", i), make([]byte, entrySize))
		}(i)
	}
	close(start)
	wg.Wait()

	assertMediaCacheAccounting(t, budget, limit, limit/entrySize)
	for _, session := range sessions {
		session.close(false)
	}
	assertMediaCacheAccounting(t, budget, 0, 0)
}

func TestMediaCacheAccountingGetReplacementAndRejection(t *testing.T) {
	budget := &mediaCacheBudget{limit: 10}
	session := newMediaCacheTestSession(budget)

	if !session.mediaCachePut("one", make([]byte, 6)) {
		t.Fatal("initial insert was rejected")
	}
	if !session.mediaCachePut("one", make([]byte, 8)) {
		t.Fatal("replacement that fits was rejected")
	}
	assertMediaCacheAccounting(t, budget, 8, 1)
	if session.mediaCachePut("two", make([]byte, 3)) {
		t.Fatal("insert above process budget was accepted")
	}
	assertMediaCacheAccounting(t, budget, 8, 1)

	data, ok := session.mediaCacheGet("one")
	if !ok || len(data) != 8 {
		t.Fatalf("cache get = %d bytes, %v; want 8 bytes, true", len(data), ok)
	}
	assertMediaCacheAccounting(t, budget, 0, 0)

	if session.mediaCachePut("oversized", make([]byte, mediaCacheMaxEntryBytes+1)) {
		t.Fatal("oversized entry was accepted")
	}
	assertMediaCacheAccounting(t, budget, 0, 0)
}

func TestMediaCacheAccountingTTLEvictionAndClose(t *testing.T) {
	budget := &mediaCacheBudget{limit: 100}
	session := newMediaCacheTestSession(budget)
	if !session.mediaCachePut("expired", make([]byte, 30)) || !session.mediaCachePut("fresh", make([]byte, 20)) {
		t.Fatal("test cache inserts were rejected")
	}

	lockedBudget := session.lockMediaCache()
	session.MediaCache["expired"].cachedAt = time.Now().Add(-mediaCacheTTL - time.Second)
	session.unlockMediaCache(lockedBudget)
	if got := session.mediaCacheEvictExpired(time.Now()); got != 1 {
		t.Fatalf("TTL evicted %d entries, want 1", got)
	}
	assertMediaCacheAccounting(t, budget, 20, 1)

	session.close(false)
	assertMediaCacheAccounting(t, budget, 0, 0)
	if session.mediaCachePut("after-close", []byte("x")) {
		t.Fatal("closed session accepted cache data")
	}
	assertMediaCacheAccounting(t, budget, 0, 0)
}
