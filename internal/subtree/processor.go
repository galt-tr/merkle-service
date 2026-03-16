package subtree

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/IBM/sarama"

	"github.com/bsv-blockchain/merkle-service/internal/cache"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

// Processor consumes subtree messages from Kafka, checks for registered txids,
// and emits callback notifications to the stumps topic.
type Processor struct {
	service.BaseService

	cfg               *config.Config
	consumer          *kafka.Consumer
	stumpsProducer    *kafka.Producer
	registrationStore *store.RegistrationStore
	seenCounterStore  *store.SeenCounterStore
	subtreeStore      *store.SubtreeStore
	regCache          *cache.RegistrationCache

	messagesProcessed atomic.Int64
}

// NewProcessor creates a new subtree Processor.
func NewProcessor(
	cfg *config.Config,
	registrationStore *store.RegistrationStore,
	seenCounterStore *store.SeenCounterStore,
	subtreeStore *store.SubtreeStore,
) *Processor {
	return &Processor{
		cfg:               cfg,
		registrationStore: registrationStore,
		seenCounterStore:  seenCounterStore,
		subtreeStore:      subtreeStore,
	}
}

// Init initializes the subtree processor, setting up the Kafka consumer, producer, and registration cache.
func (p *Processor) Init(_ interface{}) error {
	p.InitBase("subtree-processor")

	// Initialize registration deduplication cache (txmetacache).
	regCache, err := cache.NewRegistrationCache(p.cfg.Subtree.CacheMaxMB, p.Logger)
	if err != nil {
		p.Logger.Warn("failed to create registration cache, proceeding without cache", "error", err)
	} else {
		p.regCache = regCache
	}

	stumpsProducer, err := kafka.NewProducer(
		p.cfg.Kafka.Brokers,
		p.cfg.Kafka.StumpsTopic,
		p.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create stumps producer: %w", err)
	}
	p.stumpsProducer = stumpsProducer

	consumer, err := kafka.NewConsumer(
		p.cfg.Kafka.Brokers,
		p.cfg.Kafka.ConsumerGroup+"-subtree",
		[]string{p.cfg.Kafka.SubtreeTopic},
		p.handleMessage,
		p.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create subtree consumer: %w", err)
	}
	p.consumer = consumer

	p.Logger.Info("subtree processor initialized",
		"storageMode", p.cfg.Subtree.StorageMode,
		"subtreeTopic", p.cfg.Kafka.SubtreeTopic,
		"stumpsTopic", p.cfg.Kafka.StumpsTopic,
		"cacheEnabled", p.regCache != nil,
	)

	return nil
}

// Start begins consuming subtree messages from Kafka.
func (p *Processor) Start(ctx context.Context) error {
	p.Logger.Info("starting subtree processor")

	if err := p.consumer.Start(ctx); err != nil {
		return fmt.Errorf("failed to start subtree consumer: %w", err)
	}

	p.SetStarted(true)
	p.Logger.Info("subtree processor started")
	return nil
}

