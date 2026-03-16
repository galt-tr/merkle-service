package block

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	subtreepkg "github.com/bsv-blockchain/go-subtree"

	"github.com/bsv-blockchain/merkle-service/internal/datahub"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/store"
	"github.com/bsv-blockchain/merkle-service/internal/stump"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildRawSubtreeBytes creates raw DataHub-format subtree data (concatenated 32-byte hashes).
func buildRawSubtreeBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n*32)
	for i := 0; i < n; i++ {
		data[i*32] = byte(i + 1) // unique hash per node
	}
	return data
}

// buildNodesForMerkle creates subtree Nodes for merkle tree building.
func buildNodesForMerkle(t *testing.T, n int) []subtreepkg.Node {
	t.Helper()
	nodes := make([]subtreepkg.Node, n)
	for i := 0; i < n; i++ {
		nodes[i].Hash[0] = byte(i + 1)
	}
	return nodes
}

// TestParseRawTxids verifies parsing of DataHub raw hash format.
// ParseRawTxids must produce txids in Bitcoin display order (reversed bytes),
// matching chainhash.Hash.String() and the format used in registration storage.
func TestParseRawTxids(t *testing.T) {
	raw := buildRawSubtreeBytes(t, 4)
	txids, err := datahub.ParseRawTxids(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txids) != 4 {
		t.Fatalf("expected 4 txids, got %d", len(txids))
	}
	// First hash has byte[0]=1, rest zeros.
	// In Bitcoin display order (reversed), byte[0]=1 appears at the END: "00...0001"
	if !strings.HasSuffix(txids[0], "01") {
		t.Errorf("expected first txid to end with '01' (reversed byte order), got %s", txids[0])
	}
	if len(txids[0]) != 64 {
		t.Errorf("expected 64-char hex txid, got %d chars", len(txids[0]))
	}
}

// TestParseRawTxids_InvalidLength verifies error on non-multiple-of-32 data.
func TestParseRawTxids_InvalidLength(t *testing.T) {
	_, err := datahub.ParseRawTxids([]byte{0x01, 0x02, 0x03})
	if err == nil {
		t.Fatal("expected error for invalid length")
	}
}

// TestParseRawNodes verifies parsing into Node structs.
func TestParseRawNodes(t *testing.T) {
	raw := buildRawSubtreeBytes(t, 2)
	nodes, err := datahub.ParseRawNodes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].Hash[0] != 1 {
		t.Errorf("expected first node hash[0]=1, got %d", nodes[0].Hash[0])
	}
	if nodes[1].Hash[0] != 2 {
		t.Errorf("expected second node hash[0]=2, got %d", nodes[1].Hash[0])
	}
}

// TestSubtreeDataRetrieval_BlobStore verifies subtree data is retrieved from blob store.
func TestSubtreeDataRetrieval_BlobStore(t *testing.T) {
	rawBytes := buildRawSubtreeBytes(t, 4)

	blobStore := store.NewMemoryBlobStore()
	subtreeStore := store.NewSubtreeStore(blobStore, 1, testLogger())

	// Pre-store the raw subtree data (as DataHub would serve it).
	if err := subtreeStore.StoreSubtree("st-123", rawBytes, 0); err != nil {
		t.Fatalf("failed to store subtree: %v", err)
	}

	// Retrieve from blob store.
	data, err := subtreeStore.GetSubtree("st-123")
	if err != nil {
		t.Fatalf("failed to get subtree: %v", err)
	}

	// Parse and verify using the raw format parser.
	txids, err := datahub.ParseRawTxids(data)
	if err != nil {
		t.Fatalf("failed to parse raw txids: %v", err)
	}
	if len(txids) != 4 {
		t.Errorf("expected 4 txids, got %d", len(txids))
	}
}

// TestSubtreeDataRetrieval_DataHubFallback verifies fallback to DataHub when blob store is empty.
func TestSubtreeDataRetrieval_DataHubFallback(t *testing.T) {
	rawBytes := buildRawSubtreeBytes(t, 2)

	fetchCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/subtree/") {
			fetchCount++
			w.WriteHeader(http.StatusOK)
			w.Write(rawBytes)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	blobStore := store.NewMemoryBlobStore()
	subtreeStore := store.NewSubtreeStore(blobStore, 1, testLogger())
	dhClient := datahub.NewClient(5, 0, testLogger())

	// Blob store is empty — should fallback to DataHub.
	_, getErr := subtreeStore.GetSubtree("st-new")
	if getErr == nil {
		t.Fatal("expected error from empty blob store")
	}

	// Fetch from DataHub (as ProcessBlockSubtree would).
	rawData, err := dhClient.FetchSubtreeRaw(context.Background(), server.URL, "st-new")
	if err != nil {
		t.Fatalf("failed to fetch from DataHub: %v", err)
	}
	if fetchCount != 1 {
		t.Errorf("expected 1 DataHub fetch, got %d", fetchCount)
	}

	// Store in blob store for future use.
	if err := subtreeStore.StoreSubtree("st-new", rawData, 100); err != nil {
		t.Fatalf("failed to store subtree: %v", err)
	}

	// Verify it's now in blob store and parseable.
	data, err := subtreeStore.GetSubtree("st-new")
	if err != nil {
		t.Fatalf("subtree should now be in blob store: %v", err)
	}
	txids, err := datahub.ParseRawTxids(data)
	if err != nil {
		t.Fatalf("failed to parse stored data: %v", err)
	}
	if len(txids) != 2 {
		t.Errorf("expected 2 txids, got %d", len(txids))
	}
}

