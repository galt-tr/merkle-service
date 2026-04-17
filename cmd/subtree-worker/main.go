package main

import (
	"context"
	"log"

	"github.com/bsv-blockchain/merkle-service/internal/block"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

func main() {
	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	logger := service.NewLogger(config.ParseLogLevel(cfg.LogLevel))

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
	stumpStore := store.NewStumpStore(
		blobStore,
		uint64(cfg.Subtree.StumpDAHOffset),
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

	worker := block.NewSubtreeWorkerService(
		cfg.Kafka, cfg.Block, cfg.DataHub,
		regStore, subtreeStore, stumpStore, urlRegistry, subtreeCounter,
		logger,
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
