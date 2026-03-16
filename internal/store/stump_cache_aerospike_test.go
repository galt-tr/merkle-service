//go:build integration

package store_test

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/bsv-blockchain/merkle-service/internal/store"
)

func TestAerospikeStumpCache_PutGet(t *testing.T) {
	client := newAerospikeClient(t)
	setName := fmt.Sprintf("stump_cache_test_%d", time.Now().UnixNano())

	cache, err := store.NewAerospikeStumpCache(client, setName, 60, 128, 3, 100)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	data := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	if err := cache.Put("subtreeA", "blockA", data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got, ok, err := cache.Get("subtreeA", "blockA")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 4 || got[0] != 0xCA {
		t.Errorf("expected data [CA,FE,BA,BE], got %v", got)
	}
}

func TestAerospikeStumpCache_Miss(t *testing.T) {
	client := newAerospikeClient(t)
	setName := fmt.Sprintf("stump_cache_test_%d", time.Now().UnixNano())

	cache, err := store.NewAerospikeStumpCache(client, setName, 60, 128, 3, 100)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	_, ok, err := cache.Get("missing", "block")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestAerospikeStumpCache_LRUHitAvoidsAerospike(t *testing.T) {
	client := newAerospikeClient(t)
	setName := fmt.Sprintf("stump_cache_test_%d", time.Now().UnixNano())

	cache, err := store.NewAerospikeStumpCache(client, setName, 60, 128, 3, 100)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	data := []byte{0xDE, 0xAD}
	if err := cache.Put("st1", "b1", data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// First Get populates LRU from Put. Second Get should serve from LRU.
	for i := 0; i < 10; i++ {
		got, ok, err := cache.Get("st1", "b1")
		if err != nil {
			t.Fatalf("Get %d failed: %v", i, err)
		}
		if !ok {
			t.Fatalf("Get %d: expected hit", i)
		}
		if len(got) != 2 {
			t.Fatalf("Get %d: unexpected data length %d", i, len(got))
		}
	}
}

func TestAerospikeStumpCache_LRUEviction(t *testing.T) {
	client := newAerospikeClient(t)
	setName := fmt.Sprintf("stump_cache_test_%d", time.Now().UnixNano())

	// LRU capacity of 2.
	cache, err := store.NewAerospikeStumpCache(client, setName, 60, 2, 3, 100)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	// Put 3 entries — first should be evicted from LRU but still in Aerospike.
	cache.Put("st1", "b1", []byte{0x01})
	cache.Put("st2", "b2", []byte{0x02})
	cache.Put("st3", "b3", []byte{0x03})

	// st1 should still be retrievable (falls through to Aerospike).
	got, ok, err := cache.Get("st1", "b1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !ok {
		t.Fatal("expected hit from Aerospike after LRU eviction")
	}
	if got[0] != 0x01 {
		t.Errorf("expected 0x01, got %x", got[0])
	}
}

// Ensure slog is used (already imported via newAerospikeClient helper).
var _ = slog.Default
