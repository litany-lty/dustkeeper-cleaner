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
	"strings"
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
//   - a SQLite database whose schema is the one this build writes at the version
//     in its header: the residue of an interrupted first installation, which is
//     exactly what resuming is for;
//   - anything else - a symlink, a hard link, a FIFO, a file owned by somebody
//     else, a truncated database, a file that is not SQLite, a database from a
//     newer build, a database whose schema and contents are not this build's own
//     initialisation - is refused with the bytes untouched.
//
// The header is read here rather than through SQLite on purpose: "which schema is
// this" has to be answered before the driver gets a chance to rewrite the journal
// mode. CheckSupportedState makes the same argument for the published name, and
// requireOwnStagingSchema makes it for the schema itself.
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
	return requireOwnStagingSchema(staging, size)
}

// maxStagingBytes bounds the copy the ownership check makes. An installation that
// has not been published yet contains the baseline and the meta rows the
// migrations write - no items, no observations, no events - so it is a few pages
// wide. A file past this size is not something this build produced, and copying
// it to ask would turn a refusal into a wait.
const maxStagingBytes = 16 << 20

// requireOwnStagingSchema answers the question the version number cannot: is the
// schema in this file the one this build's own initialisation writes at that
// version?
//
// user_version is a number any SQLite database can carry, and the staging name is
// fixed. A backup, an export or another product's database sitting at
// state.db.init with user_version 0 and a table of its own used to pass every
// check there was - type, owner, mode, link count, SQLite header - and then be
// migrated and published as the installation: its own rows intact, this build's
// schema added, `init` reporting a fresh start. "It is a SQLite database with a
// plausible version" is not evidence that this build produced it.
//
// So the schema is compared with the schema this build produces at that version,
// and the file must hold nothing else: a first installation that was never
// published carries the migrations' meta rows and has never taken data.
//
// The comparison runs on a copy, for the same reason all of this happens before
// SQLite is opened: the first thing SQLite does with a database is recover a hot
// journal, and that is a write. The copy also makes an interrupted migration
// readable - the journal goes with it and recovery happens there, so the version
// that comes back out of the copy is the one a resume would actually find.
func requireOwnStagingSchema(staging string, size int64) error {
	if size > maxStagingBytes {
		return refuseStaging(staging,
			"it is %d bytes, and an installation that was never published holds the schema and nothing else", size)
	}
	dir, err := os.MkdirTemp("", "dustkeeper-staging-")
	if err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer os.RemoveAll(dir)

	// A private copy, in a directory only this process can read, named the way
	// SQLite expects to find a database and its log.
	work := filepath.Join(dir, "state.db")
	if err := copyStagingFile(staging, work); err != nil {
		return err
	}
	for _, suffix := range []string{"-journal", "-wal"} {
		if err := copyStagingFile(staging+suffix, work+suffix); err != nil {
			return err
		}
	}
	return compareStagingSchema(work, staging)
}

