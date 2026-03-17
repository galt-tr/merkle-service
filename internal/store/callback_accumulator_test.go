package store

import (
	"fmt"
	"log/slog"
	"os"
	"testing"
)

func newAccumulatorTestStore(t *testing.T) *CallbackAccumulatorStore {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	client, err := NewAerospikeClient("localhost", 3000, "merkle", 2, 50, logger)
	if err != nil {
		t.Skipf("Aerospike not available: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	setName := fmt.Sprintf("test_accum_%d", os.Getpid())
	return NewCallbackAccumulatorStore(client, setName, 60, 2, 50, logger)
}

func TestCallbackAccumulatorStore_AppendSingle(t *testing.T) {
	store := newAccumulatorTestStore(t)

	err := store.Append("block1", "http://example.com/cb", []string{"txid1", "txid2"}, "subtreeA")
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	result, err := store.ReadAndDelete("block1")
	if err != nil {
		t.Fatalf("ReadAndDelete failed: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 URL entry, got %d", len(result))
	}

	acc := result["http://example.com/cb"]
	if acc == nil {
		t.Fatal("expected entry for callback URL")
	}
	if len(acc.TxIDs) != 2 {
		t.Errorf("expected 2 txids, got %d", len(acc.TxIDs))
	}
	if len(acc.StumpRefs) != 1 || acc.StumpRefs[0] != "subtreeA" {
		t.Errorf("expected StumpRefs=[subtreeA], got %v", acc.StumpRefs)
	}
}

func TestCallbackAccumulatorStore_AppendMultipleSameURL(t *testing.T) {
	store := newAccumulatorTestStore(t)

	// Two subtrees with txids for the same callback URL.
	if err := store.Append("block2", "http://example.com/cb", []string{"txid1"}, "subtreeA"); err != nil {
		t.Fatalf("Append 1 failed: %v", err)
	}
	if err := store.Append("block2", "http://example.com/cb", []string{"txid2", "txid3"}, "subtreeB"); err != nil {
		t.Fatalf("Append 2 failed: %v", err)
	}

	result, err := store.ReadAndDelete("block2")
	if err != nil {
		t.Fatalf("ReadAndDelete failed: %v", err)
	}

	acc := result["http://example.com/cb"]
	if acc == nil {
		t.Fatal("expected entry for callback URL")
	}
	if len(acc.TxIDs) != 3 {
		t.Errorf("expected 3 txids, got %d: %v", len(acc.TxIDs), acc.TxIDs)
	}
	if len(acc.StumpRefs) != 2 {
		t.Errorf("expected 2 StumpRefs, got %d: %v", len(acc.StumpRefs), acc.StumpRefs)
	}
}

func TestCallbackAccumulatorStore_AppendDifferentURLs(t *testing.T) {
	store := newAccumulatorTestStore(t)

	if err := store.Append("block3", "http://a.com/cb", []string{"txid1"}, "subtreeA"); err != nil {
		t.Fatalf("Append 1 failed: %v", err)
	}
	if err := store.Append("block3", "http://b.com/cb", []string{"txid2"}, "subtreeA"); err != nil {
		t.Fatalf("Append 2 failed: %v", err)
	}

	result, err := store.ReadAndDelete("block3")
	if err != nil {
		t.Fatalf("ReadAndDelete failed: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 URL entries, got %d", len(result))
	}
	if result["http://a.com/cb"] == nil || result["http://b.com/cb"] == nil {
		t.Error("missing expected callback URLs in result")
	}
}

func TestCallbackAccumulatorStore_ReadAndDeleteRemovesRecord(t *testing.T) {
	store := newAccumulatorTestStore(t)

	if err := store.Append("block4", "http://example.com/cb", []string{"txid1"}, "subtreeA"); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// First read should return data.
	result, err := store.ReadAndDelete("block4")
	if err != nil {
		t.Fatalf("ReadAndDelete failed: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}

	// Second read should return nil (record deleted).
	result2, err := store.ReadAndDelete("block4")
	if err != nil {
		t.Fatalf("second ReadAndDelete failed: %v", err)
	}
	if result2 != nil && len(result2) != 0 {
		t.Errorf("expected empty result after delete, got %d entries", len(result2))
	}
}

func TestCallbackAccumulatorStore_ReadNonexistent(t *testing.T) {
	store := newAccumulatorTestStore(t)

	result, err := store.ReadAndDelete("nonexistent-block")
	if err != nil {
		t.Fatalf("ReadAndDelete failed: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for nonexistent block, got %v", result)
	}
}
