-- Schema 002: separate "parked, still reconcilable" from "held, terminal".
--
-- Why a migration instead of a code change: ParkIntent used to write a terminal
-- `hold` event while keeping the pending marker. That made two independent
-- requirements contradict each other:
--
--   * one_terminal_per_op allows only one event with phase in
--     (complete, abort, hold) per operation;
--   * finishing the operation writes phase=complete for the same operation.
--
-- So an object that really did move, whose fsync failed once, could never be
-- completed: the completion UPDATE hit zero rows because the item was 'unknown',
-- and even a relaxed state gate collided with the existing hold event. The item
-- then lost its pending marker and restore refused it, leaving a file in the
-- vault with no product path back.
--
-- The fix is a non-terminal phase. A parked operation is an observation that
-- still needs reconciling, not a verdict. `hold` stays terminal and keeps its
-- unique index.
--
-- Old rows are not rewritten. Items already stuck in the dead end are handled by
-- `recover adopt`, which opens a *new* operation with a full contract and keeps
-- the historical event attached to the old operation id.

ALTER TABLE events RENAME TO events_001;

CREATE TABLE events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE,
    op_id TEXT,
    item_id TEXT REFERENCES items(item_id),
    execution_plan_id TEXT REFERENCES plans(plan_id),
    action TEXT NOT NULL,
    phase TEXT NOT NULL CHECK(phase IN
        ('intent','complete','abort','hold','park','info')),
    occurred_at_ns INTEGER NOT NULL,
    contract_json TEXT NOT NULL,
    result_json TEXT
) STRICT;

INSERT INTO events(sequence, event_id, op_id, item_id, execution_plan_id, action,
                   phase, occurred_at_ns, contract_json, result_json)
SELECT sequence, event_id, op_id, item_id, execution_plan_id, action,
       phase, occurred_at_ns, contract_json, result_json
FROM events_001;

DROP TABLE events_001;

CREATE UNIQUE INDEX one_intent_per_op
    ON events(op_id) WHERE op_id IS NOT NULL AND phase='intent';
CREATE UNIQUE INDEX one_terminal_per_op
    ON events(op_id) WHERE op_id IS NOT NULL
    AND phase IN ('complete','abort','hold');
CREATE INDEX events_by_item
    ON events(item_id, sequence);

-- The due-for-purge queue is read with a keyset cursor so that a page full of
-- objects that cannot be processed automatically does not starve the objects
-- behind them.
CREATE INDEX items_by_purge_cursor
    ON items(scope_id, state, purge_not_before_ns, item_id);

INSERT INTO meta(key, value)
VALUES('schema_002', 'park is a non-terminal phase; see docs/architecture.md section 6.4')
ON CONFLICT(key) DO UPDATE SET value=excluded.value;
