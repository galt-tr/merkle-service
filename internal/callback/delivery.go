package callback

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"

	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

// CallbackDeduper abstracts callback deduplication for testability.
type CallbackDeduper interface {
	Exists(txid, callbackURL, statusType string) (bool, error)
	Record(txid, callbackURL, statusType string, ttl time.Duration) error
}

// callbackPayload is the JSON body sent to the callback URL.
type callbackPayload struct {
	TxID      string   `json:"txid,omitempty"`
	TxIDs     []string `json:"txids,omitempty"`
	Status    string   `json:"status"`
	StumpData string   `json:"stumpData,omitempty"`
	BlockHash string   `json:"blockHash,omitempty"`
}

// DeliveryService consumes callback messages from the stumps Kafka topic
// and delivers them via HTTP POST, with linear backoff retry logic.
type DeliveryService struct {
	service.BaseService

	cfg         *config.Config
	consumer    *kafka.Consumer
	producer    *kafka.Producer
	dlqProducer *kafka.Producer
	httpClient  *http.Client
	dedupStore  CallbackDeduper
	stumpCache  store.StumpCache

	// Worker pool for concurrent delivery.
	workCh   chan *kafka.StumpsMessage
	workerWg sync.WaitGroup

	messagesProcessed atomic.Int64
	messagesRetried   atomic.Int64
	messagesFailed    atomic.Int64
	messagesDedupe    atomic.Int64
}

// NewDeliveryService creates a new callback DeliveryService.
func NewDeliveryService(cfg *config.Config, dedupStore CallbackDeduper, stumpCache store.StumpCache) *DeliveryService {
	return &DeliveryService{
		cfg:        cfg,
		dedupStore: dedupStore,
		stumpCache: stumpCache,
	}
}

// Init initializes the delivery service, setting up the Kafka consumer, producers, and HTTP client.
func (d *DeliveryService) Init(_ interface{}) error {
	d.InitBase("callback-delivery")

	// Set up the HTTP client with tuned transport for high-throughput delivery.
	maxConnsPerHost := d.cfg.Callback.MaxConnsPerHost
	if maxConnsPerHost <= 0 {
		maxConnsPerHost = 32
	}
	maxIdleConnsPerHost := d.cfg.Callback.MaxIdleConnsPerHost
	if maxIdleConnsPerHost <= 0 {
		maxIdleConnsPerHost = 16
	}

	transport := &http.Transport{
		MaxIdleConns:        maxConnsPerHost * 10, // global pool
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		MaxConnsPerHost:     maxConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // small JSON payloads — compression adds latency
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	d.httpClient = &http.Client{
		Timeout:   time.Duration(d.cfg.Callback.TimeoutSec) * time.Second,
		Transport: transport,
	}

	// Create producer for re-enqueuing retries to the stumps topic.
	producer, err := kafka.NewProducer(
		d.cfg.Kafka.Brokers,
		d.cfg.Kafka.StumpsTopic,
		d.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create stumps producer: %w", err)
	}
	d.producer = producer

	// Create producer for publishing permanently failed messages to the DLQ topic.
	dlqProducer, err := kafka.NewProducer(
		d.cfg.Kafka.Brokers,
		d.cfg.Kafka.StumpsDLQTopic,
		d.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create stumps DLQ producer: %w", err)
	}
	d.dlqProducer = dlqProducer

	// Create consumer for the stumps topic.
	consumer, err := kafka.NewConsumer(
		d.cfg.Kafka.Brokers,
		d.cfg.Kafka.ConsumerGroup+"-callback",
		[]string{d.cfg.Kafka.StumpsTopic},
		d.handleMessage,
		d.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create stumps consumer: %w", err)
	}
	d.consumer = consumer

	// Initialize the work channel for the worker pool.
	workers := d.cfg.Callback.DeliveryWorkers
	if workers <= 0 {
		workers = 64
	}
	d.workCh = make(chan *kafka.StumpsMessage, workers*2)

	d.Logger.Info("callback delivery service initialized",
		"stumpsTopic", d.cfg.Kafka.StumpsTopic,
		"stumpsDlqTopic", d.cfg.Kafka.StumpsDLQTopic,
		"maxRetries", d.cfg.Callback.MaxRetries,
		"backoffBaseSec", d.cfg.Callback.BackoffBaseSec,
		"timeoutSec", d.cfg.Callback.TimeoutSec,
		"deliveryWorkers", workers,
		"maxConnsPerHost", maxConnsPerHost,
		"maxIdleConnsPerHost", maxIdleConnsPerHost,
	)

	return nil
}

// Start begins consuming callback messages from Kafka and launches delivery workers.
func (d *DeliveryService) Start(ctx context.Context) error {
	d.Logger.Info("starting callback delivery service")

	// Launch delivery workers.
	workers := d.cfg.Callback.DeliveryWorkers
	if workers <= 0 {
		workers = 64
	}
	d.workerWg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.deliveryWorker()
	}

	if err := d.consumer.Start(ctx); err != nil {
		return fmt.Errorf("failed to start stumps consumer: %w", err)
	}

	d.SetStarted(true)
	d.Logger.Info("callback delivery service started")
	return nil
}

