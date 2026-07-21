package store

import (
	"io"
	"time"
)

// RegistrationStore maps a txid to the set of callback URLs registered for it.
// Add is set-insert: duplicate (txid, url) pairs are silently deduplicated.
type RegistrationStore interface {
	Add(txid string, callbackURL string) error
	Get(txid string) ([]string, error)
	BatchGet(txids []string) (map[string][]string, error)
	UpdateTTL(txid string, ttl time.Duration) error
	BatchUpdateTTL(txids []string, ttl time.Duration) error
}

// StumpStore provides content-addressed STUMP payload storage with
// delete-at-height pruning. Put returns a ref that Get/Delete resolve.
type StumpStore interface {
	Put(data []byte, blockHeight uint64) (string, error)
	Get(ref string) ([]byte, error)
	Delete(ref string) error
}

// SubtreeStore provides subtree payload storage with delete-at-height pruning.
type SubtreeStore interface {
	StoreSubtree(id string, data []byte, blockHeight uint64) error
	StoreSubtreeFromReader(id string, r io.Reader, size int64, blockHeight uint64) error
	GetSubtree(id string) ([]byte, error)
	GetSubtreeReader(id string) (io.ReadCloser, error)
	DeleteSubtree(id string) error
	SetCurrentBlockHeight(height uint64)
}

// CallbackDedupStore tracks whether a (txid, url, statusType) combination has
// already been delivered so retries don't double-fire callbacks.
type CallbackDedupStore interface {
	Exists(txid, callbackURL, statusType string) (bool, error)
	Record(txid, callbackURL, statusType string, ttl time.Duration) error
}

// CallbackURLRegistry enumerates every known callback URL. Add is set-insert.
type CallbackURLRegistry interface {
	Add(callbackURL string) error
	GetAll() ([]string, error)
}

// CallbackAccumulatorStore aggregates per-block, per-URL callback data across
// subtrees, then hands it off atomically for dispatch via ReadAndDelete.
type CallbackAccumulatorStore interface {
	Append(blockHash, callbackURL string, txids []string, subtreeIndex int, stumpData []byte) error
	ReadAndDelete(blockHash string) (map[string]*AccumulatedCallback, error)
}

// SeenCounterStore tracks weighted confidence that multiple mining nodes have
// seen a txid. AddPeer records each peerID at most once per txid, adding the
// peer's current node weight to the score. ThresholdReached fires exactly once
// when the score first reaches the configured threshold.
//
// BatchAddPeer is the hot-path API: one subtree may contain tens or hundreds of
// thousands of registered txids. Implementations MUST batch store I/O (Aerospike
// BatchOperate / multi-row SQL) rather than one RTT per txid.
//
// The former Increment(txid, subtreeID) unique-subtree counter is deprecated
// in favour of peer-weighted scoring against the node registry window.
type SeenCounterStore interface {
	AddPeer(txid, peerID string, weight int) (*IncrementResult, error)
	// BatchAddPeer applies peerID/weight to each txid once. Returns the subset
	// of txids for which ThresholdReached became true on this call (fire-once).
	BatchAddPeer(txids []string, peerID string, weight int) (thresholdReached []string, err error)
	Threshold() int
}

// SubtreeAttributionStore records the first peer to announce each subtree hash.
// Later announcements of the same hash are discarded for scoring and processing.
type SubtreeAttributionStore interface {
	// TryAttribute returns the stored peer for subtreeHash. first is true only
	// when this call won the first-seen race and inserted peerID.
	TryAttribute(subtreeHash, peerID string) (attributedPeer string, first bool, err error)
}

// BlockAttributionStore persists first-seen block→peer attributions for the
// node registry (shared across k8s replicas).
type BlockAttributionStore interface {
	TryAttribute(hash, prevHash, peerID string, height uint32) (attributedPeer string, first bool, err error)
	ListAll() ([]BlockAttribution, error)
	DeleteHashes(hashes []string) error
}

// BlockAttribution is a persisted first-seen block announcement.
type BlockAttribution struct {
	Hash     string
	PrevHash string
	Height   uint32
	PeerID   string
}

// SubtreeCounterStore coordinates BLOCK_PROCESSED emission: Init sets the
// expected subtree count for a block, Decrement atomically counts one subtree
// as done and returns the remaining count (caller fires BLOCK_PROCESSED at 0).
type SubtreeCounterStore interface {
	Init(blockHash string, count int) error
	Decrement(blockHash string) (remaining int, err error)
}

// BackendHealth reports whether the underlying backend (Aerospike cluster, SQL
// connection pool) is reachable. Used by the API /health endpoint.
type BackendHealth interface {
	Healthy() bool
}