// copyStagingFile copies one file, or accepts that it is not there.
//
// Only regular files are copied. A symbolic link at the staging name was already
// refused before this point, and the same answer holds for its journal: a log this
// build cannot read is a log it cannot account for, and "somebody else's journal"
// is not a state to resume from.
func copyStagingFile(src, dst string) error {
	in, err := unix.Open(src, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	srcFile := os.NewFile(uintptr(in), src)
	defer srcFile.Close()
	var st unix.Stat_t
	if err := unix.Fstat(in, &st); err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return refuseStaging(src, "it is not a regular file, so it is not a log this build can read")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	if _, err := io.Copy(out, srcFile); err != nil {
		out.Close()
		return errs.Wrap(errs.CodeIOError, err)
	}
	if err := out.Close(); err != nil {
		return errs.Wrap(errs.CodeIOError, err)
	}
	return nil
}

// compareStagingSchema decides, from the copy, whether the file is a first
// installation this build may finish.
func compareStagingSchema(work, staging string) error {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_busy_timeout=1000&_txlock=immediate", work))
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	defer conn.Close()

	got, err := stagingShapeOf(ctx, conn)
	if err != nil {
		return refuseStaging(staging,
			"it cannot be read as a SQLite database this build knows (%v)", err)
	}
	if got.version > migrations.CurrentVersion {
		return errs.Newf(errs.CodeMigrationTooNew,
			"%s is at schema version %d and this build writes %d; a newer installation cannot be finished by an older binary, and the file was left as it is",
			staging, got.version, migrations.CurrentVersion)
	}
	want, err := expectedStagingShape(ctx, got.version)
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	switch {
	case !sameStrings(got.objects, want.objects):
		return refuseStaging(staging,
			"it is a SQLite database at version %d, but its schema is not the one this build writes at that version: %s",
			got.version, describeDifference(got.objects, want.objects))
	case !sameStrings(got.meta, want.meta):
		return refuseStaging(staging,
			"its schema is this build's, but its meta table says something else: %s",
			describeDifference(got.meta, want.meta))
	case got.rows != want.rows:
		return refuseStaging(staging,
			"it holds %d row(s) outside meta, and an installation that was never published has taken no data; if this is a state database, it belongs at the published name",
			got.rows)
	}
	return nil
}

// stagingShape is what a file says about itself: the objects in its schema, the
// rows in its meta table, and how much data everything else holds.
type stagingShape struct {
	version int
	objects []string
	meta    []string
	rows    int64
}

// schemaQueryer is the part of *sql.DB and *sql.Conn this needs, so the same code
// can look at the copy and at the schema this build would have produced.
type schemaQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func stagingShapeOf(ctx context.Context, q schemaQueryer) (stagingShape, error) {
	var out stagingShape
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&out.version); err != nil {
		return out, err
	}
	// SQLite's own bookkeeping is included on purpose: sqlite_sequence only exists
	// after an AUTOINCREMENT table, so its presence is one more fact this build
	// produces and a foreign schema would have to reproduce. Autoindexes are not:
	// they are named after their table and carry no independent information.
	rows, err := q.QueryContext(ctx, `
SELECT type, name FROM sqlite_master
WHERE name NOT LIKE 'sqlite_autoindex%'
ORDER BY type, name`)
	if err != nil {
		return out, err
	}
	var tables []string
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			rows.Close()
			return out, err
		}
		out.objects = append(out.objects, kind+" "+name)
		if kind == "table" && name != "meta" {
			tables = append(tables, name)
		}
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	if containsString(out.objects, "table meta") {
		metaRows, err := q.QueryContext(ctx, "SELECT key || '=' || value FROM meta ORDER BY key")
		if err != nil {
			return out, err
		}
		for metaRows.Next() {
			var row string
			if err := metaRows.Scan(&row); err != nil {
				metaRows.Close()
				return out, err
			}
			out.meta = append(out.meta, row)
		}
		if err := metaRows.Close(); err != nil {
			return out, err
		}
		if err := metaRows.Err(); err != nil {
			return out, err
		}
	}

	for _, table := range tables {
		var n int64
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteIdent(table)).Scan(&n); err != nil {
			return out, err
		}
		out.rows += n
	}
	return out, nil
}

// expectedStagingShape builds, in memory, the schema this build would have at the
// given version. It replays the migrations the product ships rather than keeping a
// second copy of the answer: a table name list maintained by hand would be wrong
// the first time a migration is added, and wrong quietly.
func expectedStagingShape(ctx context.Context, version int) (stagingShape, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return stagingShape{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		return stagingShape{}, err
	}
	defer conn.Close()

	for _, step := range migrations.Steps {
		if step.Version > version {
			break
		}
		if _, err := conn.ExecContext(ctx, step.SQL); err != nil {
			return stagingShape{}, fmt.Errorf("replay schema %03d: %w", step.Version, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", step.Version)); err != nil {
			return stagingShape{}, fmt.Errorf("replay schema %03d: %w", step.Version, err)
		}
		// The same row migrate writes, in the same place: a staging file that was
		// interrupted and one that finished are told apart by the schema, not by
		// this row, but a file that carries the schema without it did not come
		// through this code.
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO meta(key,value) VALUES('schema_version',?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			fmt.Sprintf("%d", step.Version)); err != nil {
			return stagingShape{}, fmt.Errorf("replay schema %03d: %w", step.Version, err)
		}
	}
	return stagingShapeOf(ctx, conn)
}

// refuseStaging reports why a staged file is not an interrupted first
// installation.
//
// The code is RECOVERY_REQUIRED and not DURABILITY_FAILED: nothing about the bytes
// failed, they are intact and were never written to. What is missing is proof that
// this build produced them, and that is a decision for the person who put the file
// there - move it aside, or point the installation somewhere else.
func refuseStaging(staging, format string, args ...any) error {
	return errs.Newf(errs.CodeRecoveryRequired,
		"%s: %s; nothing here was opened for writing, removed or migrated, and the file was left as it is",
		staging, fmt.Sprintf(format, args...))
}

// describeDifference names the first object that does not match, so the message
// says what the file is rather than only what it is not.
func describeDifference(got, want []string) string {
	for i, name := range got {
		if i >= len(want) {
			return fmt.Sprintf("found %q, which this build does not create at that version", name)
		}
		if want[i] != name {
			return fmt.Sprintf("expected %q, found %q", want[i], name)
		}
	}
	if len(want) > len(got) {
		return fmt.Sprintf("expected %q, which is not there", want[len(got)])
	}
	return "the two schemas differ"
}

func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// quoteIdent renders a table name as an SQLite identifier. The names come from the
// file being inspected, so they are escaped rather than trusted.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
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
