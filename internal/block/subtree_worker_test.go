package block

import (
	"io"
	"log/slog"
	"testing"

	"github.com/bsv-blockchain/merkle-service/internal/config"
	"github.com/bsv-blockchain/merkle-service/internal/kafka"
	"github.com/bsv-blockchain/merkle-service/internal/store"
)

func TestSubtreeWorkerService_NewAndHealth(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache := store.NewMemoryStumpCache(300)

	svc := NewSubtreeWorkerService(
		config.KafkaConfig{SubtreeWorkTopic: "subtree-work"},
		config.BlockConfig{},
		config.DataHubConfig{},
		nil, // regStore
		nil, // subtreeStore
		nil, // urlRegistry
		nil, // subtreeCounter
		cache,
		nil, // callbackAccumulator
		logger,
	)

	if svc == nil {
		t.Fatal("expected non-nil service")
	}

	health := svc.Health()
	if health.Name != "subtree-worker" {
		t.Errorf("expected name 'subtree-worker', got %s", health.Name)
	}
	if health.Status != "unhealthy" {
		t.Errorf("expected 'unhealthy' before start, got %s", health.Status)
	}
}

func TestSubtreeWorkerService_EmitBlockProcessed_NilRegistry(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock := &mockSyncProducer{}

	svc := &SubtreeWorkerService{
		urlRegistry: nil,
	}
	svc.InitBase("subtree-worker-test")
	svc.Logger = logger
	svc.stumpsProducer = newTestKafkaProducer(mock, "stumps-test", logger)

	// Should not panic with nil registry.
	svc.emitBlockProcessed("blockhash-123")

	if len(mock.messages) != 0 {
		t.Errorf("expected no messages with nil registry, got %d", len(mock.messages))
	}
}

func newTestKafkaProducer(mock *mockSyncProducer, topic string, logger *slog.Logger) *kafka.Producer {
	return kafka.NewTestProducer(mock, topic, logger)
}
