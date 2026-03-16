package store

import (
	"fmt"
	"sync"
	"time"
)

// StumpCache abstracts STUMP binary storage keyed by subtreeHash:blockHash.
// Implementations include MemoryStumpCache (in-process) and AerospikeStumpCache (distributed).
type StumpCache interface {
	Put(subtreeHash, blockHash string, data []byte) error
	Get(subtreeHash, blockHash string) ([]byte, bool, error)
	Close() error
}

// NewStumpCacheFromConfig creates a StumpCache based on the mode string.
// "memory" returns a MemoryStumpCache; "aerospike" returns an AerospikeStumpCache.
func NewStumpCacheFromConfig(mode string, asClient *AerospikeClient, setName string, ttlSec int, lruSize int, maxRetries int, retryBaseMs int) (StumpCache, error) {
	switch mode {
	case "aerospike":
		if asClient == nil {
			return nil, fmt.Errorf("aerospike client required for aerospike stump cache mode")
		}
		return NewAerospikeStumpCache(asClient, setName, ttlSec, lruSize, maxRetries, retryBaseMs)
	case "memory", "":
		cache := NewMemoryStumpCache(ttlSec)
		cache.Start()
		return cache, nil
	default:
		return nil, fmt.Errorf("unknown stump cache mode: %s", mode)
	}
}

// MemoryStumpCache stores encoded STUMP binary data in-process using sync.Map.
// It uses a background goroutine for TTL eviction.
type MemoryStumpCache struct {
	entries sync.Map
	ttl     time.Duration
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

type stumpCacheEntry struct {
	data      []byte
	expiresAt time.Time
}

// NewMemoryStumpCache creates a new in-process StumpCache with the given TTL.
func NewMemoryStumpCache(ttlSec int) *MemoryStumpCache {
	if ttlSec <= 0 {
		ttlSec = 300 // 5 minutes default
	}
	return &MemoryStumpCache{
		ttl:    time.Duration(ttlSec) * time.Second,
		stopCh: make(chan struct{}),
	}
}

func stumpCacheKey(subtreeHash, blockHash string) string {
	return subtreeHash + ":" + blockHash
}

// Put stores a STUMP binary in the cache.
func (c *MemoryStumpCache) Put(subtreeHash, blockHash string, data []byte) error {
	c.entries.Store(stumpCacheKey(subtreeHash, blockHash), &stumpCacheEntry{
		data:      data,
		expiresAt: time.Now().Add(c.ttl),
	})
	return nil
}

// Get retrieves a STUMP binary from the cache. Returns (data, found, error).
func (c *MemoryStumpCache) Get(subtreeHash, blockHash string) ([]byte, bool, error) {
	val, ok := c.entries.Load(stumpCacheKey(subtreeHash, blockHash))
	if !ok {
		return nil, false, nil
	}
	entry := val.(*stumpCacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.entries.Delete(stumpCacheKey(subtreeHash, blockHash))
		return nil, false, nil
	}
	return entry.data, true, nil
}

// Close stops the background sweep and waits for it to finish.
func (c *MemoryStumpCache) Close() error {
	close(c.stopCh)
	c.wg.Wait()
	return nil
}

// Start begins the background TTL sweep goroutine.
func (c *MemoryStumpCache) Start() {
	c.wg.Add(1)
	go c.sweepLoop()
}

func (c *MemoryStumpCache) sweepLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			c.entries.Range(func(key, val any) bool {
				entry := val.(*stumpCacheEntry)
				if now.After(entry.expiresAt) {
					c.entries.Delete(key)
				}
				return true
			})
		}
	}
}
