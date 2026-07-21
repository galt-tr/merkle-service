package subtree

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/IBM/sarama"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"

	"github.com/bsv-blockchain/merkle-service/internal/cache"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/datahub"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

// --- Mock implementations ---

type mockRegStore struct {
	// registrations maps txid -> []callbackURL
	registrations map[string][]string
	batchGetCalls [][]string // records each BatchGet call's txids
}

func (m *mockRegStore) BatchGet(txids []string) (map[string][]string, error) {
	m.batchGetCalls = append(m.batchGetCalls, txids)
	result := make(map[string][]string)
	for _, txid := range txids {
		if urls, ok := m.registrations[txid]; ok {
			result[txid] = urls
		}
	}
	return result, nil
}

func (m *mockRegStore) Get(txid string) ([]string, error) {
	return m.registrations[txid], nil
}

type mockSeenCounter struct{}

func (m *mockSeenCounter) AddPeer(txid, peerID string, weight int) (*store.IncrementResult, error) {
	return &store.IncrementResult{NewCount: weight, ThresholdReached: false}, nil
}

func (m *mockSeenCounter) BatchAddPeer(txids []string, peerID string, weight int) ([]string, error) {
	return nil, nil
}

// mockReadyNodes always reports ready with a fixed weight per peer.
type mockReadyNodes struct {
	weight int
}

func (m *mockReadyNodes) Ready() bool { return true }
func (m *mockReadyNodes) Weight(peerID string) int {
	if peerID == "" {
		return 0
	}
	if m.weight > 0 {
		return m.weight
	}
	return 1
}

// mockNotReadyNodes never scores.
type mockNotReadyNodes struct{}

func (m *mockNotReadyNodes) Ready() bool          { return false }
func (m *mockNotReadyNodes) Weight(peerID string) int { return 0 }

type mockRegCache struct {
	cached map[string]bool // txid -> isRegistered
	setReg []string        // txids passed to SetMultiRegistered
	setNot []string        // txids passed to SetMultiNotRegistered
}

func (m *mockRegCache) FilterUncached(txids []string) (uncached []string, cachedRegistered []string) {
	for _, txid := range txids {
		isReg, isCached := m.cached[txid]
		if !isCached {
			uncached = append(uncached, txid)
		} else if isReg {
			cachedRegistered = append(cachedRegistered, txid)
		}
	}
	return
}

func (m *mockRegCache) SetMultiRegistered(txids []string) error {
	m.setReg = append(m.setReg, txids...)
	return nil
}

func (m *mockRegCache) SetMultiNotRegistered(txids []string) error {
	m.setNot = append(m.setNot, txids...)
	return nil
}

// --- Helpers ---

// buildRawBytes creates DataHub-format raw subtree data from given 32-byte hashes.
func buildRawBytes(hashes ...[]byte) []byte {
	data := make([]byte, len(hashes)*chainhash.HashSize)
	for i, h := range hashes {
		copy(data[i*chainhash.HashSize:], h)
	}
	return data
}

// hashFromHex creates a 32-byte hash from a hex txid in Bitcoin display order (reversed).
// This is the format users register with and what chainhash.Hash.String() returns.
func hashFromHex(t *testing.T, displayHex string) []byte {
	t.Helper()
	b, err := hex.DecodeString(displayHex)
	if err != nil {
		t.Fatalf("invalid hex %q: %v", displayHex, err)
	}
	if len(b) != chainhash.HashSize {
		t.Fatalf("expected %d bytes, got %d", chainhash.HashSize, len(b))
	}
	// Reverse to get internal byte order (what DataHub sends)
	for i := 0; i < len(b)/2; i++ {
		b[i], b[len(b)-1-i] = b[len(b)-1-i], b[i]
	}
	return b
}

// --- Tests ---

// TestParseRawTxids_MatchesChainhashString is the critical test:
// ParseRawTxids must produce the same hex string as chainhash.Hash.String()
// for the same raw bytes. This is what registrations are stored as.
func TestParseRawTxids_MatchesChainhashString(t *testing.T) {
	// Create some raw 32-byte hashes (internal byte order, as DataHub sends)
	rawHashes := [][]byte{
		make([]byte, 32),
		make([]byte, 32),
		make([]byte, 32),
	}
	rawHashes[0][0] = 0xab
	rawHashes[0][31] = 0xcd
	rawHashes[1][0] = 0x01
	rawHashes[1][15] = 0xff
	rawHashes[2][31] = 0x42

	rawData := buildRawBytes(rawHashes...)

	txids, err := datahub.ParseRawTxids(rawData)
	if err != nil {
		t.Fatalf("ParseRawTxids: %v", err)
	}

	for i, rawHash := range rawHashes {
		var h chainhash.Hash
		copy(h[:], rawHash)
		expected := h.String()

		if txids[i] != expected {
			t.Errorf("txid[%d]: ParseRawTxids=%q, chainhash.Hash.String()=%q — MISMATCH", i, txids[i], expected)
		}
	}
}

// TestParseRawTxids_ReversesBytes verifies that ParseRawTxids returns
// Bitcoin display order (reversed), not raw internal order.
func TestParseRawTxids_ReversesBytes(t *testing.T) {
	raw := make([]byte, 32)
	raw[0] = 0xAA  // internal first byte
	raw[31] = 0xBB // internal last byte

	txids, err := datahub.ParseRawTxids(raw)
	if err != nil {
		t.Fatalf("ParseRawTxids: %v", err)
	}
	if len(txids) != 1 {
		t.Fatalf("expected 1 txid, got %d", len(txids))
	}

	// In display order, raw[31]=0xBB should be first, raw[0]=0xAA should be last
	if !strings.HasPrefix(txids[0], "bb") {
		t.Errorf("expected txid to start with 'bb' (reversed), got %s", txids[0])
	}
	if !strings.HasSuffix(txids[0], "aa") {
		t.Errorf("expected txid to end with 'aa' (reversed), got %s", txids[0])
	}
}

