package main

import (
	"context"
	"log"
	"log/slog"

	"github.com/bsv-blockchain/merkle-service/internal/callback"
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

	// Create Aerospike client for callback dedup.
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

	callbackDedupStore := store.NewCallbackDedupStore(
		asClient,
		cfg.Aerospike.CallbackDedupSet,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	// Create STUMP cache for StumpRef resolution.
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

	// Create, init, and start the callback delivery service.
	deliverySvc := callback.NewDeliveryService(cfg, callbackDedupStore, stumpCache)

	if err := deliverySvc.Init(nil); err != nil {
		log.Fatal("failed to init callback delivery service: ", err)
	}

	ctx := context.Background()
	if err := deliverySvc.Start(ctx); err != nil {
		log.Fatal("failed to start callback delivery service: ", err)
	}

	// Wait for shutdown signal.
	var base service.BaseService
	base.InitBase("callback-delivery")
	base.WaitForShutdown(ctx)

	if err := deliverySvc.Stop(); err != nil {
		logger.Error("failed to stop callback delivery service", "error", err)
	}
}
