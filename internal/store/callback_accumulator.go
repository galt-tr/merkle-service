package store

import (
	"errors"
	"fmt"
	"log/slog"

	as "github.com/aerospike/aerospike-client-go/v7"
	astypes "github.com/aerospike/aerospike-client-go/v7/types"
)

const accumEntriesBin = "entries"

// AccumulatedCallback holds the aggregated txids and stump references for a
// single callback URL across multiple subtrees within a block.
type AccumulatedCallback struct {
	TxIDs     []string
	StumpRefs []string
}

// accumEntry is the per-subtree entry stored in the Aerospike list.
// Each Append() call adds one entry per callback URL.
type accumEntry struct {
	URL      string   `json:"u"`
	TxIDs    []string `json:"t"`
	StumpRef string   `json:"r"`
}

// CallbackAccumulatorStore manages per-block callback accumulation in Aerospike.
// Subtree workers append entries as they process subtrees. When all subtrees for
// a block are done, the last worker reads and deletes the accumulated data, then
// publishes batched StumpsMessages.
type CallbackAccumulatorStore struct {
	client      *AerospikeClient
	setName     string
	ttlSec      int
	maxRetries  int
	retryBaseMs int
	logger      *slog.Logger
}

func NewCallbackAccumulatorStore(client *AerospikeClient, setName string, ttlSec int, maxRetries int, retryBaseMs int, logger *slog.Logger) *CallbackAccumulatorStore {
	return &CallbackAccumulatorStore{
		client:      client,
		setName:     setName,
		ttlSec:      ttlSec,
		maxRetries:  maxRetries,
		retryBaseMs: retryBaseMs,
		logger:      logger,
	}
}

// Append atomically appends callback data for a single subtree to the
// accumulation record for the given block. Thread-safe across pods.
func (s *CallbackAccumulatorStore) Append(blockHash, callbackURL string, txids []string, stumpRef string) error {
	key, err := as.NewKey(s.client.Namespace(), s.setName, blockHash)
	if err != nil {
		return fmt.Errorf("failed to create key: %w", err)
	}

	entry := map[string]interface{}{
		"u": callbackURL,
		"t": txids,
		"r": stumpRef,
	}

	wp := s.client.WritePolicy(s.maxRetries, s.retryBaseMs)
	wp.RecordExistsAction = as.UPDATE
	wp.Expiration = uint32(s.ttlSec)

	_, err = s.client.Client().Operate(wp, key,
		as.ListAppendOp(accumEntriesBin, entry),
	)
	if err != nil {
		return fmt.Errorf("failed to append to accumulator: %w", err)
	}
	return nil
}

// ReadAndDelete reads all accumulated callback data for the given block and
// deletes the record atomically. Returns a map of callbackURL → AccumulatedCallback.
func (s *CallbackAccumulatorStore) ReadAndDelete(blockHash string) (map[string]*AccumulatedCallback, error) {
	key, err := as.NewKey(s.client.Namespace(), s.setName, blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to create key: %w", err)
	}

	// Read the record first.
	record, err := s.client.Client().Get(nil, key, accumEntriesBin)
	if err != nil {
		var asErr *as.AerospikeError
		if errors.As(err, &asErr) && asErr.Matches(astypes.KEY_NOT_FOUND_ERROR) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read accumulator: %w", err)
	}
	if record == nil {
		return nil, nil
	}

	// Delete the record.
	wp := s.client.WritePolicy(s.maxRetries, s.retryBaseMs)
	_, err = s.client.Client().Delete(wp, key)
	if err != nil {
		s.logger.Warn("failed to delete accumulator record after read",
			"blockHash", blockHash, "error", err)
	}

	// Parse the entries list.
	binVal := record.Bins[accumEntriesBin]
	if binVal == nil {
		return nil, nil
	}

	list, ok := binVal.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected bin type for accumulator entries: %T", binVal)
	}

	result := make(map[string]*AccumulatedCallback)
	for _, item := range list {
		entryMap, ok := item.(map[interface{}]interface{})
		if !ok {
			continue
		}

		url, _ := entryMap["u"].(string)
		if url == "" {
			continue
		}

		acc, exists := result[url]
		if !exists {
			acc = &AccumulatedCallback{}
			result[url] = acc
		}

		if txidList, ok := entryMap["t"].([]interface{}); ok {
			for _, v := range txidList {
				if s, ok := v.(string); ok {
					acc.TxIDs = append(acc.TxIDs, s)
				}
			}
		}

		if ref, ok := entryMap["r"].(string); ok && ref != "" {
			acc.StumpRefs = append(acc.StumpRefs, ref)
		}
	}

	return result, nil
}
