package subtree

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/IBM/sarama"

	"github.com/bsv-blockchain/merkle-service/internal/cache"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/datahub"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

// RegistrationGetter abstracts registration lookups for testability.
type RegistrationGetter interface {
	BatchGet(txids []string) (map[string][]string, error)
	Get(txid string) ([]string, error)
}

// SeenCounter abstracts seen-count tracking for testability.
type SeenCounter interface {
	Increment(txid string, subtreeID string) (*store.IncrementResult, error)
}

// RegCache abstracts the registration deduplication cache for testability.
type RegCache interface {
	FilterUncached(txids []string) (uncached []string, cachedRegistered []string)
	SetMultiRegistered(txids []string) error
	SetMultiNotRegistered(txids []string) error
}

// Processor consumes subtree announcement messages from Kafka, fetches full
// subtree data from DataHub, stores it, checks registrations, and emits callbacks.
type Processor struct {
	service.BaseService

	cfg               *config.Config
	consumer          *kafka.Consumer
	stumpsProducer    *kafka.Producer
	registrationStore RegistrationGetter
	seenCounterStore  SeenCounter
	subtreeStore      *store.SubtreeStore
	regCache          RegCache
	dedupCache        *cache.DedupCache
	dataHubClient     *datahub.Client

	messagesProcessed atomic.Int64
}

// NewProcessor creates a new subtree Processor.
func NewProcessor(
	cfg *config.Config,
	registrationStore RegistrationGetter,
	seenCounterStore SeenCounter,
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
	p.InitBase("subtree-fetcher")

	// Initialize DataHub client.
	p.dataHubClient = datahub.NewClient(p.cfg.DataHub.TimeoutSec, p.cfg.DataHub.MaxRetries, p.Logger)

	// Initialize message dedup cache.
	if p.cfg.Subtree.DedupCacheSize > 0 {
		p.dedupCache = cache.NewDedupCache(p.cfg.Subtree.DedupCacheSize)
	}

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

	p.Logger.Info("subtree-fetcher initialized",
		"storageMode", p.cfg.Subtree.StorageMode,
		"subtreeTopic", p.cfg.Kafka.SubtreeTopic,
		"stumpsTopic", p.cfg.Kafka.StumpsTopic,
		"cacheEnabled", p.regCache != nil,
	)

	return nil
}

// Start begins consuming subtree messages from Kafka.
func (p *Processor) Start(ctx context.Context) error {
	p.Logger.Info("starting subtree-fetcher")

	if err := p.consumer.Start(ctx); err != nil {
		return fmt.Errorf("failed to start subtree consumer: %w", err)
	}

	p.SetStarted(true)
	p.Logger.Info("subtree-fetcher started")
	return nil
}

// Stop gracefully shuts down the subtree processor.
func (p *Processor) Stop() error {
	p.Logger.Info("stopping subtree-fetcher")

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
	p.Logger.Info("subtree-fetcher stopped", "messagesProcessed", p.messagesProcessed.Load())
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

// handleMessage processes a single subtree announcement message from Kafka.
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

	p.Logger.Debug("processing subtree announcement",
		"hash", subtreeMsg.Hash,
		"dataHubUrl", subtreeMsg.DataHubURL,
	)

	// Check dedup cache — skip if already successfully processed.
	if p.dedupCache != nil && p.dedupCache.Contains(subtreeMsg.Hash) {
		p.Logger.Debug("skipping duplicate subtree message", "hash", subtreeMsg.Hash)
		return nil
	}

	// 3.2: Fetch binary subtree data from DataHub.
	rawData, err := p.dataHubClient.FetchSubtreeRaw(ctx, subtreeMsg.DataHubURL, subtreeMsg.Hash)
	if err != nil {
		p.Logger.Error("failed to fetch subtree from DataHub",
			"hash", subtreeMsg.Hash,
			"error", err,
		)
		return fmt.Errorf("fetching subtree %s: %w", subtreeMsg.Hash, err)
	}

	// 3.3: Store raw binary data in the subtree blob store.
	if p.cfg.Subtree.StorageMode == "realtime" {
		if err := p.subtreeStore.StoreSubtree(subtreeMsg.Hash, rawData, 0); err != nil {
			p.Logger.Error("failed to store subtree", "hash", subtreeMsg.Hash, "error", err)
			return fmt.Errorf("storing subtree %s: %w", subtreeMsg.Hash, err)
		}
	}

	// 3.4: Parse raw binary data into txid list.
	// DataHub returns concatenated 32-byte hashes, not full go-subtree Serialize() format.
	txids, err := datahub.ParseRawTxids(rawData)
	if err != nil {
		p.Logger.Error("failed to parse subtree txids", "hash", subtreeMsg.Hash, "error", err)
		return fmt.Errorf("parsing subtree %s: %w", subtreeMsg.Hash, err)
	}
	p.Logger.Debug("processing subtree txids", "length", len(txids), "hash", subtreeMsg.Hash)

	if len(txids) == 0 {
		if p.dedupCache != nil {
			p.dedupCache.Add(subtreeMsg.Hash)
		}
		p.messagesProcessed.Add(1)
		return nil
	}

	// 4.2-4.4: Check registrations via cache and Aerospike.
	registeredTxids, err := p.findRegisteredTxids(txids)
	if err != nil {
		p.Logger.Error("failed to check registrations", "hash", subtreeMsg.Hash, "error", err)
		return fmt.Errorf("checking registrations for subtree %s: %w", subtreeMsg.Hash, err)
	}

	// 4.5-4.6: Emit callbacks for registered txids.
	for _, txid := range registeredTxids {
		if err := p.emitSeenCallbacks(txid, subtreeMsg.Hash); err != nil {
			p.Logger.Error("failed to emit callbacks", "txid", txid, "error", err)
		}
	}

	// Mark subtree as successfully processed for dedup.
	if p.dedupCache != nil {
		p.dedupCache.Add(subtreeMsg.Hash)
	}

	p.messagesProcessed.Add(1)
	return nil
}

