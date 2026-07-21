package sql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	storepkg "github.com/bsv-blockchain/merkle-service/internal/store"
)

type seenCounter struct {
	db        *sql.DB
	d         *dialect
	threshold int
}

var _ storepkg.SeenCounterStore = (*seenCounter)(nil)

func newSeenCounter(db *sql.DB, d *dialect, threshold int) *seenCounter {
	return &seenCounter{db: db, d: d, threshold: threshold}
}

func (s *seenCounter) Threshold() int { return s.threshold }

// AddPeer records peerID as a unique observer of txid.
// Incremental score update (no SUM over peers) — one short transaction.
func (s *seenCounter) AddPeer(txid, peerID string, weight int) (*storepkg.IncrementResult, error) {
	fired, err := s.BatchAddPeer([]string{txid}, peerID, weight)
	if err != nil {
		return nil, err
	}
	if len(fired) == 1 {
		return &storepkg.IncrementResult{NewCount: s.threshold, ThresholdReached: true}, nil
	}
	return &storepkg.IncrementResult{NewCount: 0, ThresholdReached: false}, nil
}

// BatchAddPeer applies the same peer/weight to many txids in one transaction.
// Hot path: tens–hundreds of thousands of registered txids per subtree.
func (s *seenCounter) BatchAddPeer(txids []string, peerID string, weight int) ([]string, error) {
	if weight <= 0 || len(txids) == 0 || peerID == "" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	qParent := fmt.Sprintf("INSERT INTO seen_counters (txid) VALUES (%s)%s",
		s.d.placeholder(1), s.d.onConflictDoNothing)
	qPeer := fmt.Sprintf(
		"INSERT INTO seen_counter_peers (txid, peer_id, weight) VALUES (%s, %s, %s)%s",
		s.d.placeholder(1), s.d.placeholder(2), s.d.placeholder(3), s.d.onConflictDoNothing,
	)
	qBump := fmt.Sprintf(
		"UPDATE seen_counters SET score = score + %s WHERE txid = %s",
		s.d.placeholder(1), s.d.placeholder(2),
	)
	var qRead string
	if isPostgres(s.d) {
		qRead = fmt.Sprintf(
			"SELECT score, threshold_fired FROM seen_counters WHERE txid = %s FOR UPDATE",
			s.d.placeholder(1),
		)
	} else {
		qRead = fmt.Sprintf(
			"SELECT score, threshold_fired FROM seen_counters WHERE txid = %s",
			s.d.placeholder(1),
		)
	}
	qFire := fmt.Sprintf(
		"UPDATE seen_counters SET threshold_fired = 1 WHERE txid = %s AND threshold_fired = 0 AND score >= %s",
		s.d.placeholder(1), s.d.placeholder(2),
	)

	var fired []string
	for _, txid := range txids {
		if _, err := tx.ExecContext(ctx, qParent, txid); err != nil {
			return nil, fmt.Errorf("insert seen_counters: %w", err)
		}
		res, err := tx.ExecContext(ctx, qPeer, txid, peerID, weight)
		if err != nil {
			return nil, fmt.Errorf("insert seen_counter_peers: %w", err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			// New peer for this txid — bump score by weight observed now.
			if _, err := tx.ExecContext(ctx, qBump, weight, txid); err != nil {
				return nil, fmt.Errorf("bump score: %w", err)
			}
		}

		var score, alreadyFired int
		if err := tx.QueryRowContext(ctx, qRead, txid).Scan(&score, &alreadyFired); err != nil {
			return nil, fmt.Errorf("read score: %w", err)
		}
		if alreadyFired == 0 && score >= s.threshold {
			fr, err := tx.ExecContext(ctx, qFire, txid, s.threshold)
			if err != nil {
				return nil, fmt.Errorf("set fired: %w", err)
			}
			if fn, _ := fr.RowsAffected(); fn > 0 {
				fired = append(fired, txid)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return fired, nil
}
