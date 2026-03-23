package main

import (
	"context"
	"log"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/service"
	"github.com/bsv-blockchain/merkle-service/internal/store"
	"github.com/bsv-blockchain/merkle-service/internal/subtree"
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

	// Create, init, and start the subtree processor.
	processor := subtree.NewProcessor(cfg, regStore, seenStore, subtreeStore)

	if err := processor.Init(nil); err != nil {
		log.Fatal("failed to init subtree processor: ", err)
	}

	ctx := context.Background()
	if err := processor.Start(ctx); err != nil {
		log.Fatal("failed to start subtree processor: ", err)
	}

	// Wait for shutdown signal.
	var base service.BaseService
	base.InitBase("subtree-fetcher")
	base.WaitForShutdown(ctx)

	if err := processor.Stop(); err != nil {
		logger.Error("failed to stop subtree processor", "error", err)
	}
}
