package callback

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"

	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
)

// mockSyncProducer implements sarama.SyncProducer for testing.
type mockSyncProducer struct {
	mu       sync.Mutex
	messages []*sarama.ProducerMessage
}

func (m *mockSyncProducer) SendMessage(msg *sarama.ProducerMessage) (partition int32, offset int64, err error) {
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

func (m *mockSyncProducer) Close() error { return nil }

func (m *mockSyncProducer) IsTransactional() bool { return false }

func (m *mockSyncProducer) TxnStatus() sarama.ProducerTxnStatusFlag {
	return sarama.ProducerTxnFlagReady
}

func (m *mockSyncProducer) BeginTxn() error   { return nil }
func (m *mockSyncProducer) CommitTxn() error   { return nil }
func (m *mockSyncProducer) AbortTxn() error    { return nil }
func (m *mockSyncProducer) AddOffsetsToTxn(offsets map[string][]*sarama.PartitionOffsetMetadata, groupId string) error {
	return nil
}
func (m *mockSyncProducer) AddMessageToTxn(msg *sarama.ConsumerMessage, groupId string, metadata *string) error {
	return nil
}

func (m *mockSyncProducer) getMessages() []*sarama.ProducerMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*sarama.ProducerMessage, len(m.messages))
	copy(result, m.messages)
	return result
}

// decodePublishedCallbackMessage extracts the CallbackTopicMessage from a captured ProducerMessage.
func decodePublishedCallbackMessage(t *testing.T, pm *sarama.ProducerMessage) *kafka.CallbackTopicMessage {
	t.Helper()
	valueBytes, err := pm.Value.Encode()
	if err != nil {
		t.Fatalf("failed to encode producer message value: %v", err)
	}
	msg, err := kafka.DecodeCallbackTopicMessage(valueBytes)
	if err != nil {
		t.Fatalf("failed to decode callback message from producer: %v", err)
	}
	return msg
}

// newTestDeliveryService creates a DeliveryService wired with mock producers and a custom HTTP client.
func newTestDeliveryService(t *testing.T, cfg *config.Config, httpClient *http.Client) (*DeliveryService, *mockSyncProducer, *mockSyncProducer) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mockRetryProducer := &mockSyncProducer{}
	mockDLQProducer := &mockSyncProducer{}

	ds := &DeliveryService{
		cfg:         cfg,
		httpClient:  httpClient,
		producer:    kafka.NewTestProducer(mockRetryProducer, cfg.Kafka.CallbackTopic, logger),
		dlqProducer: kafka.NewTestProducer(mockDLQProducer, cfg.Kafka.CallbackDLQTopic, logger),
		workCh:      make(chan *kafka.CallbackTopicMessage, 64),
	}
	ds.InitBase("callback-delivery-test")
	ds.Logger = logger

	// Start workers for handleMessage dispatch.
	workers := 4
	ds.workerWg.Add(workers)
	for i := 0; i < workers; i++ {
		go ds.deliveryWorker()
	}

	t.Cleanup(func() {
		close(ds.workCh)
		ds.workerWg.Wait()
	})

	return ds, mockRetryProducer, mockDLQProducer
}

// waitForCondition polls until condition returns true or timeout expires.
func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

// defaultTestConfig returns a config suitable for testing.
func defaultTestConfig() *config.Config {
	return &config.Config{
		Kafka: config.KafkaConfig{
			CallbackTopic:    "callback-test",
			CallbackDLQTopic: "callback-dlq-test",
		},
		Callback: config.CallbackConfig{
			MaxRetries:     3,
			BackoffBaseSec: 10,
			TimeoutSec:     5,
		},
	}
}

func TestDeliverCallback_StumpSuccess(t *testing.T) {
	var receivedBody []byte
	var receivedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	stumpData := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	msg := &kafka.CallbackTopicMessage{
		CallbackURL:  server.URL + "/callback",
		Type:         kafka.CallbackStump,
		TxID:         "abc123",
		BlockHash:    "blockhash456",
		SubtreeIndex: 3,
		Stump:        stumpData,
	}

	err := ds.deliverCallback(context.Background(), msg)
	if err != nil {
		t.Fatalf("expected successful delivery, got error: %v", err)
	}

	if receivedContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", receivedContentType)
	}

	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal received payload: %v", err)
	}

	if payload.TxID != "abc123" {
		t.Errorf("expected txid 'abc123', got %q", payload.TxID)
	}
	if payload.Type != "STUMP" {
		t.Errorf("expected type 'STUMP', got %q", payload.Type)
	}
	if payload.BlockHash != "blockhash456" {
		t.Errorf("expected blockHash 'blockhash456', got %q", payload.BlockHash)
	}
	if payload.SubtreeIndex != 3 {
		t.Errorf("expected subtreeIndex 3, got %d", payload.SubtreeIndex)
	}

	expectedStump := hex.EncodeToString(stumpData)
	if payload.Stump != expectedStump {
		t.Errorf("expected stump %q, got %q", expectedStump, payload.Stump)
	}
}

