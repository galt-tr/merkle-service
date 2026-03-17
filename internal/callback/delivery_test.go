package callback

import (
	"context"
	"encoding/base64"
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
	"github.com/bsv-blockchain/merkle-service/internal/store"
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

// decodePublishedStumpsMessage extracts the StumpsMessage from a captured ProducerMessage.
func decodePublishedStumpsMessage(t *testing.T, pm *sarama.ProducerMessage) *kafka.StumpsMessage {
	t.Helper()
	valueBytes, err := pm.Value.Encode()
	if err != nil {
		t.Fatalf("failed to encode producer message value: %v", err)
	}
	msg, err := kafka.DecodeStumpsMessage(valueBytes)
	if err != nil {
		t.Fatalf("failed to decode stumps message from producer: %v", err)
	}
	return msg
}

// newTestDeliveryService creates a DeliveryService wired with mock producers and a custom HTTP client.
// It initializes the worker pool with 4 workers for testing concurrent delivery.
func newTestDeliveryService(t *testing.T, cfg *config.Config, httpClient *http.Client) (*DeliveryService, *mockSyncProducer, *mockSyncProducer) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mockRetryProducer := &mockSyncProducer{}
	mockDLQProducer := &mockSyncProducer{}

	ds := &DeliveryService{
		cfg:        cfg,
		httpClient: httpClient,
		producer:   kafka.NewTestProducer(mockRetryProducer, cfg.Kafka.StumpsTopic, logger),
		dlqProducer: kafka.NewTestProducer(mockDLQProducer, cfg.Kafka.StumpsDLQTopic, logger),
		workCh:     make(chan *kafka.StumpsMessage, 64),
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
			StumpsTopic:    "stumps-test",
			StumpsDLQTopic: "stumps-dlq-test",
		},
		Callback: config.CallbackConfig{
			MaxRetries:     3,
			BackoffBaseSec: 10,
			TimeoutSec:     5,
		},
	}
}

func TestDeliverCallback_Success(t *testing.T) {
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
	msg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "abc123",
		TxIDs:       []string{"tx1", "tx2"},
		StatusType:  kafka.StatusMined,
		BlockHash:   "blockhash456",
		StumpData:   stumpData,
	}

	err := ds.deliverCallback(context.Background(), msg, [][]byte{msg.StumpData})
	if err != nil {
		t.Fatalf("expected successful delivery, got error: %v", err)
	}

	// Verify content type.
	if receivedContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", receivedContentType)
	}

	// Verify payload structure.
	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal received payload: %v", err)
	}

	if payload.TxID != "abc123" {
		t.Errorf("expected txid 'abc123', got %q", payload.TxID)
	}
	if len(payload.TxIDs) != 2 || payload.TxIDs[0] != "tx1" || payload.TxIDs[1] != "tx2" {
		t.Errorf("expected txids [tx1, tx2], got %v", payload.TxIDs)
	}
	if payload.Status != "MINED" {
		t.Errorf("expected status 'MINED', got %q", payload.Status)
	}
	if payload.BlockHash != "blockhash456" {
		t.Errorf("expected blockHash 'blockhash456', got %q", payload.BlockHash)
	}

	expectedStumpData := base64.StdEncoding.EncodeToString(stumpData)
	if payload.StumpData != expectedStumpData {
		t.Errorf("expected stumpData %q, got %q", expectedStumpData, payload.StumpData)
	}
}

func TestDeliverCallback_NoStumpData(t *testing.T) {
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

	msg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-no-stump",
		StatusType:  kafka.StatusSeenOnNetwork,
	}

	err := ds.deliverCallback(context.Background(), msg, [][]byte{msg.StumpData})
	if err != nil {
		t.Fatalf("expected successful delivery, got error: %v", err)
	}

	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal received payload: %v", err)
	}

	if payload.StumpData != "" {
		t.Errorf("expected empty stumpData when no stump data present, got %q", payload.StumpData)
	}
	if payload.Status != "SEEN_ON_NETWORK" {
		t.Errorf("expected status 'SEEN_ON_NETWORK', got %q", payload.Status)
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

			msg := &kafka.StumpsMessage{
				CallbackURL: server.URL + "/callback",
				TxID:        "tx-fail",
				StatusType:  kafka.StatusMined,
			}

			err := ds.deliverCallback(context.Background(), msg, [][]byte{msg.StumpData})
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

			msg := &kafka.StumpsMessage{
				CallbackURL: server.URL + "/callback",
				TxID:        "tx-ok",
				StatusType:  kafka.StatusMined,
			}

			err := ds.deliverCallback(context.Background(), msg, [][]byte{msg.StumpData})
			if err != nil {
				t.Fatalf("expected success for status %d, got error: %v", code, err)
			}
		})
	}
}

func TestHandleMessage_SuccessfulDelivery(t *testing.T) {
	var called bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, retryProducer, dlqProducer := newTestDeliveryService(t, cfg, server.Client())

	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-success",
		StatusType:  kafka.StatusMined,
		RetryCount:  0,
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode stumps message: %v", err)
	}

	consumerMsg := &sarama.ConsumerMessage{
		Value:     data,
		Offset:    1,
		Partition: 0,
	}

	err = ds.handleMessage(context.Background(), consumerMsg)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	if !called {
		t.Error("expected HTTP callback to be called")
	}

	if len(retryProducer.getMessages()) != 0 {
		t.Error("expected no retry messages")
	}
	if len(dlqProducer.getMessages()) != 0 {
		t.Error("expected no DLQ messages")
	}
}