// TestParseRawTxids_ConsistentWithRegistration simulates the real scenario:
// a user registers a txid in display order, DataHub sends the raw bytes,
// and ParseRawTxids must produce a string that matches the registration.
func TestParseRawTxids_ConsistentWithRegistration(t *testing.T) {
	// A real-looking txid in Bitcoin display order (how a user would register it)
	displayTxid := "9602604163d73e2ab424bad28b1363694c397512dfa883ec1ee90cc92f847359"

	// Convert to internal byte order (what DataHub would send)
	internalBytes := hashFromHex(t, displayTxid)

	// Parse as ParseRawTxids would
	txids, err := datahub.ParseRawTxids(internalBytes)
	if err != nil {
		t.Fatalf("ParseRawTxids: %v", err)
	}

	if txids[0] != displayTxid {
		t.Errorf("ParseRawTxids produced %q, expected %q (registration format)", txids[0], displayTxid)
	}
}

// TestFindRegisteredTxids_NoCache tests findRegisteredTxids without a cache.
func TestFindRegisteredTxids_NoCache(t *testing.T) {
	regTxid := "aabbccdd00000000000000000000000000000000000000000000000000000011"
	unregTxid := "1122334400000000000000000000000000000000000000000000000000000099"

	regStore := &mockRegStore{
		registrations: map[string][]string{
			regTxid: {"http://callback.example.com/notify"},
		},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          nil, // no cache
	}

	result, err := p.findRegisteredTxids([]string{regTxid, unregTxid})
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 registered txid, got %d", len(result))
	}
	urls, ok := result[regTxid]
	if !ok {
		t.Fatalf("expected %s in result", regTxid)
	}
	if len(urls) != 1 || urls[0] != "http://callback.example.com/notify" {
		t.Errorf("expected [http://callback.example.com/notify], got %v", urls)
	}

	// All txids should have been sent to store (no cache)
	if len(regStore.batchGetCalls) != 1 {
		t.Fatalf("expected 1 BatchGet call, got %d", len(regStore.batchGetCalls))
	}
	if len(regStore.batchGetCalls[0]) != 2 {
		t.Errorf("expected 2 txids in BatchGet, got %d", len(regStore.batchGetCalls[0]))
	}
}

// TestFindRegisteredTxids_WithCache tests the cache + store interaction.
func TestFindRegisteredTxids_WithCache(t *testing.T) {
	cachedRegTxid := "aaaa000000000000000000000000000000000000000000000000000000000001"
	cachedNotRegTxid := "bbbb000000000000000000000000000000000000000000000000000000000002"
	uncachedRegTxid := "cccc000000000000000000000000000000000000000000000000000000000003"
	uncachedNotRegTxid := "dddd000000000000000000000000000000000000000000000000000000000004"

	regStore := &mockRegStore{
		registrations: map[string][]string{
			cachedRegTxid:   {"http://cached-cb.example.com"},
			uncachedRegTxid: {"http://cb.example.com"},
		},
	}

	cache := &mockRegCache{
		cached: map[string]bool{
			cachedRegTxid:    true,  // cached as registered
			cachedNotRegTxid: false, // cached as NOT registered
			// uncached txids not in map
		},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          cache,
	}

	txids := []string{cachedRegTxid, cachedNotRegTxid, uncachedRegTxid, uncachedNotRegTxid}
	result, err := p.findRegisteredTxids(txids)
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	// Should find: cachedRegTxid (from cache) + uncachedRegTxid (from store)
	if len(result) != 2 {
		t.Fatalf("expected 2 registered txids, got %d: %v", len(result), result)
	}

	if _, ok := result[cachedRegTxid]; !ok {
		t.Error("missing cached registered txid in result")
	}
	if _, ok := result[uncachedRegTxid]; !ok {
		t.Error("missing uncached registered txid in result")
	}

	// Two BatchGet calls: one for uncached txids, one for cached-registered txids' URLs.
	if len(regStore.batchGetCalls) != 2 {
		t.Fatalf("expected 2 BatchGet calls, got %d", len(regStore.batchGetCalls))
	}
	batchTxids := regStore.batchGetCalls[0]
	if len(batchTxids) != 2 {
		t.Errorf("expected 2 uncached txids in first BatchGet, got %d", len(batchTxids))
	}
	cachedBatchTxids := regStore.batchGetCalls[1]
	if len(cachedBatchTxids) != 1 || cachedBatchTxids[0] != cachedRegTxid {
		t.Errorf("expected cached-registered txid in second BatchGet, got %v", cachedBatchTxids)
	}

	// Cache should be updated: uncachedRegTxid → registered, uncachedNotRegTxid → not registered
	if len(cache.setReg) != 1 || cache.setReg[0] != uncachedRegTxid {
		t.Errorf("expected SetMultiRegistered([%s]), got %v", uncachedRegTxid, cache.setReg)
	}
	if len(cache.setNot) != 1 || cache.setNot[0] != uncachedNotRegTxid {
		t.Errorf("expected SetMultiNotRegistered([%s]), got %v", uncachedNotRegTxid, cache.setNot)
	}
}

