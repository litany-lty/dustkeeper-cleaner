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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"

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
//
// A database this call creates is published as one step. The baseline is built at
// a staging path and only linked into place once every migration has committed,
// because the refusal above is otherwise self-defeating: the migrations commit one
// at a time, so a first installation interrupted after 001 leaves a file that the
// next run reads as "an old schema, keep the previous binary" - and there is no
// previous binary, because this was the first one. See createStateFile.
func Open(path string) (*Store, error) {
	existing, err := databaseExists(path)
	if err != nil {
		return nil, err
	}
	// A zero-length file is not a database: no header, no schema, no rows. It is
	// what an attempt that stopped inside SQLite's own initialisation leaves
	// behind, and it cannot be distinguished from an empty state directory by the
	// version check - version 0 reads as "older than this build". It is removed
	// rather than migrated: there is nothing in it to keep.
	//
	// This is the explicit-initialisation entry point only. A command that merely
	// reads the state must not take this path: see OpenExisting.
	if existing {
		empty, eerr := isEmptyFile(path)
		if eerr != nil {
			return nil, eerr
		}
		if empty {
			if rerr := os.Remove(path); rerr != nil {
				return nil, errs.Wrap(errs.CodeAccessDenied, rerr)
			}
			existing = false
		}
	}
	if !existing {
		if err := createStateFile(path); err != nil {
			return nil, err
		}
	}
	return openPublished(path)
}

// OpenExisting opens the state database of an instance that is already there and
// creates nothing, removes nothing and rebuilds nothing.
//
// It is the entry point for every command that reads or acts on an existing
// installation. Open is not, because Open answers "there is no database here" by
// installing one - which is right for `init` and wrong for `status`: a state file
// that was truncated to zero bytes used to be deleted and replaced by an empty
// baseline, and the command then reported zero scopes, zero objects and zero
// pending operations for an instance whose vault still held the files. A read-only
// diagnostic must report damage, never repair it into an empty installation.
//
// Missing, empty and unreadable are therefore three refusals with three messages,
// and in all of them the bytes on disk are left exactly as they were.
func OpenExisting(path string) (*Store, error) {
	existing, err := databaseExists(path)
	if err != nil {
		return nil, err
	}
	if !existing {
		return nil, errs.Newf(errs.CodeInvalidArgument,
			"no state database at %s; run `dustkeeper init` first", path)
	}
	empty, err := isEmptyFile(path)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, errs.Newf(errs.CodeRecoveryRequired,
			"%s is zero bytes, so it is not a database: nothing was created, removed or rebuilt. "+
				"An empty database would report no scopes and no quarantined objects while the vault and the tree still hold them. "+
				"Restore this file from a copy; if the previous state is gone, start a new state directory and leave the vault as it is", path)
	}
	return openPublished(path)
}

