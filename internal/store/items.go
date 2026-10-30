package store

import (
	"context"
	"database/sql"
	"strings"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Item is one planned object and, after execution, one quarantine object.
// Keeping both in one row avoids maintaining two identities that can drift.
type Item struct {
	ID                       domain.ItemID
	PlanID                   domain.PlanID
	Ordinal                  int
	ScopeID                  domain.ScopeID
	RawRelative              []byte
	ObjectName               string
	Reason                   string
	SourceKind               string
	BindingJSON              string
	SourceParentIdentityJSON string
	PreIdentityJSON          string
	PreTokenJSON             string
	CapturedIdentityJSON     *string
	CapturedTokenJSON        *string
	BoundRetentionSeconds    int64
	ContentHash              *string
	MetadataHash             *string
	KeeperJSON               *string
	KeeperScopeID            *domain.ScopeID
	KeeperRawRelative        []byte
	KeeperPinActive          bool
	AutoPurgeGrantID         *domain.GrantID
	State                    string
	PendingOpID              *domain.OperationID
	QuarantinedAtNS          *int64
	PurgeNotBeforeNS         *int64
	HoldCode                 *string
}

var itemColumnNames = []string{
	"item_id", "plan_id", "ordinal", "scope_id", "raw_relative", "object_name",
	"reason", "source_kind", "binding_json", "source_parent_identity_json",
	"pre_identity_json", "pre_token_json", "captured_identity_json",
	"captured_token_json", "bound_retention_seconds", "content_hash",
	"metadata_hash", "keeper_json", "keeper_scope_id", "keeper_raw_relative",
	"keeper_pin_active", "auto_purge_grant_id", "state", "pending_op_id",
	"quarantined_at_ns", "purge_not_before_ns", "hold_code",
}

func prefixedItemColumns(prefix string) string {
	out := make([]string, len(itemColumnNames))
	for i, c := range itemColumnNames {
		out[i] = prefix + "." + c
	}
	return strings.Join(out, ", ")
}

// scanItemInto reads one row into it. The column order must stay in sync with
// itemColumnNames; a mismatch is caught by the integration tests, which read
// back every field they wrote.
func scanItemInto(rows *sql.Rows, it *Item) error {
	var (
		id, plan, scope string
		keeperScope     sql.NullString
		grantID         sql.NullString
		pendingOp       sql.NullString
		holdCode        sql.NullString
		capturedID      sql.NullString
		capturedTok     sql.NullString
		contentHash     sql.NullString
		metaHash        sql.NullString
		keeperJSON      sql.NullString
		quarantinedAt   sql.NullInt64
		purgeNotBefore  sql.NullInt64
	)
	err := rows.Scan(
		&id, &plan, &it.Ordinal, &scope, &it.RawRelative, &it.ObjectName,
		&it.Reason, &it.SourceKind, &it.BindingJSON, &it.SourceParentIdentityJSON,
		&it.PreIdentityJSON, &it.PreTokenJSON, &capturedID,
		&capturedTok, &it.BoundRetentionSeconds, &contentHash,
		&metaHash, &keeperJSON, &keeperScope, &it.KeeperRawRelative,
		&it.KeeperPinActive, &grantID, &it.State, &pendingOp,
		&quarantinedAt, &purgeNotBefore, &holdCode,
	)
	if err != nil {
		return err
	}
	it.ID = domain.ItemID(id)
	it.PlanID = domain.PlanID(plan)
	it.ScopeID = domain.ScopeID(scope)
	it.CapturedIdentityJSON = nullStr(capturedID)
	it.CapturedTokenJSON = nullStr(capturedTok)
	it.ContentHash = nullStr(contentHash)
	it.MetadataHash = nullStr(metaHash)
	it.KeeperJSON = nullStr(keeperJSON)
	it.HoldCode = nullStr(holdCode)
	if keeperScope.Valid {
		v := domain.ScopeID(keeperScope.String)
		it.KeeperScopeID = &v
	}
	if grantID.Valid {
		v := domain.GrantID(grantID.String)
		it.AutoPurgeGrantID = &v
	}
	if pendingOp.Valid {
		v := domain.OperationID(pendingOp.String)
		it.PendingOpID = &v
	}
	if quarantinedAt.Valid {
		v := quarantinedAt.Int64
		it.QuarantinedAtNS = &v
	}
	if purgeNotBefore.Valid {
		v := purgeNotBefore.Int64
		it.PurgeNotBeforeNS = &v
	}
	return nil
}

func nullStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func insertItemTx(ctx context.Context, tx *sql.Tx, it Item) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO items(item_id, plan_id, ordinal, scope_id, raw_relative, object_name,
                  reason, source_kind, binding_json, source_parent_identity_json,
                  pre_identity_json, pre_token_json, captured_identity_json,
                  captured_token_json, bound_retention_seconds, content_hash,
                  metadata_hash, keeper_json, keeper_scope_id, keeper_raw_relative,
                  keeper_pin_active, auto_purge_grant_id, state, pending_op_id,
                  quarantined_at_ns, purge_not_before_ns, hold_code)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(it.ID), string(it.PlanID), it.Ordinal, string(it.ScopeID), it.RawRelative,
		it.ObjectName, it.Reason, it.SourceKind, it.BindingJSON, it.SourceParentIdentityJSON,
		it.PreIdentityJSON, it.PreTokenJSON, it.CapturedIdentityJSON, it.CapturedTokenJSON,
		it.BoundRetentionSeconds, it.ContentHash, it.MetadataHash, it.KeeperJSON,
		it.KeeperScopeID, it.KeeperRawRelative, boolToInt(it.KeeperPinActive),
		it.AutoPurgeGrantID, it.State, it.PendingOpID, it.QuarantinedAtNS,
		it.PurgeNotBeforeNS, it.HoldCode)
	if err != nil {
		return errs.Newf(errs.CodeStateWriteFailed, "insert item %s: %v", it.ID, err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetItem loads one item.
func (s *Store) GetItem(ctx context.Context, id domain.ItemID) (Item, error) {
	rows, err := s.read(ctx, "SELECT "+prefixedItemColumns("i")+" FROM items i WHERE i.item_id=?", string(id))
	if err != nil {
		return Item{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Item{}, errs.Newf(errs.CodeItemNotFound, "item %q does not exist", id)
	}
	var it Item
	if err := scanItemInto(rows, &it); err != nil {
		return Item{}, errs.Wrap(errs.CodeDBError, err)
	}
	return it, nil
}

// ListItemsByPlan returns the items of a plan in contract order.
func (s *Store) ListItemsByPlan(ctx context.Context, planID domain.PlanID) ([]Item, error) {
	rows, err := s.read(ctx, "SELECT "+prefixedItemColumns("i")+
		" FROM items i WHERE i.plan_id=? ORDER BY i.ordinal", string(planID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := scanItemInto(rows, &it); err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, it)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// ListItemsByState returns items of a scope in one lifecycle state.
func (s *Store) ListItemsByState(ctx context.Context, scope domain.ScopeID, state string, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.read(ctx, "SELECT "+prefixedItemColumns("i")+`
 FROM items i WHERE i.scope_id=? AND i.state=? ORDER BY i.ordinal LIMIT ?`,
		string(scope), state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := scanItemInto(rows, &it); err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, it)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// purgeQueueWhere is the one definition of "in the automatic purge queue": the
// retention bound has elapsed and no operation is in flight.
//
// The page and the count both read this, because a report that answers "how many
// are due" from a different filter than the one a run walks is the same defect as
// a wrong number. Note what it does not say: being due is not permission to
// delete. The executor still checks the bound grant, the protections and the
// object's identity at the moment it acts, so this count is a backlog size, not
// an authorisation.
const purgeQueueWhere = `
 WHERE i.scope_id=? AND i.state='quarantined' AND i.pending_op_id IS NULL
   AND i.reason='lifecycle'
   AND i.purge_not_before_ns IS NOT NULL AND i.purge_not_before_ns <= ?`

// ItemsDueForPurge lists quarantined objects whose retention has elapsed and
// which have no operation in progress.
//
// The limit is a page size for callers that are about to act. A caller that only
// wants to know how many are due must use CountDueForPurge: asking this for one
// row and reporting len() answers "one" for every backlog of one or more.
func (s *Store) ItemsDueForPurge(ctx context.Context, scope domain.ScopeID, nowNS int64, limit int) ([]Item, error) {
	return s.PurgeQueuePage(ctx, scope, nowNS, 0, "", limit)
}

// CountDueForPurge reports how many objects are in the automatic purge queue.
func (s *Store) CountDueForPurge(ctx context.Context, scope domain.ScopeID, nowNS int64) (int64, error) {
	var n int64
	err := s.queryRow(ctx, "SELECT COUNT(*) FROM items i"+purgeQueueWhere, string(scope), nowNS).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(errs.CodeDBError, err)
	}
	return n, nil
}

// PurgeQueuePage returns one page of due objects using a keyset cursor.
//
// Only lifecycle objects are listed. A historical duplicate is still readable
// and still restorable, but it is not a candidate for the automatic purge queue:
// the comparison that once authorised removing it no longer runs, so offering it
// to the queue would put a delete in front of an operator that nothing supports.
//
// Why a cursor instead of LIMIT alone: the caller cannot filter "has an active
// auto-purge grant" in SQL without duplicating the authority rules, so it used to
// read page one, find every object in it unprocessable, and give up. If the first
// page was always the same, the authorised objects behind it were never reached in
// any run. The cursor lets the caller walk the whole queue within one bounded run
// and stop only when the queue is exhausted or its own allowance is spent.
func (s *Store) PurgeQueuePage(ctx context.Context, scope domain.ScopeID, nowNS int64,
	afterDue int64, afterItem string, limit int) ([]Item, error) {

	if limit <= 0 {
		limit = 100
	}
	rows, err := s.read(ctx, "SELECT "+prefixedItemColumns("i")+" FROM items i"+purgeQueueWhere+`
   AND (i.purge_not_before_ns > ? OR (i.purge_not_before_ns = ? AND i.item_id > ?))
 ORDER BY i.purge_not_before_ns, i.item_id LIMIT ?`,
		string(scope), nowNS, afterDue, afterDue, afterItem, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := scanItemInto(rows, &it); err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, it)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// CountItemsByState reports how many items of a scope are in a state.
func (s *Store) CountItemsByState(ctx context.Context, scope domain.ScopeID, state string) (int64, error) {
	var n int64
	err := s.queryRow(ctx,
		"SELECT COUNT(*) FROM items WHERE scope_id=? AND state=?", string(scope), state).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(errs.CodeDBError, err)
	}
	return n, nil
}

// KeeperInUse reports whether any live object still depends on the given keeper
// path. A keeper with an active dependency must not be quarantined.
func (s *Store) KeeperInUse(ctx context.Context, scope domain.ScopeID, rel []byte) (bool, error) {
	var n int64
	err := s.queryRow(ctx, `
SELECT COUNT(*) FROM items
WHERE keeper_scope_id=? AND keeper_raw_relative=? AND keeper_pin_active=1
  AND state IN ('q_intent','quarantined','r_intent','p_intent')`,
		string(scope), rel).Scan(&n)
	if err != nil {
		return false, errs.Wrap(errs.CodeDBError, err)
	}
	return n > 0, nil
}

// ReleaseKeeperDependents drops the pin for a keeper once every object that
// depended on it has been restored or purged.
func (s *Store) ReleaseKeeperDependents(ctx context.Context, scope domain.ScopeID, rel []byte) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE items SET keeper_pin_active=0
WHERE keeper_scope_id=? AND keeper_raw_relative=? AND keeper_pin_active=1`,
			string(scope), rel)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// ScopeHasUnresolved reports whether a scope still holds work that no automatic
// pass may route around.
//
// Three conditions are the same question asked three ways: an operation is
// recorded but its outcome is not (pending_op_id), an object was frozen by a
// verdict (held, unknown), or an object sits between two states (q_intent,
// r_intent, p_intent) because a process stopped in the middle. Any of them means
// the account of that tree is incomplete, and starting a new quarantine or purge
// next to it would add another unexplained episode to the same scope.
//
// Restore and reconcile deliberately do not consult this: converging the
// unresolved state is the repair, and refusing to repair because a repair is
// needed would be a way to lose data.
func (s *Store) ScopeHasUnresolved(ctx context.Context, scope domain.ScopeID) (bool, error) {
	var n int64
	err := s.queryRow(ctx, `
SELECT COUNT(*) FROM items
WHERE scope_id=?
  AND (pending_op_id IS NOT NULL
       OR state IN ('held','unknown','q_intent','r_intent','p_intent'))`,
		string(scope)).Scan(&n)
	if err != nil {
		return false, errs.Wrap(errs.CodeDBError, err)
	}
	return n > 0, nil
}

// SetHoldCode records why an item is frozen.
func (s *Store) SetHoldCode(ctx context.Context, id domain.ItemID, code string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE items SET hold_code=? WHERE item_id=?", code, string(id))
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// ReleaseItemKeeperPin drops one dependent object's keeper dependency. It is
// called when that object is restored or purged, and affects only that item: a
// keeper may have several dependents and each releases its own pin.
func (s *Store) ReleaseItemKeeperPin(ctx context.Context, id domain.ItemID) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE items SET keeper_pin_active=0 WHERE item_id=? AND keeper_pin_active=1", string(id))
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}