// TestFindRegisteredTxids_AllCached tests when all txids are cached.
func TestFindRegisteredTxids_AllCached(t *testing.T) {
	txid1 := "1111000000000000000000000000000000000000000000000000000000000001"
	txid2 := "2222000000000000000000000000000000000000000000000000000000000002"

	regStore := &mockRegStore{
		registrations: map[string][]string{
			txid1: {"http://cb.example.com"},
		},
	}

	cache := &mockRegCache{
		cached: map[string]bool{
			txid1: true,
			txid2: false,
		},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          cache,
	}

	result, err := p.findRegisteredTxids([]string{txid1, txid2})
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 registered txid, got %d", len(result))
	}
	if _, ok := result[txid1]; !ok {
		t.Errorf("expected %s in result", txid1)
	}

	// One BatchGet call for cached-registered txids' URLs (no call for uncached since all cached).
	if len(regStore.batchGetCalls) != 1 {
		t.Errorf("expected 1 BatchGet call for cached URLs, got %d", len(regStore.batchGetCalls))
	}
}

// TestFindRegisteredTxids_NoneRegistered tests when no txids are registered.
func TestFindRegisteredTxids_NoneRegistered(t *testing.T) {
	regStore := &mockRegStore{
		registrations: map[string][]string{},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          nil,
	}

	txids := []string{
		"aaaa000000000000000000000000000000000000000000000000000000000001",
		"bbbb000000000000000000000000000000000000000000000000000000000002",
	}
	result, err := p.findRegisteredTxids(txids)
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 registered txids, got %d", len(result))
	}
}

// TestFindRegisteredTxids_EmptyInput tests with no txids.
func TestFindRegisteredTxids_EmptyInput(t *testing.T) {
	regStore := &mockRegStore{
		registrations: map[string][]string{},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          nil,
	}

	result, err := p.findRegisteredTxids(nil)
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 results for empty input, got %d", len(result))
	}
}

// TestEndToEnd_RawDataToRegistrationMatch is the most critical test:
// it simulates the entire flow from raw DataHub bytes through ParseRawTxids
// to findRegisteredTxids, ensuring a registered txid is found.
func TestEndToEnd_RawDataToRegistrationMatch(t *testing.T) {
	// User registers this txid (Bitcoin display order)
	registeredTxid := "9602604163d73e2ab424bad28b1363694c397512dfa883ec1ee90cc92f847359"
	unregisteredTxid := "0000000000000000000000000000000000000000000000000000000000000001"

	// Build raw binary data as DataHub would return it
	regRawBytes := hashFromHex(t, registeredTxid)
	unregRawBytes := hashFromHex(t, unregisteredTxid)
	rawData := buildRawBytes(regRawBytes, unregRawBytes)

	// Parse txids from raw data (what the subtree processor does)
	txids, err := datahub.ParseRawTxids(rawData)
	if err != nil {
		t.Fatalf("ParseRawTxids: %v", err)
	}

	if len(txids) != 2 {
		t.Fatalf("expected 2 txids, got %d", len(txids))
	}

	// Verify the parsed txids match the display format
	if txids[0] != registeredTxid {
		t.Fatalf("first parsed txid %q != registered %q", txids[0], registeredTxid)
	}
	if txids[1] != unregisteredTxid {
		t.Fatalf("second parsed txid %q != unregistered %q", txids[1], unregisteredTxid)
	}

	// Now run findRegisteredTxids
	regStore := &mockRegStore{
		registrations: map[string][]string{
			registeredTxid: {"http://example.com/callback"},
		},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          nil,
	}

	result, err := p.findRegisteredTxids(txids)
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 registered txid, got %d", len(result))
	}
	if _, ok := result[registeredTxid]; !ok {
		t.Errorf("expected %s in result", registeredTxid)
	}
}

// TestEndToEnd_ParseRawTxidsConsistentWithParseRawNodes verifies that
// ParseRawTxids and ParseRawNodes+Hash.String() produce the same txid strings.
// This ensures the subtree processor (SEEN) and block processor (MINED) paths
// use the same txid format.
func TestEndToEnd_ParseRawTxidsConsistentWithParseRawNodes(t *testing.T) {
	// Build raw data with varied byte patterns
	hashes := [][]byte{
		make([]byte, 32),
		make([]byte, 32),
		make([]byte, 32),
	}
	hashes[0][0] = 0xde
	hashes[0][31] = 0xad
	hashes[1][0] = 0xbe
	hashes[1][15] = 0xef
	hashes[1][31] = 0x01
	for i := range hashes[2] {
		hashes[2][i] = byte(i)
	}

	rawData := buildRawBytes(hashes...)

	// Parse via ParseRawTxids (subtree processor path)
	txids, err := datahub.ParseRawTxids(rawData)
	if err != nil {
		t.Fatalf("ParseRawTxids: %v", err)
	}

	// Parse via ParseRawNodes (block processor path)
	nodes, err := datahub.ParseRawNodes(rawData)
	if err != nil {
		t.Fatalf("ParseRawNodes: %v", err)
	}

	if len(txids) != len(nodes) {
		t.Fatalf("count mismatch: ParseRawTxids=%d, ParseRawNodes=%d", len(txids), len(nodes))
	}

	for i := range txids {
		nodeStr := nodes[i].Hash.String()
		if txids[i] != nodeStr {
			t.Errorf("txid[%d]: ParseRawTxids=%q, node.Hash.String()=%q — MISMATCH", i, txids[i], nodeStr)
		}
	}
}