// Stop gracefully shuts down the delivery service.
// Closes the work channel first to drain in-flight deliveries, then stops consumer and producers.
func (d *DeliveryService) Stop() error {
	d.Logger.Info("stopping callback delivery service")

	var firstErr error

	// Close work channel to signal workers to drain and exit.
	if d.workCh != nil {
		close(d.workCh)
		d.workerWg.Wait()
	}

	if d.consumer != nil {
		if err := d.consumer.Stop(); err != nil {
			d.Logger.Error("failed to stop consumer", "error", err)
			firstErr = err
		}
	}

	if d.producer != nil {
		if err := d.producer.Close(); err != nil {
			d.Logger.Error("failed to close stumps producer", "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if d.dlqProducer != nil {
		if err := d.dlqProducer.Close(); err != nil {
			d.Logger.Error("failed to close DLQ producer", "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	d.SetStarted(false)
	d.Cancel()
	d.Logger.Info("callback delivery service stopped",
		"messagesProcessed", d.messagesProcessed.Load(),
		"messagesRetried", d.messagesRetried.Load(),
		"messagesFailed", d.messagesFailed.Load(),
	)
	return firstErr
}

// Health returns the current health status of the delivery service.
func (d *DeliveryService) Health() service.HealthStatus {
	status := "healthy"
	if !d.IsStarted() {
		status = "unhealthy"
	}

	return service.HealthStatus{
		Name:   d.Name,
		Status: status,
		Details: map[string]string{
			"messagesProcessed": fmt.Sprintf("%d", d.messagesProcessed.Load()),
			"messagesRetried":   fmt.Sprintf("%d", d.messagesRetried.Load()),
			"messagesFailed":    fmt.Sprintf("%d", d.messagesFailed.Load()),
		},
	}
}

// handleMessage decodes a Kafka message and dispatches it to the worker pool.
// The Kafka offset is marked immediately after dispatch (at-least-once with dedup).
func (d *DeliveryService) handleMessage(_ context.Context, msg *sarama.ConsumerMessage) error {
	stumpsMsg, err := kafka.DecodeStumpsMessage(msg.Value)
	if err != nil {
		d.Logger.Error("failed to decode stumps message",
			"offset", msg.Offset,
			"partition", msg.Partition,
			"error", err,
		)
		return fmt.Errorf("failed to decode stumps message: %w", err)
	}

	// Check delay for retry messages before dispatching.
	if !stumpsMsg.NextRetryAt.IsZero() && time.Now().Before(stumpsMsg.NextRetryAt) {
		d.Logger.Debug("message not yet due for retry, re-enqueuing",
			"callbackUrl", stumpsMsg.CallbackURL,
			"txid", stumpsMsg.TxID,
			"nextRetryAt", stumpsMsg.NextRetryAt,
		)
		return d.reenqueue(stumpsMsg)
	}

	// Dispatch to worker pool (blocking send provides backpressure).
	d.workCh <- stumpsMsg
	return nil
}

// deliveryWorker is a goroutine that processes delivery jobs from the work channel.
func (d *DeliveryService) deliveryWorker() {
	defer d.workerWg.Done()
	for msg := range d.workCh {
		d.processDelivery(msg)
	}
}

// processDelivery handles dedup check, HTTP delivery, dedup record, and retry/DLQ logic for a single message.
func (d *DeliveryService) processDelivery(stumpsMsg *kafka.StumpsMessage) {
	d.Logger.Debug("processing callback message",
		"callbackUrl", stumpsMsg.CallbackURL,
		"txid", stumpsMsg.TxID,
		"statusType", stumpsMsg.StatusType,
		"retryCount", stumpsMsg.RetryCount,
	)

	// Resolve STUMP data: inline StumpData takes priority, then StumpRef via cache.
	var resolvedStumpData []byte
	if len(stumpsMsg.StumpData) > 0 {
		resolvedStumpData = stumpsMsg.StumpData
	} else if stumpsMsg.StumpRef != "" {
		if d.stumpCache != nil {
			data, ok, _ := d.stumpCache.Get(stumpsMsg.StumpRef, stumpsMsg.BlockHash)
			if !ok {
				d.Logger.Warn("stump cache miss for StumpRef, re-enqueuing for retry",
					"stumpRef", stumpsMsg.StumpRef,
					"blockHash", stumpsMsg.BlockHash,
					"callbackUrl", stumpsMsg.CallbackURL,
				)
				stumpsMsg.RetryCount++
				backoffSec := d.cfg.Callback.BackoffBaseSec * stumpsMsg.RetryCount
				stumpsMsg.NextRetryAt = time.Now().Add(time.Duration(backoffSec) * time.Second)
				d.messagesRetried.Add(1)
				if err := d.reenqueue(stumpsMsg); err != nil {
					d.Logger.Error("failed to reenqueue after cache miss", "error", err)
				}
				return
			}
			resolvedStumpData = data
		}
	}

	// Check callback dedup — skip if already delivered.
	if d.dedupStore != nil {
		dedupKey := dedupKeyForMessage(stumpsMsg)
		if dedupKey != "" {
			exists, err := d.dedupStore.Exists(dedupKey, stumpsMsg.CallbackURL, string(stumpsMsg.StatusType))
			if err != nil {
				d.Logger.Warn("dedup check failed, proceeding with delivery", "error", err)
			} else if exists {
				d.Logger.Debug("skipping duplicate callback delivery",
					"dedupKey", dedupKey,
					"callbackUrl", stumpsMsg.CallbackURL,
					"statusType", stumpsMsg.StatusType,
				)
				d.messagesDedupe.Add(1)
				return
			}
		}
	}

	// Attempt HTTP POST delivery.
	err := d.deliverCallback(context.Background(), stumpsMsg, resolvedStumpData)
	if err == nil {
		// Record successful delivery for dedup.
		if d.dedupStore != nil {
			dedupKey := dedupKeyForMessage(stumpsMsg)
			if dedupKey != "" {
				ttl := time.Duration(d.cfg.Callback.DedupTTLSec) * time.Second
				if recErr := d.dedupStore.Record(dedupKey, stumpsMsg.CallbackURL, string(stumpsMsg.StatusType), ttl); recErr != nil {
					d.Logger.Warn("failed to record callback dedup", "error", recErr)
				}
			}
		}
		d.messagesProcessed.Add(1)
		d.Logger.Debug("callback delivered successfully",
			"callbackUrl", stumpsMsg.CallbackURL,
			"txid", stumpsMsg.TxID,
			"statusType", stumpsMsg.StatusType,
		)
		return
	}

	d.Logger.Warn("callback delivery failed",
		"callbackUrl", stumpsMsg.CallbackURL,
		"txid", stumpsMsg.TxID,
		"statusType", stumpsMsg.StatusType,
		"retryCount", stumpsMsg.RetryCount,
		"error", err,
	)

	// Check if we've exhausted retries.
	if stumpsMsg.RetryCount >= d.cfg.Callback.MaxRetries {
		d.Logger.Error("callback permanently failed, publishing to DLQ",
			"callbackUrl", stumpsMsg.CallbackURL,
			"txid", stumpsMsg.TxID,
			"statusType", stumpsMsg.StatusType,
			"retryCount", stumpsMsg.RetryCount,
		)
		d.messagesFailed.Add(1)
		if err := d.publishToDLQ(stumpsMsg); err != nil {
			d.Logger.Error("failed to publish to DLQ", "error", err)
		}
		return
	}

	// Increment retry count and calculate next retry time with linear backoff.
	stumpsMsg.RetryCount++
	backoffSec := d.cfg.Callback.BackoffBaseSec * stumpsMsg.RetryCount
	stumpsMsg.NextRetryAt = time.Now().Add(time.Duration(backoffSec) * time.Second)

	d.Logger.Info("scheduling callback retry",
		"callbackUrl", stumpsMsg.CallbackURL,
		"txid", stumpsMsg.TxID,
		"retryCount", stumpsMsg.RetryCount,
		"nextRetryAt", stumpsMsg.NextRetryAt,
		"backoffSec", backoffSec,
	)

	d.messagesRetried.Add(1)
	if err := d.reenqueue(stumpsMsg); err != nil {
		d.Logger.Error("failed to reenqueue message", "error", err)
	}
}

// deliverCallback makes an HTTP POST to the callback URL with the appropriate payload.
func (d *DeliveryService) deliverCallback(ctx context.Context, msg *kafka.StumpsMessage, stumpData []byte) error {
	payload := callbackPayload{
		TxID:      msg.TxID,
		TxIDs:     msg.TxIDs,
		Status:    string(msg.StatusType),
		BlockHash: msg.BlockHash,
	}

	// Encode stump data as base64 if present.
	if len(stumpData) > 0 {
		payload.StumpData = base64.StdEncoding.EncodeToString(stumpData)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal callback payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, msg.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Add idempotency key header for receiver-side dedup.
	idempotencyKey := buildIdempotencyKey(msg)
	if idempotencyKey != "" {
		req.Header.Set("X-Idempotency-Key", idempotencyKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	return fmt.Errorf("callback returned non-2xx status: %d", resp.StatusCode)
}

// buildIdempotencyKey creates a unique key for a callback delivery.
func buildIdempotencyKey(msg *kafka.StumpsMessage) string {
	if msg.StatusType == kafka.StatusBlockProcessed {
		return msg.BlockHash + ":" + string(msg.StatusType)
	}
	if msg.TxID != "" {
		return msg.TxID + ":" + string(msg.StatusType)
	}
	if msg.BlockHash != "" && msg.SubtreeID != "" {
		return msg.BlockHash + ":" + msg.SubtreeID + ":" + string(msg.StatusType)
	}
	return ""
}

// dedupKeyForMessage returns the dedup key for a message.
// For BLOCK_PROCESSED, uses blockHash; for other types, uses txid.
func dedupKeyForMessage(msg *kafka.StumpsMessage) string {
	if msg.StatusType == kafka.StatusBlockProcessed {
		return msg.BlockHash
	}
	if msg.TxID != "" {
		return msg.TxID
	}
	if len(msg.TxIDs) > 0 {
		return msg.TxIDs[0]
	}
	return ""
}

// reenqueue publishes the message back to the stumps topic for later processing.
func (d *DeliveryService) reenqueue(msg *kafka.StumpsMessage) error {
	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("failed to encode stumps message for re-enqueue: %w", err)
	}

	if err := d.producer.PublishWithHashKey(msg.CallbackURL, data); err != nil {
		return fmt.Errorf("failed to re-enqueue stumps message: %w", err)
	}

	return nil
}

// publishToDLQ publishes a permanently failed message to the dead-letter queue topic.
func (d *DeliveryService) publishToDLQ(msg *kafka.StumpsMessage) error {
	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("failed to encode stumps message for DLQ: %w", err)
	}

	if err := d.dlqProducer.PublishWithHashKey(msg.CallbackURL, data); err != nil {
		return fmt.Errorf("failed to publish to DLQ: %w", err)
	}

	return nil
}
