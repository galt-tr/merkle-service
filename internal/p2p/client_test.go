package p2p

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/IBM/sarama"

	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
)

// mockSyncProducer implements sarama.SyncProducer for testing.
type mockSyncProducer struct {
	mu       sync.Mutex
	messages []*sarama.ProducerMessage
	failErr  error // if set, SendMessage returns this error
}

func (m *mockSyncProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failErr != nil {
		return 0, 0, m.failErr
	}
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
func (m *mockSyncProducer) TxnStatus() sarama.ProducerTxnStatusFlag {
	return sarama.ProducerTxnFlagReady
}
func (m *mockSyncProducer) BeginTxn() error   { return nil }
func (m *mockSyncProducer) CommitTxn() error   { return nil }
func (m *mockSyncProducer) AbortTxn() error    { return nil }
func (m *mockSyncProducer) AddOffsetsToTxn(_ map[string][]*sarama.PartitionOffsetMetadata, _ string) error {
	return nil
}
func (m *mockSyncProducer) AddMessageToTxn(_ *sarama.ConsumerMessage, _ string, _ *string) error {
	return nil
}

func (m *mockSyncProducer) getMessages() []*sarama.ProducerMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*sarama.ProducerMessage, len(m.messages))
	copy(result, m.messages)
	return result
}

// newTestClient creates a Client wired with mock Kafka producers for testing.
func newTestClient(t *testing.T) (*Client, *mockSyncProducer, *mockSyncProducer) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mockSubtreeProducer := &mockSyncProducer{}
	mockBlockProducer := &mockSyncProducer{}

	cfg := config.P2PConfig{
		SubtreeTopic: "test-subtree-topic",
		BlockTopic:   "test-block-topic",
	}

	client := NewClient(
		cfg,
		kafka.NewTestProducer(mockSubtreeProducer, "subtree-kafka-topic", logger),
		kafka.NewTestProducer(mockBlockProducer, "block-kafka-topic", logger),
		logger,
	)

	return client, mockSubtreeProducer, mockBlockProducer
}

// --- handleSubtreeMessage tests ---

func TestHandleSubtreeMessage_ValidJSON(t *testing.T) {
	client, mockProducer, _ := newTestClient(t)

	msg := kafka.SubtreeMessage{
		SubtreeID:   "subtree-abc",
		TxIDs:       []string{"tx1", "tx2", "tx3"},
		MerkleData:  []byte{0xDE, 0xAD},
		BlockHeight: 42,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal test message: %v", err)
	}

	client.handleSubtreeMessage(data)

	published := mockProducer.getMessages()
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}

	// Verify the key is the subtree ID.
	keyBytes, err := published[0].Key.Encode()
	if err != nil {
		t.Fatalf("failed to encode key: %v", err)
	}
	if string(keyBytes) != "subtree-abc" {
		t.Errorf("expected key 'subtree-abc', got %q", string(keyBytes))
	}

	// Verify the value deserializes back correctly.
	valueBytes, err := published[0].Value.Encode()
	if err != nil {
		t.Fatalf("failed to encode value: %v", err)
	}
	decoded, err := kafka.DecodeSubtreeMessage(valueBytes)
	if err != nil {
		t.Fatalf("failed to decode published subtree message: %v", err)
	}
	if decoded.SubtreeID != "subtree-abc" {
		t.Errorf("expected subtreeId 'subtree-abc', got %q", decoded.SubtreeID)
	}
	if len(decoded.TxIDs) != 3 {
		t.Errorf("expected 3 txids, got %d", len(decoded.TxIDs))
	}
	if decoded.BlockHeight != 42 {
		t.Errorf("expected blockHeight 42, got %d", decoded.BlockHeight)
	}
}

