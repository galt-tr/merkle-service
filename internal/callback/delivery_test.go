package callback

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
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
	}
	ds.InitBase("callback-delivery-test")
	// Suppress log output in tests.
	ds.Logger = logger

	return ds, mockRetryProducer, mockDLQProducer
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

	err := ds.deliverCallback(context.Background(), msg)
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

	err := ds.deliverCallback(context.Background(), msg)
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

			msg := &kafka.StumpsMessage{
				CallbackURL: server.URL + "/callback",
				TxID:        "tx-ok",
				StatusType:  kafka.StatusMined,
			}

			err := ds.deliverCallback(context.Background(), msg)
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

	if !called {
		t.Error("expected HTTP callback to be called")
	}

	if len(retryProducer.getMessages()) != 0 {
		t.Error("expected no retry messages")
	}
	if len(dlqProducer.getMessages()) != 0 {
		t.Error("expected no DLQ messages")
	}
	if ds.messagesProcessed.Load() != 1 {
		t.Errorf("expected messagesProcessed=1, got %d", ds.messagesProcessed.Load())
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

	if ds.messagesRetried.Load() != 1 {
		t.Errorf("expected messagesRetried=1, got %d", ds.messagesRetried.Load())
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

	if ds.messagesFailed.Load() != 1 {
		t.Errorf("expected messagesFailed=1, got %d", ds.messagesFailed.Load())
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

	if len(retryProducer.getMessages()) != 0 {
		t.Error("expected no retry messages")
	}
	if len(dlqProducer.getMessages()) != 1 {
		t.Fatalf("expected 1 DLQ message, got %d", len(dlqProducer.getMessages()))
	}

	if ds.messagesFailed.Load() != 1 {
		t.Errorf("expected messagesFailed=1, got %d", ds.messagesFailed.Load())
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

	if !httpCalled {
		t.Error("expected HTTP callback to be called for message with elapsed delay")
	}
	if len(retryProducer.getMessages()) != 0 {
		t.Error("expected no retry messages for successful delivery")
	}
	if ds.messagesProcessed.Load() != 1 {
		t.Errorf("expected messagesProcessed=1, got %d", ds.messagesProcessed.Load())
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

	err := ds.deliverCallback(ctx, msg)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
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