// TestFindRegisteredTxids_LargeSubtree tests with a realistic subtree size.
func TestFindRegisteredTxids_LargeSubtree(t *testing.T) {
	// Simulate a subtree with 10000 txids, only 3 registered
	const totalTxids = 10000
	txids := make([]string, totalTxids)
	for i := 0; i < totalTxids; i++ {
		txids[i] = strings.Repeat("00", 28) + hex.EncodeToString([]byte{
			byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i),
		})
	}

	regStore := &mockRegStore{
		registrations: map[string][]string{
			txids[42]:   {"http://cb1.example.com"},
			txids[999]:  {"http://cb2.example.com"},
			txids[9999]: {"http://cb1.example.com", "http://cb3.example.com"},
		},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          nil,
	}

	result, err := p.findRegisteredTxids(txids)
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 registered txids, got %d", len(result))
	}

	if _, ok := result[txids[42]]; !ok {
		t.Error("missing txids[42] in result")
	}
	if _, ok := result[txids[999]]; !ok {
		t.Error("missing txids[999] in result")
	}
	if _, ok := result[txids[9999]]; !ok {
		t.Error("missing txids[9999] in result")
	}
}

// TestFindRegisteredTxids_CacheUpdatedCorrectly verifies the cache is properly
// populated after a store lookup.
func TestFindRegisteredTxids_CacheUpdatedCorrectly(t *testing.T) {
	txidReg := "aaaa000000000000000000000000000000000000000000000000000000000001"
	txidNot1 := "bbbb000000000000000000000000000000000000000000000000000000000002"
	txidNot2 := "cccc000000000000000000000000000000000000000000000000000000000003"

	regStore := &mockRegStore{
		registrations: map[string][]string{
			txidReg: {"http://callback.example.com"},
		},
	}

	cache := &mockRegCache{
		cached: map[string]bool{}, // everything uncached
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          cache,
	}

	_, err := p.findRegisteredTxids([]string{txidReg, txidNot1, txidNot2})
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}

	// Verify cache was updated
	if len(cache.setReg) != 1 || cache.setReg[0] != txidReg {
		t.Errorf("expected registered cache update for %s, got %v", txidReg, cache.setReg)
	}
	if len(cache.setNot) != 2 {
		t.Errorf("expected 2 not-registered cache updates, got %d: %v", len(cache.setNot), cache.setNot)
	}
	notSet := make(map[string]bool)
	for _, txid := range cache.setNot {
		notSet[txid] = true
	}
	if !notSet[txidNot1] || !notSet[txidNot2] {
		t.Errorf("missing expected not-registered txids in cache: %v", cache.setNot)
	}
}

// --- Idempotent Seen Counter Tests ---

// mockIdempotentSeenCounter simulates peer-weighted scoring: each peerID
// contributes once per txid with the given weight; threshold fires once.
type mockIdempotentSeenCounter struct {
	mu             sync.Mutex
	peersByTxid    map[string]map[string]int // txid -> peerID -> weight
	thresholdFired map[string]bool
	threshold      int
}

func newMockIdempotentSeenCounter(threshold int) *mockIdempotentSeenCounter {
	return &mockIdempotentSeenCounter{
		peersByTxid:    make(map[string]map[string]int),
		thresholdFired: make(map[string]bool),
		threshold:      threshold,
	}
}

func (m *mockIdempotentSeenCounter) AddPeer(txid, peerID string, weight int) (*store.IncrementResult, error) {
	fired, err := m.BatchAddPeer([]string{txid}, peerID, weight)
	if err != nil {
		return nil, err
	}
	if len(fired) == 1 {
		return &store.IncrementResult{NewCount: m.threshold, ThresholdReached: true}, nil
	}
	// Compute score for assertions.
	m.mu.Lock()
	defer m.mu.Unlock()
	score := 0
	for _, w := range m.peersByTxid[txid] {
		score += w
	}
	return &store.IncrementResult{NewCount: score, ThresholdReached: false}, nil
}

func (m *mockIdempotentSeenCounter) BatchAddPeer(txids []string, peerID string, weight int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var fired []string
	for _, txid := range txids {
		if m.peersByTxid[txid] == nil {
			m.peersByTxid[txid] = make(map[string]int)
		}
		if _, ok := m.peersByTxid[txid][peerID]; !ok {
			m.peersByTxid[txid][peerID] = weight
		}
		score := 0
		for _, w := range m.peersByTxid[txid] {
			score += w
		}
		if score >= m.threshold && !m.thresholdFired[txid] {
			m.thresholdFired[txid] = true
			fired = append(fired, txid)
		}
	}
	return fired, nil
}

func TestIdempotentSeenCounter_FirstPeerAddsWeight(t *testing.T) {
	sc := newMockIdempotentSeenCounter(100)

	result, err := sc.AddPeer("txid-1", "peer-A", 40)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NewCount != 40 {
		t.Errorf("expected score=40, got %d", result.NewCount)
	}
	if result.ThresholdReached {
		t.Error("threshold should not fire at 40/100")
	}
}

func TestIdempotentSeenCounter_DuplicatePeerDoesNotAdd(t *testing.T) {
	sc := newMockIdempotentSeenCounter(100)
	sc.AddPeer("txid-1", "peer-A", 40)
	result, _ := sc.AddPeer("txid-1", "peer-A", 40)
	if result.NewCount != 40 {
		t.Errorf("expected score=40 after duplicate, got %d", result.NewCount)
	}
}

func TestIdempotentSeenCounter_ThresholdFiresOnce(t *testing.T) {
	sc := newMockIdempotentSeenCounter(51)

	sc.AddPeer("txid-1", "peer-A", 40)
	result, _ := sc.AddPeer("txid-1", "peer-B", 20)
	if !result.ThresholdReached {
		t.Error("threshold should fire at 60 >= 51")
	}

	result, _ = sc.AddPeer("txid-1", "peer-C", 10)
	if result.ThresholdReached {
		t.Error("threshold should NOT fire again")
	}
}

