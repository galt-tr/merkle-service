-- Incremental score column for O(1) score updates on peer insert (avoids SUM per call).
-- Used by the peer-weighted SEEN_MULTIPLE_NODES hot path.

-- Postgres and SQLite both support ADD COLUMN IF NOT EXISTS on modern versions;
-- SQLite 3.35+ has IF NOT EXISTS for columns; for older SQLite the migrate
-- runner may re-run safely only if we guard. Use plain ADD COLUMN and ignore
-- "duplicate column" at application level is not available, so use a portable
-- approach: only add if missing is hard in pure SQL across dialects.
-- We use a simple ADD COLUMN; migrations run once via schema_migrations.

ALTER TABLE seen_counters ADD COLUMN score INTEGER NOT NULL DEFAULT 0;
