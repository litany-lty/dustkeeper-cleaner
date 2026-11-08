package store

// Explicit schema upgrade for an existing installation.
//
// Every normal command calls OpenExisting, which refuses a database whose
// version is not exactly the one this build writes. That refusal is deliberate:
// a routine status check must not silently change the schema underneath a vault
// whose objects were written by an older binary.
//
// UpgradeExplicit is the one place where a known older schema may move forward,
// and it is reachable only from `init` - the command an operator runs on purpose
// after stopping the schedule, checking sidecars and taking a backup. It applies
// the same migration steps a fresh installation applies, commits each step on its
// own, and refuses anything it cannot prove is one of our databases.

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"

	"dustkeeper/internal/errs"
	"dustkeeper/migrations"
)

// UpgradeResult says what an explicit init found and did.
type UpgradeResult struct {
	From     int  `json:"from"`
	To       int  `json:"to"`
	Upgraded bool `json:"upgraded"`
}

// UpgradeExplicit brings a schema-3, schema-4 or schema-5 database up to
// CurrentVersion (schema 6). A database that is already current is a no-op.
// Files too short to carry a SQLite header are left to the caller's normal
// damage handling: this function does not create, delete or rebuild a state
// file.
func UpgradeExplicit(path string) (UpgradeResult, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return UpgradeResult{}, nil
		}
		return UpgradeResult{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return UpgradeResult{}, errs.Newf(errs.CodeRecoveryRequired,
			"%s is not a regular file; refusing to upgrade it", path)
	}
	if info.Size() < 100 {
		// Zero-length and truncated files belong to the existing state-damage
		// path, which reports damage without replacing anything.
		return UpgradeResult{}, nil
	}

	if err := refuseNonEmptySidecars(path); err != nil {
		return UpgradeResult{}, err
	}

	from, err := headerVersion(path)
	if err != nil {
		return UpgradeResult{}, err
	}
	if from == migrations.CurrentVersion {
		return UpgradeResult{From: from, To: from}, nil
	}
	if from > migrations.CurrentVersion {
		return UpgradeResult{}, errs.Newf(errs.CodeMigrationTooNew,
			"%s is schema version %d, newer than this build's %d; keep the newer binary", path, from, migrations.CurrentVersion)
	}
	if from < migrations.OldestUpgradable {
		return UpgradeResult{}, errs.Newf(errs.CodeMigrationTooOld,
			"%s is schema version %d; only %d..%d can be upgraded explicitly. Use the binary that wrote it first",
			path, from, migrations.OldestUpgradable, migrations.CurrentVersion-1)
	}

	dsn := fmt.Sprintf("file:%s?_busy_timeout=1000&_txlock=immediate", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return UpgradeResult{}, errs.Wrap(errs.CodeDBError, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := proveOurDatabase(db, path); err != nil {
		return UpgradeResult{}, err
	}
	var live int
	if err := db.QueryRow("PRAGMA user_version").Scan(&live); err != nil {
		return UpgradeResult{}, errs.Wrap(errs.CodeDBError, err)
	}
	if live != from {
		return UpgradeResult{}, errs.Newf(errs.CodeRecoveryRequired,
			"%s reports schema %d in its header and %d after recovery; close it with the old binary before upgrading",
			path, from, live)
	}

	steps, err := migrations.For(from)
	if err != nil {
		return UpgradeResult{}, errs.Wrap(errs.CodeDBError, err)
	}
	for _, step := range steps {
		// One transaction per step. A crash between steps leaves the database at
		// a version this build can still explain and resume from.
		tx, err := db.Begin()
		if err != nil {
			return UpgradeResult{}, errs.Wrap(errs.CodeDBError, err)
		}
		if _, err := tx.Exec(step.SQL); err != nil {
			_ = tx.Rollback()
			return UpgradeResult{}, errs.Newf(errs.CodeStateWriteFailed,
				"apply schema %03d (%s): %v", step.Version, step.Note, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.Version)); err != nil {
			_ = tx.Rollback()
			return UpgradeResult{}, errs.Newf(errs.CodeStateWriteFailed, "set schema %03d: %v", step.Version, err)
		}
		if _, err := tx.Exec("INSERT INTO meta(key,value) VALUES('schema_version',?) "+
			"ON CONFLICT(key) DO UPDATE SET value=excluded.value", strconv.Itoa(step.Version)); err != nil {
			_ = tx.Rollback()
			return UpgradeResult{}, errs.Newf(errs.CodeStateWriteFailed, "record schema %03d: %v", step.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return UpgradeResult{}, errs.Wrap(errs.CodeStateWriteFailed, err)
		}
	}

	var after int
	if err := db.QueryRow("PRAGMA user_version").Scan(&after); err != nil {
		return UpgradeResult{}, errs.Wrap(errs.CodeDBError, err)
	}
	if after != migrations.CurrentVersion {
		return UpgradeResult{}, errs.Newf(errs.CodeStateWriteFailed,
			"upgrade finished at schema %d, expected %d", after, migrations.CurrentVersion)
	}
	if err := proveUpgradedSchema(db, path); err != nil {
		return UpgradeResult{}, err
	}
	return UpgradeResult{From: from, To: after, Upgraded: true}, nil
}

func headerVersion(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer f.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(f, header); err != nil {
		return 0, errs.Wrap(errs.CodeDBError, err)
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return 0, errs.Newf(errs.CodeRecoveryRequired,
			"%s is not a SQLite state database; refusing to upgrade it", path)
	}
	return int(binary.BigEndian.Uint32(header[60:64])), nil
}

func refuseNonEmptySidecars(path string) error {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		st, err := os.Lstat(path + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errs.Wrap(errs.CodeAccessDenied, err)
		}
		if st.Size() > 0 {
			return errs.Newf(errs.CodeRecoveryRequired,
				"%s still has a non-empty %s sidecar; open and close the database normally with the previous binary, then retry. Nothing was migrated",
				path, suffix)
		}
	}
	return nil
}

// proveOurDatabase checks that the file carries the tables and instance identity
// this product writes, rather than merely a plausible user_version number.
//
// It runs BEFORE any migration, on a schema-3/4/5 file. It must therefore only
// require tables that exist in every upgradable schema; a schema-6-only table
// such as review_dispatch_resolution does not exist yet and is checked after
// the steps by proveUpgradedSchema. Requiring it here would refuse every
// legitimate 3/4/5 -> 6 upgrade.
func proveOurDatabase(db *sql.DB, path string) error {
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'
		AND name IN ('meta','scopes','plans','items','events')`).Scan(&tables); err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if tables != 5 {
		return errs.Newf(errs.CodeRecoveryRequired,
			"%s is missing tables this product writes (found %d of 5); refusing to upgrade it", path, tables)
	}
	var instance string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='instance_id'").Scan(&instance); err != nil || instance == "" {
		return errs.Newf(errs.CodeRecoveryRequired,
			"%s has no instance identity; it is not a state database this build can upgrade", path)
	}
	return nil
}

// proveUpgradedSchema checks the other half of the integrity contract: after
// the steps ran, the schema-6 objects must actually be present. A matching
// user_version alone would still let a build whose 006 step was missing report
// a successful upgrade and leave the manual-resolution command with no table.
func proveUpgradedSchema(db *sql.DB, path string) error {
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'
		AND name='review_dispatch_resolution'`).Scan(&tables); err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if tables != 1 {
		return errs.Newf(errs.CodeStateWriteFailed,
			"%s reached schema %d without review_dispatch_resolution; refusing to report the upgrade as complete",
			path, migrations.CurrentVersion)
	}
	return nil
}
