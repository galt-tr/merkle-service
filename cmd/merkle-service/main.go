package main

import (
	"context"
	"log"
	"log/slog"

	"github.com/bsv-blockchain/merkle-service/internal/api"
	"github.com/bsv-blockchain/merkle-service/internal/block"
	"github.com/bsv-blockchain/merkle-service/internal/callback"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/p2p"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
	"github.com/bsv-blockchain/merkle-service/internal/subtree"
)

func main() {
	logger := slog.Default()

	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	// Create shared Aerospike client.
	asClient, err := store.NewAerospikeClient(
		cfg.Aerospike.Host,
		cfg.Aerospike.Port,
		cfg.Aerospike.Namespace,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)
	if err != nil {
		log.Fatal("failed to create aerospike client: ", err)
	}
	defer asClient.Close()

	// Create shared stores.
	regStore := store.NewRegistrationStore(
		asClient,
		cfg.Aerospike.SetName,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	seenStore := store.NewSeenCounterStore(
		asClient,
		cfg.Aerospike.SeenSet,
		cfg.Callback.SeenThreshold,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	blobStore, err := store.NewBlobStoreFromURL(cfg.BlobStore.URL)
	if err != nil {
		log.Fatal("failed to create blob store: ", err)
	}
	subtreeStore := store.NewSubtreeStore(
		blobStore,
		uint64(cfg.Subtree.DAHOffset),
		logger,
	)

	// Create Kafka producers.
	subtreeProducer, err := kafka.NewProducer(cfg.Kafka.Brokers, cfg.Kafka.SubtreeTopic, logger)
	if err != nil {
		log.Fatal("failed to create subtree producer: ", err)
	}
	defer subtreeProducer.Close()

	blockProducer, err := kafka.NewProducer(cfg.Kafka.Brokers, cfg.Kafka.BlockTopic, logger)
	if err != nil {
		log.Fatal("failed to create block producer: ", err)
	}
	defer blockProducer.Close()

	stumpsProducer, err := kafka.NewProducer(cfg.Kafka.Brokers, cfg.Kafka.StumpsTopic, logger)
	if err != nil {
		log.Fatal("failed to create stumps producer: ", err)
	}
	defer stumpsProducer.Close()

	urlRegistry := store.NewCallbackURLRegistry(
		asClient,
		cfg.Aerospike.CallbackURLRegistry,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	// Create shared STUMP cache (used by subtree worker and delivery service).
	stumpCache, err := store.NewStumpCacheFromConfig(
		cfg.Callback.StumpCacheMode,
		asClient,
		cfg.Aerospike.StumpCacheSet,
		cfg.Callback.StumpCacheTTLSec,
		cfg.Callback.StumpCacheLRUSize,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
	)
	if err != nil {
		log.Fatal("failed to create stump cache: ", err)
	}
	defer stumpCache.Close()

	// Create subtree counter store for BLOCK_PROCESSED coordination.
	subtreeCounter := store.NewSubtreeCounterStore(
		asClient,
		cfg.Aerospike.SubtreeCounterSet,
		cfg.Aerospike.SubtreeCounterTTLSec,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	// Create callback accumulator for cross-subtree batching.
	callbackAccumulator := store.NewCallbackAccumulatorStore(
		asClient,
		cfg.Aerospike.CallbackAccumulatorSet,
		cfg.Aerospike.CallbackAccumulatorTTLSec,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	// Create services.
	apiServer := api.NewServer(cfg.API, regStore, urlRegistry, asClient, logger)
	p2pClient := p2p.NewClient(cfg.P2P, subtreeProducer, blockProducer, logger)
	subtreeFetcher := subtree.NewProcessor(cfg, regStore, seenStore, subtreeStore)
	blockProcessor := block.NewProcessor(cfg.Kafka, cfg.Block, cfg.DataHub, stumpsProducer, regStore, subtreeStore, urlRegistry, subtreeCounter, logger)
	subtreeWorker := block.NewSubtreeWorkerService(cfg.Kafka, cfg.Block, cfg.DataHub, regStore, subtreeStore, urlRegistry, subtreeCounter, stumpCache, callbackAccumulator, logger)
	callbackDedupStore := store.NewCallbackDedupStore(
		asClient,
		cfg.Aerospike.CallbackDedupSet,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)
	callbackDelivery := callback.NewDeliveryService(cfg, callbackDedupStore, stumpCache)

	// Initialize all services.
	services := []service.Service{apiServer, p2pClient, subtreeFetcher, blockProcessor, subtreeWorker, callbackDelivery}
	for _, svc := range services {
		if err := svc.Init(nil); err != nil {
			log.Fatal("failed to init service: ", err)
		}
	}

	// Start all services.
	ctx := context.Background()
	for _, svc := range services {
		if err := svc.Start(ctx); err != nil {
			log.Fatal("failed to start service: ", err)
		}
	}

	// Wait for shutdown signal.
	var base service.BaseService
	base.InitBase("merkle-service")
	base.WaitForShutdown(ctx)

	// Stop all services in reverse order.
	for i := len(services) - 1; i >= 0; i-- {
		if err := services[i].Stop(); err != nil {
			logger.Error("failed to stop service", "error", err)
		}
	}
}
