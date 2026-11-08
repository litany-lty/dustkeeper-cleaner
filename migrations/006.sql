-- 006: a person's reconciliation of a prepared review that never settled.
--
-- 005 records the moment a request was prepared, before it was sent. A process
-- that died between sending the first request and recording its answer leaves a
-- row in state 'pending' whose cost_status/attempts are only preparation-time
-- facts: they cannot prove the supplier never received or billed the request.
--
-- This table is the manual, append-only answer to that gap. It does NOT edit the
-- original row: a human resolution must never be disguised as a model 'settled'
-- or a local 'refused', and the original cost_status/attempts/usage stay exactly
-- as they were prepared. One row per dispatch_id (the primary key) is the whole
-- idempotence rule: a second confirmation cannot overwrite the first record.
--
-- A pending dispatch with a row here is no longer "unresolved", so the next run
-- may ask the question again - through the normal gates, with a brand new
-- request/review id. Other pending dispatches, other actions and review_unknown
-- rows keep their own independent hold.
--
-- The foreign key is deliberate: a conclusion can only name a real prepared
-- request. Additive on purpose - upgrading a 3/4/5 database only creates this
-- table. No business row is rebuilt, renamed or rewritten.

CREATE TABLE review_dispatch_resolution (
    dispatch_id    TEXT PRIMARY KEY REFERENCES review_dispatch(dispatch_id),
    resolved_ns    INTEGER NOT NULL,
    request_status TEXT NOT NULL CHECK(request_status IN ('not_sent','completed','failed')),
    billing_status TEXT NOT NULL CHECK(billing_status IN ('not_charged','charged')),
    note           TEXT NOT NULL CHECK(length(note) > 0)
) STRICT;