// findRegisteredTxids uses the cache and Aerospike to find which txids are registered.
func (p *Processor) findRegisteredTxids(txids []string) ([]string, error) {
	var uncached, cachedRegistered []string

	if p.regCache != nil {
		uncached, cachedRegistered = p.regCache.FilterUncached(txids)
	} else {
		uncached = txids
	}

	// 4.3: Batch lookup uncached txids in Aerospike.
	var registeredFromStore map[string][]string
	if len(uncached) > 0 {
		var err error
		registeredFromStore, err = p.registrationStore.BatchGet(uncached)
		if err != nil {
			return nil, fmt.Errorf("batch get registrations: %w", err)
		}
	}

	// 4.4: Update cache with results.
	if p.regCache != nil {
		foundTxids := make([]string, 0, len(registeredFromStore))
		notFoundTxids := make([]string, 0, len(uncached)-len(registeredFromStore))
		for _, txid := range uncached {
			if _, found := registeredFromStore[txid]; found {
				foundTxids = append(foundTxids, txid)
			} else {
				notFoundTxids = append(notFoundTxids, txid)
			}
		}
		if len(foundTxids) > 0 {
			_ = p.regCache.SetMultiRegistered(foundTxids)
		}
		if len(notFoundTxids) > 0 {
			_ = p.regCache.SetMultiNotRegistered(notFoundTxids)
		}
	}

	// Combine cached registered + newly found registered.
	allRegistered := make([]string, 0, len(cachedRegistered)+len(registeredFromStore))
	allRegistered = append(allRegistered, cachedRegistered...)
	for txid := range registeredFromStore {
		allRegistered = append(allRegistered, txid)
	}

	return allRegistered, nil
}

// emitSeenCallbacks emits SEEN_ON_NETWORK callbacks and checks the seen counter for SEEN_MULTIPLE_NODES.
func (p *Processor) emitSeenCallbacks(txid string, subtreeID string) error {
	// Get callback URLs for this txid.
	callbacks, err := p.registrationStore.Get(txid)
	if err != nil {
		return fmt.Errorf("getting callbacks for %s: %w", txid, err)
	}

	// 4.5: Emit SEEN_ON_NETWORK for each callback URL.
	for _, callbackURL := range callbacks {
		stumpsMsg := &kafka.StumpsMessage{
			CallbackURL: callbackURL,
			TxID:        txid,
			StatusType:  kafka.StatusSeenOnNetwork,
			SubtreeID:   subtreeID,
		}
		data, err := stumpsMsg.Encode()
		if err != nil {
			p.Logger.Error("failed to encode stumps message", "txid", txid, "error", err)
			continue
		}
		if err := p.stumpsProducer.Publish(txid, data); err != nil {
			p.Logger.Error("failed to publish SEEN_ON_NETWORK", "txid", txid, "error", err)
		}
	}

	// 4.6: Increment seen counter and check threshold (idempotent per subtreeID).
	result, err := p.seenCounterStore.Increment(txid, subtreeID)
	if err != nil {
		p.Logger.Warn("failed to increment seen counter", "txid", txid, "error", err)
		return nil
	}

	if result.ThresholdReached {
		for _, callbackURL := range callbacks {
			stumpsMsg := &kafka.StumpsMessage{
				CallbackURL: callbackURL,
				TxID:        txid,
				StatusType:  kafka.StatusSeenMultiNodes,
				SubtreeID:   subtreeID,
			}
			data, err := stumpsMsg.Encode()
			if err != nil {
				p.Logger.Error("failed to encode stumps message", "txid", txid, "error", err)
				continue
			}
			if err := p.stumpsProducer.Publish(txid, data); err != nil {
				p.Logger.Error("failed to publish SEEN_MULTIPLE_NODES", "txid", txid, "error", err)
			}
		}
	}

	return nil
}
