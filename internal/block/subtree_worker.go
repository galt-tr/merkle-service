package block

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/IBM/sarama"

	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/datahub"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

// SubtreeWorkerService consumes SubtreeWorkMessages from Kafka, processes each
// subtree (registration lookup, STUMP build, MINED callback publishing), writes
// STUMPs to the shared cache, and coordinates BLOCK_PROCESSED emission via an
// Aerospike subtree counter.
type SubtreeWorkerService struct {
	service.BaseService

	kafkaCfg       config.KafkaConfig
	blockCfg       config.BlockConfig
	datahubCfg     config.DataHubConfig
	consumer       *kafka.Consumer
	stumpsProducer *kafka.Producer
	regStore       *store.RegistrationStore
	subtreeStore   *store.SubtreeStore
	urlRegistry    *store.CallbackURLRegistry
	subtreeCounter      *store.SubtreeCounterStore
	stumpCache          store.StumpCache
	callbackAccumulator *store.CallbackAccumulatorStore
	dataHubClient       *datahub.Client
}

func NewSubtreeWorkerService(
	kafkaCfg config.KafkaConfig,
	blockCfg config.BlockConfig,
	datahubCfg config.DataHubConfig,
	regStore *store.RegistrationStore,
	subtreeStore *store.SubtreeStore,
	urlRegistry *store.CallbackURLRegistry,
	subtreeCounter *store.SubtreeCounterStore,
	stumpCache store.StumpCache,
	callbackAccumulator *store.CallbackAccumulatorStore,
	logger *slog.Logger,
) *SubtreeWorkerService {
	s := &SubtreeWorkerService{
		kafkaCfg:            kafkaCfg,
		blockCfg:            blockCfg,
		datahubCfg:          datahubCfg,
		regStore:            regStore,
		subtreeStore:        subtreeStore,
		urlRegistry:         urlRegistry,
		subtreeCounter:      subtreeCounter,
		stumpCache:          stumpCache,
		callbackAccumulator: callbackAccumulator,
	}
	s.InitBase("subtree-worker")
	if logger != nil {
		s.Logger = logger
	}
	return s
}

func (s *SubtreeWorkerService) Init(_ interface{}) error {
	s.dataHubClient = datahub.NewClient(s.datahubCfg.TimeoutSec, s.datahubCfg.MaxRetries, s.Logger)

	// Create stumps producer for MINED callbacks.
	stumpsProducer, err := kafka.NewProducer(
		s.kafkaCfg.Brokers,
		s.kafkaCfg.StumpsTopic,
		s.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create stumps producer: %w", err)
	}
	s.stumpsProducer = stumpsProducer

	// Create consumer for the subtree-work topic.
	consumer, err := kafka.NewConsumer(
		s.kafkaCfg.Brokers,
		s.kafkaCfg.ConsumerGroup+"-subtree-worker",
		[]string{s.kafkaCfg.SubtreeWorkTopic},
		s.handleMessage,
		s.Logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create subtree-work consumer: %w", err)
	}
	s.consumer = consumer

	s.Logger.Info("subtree worker service initialized",
		"subtreeWorkTopic", s.kafkaCfg.SubtreeWorkTopic,
	)
	return nil
}

func (s *SubtreeWorkerService) Start(ctx context.Context) error {
	s.Logger.Info("starting subtree worker service")
	s.SetStarted(true)
	return s.consumer.Start(ctx)
}