func TestHandleMessage_RetryOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.MaxRetries = 5
	cfg.Callback.BackoffBaseSec = 10

	ds, retryProducer, dlqProducer := newTestDeliveryService(t, cfg, server.Client())

	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-retry",
		StatusType:  kafka.StatusMined,
		RetryCount:  1, // Not yet at max retries.
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode stumps message: %v", err)
	}

	consumerMsg := &sarama.ConsumerMessage{
		Value:     data,
		Offset:    1,
		Partition: 0,
	}

	err = ds.handleMessage(context.Background(), consumerMsg)
	if err != nil {
		t.Fatalf("expected no error (message re-enqueued), got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesRetried.Load() == 1
	})

	// Should have re-enqueued to retry producer.
	retryMsgs := retryProducer.getMessages()
	if len(retryMsgs) != 1 {
		t.Fatalf("expected 1 retry message, got %d", len(retryMsgs))
	}

	// Verify retry count was incremented and backoff was applied.
	requeued := decodePublishedStumpsMessage(t, retryMsgs[0])
	if requeued.RetryCount != 2 {
		t.Errorf("expected retry count 2, got %d", requeued.RetryCount)
	}
	if requeued.NextRetryAt.IsZero() {
		t.Error("expected NextRetryAt to be set")
	}
	// The backoff should be BackoffBaseSec * new RetryCount = 10 * 2 = 20 seconds from now.
	expectedEarliest := time.Now().Add(19 * time.Second)
	expectedLatest := time.Now().Add(21 * time.Second)
	if requeued.NextRetryAt.Before(expectedEarliest) || requeued.NextRetryAt.After(expectedLatest) {
		t.Errorf("NextRetryAt %v not within expected range [%v, %v]",
			requeued.NextRetryAt, expectedEarliest, expectedLatest)
	}

	// Should NOT have published to DLQ.
	if len(dlqProducer.getMessages()) != 0 {
		t.Error("expected no DLQ messages for retriable failure")
	}
}

func TestHandleMessage_DelayReenqueue(t *testing.T) {
	var httpCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, retryProducer, dlqProducer := newTestDeliveryService(t, cfg, server.Client())

	// Message with a future NextRetryAt - should be re-enqueued without delivery attempt.
	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-delayed",
		StatusType:  kafka.StatusMined,
		RetryCount:  1,
		NextRetryAt: time.Now().Add(1 * time.Hour), // Far in the future.
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode stumps message: %v", err)
	}

	consumerMsg := &sarama.ConsumerMessage{
		Value:     data,
		Offset:    1,
		Partition: 0,
	}

	err = ds.handleMessage(context.Background(), consumerMsg)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// HTTP callback should NOT have been called.
	if httpCalled {
		t.Error("expected HTTP callback NOT to be called for delayed message")
	}

	// Should have been re-enqueued.
	retryMsgs := retryProducer.getMessages()
	if len(retryMsgs) != 1 {
		t.Fatalf("expected 1 re-enqueued message, got %d", len(retryMsgs))
	}

	// The re-enqueued message should preserve the original retry count and NextRetryAt.
	requeued := decodePublishedStumpsMessage(t, retryMsgs[0])
	if requeued.RetryCount != 1 {
		t.Errorf("expected retry count to remain 1, got %d", requeued.RetryCount)
	}

	// No DLQ messages.
	if len(dlqProducer.getMessages()) != 0 {
		t.Error("expected no DLQ messages")
	}

	// Counters should not have been incremented (no delivery attempt, no retry increment).
	if ds.messagesProcessed.Load() != 0 {
		t.Errorf("expected messagesProcessed=0, got %d", ds.messagesProcessed.Load())
	}
	if ds.messagesRetried.Load() != 0 {
		t.Errorf("expected messagesRetried=0, got %d", ds.messagesRetried.Load())
	}
}

func TestHandleMessage_DeadLetterAfterMaxRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.MaxRetries = 3

	ds, retryProducer, dlqProducer := newTestDeliveryService(t, cfg, server.Client())

	// Message already at max retries.
	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-dead",
		StatusType:  kafka.StatusMined,
		RetryCount:  3, // Equal to MaxRetries.
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode stumps message: %v", err)
	}

	consumerMsg := &sarama.ConsumerMessage{
		Value:     data,
		Offset:    1,
		Partition: 0,
	}

	err = ds.handleMessage(context.Background(), consumerMsg)
	if err != nil {
		t.Fatalf("expected no error (message sent to DLQ), got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesFailed.Load() == 1
	})

	// Should NOT have re-enqueued to retry.
	if len(retryProducer.getMessages()) != 0 {
		t.Error("expected no retry messages when max retries exceeded")
	}

	// Should have published to DLQ.
	dlqMsgs := dlqProducer.getMessages()
	if len(dlqMsgs) != 1 {
		t.Fatalf("expected 1 DLQ message, got %d", len(dlqMsgs))
	}

	// Verify the DLQ message content.
	dlqMsg := decodePublishedStumpsMessage(t, dlqMsgs[0])
	if dlqMsg.TxID != "tx-dead" {
		t.Errorf("expected DLQ message txid 'tx-dead', got %q", dlqMsg.TxID)
	}
	if dlqMsg.RetryCount != 3 {
		t.Errorf("expected DLQ message retry count 3, got %d", dlqMsg.RetryCount)
	}

	if ds.messagesRetried.Load() != 0 {
		t.Errorf("expected messagesRetried=0, got %d", ds.messagesRetried.Load())
	}
}