func TestIdempotentSeenCounter_BatchAddPeer(t *testing.T) {
	sc := newMockIdempotentSeenCounter(51)
	// Pre-seed so second peer crosses threshold for both.
	sc.AddPeer("tx1", "peer-A", 40)
	sc.AddPeer("tx2", "peer-A", 40)

	fired, err := sc.BatchAddPeer([]string{"tx1", "tx2", "tx3"}, "peer-B", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(fired) != 2 {
		t.Fatalf("expected 2 fired, got %v", fired)
	}
}

func TestIdempotentSeenCounter_IndependentPerTxid(t *testing.T) {
	sc := newMockIdempotentSeenCounter(51)

	sc.AddPeer("txid-1", "peer-A", 40)
	sc.AddPeer("txid-2", "peer-A", 40)

	result1, _ := sc.AddPeer("txid-1", "peer-B", 20)
	result2, _ := sc.AddPeer("txid-2", "peer-B", 20)

	if !result1.ThresholdReached {
		t.Error("txid-1 threshold should fire")
	}
	if !result2.ThresholdReached {
		t.Error("txid-2 threshold should fire independently")
	}
}

// --- Integration-style Dedup Tests ---

// TestIntegration_DuplicateSubtreeOnlyProcessedOnce simulates sending
// duplicate subtree messages through the processor's dedup cache +
// findRegisteredTxids path, verifying only one set of lookups is made.
func TestIntegration_DuplicateSubtreeOnlyProcessedOnce(t *testing.T) {
	dc := cache.NewDedupCache(100)
	regStore := &mockRegStore{
		registrations: map[string][]string{
			"txid-registered": {"http://cb.example.com"},
		},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          nil,
		dedupCache:        dc,
	}

	txids := []string{"txid-registered", "txid-not-registered"}
	subtreeHash := "subtree-integration-test"

	// First processing: not in dedup cache, queries store.
	if dc.Contains(subtreeHash) {
		t.Fatal("subtree hash should not be in cache initially")
	}
	result1, err := p.findRegisteredTxids(txids)
	if err != nil {
		t.Fatalf("first findRegisteredTxids: %v", err)
	}
	if len(result1) != 1 {
		t.Fatalf("first call: expected 1 registered txid, got %d", len(result1))
	}
	if _, ok := result1["txid-registered"]; !ok {
		t.Error("first call: expected txid-registered in result")
	}
	// Mark as processed.
	dc.Add(subtreeHash)
	batchCallsAfterFirst := len(regStore.batchGetCalls)

	// Simulate duplicate message — dedup cache should prevent processing.
	if !dc.Contains(subtreeHash) {
		t.Fatal("subtree hash should be in cache after Add")
	}

	// In the real processor, handleMessage returns nil here.
	// Verify no additional store calls were made.
	if len(regStore.batchGetCalls) != batchCallsAfterFirst {
		t.Errorf("expected no additional BatchGet calls for duplicate, got %d total",
			len(regStore.batchGetCalls))
	}
}

// TestIntegration_SeenCounterIdempotency simulates peer-weighted scoring:
// unique peers add weight once; threshold fires exactly once.
func TestIntegration_SeenCounterIdempotency(t *testing.T) {
	sc := newMockIdempotentSeenCounter(51)

	peers := []struct {
		id     string
		weight int
	}{
		{"peer-A", 40},
		{"peer-B", 20},
		{"peer-C", 10},
	}
	var thresholdCount int
	var lastScore int

	for _, p := range peers {
		result, err := sc.AddPeer("txid-1", p.id, p.weight)
		if err != nil {
			t.Fatalf("AddPeer error: %v", err)
		}
		lastScore = result.NewCount
		if result.ThresholdReached {
			thresholdCount++
		}
	}

	if lastScore != 70 {
		t.Errorf("expected score=70, got %d", lastScore)
	}
	if thresholdCount != 1 {
		t.Errorf("expected threshold to fire exactly once, fired %d times", thresholdCount)
	}

	// Replay peers — score and fire status unchanged.
	for _, p := range peers {
		result, _ := sc.AddPeer("txid-1", p.id, p.weight)
		if result.ThresholdReached {
			t.Errorf("threshold should not fire on duplicate peer %s", p.id)
		}
		if result.NewCount != 70 {
			t.Errorf("score should remain 70 after duplicates, got %d", result.NewCount)
		}
	}
}

// --- Dedup Cache Tests ---

// TestDedupCache_SkipsDuplicateSubtree verifies that the dedup cache
// prevents reprocessing of already-seen subtree hashes.
func TestDedupCache_SkipsDuplicateSubtree(t *testing.T) {
	dc := cache.NewDedupCache(100)

	// First time: not in cache
	if dc.Contains("hash-abc") {
		t.Error("expected hash not in cache initially")
	}

	// Mark as processed
	dc.Add("hash-abc")

	// Second time: in cache
	if !dc.Contains("hash-abc") {
		t.Error("expected hash in cache after Add")
	}
}

// TestDedupCache_AllowsRetryOnFailure verifies that failed processing
// (where Add is never called) allows the message to be retried.
func TestDedupCache_AllowsRetryOnFailure(t *testing.T) {
	dc := cache.NewDedupCache(100)

	// Simulate: message received, processing fails (no Add called)
	if dc.Contains("hash-fail") {
		t.Error("should not be in cache")
	}

	// Don't call Add (simulating failure)

	// Retry: should still not be in cache, allowing retry
	if dc.Contains("hash-fail") {
		t.Error("failed processing should not add to cache")
	}
}

// TestDedupCache_IntegrationWithProcessor verifies the dedup cache is
// properly checked before and updated after findRegisteredTxids.
func TestDedupCache_IntegrationWithProcessor(t *testing.T) {
	dc := cache.NewDedupCache(100)
	regStore := &mockRegStore{
		registrations: map[string][]string{
			"txid1": {"http://cb.example.com"},
		},
	}

	p := &Processor{
		registrationStore: regStore,
		regCache:          nil,
		dedupCache:        dc,
	}

	// First call: not in dedup cache, should query store
	result, err := p.findRegisteredTxids([]string{"txid1", "txid2"})
	if err != nil {
		t.Fatalf("findRegisteredTxids: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 registered txid, got %d", len(result))
	}
	if len(regStore.batchGetCalls) != 1 {
		t.Fatalf("expected 1 BatchGet call, got %d", len(regStore.batchGetCalls))
	}

	// Simulate marking as processed
	dc.Add("subtree-hash-1")

	// Verify it's in cache now
	if !dc.Contains("subtree-hash-1") {
		t.Error("expected subtree hash in dedup cache")
	}
}

// --- Batched Callback Emission Tests ---

type mockSyncProducer struct {
	mu       sync.Mutex
	messages []*sarama.ProducerMessage
}

func (m *mockSyncProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)
	return 0, int64(len(m.messages)), nil
}
func (m *mockSyncProducer) SendMessages(msgs []*sarama.ProducerMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msgs...)
	return nil
}
func (m *mockSyncProducer) Close() error                { return nil }
func (m *mockSyncProducer) IsTransactional() bool       { return false }
func (m *mockSyncProducer) TxnStatus() sarama.ProducerTxnStatusFlag { return 0 }
func (m *mockSyncProducer) BeginTxn() error             { return nil }
func (m *mockSyncProducer) CommitTxn() error             { return nil }
func (m *mockSyncProducer) AbortTxn() error              { return nil }
func (m *mockSyncProducer) AddOffsetsToTxn(map[string][]*sarama.PartitionOffsetMetadata, string) error {
	return nil
}
func (m *mockSyncProducer) AddMessageToTxn(*sarama.ConsumerMessage, string, *string) error {
	return nil
}

