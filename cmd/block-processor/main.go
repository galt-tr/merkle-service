package main

import (
	"context"
	"log"
	"log/slog"

	"github.com/bsv-blockchain/merkle-service/internal/block"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
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

	// Create Kafka stumps producer.
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

	// Create, init, and start the block processor.
	processor := block.NewProcessor(cfg.Kafka, cfg.Block, cfg.DataHub, stumpsProducer, regStore, subtreeStore, urlRegistry, nil, logger)

	if err := processor.Init(nil); err != nil {
		log.Fatal("failed to init block processor: ", err)
	}

	ctx := context.Background()
	if err := processor.Start(ctx); err != nil {
		log.Fatal("failed to start block processor: ", err)
	}

	// Wait for shutdown signal.
	var base service.BaseService
	base.InitBase("block-processor")
	base.WaitForShutdown(ctx)

	if err := processor.Stop(); err != nil {
		logger.Error("failed to stop block processor", "error", err)
	}
}
