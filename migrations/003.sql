-- Schema 003: let a scan epoch remember, across runs, which directories it still
-- has to prove.
--
-- Why a migration instead of a code change: `scan_queue.status` only allowed
-- pending/active/done, so a directory that could not be read had to be recorded
-- as 'done'. The incompleteness existed only in the in-memory Stats of that one
-- run, and the next run started with a clean slate:
--
--   round 1: A unreadable -> done;  B out of budget -> pending
--   round 2: only B is resumed; B finishes -> pending=0 -> epoch complete
--            -> every observation under A that this epoch never saw is retired
--               as stale, even though A was never read
--
-- 'failed' makes the queue carry that fact instead of the process. A failed
-- directory keeps the epoch open, is retried in a later round, and only a
-- successful pass over it allows the epoch to complete.
--
-- The other two columns answer the second half of R11. `claim_seq` orders claims
-- by least-recently-claimed, so one very large directory that exhausts the budget
-- every round no longer starves the directories behind it in path order; a NULL
-- sequence means "claimed but never really opened", which is exactly the rows a
-- claim ahead of the interruption left behind, and those sort first. A wall-clock
-- column cannot do this job: every directory in one claim batch shares a
-- timestamp, and ties fall back to path order — the very order that starved them.
-- `claimed_at_ns` stays as the diagnostic record of when a directory was last
-- attempted. `attempts` counts how many rounds a directory has been claimed, which
-- is what turns "this failed once" into "this is not progressing".
--
-- scopes.scan_interrupts is the consecutive-unfinished-round counter. It is reset
-- by a completed epoch, so "the last full pass did not finish" survives the run
-- that observed it.

ALTER TABLE scan_queue RENAME TO scan_queue_002;

CREATE TABLE scan_queue (
    scope_id TEXT NOT NULL REFERENCES scopes(scope_id),
    scan_epoch INTEGER NOT NULL,
    raw_relative_dir BLOB NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('pending','active','done','failed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
    claim_seq INTEGER,
    claimed_at_ns INTEGER,
    last_error TEXT,
    PRIMARY KEY(scope_id, scan_epoch, raw_relative_dir)
) STRICT;

-- Active rows from an interrupted run are pending work, not done work.
INSERT INTO scan_queue(scope_id, scan_epoch, raw_relative_dir, status, attempts)
SELECT scope_id, scan_epoch, raw_relative_dir,
       CASE status WHEN 'done' THEN 'done' ELSE 'pending' END,
       0
FROM scan_queue_002;

DROP TABLE scan_queue_002;

ALTER TABLE scopes ADD COLUMN scan_interrupts INTEGER NOT NULL DEFAULT 0;