func TestHandleMessage_DeadLetterExceedsMaxRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.MaxRetries = 3

	ds, retryProducer, dlqProducer := newTestDeliveryService(t, cfg, server.Client())

	// Message exceeds max retries.
	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-over-max",
		StatusType:  kafka.StatusMined,
		RetryCount:  5, // Well past MaxRetries.
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode stumps message: %v", err)
	}

	consumerMsg := &sarama.ConsumerMessage{
		Value:     data,
		Offset:    1,
		Partition: 0,
	}

	err = ds.handleMessage(context.Background(), consumerMsg)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesFailed.Load() == 1
	})

	if len(retryProducer.getMessages()) != 0 {
		t.Error("expected no retry messages")
	}
	if len(dlqProducer.getMessages()) != 1 {
		t.Fatalf("expected 1 DLQ message, got %d", len(dlqProducer.getMessages()))
	}
}

func TestHandleMessage_PastDelayDelivers(t *testing.T) {
	var httpCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, retryProducer, _ := newTestDeliveryService(t, cfg, server.Client())

	// Message with a past NextRetryAt - should proceed to delivery.
	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-past-delay",
		StatusType:  kafka.StatusMined,
		RetryCount:  1,
		NextRetryAt: time.Now().Add(-1 * time.Minute), // Already elapsed.
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode stumps message: %v", err)
	}

	consumerMsg := &sarama.ConsumerMessage{
		Value:     data,
		Offset:    1,
		Partition: 0,
	}

	err = ds.handleMessage(context.Background(), consumerMsg)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	if !httpCalled {
		t.Error("expected HTTP callback to be called for message with elapsed delay")
	}
	if len(retryProducer.getMessages()) != 0 {
		t.Error("expected no retry messages for successful delivery")
	}
}

func TestHandleMessage_InvalidMessageReturnsError(t *testing.T) {
	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, &http.Client{})

	consumerMsg := &sarama.ConsumerMessage{
		Value:     []byte("not valid json{{{"),
		Offset:    1,
		Partition: 0,
	}

	err := ds.handleMessage(context.Background(), consumerMsg)
	if err == nil {
		t.Fatal("expected error for invalid message, got nil")
	}
}

func TestDeliverCallback_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow response; the context should cancel before this completes.
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	msg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-cancelled",
		StatusType:  kafka.StatusMined,
	}

	err := ds.deliverCallback(ctx, msg, [][]byte{msg.StumpData})
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestBuildIdempotencyKey_SingleTxid(t *testing.T) {
	msg := &kafka.StumpsMessage{
		TxID:       "abc123",
		StatusType: kafka.StatusSeenOnNetwork,
	}
	key := buildIdempotencyKey(msg)
	if key != "abc123:SEEN_ON_NETWORK" {
		t.Errorf("expected 'abc123:SEEN_ON_NETWORK', got %q", key)
	}
}

func TestBuildIdempotencyKey_MinedWithBlockHash(t *testing.T) {
	msg := &kafka.StumpsMessage{
		TxIDs:      []string{"tx1", "tx2"},
		StatusType: kafka.StatusMined,
		BlockHash:  "blockhash",
		SubtreeID:  "subtree123",
	}
	key := buildIdempotencyKey(msg)
	if key != "blockhash:subtree123:MINED" {
		t.Errorf("expected 'blockhash:subtree123:MINED', got %q", key)
	}
}

func TestBuildIdempotencyKey_Empty(t *testing.T) {
	msg := &kafka.StumpsMessage{
		StatusType: kafka.StatusMined,
	}
	key := buildIdempotencyKey(msg)
	if key != "" {
		t.Errorf("expected empty key for message without txid or blockhash, got %q", key)
	}
}

func TestDeliverCallback_IdempotencyKeyHeader(t *testing.T) {
	var receivedIdempotencyKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedIdempotencyKey = r.Header.Get("X-Idempotency-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	msg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "txid-abc",
		StatusType:  kafka.StatusSeenOnNetwork,
	}

	err := ds.deliverCallback(context.Background(), msg, [][]byte{msg.StumpData})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedIdempotencyKey != "txid-abc:SEEN_ON_NETWORK" {
		t.Errorf("expected X-Idempotency-Key 'txid-abc:SEEN_ON_NETWORK', got %q", receivedIdempotencyKey)
	}
}

// --- Callback Dedup Tests ---

type mockDedupStore struct {
	mu       sync.Mutex
	records  map[string]bool
	failNext bool
}