// openPublished opens a database that exists at its published name.
func openPublished(path string) (*Store, error) {
	// The file exists and has to be the schema this build writes. The pre-flight
	// reads the header directly, so a file that is not ours is refused without
	// SQLite touching it.
	if err := CheckSupportedState(path); err != nil {
		return nil, err
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

	// Opening the file is where SQLite rolls back a hot journal, so the version is
	// re-read after that recovery and before the pragmas. The header check ran on
	// the bytes that were on disk before it.
	if err := s.requireCurrentSchema(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.applyPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.verifyPragmas(); err != nil {
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

// stagingSuffix names the half-built database of an interrupted first
// installation. Nothing else in the product creates this name.
const stagingSuffix = ".init"

// createStateFile builds a new state database and publishes it in one step.
//
// The order is the point:
//
//	build and migrate at state.db.init  ->  link it to state.db  ->  remove the
//	staging name  ->  flush the directory
//
// link(2) does not replace an existing file, so a database that appeared in the
// meantime is an error rather than a casualty; and a process that dies before the
// link leaves no state.db at all, only a staging file that the next run resumes
// from. Resuming is safe because every migration commits on its own and
// migrations.For picks up at whatever version the file reached - unlike the
// published name, which is held to the "no in-place migration" rule.
func createStateFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	staging := path + stagingSuffix
	// Before SQLite sees the staging name, the name has to be ours.
	//
	// The name is fixed, so whatever is sitting there gets opened by the driver -
	// and the first thing this function does to an open database is set the
	// pragmas, which includes journal_mode. That is a write. A staging path that
	// is a symlink, a hard link, somebody else's file, or a database from another
	// build would therefore be modified and only afterwards refused for its
	// schema. The checks below happen on the directory entry and on the file's own
	// bytes, with SQLite nowhere in the picture.
	if err := prepareStaging(staging); err != nil {
		return err
	}
	dsn := fmt.Sprintf("file:%s?_busy_timeout=1000&_txlock=immediate", staging)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	tmp := &Store{db: db, path: staging, now: func() int64 { return time.Now().UnixNano() }}
	if err := tmp.applyPragmas(); err != nil {
		db.Close()
		return err
	}
	if err := tmp.verifyPragmas(); err != nil {
		db.Close()
		return err
	}
	if err := tmp.migrate(); err != nil {
		// The staging file is left where it is on purpose: the next attempt
		// resumes from the last committed step.
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if err := os.Link(staging, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errs.Newf(errs.CodeDBError,
				"%s appeared while this installation was being created; refusing to replace it", path)
		}
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	if err := os.Remove(staging); err != nil {
		// The published name is the same inode, so this is untidiness rather
		// than a state problem.
		_ = err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

// prepareStaging makes the staging name safe to hand to SQLite, or refuses.
//
// Four things can be at that path, and only two of them may be opened:
//
//   - nothing: this is the first attempt. The name is created here with O_EXCL,
//     so it is this process's file and not whatever a concurrent writer places
//     between the check and the driver's open;
//   - an empty regular file: an attempt that stopped before SQLite wrote its
//     first page. SQLite initialises it normally;
//   - a SQLite database whose header says a schema this build can finish
//     migrating (user_version <= CurrentVersion): the residue of an interrupted
//     first installation, which is exactly what resuming is for;
//   - anything else - a symlink, a hard link, a FIFO, a file owned by somebody
//     else, a truncated database, a file that is not SQLite, a database from a
//     newer build - is refused with the bytes untouched.
//
// The header is read here rather than through SQLite on purpose: "which schema is
// this" has to be answered before the driver gets a chance to rewrite the journal
// mode. CheckSupportedState makes the same argument for the published name.
func prepareStaging(staging string) error {
	for {
		var st unix.Stat_t
		err := unix.Lstat(staging, &st)
		if errors.Is(err, unix.ENOENT) {
			fd, cerr := unix.Open(staging,
				unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if errors.Is(cerr, unix.EEXIST) {
				// Something appeared between the check and the create. Look at it
				// instead of deciding for it.
				continue
			}
			if cerr != nil {
				return errs.Wrap(errs.CodeAccessDenied, cerr)
			}
			if cerr := unix.Close(fd); cerr != nil {
				return errs.Wrap(errs.CodeAccessDenied, cerr)
			}
			return nil
		}
		if err != nil {
			return errs.Wrap(errs.CodeAccessDenied, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return errs.Newf(errs.CodeDurabilityFailed,
				"%s is not a regular file; an interrupted installation is resumed only through a file this build created, and nothing here was opened or written",
				staging)
		}
		if st.Nlink != 1 {
			return errs.Newf(errs.CodeDurabilityFailed,
				"%s has %d links; an interrupted installation is resumed only through a file nothing else can reach, and nothing here was opened or written",
				staging, st.Nlink)
		}
		if int(st.Uid) != os.Geteuid() {
			return errs.Newf(errs.CodeDurabilityFailed,
				"%s belongs to uid %d, not to the user running this command (uid %d); nothing here was opened or written",
				staging, st.Uid, os.Geteuid())
		}
		if st.Mode&0o022 != 0 {
			return errs.Newf(errs.CodeDurabilityFailed,
				"%s is writable by group or other (%#o); nothing here was opened or written", staging, st.Mode&0o777)
		}
		if st.Size == 0 {
			// An attempt that stopped before the first page was written.
			return nil
		}
		return checkResumableStaging(staging, st.Size)
	}
}

// checkResumableStaging answers "is this file a first installation this build may
// finish" from the file itself.
func checkResumableStaging(staging string, size int64) error {
	if size < sqliteHeaderSize {
		return errs.Newf(errs.CodeDurabilityFailed,
			"%s is %d bytes, shorter than a SQLite header; it is not an interrupted installation this build can resume, and it was left as it is",
			staging, size)
	}
	f, err := os.Open(staging)
	if err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer f.Close()
	header := make([]byte, sqliteHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if string(header[:len(sqliteMagic)]) != string(sqliteMagic) {
		return errs.Newf(errs.CodeDurabilityFailed,
			"%s is not a SQLite database; it is not an interrupted installation this build can resume, and it was left as it is", staging)
	}
	version := int(binary.BigEndian.Uint32(header[sqliteUserOffset : sqliteUserOffset+4]))
	if version > migrations.CurrentVersion {
		return errs.Newf(errs.CodeMigrationTooNew,
			"%s is at schema version %d and this build writes %d; a newer installation cannot be finished by an older binary, and the file was left as it is",
			staging, version, migrations.CurrentVersion)
	}
	return nil
}

// isEmptyFile reports whether path is a regular file of zero length.
func isEmptyFile(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		// Not a regular file: CheckSupportedState is what refuses it, with the
		// right message.
		return false, nil
	}
	return st.Size == 0, nil
}

// syncDir flushes a directory so a link created in it survives a crash.
func syncDir(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer unix.Close(fd)
	if err := unix.Fsync(fd); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	return nil
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
