package block

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/IBM/sarama"
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
	dataHubClient  *datahub.Client
}

func NewProcessor(
	kafkaCfg config.KafkaConfig,
	blockCfg config.BlockConfig,
	datahubCfg config.DataHubConfig,
	stumpsProducer *kafka.Producer,
	regStore *store.RegistrationStore,
	subtreeStore *store.SubtreeStore,
	logger *slog.Logger,
) *Processor {
	p := &Processor{
		kafkaCfg:   kafkaCfg,
		blockCfg:   blockCfg,
		datahubCfg: datahubCfg,
		stumpsProducer: stumpsProducer,
		regStore:       regStore,
		subtreeStore:   subtreeStore,
	}
	p.InitBase("block-processor")
	if logger != nil {
		p.Logger = logger
	}
	return p
}

func (p *Processor) Init(cfg interface{}) error {
	p.dataHubClient = datahub.NewClient(p.datahubCfg.TimeoutSec, p.datahubCfg.MaxRetries, p.Logger)

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
	var mu sync.Mutex

	for _, stHash := range subtreeHashes {
		wg.Add(1)
		sem <- struct{}{}

		go func(subtreeHash string) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := ProcessBlockSubtree(
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
			); err != nil {
				p.Logger.Error("failed to process block subtree",
					"subtreeHash", subtreeHash,
					"blockHash", blockMsg.Hash,
					"error", err,
				)
				mu.Lock()
				errCount++
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

	// 7.4: Update subtree store block height for DAH pruning.
	p.subtreeStore.SetCurrentBlockHeight(uint64(meta.Height))

	return nil
}