func newMockDedupStore() *mockDedupStore {
	return &mockDedupStore{records: make(map[string]bool)}
}

func (m *mockDedupStore) Exists(txid, callbackURL, statusType string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return false, fmt.Errorf("dedup store unavailable")
	}
	key := txid + ":" + callbackURL + ":" + statusType
	return m.records[key], nil
}

func (m *mockDedupStore) Record(txid, callbackURL, statusType string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := txid + ":" + callbackURL + ":" + statusType
	m.records[key] = true
	return nil
}

func TestHandleMessage_DedupFirstDeliveryProceeds(t *testing.T) {
	var httpCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.DedupTTLSec = 86400
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	dedup := newMockDedupStore()
	ds.dedupStore = dedup

	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-first",
		StatusType:  kafka.StatusSeenOnNetwork,
	}
	data, _ := stumpsMsg.Encode()

	err := ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	if !httpCalled {
		t.Error("expected HTTP callback to be called on first delivery")
	}
	// Verify dedup was recorded.
	exists, _ := dedup.Exists("tx-first", server.URL+"/callback", "SEEN_ON_NETWORK")
	if !exists {
		t.Error("expected dedup record after successful delivery")
	}
}

func TestHandleMessage_DedupDuplicateSkipped(t *testing.T) {
	var httpCallCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCallCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.DedupTTLSec = 86400
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	dedup := newMockDedupStore()
	ds.dedupStore = dedup

	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-dup",
		StatusType:  kafka.StatusSeenOnNetwork,
	}
	data, _ := stumpsMsg.Encode()

	// First delivery.
	err := ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1})
	if err != nil {
		t.Fatalf("first delivery error: %v", err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	// Duplicate delivery — should be skipped.
	err = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 2})
	if err != nil {
		t.Fatalf("duplicate delivery error: %v", err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesDedupe.Load() == 1
	})

	if httpCallCount != 1 {
		t.Errorf("expected HTTP not called again for duplicate, got %d calls", httpCallCount)
	}
}

func TestHandleMessage_DedupFailedDeliveryDoesNotRecord(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.MaxRetries = 10
	cfg.Callback.DedupTTLSec = 86400
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	dedup := newMockDedupStore()
	ds.dedupStore = dedup

	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-fail-dedup",
		StatusType:  kafka.StatusMined,
		RetryCount:  0,
	}
	data, _ := stumpsMsg.Encode()

	err := ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesRetried.Load() == 1
	})

	// Verify dedup was NOT recorded (delivery failed).
	exists, _ := dedup.Exists("tx-fail-dedup", server.URL+"/callback", "MINED")
	if exists {
		t.Error("dedup should NOT be recorded after failed delivery")
	}
}

func TestHandleMessage_DedupDifferentStatusTypesNotDeduplicated(t *testing.T) {
	var httpCallCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCallCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.DedupTTLSec = 86400
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	dedup := newMockDedupStore()
	ds.dedupStore = dedup

	// Deliver SEEN_ON_NETWORK.
	msg1 := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-multi",
		StatusType:  kafka.StatusSeenOnNetwork,
	}
	data1, _ := msg1.Encode()
	_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data1, Offset: 1})

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	// Deliver MINED for same txid — should NOT be deduplicated.
	msg2 := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-multi",
		StatusType:  kafka.StatusMined,
	}
	data2, _ := msg2.Encode()
	_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data2, Offset: 2})

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 2
	})

	if httpCallCount != 2 {
		t.Errorf("expected 2 HTTP calls (different status types), got %d", httpCallCount)
	}
}

// TestEndToEnd_ExactlyOneCallbackPerStatusType verifies the full dedup pipeline:
// each txid/callbackURL receives exactly one SEEN_ON_NETWORK, one SEEN_MULTIPLE_NODES,
// and one MINED callback. Duplicates of each are skipped.
func TestEndToEnd_ExactlyOneCallbackPerStatusType(t *testing.T) {
	var mu sync.Mutex
	deliveredCallbacks := make(map[string]int) // "txid:statusType" -> delivery count

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload callbackPayload
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &payload)

		mu.Lock()
		key := payload.TxID + ":" + payload.Status
		deliveredCallbacks[key]++
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.DedupTTLSec = 86400
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	dedup := newMockDedupStore()
	ds.dedupStore = dedup

	txid := "txid-e2e"
	callbackURL := server.URL + "/callback"

	statusTypes := []kafka.StatusType{
		kafka.StatusSeenOnNetwork,
		kafka.StatusSeenMultiNodes,
		kafka.StatusMined,
	}

	// Deliver each status type twice — first should succeed, second should be deduped.
	for i, st := range statusTypes {
		msg := &kafka.StumpsMessage{
			CallbackURL: callbackURL,
			TxID:        txid,
			StatusType:  st,
		}
		data, _ := msg.Encode()

		expectedProcessed := int64(i + 1)

		// First delivery.
		err := ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1})
		if err != nil {
			t.Fatalf("first %s delivery error: %v", st, err)
		}
		waitForCondition(t, 5*time.Second, func() bool {
			return ds.messagesProcessed.Load() == expectedProcessed
		})

		expectedDeduped := int64(i + 1)

		// Duplicate delivery.
		err = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 2})
		if err != nil {
			t.Fatalf("duplicate %s delivery error: %v", st, err)
		}
		waitForCondition(t, 5*time.Second, func() bool {
			return ds.messagesDedupe.Load() == expectedDeduped
		})
	}

	// Verify: exactly 1 delivery per status type, 3 total.
	mu.Lock()
	defer mu.Unlock()

	for _, st := range statusTypes {
		key := txid + ":" + string(st)
		count := deliveredCallbacks[key]
		if count != 1 {
			t.Errorf("expected exactly 1 %s callback, got %d", st, count)
		}
	}

	totalDeliveries := 0
	for _, count := range deliveredCallbacks {
		totalDeliveries += count
	}
	if totalDeliveries != 3 {
		t.Errorf("expected 3 total deliveries (one per status type), got %d", totalDeliveries)
	}

	if ds.messagesDedupe.Load() != 3 {
		t.Errorf("expected 3 deduplicated messages, got %d", ds.messagesDedupe.Load())
	}
}