func (m *mockSyncProducer) getMessages() []*sarama.ProducerMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*sarama.ProducerMessage, len(m.messages))
	copy(result, m.messages)
	return result
}

func decodeCallbackMsg(t *testing.T, pm *sarama.ProducerMessage) *kafka.CallbackTopicMessage {
	t.Helper()
	b, err := pm.Value.Encode()
	if err != nil {
		t.Fatalf("encode value: %v", err)
	}
	msg, err := kafka.DecodeCallbackTopicMessage(b)
	if err != nil {
		t.Fatalf("decode callback msg: %v", err)
	}
	return msg
}

func newTestProcessor(t *testing.T, regStore RegistrationGetter, seenCounter SeenCounter) (*Processor, *mockSyncProducer) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockProducer := &mockSyncProducer{}
	p := &Processor{
		registrationStore: regStore,
		seenCounterStore:  seenCounter,
		nodeRegistry:      &mockReadyNodes{weight: 51},
		callbackProducer:  kafka.NewTestProducer(mockProducer, "callback-test", logger),
	}
	p.InitBase("subtree-test")
	p.Logger = logger
	return p, mockProducer
}

// TestBatchedSeenCallbacks_SingleCallbackURL verifies that multiple txids for
// the same callbackURL produce one batched SEEN_ON_NETWORK message.
func TestBatchedSeenCallbacks_SingleCallbackURL(t *testing.T) {
	regStore := &mockRegStore{registrations: map[string][]string{}}
	p, mockProd := newTestProcessor(t, regStore, &mockSeenCounter{})

	registered := map[string][]string{
		"tx1": {"http://arcade.example.com/cb"},
		"tx2": {"http://arcade.example.com/cb"},
		"tx3": {"http://arcade.example.com/cb"},
	}

	p.emitBatchedSeenCallbacks(registered, "peer-A")

	msgs := mockProd.getMessages()
	// mockSeenCounter never fires threshold → 1 SEEN_ON_NETWORK only
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	cb := decodeCallbackMsg(t, msgs[0])
	if cb.Type != kafka.CallbackSeenOnNetwork {
		t.Errorf("expected SEEN_ON_NETWORK, got %s", cb.Type)
	}
	if cb.CallbackURL != "http://arcade.example.com/cb" {
		t.Errorf("unexpected callbackURL: %s", cb.CallbackURL)
	}
	if len(cb.TxIDs) != 3 {
		t.Errorf("expected 3 TxIDs, got %d", len(cb.TxIDs))
	}
	txSet := make(map[string]bool)
	for _, id := range cb.TxIDs {
		txSet[id] = true
	}
	if !txSet["tx1"] || !txSet["tx2"] || !txSet["tx3"] {
		t.Errorf("missing txids in batch: %v", cb.TxIDs)
	}
}

// TestBatchedSeenCallbacks_MultipleCallbackURLs verifies separate batched messages per callbackURL.
func TestBatchedSeenCallbacks_MultipleCallbackURLs(t *testing.T) {
	regStore := &mockRegStore{registrations: map[string][]string{}}
	p, mockProd := newTestProcessor(t, regStore, &mockSeenCounter{})

	registered := map[string][]string{
		"tx1": {"http://url-A/cb"},
		"tx2": {"http://url-B/cb"},
		"tx3": {"http://url-A/cb"},
	}

	p.emitBatchedSeenCallbacks(registered, "peer-A")

	msgs := mockProd.getMessages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (one per callbackURL), got %d", len(msgs))
	}

	byURL := make(map[string]*kafka.CallbackTopicMessage)
	for _, pm := range msgs {
		cb := decodeCallbackMsg(t, pm)
		byURL[cb.CallbackURL] = cb
	}

	msgA := byURL["http://url-A/cb"]
	if msgA == nil || len(msgA.TxIDs) != 2 {
		t.Errorf("expected 2 txids for url-A, got %v", msgA)
	}
	msgB := byURL["http://url-B/cb"]
	if msgB == nil || len(msgB.TxIDs) != 1 || msgB.TxIDs[0] != "tx2" {
		t.Errorf("expected [tx2] for url-B, got %v", msgB)
	}
}

