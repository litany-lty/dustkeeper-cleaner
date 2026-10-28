package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/errs"
	"dustkeeper/migrations"
)

// SQLite's header is 100 bytes and begins with this magic. The only field this
// package reads from it is the user version at offset 60.
//
// The layout is the documented one (https://www.sqlite.org/fileformat.html),
// not an inference: bytes 0..15 magic, 16..17 page size, 18/19 read and write
// versions, ... 60..63 the user version counter.
const (
	sqliteHeaderSize = 100
	sqliteUserOffset = 60
)

var sqliteMagic = []byte("SQLite format 3\x00")

// CheckSupportedState decides, from the file's own bytes, whether this build may
// write to it.
//
// Why this exists as a separate step: Open applies pragmas, and PRAGMA
// journal_mode=DELETE rewrites the header. A build that opens first and only then
// discovers "this schema is not mine" has already written to a file it was
// supposed to leave alone. The decision is therefore taken before SQLite is
// allowed to open anything.
//
// The header is a pre-filter, not a proof. A hot journal can still be rolled back
// by SQLite when the database is opened, and the only trustworthy version is the
// one read afterwards through SQLite itself; Store.Open re-reads it before the
// pragmas and the business queries. What this function guarantees is narrower and
// sufficient: a file that is not a SQLite database, or whose header already names
// a schema this build does not implement, is refused without a single write.
//
// A file that does not exist is not an unsupported state: it is a database this
// installation has not created yet, and Open creates it through the embedded
// baseline.
func CheckSupportedState(path string) error {
	f, size, err := openStateFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errs.Wrap(errs.CodeDBError, err)
	}
	defer f.Close()

	// An empty file is how SQLite represents "a database with no pages yet" and
	// is what init leaves behind for a moment. It carries no schema to refuse.
	if size == 0 {
		return nil
	}
	if size < sqliteHeaderSize {
		return errs.Newf(errs.CodeDBError,
			"%s is %d bytes long; a SQLite database is at least %d bytes",
			path, size, sqliteHeaderSize)
	}

	header := make([]byte, sqliteHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return errs.Wrap(errs.CodeDBError, fmt.Errorf("read the header of %s: %w", path, err))
	}
	if string(header[:len(sqliteMagic)]) != string(sqliteMagic) {
		return errs.Newf(errs.CodeDBError,
			"%s is not a SQLite database; refusing to write to it", path)
	}

	version := int(binary.BigEndian.Uint32(header[sqliteUserOffset : sqliteUserOffset+4]))
	switch {
	case version > migrations.CurrentVersion:
		return errs.Newf(errs.CodeMigrationTooNew,
			"%s has schema version %d; this build writes version %d. Keep the newer build, or start a new state directory",
			path, version, migrations.CurrentVersion)
	case version < migrations.CurrentVersion:
		return errs.Newf(errs.CodeMigrationTooOld,
			"%s has schema version %d; this build writes version %d and does not migrate in place. Keep the previous binary and a copy of this state directory",
			path, version, migrations.CurrentVersion)
	}
	return nil
}

// openStateFile opens the database read-only without following a symlink.
//
// The state directory is private to the user; a database that is reachable
// through a link is not the file the operator registered, and a FIFO would block
// the process that opens it. Both are refused here, before any read.
//
// O_NONBLOCK is what makes the second refusal true rather than aspirational.
// Opening a FIFO for reading without it waits for a writer, and that wait happens
// in open(2) - before the fstat that would have rejected the file. status,
// doctor, run and init would all hang on a FIFO they were about to refuse. With
// O_NONBLOCK the open returns immediately and the regular-file check below is the
// first thing that looks at the file. The flag changes nothing for a regular
// file, which is the only kind this function is willing to return.
func openStateFile(path string) (*os.File, int64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		unix.Close(fd)
		return nil, 0, errors.New("store: the database handle could not be created")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	return f, st.Size, nil
}

// databaseExists reports whether a database file is already there, without
// following a symlink.
func databaseExists(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	return true, nil
}