func (s *SubtreeWorkerService) Stop() error {
	s.Logger.Info("stopping subtree worker service")
	s.SetStarted(false)
	var firstErr error
	if s.consumer != nil {
		if err := s.consumer.Stop(); err != nil {
			firstErr = err
		}
	}
	if s.stumpsProducer != nil {
		if err := s.stumpsProducer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *SubtreeWorkerService) Health() service.HealthStatus {
	status := "healthy"
	if !s.IsStarted() {
		status = "unhealthy"
	}
	return service.HealthStatus{
		Name:   "subtree-worker",
		Status: status,
	}
}

func (s *SubtreeWorkerService) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	workMsg, err := kafka.DecodeSubtreeWorkMessage(msg.Value)
	if err != nil {
		s.Logger.Error("failed to decode subtree work message", "error", err)
		return fmt.Errorf("failed to decode subtree work message: %w", err)
	}

	s.Logger.Debug("processing subtree work item",
		"subtreeHash", workMsg.SubtreeHash,
		"blockHash", workMsg.BlockHash,
		"blockHeight", workMsg.BlockHeight,
	)

	// Process the subtree using the existing logic, with STUMP cache for StumpRef.
	result, err := ProcessBlockSubtree(
		ctx,
		workMsg.SubtreeHash,
		uint64(workMsg.BlockHeight),
		workMsg.BlockHash,
		workMsg.DataHubURL,
		s.dataHubClient,
		s.subtreeStore,
		s.regStore,
		s.blockCfg.PostMineTTLSec,
		s.Logger,
		s.stumpCache,
	)
	if err != nil {
		s.Logger.Error("failed to process subtree work item",
			"subtreeHash", workMsg.SubtreeHash,
			"blockHash", workMsg.BlockHash,
			"error", err,
		)
		// Don't return error — continue to decrement counter so BLOCK_PROCESSED
		// can still fire. The subtree processing failure is logged.
	}

	// Accumulate callback data for batched publishing (or publish individually if no accumulator).
	if result != nil && len(result.CallbackGroups) > 0 {
		if s.callbackAccumulator != nil {
			for callbackURL, txids := range result.CallbackGroups {
				if err := s.callbackAccumulator.Append(workMsg.BlockHash, callbackURL, txids, result.SubtreeHash); err != nil {
					s.Logger.Error("failed to append to callback accumulator",
						"blockHash", workMsg.BlockHash,
						"callbackURL", callbackURL,
						"error", err,
					)
				}
			}
		} else {
			s.publishIndividualCallbacks(workMsg, result)
		}
	}

	// Decrement the subtree counter. If it reaches zero, flush batched callbacks and emit BLOCK_PROCESSED.
	if s.subtreeCounter != nil {
		remaining, err := s.subtreeCounter.Decrement(workMsg.BlockHash)
		if err != nil {
			s.Logger.Error("failed to decrement subtree counter",
				"blockHash", workMsg.BlockHash,
				"error", err,
			)
		} else if remaining == 0 {
			s.flushBatchedCallbacks(workMsg.BlockHash)
			s.emitBlockProcessed(workMsg.BlockHash)
		}
	}

	return nil
}

// publishIndividualCallbacks publishes one StumpsMessage per callback URL (non-batched fallback).
func (s *SubtreeWorkerService) publishIndividualCallbacks(workMsg *kafka.SubtreeWorkMessage, result *SubtreeResult) {
	for callbackURL, txids := range result.CallbackGroups {
		msg := &kafka.StumpsMessage{
			CallbackURL: callbackURL,
			TxIDs:       txids,
			StatusType:  kafka.StatusMined,
			BlockHash:   workMsg.BlockHash,
			SubtreeID:   result.SubtreeHash,
			StumpRef:    result.SubtreeHash,
		}
		data, err := msg.Encode()
		if err != nil {
			s.Logger.Error("failed to encode MINED stumps message",
				"callbackURL", callbackURL, "error", err)
			continue
		}
		if err := s.stumpsProducer.PublishWithHashKey(callbackURL, data); err != nil {
			s.Logger.Error("failed to publish MINED callback",
				"callbackURL", callbackURL, "error", err)
		}
	}
}

// flushBatchedCallbacks reads all accumulated callback data for a block and publishes
// one batched StumpsMessage per callback URL.
func (s *SubtreeWorkerService) flushBatchedCallbacks(blockHash string) {
	if s.callbackAccumulator == nil {
		return
	}

	accumulated, err := s.callbackAccumulator.ReadAndDelete(blockHash)
	if err != nil {
		s.Logger.Error("failed to read callback accumulator",
			"blockHash", blockHash, "error", err)
		return
	}

	for callbackURL, acc := range accumulated {
		msg := &kafka.StumpsMessage{
			CallbackURL: callbackURL,
			TxIDs:       acc.TxIDs,
			StumpRefs:   acc.StumpRefs,
			StatusType:  kafka.StatusMined,
			BlockHash:   blockHash,
		}
		data, err := msg.Encode()
		if err != nil {
			s.Logger.Error("failed to encode batched MINED message",
				"callbackURL", callbackURL, "error", err)
			continue
		}
		if err := s.stumpsProducer.PublishWithHashKey(callbackURL, data); err != nil {
			s.Logger.Error("failed to publish batched MINED callback",
				"callbackURL", callbackURL, "error", err)
		}
	}

	s.Logger.Info("flushed batched callbacks",
		"blockHash", blockHash,
		"callbackURLs", len(accumulated),
	)
}

// emitBlockProcessed publishes a BLOCK_PROCESSED message to every registered callback URL.
func (s *SubtreeWorkerService) emitBlockProcessed(blockHash string) {
	if s.urlRegistry == nil {
		return
	}

	urls, err := s.urlRegistry.GetAll()
	if err != nil {
		s.Logger.Error("failed to get callback URLs for BLOCK_PROCESSED", "error", err)
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
			s.Logger.Error("failed to encode BLOCK_PROCESSED message",
				"callbackURL", callbackURL,
				"error", err,
			)
			continue
		}
		if err := s.stumpsProducer.PublishWithHashKey(callbackURL, data); err != nil {
			s.Logger.Error("failed to publish BLOCK_PROCESSED callback",
				"callbackURL", callbackURL,
				"error", err,
			)
		}
	}

	s.Logger.Info("emitted BLOCK_PROCESSED callbacks",
		"blockHash", blockHash,
		"callbackURLs", len(urls),
	)
}