func TestDeliverCallback_BlockProcessed(t *testing.T) {
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	msg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		StatusType:  kafka.StatusBlockProcessed,
		BlockHash:   "000000abc123",
	}

	err := ds.deliverCallback(context.Background(), msg, [][]byte{msg.StumpData})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if payload.Status != "BLOCK_PROCESSED" {
		t.Errorf("expected status BLOCK_PROCESSED, got %q", payload.Status)
	}
	if payload.BlockHash != "000000abc123" {
		t.Errorf("expected blockHash 000000abc123, got %q", payload.BlockHash)
	}
	if payload.TxID != "" {
		t.Errorf("expected empty txid, got %q", payload.TxID)
	}
	if payload.StumpData != "" {
		t.Errorf("expected empty stumpData, got %q", payload.StumpData)
	}
}

func TestHandleMessage_BlockProcessedDedupOnBlockHash(t *testing.T) {
	var httpCallCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCallCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.DedupTTLSec = 86400
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	dedup := newMockDedupStore()
	ds.dedupStore = dedup

	msg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		StatusType:  kafka.StatusBlockProcessed,
		BlockHash:   "blockhash-bp-1",
	}
	data, _ := msg.Encode()

	// First delivery.
	_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1})
	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	// Duplicate — should be deduped on blockHash.
	_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 2})
	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesDedupe.Load() == 1
	})

	if httpCallCount != 1 {
		t.Errorf("expected duplicate BLOCK_PROCESSED to be skipped, got %d calls", httpCallCount)
	}
}

func TestBuildIdempotencyKey_BlockProcessed(t *testing.T) {
	msg := &kafka.StumpsMessage{
		StatusType: kafka.StatusBlockProcessed,
		BlockHash:  "blockhash-xyz",
	}
	key := buildIdempotencyKey(msg)
	if key != "blockhash-xyz:BLOCK_PROCESSED" {
		t.Errorf("expected 'blockhash-xyz:BLOCK_PROCESSED', got %q", key)
	}
}

// TestEndToEnd_MinedAndBlockProcessedCallbacks verifies that both MINED and
// BLOCK_PROCESSED callbacks are delivered via the same pipeline, and each is
// delivered exactly once per callback URL.
func TestEndToEnd_MinedAndBlockProcessedCallbacks(t *testing.T) {
	var mu sync.Mutex
	delivered := make(map[string]int) // "callbackURL:status" -> count

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload callbackPayload
		json.Unmarshal(body, &payload)

		mu.Lock()
		key := r.URL.Path + ":" + payload.Status
		delivered[key]++
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.DedupTTLSec = 86400
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())
	dedup := newMockDedupStore()
	ds.dedupStore = dedup

	// Simulate MINED callback for cb1.
	minedMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/cb1",
		TxIDs:       []string{"tx1"},
		StatusType:  kafka.StatusMined,
		BlockHash:   "blockhash-e2e",
		StumpData:   []byte{0x01},
		SubtreeID:   "st-1",
	}
	data, _ := minedMsg.Encode()
	_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1})

	// Simulate BLOCK_PROCESSED for cb1.
	bpMsg1 := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/cb1",
		StatusType:  kafka.StatusBlockProcessed,
		BlockHash:   "blockhash-e2e",
	}
	data, _ = bpMsg1.Encode()
	_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 2})

	// Simulate BLOCK_PROCESSED for cb2.
	bpMsg2 := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/cb2",
		StatusType:  kafka.StatusBlockProcessed,
		BlockHash:   "blockhash-e2e",
	}
	data, _ = bpMsg2.Encode()
	_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 3})

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 3
	})

	mu.Lock()
	defer mu.Unlock()

	if delivered["/cb1:MINED"] != 1 {
		t.Errorf("expected 1 MINED for cb1, got %d", delivered["/cb1:MINED"])
	}
	if delivered["/cb1:BLOCK_PROCESSED"] != 1 {
		t.Errorf("expected 1 BLOCK_PROCESSED for cb1, got %d", delivered["/cb1:BLOCK_PROCESSED"])
	}
	if delivered["/cb2:BLOCK_PROCESSED"] != 1 {
		t.Errorf("expected 1 BLOCK_PROCESSED for cb2, got %d", delivered["/cb2:BLOCK_PROCESSED"])
	}
}

