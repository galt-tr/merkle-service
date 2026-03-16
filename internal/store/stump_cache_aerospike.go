package store

import (
	"fmt"

	as "github.com/aerospike/aerospike-client-go/v7"
	lru "github.com/hashicorp/golang-lru/v2"
)

const stumpCacheBin = "data"

// AerospikeStumpCache stores STUMP binaries in Aerospike with a local LRU read-through cache.
// This enables cross-pod STUMP sharing in Kubernetes deployments.
type AerospikeStumpCache struct {
	client      *AerospikeClient
	setName     string
	ttlSec      int
	maxRetries  int
	retryBaseMs int
	localLRU    *lru.Cache[string, []byte]
}

// NewAerospikeStumpCache creates a new Aerospike-backed STUMP cache.
func NewAerospikeStumpCache(client *AerospikeClient, setName string, ttlSec int, lruSize int, maxRetries int, retryBaseMs int) (*AerospikeStumpCache, error) {
	if lruSize <= 0 {
		lruSize = 1024
	}
	localLRU, err := lru.New[string, []byte](lruSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create LRU cache: %w", err)
	}
	return &AerospikeStumpCache{
		client:      client,
		setName:     setName,
		ttlSec:      ttlSec,
		maxRetries:  maxRetries,
		retryBaseMs: retryBaseMs,
		localLRU:    localLRU,
	}, nil
}

// Put writes a STUMP binary to Aerospike with TTL.
func (c *AerospikeStumpCache) Put(subtreeHash, blockHash string, data []byte) error {
	key, err := as.NewKey(c.client.Namespace(), c.setName, stumpCacheKey(subtreeHash, blockHash))
	if err != nil {
		return fmt.Errorf("failed to create aerospike key: %w", err)
	}

	wp := c.client.WritePolicy(c.maxRetries, c.retryBaseMs)
	wp.Expiration = uint32(c.ttlSec)

	err = c.client.Client().Put(wp, key, as.BinMap{stumpCacheBin: data})
	if err != nil {
		return fmt.Errorf("failed to put STUMP in aerospike: %w", err)
	}

	// Also populate local LRU on write.
	c.localLRU.Add(stumpCacheKey(subtreeHash, blockHash), data)
	return nil
}

// Get retrieves a STUMP binary. Checks local LRU first, then Aerospike.
func (c *AerospikeStumpCache) Get(subtreeHash, blockHash string) ([]byte, bool, error) {
	cacheKey := stumpCacheKey(subtreeHash, blockHash)

	// Check local LRU first.
	if data, ok := c.localLRU.Get(cacheKey); ok {
		return data, true, nil
	}

	// Miss — read from Aerospike.
	key, err := as.NewKey(c.client.Namespace(), c.setName, cacheKey)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create aerospike key: %w", err)
	}

	record, err := c.client.Client().Get(nil, key, stumpCacheBin)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get STUMP from aerospike: %w", err)
	}
	if record == nil {
		return nil, false, nil
	}

	data, ok := record.Bins[stumpCacheBin].([]byte)
	if !ok {
		return nil, false, nil
	}

	// Populate local LRU.
	c.localLRU.Add(cacheKey, data)
	return data, true, nil
}

// Close is a no-op — the Aerospike client lifecycle is managed externally.
func (c *AerospikeStumpCache) Close() error {
	return nil
}
