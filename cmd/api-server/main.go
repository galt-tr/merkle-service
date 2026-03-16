package main

import (
	"context"
	"log"
	"log/slog"

	"github.com/bsv-blockchain/merkle-service/internal/api"
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

	// Create Aerospike client and registration store.
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

	regStore := store.NewRegistrationStore(
		asClient,
		cfg.Aerospike.SetName,
		cfg.Aerospike.MaxRetries,
		cfg.Aerospike.RetryBaseMs,
		logger,
	)

	// Create, init, and start the API server.
	server := api.NewServer(cfg.API, regStore, asClient, logger)

	if err := server.Init(nil); err != nil {
		log.Fatal("failed to init api server: ", err)
	}

	ctx := context.Background()
	if err := server.Start(ctx); err != nil {
		log.Fatal("failed to start api server: ", err)
	}

	// Wait for shutdown signal.
	var base service.BaseService
	base.InitBase("api-server")
	base.WaitForShutdown(ctx)

	if err := server.Stop(); err != nil {
		logger.Error("failed to stop api server", "error", err)
	}
}
