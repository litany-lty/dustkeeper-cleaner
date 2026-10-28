// Package store owns the SQLite state database.
//
// Three rules from section 8.1 are structural here:
//
//   - one process, one dedicated write connection, one writer at a time;
//   - a business state change and its audit event commit in the same short
//     transaction;
//   - no hashing, byte comparison, network call or filesystem change ever runs
//     inside a transaction.
//
// Item state is private: callers cannot set it directly, they must use the
// intent / complete / abort / hold methods that also enforce the transition and
// the one-intent-per-operation indexes.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/internal/errs"
	"dustkeeper/migrations"
)

// Store is the state database handle.
type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
	now  func() int64
	// iterating counts open streaming queries. See refuseNested.
	iterating int32
}

// refuseNested turns the pool's one-connection design into an error instead of
// a hang.
//
// The pool has exactly one connection. A streaming query holds it until the last
// row is read, so any other query issued from inside the row callback waits for a
// connection that cannot be released before that callback returns. Nothing would
// time out and nothing would report: the process would simply stop, holding the
// state lock, in the middle of a plan.
//
// Callers must therefore not reach back into the store from a row callback. This
// makes that mistake diagnosable where it happens, with the operation named,
// instead of turning it into a hang that needs a debugger to explain. Callbacks
// that need the database should collect their input first.
func (s *Store) refuseNested(op string) error {
	if atomic.LoadInt32(&s.iterating) > 0 {
		return errs.Newf(errs.CodeDBError,
			"%s was requested from inside an open row iterator: the state database has one connection, "+
				"so a nested call would wait for a connection the iterator cannot release", op)
	}
	return nil
}

// enterIteration marks a streaming query as open and returns the release.
func (s *Store) enterIteration() func() {
	atomic.AddInt32(&s.iterating, 1)
	return func() { atomic.AddInt32(&s.iterating, -1) }
}

// rowScanner is what a single-row query returns. It exists so a refused nested
// query can be reported through the same Scan call the caller already writes,
// instead of needing a second error path at every site.
type rowScanner interface{ Scan(dest ...any) error }

// errRow is a row whose only outcome is an error.
type errRow struct{ err error }

func (e errRow) Scan(...any) error { return e.err }

// queryRow reads one row, refusing the call when a streaming iterator is open.
func (s *Store) queryRow(ctx context.Context, q string, args ...any) rowScanner {
	if err := s.refuseNested("a single-row query"); err != nil {
		return errRow{err: err}
	}
	return s.db.QueryRowContext(ctx, q, args...)
}

// Open opens or creates the database at path and applies the pragmas and
// migration the binary expects.
//
// An existing database is never migrated. The simplified scope is "this build
// reads the schema it writes": an older or newer schema is refused before the
// first write (CheckSupportedState), and the operator keeps the old binary and the
// old state until they have a verified copy to move forward from. Migration code
// still runs, but only for a database this call is creating.
func Open(path string) (*Store, error) {
	existing, err := databaseExists(path)
	if err != nil {
		return nil, err
	}
	if existing {
		// The pre-flight reads the header directly, so a file that is not ours is
		// refused without SQLite touching it.
		if err := CheckSupportedState(path); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}

	// Only connection-level options live in the DSN. Durability pragmas are set
	// explicitly below and read back, so there is exactly one source of truth.
	dsn := fmt.Sprintf("file:%s?_busy_timeout=1000&_txlock=immediate", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: path, now: func() int64 { return time.Now().UnixNano() }}

	if existing {
		// Opening an existing file is where SQLite rolls back a hot journal, so
		// the version is re-read after that recovery and before the pragmas. The
		// header check ran on the bytes that were on disk before it.
		if err := s.requireCurrentSchema(); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.applyPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.verifyPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	if !existing {
		if err := s.migrate(); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		// A permission fix-up failure is worth reporting but not fatal: the
		// database was created by the current user inside a 0700 directory.
		_ = err
	}
	return s, nil
}

// requireCurrentSchema re-reads the schema version of a database that already
// existed, after SQLite has had the chance to roll back a hot journal.
//
// It is deliberately narrower than the old migrate(): a version this build does
// not implement is a refusal with the version and the path in the message, not a
// migration.
func (s *Store) requireCurrentSchema() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	switch {
	case version > migrations.CurrentVersion:
		return errs.Newf(errs.CodeMigrationTooNew,
			"database schema is version %d, this binary supports %d; refusing to open",
			version, migrations.CurrentVersion)
	case version < migrations.CurrentVersion:
		return errs.Newf(errs.CodeMigrationTooOld,
			"%s is schema version %d, this binary writes %d and does not migrate in place; keep the previous binary and a copy of this state directory",
			s.path, version, migrations.CurrentVersion)
	}
	return nil
}

// applyPragmas sets the section 8.1 durability profile on the single write
// connection. synchronous=EXTRA with journal_mode=DELETE is the choice for a
// low-frequency single writer; it does not make a business-file rename
// transactional, and nothing here pretends otherwise.
func (s *Store) applyPragmas() error {
	for _, stmt := range []string{
		"PRAGMA busy_timeout=1000",
		"PRAGMA synchronous=EXTRA",
		"PRAGMA foreign_keys=ON",
		"PRAGMA cache_size=-8192",
		"PRAGMA mmap_size=0",
		"PRAGMA temp_store=FILE",
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return errs.Newf(errs.CodeDBError, "%s: %v", stmt, err)
		}
	}
	// journal_mode returns the resulting mode rather than a status code.
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		return errs.Newf(errs.CodeDBError, "PRAGMA journal_mode=DELETE: %v", err)
	}
	if runtime.GOOS == "darwin" {
		if _, err := s.db.Exec("PRAGMA fullfsync=ON"); err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
	}
	return nil
}

