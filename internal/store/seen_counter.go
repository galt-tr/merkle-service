package store

import (
	"fmt"
	"log/slog"

	as "github.com/aerospike/aerospike-client-go/v7"
)

const (
	seenSubtreesBin    = "subtrees"
	seenThresholdFired = "tfired"
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

// Increment idempotently records that a txid was seen in a specific subtree.
// Uses Aerospike CDT list with AddUnique to ensure each subtreeID is counted only once.
// ThresholdReached is true only once: when the unique count first reaches the threshold.
func (s *SeenCounterStore) Increment(txid string, subtreeID string) (*IncrementResult, error) {
	key, err := as.NewKey(s.client.Namespace(), s.setName, txid)
	if err != nil {
		return nil, fmt.Errorf("failed to create key: %w", err)
	}

	wp := s.client.WritePolicy(s.maxRetries, s.retryBaseMs)
	wp.RecordExistsAction = as.UPDATE

	// AddUnique + NoFail: appends subtreeID only if not already present, no error on duplicate.
	listPolicy := as.NewListPolicy(as.ListOrderUnordered, as.ListWriteFlagsAddUnique|as.ListWriteFlagsNoFail)
	ops := []*as.Operation{
		as.ListAppendWithPolicyOp(listPolicy, seenSubtreesBin, subtreeID),
		as.ListSizeOp(seenSubtreesBin),
		as.GetBinOp(seenThresholdFired),
	}

	record, err := s.client.Client().Operate(wp, key, ops...)
	if err != nil {
		return nil, fmt.Errorf("failed to increment seen counter: %w", err)
	}

	sizeVal := record.Bins[seenSubtreesBin]
	newSize, ok := sizeVal.(int)
	if !ok {
		return nil, fmt.Errorf("unexpected type for seen counter list size: %T", sizeVal)
	}

	// Check if threshold was already fired previously.
	alreadyFired := false
	if firedVal := record.Bins[seenThresholdFired]; firedVal != nil {
		if v, ok := firedVal.(int); ok && v == 1 {
			alreadyFired = true
		}
	}

	thresholdReached := false
	if newSize >= s.threshold && !alreadyFired {
		// Mark threshold as fired so it won't fire again.
		thresholdReached = true
		markWP := s.client.WritePolicy(s.maxRetries, s.retryBaseMs)
		markWP.RecordExistsAction = as.UPDATE
		_ = s.client.Client().Put(markWP, key, as.BinMap{seenThresholdFired: 1})
	}

	return &IncrementResult{
		NewCount:         newSize,
		ThresholdReached: thresholdReached,
	}, nil
}

// Threshold returns the configured threshold.
func (s *SeenCounterStore) Threshold() int {
	return s.threshold
}