// TestMerkleTreeAndSTUMPBuild verifies the full STUMP build pipeline.
func TestMerkleTreeAndSTUMPBuild(t *testing.T) {
	nodes := buildNodesForMerkle(t, 4)

	// Build merkle tree from nodes.
	merkleTreeStore, err := subtreepkg.BuildMerkleTreeStoreFromBytes(nodes)
	if err != nil {
		t.Fatalf("failed to build merkle tree: %v", err)
	}
	if len(*merkleTreeStore) == 0 {
		t.Fatal("merkle tree should not be empty")
	}

	// Convert to [][]byte.
	merkleTree := make([][]byte, len(*merkleTreeStore))
	for i, h := range *merkleTreeStore {
		hashCopy := make([]byte, 32)
		copy(hashCopy, h[:])
		merkleTree[i] = hashCopy
	}

	// Register leaf 0 and leaf 2.
	registeredIndices := map[int]string{
		0: nodes[0].Hash.String(),
		2: nodes[2].Hash.String(),
	}

	s := stump.Build(100, merkleTree, registeredIndices)
	if s == nil {
		t.Fatal("STUMP should not be nil")
	}
	if s.BlockHeight != 100 {
		t.Errorf("expected block height 100, got %d", s.BlockHeight)
	}

	// Encode STUMP.
	encoded := s.Encode()
	if len(encoded) == 0 {
		t.Fatal("encoded STUMP should not be empty")
	}
}

// TestGroupByCallback verifies txid grouping by callback URL.
func TestGroupByCallback(t *testing.T) {
	registrations := map[string][]string{
		"txid1": {"http://cb1.com", "http://cb2.com"},
		"txid2": {"http://cb1.com"},
		"txid3": {"http://cb2.com", "http://cb3.com"},
	}

	groups := stump.GroupByCallback(registrations)

	if len(groups["http://cb1.com"]) != 2 {
		t.Errorf("expected 2 txids for cb1, got %d", len(groups["http://cb1.com"]))
	}
	if len(groups["http://cb2.com"]) != 2 {
		t.Errorf("expected 2 txids for cb2, got %d", len(groups["http://cb2.com"]))
	}
	if len(groups["http://cb3.com"]) != 1 {
		t.Errorf("expected 1 txid for cb3, got %d", len(groups["http://cb3.com"]))
	}
}

// TestBlockMetadataFetch verifies DataHub block metadata fetching with multiple subtrees.
func TestBlockMetadataFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/block/") && strings.HasSuffix(r.URL.Path, "/json") {
			meta := datahub.BlockMetadata{
				Height:           200,
				Subtrees:         []string{"st-aaa", "st-bbb", "st-ccc"},
				TransactionCount: 15000,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(meta)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	dhClient := datahub.NewClient(5, 0, testLogger())
	meta, err := dhClient.FetchBlockMetadata(context.Background(), server.URL, "block-hash-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.Height != 200 {
		t.Errorf("expected height 200, got %d", meta.Height)
	}
	if len(meta.Subtrees) != 3 {
		t.Errorf("expected 3 subtrees, got %d", len(meta.Subtrees))
	}
	if meta.TransactionCount != 15000 {
		t.Errorf("expected 15000 txs, got %d", meta.TransactionCount)
	}
}

// TestMinedStumpsMessageEncoding verifies MINED messages are properly encoded/decoded.
func TestMinedStumpsMessageEncoding(t *testing.T) {
	msg := &kafka.StumpsMessage{
		CallbackURL: "http://example.com/callback",
		TxIDs:       []string{"txid1", "txid2"},
		StumpData:   []byte{0x01, 0x02, 0x03},
		StatusType:  kafka.StatusMined,
		BlockHash:   "blockhash123",
		SubtreeID:   "subtree456",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := kafka.DecodeStumpsMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.StatusType != kafka.StatusMined {
		t.Errorf("expected MINED, got %s", decoded.StatusType)
	}
	if decoded.CallbackURL != "http://example.com/callback" {
		t.Errorf("unexpected callback URL: %s", decoded.CallbackURL)
	}
	if len(decoded.TxIDs) != 2 {
		t.Errorf("expected 2 txids, got %d", len(decoded.TxIDs))
	}
	if decoded.BlockHash != "blockhash123" {
		t.Errorf("unexpected block hash: %s", decoded.BlockHash)
	}
}

// TestSeenOnNetworkStumpsMessage verifies SEEN_ON_NETWORK message encoding.
func TestSeenOnNetworkStumpsMessage(t *testing.T) {
	msg := &kafka.StumpsMessage{
		CallbackURL: "http://example.com/cb",
		TxID:        "abc123",
		StatusType:  kafka.StatusSeenOnNetwork,
		SubtreeID:   "st-xyz",
	}

	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := kafka.DecodeStumpsMessage(data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.StatusType != kafka.StatusSeenOnNetwork {
		t.Errorf("expected SEEN_ON_NETWORK, got %s", decoded.StatusType)
	}
	if decoded.TxID != "abc123" {
		t.Errorf("unexpected txid: %s", decoded.TxID)
	}
}