func TestDeliverCallback_SeenOnNetwork(t *testing.T) {
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	msg := &kafka.CallbackTopicMessage{
		CallbackURL: server.URL + "/callback",
		Type:        kafka.CallbackSeenOnNetwork,
		TxID:        "tx-seen",
	}

	err := ds.deliverCallback(context.Background(), msg)
	if err != nil {
		t.Fatalf("expected successful delivery, got error: %v", err)
	}

	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal received payload: %v", err)
	}

	if payload.Stump != "" {
		t.Errorf("expected empty stump for SEEN_ON_NETWORK, got %q", payload.Stump)
	}
	if payload.Type != "SEEN_ON_NETWORK" {
		t.Errorf("expected type 'SEEN_ON_NETWORK', got %q", payload.Type)
	}
}

func TestDeliverCallback_Non2xxReturnsError(t *testing.T) {
	statusCodes := []int{
		http.StatusBadRequest,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusForbidden,
	}

	for _, code := range statusCodes {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer server.Close()

			cfg := defaultTestConfig()
			ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

			msg := &kafka.CallbackTopicMessage{
				CallbackURL: server.URL + "/callback",
				Type:        kafka.CallbackStump,
				TxID:        "tx-fail",
			}

			err := ds.deliverCallback(context.Background(), msg)
			if err == nil {
				t.Fatalf("expected error for status code %d, got nil", code)
			}
		})
	}
}

func TestDeliverCallback_2xxStatusesSucceed(t *testing.T) {
	statusCodes := []int{200, 201, 202, 204}

	for _, code := range statusCodes {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer server.Close()

			cfg := defaultTestConfig()
			ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

			msg := &kafka.CallbackTopicMessage{
				CallbackURL: server.URL + "/callback",
				Type:        kafka.CallbackStump,
				TxID:        "tx-ok",
			}

			err := ds.deliverCallback(context.Background(), msg)
			if err != nil {
				t.Fatalf("expected success for status code %d, got error: %v", code, err)
			}
		})
	}
}

func TestProcessDelivery_RetriesOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, retryMock, _ := newTestDeliveryService(t, cfg, server.Client())

	msg := &kafka.CallbackTopicMessage{
		CallbackURL: server.URL + "/callback",
		Type:        kafka.CallbackStump,
		TxID:        "tx-retry",
		RetryCount:  0,
	}

	ds.processDelivery(msg)

	// Check that message was re-enqueued with incremented retry count.
	msgs := retryMock.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 retry message, got %d", len(msgs))
	}

	retried := decodePublishedCallbackMessage(t, msgs[0])
	if retried.RetryCount != 1 {
		t.Errorf("expected retry count 1, got %d", retried.RetryCount)
	}
	if retried.NextRetryAt.IsZero() {
		t.Error("expected NextRetryAt to be set")
	}
}

func TestProcessDelivery_PublishesToDLQAfterMaxRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.MaxRetries = 3
	ds, retryMock, dlqMock := newTestDeliveryService(t, cfg, server.Client())

	msg := &kafka.CallbackTopicMessage{
		CallbackURL: server.URL + "/callback",
		Type:        kafka.CallbackStump,
		TxID:        "tx-dlq",
		RetryCount:  3, // Already at max retries.
	}

	ds.processDelivery(msg)

	// No retry should happen.
	retryMsgs := retryMock.getMessages()
	if len(retryMsgs) != 0 {
		t.Errorf("expected 0 retry messages after max retries, got %d", len(retryMsgs))
	}

	// Should be published to DLQ.
	dlqMsgs := dlqMock.getMessages()
	if len(dlqMsgs) != 1 {
		t.Fatalf("expected 1 DLQ message, got %d", len(dlqMsgs))
	}
}

func TestProcessDelivery_SuccessIncrementsCounter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	msg := &kafka.CallbackTopicMessage{
		CallbackURL: server.URL + "/callback",
		Type:        kafka.CallbackSeenOnNetwork,
		TxID:        "tx-counter",
	}

	ds.processDelivery(msg)

	if ds.messagesProcessed.Load() != 1 {
		t.Errorf("expected messagesProcessed=1, got %d", ds.messagesProcessed.Load())
	}
}

