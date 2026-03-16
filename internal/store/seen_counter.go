package store

import (
	"fmt"
	"log/slog"

	as "github.com/aerospike/aerospike-client-go/v7"
)

const (
	seenCountBin = "count"
)

// SeenCounterStore manages atomic seen-count tracking per txid in Aerospike.
type SeenCounterStore struct {
	client      *AerospikeClient
	setName     string
	threshold   int
	logger      *slog.Logger
	maxRetries  int
	retryBaseMs int
}

func NewSeenCounterStore(client *AerospikeClient, setName string, threshold int, maxRetries int, retryBaseMs int, logger *slog.Logger) *SeenCounterStore {
	return &SeenCounterStore{
		client:      client,
		setName:     setName,
		threshold:   threshold,
		logger:      logger,
		maxRetries:  maxRetries,
		retryBaseMs: retryBaseMs,
	}
}

// IncrementResult holds the result of an increment operation.
type IncrementResult struct {
	NewCount         int
	ThresholdReached bool // true only when count equals threshold (not above)
}

// Increment atomically increments the seen counter for a txid and returns the new count.
func (s *SeenCounterStore) Increment(txid string) (*IncrementResult, error) {
	key, err := as.NewKey(s.client.Namespace(), s.setName, txid)
	if err != nil {
		return nil, fmt.Errorf("failed to create key: %w", err)
	}

	wp := s.client.WritePolicy(s.maxRetries, s.retryBaseMs)
	wp.RecordExistsAction = as.UPDATE

	ops := []*as.Operation{
		as.AddOp(as.NewBin(seenCountBin, 1)),
		as.GetOp(),
	}

	record, err := s.client.Client().Operate(wp, key, ops...)
	if err != nil {
		return nil, fmt.Errorf("failed to increment seen counter: %w", err)
	}

	countVal := record.Bins[seenCountBin]
	count, ok := countVal.(int)
	if !ok {
		return nil, fmt.Errorf("unexpected type for seen counter")
	}

	return &IncrementResult{
		NewCount:         count,
		ThresholdReached: count == s.threshold,
	}, nil
}

// Threshold returns the configured threshold.
func (s *SeenCounterStore) Threshold() int {
	return s.threshold
}
