package store

import (
	"fmt"
	"log/slog"

	as "github.com/aerospike/aerospike-client-go/v7"
	astypes "github.com/aerospike/aerospike-client-go/v7/types"
)

const (
	seenPeersBin       = "peers" // CDT map peerID → weight
	seenScoreBin       = "score" // optional cache (not required for hot path)
	seenThresholdFired = "tfired"
)

// aerospikeSeenCounter is the Aerospike-backed SeenCounterStore implementation.
//
// Hot-path contract (1M TPS class):
//   - BatchAddPeer uses BatchOperate — O(1) RTTs per chunk of txids, not O(N).
//   - Score is derived from the peer map in-process (≈5 entries); no extra Get.
//   - threshold_fired is marked only for the rare fire-once subset.
type aerospikeSeenCounter struct {
	client      *AerospikeClient
	setName     string
	threshold   int
	logger      *slog.Logger
	maxRetries  int
	retryBaseMs int
}

var _ SeenCounterStore = (*aerospikeSeenCounter)(nil)

func NewSeenCounterStore(client *AerospikeClient, setName string, threshold int, maxRetries int, retryBaseMs int, logger *slog.Logger) SeenCounterStore {
	return &aerospikeSeenCounter{
		client:      client,
		setName:     setName,
		threshold:   threshold,
		logger:      logger,
		maxRetries:  maxRetries,
		retryBaseMs: retryBaseMs,
	}
}

// AddPeer records peerID with the given weight if not already present.
func (s *aerospikeSeenCounter) AddPeer(txid, peerID string, weight int) (*IncrementResult, error) {
	if weight <= 0 {
		return &IncrementResult{NewCount: 0, ThresholdReached: false}, nil
	}
	fired, err := s.BatchAddPeer([]string{txid}, peerID, weight)
	if err != nil {
		return nil, err
	}
	if len(fired) == 1 && fired[0] == txid {
		return &IncrementResult{NewCount: s.threshold, ThresholdReached: true}, nil
	}
	return &IncrementResult{NewCount: 0, ThresholdReached: false}, nil
}

// BatchAddPeer applies peer/weight to many txids with Aerospike BatchOperate.
func (s *aerospikeSeenCounter) BatchAddPeer(txids []string, peerID string, weight int) ([]string, error) {
	if weight <= 0 || len(txids) == 0 || peerID == "" {
		return nil, nil
	}

	const batchChunk = 5000
	var fired []string
	for i := 0; i < len(txids); i += batchChunk {
		end := i + batchChunk
		if end > len(txids) {
			end = len(txids)
		}
		chunkFired, err := s.batchChunk(txids[i:end], peerID, weight)
		if err != nil {
			return fired, err
		}
		fired = append(fired, chunkFired...)
	}
	return fired, nil
}

func (s *aerospikeSeenCounter) batchChunk(txids []string, peerID string, weight int) ([]string, error) {
	mapPolicy := as.NewMapPolicyWithFlags(as.MapOrder.UNORDERED, as.MapWriteFlagsCreateOnly|as.MapWriteFlagsNoFail)
	bp := s.client.BatchPolicy(s.maxRetries, s.retryBaseMs)
	bwp := as.NewBatchWritePolicy()
	bwp.RecordExistsAction = as.UPDATE

	records := make([]as.BatchRecordIfc, 0, len(txids))
	keys := make([]*as.Key, len(txids))
	for i, txid := range txids {
		key, err := as.NewKey(s.client.Namespace(), s.setName, txid)
		if err != nil {
			return nil, fmt.Errorf("key for %s: %w", txid, err)
		}
		keys[i] = key
		ops := []*as.Operation{
			as.MapPutOp(mapPolicy, seenPeersBin, peerID, weight),
			as.GetBinOp(seenPeersBin),
			as.GetBinOp(seenThresholdFired),
		}
		records = append(records, as.NewBatchWrite(bwp, key, ops...))
	}

	if err := s.client.Client().BatchOperate(bp, records); err != nil {
		// Partial results may still be present; log and inspect records.
		s.logger.Warn("batch seen counter operate had errors", "error", err, "n", len(txids))
	}

	var fired []string
	var toMark []*as.Key
	for i, recIfc := range records {
		bw, ok := recIfc.(*as.BatchWrite)
		if !ok {
			continue
		}
		if bw.ResultCode != astypes.OK && bw.ResultCode != 0 {
			continue
		}
		if bw.Record == nil {
			continue
		}
		score := sumMapValues(extractPeersBin(bw.Record.Bins[seenPeersBin]))
		alreadyFired := false
		if fv := bw.Record.Bins[seenThresholdFired]; fv != nil {
			if v, ok := fv.(int); ok && v == 1 {
				alreadyFired = true
			}
		}
		if score >= s.threshold && !alreadyFired {
			fired = append(fired, txids[i])
			toMark = append(toMark, keys[i])
		}
	}

	if len(toMark) > 0 {
		markRecs := make([]as.BatchRecordIfc, 0, len(toMark))
		for _, key := range toMark {
			markRecs = append(markRecs, as.NewBatchWrite(bwp, key, as.PutOp(as.NewBin(seenThresholdFired, 1))))
		}
		if err := s.client.Client().BatchOperate(bp, markRecs); err != nil {
			s.logger.Warn("batch mark threshold_fired failed", "error", err, "n", len(toMark))
		}
	}

	return fired, nil
}

// extractPeersBin handles multi-op results on the same bin (MapPut + GetBin),
// which Aerospike may return as []interface{}{mapSize, mapValue}.
func extractPeersBin(v interface{}) interface{} {
	switch t := v.(type) {
	case []interface{}:
		// Prefer the last map-looking element.
		for i := len(t) - 1; i >= 0; i-- {
			switch t[i].(type) {
			case map[interface{}]interface{}, map[string]interface{}:
				return t[i]
			}
		}
		return nil
	default:
		return v
	}
}

// Threshold returns the configured score threshold.
func (s *aerospikeSeenCounter) Threshold() int {
	return s.threshold
}

func sumMapValues(v interface{}) int {
	score := 0
	switch m := v.(type) {
	case map[interface{}]interface{}:
		for _, x := range m {
			score += asInt(x)
		}
	case map[string]interface{}:
		for _, x := range m {
			score += asInt(x)
		}
	}
	return score
}

func asInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		return int(n)
	default:
		return 0
	}
}

// Ensure score bin const is referenced for future use / docs.
var _ = seenScoreBin
