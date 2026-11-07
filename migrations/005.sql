-- 005: a request that is about to be sent, before it is sent.
--
-- The unresolved-request table (004) records a request whose answer never came
-- back. It cannot answer a different question: was a request prepared and then
-- abandoned because the process died before the journal was closed? Writing the
-- preparation first, and settling it together with the decision, makes that
-- question answerable. A row in state 'pending' at the start of a run is a fact
-- to investigate, not a request to repeat automatically.
--
-- Additive on purpose: a database left at schema 3 or 4 only needs these two
-- objects to reach 5. Nothing here rebuilds, renames or rewrites a business
-- table, so an upgrade cannot change a recorded identity, protection or grant.
--
-- review_id is the correlation key the result page and the operational log use
-- for one review of one item. run_id is the invocation that made the request.
-- No model prompt, response body, credential or file content is stored here.

CREATE TABLE review_dispatch (
    dispatch_id TEXT PRIMARY KEY,
    review_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    item_id TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    action TEXT NOT NULL,
    identity_json TEXT NOT NULL,
    token_json TEXT NOT NULL,
    budget_json TEXT NOT NULL,
    created_ns INTEGER NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('pending','settled','refused')),
    attempts INTEGER NOT NULL DEFAULT 0,
    failure TEXT NOT NULL DEFAULT '',
    cost_status TEXT NOT NULL DEFAULT 'not_sent',
    elapsed_ms INTEGER NOT NULL DEFAULT 0,
    http_status INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    settled_ns INTEGER,
    note TEXT
) STRICT;

CREATE UNIQUE INDEX one_open_dispatch_per_review
    ON review_dispatch(review_id) WHERE state = 'pending';
CREATE INDEX review_dispatch_by_item
    ON review_dispatch(item_id, action, created_ns);
CREATE INDEX review_dispatch_unsettled
    ON review_dispatch(state, created_ns);
