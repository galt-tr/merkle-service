package main

import (
	"context"
	"log"
	"log/slog"

	"github.com/bsv-blockchain/merkle-service/internal/callback"
	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/service"
)

func main() {
	logger := slog.Default()

	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	// Create, init, and start the callback delivery service.
	deliverySvc := callback.NewDeliveryService(cfg)

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
