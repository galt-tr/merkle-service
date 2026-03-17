package main

import (
	"context"
	"log"
	"log/slog"

	"github.com/bsv-blockchain/merkle-service/internal/block"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

func main() {
	logger := slog.Default()

	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	// Create Aerospike client.
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

	// Create stores.
	regStore := store.NewRegistrationStore(
		asClient,
		cfg.Aerospike.SetName,
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

	urlRegistry := store.NewCallbackURLRegistry(
		asClient,
		cfg.Aerospike.CallbackURLRegistry,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	subtreeCounter := store.NewSubtreeCounterStore(
		asClient,
		cfg.Aerospike.SubtreeCounterSet,
		cfg.Aerospike.SubtreeCounterTTLSec,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	// Create STUMP cache (aerospike mode for K8s, memory for dev).
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

	// Create, init, and start the subtree worker service.
	// Create callback accumulator for cross-subtree batching.
	callbackAccumulator := store.NewCallbackAccumulatorStore(
		asClient,
		cfg.Aerospike.CallbackAccumulatorSet,
		cfg.Aerospike.CallbackAccumulatorTTLSec,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	worker := block.NewSubtreeWorkerService(
		cfg.Kafka, cfg.Block, cfg.DataHub,
		regStore, subtreeStore, urlRegistry, subtreeCounter, stumpCache,
		callbackAccumulator, logger,
	)

	if err := worker.Init(nil); err != nil {
		log.Fatal("failed to init subtree worker: ", err)
	}

	ctx := context.Background()
	if err := worker.Start(ctx); err != nil {
		log.Fatal("failed to start subtree worker: ", err)
	}

	// Wait for shutdown signal.
	var base service.BaseService
	base.InitBase("subtree-worker")
	base.WaitForShutdown(ctx)

	if err := worker.Stop(); err != nil {
		logger.Error("failed to stop subtree worker", "error", err)
	}
}