func TestHandleSubtreeMessage_EmptyTxIDs(t *testing.T) {
	client, mockProducer, _ := newTestClient(t)

	msg := kafka.SubtreeMessage{
		SubtreeID:   "subtree-empty",
		TxIDs:       []string{},
		BlockHeight: 1,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal test message: %v", err)
	}

	client.handleSubtreeMessage(data)

	published := mockProducer.getMessages()
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}

	valueBytes, err := published[0].Value.Encode()
	if err != nil {
		t.Fatalf("failed to encode value: %v", err)
	}
	decoded, err := kafka.DecodeSubtreeMessage(valueBytes)
	if err != nil {
		t.Fatalf("failed to decode published subtree message: %v", err)
	}
	if len(decoded.TxIDs) != 0 {
		t.Errorf("expected 0 txids, got %d", len(decoded.TxIDs))
	}
}

func TestHandleSubtreeMessage_InvalidJSON(t *testing.T) {
	client, mockProducer, _ := newTestClient(t)

	// Should not panic and should not publish anything.
	client.handleSubtreeMessage([]byte("not valid json{{{"))

	published := mockProducer.getMessages()
	if len(published) != 0 {
		t.Errorf("expected no published messages for invalid JSON, got %d", len(published))
	}
}

func TestHandleSubtreeMessage_EmptyData(t *testing.T) {
	client, mockProducer, _ := newTestClient(t)

	client.handleSubtreeMessage([]byte{})

	published := mockProducer.getMessages()
	if len(published) != 0 {
		t.Errorf("expected no published messages for empty data, got %d", len(published))
	}
}

func TestHandleSubtreeMessage_NilData(t *testing.T) {
	client, mockProducer, _ := newTestClient(t)

	client.handleSubtreeMessage(nil)

	published := mockProducer.getMessages()
	if len(published) != 0 {
		t.Errorf("expected no published messages for nil data, got %d", len(published))
	}
}

// --- handleBlockMessage tests ---

func TestHandleBlockMessage_ValidJSON(t *testing.T) {
	client, _, mockProducer := newTestClient(t)

	msg := kafka.BlockMessage{
		BlockHash:   "00000000abc123",
		BlockHeader: []byte{0x01, 0x02, 0x03},
		BlockHeight: 800000,
		SubtreeRefs: []string{"sub1", "sub2"},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal test message: %v", err)
	}

	client.handleBlockMessage(data)

	published := mockProducer.getMessages()
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}

	// Verify the key is the block hash.
	keyBytes, err := published[0].Key.Encode()
	if err != nil {
		t.Fatalf("failed to encode key: %v", err)
	}
	if string(keyBytes) != "00000000abc123" {
		t.Errorf("expected key '00000000abc123', got %q", string(keyBytes))
	}

	// Verify the value deserializes back correctly.
	valueBytes, err := published[0].Value.Encode()
	if err != nil {
		t.Fatalf("failed to encode value: %v", err)
	}
	decoded, err := kafka.DecodeBlockMessage(valueBytes)
	if err != nil {
		t.Fatalf("failed to decode published block message: %v", err)
	}
	if decoded.BlockHash != "00000000abc123" {
		t.Errorf("expected blockHash '00000000abc123', got %q", decoded.BlockHash)
	}
	if decoded.BlockHeight != 800000 {
		t.Errorf("expected blockHeight 800000, got %d", decoded.BlockHeight)
	}
	if len(decoded.SubtreeRefs) != 2 {
		t.Errorf("expected 2 subtreeRefs, got %d", len(decoded.SubtreeRefs))
	}
	if decoded.SubtreeRefs[0] != "sub1" || decoded.SubtreeRefs[1] != "sub2" {
		t.Errorf("unexpected subtreeRefs: %v", decoded.SubtreeRefs)
	}
}