func TestProcessDelivery_DedupSkipsDuplicate(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	ds.dedupStore = &mockDedupStore{exists: true}

	msg := &kafka.CallbackTopicMessage{
		CallbackURL: server.URL + "/callback",
		Type:        kafka.CallbackStump,
		TxID:        "tx-dedup",
	}

	ds.processDelivery(msg)

	if requestCount.Load() != 0 {
		t.Errorf("expected no HTTP requests for dedup hit, got %d", requestCount.Load())
	}
	if ds.messagesDedupe.Load() != 1 {
		t.Errorf("expected messagesDedupe=1, got %d", ds.messagesDedupe.Load())
	}
}

func TestHandleMessage_DispatchesToWorker(t *testing.T) {
	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, &http.Client{Timeout: time.Second})

	var delivered atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ds.httpClient = server.Client()

	msg := &kafka.CallbackTopicMessage{
		CallbackURL: server.URL + "/callback",
		Type:        kafka.CallbackSeenOnNetwork,
		TxID:        "tx-dispatch",
	}
	data, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	consumerMsg := &sarama.ConsumerMessage{
		Value: data,
	}

	if err := ds.handleMessage(context.Background(), consumerMsg); err != nil {
		t.Fatalf("handleMessage failed: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return delivered.Load() > 0
	})
}

func TestBuildIdempotencyKey(t *testing.T) {
	tests := []struct {
		name     string
		msg      *kafka.CallbackTopicMessage
		expected string
	}{
		{
			name: "BLOCK_PROCESSED uses blockHash",
			msg: &kafka.CallbackTopicMessage{
				Type:      kafka.CallbackBlockProcessed,
				BlockHash: "blockhash123",
			},
			expected: "blockhash123:BLOCK_PROCESSED",
		},
		{
			name: "STUMP uses txid",
			msg: &kafka.CallbackTopicMessage{
				Type: kafka.CallbackStump,
				TxID: "txid456",
			},
			expected: "txid456:STUMP",
		},
		{
			name: "SEEN_ON_NETWORK uses txid",
			msg: &kafka.CallbackTopicMessage{
				Type: kafka.CallbackSeenOnNetwork,
				TxID: "txid789",
			},
			expected: "txid789:SEEN_ON_NETWORK",
		},
		{
			name: "empty txid returns empty",
			msg: &kafka.CallbackTopicMessage{
				Type: kafka.CallbackStump,
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildIdempotencyKey(tt.msg)
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestDedupKeyForMessage(t *testing.T) {
	tests := []struct {
		name     string
		msg      *kafka.CallbackTopicMessage
		expected string
	}{
		{
			name: "BLOCK_PROCESSED uses blockHash",
			msg: &kafka.CallbackTopicMessage{
				Type:      kafka.CallbackBlockProcessed,
				BlockHash: "block123",
			},
			expected: "block123",
		},
		{
			name: "STUMP uses txid",
			msg: &kafka.CallbackTopicMessage{
				Type: kafka.CallbackStump,
				TxID: "tx123",
			},
			expected: "tx123",
		},
		{
			name: "empty txid returns empty",
			msg: &kafka.CallbackTopicMessage{
				Type: kafka.CallbackSeenOnNetwork,
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupKeyForMessage(tt.msg)
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestDeliverCallback_BlockProcessedPayload(t *testing.T) {
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	msg := &kafka.CallbackTopicMessage{
		CallbackURL: server.URL + "/callback",
		Type:        kafka.CallbackBlockProcessed,
		BlockHash:   "block-abc",
	}

	err := ds.deliverCallback(context.Background(), msg)
	if err != nil {
		t.Fatalf("expected successful delivery, got error: %v", err)
	}

	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if payload.Type != "BLOCK_PROCESSED" {
		t.Errorf("expected type BLOCK_PROCESSED, got %q", payload.Type)
	}
	if payload.BlockHash != "block-abc" {
		t.Errorf("expected blockHash block-abc, got %q", payload.BlockHash)
	}
	if payload.TxID != "" {
		t.Errorf("expected empty txid, got %q", payload.TxID)
	}
}

func TestConcurrentDelivery(t *testing.T) {
	var deliveryCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	ds.httpClient = server.Client()

	// Dispatch multiple messages.
	for i := 0; i < 10; i++ {
		msg := &kafka.CallbackTopicMessage{
			CallbackURL: server.URL + "/callback",
			Type:        kafka.CallbackSeenOnNetwork,
			TxID:        fmt.Sprintf("tx-%d", i),
		}
		ds.workCh <- msg
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return deliveryCount.Load() >= 10
	})

	if deliveryCount.Load() != 10 {
		t.Errorf("expected 10 deliveries, got %d", deliveryCount.Load())
	}
}

// mockDedupStore implements CallbackDeduper for testing.
type mockDedupStore struct {
	exists bool
}

func (m *mockDedupStore) Exists(txid, callbackURL, statusType string) (bool, error) {
	return m.exists, nil
}

func (m *mockDedupStore) Record(txid, callbackURL, statusType string, ttl time.Duration) error {
	return nil
}
