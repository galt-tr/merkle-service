package block

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/IBM/sarama"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
	"golang.org/x/sync/errgroup"
)

// Processor implements the block processor service.
type Processor struct {
	service.BaseService
	kafkaCfg       config.KafkaConfig
	blockCfg       config.BlockConfig
	consumer       *kafka.Consumer
	stumpsProducer *kafka.Producer
	regStore       *store.RegistrationStore
	subtreeStore   *store.SubtreeStore
}

func NewProcessor(
	kafkaCfg config.KafkaConfig,
	blockCfg config.BlockConfig,
	stumpsProducer *kafka.Producer,
	regStore *store.RegistrationStore,
	subtreeStore *store.SubtreeStore,
	logger *slog.Logger,
) *Processor {
	p := &Processor{
		kafkaCfg:       kafkaCfg,
		blockCfg:       blockCfg,
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

	p.Logger.Info("processing block",
		"blockHash", blockMsg.BlockHash,
		"blockHeight", blockMsg.BlockHeight,
		"subtreeCount", len(blockMsg.SubtreeRefs),
	)

	// Update current block height for DAH
	p.subtreeStore.SetCurrentBlockHeight(blockMsg.BlockHeight)

	// Process all subtrees in parallel with bounded worker pool
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.blockCfg.WorkerPoolSize)

	allRegisteredTxIDs := make(chan string, 1000)

	for i, subtreeRef := range blockMsg.SubtreeRefs {
		subtreeID := subtreeRef
		subtreeIdx := i
		g.Go(func() error {
			return p.processSubtree(gctx, blockMsg, subtreeID, subtreeIdx, allRegisteredTxIDs)
		})
	}

	// Wait for all subtree processors to complete
	go func() {
		g.Wait()
		close(allRegisteredTxIDs)
	}()

	// Collect all registered txids for TTL update
	var registeredTxIDs []string
	for txid := range allRegisteredTxIDs {
		registeredTxIDs = append(registeredTxIDs, txid)
	}

	if err := g.Wait(); err != nil {
		p.Logger.Error("block processing error", "blockHash", blockMsg.BlockHash, "error", err)
		return err
	}

	// Post-block TTL updates
	if len(registeredTxIDs) > 0 {
		ttl := time.Duration(p.blockCfg.PostMineTTLSec) * time.Second
		if err := p.regStore.BatchUpdateTTL(registeredTxIDs, ttl); err != nil {
			p.Logger.Error("failed to update TTLs", "error", err)
		}
	}

	// Cleanup subtree store
	for _, subtreeRef := range blockMsg.SubtreeRefs {
		if err := p.subtreeStore.DeleteSubtree(subtreeRef); err != nil {
			p.Logger.Warn("failed to delete subtree (DAH will handle)", "subtreeId", subtreeRef, "error", err)
		}
	}

	p.Logger.Info("block processing complete",
		"blockHash", blockMsg.BlockHash,
		"registeredTxCount", len(registeredTxIDs),
	)

	return nil
}