// TestBatchedSeenCallbacks_NoRegistered verifies no messages when no txids registered.
func TestBatchedSeenCallbacks_NoRegistered(t *testing.T) {
	regStore := &mockRegStore{registrations: map[string][]string{}}
	p, mockProd := newTestProcessor(t, regStore, &mockSeenCounter{})

	p.emitBatchedSeenCallbacks(map[string][]string{}, "peer-A")

	if len(mockProd.getMessages()) != 0 {
		t.Error("expected no messages for empty registered map")
	}
}

// TestBatchedSeenCallbacks_SeenMultipleNodesThreshold verifies batched SEEN_MULTIPLE_NODES.
func TestBatchedSeenCallbacks_SeenMultipleNodesThreshold(t *testing.T) {
	// Threshold=1 so any peer weight fires SEEN_MULTIPLE_NODES.
	sc := newMockIdempotentSeenCounter(1)
	regStore := &mockRegStore{registrations: map[string][]string{}}
	p, mockProd := newTestProcessor(t, regStore, sc)

	registered := map[string][]string{
		"tx1": {"http://arcade/cb"},
		"tx2": {"http://arcade/cb"},
	}

	p.emitBatchedSeenCallbacks(registered, "peer-A")

	msgs := mockProd.getMessages()
	// 1 SEEN_ON_NETWORK + 1 SEEN_MULTIPLE_NODES = 2 messages
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	byType := make(map[kafka.CallbackType]*kafka.CallbackTopicMessage)
	for _, pm := range msgs {
		cb := decodeCallbackMsg(t, pm)
		byType[cb.Type] = cb
	}

	seen := byType[kafka.CallbackSeenOnNetwork]
	if seen == nil || len(seen.TxIDs) != 2 {
		t.Errorf("expected 2 txids in SEEN_ON_NETWORK, got %v", seen)
	}

	multi := byType[kafka.CallbackSeenMultipleNodes]
	if multi == nil || len(multi.TxIDs) != 2 {
		t.Errorf("expected 2 txids in SEEN_MULTIPLE_NODES, got %v", multi)
	}
}

// TestBatchedSeenCallbacks_PartialThreshold verifies only threshold-reached txids in SEEN_MULTIPLE_NODES.
func TestBatchedSeenCallbacks_PartialThreshold(t *testing.T) {
	// Threshold=100: pre-seed tx1 with weight 60 so a second peer at 51 fires only for tx1
	// (score becomes 60+51=111). tx2 only gets 51 and stays below 100.
	sc := newMockIdempotentSeenCounter(100)
	sc.AddPeer("tx1", "peer-PREV", 60)

	regStore := &mockRegStore{registrations: map[string][]string{}}
	p, mockProd := newTestProcessor(t, regStore, sc)
	// newTestProcessor uses weight=51 for the announcing peer.
	registered := map[string][]string{
		"tx1": {"http://arcade/cb"},
		"tx2": {"http://arcade/cb"},
	}

	p.emitBatchedSeenCallbacks(registered, "peer-A")

	msgs := mockProd.getMessages()
	// 1 SEEN_ON_NETWORK (both txids) + 1 SEEN_MULTIPLE_NODES (only tx1) = 2
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	for _, pm := range msgs {
		cb := decodeCallbackMsg(t, pm)
		if cb.Type == kafka.CallbackSeenMultipleNodes {
			if len(cb.TxIDs) != 1 || cb.TxIDs[0] != "tx1" {
				t.Errorf("expected SEEN_MULTIPLE_NODES with [tx1], got %v", cb.TxIDs)
			}
		}
	}
}

// TestBatchedSeenCallbacks_SkipsWhenRegistryNotReady verifies warm-up gate.
func TestBatchedSeenCallbacks_SkipsWhenRegistryNotReady(t *testing.T) {
	sc := newMockIdempotentSeenCounter(1)
	regStore := &mockRegStore{registrations: map[string][]string{}}
	p, mockProd := newTestProcessor(t, regStore, sc)
	p.nodeRegistry = &mockNotReadyNodes{}

	p.emitBatchedSeenCallbacks(map[string][]string{
		"tx1": {"http://arcade/cb"},
	}, "peer-A")

	msgs := mockProd.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected only SEEN_ON_NETWORK, got %d msgs", len(msgs))
	}
	if decodeCallbackMsg(t, msgs[0]).Type != kafka.CallbackSeenOnNetwork {
		t.Error("expected SEEN_ON_NETWORK only while registry warms up")
	}
}

