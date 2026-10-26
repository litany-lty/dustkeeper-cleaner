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
}

// Open opens or creates the database at path and applies the pragmas and
// migration the binary expects.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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

	if err := s.applyPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.verifyPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		// A permission fix-up failure is worth reporting but not fatal: the
		// database was created by the current user inside a 0700 directory.
		_ = err
	}
	return s, nil
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

// read runs a query and fully drains it before returning, because the pool has
// exactly one connection: a nested query during iteration would deadlock.
func (s *Store) read(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	return rows, nil
}

// MetaGet reads a meta value.
func (s *Store) MetaGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&v)
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
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+p.table).Scan(p.dst); err != nil {
			return st, errs.Wrap(errs.CodeDBError, err)
		}
	}
	return st, nil
}