// Stop gracefully shuts down the subtree processor.
func (p *Processor) Stop() error {
	p.Logger.Info("stopping subtree processor")

	var firstErr error

	if p.consumer != nil {
		if err := p.consumer.Stop(); err != nil {
			p.Logger.Error("failed to stop consumer", "error", err)
			firstErr = err
		}
	}

	if p.stumpsProducer != nil {
		if err := p.stumpsProducer.Close(); err != nil {
			p.Logger.Error("failed to close stumps producer", "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	p.SetStarted(false)
	p.Cancel()
	p.Logger.Info("subtree processor stopped", "messagesProcessed", p.messagesProcessed.Load())
	return firstErr
}

// Health returns the current health status of the subtree processor.
func (p *Processor) Health() service.HealthStatus {
	status := "healthy"
	if !p.IsStarted() {
		status = "unhealthy"
	}

	return service.HealthStatus{
		Name:   p.Name,
		Status: status,
		Details: map[string]string{
			"messagesProcessed": fmt.Sprintf("%d", p.messagesProcessed.Load()),
		},
	}
}

// handleMessage processes a single subtree message from Kafka.
func (p *Processor) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	subtreeMsg, err := kafka.DecodeSubtreeMessage(msg.Value)
	if err != nil {
		p.Logger.Error("failed to decode subtree message",
			"offset", msg.Offset,
			"partition", msg.Partition,
			"error", err,
		)
		return fmt.Errorf("failed to decode subtree message: %w", err)
	}

	p.Logger.Debug("processing subtree",
		"subtreeId", subtreeMsg.SubtreeID,
		"txCount", len(subtreeMsg.TxIDs),
		"blockHeight", subtreeMsg.BlockHeight,
	)

	// Step 1: Store subtree if in realtime storage mode.
	if p.cfg.Subtree.StorageMode == "realtime" {
		if err := p.subtreeStore.StoreSubtree(subtreeMsg.SubtreeID, subtreeMsg.MerkleData, subtreeMsg.BlockHeight); err != nil {
			p.Logger.Error("failed to store subtree",
				"subtreeId", subtreeMsg.SubtreeID,
				"error", err,
			)
			return fmt.Errorf("failed to store subtree %s: %w", subtreeMsg.SubtreeID, err)
		}
	}

	// Step 2: Extract txids and check registrations with cache deduplication.
	txids := subtreeMsg.TxIDs
	if len(txids) == 0 {
		p.messagesProcessed.Add(1)
		return nil
	}

	var registrations map[string][]string

	if p.regCache != nil {
		// Use cache to filter out txids we've already checked.
		uncached, cachedRegistered := p.regCache.FilterUncached(txids)

		// For cached-registered txids, we still need the callback URLs from Aerospike.
		// But we only need to query Aerospike for uncached + cached-registered txids.
		toQuery := append(uncached, cachedRegistered...)
		if len(toQuery) > 0 {
			var err error
			registrations, err = p.registrationStore.BatchGet(toQuery)
			if err != nil {
				p.Logger.Error("failed to batch get registrations",
					"subtreeId", subtreeMsg.SubtreeID,
					"txCount", len(toQuery),
					"error", err,
				)
				return fmt.Errorf("failed to batch get registrations: %w", err)
			}
		} else {
			registrations = make(map[string][]string)
		}

		// Populate cache: mark uncached txids as registered or not.
		var newRegistered, newNotRegistered []string
		for _, txid := range uncached {
			if _, ok := registrations[txid]; ok {
				newRegistered = append(newRegistered, txid)
			} else {
				newNotRegistered = append(newNotRegistered, txid)
			}
		}
		if len(newRegistered) > 0 {
			_ = p.regCache.SetMultiRegistered(newRegistered)
		}
		if len(newNotRegistered) > 0 {
			_ = p.regCache.SetMultiNotRegistered(newNotRegistered)
		}
	} else {
		// No cache, query Aerospike directly.
		var err error
		registrations, err = p.registrationStore.BatchGet(txids)
		if err != nil {
			p.Logger.Error("failed to batch get registrations",
				"subtreeId", subtreeMsg.SubtreeID,
				"txCount", len(txids),
				"error", err,
			)
			return fmt.Errorf("failed to batch get registrations: %w", err)
		}
	}

	// Step 3: Process each registered txid.
	for txid, callbackURLs := range registrations {
		if err := p.processRegisteredTxid(ctx, txid, callbackURLs, subtreeMsg.SubtreeID); err != nil {
			p.Logger.Error("failed to process registered txid",
				"txid", txid,
				"subtreeId", subtreeMsg.SubtreeID,
				"error", err,
			)
			// Continue processing other txids rather than failing the entire message.
		}
	}

	p.messagesProcessed.Add(1)
	return nil
}

// processRegisteredTxid handles a single registered txid: emits SEEN_ON_NETWORK,
// increments the seen counter, and conditionally emits SEEN_MULTIPLE_NODES.
func (p *Processor) processRegisteredTxid(_ context.Context, txid string, callbackURLs []string, subtreeID string) error {
	// Step 3a: Emit SEEN_ON_NETWORK message for each callback URL.
	for _, callbackURL := range callbackURLs {
		stumpsMsg := &kafka.StumpsMessage{
			CallbackURL: callbackURL,
			TxID:        txid,
			StatusType:  kafka.StatusSeenOnNetwork,
			SubtreeID:   subtreeID,
			RetryCount:  0,
		}

		data, err := stumpsMsg.Encode()
		if err != nil {
			return fmt.Errorf("failed to encode SEEN_ON_NETWORK message for txid %s: %w", txid, err)
		}

		if err := p.stumpsProducer.PublishWithHashKey(callbackURL, data); err != nil {
			return fmt.Errorf("failed to publish SEEN_ON_NETWORK for txid %s: %w", txid, err)
		}

		p.Logger.Debug("emitted SEEN_ON_NETWORK",
			"txid", txid,
			"callbackUrl", callbackURL,
			"subtreeId", subtreeID,
		)
	}

	// Step 3b: Increment seen counter.
	result, err := p.seenCounterStore.Increment(txid)
	if err != nil {
		return fmt.Errorf("failed to increment seen counter for txid %s: %w", txid, err)
	}

	// Step 3c: If threshold reached, emit SEEN_MULTIPLE_NODES message.
	if result.ThresholdReached {
		for _, callbackURL := range callbackURLs {
			stumpsMsg := &kafka.StumpsMessage{
				CallbackURL: callbackURL,
				TxID:        txid,
				StatusType:  kafka.StatusSeenMultiNodes,
				SubtreeID:   subtreeID,
				RetryCount:  0,
			}

			data, err := json.Marshal(stumpsMsg)
			if err != nil {
				return fmt.Errorf("failed to encode SEEN_MULTIPLE_NODES message for txid %s: %w", txid, err)
			}

			if err := p.stumpsProducer.PublishWithHashKey(callbackURL, data); err != nil {
				return fmt.Errorf("failed to publish SEEN_MULTIPLE_NODES for txid %s: %w", txid, err)
			}

			p.Logger.Debug("emitted SEEN_MULTIPLE_NODES",
				"txid", txid,
				"callbackUrl", callbackURL,
				"seenCount", result.NewCount,
			)
		}
	}

	// Step 3d: If above threshold, suppress duplicate emissions (do nothing).
	if result.NewCount > p.seenCounterStore.Threshold() {
		p.Logger.Debug("suppressing duplicate SEEN_MULTIPLE_NODES emission",
			"txid", txid,
			"seenCount", result.NewCount,
			"threshold", p.seenCounterStore.Threshold(),
		)
	}

	return nil
}
