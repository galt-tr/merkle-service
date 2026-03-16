package block

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/IBM/sarama"
	"github.com/bsv-blockchain/merkle-service/internal/cache"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/datahub"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

// Processor implements the block processor service.
type Processor struct {
	service.BaseService
	kafkaCfg       config.KafkaConfig
	blockCfg       config.BlockConfig
	datahubCfg     config.DataHubConfig
	consumer       *kafka.Consumer
	stumpsProducer *kafka.Producer
	regStore       *store.RegistrationStore
	subtreeStore   *store.SubtreeStore
	urlRegistry    *store.CallbackURLRegistry
	dataHubClient  *datahub.Client
	dedupCache     *cache.DedupCache
}

func NewProcessor(
	kafkaCfg config.KafkaConfig,
	blockCfg config.BlockConfig,
	datahubCfg config.DataHubConfig,
	stumpsProducer *kafka.Producer,
	regStore *store.RegistrationStore,
	subtreeStore *store.SubtreeStore,
	urlRegistry *store.CallbackURLRegistry,
	logger *slog.Logger,
) *Processor {
	p := &Processor{
		kafkaCfg:       kafkaCfg,
		blockCfg:       blockCfg,
		datahubCfg:     datahubCfg,
		stumpsProducer: stumpsProducer,
		regStore:       regStore,
		subtreeStore:   subtreeStore,
		urlRegistry:    urlRegistry,
	}
	p.InitBase("block-processor")
	if logger != nil {
		p.Logger = logger
	}
	return p
}

func (p *Processor) Init(cfg interface{}) error {
	p.dataHubClient = datahub.NewClient(p.datahubCfg.TimeoutSec, p.datahubCfg.MaxRetries, p.Logger)

	// Initialize message dedup cache.
	if p.blockCfg.DedupCacheSize > 0 {
		p.dedupCache = cache.NewDedupCache(p.blockCfg.DedupCacheSize)
	}

	consumer, err := kafka.NewConsumer(
		p.kafkaCfg.Brokers,
		p.kafkaCfg.ConsumerGroup+"-block",
		[]string{p.kafkaCfg.BlockTopic},
		p.handleMessage,
		p.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create block consumer: %w", err)
	}
	p.consumer = consumer
	return nil
}

func (p *Processor) Start(ctx context.Context) error {
	p.SetStarted(true)
	p.Logger.Info("starting block processor")
	return p.consumer.Start(ctx)
}

func (p *Processor) Stop() error {
	p.Logger.Info("stopping block processor")
	p.SetStarted(false)
	return p.consumer.Stop()
}

func (p *Processor) Health() service.HealthStatus {
	status := "healthy"
	if !p.IsStarted() {
		status = "unhealthy"
	}
	return service.HealthStatus{
		Name:   "block-processor",
		Status: status,
	}
}

func (p *Processor) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	blockMsg, err := kafka.DecodeBlockMessage(msg.Value)
	if err != nil {
		p.Logger.Error("failed to decode block message", "error", err)
		return err
	}

	p.Logger.Info("processing block announcement",
		"hash", blockMsg.Hash,
		"height", blockMsg.Height,
		"dataHubUrl", blockMsg.DataHubURL,
	)

	// Check dedup cache — skip if already successfully processed.
	if p.dedupCache != nil && p.dedupCache.Contains(blockMsg.Hash) {
		p.Logger.Debug("skipping duplicate block message", "hash", blockMsg.Hash)
		return nil
	}

	// 5.2: Fetch block metadata from DataHub.
	meta, err := p.dataHubClient.FetchBlockMetadata(ctx, blockMsg.DataHubURL, blockMsg.Hash)
	if err != nil {
		p.Logger.Error("failed to fetch block metadata", "hash", blockMsg.Hash, "error", err)
		return fmt.Errorf("fetching block metadata %s: %w", blockMsg.Hash, err)
	}

	// 5.3: Extract subtree hashes from block metadata.
	subtreeHashes := meta.Subtrees
	p.Logger.Info("block metadata fetched",
		"hash", blockMsg.Hash,
		"height", meta.Height,
		"subtreeCount", len(subtreeHashes),
	)

	if len(subtreeHashes) == 0 {
		return nil
	}

	// 7.1-7.3: Dispatch subtree processing to a bounded worker pool.
	poolSize := p.blockCfg.WorkerPoolSize
	if poolSize > len(subtreeHashes) {
		poolSize = len(subtreeHashes)
	}

	sem := make(chan struct{}, poolSize)
	var wg sync.WaitGroup
	var errCount int64
	var hadRegistrations int64
	var mu sync.Mutex

	for _, stHash := range subtreeHashes {
		wg.Add(1)
		sem <- struct{}{}

		go func(subtreeHash string) {
			defer wg.Done()
			defer func() { <-sem }()

			hadRegs, err := ProcessBlockSubtree(
				ctx,
				subtreeHash,
				uint64(meta.Height),
				blockMsg.Hash,
				blockMsg.DataHubURL,
				p.dataHubClient,
				p.subtreeStore,
				p.regStore,
				p.stumpsProducer,
				p.blockCfg.PostMineTTLSec,
				p.Logger,
			)
			if err != nil {
				p.Logger.Error("failed to process block subtree",
					"subtreeHash", subtreeHash,
					"blockHash", blockMsg.Hash,
					"error", err,
				)
				mu.Lock()
				errCount++
				mu.Unlock()
			}
			if hadRegs {
				mu.Lock()
				hadRegistrations++
				mu.Unlock()
			}
		}(stHash)
	}

	wg.Wait()

	if errCount > 0 {
		p.Logger.Warn("block processing completed with errors",
			"blockHash", blockMsg.Hash,
			"subtreeErrors", errCount,
			"totalSubtrees", len(subtreeHashes),
		)
	}

	// Emit BLOCK_PROCESSED if any subtree had registered txids.
	if hadRegistrations > 0 {
		p.emitBlockProcessed(blockMsg.Hash)
	}

	// 7.4: Update subtree store block height for DAH pruning.
	p.subtreeStore.SetCurrentBlockHeight(uint64(meta.Height))

	// Mark block as successfully processed for dedup.
	if p.dedupCache != nil {
		p.dedupCache.Add(blockMsg.Hash)
	}

	return nil
}

// emitBlockProcessed publishes a BLOCK_PROCESSED message to every registered callback URL.
func (p *Processor) emitBlockProcessed(blockHash string) {
	if p.urlRegistry == nil {
		return
	}

	urls, err := p.urlRegistry.GetAll()
	if err != nil {
		p.Logger.Error("failed to get callback URLs for BLOCK_PROCESSED", "error", err)
		return
	}
	if len(urls) == 0 {
		return
	}

	for _, callbackURL := range urls {
		msg := &kafka.StumpsMessage{
			CallbackURL: callbackURL,
			StatusType:  kafka.StatusBlockProcessed,
			BlockHash:   blockHash,
		}
		data, err := msg.Encode()
		if err != nil {
			p.Logger.Error("failed to encode BLOCK_PROCESSED message",
				"callbackURL", callbackURL,
				"error", err,
			)
			continue
		}
		if err := p.stumpsProducer.PublishWithHashKey(callbackURL, data); err != nil {
			p.Logger.Error("failed to publish BLOCK_PROCESSED callback",
				"callbackURL", callbackURL,
				"error", err,
			)
		}
	}

	p.Logger.Info("emitted BLOCK_PROCESSED callbacks",
		"blockHash", blockHash,
		"callbackURLs", len(urls),
	)
}
