package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"

	"github.com/mattn/go-sqlite3"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// ReviewUnknown is one request that was sent and produced no answer.
//
// It is not a decision about the file and it is not a failure to retry: it is
// the record that a question was paid for and not answered. Until a person
// says what to do, the same object and action are not asked again.
type ReviewUnknown struct {
	ItemID       domain.ItemID `json:"item_id"`
	Action       string        `json:"action"`
	ScopeID      string        `json:"scope_id"`
	PathB64      string        `json:"path_b64,omitempty"`
	Path         string        `json:"path"`
	IdentityJSON string        `json:"-"`
	TokenJSON    string        `json:"-"`
	CreatedNS    int64         `json:"created_ns"`
	Attempts     int           `json:"attempts"`
	Failure      string        `json:"failure"`
	CostStatus   string        `json:"cost_status"`
	ResolvedNS   *int64        `json:"resolved_ns,omitempty"`
	Resolution   string        `json:"resolution,omitempty"`
}

// RecordReviewUnknown writes the unresolved request. A second unresolved row for
// the same object and action is refused rather than merged: two rows would make
// "how many requests are unreconciled" unanswerable.
func (s *Store) RecordReviewUnknown(ctx context.Context, u ReviewUnknown) error {
	if u.Attempts <= 0 {
		return errs.New(errs.CodeInvalidArgument, "an unresolved review must record at least one attempt")
	}
	if u.ItemID == "" || u.Action == "" || u.IdentityJSON == "" || u.TokenJSON == "" {
		return errs.New(errs.CodeInvalidArgument, "an unresolved review needs the object and action it was about")
	}
	if u.PathB64 == "" {
		return errs.New(errs.CodeInvalidArgument, "an unresolved review needs the path it was about")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO review_unknown(
			item_id, action, scope_id, raw_relative_b64, identity_json, token_json,
			created_ns, attempts, failure, cost_status)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			string(u.ItemID), u.Action, u.ScopeID, u.PathB64, u.IdentityJSON, u.TokenJSON,
			u.CreatedNS, u.Attempts, u.Failure, u.CostStatus)
		if err != nil {
			if isUniqueViolation(err) {
				return errs.New(errs.CodeStateWriteFailed,
					"this object already has an unresolved review request for this action")
			}
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return nil
	})
}

// OpenReviewUnknown returns the unresolved requests for this path and action.
//
// The lookup is by path rather than by item id on purpose: a later plan creates
// a new item for the same file, and keying on the item would let the next run
// pay for the same question again. The caller compares the recorded identity and
// token, which is what makes "the same evidence" precise.
func (s *Store) OpenReviewUnknown(ctx context.Context, scopeID, pathB64, action string) ([]ReviewUnknown, error) {
	rows, err := s.read(ctx, `SELECT item_id, action, scope_id, raw_relative_b64, identity_json,
		token_json, created_ns, attempts, failure, cost_status, resolved_ns, resolution
		FROM review_unknown WHERE scope_id=? AND raw_relative_b64=? AND action=? AND resolved_ns IS NULL
		ORDER BY created_ns DESC LIMIT 4`, scopeID, pathB64, action)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ReviewUnknown, 0, 2)
	for rows.Next() {
		u, err := scanReviewUnknown(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	return out, nil
}

// ListReviewUnknown returns the open requests, newest first, or every request
// when includeResolved is set.
func (s *Store) ListReviewUnknown(ctx context.Context, includeResolved bool, limit int) ([]ReviewUnknown, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT item_id, action, scope_id, raw_relative_b64, identity_json, token_json,
		created_ns, attempts, failure, cost_status, resolved_ns, resolution
		FROM review_unknown`
	if !includeResolved {
		q += " WHERE resolved_ns IS NULL"
	}
	q += " ORDER BY created_ns DESC, item_id LIMIT ?"
	rows, err := s.read(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ReviewUnknown, 0, 8)
	for rows.Next() {
		u, err := scanReviewUnknown(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	return out, nil
}

// ResolveReviewUnknown records that a person has dealt with the unresolved
// request, and that a new request for this object and action may be made.
func (s *Store) ResolveReviewUnknown(ctx context.Context, item domain.ItemID, action, note string, nowNS int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE review_unknown SET resolved_ns=?, resolution=?
			WHERE item_id=? AND action=? AND resolved_ns IS NULL`, nowNS, note, string(item), action)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errs.New(errs.CodeItemNotFound,
				"there is no unresolved review request for this object and action")
		}
		return nil
	})
}

func scanReviewUnknown(rows *sql.Rows) (ReviewUnknown, error) {
	var u ReviewUnknown
	var item, path string
	var resolved sql.NullInt64
	var resolution sql.NullString
	if err := rows.Scan(&item, &u.Action, &u.ScopeID, &path, &u.IdentityJSON, &u.TokenJSON,
		&u.CreatedNS, &u.Attempts, &u.Failure, &u.CostStatus, &resolved, &resolution); err != nil {
		return ReviewUnknown{}, errs.Wrap(errs.CodeDBError, err)
	}
	u.ItemID = domain.ItemID(item)
	u.PathB64 = path
	if raw, err := base64.StdEncoding.DecodeString(path); err == nil {
		u.Path = string(raw)
	} else {
		// Show what is stored rather than an empty column: an undecodable path
		// is itself a fact the person settling the row needs to see.
		u.Path = path
	}
	if resolved.Valid {
		v := resolved.Int64
		u.ResolvedNS = &v
	}
	u.Resolution = resolution.String
	return u, nil
}

// isUniqueViolation asks the driver, not the message: a text comparison would
// depend on the SQLite version's wording.
func isUniqueViolation(err error) bool {
	var se sqlite3.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.ExtendedCode == sqlite3.ErrConstraintUnique ||
		se.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
}
