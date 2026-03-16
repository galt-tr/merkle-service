package datahub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildRawSubtreeBytes creates DataHub-format raw subtree data (concatenated 32-byte hashes).
func buildRawSubtreeBytes(n int) []byte {
	data := make([]byte, n*32)
	for i := 0; i < n; i++ {
		data[i*32] = byte(i + 1)
	}
	return data
}

func TestFetchSubtree_Success(t *testing.T) {
	// Build raw DataHub-format subtree data with 2 nodes.
	subtreeBytes := buildRawSubtreeBytes(2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/subtree/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(subtreeBytes)
	}))
	defer server.Close()

	client := NewClient(5, 0, testLogger())
	result, err := client.FetchSubtree(context.Background(), server.URL, "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil subtree")
	}
	if len(result.Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(result.Nodes))
	}
	if result.Nodes[0].Hash[0] != 1 {
		t.Errorf("expected first node hash[0]=1, got %d", result.Nodes[0].Hash[0])
	}
}

func TestFetchSubtreeRaw_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(5, 0, testLogger())
	_, err := client.FetchSubtreeRaw(context.Background(), server.URL, "abc123")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 in error, got: %v", err)
	}
}

func TestFetchBlockMetadata_Success(t *testing.T) {
	meta := BlockMetadata{
		Height:           100,
		Subtrees:         []string{"aaa", "bbb", "ccc"},
		TransactionCount: 5000,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/block/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta)
	}))
	defer server.Close()

	client := NewClient(5, 0, testLogger())
	result, err := client.FetchBlockMetadata(context.Background(), server.URL, "blockhash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Height != 100 {
		t.Errorf("expected height 100, got %d", result.Height)
	}
	if len(result.Subtrees) != 3 {
		t.Errorf("expected 3 subtrees, got %d", len(result.Subtrees))
	}
}

func TestFetchSubtreeRaw_RetryOnServerError(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("error"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := NewClient(5, 3, testLogger()) // 3 retries
	data, err := client.FetchSubtreeRaw(context.Background(), server.URL, "abc123")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("expected 'ok', got %q", string(data))
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestParseRawTxids(t *testing.T) {
	raw := buildRawSubtreeBytes(3)
	txids, err := ParseRawTxids(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txids) != 3 {
		t.Fatalf("expected 3 txids, got %d", len(txids))
	}
	if len(txids[0]) != 64 {
		t.Errorf("expected 64-char hex, got %d", len(txids[0]))
	}
	// Byte[0]=1, rest zeros. Bitcoin display order reverses: "00...0001"
	if !strings.HasSuffix(txids[0], "01") {
		t.Errorf("expected reversed byte order (suffix '01'), got %s", txids[0])
	}
}

func TestParseRawTxids_InvalidLength(t *testing.T) {
	_, err := ParseRawTxids([]byte{0x01, 0x02})
	if err == nil {
		t.Fatal("expected error for non-multiple-of-32")
	}
}

func TestParseRawNodes(t *testing.T) {
	raw := buildRawSubtreeBytes(4)
	nodes, err := ParseRawNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(nodes))
	}
	for i, node := range nodes {
		if node.Hash[0] != byte(i+1) {
			t.Errorf("node %d: expected hash[0]=%d, got %d", i, i+1, node.Hash[0])
		}
	}
}