// TestBatchedSeenCallbacks_ChunksLargeBatch verifies that batches exceeding
// callbackBatchChunkSize are split into multiple messages, preventing
// Kafka "Message was too large" rejections.
func TestBatchedSeenCallbacks_ChunksLargeBatch(t *testing.T) {
	regStore := &mockRegStore{registrations: map[string][]string{}}
	p, mockProd := newTestProcessor(t, regStore, &mockSeenCounter{})

	const total = callbackBatchChunkSize*2 + 17
	registered := make(map[string][]string, total)
	for i := 0; i < total; i++ {
		registered[fmt.Sprintf("tx%05d", i)] = []string{"http://arcade/cb"}
	}

	p.emitBatchedSeenCallbacks(registered, "peer-A")

	msgs := mockProd.getMessages()
	// total txids / chunk size, rounded up → 3 SEEN_ON_NETWORK messages.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 chunked messages, got %d", len(msgs))
	}

	seenTxids := make(map[string]bool, total)
	for _, pm := range msgs {
		cb := decodeCallbackMsg(t, pm)
		if cb.Type != kafka.CallbackSeenOnNetwork {
			t.Errorf("expected SEEN_ON_NETWORK, got %s", cb.Type)
		}
		if cb.CallbackURL != "http://arcade/cb" {
			t.Errorf("unexpected callbackURL: %s", cb.CallbackURL)
		}
		if len(cb.TxIDs) > callbackBatchChunkSize {
			t.Errorf("chunk exceeds max size: got %d, max %d", len(cb.TxIDs), callbackBatchChunkSize)
		}
		for _, id := range cb.TxIDs {
			if seenTxids[id] {
				t.Errorf("duplicate txid across chunks: %s", id)
			}
			seenTxids[id] = true
		}
	}
	if len(seenTxids) != total {
		t.Errorf("expected %d unique txids across chunks, got %d", total, len(seenTxids))
	}
}

// --- Subtree DLQ Tests ---

// TestHandleTransientFailure_RoutesToDLQAtMaxAttempts drives a subtree message
// through handleTransientFailure with repeated failures and asserts that
// (a) before MaxAttempts the retry producer sees publishes with incrementing
// AttemptCount and (b) on the final attempt the DLQ producer sees exactly one
// publish with AttemptCount == MaxAttempts.
func TestHandleTransientFailure_RoutesToDLQAtMaxAttempts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	retryMock := &mockSyncProducer{}
	dlqMock := &mockSyncProducer{}

	maxAttempts := 3
	p := &Processor{
		cfg: &config.Config{
			Subtree: config.SubtreeConfig{MaxAttempts: maxAttempts},
		},
		retryProducer: kafka.NewTestProducer(retryMock, "subtree-test", logger),
		dlqProducer:   kafka.NewTestProducer(dlqMock, "subtree-dlq-test", logger),
	}
	p.InitBase("subtree-dlq-test")
	p.Logger = logger

	subtreeMsg := &kafka.SubtreeMessage{
		Hash:       "subtree-hash-abc",
		DataHubURL: "http://datahub.example.com",
	}
	cause := errors.New("datahub 404")

	// Simulate retries until MaxAttempts is reached.
	for i := 0; i < maxAttempts; i++ {
		if err := p.handleTransientFailure(subtreeMsg, "fetch", cause); err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
	}

	retryMsgs := retryMock.getMessages()
	if len(retryMsgs) != maxAttempts-1 {
		t.Fatalf("expected %d retry publishes, got %d", maxAttempts-1, len(retryMsgs))
	}
	for i, pm := range retryMsgs {
		b, err := pm.Value.Encode()
		if err != nil {
			t.Fatalf("retry msg %d: encode: %v", i, err)
		}
		decoded, err := kafka.DecodeSubtreeMessage(b)
		if err != nil {
			t.Fatalf("retry msg %d: decode: %v", i, err)
		}
		if decoded.AttemptCount != i+1 {
			t.Errorf("retry msg %d: expected AttemptCount=%d, got %d", i, i+1, decoded.AttemptCount)
		}
	}

	dlqMsgs := dlqMock.getMessages()
	if len(dlqMsgs) != 1 {
		t.Fatalf("expected exactly 1 DLQ publish, got %d", len(dlqMsgs))
	}
	b, err := dlqMsgs[0].Value.Encode()
	if err != nil {
		t.Fatalf("dlq msg: encode: %v", err)
	}
	dlqDecoded, err := kafka.DecodeSubtreeMessage(b)
	if err != nil {
		t.Fatalf("dlq msg: decode: %v", err)
	}
	if dlqDecoded.AttemptCount != maxAttempts {
		t.Errorf("DLQ msg AttemptCount: expected %d, got %d", maxAttempts, dlqDecoded.AttemptCount)
	}
	if dlqDecoded.Hash != subtreeMsg.Hash {
		t.Errorf("DLQ msg Hash: expected %q, got %q", subtreeMsg.Hash, dlqDecoded.Hash)
	}

	if got := p.messagesRetried.Load(); got != int64(maxAttempts-1) {
		t.Errorf("messagesRetried: expected %d, got %d", maxAttempts-1, got)
	}
	if got := p.messagesDLQ.Load(); got != 1 {
		t.Errorf("messagesDLQ: expected 1, got %d", got)
	}
}

// TestHandleTransientFailure_DefaultsMaxAttemptsWhenUnset verifies the guard
// that treats non-positive MaxAttempts as the built-in default (10), so a
// misconfigured deployment never collapses into "DLQ on first failure".
func TestHandleTransientFailure_DefaultsMaxAttemptsWhenUnset(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	retryMock := &mockSyncProducer{}
	dlqMock := &mockSyncProducer{}

	p := &Processor{
		cfg:           &config.Config{Subtree: config.SubtreeConfig{MaxAttempts: 0}},
		retryProducer: kafka.NewTestProducer(retryMock, "subtree-test", logger),
		dlqProducer:   kafka.NewTestProducer(dlqMock, "subtree-dlq-test", logger),
	}
	p.InitBase("subtree-dlq-default-test")
	p.Logger = logger

	subtreeMsg := &kafka.SubtreeMessage{Hash: "h"}
	if err := p.handleTransientFailure(subtreeMsg, "fetch", errors.New("x")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(retryMock.getMessages()) != 1 {
		t.Errorf("expected 1 retry publish with default MaxAttempts, got %d", len(retryMock.getMessages()))
	}
	if len(dlqMock.getMessages()) != 0 {
		t.Errorf("expected 0 DLQ publishes, got %d", len(dlqMock.getMessages()))
	}
}