// verifyPragmas reads back every setting. SQLite silently ignores unknown
// pragma names, so writing them is not evidence that they took effect.
func (s *Store) verifyPragmas() error {
	type check struct {
		pragma string
		want   string
	}
	checks := []check{
		{"journal_mode", "delete"},
		{"synchronous", "3"},
		{"foreign_keys", "1"},
		{"busy_timeout", "1000"},
		{"cache_size", "-8192"},
		{"mmap_size", "0"},
		{"temp_store", "1"},
	}
	if runtime.GOOS == "darwin" {
		checks = append(checks, check{"fullfsync", "1"})
	}

	for _, c := range checks {
		var got string
		row := s.db.QueryRow("PRAGMA " + c.pragma)
		if err := row.Scan(&got); err != nil {
			return errs.Newf(errs.CodeDBError, "read back PRAGMA %s: %v", c.pragma, err)
		}
		if got != c.want {
			return errs.Newf(errs.CodeDBError, "PRAGMA %s is %q, expected %q", c.pragma, got, c.want)
		}
	}
	return nil
}

// migrate applies the baseline once and refuses to open a newer schema. A
// binary that silently downgrades would keep cleaning with the wrong rules.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	switch {
	case version > migrations.CurrentVersion:
		return errs.Newf(errs.CodeMigrationTooNew,
			"database schema is version %d, this binary supports %d; refusing to open",
			version, migrations.CurrentVersion)
	case version == migrations.CurrentVersion:
		return nil
	}

	steps, err := migrations.For(version)
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	for _, step := range steps {
		// Each step commits on its own. A failed step leaves the database at the
		// previous version, which is the only state that can be re-opened
		// without guessing which half of a rebuild ran.
		if _, err := s.db.Exec("BEGIN IMMEDIATE"); err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		if _, err := s.db.Exec(step.SQL); err != nil {
			s.db.Exec("ROLLBACK")
			return errs.Newf(errs.CodeDBError, "apply schema %03d (%s): %v", step.Version, step.Note, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.Version)); err != nil {
			s.db.Exec("ROLLBACK")
			return errs.Wrap(errs.CodeDBError, err)
		}
		if _, err := s.db.Exec(
			"INSERT INTO meta(key,value) VALUES('schema_version',?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			fmt.Sprintf("%d", step.Version)); err != nil {
			s.db.Exec("ROLLBACK")
			return errs.Wrap(errs.CodeDBError, err)
		}
		if _, err := s.db.Exec("COMMIT"); err != nil {
			s.db.Exec("ROLLBACK")
			return errs.Wrap(errs.CodeDBError, err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// SetNow overrides the event timestamp source. Tests inject a fake clock here.
func (s *Store) SetNow(f func() int64) { s.now = f }

// NowNS returns the current timestamp used for events.
func (s *Store) NowNS() int64 { return s.now() }

// write runs fn inside an immediate transaction, holding the process-wide write
// mutex so two goroutines can never interleave a state change and its event.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := s.refuseNested("a write"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errs.Newf(errs.CodeStateWriteFailed, "%v (rollback also failed: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return errs.Newf(errs.CodeStateWriteFailed, "commit failed: %v", err)
	}
	return nil
}

// read starts a streaming query.
//
// The rows keep the pool's single connection until they are closed, so the
// caller must consume them before asking the store anything else; a nested call
// is refused with an error rather than left to wait (see refuseNested).
func (s *Store) read(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if err := s.refuseNested("a query"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	return rows, nil
}

// MetaGet reads a meta value.
func (s *Store) MetaGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.queryRow(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errs.Wrap(errs.CodeDBError, err)
	}
	return v, nil
}

// MetaSet writes a meta value.
func (s *Store) MetaSet(ctx context.Context, key, value string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			key, value)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// Stats reports row counts for status output. It is a read-only convenience and
// never drives a safety decision.
type Stats struct {
	Scopes       int64 `json:"scopes"`
	Rules        int64 `json:"rules"`
	Grants       int64 `json:"grants"`
	Observations int64 `json:"observations"`
	Plans        int64 `json:"plans"`
	Items        int64 `json:"items"`
	Events       int64 `json:"events"`
	Protections  int64 `json:"protections"`
}

// Stats counts rows per table.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	pairs := []struct {
		table string
		dst   *int64
	}{
		{"scopes", &st.Scopes},
		{"rules", &st.Rules},
		{"grants", &st.Grants},
		{"observations", &st.Observations},
		{"plans", &st.Plans},
		{"items", &st.Items},
		{"events", &st.Events},
		{"protections", &st.Protections},
	}
	for _, p := range pairs {
		if err := s.queryRow(ctx, "SELECT COUNT(*) FROM "+p.table).Scan(p.dst); err != nil {
			return st, errs.Wrap(errs.CodeDBError, err)
		}
	}
	return st, nil
}