// TestEndToEnd_NoRegistrations_NoBlockProcessed verifies that when no
// BLOCK_PROCESSED messages are sent through the delivery pipeline, none are delivered.
func TestEndToEnd_NoRegistrations_NoBlockProcessed(t *testing.T) {
	var httpCallCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCallCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	// No messages sent = no callbacks. This verifies the contract that the
	// block processor must decide whether to emit BLOCK_PROCESSED.
	if httpCallCount != 0 {
		t.Errorf("expected no HTTP calls with no messages, got %d", httpCallCount)
	}
	if ds.messagesProcessed.Load() != 0 {
		t.Errorf("expected 0 processed, got %d", ds.messagesProcessed.Load())
	}
}

// TestEndToEnd_BlockProcessedRetryOnFailure verifies BLOCK_PROCESSED messages
// follow the same retry/DLQ path as other message types.
func TestEndToEnd_BlockProcessedRetryOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.MaxRetries = 3
	cfg.Callback.BackoffBaseSec = 5
	ds, retryProducer, dlqProducer := newTestDeliveryService(t, cfg, server.Client())

	// First attempt (retryCount=0) fails -> should retry.
	msg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		StatusType:  kafka.StatusBlockProcessed,
		BlockHash:   "blockhash-retry",
		RetryCount:  0,
	}
	data, _ := msg.Encode()
	err := ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesRetried.Load() == 1
	})

	retryMsgs := retryProducer.getMessages()
	if len(retryMsgs) != 1 {
		t.Fatalf("expected 1 retry, got %d", len(retryMsgs))
	}
	requeued := decodePublishedStumpsMessage(t, retryMsgs[0])
	if requeued.RetryCount != 1 {
		t.Errorf("expected retryCount=1, got %d", requeued.RetryCount)
	}
	if requeued.StatusType != kafka.StatusBlockProcessed {
		t.Errorf("expected BLOCK_PROCESSED status preserved, got %s", requeued.StatusType)
	}

	// Now test DLQ: message at max retries.
	msg2 := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		StatusType:  kafka.StatusBlockProcessed,
		BlockHash:   "blockhash-dlq",
		RetryCount:  3,
	}
	data2, _ := msg2.Encode()
	err = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data2, Offset: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesFailed.Load() == 1
	})

	dlqMsgs := dlqProducer.getMessages()
	if len(dlqMsgs) != 1 {
		t.Fatalf("expected 1 DLQ message, got %d", len(dlqMsgs))
	}
	dlqMsg := decodePublishedStumpsMessage(t, dlqMsgs[0])
	if dlqMsg.StatusType != kafka.StatusBlockProcessed {
		t.Errorf("expected BLOCK_PROCESSED in DLQ, got %s", dlqMsg.StatusType)
	}
	if dlqMsg.BlockHash != "blockhash-dlq" {
		t.Errorf("expected blockhash-dlq, got %s", dlqMsg.BlockHash)
	}
}

func TestHandleMessage_LinearBackoffCalculation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.MaxRetries = 10
	cfg.Callback.BackoffBaseSec = 5

	ds, retryProducer, _ := newTestDeliveryService(t, cfg, server.Client())

	// Test with retryCount=0: after failure, retryCount becomes 1, backoff = 5 * 1 = 5s.
	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-backoff-0",
		StatusType:  kafka.StatusMined,
		RetryCount:  0,
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode stumps message: %v", err)
	}

	beforeCall := time.Now()
	err = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{
		Value:     data,
		Offset:    1,
		Partition: 0,
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesRetried.Load() == 1
	})

	retryMsgs := retryProducer.getMessages()
	if len(retryMsgs) != 1 {
		t.Fatalf("expected 1 retry message, got %d", len(retryMsgs))
	}

	requeued := decodePublishedStumpsMessage(t, retryMsgs[0])
	if requeued.RetryCount != 1 {
		t.Errorf("expected retryCount=1, got %d", requeued.RetryCount)
	}

	// Backoff should be 5 seconds (BackoffBaseSec * 1).
	expectedNextRetry := beforeCall.Add(5 * time.Second)
	tolerance := 2 * time.Second
	if requeued.NextRetryAt.Before(expectedNextRetry.Add(-tolerance)) ||
		requeued.NextRetryAt.After(expectedNextRetry.Add(tolerance)) {
		t.Errorf("NextRetryAt %v not within expected range around %v",
			requeued.NextRetryAt, expectedNextRetry)
	}
}

// TestHandleMessage_StumpRefResolvesFromCache verifies that when a message has StumpRef
// instead of inline StumpData, the delivery resolves the STUMP from the cache.
func TestHandleMessage_StumpRefResolvesFromCache(t *testing.T) {
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	// Set up stump cache with test data.
	cache := store.NewMemoryStumpCache(300)
	stumpData := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	cache.Put("subtreeABC", "block123", stumpData)
	ds.stumpCache = cache

	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-stumpref",
		StatusType:  kafka.StatusMined,
		StumpRef:    "subtreeABC",
		BlockHash:   "block123",
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode: %v", err)
	}

	err = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1, Partition: 0})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	// Verify the STUMP data was resolved and included in the callback payload.
	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	expectedStump := base64.StdEncoding.EncodeToString(stumpData)
	if payload.StumpData != expectedStump {
		t.Errorf("expected stumpData %q, got %q", expectedStump, payload.StumpData)
	}
}