func TestHandleBlockMessage_NoSubtreeRefs(t *testing.T) {
	client, _, mockProducer := newTestClient(t)

	msg := kafka.BlockMessage{
		BlockHash:   "blockhash-no-refs",
		BlockHeader: []byte{0xFF},
		BlockHeight: 1,
		SubtreeRefs: []string{},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal test message: %v", err)
	}

	client.handleBlockMessage(data)

	published := mockProducer.getMessages()
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}

	valueBytes, err := published[0].Value.Encode()
	if err != nil {
		t.Fatalf("failed to encode value: %v", err)
	}
	decoded, err := kafka.DecodeBlockMessage(valueBytes)
	if err != nil {
		t.Fatalf("failed to decode published block message: %v", err)
	}
	if len(decoded.SubtreeRefs) != 0 {
		t.Errorf("expected 0 subtreeRefs, got %d", len(decoded.SubtreeRefs))
	}
}

func TestHandleBlockMessage_InvalidJSON(t *testing.T) {
	client, _, mockProducer := newTestClient(t)

	// Should not panic and should not publish anything.
	client.handleBlockMessage([]byte("{invalid json"))

	published := mockProducer.getMessages()
	if len(published) != 0 {
		t.Errorf("expected no published messages for invalid JSON, got %d", len(published))
	}
}

func TestHandleBlockMessage_EmptyData(t *testing.T) {
	client, _, mockProducer := newTestClient(t)

	client.handleBlockMessage([]byte{})

	published := mockProducer.getMessages()
	if len(published) != 0 {
		t.Errorf("expected no published messages for empty data, got %d", len(published))
	}
}

func TestHandleBlockMessage_NilData(t *testing.T) {
	client, _, mockProducer := newTestClient(t)

	client.handleBlockMessage(nil)

	published := mockProducer.getMessages()
	if len(published) != 0 {
		t.Errorf("expected no published messages for nil data, got %d", len(published))
	}
}

// --- Health tests (does not require libp2p) ---

func TestHealth_InitialState(t *testing.T) {
	client, _, _ := newTestClient(t)

	health := client.Health()
	if health.Name != "p2p-client" {
		t.Errorf("expected name 'p2p-client', got %q", health.Name)
	}
	// Initially not connected and no peers, so should be unhealthy or degraded.
	if health.Status == "healthy" {
		t.Error("expected non-healthy status for unconnected client")
	}
}

func TestHealth_Connected_NoPeers(t *testing.T) {
	client, _, _ := newTestClient(t)
	client.setConnected(true)
	client.mu.Lock()
	client.peerCount = 0
	client.mu.Unlock()

	health := client.Health()
	if health.Status != "degraded" {
		t.Errorf("expected status 'degraded' with 0 peers, got %q", health.Status)
	}
}

func TestHealth_Connected_WithPeers(t *testing.T) {
	client, _, _ := newTestClient(t)
	client.setConnected(true)
	client.mu.Lock()
	client.peerCount = 3
	client.mu.Unlock()

	health := client.Health()
	if health.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", health.Status)
	}
	if health.Details["peerCount"] != "3" {
		t.Errorf("expected peerCount '3', got %q", health.Details["peerCount"])
	}
}

func TestHealth_Disconnected(t *testing.T) {
	client, _, _ := newTestClient(t)
	client.setConnected(false)
	client.mu.Lock()
	client.peerCount = 5
	client.mu.Unlock()

	health := client.Health()
	// Disconnected overrides to unhealthy, but 0-peer check may also apply.
	// With peerCount=5 and disconnected, the status should be unhealthy.
	if health.Details["connection"] != "disconnected" {
		t.Errorf("expected connection 'disconnected', got %q", health.Details["connection"])
	}
}

// --- Init tests ---

func TestInit_MissingSubtreeProducer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockProducer := &mockSyncProducer{}

	client := NewClient(
		config.P2PConfig{SubtreeTopic: "st", BlockTopic: "bt"},
		nil, // missing subtree producer
		kafka.NewTestProducer(mockProducer, "block", logger),
		logger,
	)

	err := client.Init(nil)
	if err == nil {
		t.Fatal("expected error for nil subtree producer")
	}
}

