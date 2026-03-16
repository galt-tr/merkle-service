package subtree

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"

	"github.com/bsv-blockchain/merkle-service/internal/datahub"
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

func (m *mockSeenCounter) Increment(txid string) (*store.IncrementResult, error) {
	return &store.IncrementResult{NewCount: 1, ThresholdReached: false}, nil
}

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
	if result[0] != regTxid {
		t.Errorf("expected %s, got %s", regTxid, result[0])
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

	resultSet := make(map[string]bool)
	for _, txid := range result {
		resultSet[txid] = true
	}
	if !resultSet[cachedRegTxid] {
		t.Error("missing cached registered txid in result")
	}
	if !resultSet[uncachedRegTxid] {
		t.Error("missing uncached registered txid in result")
	}

	// Only uncached txids should be sent to the store
	if len(regStore.batchGetCalls) != 1 {
		t.Fatalf("expected 1 BatchGet call, got %d", len(regStore.batchGetCalls))
	}
	batchTxids := regStore.batchGetCalls[0]
	if len(batchTxids) != 2 {
		t.Errorf("expected 2 uncached txids in BatchGet, got %d", len(batchTxids))
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
		registrations: map[string][]string{},
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

	if len(result) != 1 || result[0] != txid1 {
		t.Errorf("expected [%s], got %v", txid1, result)
	}

	// No store calls should be made when everything is cached
	if len(regStore.batchGetCalls) != 0 {
		t.Errorf("expected 0 BatchGet calls when all cached, got %d", len(regStore.batchGetCalls))
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
		t.Errorf("expected 0 registered txids, got %d: %v", len(result), result)
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
		t.Fatalf("expected 1 registered txid, got %d: %v", len(result), result)
	}
	if result[0] != registeredTxid {
		t.Errorf("expected %s, got %s", registeredTxid, result[0])
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

	resultSet := make(map[string]bool)
	for _, txid := range result {
		resultSet[txid] = true
	}
	if !resultSet[txids[42]] || !resultSet[txids[999]] || !resultSet[txids[9999]] {
		t.Errorf("missing expected registered txids in result: %v", result)
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
