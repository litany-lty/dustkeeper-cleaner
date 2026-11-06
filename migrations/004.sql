-- 004: unresolved review requests.
--
-- A request that was sent and produced no answer is neither a decision nor a
-- failure to try again. It is a fact with two consequences: the file keeps
-- whatever state it had, and the cost of that request is unknown. The valid-keep
-- cache deliberately holds only decisions, so before this table the next run
-- asked the same question again and paid for it a second time.
--
-- Rows are per object and action, because "may this be deleted" and "may this be
-- moved" are different questions and only one of them was left unanswered.
-- Resolution is a person's explicit statement, recorded rather than inferred: a
-- new process, a restart or a re-scan does not clear it.
--
-- identity_json/token_json record the exact version the request was about. A
-- changed file makes the old row irrelevant for suppression, but it is never
-- deleted: the possibility that the earlier request was billed outlives the
-- file's current bytes.

CREATE TABLE review_unknown (
    item_id TEXT NOT NULL REFERENCES items(item_id),
    action TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    raw_relative_b64 TEXT NOT NULL,
    identity_json TEXT NOT NULL,
    token_json TEXT NOT NULL,
    created_ns INTEGER NOT NULL,
    attempts INTEGER NOT NULL CHECK(attempts > 0),
    failure TEXT NOT NULL,
    cost_status TEXT NOT NULL,
    resolved_ns INTEGER,
    resolution TEXT
) STRICT;

CREATE UNIQUE INDEX one_open_request_per_item_action
    ON review_unknown(item_id, action) WHERE resolved_ns IS NULL;
CREATE INDEX review_unknown_open
    ON review_unknown(resolved_ns, created_ns);