// TestHandleMessage_InlineStumpDataStillWorks verifies that messages with inline StumpData
// (no StumpRef) continue to work correctly.
func TestHandleMessage_InlineStumpDataStillWorks(t *testing.T) {
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	ds, _, _ := newTestDeliveryService(t, cfg, server.Client())

	stumpData := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-inline",
		StatusType:  kafka.StatusMined,
		StumpData:   stumpData,
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode: %v", err)
	}

	err = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1, Partition: 0})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == 1
	})

	var payload callbackPayload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	expectedStump := base64.StdEncoding.EncodeToString(stumpData)
	if payload.StumpData != expectedStump {
		t.Errorf("expected stumpData %q, got %q", expectedStump, payload.StumpData)
	}
}

// TestHandleMessage_StumpRefCacheMissTriggersRetry verifies that when StumpRef is set
// but the cache misses, the message is re-enqueued for retry.
func TestHandleMessage_StumpRefCacheMissTriggersRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP callback should not have been called on cache miss")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.BackoffBaseSec = 5
	ds, retryProducer, _ := newTestDeliveryService(t, cfg, server.Client())

	// Set up empty stump cache (no data for the ref).
	ds.stumpCache = store.NewMemoryStumpCache(300)

	stumpsMsg := &kafka.StumpsMessage{
		CallbackURL: server.URL + "/callback",
		TxID:        "tx-cachemiss",
		StatusType:  kafka.StatusMined,
		StumpRef:    "missingSubtree",
		BlockHash:   "block456",
		RetryCount:  0,
	}
	data, err := stumpsMsg.Encode()
	if err != nil {
		t.Fatalf("failed to encode: %v", err)
	}

	err = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 1, Partition: 0})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesRetried.Load() == 1
	})

	// Verify message was re-enqueued.
	retryMsgs := retryProducer.getMessages()
	if len(retryMsgs) != 1 {
		t.Fatalf("expected 1 retry message, got %d", len(retryMsgs))
	}

	// Verify retry count was incremented.
	requeued := decodePublishedStumpsMessage(t, retryMsgs[0])
	if requeued.RetryCount != 1 {
		t.Errorf("expected retryCount 1, got %d", requeued.RetryCount)
	}

	// Verify no successful deliveries.
	if ds.messagesProcessed.Load() != 0 {
		t.Error("expected no successful deliveries")
	}
}

// TestDeliveryService_ConcurrentWorkers verifies that N workers process messages
// in parallel by using a slow HTTP handler and checking N messages are in-flight simultaneously.
func TestDeliveryService_ConcurrentWorkers(t *testing.T) {
	const numWorkers = 8
	const numMessages = numWorkers

	var inFlight atomic.Int64
	var maxInFlight atomic.Int64
	done := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		// Track max concurrency.
		for {
			old := maxInFlight.Load()
			if current <= old || maxInFlight.CompareAndSwap(old, current) {
				break
			}
		}
		// Hold the request open to allow concurrency to build up.
		<-done
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	cfg.Callback.DeliveryWorkers = numWorkers

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockRetryProducer := &mockSyncProducer{}
	mockDLQProducer := &mockSyncProducer{}

	ds := &DeliveryService{
		cfg:         cfg,
		httpClient:  server.Client(),
		producer:    kafka.NewTestProducer(mockRetryProducer, cfg.Kafka.StumpsTopic, logger),
		dlqProducer: kafka.NewTestProducer(mockDLQProducer, cfg.Kafka.StumpsDLQTopic, logger),
		workCh:      make(chan *kafka.StumpsMessage, numWorkers*2),
	}
	ds.InitBase("callback-delivery-concurrent-test")
	ds.Logger = logger

	ds.workerWg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go ds.deliveryWorker()
	}

	// Dispatch all messages.
	for i := 0; i < numMessages; i++ {
		msg := &kafka.StumpsMessage{
			CallbackURL: server.URL + "/callback",
			TxID:        fmt.Sprintf("tx-concurrent-%d", i),
			StatusType:  kafka.StatusMined,
		}
		data, _ := msg.Encode()
		err := ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: int64(i)})
		if err != nil {
			t.Fatalf("handleMessage error: %v", err)
		}
	}

	// Wait until all workers are in-flight.
	waitForCondition(t, 5*time.Second, func() bool {
		return inFlight.Load() == int64(numMessages)
	})

	// Release all handlers.
	close(done)

	// Wait for all to complete.
	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == int64(numMessages)
	})

	// Clean up workers.
	close(ds.workCh)
	ds.workerWg.Wait()

	if maxInFlight.Load() < int64(numWorkers) {
		t.Errorf("expected at least %d concurrent in-flight requests, got %d", numWorkers, maxInFlight.Load())
	}
}

