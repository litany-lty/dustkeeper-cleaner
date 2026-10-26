-- Dustkeeper schema 001.
--
-- Shape follows section 8.2: relational columns for anything that is queried,
-- ordered or constrained; JSON only for immutable evidence snapshots. No single
-- cell ever holds a whole scan or a whole duplicate group.
--
-- All integer timestamps are UTC Unix nanoseconds. retention_seconds is a
-- separate unit and must never be mixed into these columns without conversion.

CREATE TABLE meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

CREATE TABLE scopes (
    scope_id TEXT PRIMARY KEY,
    raw_root BLOB NOT NULL,
    root_identity_json TEXT NOT NULL,
    vault_identity_json TEXT,
    instance_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN
        ('observe','assisted','managed_disposable','reference')),
    generation INTEGER NOT NULL CHECK(generation > 0),
    protection_revision INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL CHECK(status IN
        ('active','paused','needs_reapproval','held')),
    scan_epoch INTEGER NOT NULL DEFAULT 0,
    created_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE rules (
    rule_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision > 0),
    scope_id TEXT NOT NULL REFERENCES scopes(scope_id),
    dsl_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    review_binding_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('draft','active','disabled')),
    created_at_ns INTEGER NOT NULL,
    PRIMARY KEY(rule_id, revision)
) STRICT;

CREATE TABLE grants (
    grant_id TEXT PRIMARY KEY,
    scope_id TEXT NOT NULL REFERENCES scopes(scope_id),
    rule_id TEXT NOT NULL,
    rule_revision INTEGER NOT NULL,
    capability TEXT NOT NULL CHECK(capability IN ('auto_quarantine','auto_purge')),
    binding_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('active','revoked','needs_reapproval')),
    approved_at_ns INTEGER NOT NULL,
    FOREIGN KEY(rule_id, rule_revision) REFERENCES rules(rule_id, revision)
) STRICT;

CREATE TABLE observations (
    scope_id TEXT NOT NULL REFERENCES scopes(scope_id),
    raw_relative BLOB NOT NULL,
    identity_json TEXT NOT NULL,
    token_json TEXT NOT NULL,
    parent_identity_json TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    mtime_ns INTEGER NOT NULL,
    first_seen_ns INTEGER NOT NULL,
    last_seen_ns INTEGER NOT NULL,
    scan_epoch INTEGER NOT NULL,
    status TEXT NOT NULL,
    cached_hash TEXT,
    hash_token_json TEXT,
    PRIMARY KEY(scope_id, raw_relative)
) STRICT;

CREATE TABLE scan_queue (
    scope_id TEXT NOT NULL REFERENCES scopes(scope_id),
    scan_epoch INTEGER NOT NULL,
    raw_relative_dir BLOB NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('pending','active','done')),
    PRIMARY KEY(scope_id, scan_epoch, raw_relative_dir)
) STRICT;

CREATE TABLE plans (
    plan_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK(kind IN
        ('manual_rule','manual_duplicate','scheduled_rule',
         'manual_purge','scheduled_purge')),
    source_plan_id TEXT REFERENCES plans(plan_id),
    header_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN
        ('ready','applying','complete','partial','expired','blocked')),
    created_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE items (
    item_id TEXT PRIMARY KEY,
    plan_id TEXT NOT NULL REFERENCES plans(plan_id),
    ordinal INTEGER NOT NULL,
    scope_id TEXT NOT NULL REFERENCES scopes(scope_id),
    raw_relative BLOB NOT NULL,
    object_name TEXT NOT NULL UNIQUE,
    reason TEXT NOT NULL CHECK(reason IN ('lifecycle','exact_duplicate')),
    source_kind TEXT NOT NULL CHECK(source_kind IN
        ('manual_rule','manual_duplicate','scheduled_rule')),
    binding_json TEXT NOT NULL,
    source_parent_identity_json TEXT NOT NULL,
    pre_identity_json TEXT NOT NULL,
    pre_token_json TEXT NOT NULL,
    captured_identity_json TEXT,
    captured_token_json TEXT,
    bound_retention_seconds INTEGER NOT NULL
        CHECK(bound_retention_seconds BETWEEN 604800 AND 31536000),
    content_hash TEXT,
    metadata_hash TEXT,
    keeper_json TEXT,
    keeper_scope_id TEXT REFERENCES scopes(scope_id),
    keeper_raw_relative BLOB,
    keeper_pin_active INTEGER NOT NULL DEFAULT 0
        CHECK(keeper_pin_active IN (0,1)),
    auto_purge_grant_id TEXT REFERENCES grants(grant_id),
    state TEXT NOT NULL CHECK(state IN
        ('planned','q_intent','quarantined','r_intent','restored',
         'p_intent','purged','aborted','held','unknown')),
    pending_op_id TEXT,
    quarantined_at_ns INTEGER,
    purge_not_before_ns INTEGER,
    hold_code TEXT,
    UNIQUE(plan_id, ordinal)
) STRICT;

CREATE TABLE purge_members (
    plan_id TEXT NOT NULL REFERENCES plans(plan_id),
    ordinal INTEGER NOT NULL,
    item_id TEXT NOT NULL REFERENCES items(item_id),
    verification_json TEXT NOT NULL,
    PRIMARY KEY(plan_id, ordinal),
    UNIQUE(plan_id, item_id)
) STRICT;

CREATE TABLE protections (
    protection_id TEXT PRIMARY KEY,
    scope_id TEXT NOT NULL REFERENCES scopes(scope_id),
    raw_relative BLOB NOT NULL,
    match_kind TEXT NOT NULL CHECK(match_kind IN ('exact','subtree')),
    source TEXT NOT NULL CHECK(source IN
        ('user','restore','emergency_restore')),
    evidence_json TEXT NOT NULL,
    active INTEGER NOT NULL CHECK(active IN (0,1)),
    created_at_ns INTEGER NOT NULL,
    UNIQUE(scope_id, raw_relative, match_kind)
) STRICT;

CREATE TABLE events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE,
    op_id TEXT,
    item_id TEXT REFERENCES items(item_id),
    execution_plan_id TEXT REFERENCES plans(plan_id),
    action TEXT NOT NULL,
    phase TEXT NOT NULL CHECK(phase IN
        ('intent','complete','abort','hold','info')),
    occurred_at_ns INTEGER NOT NULL,
    contract_json TEXT NOT NULL,
    result_json TEXT
) STRICT;

CREATE UNIQUE INDEX one_intent_per_op
    ON events(op_id) WHERE op_id IS NOT NULL AND phase='intent';
CREATE UNIQUE INDEX one_terminal_per_op
    ON events(op_id) WHERE op_id IS NOT NULL
    AND phase IN ('complete','abort','hold');
CREATE UNIQUE INDEX one_active_rule_revision
    ON rules(rule_id) WHERE status='active';

CREATE INDEX observations_by_size
    ON observations(scope_id, size_bytes);
CREATE INDEX observations_by_age
    ON observations(scope_id, mtime_ns);
CREATE INDEX items_by_purge
    ON items(state, purge_not_before_ns);
CREATE INDEX keeper_pins
    ON items(keeper_scope_id, keeper_raw_relative, keeper_pin_active);
CREATE INDEX items_by_plan
    ON items(plan_id, ordinal);
CREATE INDEX events_by_item
    ON events(item_id, sequence);
CREATE INDEX plans_by_status
    ON plans(status, expires_at_ns);
CREATE INDEX scopes_by_status
    ON scopes(status);