func TestInit_MissingBlockProducer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockProducer := &mockSyncProducer{}

	client := NewClient(
		config.P2PConfig{SubtreeTopic: "st", BlockTopic: "bt"},
		kafka.NewTestProducer(mockProducer, "subtree", logger),
		nil, // missing block producer
		logger,
	)

	err := client.Init(nil)
	if err == nil {
		t.Fatal("expected error for nil block producer")
	}
}

func TestInit_MissingSubtreeTopic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockSP := &mockSyncProducer{}
	mockBP := &mockSyncProducer{}

	client := NewClient(
		config.P2PConfig{SubtreeTopic: "", BlockTopic: "bt"},
		kafka.NewTestProducer(mockSP, "subtree", logger),
		kafka.NewTestProducer(mockBP, "block", logger),
		logger,
	)

	err := client.Init(nil)
	if err == nil {
		t.Fatal("expected error for empty subtree topic")
	}
}

func TestInit_MissingBlockTopic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockSP := &mockSyncProducer{}
	mockBP := &mockSyncProducer{}

	client := NewClient(
		config.P2PConfig{SubtreeTopic: "st", BlockTopic: ""},
		kafka.NewTestProducer(mockSP, "subtree", logger),
		kafka.NewTestProducer(mockBP, "block", logger),
		logger,
	)

	err := client.Init(nil)
	if err == nil {
		t.Fatal("expected error for empty block topic")
	}
}

func TestInit_Success(t *testing.T) {
	client, _, _ := newTestClient(t)

	err := client.Init(nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// --- Multiple messages test ---

func TestHandleSubtreeMessage_MultipleMessages(t *testing.T) {
	client, mockProducer, _ := newTestClient(t)

	for i, id := range []string{"sub-1", "sub-2", "sub-3"} {
		msg := kafka.SubtreeMessage{
			SubtreeID:   id,
			BlockHeight: uint64(i + 1),
		}
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("failed to marshal message %d: %v", i, err)
		}
		client.handleSubtreeMessage(data)
	}

	published := mockProducer.getMessages()
	if len(published) != 3 {
		t.Fatalf("expected 3 published messages, got %d", len(published))
	}

	// Verify keys match expected subtree IDs.
	expectedKeys := []string{"sub-1", "sub-2", "sub-3"}
	for i, pm := range published {
		keyBytes, err := pm.Key.Encode()
		if err != nil {
			t.Fatalf("failed to encode key %d: %v", i, err)
		}
		if string(keyBytes) != expectedKeys[i] {
			t.Errorf("message %d: expected key %q, got %q", i, expectedKeys[i], string(keyBytes))
		}
	}
}

func TestHandleBlockMessage_MultipleMessages(t *testing.T) {
	client, _, mockProducer := newTestClient(t)

	for i, hash := range []string{"hash-a", "hash-b"} {
		msg := kafka.BlockMessage{
			BlockHash:   hash,
			BlockHeight: uint64(i + 100),
		}
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("failed to marshal message %d: %v", i, err)
		}
		client.handleBlockMessage(data)
	}

	published := mockProducer.getMessages()
	if len(published) != 2 {
		t.Fatalf("expected 2 published messages, got %d", len(published))
	}
}

// --- Partial/extra JSON fields test ---

func TestHandleSubtreeMessage_ExtraJSONFields(t *testing.T) {
	client, mockProducer, _ := newTestClient(t)

	// JSON with extra fields that are not in the struct should not cause an error.
	data := []byte(`{"subtreeId":"sub-extra","txids":["tx1"],"merkleData":"AQID","blockHeight":10,"unknownField":"value"}`)

	client.handleSubtreeMessage(data)

	published := mockProducer.getMessages()
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}
}

func TestHandleBlockMessage_ExtraJSONFields(t *testing.T) {
	client, _, mockProducer := newTestClient(t)

	data := []byte(`{"blockHash":"hash-extra","blockHeader":"AQ==","blockHeight":99,"subtreeRefs":["s1"],"extra":true}`)

	client.handleBlockMessage(data)

	published := mockProducer.getMessages()
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}
}