// TestDeliveryService_Backpressure verifies that when all workers are busy,
// the dispatch channel blocks the consumer.
func TestDeliveryService_Backpressure(t *testing.T) {
	const numWorkers = 2
	const chanBuffer = 2

	holdRequests := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-holdRequests
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockRetryProducer := &mockSyncProducer{}
	mockDLQProducer := &mockSyncProducer{}

	ds := &DeliveryService{
		cfg:         cfg,
		httpClient:  server.Client(),
		producer:    kafka.NewTestProducer(mockRetryProducer, cfg.Kafka.StumpsTopic, logger),
		dlqProducer: kafka.NewTestProducer(mockDLQProducer, cfg.Kafka.StumpsDLQTopic, logger),
		workCh:      make(chan *kafka.StumpsMessage, chanBuffer),
	}
	ds.InitBase("callback-delivery-backpressure-test")
	ds.Logger = logger

	ds.workerWg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go ds.deliveryWorker()
	}

	// Fill workers (2 workers blocked in HTTP handler) + fill channel buffer (2 more).
	// That's 4 messages. The 5th should block.
	for i := 0; i < numWorkers+chanBuffer; i++ {
		msg := &kafka.StumpsMessage{
			CallbackURL: server.URL + "/callback",
			TxID:        fmt.Sprintf("tx-bp-%d", i),
			StatusType:  kafka.StatusMined,
		}
		data, _ := msg.Encode()
		err := ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: int64(i)})
		if err != nil {
			t.Fatalf("handleMessage error: %v", err)
		}
	}

	// Wait for workers to pick up messages from channel (filling HTTP handlers).
	waitForCondition(t, 5*time.Second, func() bool {
		return len(ds.workCh) == chanBuffer
	})

	// The next dispatch should block since channel is full.
	blocked := make(chan struct{})
	go func() {
		msg := &kafka.StumpsMessage{
			CallbackURL: server.URL + "/callback",
			TxID:        "tx-bp-blocked",
			StatusType:  kafka.StatusMined,
		}
		data, _ := msg.Encode()
		_ = ds.handleMessage(context.Background(), &sarama.ConsumerMessage{Value: data, Offset: 99})
		close(blocked)
	}()

	// Verify the goroutine is blocked (doesn't complete within 100ms).
	select {
	case <-blocked:
		t.Fatal("expected handleMessage to block when workers and channel are full")
	case <-time.After(100 * time.Millisecond):
		// Good — backpressure is working.
	}

	// Release all HTTP handlers to unblock.
	close(holdRequests)

	// The blocked goroutine should eventually complete.
	select {
	case <-blocked:
		// Good.
	case <-time.After(5 * time.Second):
		t.Fatal("handleMessage did not unblock after releasing workers")
	}

	// Clean up.
	waitForCondition(t, 5*time.Second, func() bool {
		return ds.messagesProcessed.Load() == int64(numWorkers+chanBuffer+1)
	})
	close(ds.workCh)
	ds.workerWg.Wait()
}

// TestDeliveryService_GracefulShutdown verifies that in-flight deliveries complete
// before Stop() returns.
func TestDeliveryService_GracefulShutdown(t *testing.T) {
	const numWorkers = 4
	const numMessages = numWorkers

	var completedDeliveries atomic.Int64
	holdRequests := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-holdRequests
		completedDeliveries.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := defaultTestConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mockRetryProducer := &mockSyncProducer{}
	mockDLQProducer := &mockSyncProducer{}

	ds := &DeliveryService{
		cfg:         cfg,
		httpClient:  server.Client(),
		producer:    kafka.NewTestProducer(mockRetryProducer, cfg.Kafka.StumpsTopic, logger),
		dlqProducer: kafka.NewTestProducer(mockDLQProducer, cfg.Kafka.StumpsDLQTopic, logger),
		workCh:      make(chan *kafka.StumpsMessage, numWorkers*2),
	}
	ds.InitBase("callback-delivery-shutdown-test")
	ds.Logger = logger

	ds.workerWg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go ds.deliveryWorker()
	}

	// Dispatch messages that will be held in HTTP handler.
	for i := 0; i < numMessages; i++ {
		msg := &kafka.StumpsMessage{
			CallbackURL: server.URL + "/callback",
			TxID:        fmt.Sprintf("tx-shutdown-%d", i),
			StatusType:  kafka.StatusMined,
		}
		ds.workCh <- msg
	}

	// Wait for all workers to be in the HTTP handler.
	time.Sleep(50 * time.Millisecond)

	// Close the work channel (simulating Stop behavior).
	close(ds.workCh)

	// Release HTTP handlers so in-flight work can complete.
	close(holdRequests)

	// Wait for workers to drain.
	ds.workerWg.Wait()

	// All in-flight deliveries should have completed.
	if completedDeliveries.Load() != int64(numMessages) {
		t.Errorf("expected %d completed deliveries after shutdown, got %d",
			numMessages, completedDeliveries.Load())
	}
	if ds.messagesProcessed.Load() != int64(numMessages) {
		t.Errorf("expected messagesProcessed=%d, got %d",
			numMessages, ds.messagesProcessed.Load())
	}
}
