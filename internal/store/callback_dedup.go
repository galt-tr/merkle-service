package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	as "github.com/aerospike/aerospike-client-go/v7"
)

const (
	dedupMarkerBin = "d"
)

// CallbackDedupStore tracks whether a specific txid/callbackURL/statusType
// combination has been successfully delivered, preventing duplicate callbacks.
type CallbackDedupStore struct {
	client      *AerospikeClient
	setName     string
	logger      *slog.Logger
	maxRetries  int
	retryBaseMs int
}

func NewCallbackDedupStore(client *AerospikeClient, setName string, maxRetries int, retryBaseMs int, logger *slog.Logger) *CallbackDedupStore {
	return &CallbackDedupStore{
		client:      client,
		setName:     setName,
		logger:      logger,
		maxRetries:  maxRetries,
		retryBaseMs: retryBaseMs,
	}
}

// dedupKey builds a deterministic Aerospike key from the callback parameters.
// Uses SHA-256 to keep key size bounded regardless of callbackURL length.
func dedupKey(txid, callbackURL, statusType string) string {
	h := sha256.Sum256([]byte(txid + ":" + callbackURL + ":" + statusType))
	return hex.EncodeToString(h[:])
}

// Exists checks if a callback delivery has already been recorded.
func (s *CallbackDedupStore) Exists(txid, callbackURL, statusType string) (bool, error) {
	keyStr := dedupKey(txid, callbackURL, statusType)
	key, err := as.NewKey(s.client.Namespace(), s.setName, keyStr)
	if err != nil {
		return false, fmt.Errorf("failed to create dedup key: %w", err)
	}

	exists, err := s.client.Client().Exists(nil, key)
	if err != nil {
		return false, fmt.Errorf("failed to check dedup record: %w", err)
	}
	return exists, nil
}

// Record marks a callback delivery as completed with a TTL.
func (s *CallbackDedupStore) Record(txid, callbackURL, statusType string, ttl time.Duration) error {
	keyStr := dedupKey(txid, callbackURL, statusType)
	key, err := as.NewKey(s.client.Namespace(), s.setName, keyStr)
	if err != nil {
		return fmt.Errorf("failed to create dedup key: %w", err)
	}

	wp := s.client.WritePolicy(s.maxRetries, s.retryBaseMs)
	wp.Expiration = uint32(ttl.Seconds())

	bins := as.BinMap{dedupMarkerBin: 1}
	if err := s.client.Client().Put(wp, key, bins); err != nil {
		return fmt.Errorf("failed to record dedup: %w", err)
	}
	return nil
}
