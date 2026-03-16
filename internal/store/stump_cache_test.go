package store

import (
	"sync"
	"testing"
	"time"
)

func TestStumpCache_PutGet(t *testing.T) {
	cache := NewMemoryStumpCache(300)

	data := []byte{0x01, 0x02, 0x03}
	cache.Put("subtreeA", "blockA", data)

	got, ok, err := cache.Get("subtreeA", "blockA")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 3 || got[0] != 0x01 {
		t.Errorf("expected data [1,2,3], got %v", got)
	}
}

func TestStumpCache_Miss(t *testing.T) {
	cache := NewMemoryStumpCache(300)

	_, ok, err := cache.Get("missing", "block")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestStumpCache_TTLExpiry(t *testing.T) {
	cache := NewMemoryStumpCache(1) // 1 second TTL

	cache.Put("subtreeA", "blockA", []byte{0x01})

	// Should be found immediately.
	_, ok, _ := cache.Get("subtreeA", "blockA")
	if !ok {
		t.Fatal("expected cache hit before TTL")
	}

	// Wait for TTL to expire.
	time.Sleep(1100 * time.Millisecond)

	_, ok, _ = cache.Get("subtreeA", "blockA")
	if ok {
		t.Fatal("expected cache miss after TTL")
	}
}

func TestStumpCache_ConcurrentAccess(t *testing.T) {
	cache := NewMemoryStumpCache(300)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "subtree"
			block := "block"
			cache.Put(key, block, []byte{byte(n)})
			cache.Get(key, block)
		}(i)
	}
	wg.Wait()
}

func TestStumpCache_StartClose(t *testing.T) {
	cache := NewMemoryStumpCache(300)
	cache.Start()

	cache.Put("s", "b", []byte{0x01})

	// Verify it's working.
	_, ok, _ := cache.Get("s", "b")
	if !ok {
		t.Fatal("expected cache hit")
	}

	cache.Close()
}

func TestStumpCache_SweepEvictsExpired(t *testing.T) {
	cache := NewMemoryStumpCache(1) // 1 second TTL

	cache.Put("subtreeA", "blockA", []byte{0x01})
	cache.Put("subtreeB", "blockB", []byte{0x02})

	time.Sleep(1100 * time.Millisecond)

	// Add a fresh entry.
	cache.Put("subtreeC", "blockC", []byte{0x03})

	// Manually trigger sweep logic.
	now := time.Now()
	cache.entries.Range(func(key, val any) bool {
		entry := val.(*stumpCacheEntry)
		if now.After(entry.expiresAt) {
			cache.entries.Delete(key)
		}
		return true
	})

	// Expired entries should be gone.
	_, ok, _ := cache.Get("subtreeA", "blockA")
	if ok {
		t.Error("expected subtreeA to be evicted")
	}
	_, ok, _ = cache.Get("subtreeB", "blockB")
	if ok {
		t.Error("expected subtreeB to be evicted")
	}

	// Fresh entry should remain.
	_, ok, _ = cache.Get("subtreeC", "blockC")
	if !ok {
		t.Error("expected subtreeC to be present")
	}
}
