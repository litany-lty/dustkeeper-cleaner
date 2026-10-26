package store

import (
	"context"
	"database/sql"
	"errors"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Clock state lives in `meta` because it describes the installation, not one
// scope. The three keys answer three different questions and must never be
// collapsed into one timestamp:
//
//	clock_latch_ns     an anomaly that has not been acknowledged
//	clock_latch_reason why, in words, so a later operator can judge it
//	clock_ack_ns       when a human said "the clock is fine now"
//
// A trusted run writes last_trusted_run_ns (in package clocktrust). An
// untrusted run writes the latch. Neither write can substitute for the other.

// ClockLatch returns the pending anomaly, if any.
func (s *Store) ClockLatch(ctx context.Context) (int64, string, error) {
	atStr, err := s.MetaGet(ctx, "clock_latch_ns")
	if err != nil {
		return 0, "", err
	}
	reason, err := s.MetaGet(ctx, "clock_latch_reason")
	if err != nil {
		return 0, "", err
	}
	var at int64
	if atStr != "" {
		at = parseMetaInt(atStr)
	}
	return at, reason, nil
}

// LatchClock records an unresolved clock anomaly.
//
// It never overwrites an earlier latch: the first anomaly is the one that
// explains why retention cannot be trusted, and a second anomaly during the same
// doubt does not make the first one less relevant.
func (s *Store) LatchClock(ctx context.Context, atNS int64, reason string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		existing, err := metaGetTx(ctx, tx, "clock_latch_ns")
		if err != nil {
			return err
		}
		if existing != "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO meta(key,value) VALUES('clock_latch_ns',?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			itoa64(atNS)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO meta(key,value) VALUES('clock_latch_reason',?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			reason); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return nil
	})
}

// ClearClockLatchAndExtend clears the latch and gives every object whose
// retention window cannot be proven a full new window.
//
// "Cannot be proven" has two shapes, and both are covered:
//
//   - the object was quarantined at or after the latch, so its recorded start
//     time came from a clock that was already suspect;
//   - the object is already past its due time at the moment of acknowledgement,
//     so clearing the latch alone would make it deletable immediately.
//
// The new deadline is never earlier than the stored one. An acknowledgement can
// delay a deletion; it can never bring one forward.
func (s *Store) ClearClockLatchAndExtend(ctx context.Context, ackNS, latchNS int64) (int, error) {
	extended := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT item_id, bound_retention_seconds, purge_not_before_ns
FROM items
WHERE state='quarantined'
  AND purge_not_before_ns IS NOT NULL
  AND quarantined_at_ns IS NOT NULL`)
		if err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		type candidate struct {
			id       string
			retained int64
			current  int64
		}
		var list []candidate
		for rows.Next() {
			var id string
			var retained, current int64
			if err := rows.Scan(&id, &retained, &current); err != nil {
				rows.Close()
				return errs.Wrap(errs.CodeDBError, err)
			}
			list = append(list, candidate{id: id, retained: retained, current: current})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return errs.Wrap(errs.CodeDBError, err)
		}
		rows.Close()

		for _, c := range list {
			next, ok := domain.AddSecondsNS(ackNS, c.retained)
			if !ok {
				return errs.Newf(errs.CodeHeld,
					"retention of item %s overflows; acknowledging the clock would produce an unusable deadline", c.id)
			}
			if next <= c.current {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE items SET purge_not_before_ns=? WHERE item_id=? AND state='quarantined'",
				next, c.id); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
			itemID := domain.ItemID(c.id)
			result := `{"source":"clock_ack","result":"retention window restarted at acknowledgement","previous_due_ns":` +
				itoa64(c.current) + `,"new_due_ns":` + itoa64(next) + `}`
			if err := appendEventTx(ctx, tx, Event{
				EventID:      domain.NewID("ev"),
				ItemID:       &itemID,
				Action:       "clock_ack",
				Phase:        domain.PhaseInfo,
				OccurredAtNS: ackNS,
				ContractJSON: `{"source":"doctor","reason":"clock acknowledged"}`,
				ResultJSON:   &result,
			}); err != nil {
				return err
			}
			extended++
		}

		if _, err := tx.ExecContext(ctx,
			"DELETE FROM meta WHERE key IN ('clock_latch_ns','clock_latch_reason')"); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO meta(key,value) VALUES('clock_ack_ns',?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value", itoa64(ackNS)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if latchNS > 0 {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO meta(key,value) VALUES('last_trusted_run_ns',?) "+
					"ON CONFLICT(key) DO UPDATE SET value=excluded.value", itoa64(ackNS)); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return extended, nil
}

func metaGetTx(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var v string
	err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errs.Wrap(errs.CodeDBError, err)
	}
	return v, nil
}

func parseMetaInt(s string) int64 {
	var v int64
	neg := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int64(c-'0')
	}
	if neg {
		return -v
	}
	return v
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	var buf [24]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
